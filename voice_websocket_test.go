package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestVoiceInputLoggingDefaultsOff(t *testing.T) {
	hub := NewVoiceHub(VoiceConfig{})
	var output bytes.Buffer
	previousOutput := log.Writer()
	previousFlags := log.Flags()
	log.SetOutput(&output)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(previousOutput)
		log.SetFlags(previousFlags)
	}()

	hub.logVoiceInput("received device=%s", "device-1")
	if output.Len() != 0 {
		t.Fatalf("default logging output = %q, want empty", output.String())
	}

	hub.logInput = true
	hub.logVoiceInput("received device=%s", "device-1")
	if got := output.String(); !strings.Contains(got, "ESP32 voice input: received device=device-1") {
		t.Fatalf("enabled logging output = %q", got)
	}
}

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

func TestVoiceHubReceivesDeviceUtterance(t *testing.T) {
	hub := NewVoiceHub(VoiceConfig{Devices: map[string]VoiceDeviceConfig{
		"device-1": {Token: "device-token"},
	}})
	type capturedUtterance struct {
		deviceID    string
		utteranceID string
		pcm         []byte
	}
	captured := make(chan capturedUtterance, 1)
	hub.SetUtteranceHandler(func(deviceID, utteranceID string, pcm []byte) {
		captured <- capturedUtterance{deviceID: deviceID, utteranceID: utteranceID, pcm: pcm}
	})
	server := httptest.NewServer(hub)
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

	start := voiceControl{Type: "listen_start", UtteranceID: "utt-1", Codec: codecPCMS16LE,
		SampleRate: 16000, Channels: 1, BitsPerSample: 16}
	if err := conn.WriteJSON(start); err != nil {
		t.Fatalf("send listen_start: %v", err)
	}
	assertControl(t, conn, voiceControl{Type: "listen_ready", UtteranceID: "utt-1"})
	pcm := make([]byte, 1600)
	for i := range pcm {
		pcm[i] = byte(i)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, pcm[:800]); err != nil {
		t.Fatalf("send first PCM frame: %v", err)
	}
	if err := conn.WriteMessage(websocket.BinaryMessage, pcm[800:]); err != nil {
		t.Fatalf("send second PCM frame: %v", err)
	}
	if err := conn.WriteJSON(voiceControl{Type: "listen_end", UtteranceID: "utt-1"}); err != nil {
		t.Fatalf("send listen_end: %v", err)
	}
	assertControl(t, conn, voiceControl{Type: "listen_received", UtteranceID: "utt-1"})

	select {
	case got := <-captured:
		if got.deviceID != "device-1" || got.utteranceID != "utt-1" || !bytes.Equal(got.pcm, pcm) {
			t.Fatalf("unexpected capture: device=%q utterance=%q bytes=%d", got.deviceID, got.utteranceID, len(got.pcm))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for utterance handler")
	}
}

func TestVoiceHubSwitchesCodexSession(t *testing.T) {
	hub := NewVoiceHub(VoiceConfig{Devices: map[string]VoiceDeviceConfig{
		"device-1": {Token: "device-token"},
	}})
	type sessionAction struct {
		deviceID string
		action   string
	}
	actions := make(chan sessionAction, 2)
	hub.SetCodexSessionHandler(func(deviceID, action, _ string) {
		actions <- sessionAction{deviceID: deviceID, action: action}
	})
	server := httptest.NewServer(hub)
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
	select {
	case action := <-actions:
		if action.deviceID != "device-1" || action.action != "session_list" {
			t.Fatalf("hello session action = %#v", action)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for initial session list")
	}

	if err := conn.WriteJSON(voiceControl{Type: "session_next"}); err != nil {
		t.Fatalf("send session_next: %v", err)
	}
	select {
	case action := <-actions:
		if action.deviceID != "device-1" || action.action != "session_next" {
			t.Fatalf("session action = %#v", action)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for session switch")
	}

	selected := voiceSessionOption{ID: "session-1", Title: "Firmware", Surface: "cli", State: "running"}
	if err := hub.SendCodexSessionSelected("device-1", selected); err != nil {
		t.Fatalf("send selected session: %v", err)
	}
	var control voiceControl
	if err := conn.ReadJSON(&control); err != nil {
		t.Fatalf("read selected session: %v", err)
	}
	if control.Type != "session_selected" || control.SessionID != selected.ID ||
		control.Message != selected.Title || control.Surface != selected.Surface {
		t.Fatalf("selected session control = %#v", control)
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
	if got.Type != want.Type || got.DeviceID != want.DeviceID || got.StreamID != want.StreamID ||
		got.UtteranceID != want.UtteranceID {
		t.Fatalf("control = %#v, want %#v", got, want)
	}
}
