package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testRecordStore(t *testing.T) *VoiceRecordStore {
	t.Helper()
	s, err := NewVoiceRecordStore(VoiceRecordsConfig{Directory: t.TempDir()}, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func recordRequest(s *VoiceRecordStore, method, path, body, token string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	return w
}

func TestVoiceRecordsRetentionAndRestart(t *testing.T) {
	s := testRecordStore(t)
	if s.retentionDays != 5 {
		t.Fatalf("default retention = %d", s.retentionDays)
	}
	now := time.Now()
	old := newVoiceRecord("esp32", "old", "voice")
	old.StartedAt = now.Add(-6 * 24 * time.Hour)
	// Seed a preexisting expired record to exercise cleanup, not Save's cutoff.
	if err := s.saveLocked(old); err != nil {
		t.Fatal(err)
	}
	boundary := newVoiceRecord("esp32", "boundary", "voice")
	boundary.StartedAt = now.Add(-5 * 24 * time.Hour)
	boundary.Status = "success"
	if err := s.saveLocked(boundary); err != nil {
		t.Fatal(err)
	}
	if err := s.pruneLocked(now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.directory, old.ID+".json")); !os.IsNotExist(err) {
		t.Fatalf("expired record survives: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.directory, boundary.ID+".json")); err != nil {
		t.Fatal("exact cutoff removed:", err)
	}
	active := newVoiceRecord("esp32", "active", "voice")
	active.ASR.Status = "success"
	active.TTS.Status = "processing"
	if err := s.Save(active); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(s.directory, active.ID+".json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("private file mode: %v %v", info, err)
	}
	s, err = NewVoiceRecordStore(VoiceRecordsConfig{Directory: s.directory}, "test-token")
	if err != nil {
		t.Fatal(err)
	}
	rows, err := s.readLocked()
	if err != nil || len(rows) != 1 || rows[0].Status != "interrupted" || rows[0].TTS.Status != "interrupted" || rows[0].ASR.Status != "success" || rows[0].CompletedAt == nil {
		t.Fatalf("restart recovery: %+v %v", rows, err)
	}
}

func TestVoiceRecordsSettingsPersistAndPrune(t *testing.T) {
	s := testRecordStore(t)
	row := newVoiceRecord("esp32", "two-days-old", "voice")
	row.StartedAt = time.Now().Add(-48 * time.Hour)
	row.Status = "success"
	if err := s.Save(row); err != nil {
		t.Fatal(err)
	}
	for _, token := range []string{"", "wrong"} {
		if w := recordRequest(s, "PUT", "/v1/voice/records/settings", `{"retention_days":1}`, token); w.Code != 401 {
			t.Fatalf("settings auth: %d", w.Code)
		}
	}
	for _, body := range []string{`{"retention_days":0}`, `{"retention_days":366}`, `{"retention_days":1.5}`, `invalid`} {
		if w := recordRequest(s, "PUT", "/v1/voice/records/settings", body, "test-token"); w.Code != 400 {
			t.Fatalf("invalid settings: %d", w.Code)
		}
	}
	w := recordRequest(s, "PUT", "/v1/voice/records/settings", `{"retention_days":1}`, "test-token")
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if _, err := os.Stat(filepath.Join(s.directory, row.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("retention change did not delete expired record")
	}
	reloaded, err := NewVoiceRecordStore(VoiceRecordsConfig{Directory: s.directory, RetentionDays: 10}, "test-token")
	if err != nil || reloaded.retentionDays != 1 {
		t.Fatalf("saved setting lost: %v %v", reloaded, err)
	}
	row.Status = "success"
	if err := reloaded.Save(row); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.directory, row.ID+".json")); !os.IsNotExist(err) {
		t.Fatal("late completion resurrected expired record")
	}
}

func TestVoiceRecordsQueryDetailsAndCompletedAverages(t *testing.T) {
	s := testRecordStore(t)
	full := strings.Repeat("完整回复", 200) + "needle"
	row := newVoiceRecord("esp32", "task", "voice")
	row.Status = "success"
	row.ASR = VoiceStage{Status: "success", Output: "问题", DurationMS: 120}
	row.Nanobot = VoiceStage{Status: "success", Input: "问题", Output: full, DurationMS: 240}
	row.TTS = VoiceStage{Status: "success", Input: "播报", DurationMS: 360}
	if err := s.Save(row); err != nil {
		t.Fatal(err)
	}
	processing := newVoiceRecord("esp32", "processing", "voice")
	processing.ASR = VoiceStage{Status: "processing"}
	if err := s.Save(processing); err != nil {
		t.Fatal(err)
	}
	other := newVoiceRecord("other", "other", "speak")
	other.Status = "error"
	if err := s.Save(other); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/v1/voice/records", "/v1/voice/records/" + row.ID, "/v1/voice/records/settings"} {
		if w := recordRequest(s, "GET", path, "", "wrong"); w.Code != 401 {
			t.Fatalf("unauthorized: %d", w.Code)
		}
	}
	w := recordRequest(s, "GET", "/v1/voice/records?device=esp32&limit=1&page=1", "", "test-token")
	var result struct {
		Records []VoiceRecord `json:"records"`
		Total   int           `json:"total"`
		Stats   struct {
			ASRMS     int64 `json:"asr_ms"`
			NanobotMS int64 `json:"nanobot_ms"`
			TTSMS     int64 `json:"tts_ms"`
		} `json:"stats"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if w.Code != 200 || result.Total != 2 || len(result.Records) != 1 || result.Records[0].ID != processing.ID || result.Stats.ASRMS != 120 || result.Stats.NanobotMS != 240 || result.Stats.TTSMS != 360 {
		t.Fatalf("query or averages: %s", w.Body.String())
	}
	w = recordRequest(s, "GET", "/v1/voice/records?device=esp32&status=success&q=NEEDLE", "", "test-token")
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Total != 1 || len(result.Records) != 1 || len([]rune(result.Records[0].Nanobot.Output)) > 160 {
		t.Fatalf("full output search/preview: %s", w.Body.String())
	}
	w = recordRequest(s, "GET", "/v1/voice/records/"+row.ID, "", "test-token")
	var detail VoiceRecord
	if err := json.Unmarshal(w.Body.Bytes(), &detail); err != nil || detail.Nanobot.Output != full {
		t.Fatal("full reply was lost")
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("private records cached")
	}
	start := time.Now().Add(time.Hour).Format(time.RFC3339)
	w = recordRequest(s, "GET", "/v1/voice/records?start="+url.QueryEscape(start), "", "test-token")
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Total != 0 {
		t.Fatal("time filter failed")
	}
	for _, query := range []string{"?page=0", "?limit=101", "?start=bad", "?start=2026-10-05T00:00:00Z&end=2026-10-04T00:00:00Z"} {
		if w := recordRequest(s, "GET", "/v1/voice/records"+query, "", "test-token"); w.Code != http.StatusBadRequest {
			t.Fatalf("invalid query %s: %d", query, w.Code)
		}
	}
}

func TestVoiceRecordsRedactionAndCorruptionIsolation(t *testing.T) {
	s := testRecordStore(t)
	s.secrets = []string{"private-token"}
	row := newVoiceRecord("esp32", "task", "voice")
	row.Status = "error"
	row.ASR.Error = "upstream echoed private-token"
	if err := s.Save(row); err != nil {
		t.Fatal(err)
	}
	bad := newVoiceRecord("esp32", "bad", "voice")
	if err := os.WriteFile(filepath.Join(s.directory, bad.ID+".json"), []byte("bad JSON"), 0600); err != nil {
		t.Fatal(err)
	}
	w := recordRequest(s, "GET", "/v1/voice/records/"+row.ID, "", "test-token")
	if w.Code != 200 || strings.Contains(w.Body.String(), "private-token") || !strings.Contains(w.Body.String(), "[redacted]") {
		t.Fatalf("corruption/redaction: %s", w.Body.String())
	}
}
