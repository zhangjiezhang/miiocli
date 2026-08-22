package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestConsolePages(t *testing.T) {
	tests := []struct {
		name        string
		path        string
		handler     http.HandlerFunc
		contentType string
		contains    []string
	}{
		{
			name: "home", path: "/", handler: dashboard,
			contentType: "text/html; charset=utf-8",
			contains:    []string{"设备服务与调试控制台", "KiwiVM 服务器", `id="kiwi-progress"`, `href="/ota"`, `href="/websocket"`},
		},
		{
			name: "ota", path: "/ota", handler: otaPage,
			contentType: "text/html; charset=utf-8",
			contains:    []string{"OTA 固件管理", `id="uploadForm"`, `name="notes"`},
		},
		{
			name: "webapp", path: "/webapp", handler: webAppPage,
			contentType: "text/html; charset=utf-8",
			contains:    []string{"Web App 版本管理", `name="artifact"`, `name="control_version"`, "/v1/web/current", `data-delete`},
		},
		{
			name: "websocket", path: "/websocket", handler: websocketPage,
			contentType: "text/html; charset=utf-8",
			contains:    []string{"WebSocket 消息发送", `id="device-select"`, `/v1/devices/${encodeURIComponent(deviceID)}/speak`},
		},
		{
			name: "codex", path: "/codex", handler: codexPage,
			contentType: "text/html; charset=utf-8",
			contains:    []string{"Codex Session", `id="session-list"`, `/v1/codex/sessions/${encodeURIComponent(selectedSessionID)}/messages`},
		},
		{
			name: "styles", path: "/app.css", handler: appStyles,
			contentType: "text/css; charset=utf-8",
			contains:    []string{".site-header", ".voice-layout"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			response := httptest.NewRecorder()
			test.handler(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
			}
			if got := response.Header().Get("Content-Type"); got != test.contentType {
				t.Fatalf("Content-Type = %q, want %q", got, test.contentType)
			}
			for _, expected := range test.contains {
				if !strings.Contains(response.Body.String(), expected) {
					t.Errorf("response does not contain %q", expected)
				}
			}
		})
	}
}

func TestConsolePagesRejectUnknownPaths(t *testing.T) {
	for _, handler := range []http.HandlerFunc{dashboard, otaPage, webAppPage, websocketPage, codexPage, appStyles} {
		response := httptest.NewRecorder()
		handler(response, httptest.NewRequest(http.MethodGet, "/unknown", nil))
		if response.Code != http.StatusNotFound {
			t.Errorf("status = %d, want %d", response.Code, http.StatusNotFound)
		}
	}
}
