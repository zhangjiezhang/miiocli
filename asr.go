package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

type ASRConfig struct {
	Provider       string `yaml:"provider"`
	LocalURL       string `yaml:"localUrl"`
	LocalToken     string `yaml:"localToken"`
	TimeoutSeconds int    `yaml:"timeoutSeconds"`
}

type speechRecognizer interface {
	Transcribe(context.Context, []byte) (string, error)
}

type localRecognizer struct {
	endpoint string
	token    string
	client   *http.Client
}

type fallbackRecognizer struct {
	local  speechRecognizer
	aliyun speechRecognizer
}

func newSpeechRecognizer(cfg ASRConfig, cloud AliyunTTSConfig) (speechRecognizer, error) {
	provider := strings.ToLower(strings.TrimSpace(cfg.Provider))
	if provider == "" {
		provider = "aliyun"
	}
	if provider != "local" && provider != "aliyun" && provider != "auto" {
		return nil, errors.New("voice.asr.provider must be local, aliyun, or auto")
	}
	if provider != "local" && (cloud.AppKey == "" ||
		(cloud.Token == "" && (cloud.AccessKeyID == "" || cloud.AccessKeySecret == ""))) {
		return nil, errors.New("Alibaba Cloud NLS credentials are required for aliyun/auto ASR")
	}
	aliyun := &aliyunRecognizer{cfg: cloud}
	if provider == "aliyun" {
		return aliyun, nil
	}
	base := strings.TrimRight(strings.TrimSpace(cfg.LocalURL), "/")
	parsed, err := url.Parse(base)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("voice.asr.localUrl must be an HTTP(S) base URL without credentials, query, or fragment")
	}
	token := strings.TrimSpace(cfg.LocalToken)
	if token == "" {
		token = strings.TrimSpace(os.Getenv("MIIOCLI_LOCAL_ASR_TOKEN"))
	}
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return nil, errors.New("voice.asr.localToken or MIIOCLI_LOCAL_ASR_TOKEN is required")
	}
	timeout := cfg.TimeoutSeconds
	if timeout == 0 {
		timeout = 15
	}
	if timeout < 1 || timeout > 120 {
		return nil, errors.New("voice.asr.timeoutSeconds must be between 1 and 120")
	}
	// The local service is reached directly, independently of proxy environment variables.
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	local := &localRecognizer{
		endpoint: base + "/v1/transcribe", token: token,
		client: &http.Client{Timeout: time.Duration(timeout) * time.Second, Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	}
	if provider == "auto" {
		return &fallbackRecognizer{local: local, aliyun: aliyun}, nil
	}
	return local, nil
}

func (r *localRecognizer) Transcribe(ctx context.Context, pcm []byte) (string, error) {
	if len(pcm) < 1600 || len(pcm)%2 != 0 || len(pcm) > maxCaptureBytes {
		return "", errors.New("PCM audio is empty, malformed, or too large")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint, bytes.NewReader(pcm))
	if err != nil {
		return "", errors.New("create local ASR request failed")
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Content-Type", "audio/pcm")
	resp, err := r.client.Do(req)
	if err != nil {
		return "", errors.New("local ASR request failed or timed out")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("local ASR returned HTTP %d", resp.StatusCode)
	}
	const maxResponse = 64 << 10
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil || len(body) > maxResponse {
		return "", errors.New("local ASR response unreadable or too large")
	}
	var result struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(body, &result) != nil {
		return "", errors.New("local ASR returned invalid JSON")
	}
	text := strings.TrimSpace(result.Text)
	if text == "" {
		return "", errors.New("local ASR returned no text")
	}
	return text, nil
}

func (r *fallbackRecognizer) Transcribe(ctx context.Context, pcm []byte) (string, error) {
	text, _, err := transcribeWithProvider(r, ctx, pcm)
	return text, err
}

func transcribeWithProvider(r speechRecognizer, ctx context.Context, pcm []byte) (string, string, error) {
	fallback, ok := r.(*fallbackRecognizer)
	if !ok {
		provider := "aliyun"
		if _, local := r.(*localRecognizer); local {
			provider = "local"
		}
		text, err := r.Transcribe(ctx, pcm)
		return text, provider, err
	}
	text, provider, err := transcribeWithProvider(fallback.local, ctx, pcm)
	if err == nil {
		return text, provider, nil
	}
	if ctx.Err() != nil {
		return "", provider, ctx.Err()
	}
	// Do not log provider response bodies, recognized text, or credentials here.
	log.Print("local ASR unavailable; falling back to Alibaba Cloud NLS")
	text, cloudProvider, err := transcribeWithProvider(fallback.aliyun, ctx, pcm)
	return text, provider + " → " + cloudProvider, err
}
