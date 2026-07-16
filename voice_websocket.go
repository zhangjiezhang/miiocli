package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	defaultVoicePath = "/v1/device/ws"
	voiceReadWait    = 75 * time.Second
	voicePingPeriod  = 20 * time.Second
	maxOpusFrameSize = 1024
)

// VoiceConfig configures the XiaoZhi-compatible device WebSocket endpoint.
type VoiceConfig struct {
	Path     string                       `yaml:"path"`
	APIToken string                       `yaml:"apiToken"`
	Devices  map[string]VoiceDeviceConfig `yaml:"devices"`
	TTS      AliyunTTSConfig              `yaml:"tts"`
}

type VoiceDeviceConfig struct {
	Token string `yaml:"token"`
}

type voiceHello struct {
	Type     string `json:"type"`
	DeviceID string `json:"device_id"`
	Token    string `json:"token"`
}

type voiceControl struct {
	Type     string `json:"type"`
	StreamID uint32 `json:"stream_id,omitempty"`
	DeviceID string `json:"device_id,omitempty"`
}

// VoiceHub tracks one current connection per configured device. The exported
// methods are the integration point for a future TTS worker.
type VoiceHub struct {
	path     string
	devices  map[string]VoiceDeviceConfig
	mu       sync.RWMutex
	sessions map[string]*voiceSession
	upgrader websocket.Upgrader
}

type voiceSession struct {
	deviceID  string
	conn      *websocket.Conn
	writeMu   sync.Mutex
	done      chan struct{}
	closeOnce sync.Once
	streamID  uint32
}

func NewVoiceHub(cfg VoiceConfig) *VoiceHub {
	path := cfg.Path
	if path == "" {
		path = defaultVoicePath
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	return &VoiceHub{
		path:     path,
		devices:  cfg.Devices,
		sessions: make(map[string]*voiceSession),
		upgrader: websocket.Upgrader{
			ReadBufferSize:  2048,
			WriteBufferSize: 2048,
			CheckOrigin:     func(*http.Request) bool { return true },
		},
	}
}

func (h *VoiceHub) Path() string {
	return h.path
}

func (h *VoiceHub) IsOnline(deviceID string) bool {
	_, err := h.session(deviceID)
	return err == nil
}

func (h *VoiceHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	conn, err := h.upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("voice websocket upgrade failed: %v", err)
		return
	}
	defer conn.Close()

	conn.SetReadLimit(2048)
	_ = conn.SetReadDeadline(time.Now().Add(15 * time.Second))
	messageType, payload, err := conn.ReadMessage()
	if err != nil {
		return
	}
	if messageType != websocket.TextMessage {
		h.closeWithPolicy(conn, "first message must be hello")
		return
	}

	var hello voiceHello
	if err := json.Unmarshal(payload, &hello); err != nil || hello.Type != "hello" ||
		!h.authorize(hello.DeviceID, hello.Token) {
		h.closeWithPolicy(conn, "invalid device credentials")
		return
	}

	session := &voiceSession{
		deviceID: hello.DeviceID,
		conn:     conn,
		done:     make(chan struct{}),
	}
	old := h.register(session)
	if old != nil {
		old.close()
	}
	defer func() {
		h.unregister(session)
		session.close()
	}()

	if err := session.writeControl(voiceControl{Type: "hello_ok", DeviceID: hello.DeviceID}); err != nil {
		return
	}
	log.Printf("voice device connected: %s", hello.DeviceID)
	go session.pingLoop()

	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(voiceReadWait))
	})
	for {
		_ = conn.SetReadDeadline(time.Now().Add(voiceReadWait))
		messageType, payload, err = conn.ReadMessage()
		if err != nil {
			return
		}
		if messageType == websocket.TextMessage {
			var control voiceControl
			if json.Unmarshal(payload, &control) == nil && control.Type == "ping" {
				_ = session.writeControl(voiceControl{Type: "pong"})
			}
		}
	}
}

