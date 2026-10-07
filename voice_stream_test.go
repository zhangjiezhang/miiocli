package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestSentenceBufferAcrossTokens(t *testing.T) {
	for _, tc := range []struct {
		name         string
		tokens, want []string
	}{
		{"Chinese", []string{"你好", "。今天温度是3.", "14度，", "请注意保暖！尾", "句"}, []string{"你好。", "今天温度是3.14度，请注意保暖！", "尾句"}},
		{"English", []string{"Hello.", " Next sentence", "!"}, []string{"Hello.", "Next sentence!"}},
		{"comma and length", []string{"这是达到逗号断句要求的一句话，", strings.Repeat("长", 85)}, []string{"这是达到逗号断句要求的一句话，", strings.Repeat("长", 80), strings.Repeat("长", 5)}},
		{"empty punctuation", []string{"。", "\n", "剩余内容"}, []string{"剩余内容"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buffer := sentenceBuffer{cfg: speechStreamConfig{12, 80, 6000}}
			var got []string
			for _, token := range tc.tokens {
				got = append(got, buffer.Push(token, false)...)
			}
			got = append(got, buffer.Push("", true)...)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("sentences=%q want=%q", got, tc.want)
			}
		})
	}
}

func TestNanobotSSEProtocol(t *testing.T) {
	stream := ": heartbeat\r\n\r\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"reasoning_content\":\"hidden\"}}]}\r\n\r\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好\"}}]}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"。\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	var deltas []string
	summary, err := readNanobotSSE(strings.NewReader(stream), func(s string) error { deltas = append(deltas, s); return nil })
	if err != nil || summary != "你好。" || !reflect.DeepEqual(deltas, []string{"你好", "。"}) {
		t.Fatalf("summary=%q deltas=%q err=%v", summary, deltas, err)
	}
	for _, body := range []string{"data: not JSON\n\n", `data: {"error":{"message":"private-secret"}}` + "\n\n", `data: {"choices":[{"delta":{"content":"partial"}}]}` + "\n\n", "data: [DONE]\n\n", strings.Repeat("x", maxNanobotResponse+1)} {
		if _, err := readNanobotSSE(strings.NewReader(body), func(string) error { return nil }); err == nil || strings.Contains(err.Error(), "private-secret") {
			t.Fatal("incomplete, empty, invalid, or oversized SSE accepted / secret leaked", err)
		}
	}
	want := errors.New("consumer stopped")
	_, err = readNanobotSSE(strings.NewReader(stream), func(string) error { return want })
	if !errors.Is(err, want) {
		t.Fatal("consumer cancellation lost", err)
	}
}

func TestNanobotStreamRequestAndJSONCompatibility(t *testing.T) {
	for _, sse := range []bool{true, false} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var request nanobotChatRequest
			if json.NewDecoder(r.Body).Decode(&request) != nil || !request.Stream || request.User != "esp32:device" || r.Header.Get("Accept") != "text/event-stream" || r.Header.Get("Authorization") != "Bearer private-key" {
				t.Error("invalid streaming request")
			}
			if sse {
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"你好。\"}}]}\n\ndata: [DONE]\n\n")
			} else {
				w.Header().Set("Content-Type", "application/json")
				io.WriteString(w, `{"choices":[{"message":{"content":"你好。"}}]}`)
			}
		}))
		client, err := NewNanobotClient(NanobotConfig{Enabled: true, BaseURL: server.URL, APIKey: "private-key"})
		if err != nil {
			t.Fatal(err)
		}
		var delta string
		text, err := client.Stream(context.Background(), "task", "device", "问题", func(s string) error { delta += s; return nil })
		server.Close()
		if err != nil || text != "你好。" || delta != text || !client.StreamingEnabled() {
			t.Fatalf("text=%q delta=%q err=%v", text, delta, err)
		}
	}
}

func TestStreamingReplyFirstAudioBeforeLLMCompletes(t *testing.T) {
	for _, provider := range []string{"local", "aliyun"} {
		t.Run(provider, func(t *testing.T) { testStreamingReply(t, provider, "") })
	}
}

func TestStreamingReplyWithLiveLocalTTS(t *testing.T) {
	base := os.Getenv("MIIOCLI_TEST_LOCAL_TTS_URL")
	if base == "" {
		t.Skip("MIIOCLI_TEST_LOCAL_TTS_URL not set")
	}
	testStreamingReply(t, "local", base)
}

