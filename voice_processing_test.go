package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func tonePCM(rate, ms int) []byte {
	pcm := make([]byte, rate*ms/1000*2)
	for i := 0; i < len(pcm)/2; i++ {
		binary.LittleEndian.PutUint16(pcm[i*2:], uint16(int16(5000*math.Sin(2*math.Pi*440*float64(i)/float64(rate)))))
	}
	return pcm
}

func TestVADSilenceClickDCAndSpeech(t *testing.T) {
	vad, err := newVAD(VADConfig{Enabled: true, PaddingMS: 40})
	if err != nil {
		t.Fatal(err)
	}
	dc := make([]byte, 32000)
	for i := 0; i < len(dc); i += 2 {
		binary.LittleEndian.PutUint16(dc[i:], 5000)
	}
	for name, pcm := range map[string][]byte{"silence": make([]byte, 32000), "DC": dc, "click": append(tonePCM(16000, 20), make([]byte, 16000)...)} {
		t.Run(name, func(t *testing.T) {
			_, speech, err := vad.Filter(pcm)
			if err != nil || speech {
				t.Fatalf("speech=%v err=%v", speech, err)
			}
		})
	}
	pcm := append(make([]byte, 16000), tonePCM(16000, 200)...)
	pcm = append(pcm, make([]byte, 3200)...)
	pcm = append(pcm, tonePCM(16000, 200)...)
	pcm = append(pcm, make([]byte, 16000)...)
	filtered, speech, err := vad.Filter(pcm)
	if err != nil || !speech || len(filtered) != 18560 {
		t.Fatalf("trimmed speech bytes=%d speech=%v err=%v", len(filtered), speech, err)
	}
	if !bytes.Equal(filtered[7680:10880], make([]byte, 3200)) {
		t.Fatal("internal pause was removed")
	}
	if _, _, err := vad.Filter([]byte{1}); err == nil {
		t.Fatal("malformed PCM accepted")
	}
}

func TestResamplerChunkBoundaries(t *testing.T) {
	for _, rate := range []int{8000, 16000, 22050, 48000} {
		pcm := tonePCM(rate, 1000)
		whole := (&pcmResampler{rate: int64(rate)}).push(pcm)
		resampler := pcmResampler{rate: int64(rate)}
		var fragmented []byte
		for start := 0; start < len(pcm); start += 513 {
			end := minInt(len(pcm), start+513)
			fragmented = append(fragmented, resampler.push(pcm[start:end])...)
		}
		if !bytes.Equal(whole, fragmented) || len(resampler.carry) != 0 {
			t.Fatalf("chunk-dependent output at %d Hz", rate)
		}
		if math.Abs(float64(len(whole)-32000)) > 4 {
			t.Fatalf("wrong duration at %d Hz: %d", rate, len(whole))
		}
	}
}

func processingDevice(t *testing.T) (*VoiceHub, *websocket.Conn) {
	t.Helper()
	hub := NewVoiceHub(VoiceConfig{Devices: map[string]VoiceDeviceConfig{"esp32": {Token: "test-device-token"}}})
	server := httptest.NewServer(hub)
	t.Cleanup(server.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+hub.Path(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	if err := conn.WriteJSON(voiceHello{Type: "hello", DeviceID: "esp32", Token: "test-device-token"}); err != nil {
		t.Fatal(err)
	}
	assertControl(t, conn, voiceControl{Type: "hello_ok", DeviceID: "esp32"})
	return hub, conn
}

func TestLocalTTSDeviceStream(t *testing.T) {
	for _, bad := range []bool{false, true} {
		t.Run(map[bool]string{false: "long text", true: "truncated PCM"}[bad], func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				var req struct {
					Text  string  `json:"text"`
					Speed float64 `json:"speed"`
				}
				if r.URL.Path != "/tts/stream" || r.Method != "POST" || json.NewDecoder(r.Body).Decode(&req) != nil || len([]rune(req.Text)) > 500 || req.Speed != 1 {
					t.Error("invalid local TTS request")
					w.WriteHeader(400)
					return
				}
				w.Header().Set("X-Sample-Rate", "22050")
				pcm := tonePCM(22050, 1000)
				if bad {
					pcm = append(pcm, 1)
				}
				for start := 0; start < len(pcm); start += 513 {
					_, _ = w.Write(pcm[start:minInt(start+513, len(pcm))])
					w.(http.Flusher).Flush()
				}
			}))
			defer server.Close()
			hub, conn := processingDevice(t)
			tts, err := NewAliyunTTSService(AliyunTTSConfig{Provider: "local", LocalURL: server.URL}, "test-api-token", hub)
			if err != nil {
				t.Fatal(err)
			}
			type result struct {
				stats TTSStats
				err   error
			}
			done := make(chan result, 1)
			go func() {
				stats, err := tts.synthesizeLocal("esp32", 1, strings.Repeat("你", 600))
				done <- result{stats, err}
			}()
			_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
			assertControl(t, conn, voiceControl{Type: "audio_start", StreamID: 1, Codec: codecPCMS16LE, SampleRate: 16000, Channels: 1, BitsPerSample: 16})
			var audio []byte
			for {
				kind, data, err := conn.ReadMessage()
				if err != nil {
					t.Fatal(err)
				}
				if kind == websocket.BinaryMessage {
					if len(data) > 1024 || len(data)%2 != 0 {
						t.Fatal("invalid device audio frame")
					}
					audio = append(audio, data...)
					continue
				}
				var event voiceControl
				_ = json.Unmarshal(data, &event)
				want := "audio_end"
				if bad {
					want = "audio_cancel"
				}
				if event.Type != want {
					t.Fatalf("unexpected event %+v", event)
				}
				break
			}
			got := <-done
			wantBytes := 64000
			wantCalls := int32(2)
			if bad {
				wantBytes = 32000
				wantCalls = 1
			}
			if (got.err != nil) != bad || len(audio) != wantBytes || got.stats.AudioBytes != int64(wantBytes) || calls.Load() != wantCalls {
				t.Fatalf("audio=%d calls=%d stats=%+v err=%v", len(audio), calls.Load(), got.stats, got.err)
			}
		})
	}
}