func (h *VoiceHub) StartAudio(deviceID string, streamID uint32) error {
	if streamID == 0 {
		return errors.New("stream id must be non-zero")
	}
	session, err := h.session(deviceID)
	if err != nil {
		return err
	}
	return session.startAudio(streamID)
}

func (h *VoiceHub) SendOpus(deviceID string, streamID uint32, frame []byte) error {
	if len(frame) == 0 || len(frame) > maxOpusFrameSize {
		return fmt.Errorf("opus frame length must be between 1 and %d bytes", maxOpusFrameSize)
	}
	session, err := h.session(deviceID)
	if err != nil {
		return err
	}
	return session.sendOpus(streamID, frame)
}

func (h *VoiceHub) EndAudio(deviceID string, streamID uint32) error {
	session, err := h.session(deviceID)
	if err != nil {
		return err
	}
	return session.endAudio(streamID)
}

func (h *VoiceHub) CancelAudio(deviceID string, streamID uint32) error {
	session, err := h.session(deviceID)
	if err != nil {
		return err
	}
	return session.cancelAudio(streamID)
}

func (h *VoiceHub) authorize(deviceID, token string) bool {
	device, ok := h.devices[deviceID]
	if !ok || deviceID == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(device.Token), []byte(token)) == 1
}

func (h *VoiceHub) register(session *voiceSession) *voiceSession {
	h.mu.Lock()
	defer h.mu.Unlock()
	old := h.sessions[session.deviceID]
	h.sessions[session.deviceID] = session
	return old
}

func (h *VoiceHub) unregister(session *voiceSession) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.sessions[session.deviceID] == session {
		delete(h.sessions, session.deviceID)
		log.Printf("voice device disconnected: %s", session.deviceID)
	}
}

func (h *VoiceHub) session(deviceID string) (*voiceSession, error) {
	h.mu.RLock()
	session := h.sessions[deviceID]
	h.mu.RUnlock()
	if session == nil {
		return nil, fmt.Errorf("device %q is offline", deviceID)
	}
	return session, nil
}

func (h *VoiceHub) closeWithPolicy(conn *websocket.Conn, reason string) {
	_ = conn.WriteControl(websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.ClosePolicyViolation, reason), time.Now().Add(time.Second))
}

func (s *voiceSession) startAudio(streamID uint32) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.writeJSONLocked(voiceControl{Type: "audio_start", StreamID: streamID}); err != nil {
		return err
	}
	s.streamID = streamID
	return nil
}

func (s *voiceSession) sendOpus(streamID uint32, frame []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.streamID != streamID {
		return errors.New("audio stream is not active")
	}
	return s.conn.WriteMessage(websocket.BinaryMessage, frame)
}

func (s *voiceSession) endAudio(streamID uint32) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.streamID != streamID {
		return errors.New("audio stream is not active")
	}
	if err := s.writeJSONLocked(voiceControl{Type: "audio_end", StreamID: streamID}); err != nil {
		return err
	}
	s.streamID = 0
	return nil
}

func (s *voiceSession) cancelAudio(streamID uint32) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.streamID != streamID {
		return errors.New("audio stream is not active")
	}
	if err := s.writeJSONLocked(voiceControl{Type: "audio_cancel", StreamID: streamID}); err != nil {
		return err
	}
	s.streamID = 0
	return nil
}

func (s *voiceSession) writeControl(control voiceControl) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.writeJSONLocked(control)
}

func (s *voiceSession) writeJSONLocked(value any) error {
	_ = s.conn.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return s.conn.WriteJSON(value)
}

func (s *voiceSession) pingLoop() {
	ticker := time.NewTicker(voicePingPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			s.writeMu.Lock()
			err := s.conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(5*time.Second))
			s.writeMu.Unlock()
			if err != nil {
				s.close()
				return
			}
		case <-s.done:
			return
		}
	}
}

func (s *voiceSession) close() {
	s.closeOnce.Do(func() {
		close(s.done)
		_ = s.conn.Close()
	})
}