func testStreamingReply(t *testing.T, provider, liveBase string) {
	firstAudio := make(chan struct{})
	var firstOnce sync.Once
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"第一\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"句。\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-firstAudio:
		case <-time.After(3 * time.Second):
			t.Error("first audio waited for full LLM response")
			return
		}
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"第二句。末尾\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer llm.Close()
	segments := make(chan string, 4)
	ttsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if provider == "aliyun" {
			ws, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer ws.Close()
			var request struct {
				Payload struct {
					Text string `json:"text"`
				} `json:"payload"`
			}
			if err := ws.ReadJSON(&request); err != nil {
				t.Error(err)
				return
			}
			segments <- request.Payload.Text
			_ = ws.WriteMessage(websocket.BinaryMessage, tonePCM(16000, 20))
			_ = ws.WriteMessage(websocket.TextMessage, []byte(`{"header":{"namespace":"SpeechSynthesizer","name":"SynthesisCompleted"}}`))
			_, _, _ = ws.ReadMessage()
			return
		}
		var request struct {
			Text string `json:"text"`
		}
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("invalid TTS payload")
		}
		segments <- request.Text
		w.Header().Set("X-Sample-Rate", "16000")
		_, _ = w.Write(tonePCM(16000, 20))
	}))
	defer ttsServer.Close()
	hub, device := processingDevice(t)
	cfg := AliyunTTSConfig{Provider: "local", LocalURL: ttsServer.URL}
	if provider == "aliyun" {
		cfg = AliyunTTSConfig{Provider: "aliyun", AppKey: "app-key", Token: "mock-token", Endpoint: "ws" + strings.TrimPrefix(ttsServer.URL, "http")}
	}
	if liveBase != "" {
		cfg.LocalURL = liveBase
	}
	tts, err := NewAliyunTTSService(cfg, "test-token", hub)
	if err != nil {
		t.Fatal(err)
	}
	agent, err := NewNanobotClient(NanobotConfig{Enabled: true, BaseURL: llm.URL, APIKey: "private-key"})
	if err != nil {
		t.Fatal(err)
	}
	store := testRecordStore(t)
	service := &VoiceCommandService{hub: hub, tts: tts, runner: agent, recognizer: &stubRecognizer{text: "问题"}, records: store}
	finished := make(chan struct{})
	go func() { service.HandleUtterance("esp32", "task", tonePCM(16000, 1000)); close(finished) }()
	_ = device.SetReadDeadline(time.Now().Add(30 * time.Second))
	starts, ends, audioBytes := 0, 0, 0
	for {
		kind, body, err := device.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if kind == websocket.BinaryMessage {
			audioBytes += len(body)
			firstOnce.Do(func() { close(firstAudio) })
			continue
		}
		var control voiceControl
		_ = json.Unmarshal(body, &control)
		if control.Type == "audio_start" {
			starts++
		}
		if control.Type == "audio_end" {
			ends++
		}
		if control.Type == "audio_cancel" || control.State == "error" {
			t.Fatalf("stream failed: %+v", control)
		}
		if control.Type == "command_state" && control.State == "done" {
			break
		}
	}
	<-finished
	var got []string
	for len(segments) > 0 {
		got = append(got, <-segments)
	}
	if (liveBase == "" && (!reflect.DeepEqual(got, []string{"第一句。", "第二句。", "末尾"}) || audioBytes != 1920)) || starts != 1 || ends != 1 || audioBytes == 0 {
		t.Fatalf("segments=%q starts=%d ends=%d audio=%d", got, starts, ends, audioBytes)
	}
	row := readTestRecords(t, store)[0]
	if row.Status != "success" || row.Nanobot.Output != "第一句。第二句。末尾" || row.TTS.Input != row.Nanobot.Output || row.TTS.Segments != 3 || row.TTS.AudioBytes != int64(audioBytes) {
		t.Fatalf("stream record: %+v", row)
	}
}

