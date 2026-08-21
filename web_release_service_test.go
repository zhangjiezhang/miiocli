package main

import (
	"archive/zip"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWebReleaseUploadActivateAndServe(t *testing.T) {
	root := t.TempDir()
	service := newTestWebReleaseService(t, root)

	first := uploadTestWebArtifact(t, service, "1.0.0", 1, map[string]string{"assets/first.js": "first"})
	activateTestWebRelease(t, service, first.ID)
	assertFileContents(t, filepath.Join(root, "current", "assets", "first.js"), "first")

	currentRequest := httptest.NewRequest(http.MethodGet, "/v1/web/current", nil)
	currentResponse := httptest.NewRecorder()
	service.HandleCurrent(currentResponse, currentRequest)
	if currentResponse.Code != http.StatusOK {
		t.Fatalf("current returned %d: %s", currentResponse.Code, currentResponse.Body.String())
	}
	var current struct {
		Current ActiveWebRelease `json:"current"`
	}
	if err := json.Unmarshal(currentResponse.Body.Bytes(), &current); err != nil {
		t.Fatal(err)
	}
	if current.Current.ReleaseID != first.ID || current.Current.ControlVersion != 1 || current.Current.URL != webAppPath {
		t.Fatalf("unexpected current release: %+v", current.Current)
	}

	for _, test := range []struct {
		path, contains string
		status         int
	}{
		{path: "/ipad-show/", contains: "version 1.0.0", status: http.StatusOK},
		{path: "/ipad-show/assets/first.js", contains: "first", status: http.StatusOK},
		{path: "/ipad-show/settings", contains: "version 1.0.0", status: http.StatusOK},
		{path: "/ipad-show/assets/missing.js", status: http.StatusNotFound},
	} {
		response := httptest.NewRecorder()
		service.HandleApp(response, httptest.NewRequest(http.MethodGet, test.path, nil))
		if response.Code != test.status || !strings.Contains(response.Body.String(), test.contains) {
			t.Errorf("GET %s returned %d %q", test.path, response.Code, response.Body.String())
		}
	}

	second := uploadTestWebArtifact(t, service, "1.1.0", 2, map[string]string{"assets/second.js": "second"})
	activateTestWebRelease(t, service, second.ID)
	assertFileContents(t, filepath.Join(root, "current", "assets", "second.js"), "second")
	if _, err := os.Stat(filepath.Join(root, "current", "assets", "first.js")); !os.IsNotExist(err) {
		t.Fatalf("old published file still exists: %v", err)
	}

	reloaded, err := NewWebReleaseService(WebReleaseConfig{
		Directory: filepath.Join(root, "releases"), PublishDirectory: filepath.Join(root, "current"), AdminToken: "admin-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.current == nil || reloaded.current.ReleaseID != second.ID || len(reloaded.releases) != 2 {
		t.Fatalf("reloaded unexpected state: current=%+v releases=%d", reloaded.current, len(reloaded.releases))
	}
}

func TestWebReleaseAuthorizationAndDuplicateControlVersion(t *testing.T) {
	service := newTestWebReleaseService(t, t.TempDir())
	response := httptest.NewRecorder()
	service.HandleReleases(response, httptest.NewRequest(http.MethodGet, "/v1/web/releases", nil))
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated list returned %d", response.Code)
	}
	authorized := httptest.NewRequest(http.MethodGet, "/v1/web/releases", nil)
	authorized.Header.Set("Authorization", "Bearer admin-secret")
	response = httptest.NewRecorder()
	service.HandleReleases(response, authorized)
	if response.Code != http.StatusOK || response.Body.String() != "{\"releases\":[]}\n" {
		t.Fatalf("empty release list returned %d: %s", response.Code, response.Body.String())
	}

	uploadTestWebArtifact(t, service, "1.0.0", 7, nil)
	body, contentType := testWebUploadBody(t, "1.0.1", 7, nil)
	request := httptest.NewRequest(http.MethodPost, "/v1/web/releases", body)
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Authorization", "Bearer admin-secret")
	response = httptest.NewRecorder()
	service.HandleReleases(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("duplicate control version returned %d: %s", response.Code, response.Body.String())
	}

	activate := httptest.NewRequest(http.MethodPut, "/v1/web/current", strings.NewReader(`{"release_id":"missing"}`))
	activate.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	service.HandleCurrent(response, activate)
	if response.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated activation returned %d", response.Code)
	}
}

func TestWebReleaseDirectoriesMustBeSeparate(t *testing.T) {
	root := t.TempDir()
	_, err := NewWebReleaseService(WebReleaseConfig{
		Directory: root, PublishDirectory: filepath.Join(root, "current"), AdminToken: "admin-secret",
	})
	if err == nil || !strings.Contains(err.Error(), "separate directories") {
		t.Fatalf("overlapping directories returned %v", err)
	}
}

