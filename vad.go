package main

import (
	"encoding/binary"
	"errors"
	"math"
)

type VADConfig struct {
	Enabled     bool    `yaml:"enabled"`
	Provider    string  `yaml:"provider"`
	ThresholdDB float64 `yaml:"thresholdDb"`
	MinSpeechMS int     `yaml:"minSpeechMs"`
	PaddingMS   int     `yaml:"paddingMs"`
}

type voiceActivityDetector struct{ cfg VADConfig }

func newVAD(cfg VADConfig) (*voiceActivityDetector, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if cfg.Provider == "" {
		cfg.Provider = "energy"
	}
	if cfg.Provider != "energy" {
		return nil, errors.New("voice.vad.provider must be energy")
	}
	if cfg.ThresholdDB == 0 {
		cfg.ThresholdDB = -40
	}
	if cfg.MinSpeechMS == 0 {
		cfg.MinSpeechMS = 120
	}
	if cfg.PaddingMS == 0 {
		cfg.PaddingMS = 200
	}
	if math.IsNaN(cfg.ThresholdDB) || math.IsInf(cfg.ThresholdDB, 0) || cfg.ThresholdDB < -90 || cfg.ThresholdDB > -5 || cfg.MinSpeechMS < 20 || cfg.MinSpeechMS > 2000 || cfg.PaddingMS < 0 || cfg.PaddingMS > 2000 {
		return nil, errors.New("invalid voice.vad thresholdDb (-90..-5), minSpeechMs (20..2000), or paddingMs (0..2000)")
	}
	return &voiceActivityDetector{cfg: cfg}, nil
}

// Filter rejects silence/short clicks and removes only leading/trailing silence.
// Internal pauses remain intact. DC offset is removed from each 20 ms frame.
// This energy detector is deliberately model-free; background noise can count
// as activity. It is not equivalent to XiaoZhi's neural Silero speech classifier.
func (v *voiceActivityDetector) Filter(pcm []byte) ([]byte, bool, error) {
	if len(pcm) == 0 || len(pcm)%2 != 0 || len(pcm) > maxCaptureBytes {
		return nil, false, errors.New("VAD requires valid 16kHz mono PCM16")
	}
	if v == nil {
		return pcm, true, nil
	}
	const frameSamples = 320
	first, last, consecutive, longest := -1, -1, 0, 0
	threshold := math.Pow(10, v.cfg.ThresholdDB/20) * 32768
	for start := 0; start < len(pcm); start += frameSamples * 2 {
		end := start + frameSamples*2
		if end > len(pcm) {
			end = len(pcm)
		}
		count := float64((end - start) / 2)
		var sum, squares float64
		for i := start; i < end; i += 2 {
			value := float64(int16(binary.LittleEndian.Uint16(pcm[i:])))
			sum += value
			squares += value * value
		}
		rms := math.Sqrt(math.Max(0, squares/count-(sum/count)*(sum/count)))
		if rms >= threshold {
			if first < 0 {
				first = start
			}
			last = end
			consecutive += (end - start) / 32
			if consecutive > longest {
				longest = consecutive
			}
		} else {
			consecutive = 0
		}
	}
	if first < 0 || longest < v.cfg.MinSpeechMS {
		return nil, false, nil
	}
	padding := v.cfg.PaddingMS * 32
	first = maxInt(0, first-padding)
	last = minInt(len(pcm), last+padding)
	return pcm[first:last], true, nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
