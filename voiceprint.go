package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime/multipart"
	"net/http"
	"net/textproto"
	"os"
	"strings"
	"time"
)

type VoiceprintConfig struct {
	Enabled             bool                `yaml:"enabled"`
	BaseURL             string              `yaml:"baseUrl"`
	APIKey              string              `yaml:"apiKey"`
	Speakers            []VoiceprintSpeaker `yaml:"speakers"`
	SimilarityThreshold float64             `yaml:"similarityThreshold"`
	TimeoutSeconds      int                 `yaml:"timeoutSeconds"`
	RequireMatch        bool                `yaml:"requireMatch"`
}

type VoiceprintSpeaker struct {
	ID          string `yaml:"id" json:"id"`
	Name        string `yaml:"name" json:"name"`
	Description string `yaml:"description" json:"description,omitempty"`
}

type SpeakerIdentity struct {
	VoiceprintSpeaker
	Score   float64 `json:"score"`
	Matched bool    `json:"matched"`
}

type voiceprintClient struct {
	cfg    VoiceprintConfig
	client *http.Client
}
type speakerContextKey struct{}

func newVoiceprintClient(cfg VoiceprintConfig) (*voiceprintClient, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	cfg.BaseURL = strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if cfg.BaseURL == "" {
		cfg.BaseURL = "http://192.168.1.62:8005"
	}
	if !validateServiceURL(cfg.BaseURL) {
		return nil, errors.New("voice.vp.baseUrl must be an HTTP(S) base URL without credentials, query, or fragment")
	}
	if cfg.APIKey == "" {
		cfg.APIKey = strings.TrimSpace(os.Getenv("MIIOCLI_VP_API_KEY"))
	}
	if strings.TrimSpace(cfg.APIKey) == "" || strings.ContainsAny(cfg.APIKey, "\r\n") {
		return nil, errors.New("voice.vp.apiKey or MIIOCLI_VP_API_KEY is required")
	}
	if cfg.SimilarityThreshold == 0 {
		cfg.SimilarityThreshold = .4
	}
	if math.IsNaN(cfg.SimilarityThreshold) || math.IsInf(cfg.SimilarityThreshold, 0) || cfg.SimilarityThreshold < 0 || cfg.SimilarityThreshold > 1 {
		return nil, errors.New("voice.vp.similarityThreshold must be between 0 and 1")
	}
	if cfg.TimeoutSeconds == 0 {
		cfg.TimeoutSeconds = 10
	}
	if cfg.TimeoutSeconds < 1 || cfg.TimeoutSeconds > 120 {
		return nil, errors.New("voice.vp.timeoutSeconds must be between 1 and 120")
	}
	if len(cfg.Speakers) == 0 {
		return nil, errors.New("voice.vp.speakers requires registered speaker IDs")
	}
	seen := map[string]bool{}
	for _, speaker := range cfg.Speakers {
		if speaker.ID == "" || strings.TrimSpace(speaker.ID) != speaker.ID || strings.ContainsAny(speaker.ID, ",\r\n") || seen[speaker.ID] {
			return nil, errors.New("voice.vp.speakers IDs must be nonempty, unique, and contain no commas or newlines")
		}
		seen[speaker.ID] = true
	}
	return &voiceprintClient{cfg: cfg, client: localHTTPClient(time.Duration(cfg.TimeoutSeconds) * time.Second)}, nil
}

func pcmWAV(pcm []byte) []byte {
	wav := make([]byte, 44, 44+len(pcm))
	copy(wav, "RIFF")
	binary.LittleEndian.PutUint32(wav[4:], uint32(36+len(pcm)))
	copy(wav[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(wav[16:], 16)
	binary.LittleEndian.PutUint16(wav[20:], 1)
	binary.LittleEndian.PutUint16(wav[22:], 1)
	binary.LittleEndian.PutUint32(wav[24:], 16000)
	binary.LittleEndian.PutUint32(wav[28:], 32000)
	binary.LittleEndian.PutUint16(wav[32:], 2)
	binary.LittleEndian.PutUint16(wav[34:], 16)
	copy(wav[36:], "data")
	binary.LittleEndian.PutUint32(wav[40:], uint32(len(pcm)))
	return append(wav, pcm...)
}

func (v *voiceprintClient) Identify(ctx context.Context, pcm []byte) (SpeakerIdentity, error) {
	var identity SpeakerIdentity
	if len(pcm) < 1600 || len(pcm)%2 != 0 || len(pcm) > maxCaptureBytes {
		return identity, errors.New("VP requires valid 16kHz mono PCM16")
	}
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	ids := make([]string, 0, len(v.cfg.Speakers))
	for _, speaker := range v.cfg.Speakers {
		ids = append(ids, speaker.ID)
	}
	_ = form.WriteField("speaker_ids", strings.Join(ids, ","))
	header := make(textproto.MIMEHeader)
	header.Set("Content-Disposition", `form-data; name="file"; filename="audio.wav"`)
	header.Set("Content-Type", "audio/wav")
	part, err := form.CreatePart(header)
	if err != nil {
		return identity, errors.New("encode VP audio failed")
	}
	_, _ = part.Write(pcmWAV(pcm))
	_ = form.Close()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.cfg.BaseURL+"/voiceprint/identify", &body)
	if err != nil {
		return identity, errors.New("create VP request failed")
	}
	req.Header.Set("Content-Type", form.FormDataContentType())
	req.Header.Set("Authorization", "Bearer "+v.cfg.APIKey)
	resp, err := v.client.Do(req)
	if err != nil {
		return identity, errors.New("VP request failed or timed out")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return identity, fmt.Errorf("VP returned HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return identity, errors.New("VP response unreadable or too large")
	}
	var result struct {
		ID    *string  `json:"speaker_id"`
		Score *float64 `json:"score"`
	}
	if json.Unmarshal(data, &result) != nil || result.ID == nil || result.Score == nil || math.IsNaN(*result.Score) || math.IsInf(*result.Score, 0) || *result.Score < -1 || *result.Score > 1 {
		return identity, errors.New("VP returned invalid JSON result")
	}
	identity.Score = *result.Score
	for _, speaker := range v.cfg.Speakers {
		if speaker.ID == *result.ID && identity.Score >= v.cfg.SimilarityThreshold {
			identity.VoiceprintSpeaker = speaker
			identity.Matched = true
			break
		}
	}
	return identity, nil
}
