package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	defaultVoicePath  = "/v1/device/ws"
	voiceReadWait     = 75 * time.Second
	voicePingPeriod   = 20 * time.Second
	maxAudioFrameSize = 1024
	maxCaptureBytes   = 16000 * 2 * 30
	codecOpus         = "opus"
	codecPCMS16LE     = "pcm_s16le"
)

// VoiceConfig configures the XiaoZhi-compatible device WebSocket endpoint.
type VoiceConfig struct {
	Path     string                       `yaml:"path"`
	APIToken string                       `yaml:"apiToken"`
	LogInput bool                         `yaml:"logInput"`
	Devices  map[string]VoiceDeviceConfig `yaml:"devices"`
	TTS      AliyunTTSConfig              `yaml:"tts"`
	Codex    CodexConfig                  `yaml:"codex"`
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
	Type          string               `json:"type"`
	StreamID      uint32               `json:"stream_id,omitempty"`
	UtteranceID   string               `json:"utterance_id,omitempty"`
	TaskID        string               `json:"task_id,omitempty"`
	DeviceID      string               `json:"device_id,omitempty"`
	Codec         string               `json:"codec,omitempty"`
	SampleRate    int                  `json:"sample_rate,omitempty"`
	Channels      int                  `json:"channels,omitempty"`
	BitsPerSample int                  `json:"bits_per_sample,omitempty"`
	State         string               `json:"state,omitempty"`
	Message       string               `json:"message,omitempty"`
	Text          string               `json:"text,omitempty"`
	Bytes         int                  `json:"bytes,omitempty"`
	SessionID     string               `json:"session_id,omitempty"`
	Surface       string               `json:"surface,omitempty"`
	Sessions      []voiceSessionOption `json:"sessions,omitempty"`
}

type voiceSessionOption struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	Surface  string `json:"surface"`
	State    string `json:"state"`
	Selected bool   `json:"selected"`
}

type utteranceHandler func(deviceID, utteranceID string, pcm []byte)
type codexSessionHandler func(deviceID, action, sessionID string)

// VoiceHub tracks one current connection per configured device. The exported
// methods are the integration point for a future TTS worker.
type VoiceHub struct {
	path           string
	logInput       bool
	devices        map[string]VoiceDeviceConfig
	mu             sync.RWMutex
	sessions       map[string]*voiceSession
	upgrader       websocket.Upgrader
	handler        utteranceHandler
	sessionHandler codexSessionHandler
}

type voiceSession struct {
	deviceID  string
	conn      *websocket.Conn
	writeMu   sync.Mutex
	done      chan struct{}
	closeOnce sync.Once
	streamID  uint32
	codec     string
	captureMu sync.Mutex
	captureID string
	capture   []byte
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
		logInput: cfg.LogInput,
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

func (h *VoiceHub) SetUtteranceHandler(handler utteranceHandler) {
	h.mu.Lock()
	h.handler = handler
	h.mu.Unlock()
}

func (h *VoiceHub) SetCodexSessionHandler(handler codexSessionHandler) {
	h.mu.Lock()
	h.sessionHandler = handler
	h.mu.Unlock()
}

func (h *VoiceHub) SendCommandState(deviceID, taskID, state, message, text string) error {
	session, err := h.session(deviceID)
	if err != nil {
		return err
	}
	return session.writeControl(voiceControl{
		Type: "command_state", TaskID: taskID, State: state, Message: message, Text: text,
	})
}

func (h *VoiceHub) SendCodexSessions(deviceID string, sessions []voiceSessionOption) error {
	session, err := h.session(deviceID)
	if err != nil {
		return err
	}
	return session.writeControl(voiceControl{Type: "session_list", Sessions: sessions})
}

func (h *VoiceHub) SendCodexSessionSelected(deviceID string, selected voiceSessionOption) error {
	session, err := h.session(deviceID)
	if err != nil {
		return err
	}
	return session.writeControl(voiceControl{
		Type: "session_selected", SessionID: selected.ID, Message: selected.Title,
		Surface: selected.Surface, State: selected.State,
	})
}

func (h *VoiceHub) OnlineDeviceIDs() []string {
	h.mu.RLock()
	deviceIDs := make([]string, 0, len(h.sessions))
	for deviceID := range h.sessions {
		deviceIDs = append(deviceIDs, deviceID)
	}
	h.mu.RUnlock()
	sort.Strings(deviceIDs)
	return deviceIDs
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
	h.dispatchSessionControl(hello.DeviceID, "session_list", "")

	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(voiceReadWait))
	})
	for {
		_ = conn.SetReadDeadline(time.Now().Add(voiceReadWait))
		messageType, payload, err = conn.ReadMessage()
		if err != nil {
			return
		}
		switch messageType {
		case websocket.TextMessage:
			h.handleDeviceControl(session, payload)
		case websocket.BinaryMessage:
			if err := session.appendCapture(payload); err != nil {
				_ = session.writeControl(voiceControl{Type: "command_state", State: "error", Message: err.Error()})
			}
		}
	}
}

