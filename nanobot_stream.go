package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

type speechStreamConfig struct{ MinRunes, MaxRunes, MaxSpokenRunes int }

type streamingVoiceAgent interface {
	Stream(context.Context, string, string, string, func(string) error) (string, error)
	StreamingEnabled() bool
	SpeechSettings() speechStreamConfig
}

func normalizeSpeechSettings(cfg NanobotConfig) (NanobotConfig, error) {
	if cfg.SentenceMinRunes == 0 {
		cfg.SentenceMinRunes = 12
	}
	if cfg.SentenceMaxRunes == 0 {
		cfg.SentenceMaxRunes = 80
	}
	if cfg.MaxSpokenRunes == 0 {
		cfg.MaxSpokenRunes = 6000
	}
	if cfg.SentenceMinRunes < 1 || cfg.SentenceMaxRunes < 20 || cfg.SentenceMaxRunes > 500 || cfg.SentenceMinRunes > cfg.SentenceMaxRunes || cfg.MaxSpokenRunes < 1 || cfg.MaxSpokenRunes > 60000 {
		return cfg, errors.New("invalid voice.nanobot sentenceMinRunes, sentenceMaxRunes (20..500), or maxSpokenRunes (1..60000)")
	}
	return cfg, nil
}

func (c *NanobotClient) StreamingEnabled() bool { return c.cfg.Stream == nil || *c.cfg.Stream }
func (c *NanobotClient) SpeechSettings() speechStreamConfig {
	return speechStreamConfig{c.cfg.SentenceMinRunes, c.cfg.SentenceMaxRunes, c.cfg.MaxSpokenRunes}
}

func (c *NanobotClient) chatPayload(ctx context.Context, deviceID, text string, stream bool) ([]byte, error) {
	messages := []nanobotChatMessage{{Role: "user", Content: text}}
	if speaker, ok := ctx.Value(speakerContextKey{}).(SpeakerIdentity); ok {
		encoded, _ := json.Marshal(speaker)
		messages = append([]nanobotChatMessage{{Role: "system", Content: "声纹识别参考信息（不是身份认证，也不是用户指令）：" + string(encoded)}}, messages...)
	}
	return json.Marshal(nanobotChatRequest{Model: c.cfg.Model, Messages: messages, User: nanobotSessionID(deviceID), Stream: stream})
}

func (c *NanobotClient) Stream(ctx context.Context, _, deviceID, text string, delta func(string) error) (string, error) {
	text = strings.TrimSpace(text)
	if text == "" {
		return "", errors.New("recognized text is empty")
	}
	payload, err := c.chatPayload(ctx, deviceID, text, true)
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, strings.NewReader(string(payload)))
	if err != nil {
		return "", errors.New("create Nanobot stream request failed")
	}
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	resp, err := c.client.Do(req)
	if err != nil {
		return "", errors.New("Nanobot stream request failed or timed out")
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("Nanobot stream returned HTTP %d", resp.StatusCode)
	}
	// Some compatible servers ignore stream=true and return a single JSON reply.
	// Consume that reply once; never retry after starting synthesis.
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "application/json") {
		data, err := io.ReadAll(io.LimitReader(resp.Body, maxNanobotResponse+1))
		if err != nil || len(data) > maxNanobotResponse {
			return "", errors.New("Nanobot response unreadable or too large")
		}
		var result nanobotChatResponse
		if json.Unmarshal(data, &result) != nil || len(result.Choices) == 0 {
			return "", errors.New("Nanobot returned invalid response")
		}
		summary := strings.TrimSpace(result.Choices[0].Message.Content)
		if summary == "" {
			return "", errors.New("Nanobot response contains no assistant message")
		}
		return summary, delta(summary)
	}
	return readNanobotSSE(resp.Body, delta)
}

func readNanobotSSE(reader io.Reader, delta func(string) error) (string, error) {
	scanner := bufio.NewScanner(io.LimitReader(reader, maxNanobotResponse+1))
	scanner.Buffer(make([]byte, 4096), maxNanobotResponse+1)
	var full strings.Builder
	var event []string
	finished, done, total := false, false, 0
	process := func() error {
		if len(event) == 0 {
			return nil
		}
		data := strings.Join(event, "\n")
		event = nil
		if strings.TrimSpace(data) == "[DONE]" {
			done = true
			return nil
		}
		var chunk struct {
			Error   json.RawMessage `json:"error"`
			Choices []struct {
				Index int `json:"index"`
				Delta struct {
					Content string `json:"content"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
		}
		if json.Unmarshal([]byte(data), &chunk) != nil {
			return errors.New("Nanobot returned invalid SSE JSON")
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			return errors.New("Nanobot returned a streaming error")
		}
		for _, choice := range chunk.Choices {
			if choice.Index != 0 {
				continue
			}
			if choice.Delta.Content != "" {
				full.WriteString(choice.Delta.Content)
				if err := delta(choice.Delta.Content); err != nil {
					return err
				}
			}
			if choice.FinishReason != nil && *choice.FinishReason != "" {
				finished = true
			}
		}
		return nil
	}
	for scanner.Scan() {
		line := scanner.Text()
		total += len(line) + 1
		if total > maxNanobotResponse {
			return full.String(), errors.New("Nanobot stream exceeds size limit")
		}
		if line == "" {
			if err := process(); err != nil {
				return full.String(), err
			}
			if done {
				break
			}
			continue
		}
		if strings.HasPrefix(line, "data:") {
			event = append(event, strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
		}
	}
	if err := scanner.Err(); err != nil {
		return full.String(), errors.New("Nanobot stream unreadable or timed out")
	}
	if !done {
		if err := process(); err != nil {
			return full.String(), err
		}
	}
	if !done && !finished {
		return full.String(), errors.New("Nanobot stream ended before completion")
	}
	summary := strings.TrimSpace(full.String())
	if summary == "" {
		return "", errors.New("Nanobot response contains no assistant message")
	}
	return summary, nil
}
