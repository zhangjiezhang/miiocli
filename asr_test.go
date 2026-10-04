package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLocalASRRequest(t *testing.T) {
	pcm := make([]byte, 3200)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.URL.Path != "/v1/transcribe" || r.Method != "POST" ||
			r.Header.Get("Authorization") != "Bearer test-token" || r.Header.Get("Content-Type") != "audio/pcm" ||
			!bytes.Equal(body, pcm) {
			t.Error("incorrect local ASR request")
		}
		io.WriteString(w, `{"text":"  查询服务器状态  ","inference_ms":220}`)
	}))
	defer server.Close()
	r, err := newSpeechRecognizer(ASRConfig{Provider: "local", LocalURL: server.URL, LocalToken: "test-token"}, AliyunTTSConfig{})
	if err != nil {
		t.Fatal(err)
	}
	text, err := r.Transcribe(context.Background(), pcm)
	if err != nil || text != "查询服务器状态" {
		t.Fatalf("text=%q error=%v", text, err)
	}
}

func TestLocalASRFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"unauthorized", 401, "secret-provider-details"},
		{"busy", 429, ""}, {"unavailable", 503, ""},
		{"invalid_json", 200, "not JSON"}, {"empty", 200, `{"text":" "}`},
		{"oversized", 200, strings.Repeat("a", (64<<10)+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				io.WriteString(w, tc.body)
			}))
			defer server.Close()
			r, err := newSpeechRecognizer(ASRConfig{Provider: "local", LocalURL: server.URL, LocalToken: "test"}, AliyunTTSConfig{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = r.Transcribe(context.Background(), make([]byte, 3200))
			if err == nil || strings.Contains(err.Error(), "secret-provider-details") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestLocalASRTimeoutAndRedirect(t *testing.T) {
	targetCalled := false
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { targetCalled = true }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL, http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	r, _ := newSpeechRecognizer(ASRConfig{Provider: "local", LocalURL: server.URL, LocalToken: "test"}, AliyunTTSConfig{})
	if _, err := r.Transcribe(context.Background(), make([]byte, 3200)); err == nil || targetCalled {
		t.Fatal("redirect followed")
	}
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(200 * time.Millisecond):
		}
	}))
	defer slow.Close()
	r, _ = newSpeechRecognizer(ASRConfig{Provider: "local", LocalURL: slow.URL, LocalToken: "test"}, AliyunTTSConfig{})
	r.(*localRecognizer).client.Timeout = 20 * time.Millisecond
	if _, err := r.Transcribe(context.Background(), make([]byte, 3200)); err == nil {
		t.Fatal("timeout not enforced")
	}
}

type stubRecognizer struct {
	text  string
	err   error
	calls int
}

func (r *stubRecognizer) Transcribe(context.Context, []byte) (string, error) {
	r.calls++
	return r.text, r.err
}

func TestASRFallback(t *testing.T) {
	local := &stubRecognizer{text: "本地结果"}
	cloud := &stubRecognizer{text: "云端结果"}
	r := &fallbackRecognizer{local: local, aliyun: cloud}
	text, err := r.Transcribe(context.Background(), nil)
	if err != nil || text != "本地结果" || cloud.calls != 0 {
		t.Fatal("successful local result should not use cloud")
	}
	local.err = errors.New("unavailable")
	text, err = r.Transcribe(context.Background(), nil)
	if err != nil || text != "云端结果" || cloud.calls != 1 {
		t.Fatal("fallback failed")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = r.Transcribe(ctx, nil)
	if !errors.Is(err, context.Canceled) || cloud.calls != 1 {
		t.Fatal("canceled request fell back")
	}
	cloud.err = errors.New("cloud unavailable")
	if _, err = r.Transcribe(context.Background(), nil); err == nil {
		t.Fatal("cloud error lost")
	}
}

func TestASRConfiguration(t *testing.T) {
	cloud := AliyunTTSConfig{AppKey: "app", Token: "token"}
	r, err := newSpeechRecognizer(ASRConfig{}, cloud)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := r.(*aliyunRecognizer); !ok {
		t.Fatal("default changed")
	}
	t.Setenv("MIIOCLI_LOCAL_ASR_TOKEN", "env-token")
	r, err = newSpeechRecognizer(ASRConfig{Provider: "local", LocalURL: "http://localhost:18080"}, AliyunTTSConfig{})
	if err != nil || r.(*localRecognizer).token != "env-token" {
		t.Fatal("environment token not loaded")
	}
	for _, cfg := range []ASRConfig{
		{Provider: "unknown"}, {Provider: "local", LocalURL: "ftp://localhost"},
		{Provider: "local", LocalURL: "http://user:pass@localhost"},
		{Provider: "local", LocalURL: "http://localhost?secret=value"},
		{Provider: "local", LocalURL: "http://localhost", TimeoutSeconds: -1},
		{Provider: "local", LocalURL: "http://localhost", TimeoutSeconds: 121},
		{Provider: "local", LocalURL: "http://localhost", LocalToken: "bad\r\ntoken"},
	} {
		if _, err := newSpeechRecognizer(cfg, cloud); err == nil {
			t.Errorf("accepted invalid configuration: %+v", cfg)
		}
	}
	if _, err := newSpeechRecognizer(ASRConfig{Provider: "auto", LocalURL: "http://localhost"}, AliyunTTSConfig{}); err == nil {
		t.Fatal("auto requires cloud credentials")
	}
}

func TestLocalASRIntegration(t *testing.T) {
	base := os.Getenv("MIIOCLI_TEST_ASR_URL")
	if base == "" {
		t.Skip("set MIIOCLI_TEST_ASR_URL and MIIOCLI_TEST_ASR_PCM for real server verification")
	}
	pcm, err := os.ReadFile(os.Getenv("MIIOCLI_TEST_ASR_PCM"))
	if err != nil {
		t.Fatal(err)
	}
	r, err := newSpeechRecognizer(ASRConfig{Provider: "local", LocalURL: base}, AliyunTTSConfig{})
	if err != nil {
		t.Fatal(err)
	}
	text, err := r.Transcribe(context.Background(), pcm)
	if err != nil || text == "" {
		t.Fatalf("text=%q error=%v", text, err)
	}
}