func TestWebReleaseRejectsUnsafeAndMismatchedArchives(t *testing.T) {
	service := newTestWebReleaseService(t, t.TempDir())
	tests := []struct {
		name    string
		version string
		files   map[string]string
	}{
		{name: "path traversal", version: "1.0.0", files: map[string]string{"../escape.txt": "bad"}},
		{name: "missing app shell", version: "1.0.0", files: map[string]string{"index.html": "only one file"}},
		{name: "version mismatch", version: "2.0.0", files: requiredTestWebFiles("1.0.0")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body, contentType := testWebUploadBodyWithFiles(t, test.version, 10, test.files)
			request := httptest.NewRequest(http.MethodPost, "/v1/web/releases", body)
			request.Header.Set("Content-Type", contentType)
			request.Header.Set("Authorization", "Bearer admin-secret")
			response := httptest.NewRecorder()
			service.HandleReleases(response, request)
			if response.Code != http.StatusBadRequest {
				t.Fatalf("unsafe upload returned %d: %s", response.Code, response.Body.String())
			}
		})
	}
}

func TestWebReleaseFailedActivationKeepsPublishedVersion(t *testing.T) {
	root := t.TempDir()
	service := newTestWebReleaseService(t, root)
	first := uploadTestWebArtifact(t, service, "1.0.0", 1, nil)
	activateTestWebRelease(t, service, first.ID)
	second := uploadTestWebArtifact(t, service, "1.1.0", 2, nil)
	if err := os.WriteFile(filepath.Join(service.cfg.Directory, second.ID+".zip"), []byte("broken"), 0644); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPut, "/v1/web/current", strings.NewReader(`{"release_id":"`+second.ID+`"}`))
	request.Header.Set("Authorization", "Bearer admin-secret")
	response := httptest.NewRecorder()
	service.HandleCurrent(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("broken activation returned %d: %s", response.Code, response.Body.String())
	}
	if service.current == nil || service.current.ReleaseID != first.ID {
		t.Fatalf("current release changed after failed activation: %+v", service.current)
	}
	assertFileContents(t, filepath.Join(root, "current", "index.html"), "version 1.0.0")
}

func newTestWebReleaseService(t *testing.T, root string) *WebReleaseService {
	t.Helper()
	service, err := NewWebReleaseService(WebReleaseConfig{
		Directory: filepath.Join(root, "releases"), PublishDirectory: filepath.Join(root, "current"), AdminToken: "admin-secret",
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func uploadTestWebArtifact(t *testing.T, service *WebReleaseService, version string, controlVersion uint64, extra map[string]string) WebRelease {
	t.Helper()
	body, contentType := testWebUploadBody(t, version, controlVersion, extra)
	request := httptest.NewRequest(http.MethodPost, "/v1/web/releases", body)
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("Authorization", "Bearer admin-secret")
	response := httptest.NewRecorder()
	service.HandleReleases(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("upload returned %d: %s", response.Code, response.Body.String())
	}
	var release WebRelease
	if err := json.Unmarshal(response.Body.Bytes(), &release); err != nil {
		t.Fatal(err)
	}
	return release
}

func testWebUploadBody(t *testing.T, version string, controlVersion uint64, extra map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	files := requiredTestWebFiles(version)
	for name, contents := range extra {
		files[name] = contents
	}
	return testWebUploadBodyWithFiles(t, version, controlVersion, files)
}

func testWebUploadBodyWithFiles(t *testing.T, version string, controlVersion uint64, files map[string]string) (*bytes.Buffer, string) {
	t.Helper()
	var archive bytes.Buffer
	zipWriter := zip.NewWriter(&archive)
	for name, contents := range files {
		entry, err := zipWriter.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(entry, contents); err != nil {
			t.Fatal(err)
		}
	}
	if err := zipWriter.Close(); err != nil {
		t.Fatal(err)
	}

	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	_ = writer.WriteField("version", version)
	_ = writer.WriteField("control_version", strconvFormatUint(controlVersion))
	_ = writer.WriteField("notes", "test release")
	part, err := writer.CreateFormFile("artifact", "web-app.zip")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(archive.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return body, writer.FormDataContentType()
}

func requiredTestWebFiles(version string) map[string]string {
	return map[string]string{
		"index.html":           "<html>version " + version + "</html>",
		"manifest.webmanifest": `{"name":"test"}`,
		"sw.js":                "self.addEventListener('fetch', () => {})",
		"version.json":         `{"version":"` + version + `","build":"test"}`,
	}
}

func activateTestWebRelease(t *testing.T, service *WebReleaseService, releaseID string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPut, "/v1/web/current", strings.NewReader(`{"release_id":"`+releaseID+`"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer admin-secret")
	response := httptest.NewRecorder()
	service.HandleCurrent(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("activate returned %d: %s", response.Code, response.Body.String())
	}
}

func assertFileContents(t *testing.T, path, expected string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), expected) {
		t.Fatalf("%s = %q, want text containing %q", path, data, expected)
	}
}

func strconvFormatUint(value uint64) string {
	return fmt.Sprintf("%d", value)
}
