package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNanobotClientSendsOpenAIRequest(t *testing.T) {
	var got nanobotChatRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if authorization := r.Header.Get("Authorization"); authorization != "Bearer test-secret" {
			t.Errorf("Authorization = %q", authorization)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"  已完成  "}}]}`))
	}))
	defer server.Close()

	client, err := NewNanobotClient(NanobotConfig{
		Enabled: true, BaseURL: server.URL, APIKey: "test-secret", Model: "nanobot",
	})
	if err != nil {
		t.Fatalf("create client: %v", err)
	}
	var progress string
	response, err := client.Send(context.Background(), "task-1", "szp-001", "打开灯", func(message string) {
		progress = message
	})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if response != "已完成" || progress != "Nanobot 正在处理" {
		t.Fatalf("response = %q, progress = %q", response, progress)
	}
	if got.Model != "nanobot" || got.User != "esp32:szp-001" || got.Stream || len(got.Messages) != 1 ||
		got.Messages[0].Role != "user" || got.Messages[0].Content != "打开灯" {
		t.Fatalf("request = %#v", got)
	}
}

func TestNanobotClientOmitsEmptyModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		if _, exists := payload["model"]; exists {
			t.Error("empty model was included")
		}
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}]}`))
	}))
	defer server.Close()
	client, err := NewNanobotClient(NanobotConfig{Enabled: true, BaseURL: server.URL + "/v1", APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Send(context.Background(), "", "device", "hello", nil); err != nil {
		t.Fatal(err)
	}
}

func TestNanobotClientErrorsAreBoundedAndRedacted(t *testing.T) {
	secret := "top-secret"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(secret + strings.Repeat("x", 400)))
	}))
	defer server.Close()
	client, err := NewNanobotClient(NanobotConfig{Enabled: true, BaseURL: server.URL, APIKey: secret})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Send(context.Background(), "", "device", "hello", nil)
	if err == nil || strings.Contains(err.Error(), secret) || len([]rune(err.Error())) > 260 {
		t.Fatalf("unexpected error: %q", err)
	}
}

func TestNanobotClientRejectsEmptyResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[]}`))
	}))
	defer server.Close()
	client, err := NewNanobotClient(NanobotConfig{Enabled: true, BaseURL: server.URL, APIKey: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Send(context.Background(), "", "device", "hello", nil); err == nil {
		t.Fatal("expected empty response error")
	}
}

func TestNanobotClientHonorsContextCancellation(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(100 * time.Millisecond)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"too late"}}]}`))
	}))
	defer server.Close()
	client, err := NewNanobotClient(NanobotConfig{
		Enabled: true, BaseURL: server.URL, APIKey: "secret", TimeoutSeconds: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := client.Send(ctx, "", "device", "hello", nil); err == nil {
		t.Fatal("expected context cancellation")
	}
}

func TestNanobotSessionID(t *testing.T) {
	if got := nanobotSessionID("szp-001"); got != "esp32:szp-001" {
		t.Fatalf("valid device session = %q", got)
	}
	got := nanobotSessionID("设备 1")
	if !regexpSessionID(got) || got == "esp32:设备 1" {
		t.Fatalf("sanitized device session = %q", got)
	}
}

func regexpSessionID(value string) bool {
	return nanobotSessionPattern.MatchString(strings.TrimPrefix(value, "esp32:")) && strings.HasPrefix(value, "esp32:")
}
