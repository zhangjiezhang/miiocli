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
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

func localHTTPClient(timeout time.Duration) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = nil
	return &http.Client{Timeout: timeout, Transport: transport,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func validateServiceURL(base string) bool {
	u, err := url.Parse(base)
	return err == nil && u.Host != "" && (u.Scheme == "http" || u.Scheme == "https") && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func normalizeTTSConfig(cfg AliyunTTSConfig) (AliyunTTSConfig, error) {
	cfg.Provider = strings.ToLower(strings.TrimSpace(cfg.Provider))
	if cfg.Provider == "" {
		cfg.Provider = "aliyun"
	}
	if cfg.Provider != "local" && cfg.Provider != "aliyun" {
		return cfg, errors.New("voice.tts.provider must be local or aliyun")
	}
	if cfg.Provider == "local" {
		cfg.LocalURL = strings.TrimRight(strings.TrimSpace(cfg.LocalURL), "/")
		if cfg.LocalURL == "" {
			cfg.LocalURL = "http://192.168.1.8:18081"
		}
		if !validateServiceURL(cfg.LocalURL) {
			return cfg, errors.New("voice.tts.localUrl must be an HTTP(S) base URL without credentials, query, or fragment")
		}
		if cfg.LocalSpeed == 0 {
			cfg.LocalSpeed = 1
		}
		if math.IsNaN(cfg.LocalSpeed) || math.IsInf(cfg.LocalSpeed, 0) || cfg.LocalSpeed < .7 || cfg.LocalSpeed > 1.5 {
			return cfg, errors.New("voice.tts.localSpeed must be between 0.7 and 1.5")
		}
		if cfg.TimeoutSeconds == 0 {
			cfg.TimeoutSeconds = 90
		}
		if cfg.TimeoutSeconds < 1 || cfg.TimeoutSeconds > 300 {
			return cfg, errors.New("voice.tts.timeoutSeconds must be between 1 and 300")
		}
	}
	return cfg, nil
}

func (s *AliyunTTSService) Provider() string {
	if s.cfg.Provider == "local" {
		return "local"
	}
	return "aliyun"
}

// pcmResampler keeps sample positions and byte carry across HTTP chunk boundaries.
// Linear interpolation converts local 22050 Hz output to the device's 16000 Hz PCM.
type pcmResampler struct {
	rate     int64
	index    int64
	next     int64
	previous int16
	carry    []byte
}

func (r *pcmResampler) push(data []byte) []byte {
	data = append(r.carry, data...)
	var out []byte
	for len(data) >= 2 {
		value := int16(binary.LittleEndian.Uint16(data))
		if r.index == 0 {
			out = binary.LittleEndian.AppendUint16(out, uint16(value))
			r.next = r.rate
		} else {
			for r.next <= r.index*16000 {
				fraction := float64(r.next-(r.index-1)*16000) / 16000
				sample := int16(math.Round(float64(r.previous) + (float64(value)-float64(r.previous))*fraction))
				out = binary.LittleEndian.AppendUint16(out, uint16(sample))
				r.next += r.rate
			}
		}
		r.previous = value
		r.index++
		data = data[2:]
	}
	r.carry = append([]byte(nil), data...)
	return out
}

func (s *AliyunTTSService) synthesizeLocal(deviceID string, streamID uint32, text string) (TTSStats, error) {
	return s.synthesizeLocalPart(context.Background(), deviceID, streamID, text, true)
}

func (s *AliyunTTSService) synthesizeLocalPart(parent context.Context, deviceID string, streamID uint32, text string, manageStream bool) (stats TTSStats, resultErr error) {
	started := time.Now()
	defer func() { stats.DurationMS = time.Since(started).Milliseconds() }()
	cfg, err := normalizeTTSConfig(s.cfg)
	if err != nil {
		return stats, err
	}
	ctx, cancel := context.WithTimeout(parent, time.Duration(cfg.TimeoutSeconds)*time.Second)
	defer cancel()
	client := localHTTPClient(time.Duration(cfg.TimeoutSeconds) * time.Second)
	defer client.CloseIdleConnections()
	// The local service accepts at most 500 runes per request. Keep one device
	// playback stream while processing long Speak/agent replies in order.
	runes := []rune(text)
	streamStarted := false
	defer func() {
		if resultErr != nil && streamStarted && manageStream {
			_ = s.hub.CancelAudio(deviceID, streamID)
		}
	}()
	for offset := 0; offset < len(runes); offset += 500 {
		end := offset + 500
		if end > len(runes) {
			end = len(runes)
		}
		payload, _ := json.Marshal(map[string]any{"text": string(runes[offset:end]), "speed": cfg.LocalSpeed})
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.LocalURL+"/tts/stream", bytes.NewReader(payload))
		if err != nil {
			return stats, errors.New("create local TTS request failed")
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			return stats, errors.New("local TTS request failed or timed out")
		}
		err = func() error {
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				return fmt.Errorf("local TTS returned HTTP %d", resp.StatusCode)
			}
			rate, err := strconv.Atoi(resp.Header.Get("X-Sample-Rate"))
			if err != nil || rate < 8000 || rate > 48000 {
				return errors.New("local TTS returned invalid X-Sample-Rate")
			}
			if !streamStarted && manageStream {
				if err := s.hub.StartPCM(deviceID, streamID); err != nil {
					return err
				}
				streamStarted = true
			}
			r := pcmResampler{rate: int64(rate)}
			buffer := make([]byte, 4096)
			const maxAudio = 48000 * 2 * 300
			total := 0
			for {
				n, readErr := resp.Body.Read(buffer)
				total += n
				if total > maxAudio {
					return errors.New("local TTS audio exceeds size limit")
				}
				pcm := r.push(buffer[:n])
				if len(pcm) > 0 {
					if err := s.hub.SendPCM(deviceID, streamID, pcm); err != nil {
						return err
					}
					if stats.AudioBytes == 0 {
						stats.FirstAudioMS = time.Since(started).Milliseconds()
					}
					stats.AudioBytes += int64(len(pcm))
				}
				if readErr == io.EOF {
					break
				}
				if readErr != nil {
					return errors.New("read local TTS audio failed or timed out")
				}
			}
			if len(r.carry) != 0 || r.index == 0 {
				return errors.New("local TTS returned empty or malformed PCM")
			}
			return nil
		}()
		if err != nil {
			return stats, err
		}
	}
	if manageStream {
		return stats, s.hub.EndAudio(deviceID, streamID)
	}
	return stats, nil
}
