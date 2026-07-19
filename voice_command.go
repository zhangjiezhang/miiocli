package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	nls "github.com/aliyun/alibabacloud-nls-go-sdk"
)

const (
	defaultCodexTimeout = 10 * time.Minute
	maxCodexSummary     = 600
)

type CodexConfig struct {
	Enabled           bool   `yaml:"enabled"`
	CallbackBaseURL   string `yaml:"callbackBaseUrl"`
	RegisterPath      string `yaml:"registerPath"`
	EventPath         string `yaml:"eventPath"`
	Token             string `yaml:"token"`
	SessionTTLSeconds int    `yaml:"sessionTtlSeconds"`
	TimeoutSeconds    int    `yaml:"timeoutSeconds"`
}

type VoiceCommandService struct {
	hub        *VoiceHub
	tts        *AliyunTTSService
	recognizer *aliyunRecognizer
	runner     *CodexSessionService
	logInput   bool
}

type aliyunRecognizer struct {
	cfg AliyunTTSConfig
}

type recognitionState struct {
	mu     sync.Mutex
	result string
	err    error
}

func NewVoiceCommandService(cfg VoiceConfig, hub *VoiceHub, tts *AliyunTTSService,
	runner *CodexSessionService) (*VoiceCommandService, error) {
	if !cfg.Codex.Enabled {
		return nil, errors.New("voice.codex.enabled is false")
	}
	if hub == nil || tts == nil || runner == nil {
		return nil, errors.New("voice hub, TTS service, and Codex session service are required")
	}
	if tts.cfg.AppKey == "" || (tts.cfg.Token == "" && (tts.cfg.AccessKeyID == "" || tts.cfg.AccessKeySecret == "")) {
		return nil, errors.New("Alibaba Cloud NLS credentials are required for speech recognition")
	}
	return &VoiceCommandService{
		hub: hub, tts: tts,
		recognizer: &aliyunRecognizer{cfg: tts.cfg},
		runner:     runner,
		logInput:   cfg.LogInput,
	}, nil
}

func (s *VoiceCommandService) HandleUtterance(deviceID, utteranceID string, pcm []byte) {
	taskID := utteranceID
	s.sendState(deviceID, taskID, "transcribing", "正在识别语音", "")
	text, err := s.recognizer.Transcribe(context.Background(), pcm)
	if err != nil {
		s.fail(deviceID, taskID, fmt.Errorf("语音识别失败: %w", err))
		return
	}
	if s.logInput {
		log.Printf("ESP32 voice input: transcribed device=%s utterance=%s bytes=%d text=%q",
			deviceID, utteranceID, len(pcm), text)
	}

	s.sendState(deviceID, taskID, "thinking", "Codex 正在分析", text)
	timeout := defaultCodexTimeout
	if s.runner.cfg.TimeoutSeconds > 0 {
		timeout = time.Duration(s.runner.cfg.TimeoutSeconds) * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	summary, err := s.runner.Send(ctx, taskID, deviceID, text, func(message string) {
		s.sendState(deviceID, taskID, "working", message, text)
	})
	if err != nil {
		s.fail(deviceID, taskID, fmt.Errorf("Codex 执行失败: %w", err))
		return
	}
	summary = truncateRunes(strings.TrimSpace(summary), maxCodexSummary)
	if summary == "" {
		summary = "任务已完成"
	}
	s.sendState(deviceID, taskID, "done", summary, text)
	if _, err := s.tts.Speak(deviceID, summary, ""); err != nil {
		log.Printf("voice command TTS failed for %s: %v", deviceID, err)
	}
}

func (s *VoiceCommandService) sendState(deviceID, taskID, state, message, text string) {
	if err := s.hub.SendCommandState(deviceID, taskID, state, message, text); err != nil {
		log.Printf("send command state to %s failed: %v", deviceID, err)
	}
}

func (s *VoiceCommandService) fail(deviceID, taskID string, err error) {
	log.Printf("voice command %s failed for %s: %v", taskID, deviceID, err)
	s.sendState(deviceID, taskID, "error", truncateRunes(err.Error(), 160), "")
}

func (r *aliyunRecognizer) Transcribe(ctx context.Context, pcm []byte) (string, error) {
	if len(pcm) < 1600 || len(pcm)%2 != 0 {
		return "", errors.New("PCM audio is empty or malformed")
	}
	connection, err := aliyunConnectionConfig(r.cfg)
	if err != nil {
		return "", err
	}
	state := &recognitionState{}
	logger := nls.NewNlsLogger(io.Discard, "NLS-STT", 0)
	logger.SetLogSil(true)
	recognition, err := nls.NewSpeechRecognition(connection, logger,
		func(message string, _ interface{}) { state.setError(errors.New(message)) },
		nil,
		func(message string, _ interface{}) { state.setResult(parseRecognitionText(message)) },
		func(message string, _ interface{}) { state.setResult(parseRecognitionText(message)) },
		nil, state)
	if err != nil {
		return "", err
	}
	defer recognition.Shutdown()

	param := nls.DefaultSpeechRecognitionParam()
	ready, err := recognition.Start(param, nil)
	if err != nil {
		return "", err
	}
	if err := waitNLS(ctx, ready); err != nil {
		return "", err
	}

	for offset := 0; offset < len(pcm); offset += 3200 {
		end := offset + 3200
		if end > len(pcm) {
			end = len(pcm)
		}
		if err := recognition.SendAudioData(pcm[offset:end]); err != nil {
			return "", err
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	ready, err = recognition.Stop()
	if err != nil {
		return "", err
	}
	if err := waitNLS(ctx, ready); err != nil {
		return "", err
	}
	result, recognitionErr := state.values()
	if recognitionErr != nil {
		return "", recognitionErr
	}
	if strings.TrimSpace(result) == "" {
		return "", errors.New("speech recognition returned no text")
	}
	return strings.TrimSpace(result), nil
}

func waitNLS(ctx context.Context, ready <-chan bool) error {
	select {
	case ok := <-ready:
		if !ok {
			return errors.New("NLS operation failed")
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(20 * time.Second):
		return errors.New("NLS operation timed out")
	}
}

func parseRecognitionText(message string) string {
	var response struct {
		Payload map[string]interface{} `json:"payload"`
	}
	if json.Unmarshal([]byte(message), &response) != nil {
		return ""
	}
	for _, key := range []string{"result", "text", "sentence"} {
		if value, ok := response.Payload[key].(string); ok && strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func (s *recognitionState) setResult(result string) {
	if result == "" {
		return
	}
	s.mu.Lock()
	s.result = result
	s.mu.Unlock()
}

func (s *recognitionState) setError(err error) {
	s.mu.Lock()
	if s.err == nil {
		s.err = err
	}
	s.mu.Unlock()
}

func (s *recognitionState) values() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.result, s.err
}

func truncateRunes(value string, max int) string {
	if utf8.RuneCountInString(value) <= max {
		return value
	}
	runes := []rune(value)
	return string(runes[:max])
}
