package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"cn.pascall/gomiio/internal/codexprotocol"
)

func TestCodexSessionServiceRoutesBoundDeviceMessage(t *testing.T) {
	const connectorToken = "connector-secret"
	service, err := NewCodexSessionService(CodexConfig{
		Enabled: true, Token: connectorToken, CallbackBaseURL: "http://miiocli.test",
	}, "api-secret", map[string]VoiceDeviceConfig{"device-1": {Token: "device-secret"}})
	if err != nil {
		t.Fatalf("create service: %v", err)
	}

	connector := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+connectorToken {
			t.Errorf("connector authorization = %q", r.Header.Get("Authorization"))
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var message codexprotocol.SessionMessage
		if err := json.NewDecoder(r.Body).Decode(&message); err != nil {
			t.Errorf("decode message: %v", err)
			http.Error(w, "invalid", http.StatusBadRequest)
			return
		}
		if message.SessionID != "session-1" || message.ThreadID != "thread-1" ||
			message.ExpectedTurnID != "turn-1" || message.Source != "esp32" ||
			message.DeviceID != "device-1" || message.Text != "运行测试" {
			t.Errorf("unexpected message: %#v", message)
		}
		w.WriteHeader(http.StatusAccepted)
		go sendSessionEvent(t, service, connectorToken, codexprotocol.SessionEvent{
			MessageID: message.MessageID, SessionID: message.SessionID, ThreadID: message.ThreadID,
			TurnID: "turn-1", State: codexprotocol.StateWorking, Message: "Codex 正在执行",
		})
		go func() {
			time.Sleep(10 * time.Millisecond)
			sendSessionEvent(t, service, connectorToken, codexprotocol.SessionEvent{
				MessageID: message.MessageID, SessionID: message.SessionID, ThreadID: message.ThreadID,
				TurnID: "turn-1", State: codexprotocol.StateDone, Result: "测试通过",
			})
		}()
	}))
	defer connector.Close()

	registerSession(t, service, connectorToken, codexprotocol.SessionRegistration{
		SessionID: "session-1", ThreadID: "thread-1", TurnID: "turn-1", Surface: "cli",
		Title: "Firmware", Project: "lichuang-esp32s3", State: "running",
		Endpoint: connector.URL, Capabilities: []string{"turn_start", "turn_steer"},
	})
	bindSession(t, service, "api-secret", "session-1", "device-1")

	progress := make(chan string, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	result, err := service.Send(ctx, "message-1", "device-1", "运行测试", func(message string) {
		progress <- message
	})
	if err != nil || result != "测试通过" {
		t.Fatalf("result = %q, err = %v", result, err)
	}
	select {
	case message := <-progress:
		if message != "Codex 正在执行" {
			t.Fatalf("progress = %q", message)
		}
	default:
		t.Fatal("missing progress callback")
	}
}

func TestCodexSessionServiceRequiresSelectionForMultipleSessions(t *testing.T) {
	service, err := NewCodexSessionService(CodexConfig{
		Enabled: true, Token: "connector-secret", CallbackBaseURL: "http://miiocli.test",
	}, "api-secret", map[string]VoiceDeviceConfig{"device-1": {Token: "device-secret"}})
	if err != nil {
		t.Fatalf("create service: %v", err)
	}
	for _, sessionID := range []string{"session-1", "session-2"} {
		registerSession(t, service, "connector-secret", codexprotocol.SessionRegistration{
			SessionID: sessionID, ThreadID: "thread-" + sessionID, Surface: "sdk",
			State: "idle", Endpoint: "http://connector.test/messages",
		})
	}
	_, err = service.Send(context.Background(), "message-1", "device-1", "运行测试", nil)
	if err == nil || !strings.Contains(err.Error(), "select one") {
		t.Fatalf("error = %v", err)
	}
}

func TestCodexSessionConnectorAuthentication(t *testing.T) {
	service, err := NewCodexSessionService(CodexConfig{
		Enabled: true, Token: "connector-secret", CallbackBaseURL: "http://miiocli.test",
	}, "api-secret", map[string]VoiceDeviceConfig{})
	if err != nil {
		t.Fatalf("create service: %v", err)
	}
	body, _ := json.Marshal(codexprotocol.SessionRegistration{
		SessionID: "session-1", ThreadID: "thread-1", Surface: "app",
		State: "idle", Endpoint: "http://connector.test/messages",
	})
	request := httptest.NewRequest(http.MethodPost, service.RegisterPath(), bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer wrong-secret")
	response := httptest.NewRecorder()
	service.HandleRegister(response, request)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestCodexSessionWebMessageRejectsUnavailableSession(t *testing.T) {
	service, err := NewCodexSessionService(CodexConfig{
		Enabled: true, Token: "connector-secret", CallbackBaseURL: "http://miiocli.test",
		SessionTTLSeconds: 1,
	}, "api-secret", map[string]VoiceDeviceConfig{})
	if err != nil {
		t.Fatalf("create service: %v", err)
	}

	request := httptest.NewRequest(http.MethodPost,
		"/v1/codex/sessions/missing/messages", strings.NewReader(`{"text":"test"}`))
	request.Header.Set("Authorization", "Bearer api-secret")
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing session status = %d, want %d", response.Code, http.StatusNotFound)
	}

	registerSession(t, service, "connector-secret", codexprotocol.SessionRegistration{
		SessionID: "session-1", ThreadID: "thread-1", Surface: "cli",
		State: "idle", Endpoint: "http://connector.test/messages",
	})
	service.mu.Lock()
	service.sessions["session-1"].lastSeen = time.Now().Add(-2 * time.Second)
	service.mu.Unlock()

	request = httptest.NewRequest(http.MethodPost,
		"/v1/codex/sessions/session-1/messages", strings.NewReader(`{"text":"test"}`))
	request.Header.Set("Authorization", "Bearer api-secret")
	response = httptest.NewRecorder()
	service.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("offline session status = %d, want %d", response.Code, http.StatusConflict)
	}
}

func registerSession(t *testing.T, service *CodexSessionService, token string,
	registration codexprotocol.SessionRegistration) {
	t.Helper()
	body, _ := json.Marshal(registration)
	request := httptest.NewRequest(http.MethodPost, service.RegisterPath(), bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	service.HandleRegister(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("register status = %d: %s", response.Code, response.Body.String())
	}
}

func bindSession(t *testing.T, service *CodexSessionService, token, sessionID, deviceID string) {
	t.Helper()
	body, _ := json.Marshal(codexBindingInput{DeviceID: deviceID})
	request := httptest.NewRequest(http.MethodPost,
		"/v1/codex/sessions/"+sessionID+"/bind", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	service.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("bind status = %d: %s", response.Code, response.Body.String())
	}
}

func sendSessionEvent(t *testing.T, service *CodexSessionService, token string,
	event codexprotocol.SessionEvent) {
	t.Helper()
	body, _ := json.Marshal(event)
	request := httptest.NewRequest(http.MethodPost, service.EventPath(), bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	service.HandleEvent(response, request)
	if response.Code != http.StatusNoContent {
		t.Errorf("event status = %d: %s", response.Code, response.Body.String())
	}
}
