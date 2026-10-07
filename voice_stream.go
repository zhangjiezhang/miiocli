package main

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"
)

type sentenceBuffer struct {
	pending []rune
	cfg     speechStreamConfig
}

func (b *sentenceBuffer) Push(delta string, flush bool) []string {
	b.pending = append(b.pending, []rune(delta)...)
	var sentences []string
	for len(b.pending) > 0 {
		cut := 0
		for i, r := range b.pending {
			strong := strings.ContainsRune("。！？!?；;\n", r)
			weak := strings.ContainsRune("，,：:", r) && i+1 >= b.cfg.MinRunes
			if r == '.' {
				// Keep a trailing period until the next token disambiguates a decimal.
				if i+1 < len(b.pending) {
					strong = !(i > 0 && unicode.IsDigit(b.pending[i-1]) && unicode.IsDigit(b.pending[i+1]))
				}
			}
			if strong || weak || i+1 >= b.cfg.MaxRunes {
				cut = i + 1
				break
			}
		}
		if cut == 0 {
			if !flush {
				break
			}
			cut = len(b.pending)
		}
		for cut < len(b.pending) && strings.ContainsRune("”’\"'）)]】」", b.pending[cut]) {
			cut++
		}
		text := strings.TrimSpace(string(b.pending[:cut]))
		b.pending = b.pending[cut:]
		if strings.IndexFunc(text, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }) >= 0 {
			sentences = append(sentences, text)
		}
	}
	return sentences
}

type speechStreamResult struct {
	stats TTSStats
	err   error
}

// A whole reply uses ONE device stream. Starting a new stream per sentence would
// reset the firmware playback queue and cut off audio already sent to the board.
func (s *VoiceCommandService) speakSegments(ctx context.Context, cancel context.CancelFunc, deviceID string, sentences <-chan string) (result speechStreamResult) {
	var started time.Time
	streamID := s.tts.nextStreamID()
	opened := false
	defer func() {
		if !started.IsZero() {
			result.stats.DurationMS = time.Since(started).Milliseconds()
		}
		if result.err != nil && opened {
			_ = s.hub.CancelAudio(deviceID, streamID)
		}
		if result.err != nil {
			cancel()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			result.err = ctx.Err()
			return
		case text, ok := <-sentences:
			if !ok {
				if err := ctx.Err(); err != nil {
					result.err = err
					return
				}
				if opened {
					result.err = s.hub.EndAudio(deviceID, streamID)
				}
				return
			}
			if !opened {
				started = time.Now()
				if err := s.hub.StartPCM(deviceID, streamID); err != nil {
					result.err = err
					return
				}
				opened = true
			}
			if err := ctx.Err(); err != nil {
				result.err = err
				return
			}
			segmentStarted := time.Now()
			stats, err := s.tts.synthesizePart(ctx, deviceID, streamID, text, s.tts.cfg.Voice, false)
			if result.stats.AudioBytes == 0 && stats.AudioBytes > 0 {
				result.stats.FirstAudioMS = segmentStarted.Sub(started).Milliseconds() + stats.FirstAudioMS
			}
			result.stats.AudioBytes += stats.AudioBytes
			if err != nil {
				result.err = err
				return
			}
			result.stats.Segments++
		}
	}
}

func (s *VoiceCommandService) handleStreamingReply(parent context.Context, deviceID, taskID, text string, record VoiceRecord, agent streamingVoiceAgent) {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	queue := make(chan string, 8)
	done := make(chan speechStreamResult, 1)
	go func() { done <- s.speakSegments(ctx, cancel, deviceID, queue) }()
	settings := agent.SpeechSettings()
	buffer := sentenceBuffer{cfg: settings}
	spoken := 0
	enqueue := func(sentences []string) error {
		for _, sentence := range sentences {
			remaining := settings.MaxSpokenRunes - spoken
			if remaining <= 0 {
				return nil
			}
			sentence = truncateRunes(sentence, remaining)
			if record.TTS.Status == "" {
				record.TTS = VoiceStage{Provider: s.tts.Provider(), Status: "processing"}
			}
			record.TTS.Input += sentence
			s.saveRecord(record)
			select {
			case queue <- sentence:
				spoken += len([]rune(sentence))
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	started := time.Now()
	summary, llmErr := agent.Stream(ctx, taskID, deviceID, text, func(delta string) error {
		if spoken >= settings.MaxSpokenRunes {
			return nil
		}
		return enqueue(buffer.Push(delta, false))
	})
	record.Nanobot.DurationMS = time.Since(started).Milliseconds()
	record.Nanobot.Output = summary
	record.Nanobot.Status = "success"
	if llmErr == nil {
		llmErr = enqueue(buffer.Push("", true))
		if llmErr == nil && spoken == 0 {
			llmErr = enqueue([]string{"任务已完成"})
		}
	}
	if llmErr != nil {
		record.Nanobot.Status = "error"
		record.Nanobot.Error = llmErr.Error()
		cancel()
	}
	s.saveRecord(record)
	close(queue)
	result := <-done
	if record.TTS.Status != "" {
		completeTTSRecord(&record, result.stats, result.err)
	}
	if llmErr != nil || result.err != nil {
		finishVoiceRecord(&record, "error")
		err := llmErr
		if err == nil || (result.err != nil && !errors.Is(result.err, context.Canceled)) {
			err = result.err
		}
		s.fail(deviceID, taskID, err)
	} else {
		finishVoiceRecord(&record, "success")
		s.sendState(deviceID, taskID, "done", truncateRunes(summary, maxVoiceResponse), text)
	}
	s.saveRecord(record)
}