func TestVPProtocolAndThreshold(t *testing.T) {
	pcm := tonePCM(16000, 1000)
	for _, tc := range []struct {
		name, body      string
		status          int
		matched, failed bool
	}{
		{"match", `{"speaker_id":"alice","score":0.8}`, 200, true, false},
		{"low score", `{"speaker_id":"alice","score":0.1}`, 200, false, false},
		{"unexpected ID", `{"speaker_id":"other","score":0.8}`, 200, false, false},
		{"missing score", `{"speaker_id":"alice"}`, 200, false, true},
		{"unauthorized", `secret response`, 401, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/voiceprint/identify" || r.Header.Get("Authorization") != "Bearer private-key" {
					t.Error("VP URL/auth mismatch")
				}
				if err := r.ParseMultipartForm(2 << 20); err != nil {
					t.Error(err)
					return
				}
				defer r.MultipartForm.RemoveAll()
				if r.FormValue("speaker_ids") != "alice" {
					t.Error("candidate IDs missing")
				}
				file, header, err := r.FormFile("file")
				if err != nil {
					t.Error(err)
					return
				}
				defer file.Close()
				data, _ := io.ReadAll(file)
				if header.Header.Get("Content-Type") != "audio/wav" || !bytes.Equal(data[44:], pcm) || string(data[:4]) != "RIFF" || binary.LittleEndian.Uint32(data[24:]) != 16000 {
					t.Error("invalid WAV")
				}
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			vp, err := newVoiceprintClient(VoiceprintConfig{Enabled: true, BaseURL: server.URL, APIKey: "private-key", Speakers: []VoiceprintSpeaker{{ID: "alice", Name: "Alice"}}})
			if err != nil {
				t.Fatal(err)
			}
			identity, err := vp.Identify(context.Background(), pcm)
			if (err != nil) != tc.failed || identity.Matched != tc.matched {
				t.Fatalf("identity=%+v err=%v", identity, err)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatal("response body leaked")
			}
		})
	}
}

func TestPipelineSilenceSkipsProviders(t *testing.T) {
	store := testRecordStore(t)
	vad, _ := newVAD(VADConfig{Enabled: true})
	recognizer := &stubRecognizer{text: "should not be used"}
	service := &VoiceCommandService{hub: NewVoiceHub(VoiceConfig{}), vad: vad, recognizer: recognizer, records: store}
	service.HandleUtterance("esp32", "silence", make([]byte, 32000))
	rows := readTestRecords(t, store)
	if len(rows) != 1 || rows[0].Status != "ignored" || rows[0].ASR.Status != "" || rows[0].VP.Status != "" || rows[0].Nanobot.Status != "" {
		t.Fatalf("silence pipeline: %+v", rows)
	}
}

type concurrentRecognizer struct {
	vpStarted  <-chan struct{}
	asrStarted chan<- struct{}
}