func TestStreamingReplyCancelsOnTTSFailure(t *testing.T) {
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\""+strings.Repeat("第一句。", 20)+"\"}}]}\n\n")
		w.(http.Flusher).Flush()
		select {
		case <-r.Context().Done():
		case <-time.After(3 * time.Second):
			t.Error("LLM request did not cancel")
		}
	}))
	defer llm.Close()
	ttsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	defer ttsServer.Close()
	hub, device := processingDevice(t)
	tts, _ := NewAliyunTTSService(AliyunTTSConfig{Provider: "local", LocalURL: ttsServer.URL}, "test-token", hub)
	agent, _ := NewNanobotClient(NanobotConfig{Enabled: true, BaseURL: llm.URL, APIKey: "key"})
	store := testRecordStore(t)
	service := &VoiceCommandService{hub: hub, tts: tts, runner: agent, recognizer: &stubRecognizer{text: "问题"}, records: store}
	finished := make(chan struct{})
	go func() { service.HandleUtterance("esp32", "task", tonePCM(16000, 1000)); close(finished) }()
	_ = device.SetReadDeadline(time.Now().Add(5 * time.Second))
	cancelled := false
	for {
		kind, body, err := device.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if kind != websocket.TextMessage {
			continue
		}
		var control voiceControl
		_ = json.Unmarshal(body, &control)
		if control.Type == "audio_cancel" {
			cancelled = true
		}
		if control.State == "error" {
			break
		}
	}
	<-finished
	row := readTestRecords(t, store)[0]
	if !cancelled || row.Status != "error" || row.TTS.Status != "error" || !strings.Contains(row.TTS.Error, "503") {
		t.Fatalf("failed stream record: %+v cancelled=%v", row, cancelled)
	}
}

func TestStreamingReplySpokenLimitPreservesFullText(t *testing.T) {
	reply := strings.Repeat("长", 100)
	llm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\""+reply+"\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer llm.Close()
	var mu sync.Mutex
	var parts []string
	ttsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Text string `json:"text"`
		}
		_ = json.NewDecoder(r.Body).Decode(&request)
		mu.Lock()
		parts = append(parts, request.Text)
		mu.Unlock()
		w.Header().Set("X-Sample-Rate", "16000")
		_, _ = w.Write(tonePCM(16000, 20))
	}))
	defer ttsServer.Close()
	hub, device := processingDevice(t)
	tts, _ := NewAliyunTTSService(AliyunTTSConfig{Provider: "local", LocalURL: ttsServer.URL}, "token", hub)
	agent, err := NewNanobotClient(NanobotConfig{Enabled: true, BaseURL: llm.URL, APIKey: "key", SentenceMaxRunes: 20, MaxSpokenRunes: 25})
	if err != nil {
		t.Fatal(err)
	}
	store := testRecordStore(t)
	service := &VoiceCommandService{hub: hub, tts: tts, runner: agent, recognizer: &stubRecognizer{text: "问题"}, records: store}
	finished := make(chan struct{})
	go func() { service.HandleUtterance("esp32", "task", tonePCM(16000, 1000)); close(finished) }()
	_ = device.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		kind, body, err := device.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if kind != websocket.TextMessage {
			continue
		}
		var control voiceControl
		_ = json.Unmarshal(body, &control)
		if control.State == "error" {
			t.Fatalf("stream failed: %+v", control)
		}
		if control.State == "done" {
			break
		}
	}
	<-finished
	row := readTestRecords(t, store)[0]
	mu.Lock()
	defer mu.Unlock()
	if row.Nanobot.Output != reply || row.TTS.Input != strings.Repeat("长", 25) || row.TTS.Segments != 2 || !reflect.DeepEqual(parts, []string{strings.Repeat("长", 20), strings.Repeat("长", 5)}) {
		t.Fatalf("parts=%q record=%+v", parts, row)
	}
}

func TestStreamingSettings(t *testing.T) {
	disabled := false
	client, err := NewNanobotClient(NanobotConfig{Enabled: true, BaseURL: "http://localhost", APIKey: "key", Stream: &disabled})
	if err != nil || client.StreamingEnabled() {
		t.Fatal("stream=false ignored", err)
	}
	if client.SpeechSettings() != (speechStreamConfig{12, 80, 6000}) {
		t.Fatal("incorrect speech defaults")
	}
	for _, settings := range []NanobotConfig{{SentenceMaxRunes: 19}, {SentenceMaxRunes: 501}, {SentenceMinRunes: 90}, {MaxSpokenRunes: -1}, {MaxSpokenRunes: 60001}} {
		if _, err := normalizeSpeechSettings(settings); err == nil {
			t.Fatalf("invalid settings accepted: %+v", settings)
		}
	}
}