func (h *VoiceHub) handleDeviceControl(session *voiceSession, payload []byte) {
	var control voiceControl
	if json.Unmarshal(payload, &control) != nil {
		return
	}
	switch control.Type {
	case "ping":
		_ = session.writeControl(voiceControl{Type: "pong"})
	case "listen_start":
		if control.Codec != codecPCMS16LE || control.SampleRate != 16000 || control.Channels != 1 ||
			control.BitsPerSample != 16 || control.UtteranceID == "" {
			_ = session.writeControl(voiceControl{Type: "command_state", State: "error", Message: "invalid capture format"})
			return
		}
		session.startCapture(control.UtteranceID)
		h.logVoiceInput("started device=%s utterance=%s codec=%s sample_rate=%d channels=%d bits=%d",
			session.deviceID, control.UtteranceID, control.Codec, control.SampleRate,
			control.Channels, control.BitsPerSample)
		_ = session.writeControl(voiceControl{Type: "listen_ready", UtteranceID: control.UtteranceID})
	case "listen_end":
		pcm, err := session.finishCapture(control.UtteranceID)
		if err != nil {
			_ = session.writeControl(voiceControl{Type: "command_state", State: "error", Message: err.Error()})
			return
		}
		durationMS := len(pcm) * 1000 / (16000 * 2)
		h.logVoiceInput("received device=%s utterance=%s bytes=%d duration_ms=%d",
			session.deviceID, control.UtteranceID, len(pcm), durationMS)
		_ = session.writeControl(voiceControl{Type: "listen_received", UtteranceID: control.UtteranceID, Bytes: len(pcm)})
		h.mu.RLock()
		handler := h.handler
		h.mu.RUnlock()
		if handler == nil {
			_ = session.writeControl(voiceControl{Type: "command_state", State: "error", Message: "voice command service is unavailable"})
			return
		}
		go handler(session.deviceID, control.UtteranceID, pcm)
	case "session_list", "session_next", "session_select":
		h.dispatchSessionControl(session.deviceID, control.Type, control.SessionID)
	}
}

func (h *VoiceHub) logVoiceInput(format string, args ...interface{}) {
	if h.logInput {
		log.Printf("ESP32 voice input: "+format, args...)
	}
}

func (h *VoiceHub) dispatchSessionControl(deviceID, action, sessionID string) {
	h.mu.RLock()
	handler := h.sessionHandler
	h.mu.RUnlock()
	if handler != nil {
		go handler(deviceID, action, sessionID)
	}
}

func (s *voiceSession) startCapture(utteranceID string) {
	s.captureMu.Lock()
	s.captureID = utteranceID
	s.capture = make([]byte, 0, 32*1024)
	s.captureMu.Unlock()
}

func (s *voiceSession) appendCapture(frame []byte) error {
	s.captureMu.Lock()
	defer s.captureMu.Unlock()
	if s.captureID == "" {
		return errors.New("audio frame received without listen_start")
	}
	if len(frame) == 0 || len(frame) > maxAudioFrameSize || len(s.capture)+len(frame) > maxCaptureBytes {
		s.captureID = ""
		s.capture = nil
		return errors.New("voice capture exceeded protocol limits")
	}
	s.capture = append(s.capture, frame...)
	return nil
}

func (s *voiceSession) finishCapture(utteranceID string) ([]byte, error) {
	s.captureMu.Lock()
	defer s.captureMu.Unlock()
	if utteranceID == "" || utteranceID != s.captureID {
		return nil, errors.New("listen_end does not match active capture")
	}
	if len(s.capture) < 1600 {
		s.captureID = ""
		s.capture = nil
		return nil, errors.New("voice capture is too short")
	}
	pcm := append([]byte(nil), s.capture...)
	s.captureID = ""
	s.capture = nil
	return pcm, nil
}

func (h *VoiceHub) StartAudio(deviceID string, streamID uint32) error {
	if streamID == 0 {
		return errors.New("stream id must be non-zero")
	}
	session, err := h.session(deviceID)
	if err != nil {
		return err
	}
	return session.startAudio(streamID, codecOpus)
}

func (h *VoiceHub) SendOpus(deviceID string, streamID uint32, frame []byte) error {
	if len(frame) == 0 || len(frame) > maxAudioFrameSize {
		return fmt.Errorf("opus frame length must be between 1 and %d bytes", maxAudioFrameSize)
	}
	session, err := h.session(deviceID)
	if err != nil {
		return err
	}
	return session.sendAudio(streamID, codecOpus, frame)
}

// StartPCM selects signed 16-bit little-endian PCM at 16 kHz, mono.
func (h *VoiceHub) StartPCM(deviceID string, streamID uint32) error {
	if streamID == 0 {
		return errors.New("stream id must be non-zero")
	}
	session, err := h.session(deviceID)
	if err != nil {
		return err
	}
	return session.startAudio(streamID, codecPCMS16LE)
}

// SendPCM frames signed 16-bit little-endian PCM for the current stream.
func (h *VoiceHub) SendPCM(deviceID string, streamID uint32, pcm []byte) error {
	if len(pcm) == 0 || len(pcm)%2 != 0 {
		return errors.New("PCM data must contain complete 16-bit samples")
	}
	session, err := h.session(deviceID)
	if err != nil {
		return err
	}
	for len(pcm) > 0 {
		frameLen := len(pcm)
		if frameLen > maxAudioFrameSize {
			frameLen = maxAudioFrameSize
		}
		if err := session.sendAudio(streamID, codecPCMS16LE, pcm[:frameLen]); err != nil {
			return err
		}
		pcm = pcm[frameLen:]
	}
	return nil
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

func (s *voiceSession) startAudio(streamID uint32, codec string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	control := voiceControl{Type: "audio_start", StreamID: streamID, Codec: codec}
	if codec == codecPCMS16LE {
		control.SampleRate = 16000
		control.Channels = 1
		control.BitsPerSample = 16
	}
	if err := s.writeJSONLocked(control); err != nil {
		return err
	}
	s.streamID = streamID
	s.codec = codec
	return nil
}

func (s *voiceSession) sendAudio(streamID uint32, codec string, frame []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if s.streamID != streamID || s.codec != codec {
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
	s.codec = ""
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
	s.codec = ""
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
