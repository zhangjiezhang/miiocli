package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

type recordTestAgent struct {
	reply string
	err   error
}

func (a recordTestAgent) Timeout() time.Duration { return time.Second }
func (a recordTestAgent) Send(context.Context, string, string, string, func(string)) (string, error) {
	return a.reply, a.err
}

func readTestRecords(t *testing.T, s *VoiceRecordStore) []VoiceRecord {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.readLocked()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

func TestVoicePipelineRecordsFailures(t *testing.T) {
	for _, stage := range []string{"asr", "nanobot", "tts"} {
		t.Run(stage, func(t *testing.T) {
			s := testRecordStore(t)
			hub := NewVoiceHub(VoiceConfig{}) // Offline device fails TTS validation.
			recognizer := &stubRecognizer{text: "打开灯"}
			agent := recordTestAgent{reply: strings.Repeat("回复", 400)}
			if stage == "asr" {
				recognizer.err = errors.New("recognition failed")
			}
			if stage == "nanobot" {
				agent.err = errors.New("agent failed")
			}
			service := &VoiceCommandService{hub: hub, tts: &AliyunTTSService{hub: hub}, recognizer: recognizer, runner: agent, records: s}
			service.HandleUtterance("esp32", "task", make([]byte, 32000))
			rows := readTestRecords(t, s)
			if len(rows) != 1 {
				t.Fatalf("records: %d", len(rows))
			}
			row := rows[0]
			if row.Status != "error" || row.CompletedAt == nil || row.AudioDurationMS != 1000 {
				t.Fatalf("failed record: %+v", row)
			}
			var failed VoiceStage
			switch stage {
			case "asr":
				failed = row.ASR
			case "nanobot":
				failed = row.Nanobot
			case "tts":
				failed = row.TTS
			}
			if failed.Status != "error" || failed.Error == "" {
				t.Fatalf("failed stage not saved: %+v", failed)
			}
			if stage == "tts" && (row.Nanobot.Output != agent.reply || len([]rune(row.TTS.Input)) != maxVoiceResponse) {
				t.Fatal("complete reply or bounded spoken text lost")
			}
		})
	}
}

func TestVoicePipelineWaitsForTTSSynthesis(t *testing.T) {
	for _, fail := range []bool{false, true} {
		name := "success"
		if fail {
			name = "synthesis error"
		}
		t.Run(name, func(t *testing.T) {
			s := testRecordStore(t)
			hub := NewVoiceHub(VoiceConfig{Devices: map[string]VoiceDeviceConfig{"esp32": {Token: "device-token"}}})
			deviceServer := httptest.NewServer(hub)
			defer deviceServer.Close()
			conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(deviceServer.URL, "http")+hub.Path(), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.WriteJSON(voiceHello{Type: "hello", DeviceID: "esp32", Token: "device-token"}); err != nil {
				t.Fatal(err)
			}
			assertControl(t, conn, voiceControl{Type: "hello_ok", DeviceID: "esp32"})
			started := make(chan string, 1)
			release := make(chan struct{})
			var once sync.Once
			t.Cleanup(func() { once.Do(func() { close(release) }) })
			nlsServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
					return
				}
				started <- request.Payload.Text
				<-release
				// Ensure timing measures the asynchronous operation rather than enqueue.
				time.Sleep(25 * time.Millisecond)
				if fail {
					_ = ws.WriteMessage(websocket.TextMessage, []byte(`{"header":{"namespace":"SpeechSynthesizer","name":"TaskFailed"},"payload":{"message":"mock failure"}}`))
				} else {
					_ = ws.WriteMessage(websocket.BinaryMessage, make([]byte, 2048))
					_ = ws.WriteMessage(websocket.TextMessage, []byte(`{"header":{"namespace":"SpeechSynthesizer","name":"SynthesisCompleted"}}`))
				}
				// Let the client shut down the websocket after it processes completion.
				_, _, _ = ws.ReadMessage()
			}))
			defer nlsServer.Close()
			tts := &AliyunTTSService{hub: hub, cfg: AliyunTTSConfig{Token: "nls-token", AppKey: "app-key", Voice: "xiaoyun", Endpoint: "ws" + strings.TrimPrefix(nlsServer.URL, "http")}}
			reply := strings.Repeat("完整回复", 200)
			service := &VoiceCommandService{hub: hub, tts: tts, recognizer: &stubRecognizer{text: "问题"}, runner: recordTestAgent{reply: reply}, records: s}
			service.HandleUtterance("esp32", "task", make([]byte, 32000))
			select {
			case text := <-started:
				if len([]rune(text)) != maxVoiceResponse {
					t.Fatal("unexpected synthesis input")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("synthesis did not start")
			}
			rows := readTestRecords(t, s)
			if len(rows) != 1 || rows[0].Status != "processing" || rows[0].TTS.Status != "processing" || rows[0].CompletedAt != nil {
				t.Fatalf("record completed before synthesis: %+v", rows)
			}
			once.Do(func() { close(release) })
			deadline := time.Now().Add(3 * time.Second)
			for {
				rows = readTestRecords(t, s)
				if rows[0].Status != "processing" {
					break
				}
				if time.Now().After(deadline) {
					t.Fatal("record never completed")
				}
				time.Sleep(5 * time.Millisecond)
			}
			row := rows[0]
			wantStatus := "success"
			if fail {
				wantStatus = "error"
			}
			if row.Status != wantStatus || row.TTS.Status != wantStatus || row.TTS.DurationMS < 25 || row.TotalMS < row.TTS.DurationMS || row.CompletedAt == nil || row.Nanobot.Output != reply {
				t.Fatalf("completed record: %+v", row)
			}
			if !fail && (row.TTS.AudioBytes != 2048 || row.TTS.FirstAudioMS < 25) {
				t.Fatalf("audio metrics: %+v", row.TTS)
			}
			if fail && row.TTS.Error == "" {
				t.Fatal("synthesis failure lost")
			}
		})
	}
}

func TestDirectSpeakProducesTTSOnlyFailureRecord(t *testing.T) {
	s := testRecordStore(t)
	tts := &AliyunTTSService{hub: NewVoiceHub(VoiceConfig{}), records: s}
	if _, err := tts.Speak("offline", "播报内容", ""); err == nil {
		t.Fatal("offline Speak accepted")
	}
	w := recordRequest(s, "GET", "/v1/voice/records", "", "test-token")
	var payload struct {
		Records []VoiceRecord `json:"records"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Records) != 1 || payload.Records[0].Source != "speak" || payload.Records[0].TTS.Input != "播报内容" || payload.Records[0].Status != "error" || payload.Records[0].ASR.Status != "" {
		t.Fatalf("direct Speak record: %s", w.Body.String())
	}
}
