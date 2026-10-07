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
	defaultCodexTimeout = 10 * time.Minute // Retained for the legacy Codex session package.
	maxVoiceResponse    = 600
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
	recognizer speechRecognizer
	runner     VoiceAgent
	logInput   bool
	records    *VoiceRecordStore
	vad        *voiceActivityDetector
	vp         *voiceprintClient
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
	runner VoiceAgent) (*VoiceCommandService, error) {
	if !cfg.Nanobot.Enabled {
		return nil, errors.New("voice.nanobot.enabled is false")
	}
	if hub == nil || tts == nil || runner == nil {
		return nil, errors.New("voice hub, TTS service, and voice agent are required")
	}
	recognizer, err := newSpeechRecognizer(cfg.ASR, tts.cfg)
	if err != nil {
		return nil, err
	}
	vad, err := newVAD(cfg.VAD)
	if err != nil {
		return nil, err
	}
	vp, err := newVoiceprintClient(cfg.VP)
	if err != nil {
		return nil, err
	}
	return &VoiceCommandService{
		hub: hub, tts: tts, vad: vad, vp: vp,
		recognizer: recognizer,
		runner:     runner,
		logInput:   cfg.LogInput,
	}, nil
}

func (s *VoiceCommandService) HandleUtterance(deviceID, utteranceID string, pcm []byte) {
	taskID := utteranceID
	record := newVoiceRecord(deviceID, taskID, "voice")
	record.AudioBytes = len(pcm)
	record.AudioDurationMS = len(pcm) * 1000 / 32000
	record.ASR.Input = "PCM 16kHz mono signed 16-bit little-endian"
	if s.vad != nil {
		s.sendState(deviceID, taskID, "detecting", "正在检测语音活动", "")
		started := time.Now()
		record.VAD = VoiceStage{Provider: "energy", Status: "processing", Input: record.ASR.Input}
		filtered, speech, err := s.vad.Filter(pcm)
		record.VAD.DurationMS = time.Since(started).Milliseconds()
		record.VAD.Status = "success"
		record.VAD.AudioBytes = int64(len(filtered))
		record.VAD.Output = fmt.Sprintf("speech=%t; retained_audio_ms=%d", speech, len(filtered)/32)
		if err != nil || !speech {
			status := "ignored"
			if err != nil {
				status = "error"
				record.VAD.Status = "error"
				record.VAD.Error = err.Error()
			}
			finishVoiceRecord(&record, status)
			s.saveRecord(record)
			if err != nil {
				s.fail(deviceID, taskID, err)
			} else {
				s.sendState(deviceID, taskID, "done", "未检测到有效语音", "")
			}
			return
		}
		pcm = filtered
	}
	ctxAudio, cancelAudio := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancelAudio()
	type vpResult struct {
		identity SpeakerIdentity
		err      error
		duration int64
	}
	var vpDone chan vpResult
	if s.vp != nil {
		record.VP = VoiceStage{Provider: "3dspeaker", Status: "processing", Input: "WAV 16kHz mono PCM16"}
		vpDone = make(chan vpResult, 1)
		go func() {
			started := time.Now()
			identity, err := s.vp.Identify(ctxAudio, pcm)
			vpDone <- vpResult{identity, err, time.Since(started).Milliseconds()}
		}()
	}
	record.ASR.Status = "processing"
	s.saveRecord(record)
	s.sendState(deviceID, taskID, "transcribing", "正在识别语音", "")
	started := time.Now()
	text, provider, err := transcribeWithProvider(s.recognizer, ctxAudio, pcm)
	record.ASR.DurationMS = time.Since(started).Milliseconds()
	record.ASR.Provider = provider
	record.ASR.Output = text
	record.ASR.Status = "success"
	if err != nil {
		cancelAudio()
	}
	if vpDone != nil {
		result := <-vpDone
		record.VP.DurationMS = result.duration
		record.VP.Status = "success"
		if result.err != nil {
			record.VP.Status = "error"
			record.VP.Error = result.err.Error()
		} else {
			record.Speaker = &result.identity
			encoded, _ := json.Marshal(result.identity)
			record.VP.Output = string(encoded)
			if !result.identity.Matched {
				record.VP.Status = "unknown"
			}
		}
	}
	if err != nil {
		record.ASR.Status = "error"
		record.ASR.Error = err.Error()
		finishVoiceRecord(&record, "error")
		s.saveRecord(record)
		s.fail(deviceID, taskID, fmt.Errorf("语音识别失败: %w", err))
		return
	}
	if s.logInput {
		log.Printf("ESP32 voice input: transcribed device=%s utterance=%s bytes=%d text=%q",
			deviceID, utteranceID, len(pcm), text)
	}

	if s.vp != nil && s.vp.cfg.RequireMatch && (record.Speaker == nil || !record.Speaker.Matched) {
		finishVoiceRecord(&record, "error")
		s.saveRecord(record)
		s.fail(deviceID, taskID, errors.New("未匹配到已注册说话人，停止语音交互"))
		return
	}
	s.sendState(deviceID, taskID, "thinking", "Nanobot 正在分析", text)
	record.Nanobot = VoiceStage{Provider: "nanobot", Status: "processing", Input: text}
	s.saveRecord(record)
	ctx, cancel := context.WithTimeout(context.Background(), s.runner.Timeout())
	defer cancel()
	if record.Speaker != nil {
		ctx = context.WithValue(ctx, speakerContextKey{}, *record.Speaker)
	}

	if stream, ok := s.runner.(streamingVoiceAgent); ok && stream.StreamingEnabled() {
		s.handleStreamingReply(ctx, deviceID, taskID, text, record, stream)
		return
	}
	started = time.Now()
	summary, err := s.runner.Send(ctx, taskID, deviceID, text, func(message string) {
		s.sendState(deviceID, taskID, "working", message, text)
	})
	record.Nanobot.DurationMS = time.Since(started).Milliseconds()
	record.Nanobot.Output = summary
	record.Nanobot.Status = "success"
	if err != nil {
		record.Nanobot.Status = "error"
		record.Nanobot.Error = err.Error()
		finishVoiceRecord(&record, "error")
		s.saveRecord(record)
		s.fail(deviceID, taskID, fmt.Errorf("Nanobot 执行失败: %w", err))
		return
	}
	summary = truncateRunes(strings.TrimSpace(summary), maxVoiceResponse)
	if summary == "" {
		summary = "任务已完成"
	}
	s.sendState(deviceID, taskID, "done", summary, text)
	record.TTS = VoiceStage{Provider: s.tts.Provider(), Status: "processing", Input: summary}
	s.saveRecord(record)
	if _, err := s.tts.SpeakObserved(deviceID, summary, "", func(stats TTSStats, err error) {
		completeTTSRecord(&record, stats, err)
		s.saveRecord(record)
	}); err != nil {
		completeTTSRecord(&record, TTSStats{}, err)
		s.saveRecord(record)
		log.Printf("voice command TTS failed for %s: %v", deviceID, err)
	}
}

func (s *VoiceCommandService) saveRecord(record VoiceRecord) {
	if err := s.records.Save(record); err != nil {
		log.Printf("voice record write failed: %v", err)
	}
}

func finishVoiceRecord(record *VoiceRecord, status string) {
	now := time.Now()
	record.CompletedAt = &now
	record.Status = status
	record.TotalMS = now.Sub(record.StartedAt).Milliseconds()
}

func completeTTSRecord(record *VoiceRecord, stats TTSStats, err error) {
	record.TTS.DurationMS = stats.DurationMS
	record.TTS.FirstAudioMS = stats.FirstAudioMS
	record.TTS.AudioBytes = stats.AudioBytes
	record.TTS.Segments = stats.Segments
	record.TTS.Output = "PCM 16kHz mono signed 16-bit little-endian"
	record.TTS.Status = "success"
	status := "success"
	if err != nil {
		record.TTS.Status = "error"
		record.TTS.Error = err.Error()
		status = "error"
	}
	finishVoiceRecord(record, status)
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
