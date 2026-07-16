package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestOTAReleaseLifecycle(t *testing.T) {
	service, err := NewOTAService(OTAConfig{Directory: t.TempDir(), AdminToken: "admin-secret"},
		map[string]VoiceDeviceConfig{"szp-001": {Token: "device-secret"}})
	if err != nil {
		t.Fatal(err)
	}

	release := uploadTestFirmware(t, service, "1.2.0")
	if release.Version != "1.2.0" || release.Channel != "stable" || len(release.SHA256) != 64 {
		t.Fatalf("unexpected release: %+v", release)
	}

	check := httptest.NewRequest(http.MethodGet, "http://ota.local/v1/ota/check", nil)
	setTestDeviceHeaders(check, "1.0.0")
	checkRecorder := httptest.NewRecorder()
	service.HandleCheck(checkRecorder, check)
	if checkRecorder.Code != http.StatusOK {
		t.Fatalf("check returned %d: %s", checkRecorder.Code, checkRecorder.Body.String())
	}
	var result struct {
		Version string `json:"version"`
		URL     string `json:"url"`
	}
	if err := json.Unmarshal(checkRecorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Version != "1.2.0" || result.URL != "http://ota.local/v1/ota/firmware/"+release.ID+".bin" {
		t.Fatalf("unexpected check response: %+v", result)
	}

	download := httptest.NewRequest(http.MethodGet, result.URL, nil)
	setTestDeviceHeaders(download, "1.0.0")
	downloadRecorder := httptest.NewRecorder()
	service.HandleFirmware(downloadRecorder, download)
	if downloadRecorder.Code != http.StatusOK || downloadRecorder.Body.Len() != 128 {
		t.Fatalf("download returned %d with %d bytes", downloadRecorder.Code, downloadRecorder.Body.Len())
	}

	current := httptest.NewRequest(http.MethodGet, "http://ota.local/v1/ota/check", nil)
	setTestDeviceHeaders(current, "1.2.0")
	currentRecorder := httptest.NewRecorder()
	service.HandleCheck(currentRecorder, current)
	if currentRecorder.Code != http.StatusNoContent {
		t.Fatalf("current firmware check returned %d", currentRecorder.Code)
	}

	deleteRequest := httptest.NewRequest(http.MethodDelete, "/v1/ota/releases/"+release.ID, nil)
	deleteRequest.Header.Set("Authorization", "Bearer admin-secret")
	deleteRecorder := httptest.NewRecorder()
	service.HandleRelease(deleteRecorder, deleteRequest)
	if deleteRecorder.Code != http.StatusNoContent {
		t.Fatalf("delete returned %d: %s", deleteRecorder.Code, deleteRecorder.Body.String())
	}
}

func TestOTAAuthorization(t *testing.T) {
	service, err := NewOTAService(OTAConfig{Directory: t.TempDir(), AdminToken: "admin-secret"},
		map[string]VoiceDeviceConfig{"szp-001": {Token: "device-secret"}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/ota/check", nil)
	recorder := httptest.NewRecorder()
	service.HandleCheck(recorder, request)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated check returned %d", recorder.Code)
	}

	adminRequest := httptest.NewRequest(http.MethodGet, "/v1/ota/releases", nil)
	adminRecorder := httptest.NewRecorder()
	service.HandleReleases(adminRecorder, adminRequest)
	if adminRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated admin request returned %d", adminRecorder.Code)
	}

	authorized := httptest.NewRequest(http.MethodGet, "/v1/ota/releases", nil)
	authorized.Header.Set("Authorization", "Bearer admin-secret")
	authorizedRecorder := httptest.NewRecorder()
	service.HandleReleases(authorizedRecorder, authorized)
	if authorizedRecorder.Code != http.StatusOK || authorizedRecorder.Body.String() != "{\"releases\":[]}\n" {
		t.Fatalf("empty release list returned %d: %s", authorizedRecorder.Code, authorizedRecorder.Body.String())
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		left, right string
		want        int
	}{
		{"1.1.0", "1.0.9", 1},
		{"v1.0.0", "1.0", 0},
		{"1.0.0-beta.2", "1.0.0", -1},
		{"2.0.0", "10.0.0", -1},
	}
	for _, test := range cases {
		got := compareVersions(test.left, test.right)
		if got != test.want {
			t.Errorf("compareVersions(%q, %q) = %d, want %d", test.left, test.right, got, test.want)
		}
	}
}

func uploadTestFirmware(t *testing.T, service *OTAService, version string) OTARelease {
	t.Helper()
	image := make([]byte, 128)
	image[0] = 0xE9
	binary.LittleEndian.PutUint32(image[32:36], 0xABCD5432)
	copy(image[48:80], version)

	var body bytes.Buffer
	writer := multipart.NewWriter(&body)
	_ = writer.WriteField("version", version)
	_ = writer.WriteField("channel", "stable")
	_ = writer.WriteField("mandatory", "false")
	part, err := writer.CreateFormFile("firmware", "firmware.bin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(part, bytes.NewReader(image)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/v1/ota/releases", &body)
	request.Header.Set("Content-Type", writer.FormDataContentType())
	request.Header.Set("Authorization", "Bearer admin-secret")
	recorder := httptest.NewRecorder()
	service.HandleReleases(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("upload returned %d: %s", recorder.Code, recorder.Body.String())
	}
	var release OTARelease
	if err := json.Unmarshal(recorder.Body.Bytes(), &release); err != nil {
		t.Fatal(err)
	}
	return release
}

func setTestDeviceHeaders(request *http.Request, version string) {
	request.Header.Set("Authorization", "Bearer device-secret")
	request.Header.Set("X-Device-ID", "szp-001")
	request.Header.Set("X-Firmware-Version", version)
	request.Header.Set("X-OTA-Channel", "stable")
}
