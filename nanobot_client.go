package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const (
	defaultNanobotTimeout = 10 * time.Minute
	maxNanobotResponse    = 1 << 20
)

var nanobotSessionPattern = regexp.MustCompile(`^[A-Za-z0-9_.:@-]{1,122}$`)

type NanobotConfig struct {
	Enabled          bool   `yaml:"enabled"`
	Stream           *bool  `yaml:"stream"`
	SentenceMinRunes int    `yaml:"sentenceMinRunes"`
	SentenceMaxRunes int    `yaml:"sentenceMaxRunes"`
	MaxSpokenRunes   int    `yaml:"maxSpokenRunes"`
	BaseURL          string `yaml:"baseUrl"`
	APIKey           string `yaml:"apiKey"`
	Model            string `yaml:"model"`
	TimeoutSeconds   int    `yaml:"timeoutSeconds"`
}

type VoiceAgent interface {
	Send(ctx context.Context, taskID, deviceID, text string, progress func(string)) (string, error)
	Timeout() time.Duration
}

type NanobotClient struct {
	cfg      NanobotConfig
	endpoint string
	client   *http.Client
}

type nanobotChatRequest struct {
	Model    string               `json:"model,omitempty"`
	Messages []nanobotChatMessage `json:"messages"`
	User     string               `json:"user"`
	Stream   bool                 `json:"stream"`
}

type nanobotChatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type nanobotChatResponse struct {
	Choices []struct {
		Message nanobotChatMessage `json:"message"`
	} `json:"choices"`
}

func NewNanobotClient(cfg NanobotConfig) (*NanobotClient, error) {
	var err error
	cfg, err = normalizeSpeechSettings(cfg)
	if err != nil {
		return nil, err
	}
	if !cfg.Enabled {
		return nil, errors.New("voice.nanobot.enabled is false")
	}
	if strings.TrimSpace(cfg.APIKey) == "" {
		return nil, errors.New("voice.nanobot.apiKey is required")
	}
	baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("voice.nanobot.baseUrl must be an HTTP(S) URL without credentials, query, or fragment")
	}
	endpoint := baseURL + "/v1/chat/completions"
	if strings.HasSuffix(baseURL, "/v1") {
		endpoint = baseURL + "/chat/completions"
	}
	return &NanobotClient{
		cfg:      cfg,
		endpoint: endpoint,
		client:   &http.Client{Timeout: nanobotTimeout(cfg)},
	}, nil
}

func (c *NanobotClient) Timeout() time.Duration {
	return nanobotTimeout(c.cfg)
}

func (c *NanobotClient) Send(ctx context.Context, _, deviceID, text string,
	progress func(string)) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("recognized text is empty")
	}
	if progress != nil {
		progress("Nanobot 正在处理")
	}
	payload, err := c.chatPayload(ctx, deviceID, text, false)
	if err != nil {
		return "", fmt.Errorf("encode request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")

	response, err := c.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("request failed: %w", err)
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, maxNanobotResponse+1))
	if err != nil {
		return "", fmt.Errorf("read response: %w", err)
	}
	if len(body) > maxNanobotResponse {
		return "", errors.New("response exceeds size limit")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		detail := truncateRunes(strings.TrimSpace(string(body)), 200)
		detail = strings.ReplaceAll(detail, c.cfg.APIKey, "[redacted]")
		if detail == "" {
			detail = http.StatusText(response.StatusCode)
		}
		return "", fmt.Errorf("request returned HTTP %d: %s", response.StatusCode, detail)
	}
	var result nanobotChatResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("decode response: %w", err)
	}
	if len(result.Choices) == 0 || strings.TrimSpace(result.Choices[0].Message.Content) == "" {
		return "", errors.New("response contains no assistant message")
	}
	return strings.TrimSpace(result.Choices[0].Message.Content), nil
}

func nanobotTimeout(cfg NanobotConfig) time.Duration {
	if cfg.TimeoutSeconds > 0 {
		return time.Duration(cfg.TimeoutSeconds) * time.Second
	}
	return defaultNanobotTimeout
}

func nanobotSessionID(deviceID string) string {
	if nanobotSessionPattern.MatchString(deviceID) {
		return "esp32:" + deviceID
	}
	digest := sha256.Sum256([]byte(deviceID))
	return "esp32:" + hex.EncodeToString(digest[:16])
}
