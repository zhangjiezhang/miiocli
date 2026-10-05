package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConsoleAPIServerCredentialsAndIsolation(t *testing.T) {
	cfg := Config{Voice: VoiceConfig{APIToken: "voice-secret"}}
	for _, test := range []struct{ path, token string }{
		{"/v1/ota/releases", ""}, {"/v1/ota/releases/release-id", ""},
		{"/v1/web/releases", ""}, {"/v1/web/current", ""},
		{"/v1/devices", "voice-secret"}, {"/v1/devices/esp32/speak", "voice-secret"},
		{"/v1/voice/records", "voice-secret"}, {"/v1/voice/records/settings", "voice-secret"},
	} {
		t.Run(test.path, func(t *testing.T) {
			downstream := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				expectedAuthorization := ""
				if test.token != "" {
					expectedAuthorization = "Bearer " + test.token
				}
				if r.URL.Path != test.path || r.Header.Get("Authorization") != expectedAuthorization || r.URL.Query().Get("q") != "hello" {
					t.Errorf("incorrect forwarded request: %s", r.URL)
				}
				w.WriteHeader(http.StatusNoContent)
			})
			r := httptest.NewRequest("PUT", "http://console.test/console"+test.path+"?q=hello", nil)
			r.Header.Set("Origin", "http://console.test")
			r.Header.Set("Sec-Fetch-Site", "same-origin")
			r.Header.Set("Authorization", "Bearer browser-value")
			w := httptest.NewRecorder()
			consoleAPI(downstream, cfg).ServeHTTP(w, r)
			if w.Code != 204 || r.Header.Get("Authorization") != "Bearer browser-value" || (test.token != "" && strings.Contains(w.Body.String(), test.token)) {
				t.Fatalf("credential isolation: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestConsoleAPIRejectsCrossOriginAndNonConsoleEndpoints(t *testing.T) {
	called := false
	h := consoleAPI(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { called = true }), Config{Voice: VoiceConfig{APIToken: "secret"}})
	for _, test := range []struct {
		method, path, origin, site string
		status                     int
	}{
		{"GET", "/console/v1/voice/records", "http://evil.test", "", 403},
		{"GET", "/console/v1/voice/records", "", "cross-site", 403},
		{"PUT", "/console/v1/voice/records/settings", "", "", 403},
		{"GET", "/console/v1/device/ws", "", "", 404},
		{"GET", "/console/v1/ota/check", "", "", 404},
		{"GET", "/console/v1/ota/firmware/id.bin", "", "", 404},
	} {
		r := httptest.NewRequest(test.method, "http://console.test"+test.path, nil)
		r.Header.Set("Origin", test.origin)
		r.Header.Set("Sec-Fetch-Site", test.site)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != test.status || called {
			t.Fatalf("%s: status %d, forwarded=%v", test.path, w.Code, called)
		}
	}
}

func TestConsoleNavigationAndNoCredentialInputs(t *testing.T) {
	for _, page := range []string{dashboardHTML, otaHTML, webAppHTML, websocketHTML, voiceRecordsHTML, codexHTML} {
		start := strings.Index(page, "<nav ")
		end := strings.Index(page[start:], "</nav>") + start
		nav := page[start:end]
		for _, path := range []string{"/", "/ota", "/webapp", "/websocket", "/voice-records"} {
			if !strings.Contains(nav, `href="`+path+`"`) {
				t.Errorf("missing navigation %s", path)
			}
		}
		if strings.Contains(nav, "/codex") || strings.Contains(page, `type="password"`) || strings.Contains(page, "voice_api_token") || strings.Contains(page, "ota_token") || strings.Contains(page, "webapp_token") {
			t.Error("legacy navigation or credential input remains")
		}
	}
}

func TestConsoleRecordsNeedNoTokenWhileExternalAPIStillDoes(t *testing.T) {
	store := testRecordStore(t)
	mux := http.NewServeMux()
	mux.Handle("/v1/voice/records", store)
	mux.Handle("/v1/voice/records/", store)
	mux.Handle("/console/", consoleAPI(mux, Config{Voice: VoiceConfig{APIToken: "test-token"}}))
	for _, test := range []struct {
		path   string
		status int
	}{
		{"/console/v1/voice/records", 200},
		{"/console/v1/voice/records/settings", 200},
		{"/v1/voice/records", 401},
		{"/v1/voice/records/settings", 401},
	} {
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, httptest.NewRequest("GET", test.path, nil))
		if w.Code != test.status {
			t.Errorf("%s: status %d, want %d", test.path, w.Code, test.status)
		}
	}
	r := httptest.NewRequest("PUT", "http://console.test/console/v1/voice/records/settings", strings.NewReader(`{"retention_days":7}`))
	r.Header.Set("Origin", "http://console.test")
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 200 || store.retentionDays != 7 {
		t.Fatalf("console setting without token: %d %s", w.Code, w.Body.String())
	}
}