func (r concurrentRecognizer) Transcribe(ctx context.Context, _ []byte) (string, error) {
	close(r.asrStarted)
	select {
	case <-r.vpStarted:
		return "你好", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

type speakerTestAgent struct{ received chan SpeakerIdentity }

func (a speakerTestAgent) Timeout() time.Duration { return time.Second }
func (a speakerTestAgent) Send(ctx context.Context, _, _, _ string, _ func(string)) (string, error) {
	speaker, _ := ctx.Value(speakerContextKey{}).(SpeakerIdentity)
	a.received <- speaker
	return "好的", nil
}

func TestPipelineVPConcurrentContextAndFailurePolicy(t *testing.T) {
	for _, tc := range []struct {
		name, body      string
		require, called bool
	}{
		{"match", `{"speaker_id":"alice","score":0.9}`, false, true},
		{"unknown allowed", `{"speaker_id":"alice","score":0.1}`, false, true},
		{"unknown blocked", `{"speaker_id":"alice","score":0.1}`, true, false},
		{"failure allowed", `invalid`, false, true},
		{"failure blocked", `invalid`, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vpStarted, asrStarted := make(chan struct{}), make(chan struct{})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(vpStarted)
				select {
				case <-asrStarted:
				case <-time.After(time.Second):
					t.Error("ASR and VP did not run concurrently")
				}
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			vp, err := newVoiceprintClient(VoiceprintConfig{Enabled: true, BaseURL: server.URL, APIKey: "private-key", RequireMatch: tc.require, Speakers: []VoiceprintSpeaker{{ID: "alice", Name: "Alice"}}})
			if err != nil {
				t.Fatal(err)
			}
			store := testRecordStore(t)
			hub := NewVoiceHub(VoiceConfig{})
			agent := speakerTestAgent{received: make(chan SpeakerIdentity, 1)}
			service := &VoiceCommandService{hub: hub, tts: &AliyunTTSService{hub: hub}, recognizer: concurrentRecognizer{vpStarted, asrStarted}, vp: vp, runner: agent, records: store}
			service.HandleUtterance("esp32", "task", tonePCM(16000, 1000))
			if (len(agent.received) > 0) != tc.called {
				t.Fatalf("agent invocation differs from policy: %v", tc)
			}
			if tc.name == "match" {
				speaker := <-agent.received
				if !speaker.Matched || speaker.ID != "alice" {
					t.Fatal("speaker not passed to agent")
				}
			}
			row := readTestRecords(t, store)[0]
			if row.VP.Status == "processing" || row.ASR.Status != "success" {
				t.Fatalf("stages not completed: %+v", row)
			}
			if tc.require && row.Nanobot.Status != "" {
				t.Fatal("agent ran despite required match")
			}
		})
	}
}

func TestNanobotReceivesSpeakerMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request nanobotChatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
		}
		if len(request.Messages) != 2 || request.Messages[0].Role != "system" || !strings.Contains(request.Messages[0].Content, `"name":"Alice"`) || request.Messages[1].Content != "你好" || request.User != "esp32:device" {
			t.Errorf("invalid speaker context: %+v", request)
		}
		io.WriteString(w, `{"choices":[{"message":{"role":"assistant","content":"你好"}}]}`)
	}))
	defer server.Close()
	client, err := NewNanobotClient(NanobotConfig{Enabled: true, BaseURL: server.URL, APIKey: "test-key"})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.WithValue(context.Background(), speakerContextKey{}, SpeakerIdentity{VoiceprintSpeaker: VoiceprintSpeaker{ID: "alice", Name: "Alice"}, Matched: true, Score: .9})
	if _, err := client.Send(ctx, "task", "device", "你好", nil); err != nil {
		t.Fatal(err)
	}
}

func TestVoiceProviderConfigValidation(t *testing.T) {
	hub := NewVoiceHub(VoiceConfig{})
	if _, err := NewAliyunTTSService(AliyunTTSConfig{Provider: "local"}, "api-token", hub); err != nil {
		t.Fatal("local TTS should not require cloud credentials", err)
	}
	for _, cfg := range []AliyunTTSConfig{{Provider: "auto"}, {Provider: "local", LocalURL: "http://user:secret@localhost"}, {Provider: "local", LocalSpeed: 2}, {Provider: "local", TimeoutSeconds: -1}, {}} {
		if _, err := NewAliyunTTSService(cfg, "api-token", hub); err == nil {
			t.Fatalf("invalid TTS config accepted: %+v", cfg)
		}
	}
	t.Setenv("MIIOCLI_VP_API_KEY", "test-env-key")
	vp, err := newVoiceprintClient(VoiceprintConfig{Enabled: true, Speakers: []VoiceprintSpeaker{{ID: "alice"}}})
	if err != nil || vp.cfg.APIKey != "test-env-key" {
		t.Fatal("VP environment key not loaded", err)
	}
	if _, err := newVoiceprintClient(VoiceprintConfig{Enabled: true, Speakers: []VoiceprintSpeaker{{ID: "alice"}, {ID: "alice"}}}); err == nil {
		t.Fatal("duplicate speaker IDs accepted")
	}
}

// Optional live test uses a simulated device, so it never plays audio on a real ESP32.
func TestLiveLocalTTS(t *testing.T) {
	base := os.Getenv("MIIOCLI_TEST_LOCAL_TTS_URL")
	if base == "" {
		t.Skip("MIIOCLI_TEST_LOCAL_TTS_URL not set")
	}
	hub, conn := processingDevice(t)
	tts, err := NewAliyunTTSService(AliyunTTSConfig{Provider: "local", LocalURL: base}, "test-token", hub)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		stats, err := tts.synthesizeLocal("esp32", 1, "你好，这是本地语音合成接口测试。")
		if err == nil && stats.AudioBytes == 0 {
			err = io.ErrUnexpectedEOF
		}
		done <- err
	}()
	_ = conn.SetReadDeadline(time.Now().Add(30 * time.Second))
	for {
		kind, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatal(err)
		}
		if kind == websocket.TextMessage {
			var event voiceControl
			_ = json.Unmarshal(data, &event)
			if event.Type == "audio_end" || event.Type == "audio_cancel" {
				break
			}
		}
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
