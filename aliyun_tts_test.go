package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAliyunTTSServiceListsOnlineDevices(t *testing.T) {
	hub := NewVoiceHub(VoiceConfig{Devices: map[string]VoiceDeviceConfig{
		"device-a": {Token: "token-a"},
		"device-b": {Token: "token-b"},
		"offline":  {Token: "token-c"},
	}})
	hub.register(&voiceSession{deviceID: "device-b", done: make(chan struct{})})
	hub.register(&voiceSession{deviceID: "device-a", done: make(chan struct{})})
	service := &AliyunTTSService{apiToken: "api-token", hub: hub}

	unauthorized := httptest.NewRecorder()
	service.HandleDevices(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/devices", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}

	request := httptest.NewRequest(http.MethodGet, "/v1/devices", nil)
	request.Header.Set("Authorization", "Bearer api-token")
	response := httptest.NewRecorder()
	service.HandleDevices(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}

	var payload struct {
		Devices []voiceDeviceStatus `json:"devices"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(payload.Devices) != 2 || payload.Devices[0].ID != "device-a" ||
		payload.Devices[1].ID != "device-b" || !payload.Devices[0].Online || !payload.Devices[1].Online {
		t.Fatalf("unexpected devices: %#v", payload.Devices)
	}
}

func TestAliyunTTSServiceDeviceListMethod(t *testing.T) {
	service := &AliyunTTSService{apiToken: "api-token", hub: NewVoiceHub(VoiceConfig{})}
	request := httptest.NewRequest(http.MethodPost, "/v1/devices", nil)
	response := httptest.NewRecorder()
	service.HandleDevices(response, request)

	if response.Code != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusMethodNotAllowed)
	}
	if allow := response.Header().Get("Allow"); allow != http.MethodGet {
		t.Fatalf("Allow = %q, want %q", allow, http.MethodGet)
	}
}
