package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestVoiceHubStreamsPCMFrames(t *testing.T) {
	hub := NewVoiceHub(VoiceConfig{Devices: map[string]VoiceDeviceConfig{
		"device-1": {Token: "device-token"},
	}})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != hub.Path() {
			http.NotFound(w, r)
			return
		}
		hub.ServeHTTP(w, r)
	}))
	defer server.Close()

	wsURL := "ws" + strings.TrimPrefix(server.URL, "http") + hub.Path()
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	if err := conn.WriteJSON(voiceHello{Type: "hello", DeviceID: "device-1", Token: "device-token"}); err != nil {
		t.Fatalf("send hello: %v", err)
	}
	assertControl(t, conn, voiceControl{Type: "hello_ok", DeviceID: "device-1"})

	if err := hub.StartPCM("device-1", 7); err != nil {
		t.Fatalf("start PCM: %v", err)
	}
	messageType, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read audio start: %v", err)
	}
	if messageType != websocket.TextMessage {
		t.Fatalf("audio start type = %d, want text", messageType)
	}
	var start voiceControl
	if err := json.Unmarshal(payload, &start); err != nil {
		t.Fatalf("decode audio start: %v", err)
	}
	if start.Type != "audio_start" || start.StreamID != 7 || start.Codec != codecPCMS16LE ||
		start.SampleRate != 16000 || start.Channels != 1 || start.BitsPerSample != 16 {
		t.Fatalf("unexpected audio start: %#v", start)
	}

	pcm := make([]byte, 2050)
	if err := hub.SendPCM("device-1", 7, pcm); err != nil {
		t.Fatalf("send PCM: %v", err)
	}
	for _, want := range []int{1024, 1024, 2} {
		messageType, payload, err = conn.ReadMessage()
		if err != nil {
			t.Fatalf("read PCM frame: %v", err)
		}
		if messageType != websocket.BinaryMessage || len(payload) != want {
			t.Fatalf("PCM frame = type %d, length %d; want binary, length %d", messageType, len(payload), want)
		}
	}
}

func assertControl(t *testing.T, conn *websocket.Conn, want voiceControl) {
	t.Helper()
	messageType, payload, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read control: %v", err)
	}
	if messageType != websocket.TextMessage {
		t.Fatalf("control type = %d, want text", messageType)
	}
	var got voiceControl
	if err := json.Unmarshal(payload, &got); err != nil {
		t.Fatalf("decode control: %v", err)
	}
	if got.Type != want.Type || got.DeviceID != want.DeviceID || got.StreamID != want.StreamID {
		t.Fatalf("control = %#v, want %#v", got, want)
	}
}
