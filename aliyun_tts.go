package main

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	nls "github.com/aliyun/alibabacloud-nls-go-sdk"
)

const (
	aliyunTTSTimeout = 90 * time.Second
	maxSpeakRunes    = 1000
)

// AliyunTTSConfig holds the private Alibaba Cloud NLS credentials. Use an
// AccessKey pair for automatic token renewal, or provide a short-lived token.
type AliyunTTSConfig struct {
	AppKey          string `yaml:"appKey"`
	AccessKeyID     string `yaml:"accessKeyId"`
	AccessKeySecret string `yaml:"accessKeySecret"`
	Token           string `yaml:"token"`
	Endpoint        string `yaml:"endpoint"`
	Voice           string `yaml:"voice"`
}

type speakRequest struct {
	Text  string `json:"text"`
	Voice string `json:"voice,omitempty"`
}

type speakResponse struct {
	StreamID uint32 `json:"stream_id"`
	Status   string `json:"status"`
}

// AliyunTTSService exposes a protected HTTP endpoint and streams native NLS
// PCM to an ESP32 session managed by VoiceHub.
type AliyunTTSService struct {
	cfg      AliyunTTSConfig
	apiToken string
	hub      *VoiceHub
	nextID   uint32
}

type synthesisRun struct {
	hub      *VoiceHub
	deviceID string
	streamID uint32
	errMu    sync.Mutex
	err      error
}

func NewAliyunTTSService(cfg AliyunTTSConfig, apiToken string, hub *VoiceHub) (*AliyunTTSService, error) {
	if hub == nil {
		return nil, errors.New("voice hub is required")
	}
	if cfg.AppKey == "" {
		return nil, errors.New("voice.tts.appKey is required")
	}
	if cfg.Token == "" && (cfg.AccessKeyID == "" || cfg.AccessKeySecret == "") {
		return nil, errors.New("configure voice.tts.token or an AccessKey pair")
	}
	if apiToken == "" {
		return nil, errors.New("voice.apiToken is required for the speak endpoint")
	}
	if cfg.Endpoint == "" {
		cfg.Endpoint = nls.DEFAULT_URL
	}
	if cfg.Voice == "" {
		cfg.Voice = "xiaoyun"
	}
	return &AliyunTTSService{cfg: cfg, apiToken: apiToken, hub: hub}, nil
}

func (s *AliyunTTSService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	deviceID, ok := speakDeviceID(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorize(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !s.hub.IsOnline(deviceID) {
		http.Error(w, "device is offline", http.StatusConflict)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 4096)
	defer r.Body.Close()
	var request speakRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		http.Error(w, "invalid JSON request", http.StatusBadRequest)
		return
	}
	request.Text = strings.TrimSpace(request.Text)
	if request.Text == "" || utf8.RuneCountInString(request.Text) > maxSpeakRunes {
		http.Error(w, "text must contain 1 to 1000 characters", http.StatusBadRequest)
		return
	}

	streamID := atomic.AddUint32(&s.nextID, 1)
	if streamID == 0 {
		streamID = atomic.AddUint32(&s.nextID, 1)
	}
	voice := request.Voice
	if voice == "" {
		voice = s.cfg.Voice
	}
	go s.synthesize(deviceID, streamID, request.Text, voice)

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(speakResponse{StreamID: streamID, Status: "accepted"})
}

func (s *AliyunTTSService) synthesize(deviceID string, streamID uint32, text, voice string) {
	connection, err := s.connectionConfig()
	if err != nil {
		log.Printf("TTS token setup failed for %s: %v", deviceID, err)
		return
	}

	run := &synthesisRun{hub: s.hub, deviceID: deviceID, streamID: streamID}
	logger := nls.NewNlsLogger(io.Discard, "NLS", 0)
	logger.SetLogSil(true)
	tts, err := nls.NewSpeechSynthesis(connection, logger, false,
		func(message string, _ interface{}) {
			run.setError(fmt.Errorf("Alibaba Cloud TTS failed: %s", message))
		},
		func(pcm []byte, _ interface{}) {
			if err := run.hub.SendPCM(run.deviceID, run.streamID, pcm); err != nil {
				run.setError(err)
			}
		},
		nil,
		nil,
		nil,
		run)
	if err != nil {
		log.Printf("TTS initialization failed for %s: %v", deviceID, err)
		return
	}
	defer tts.Shutdown()

	if err := s.hub.StartPCM(deviceID, streamID); err != nil {
		log.Printf("TTS stream start failed for %s: %v", deviceID, err)
		return
	}

	param := nls.DefaultSpeechSynthesisParam()
	param.Voice = voice
	param.Format = nls.PCM
	param.SampleRate = 16000
	done, err := tts.Start(text, param, nil)
	if err != nil {
		log.Printf("TTS start failed for %s: %v", deviceID, err)
		_ = s.hub.CancelAudio(deviceID, streamID)
		return
	}

	completed := false
	select {
	case completed = <-done:
	case <-time.After(aliyunTTSTimeout):
		run.setError(errors.New("Alibaba Cloud TTS timed out"))
	}
	if err := run.getError(); err != nil || !completed {
		if err == nil {
			err = errors.New("Alibaba Cloud TTS did not complete")
		}
		log.Printf("TTS stream failed for %s: %v", deviceID, err)
		_ = s.hub.CancelAudio(deviceID, streamID)
		return
	}
	if err := s.hub.EndAudio(deviceID, streamID); err != nil {
		log.Printf("TTS stream end failed for %s: %v", deviceID, err)
	}
}

func (s *AliyunTTSService) connectionConfig() (*nls.ConnectionConfig, error) {
	if s.cfg.Token != "" {
		return nls.NewConnectionConfigWithToken(s.cfg.Endpoint, s.cfg.AppKey, s.cfg.Token), nil
	}
	return nls.NewConnectionConfigWithAKInfoDefault(s.cfg.Endpoint, s.cfg.AppKey,
		s.cfg.AccessKeyID, s.cfg.AccessKeySecret)
}

func (s *AliyunTTSService) authorize(r *http.Request) bool {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(s.apiToken), []byte(strings.TrimPrefix(header, prefix))) == 1
}

func (r *synthesisRun) setError(err error) {
	if err == nil {
		return
	}
	r.errMu.Lock()
	if r.err == nil {
		r.err = err
	}
	r.errMu.Unlock()
}

func (r *synthesisRun) getError() error {
	r.errMu.Lock()
	defer r.errMu.Unlock()
	return r.err
}

func speakDeviceID(path string) (string, bool) {
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) != 4 || parts[0] != "v1" || parts[1] != "devices" || parts[2] == "" || parts[3] != "speak" {
		return "", false
	}
	return parts[2], true
}
