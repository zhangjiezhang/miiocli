package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type VoiceRecordsConfig struct {
	Directory     string `yaml:"directory"`
	RetentionDays int    `yaml:"retentionDays"`
}

type VoiceStage struct {
	Provider     string `json:"provider"`
	Status       string `json:"status"`
	Input        string `json:"input"`
	Output       string `json:"output"`
	Error        string `json:"error,omitempty"`
	DurationMS   int64  `json:"duration_ms"`
	FirstAudioMS int64  `json:"first_audio_ms,omitempty"`
	AudioBytes   int64  `json:"audio_bytes,omitempty"`
}

type VoiceRecord struct {
	ID              string           `json:"id"`
	TaskID          string           `json:"task_id"`
	DeviceID        string           `json:"device_id"`
	Source          string           `json:"source"`
	StartedAt       time.Time        `json:"started_at"`
	CompletedAt     *time.Time       `json:"completed_at,omitempty"`
	Status          string           `json:"status"`
	AudioBytes      int              `json:"audio_bytes"`
	AudioDurationMS int              `json:"audio_duration_ms"`
	TotalMS         int64            `json:"total_ms"`
	VAD             VoiceStage       `json:"vad"`
	VP              VoiceStage       `json:"vp"`
	Speaker         *SpeakerIdentity `json:"speaker,omitempty"`
	ASR             VoiceStage       `json:"asr"`
	Nanobot         VoiceStage       `json:"nanobot"`
	TTS             VoiceStage       `json:"tts"`
}

type VoiceRecordStore struct {
	mu            sync.Mutex
	directory     string
	token         string
	retentionDays int
	secrets       []string
}

var recordIDPattern = regexp.MustCompile(`^[a-f0-9]{32}$`)

func NewVoiceRecordStore(cfg VoiceRecordsConfig, token string) (*VoiceRecordStore, error) {
	if cfg.Directory == "" {
		cfg.Directory = "./voice-records"
	}
	if cfg.RetentionDays == 0 {
		cfg.RetentionDays = 5
	}
	if cfg.RetentionDays < 1 || cfg.RetentionDays > 365 {
		return nil, errors.New("voice.records.retentionDays must be between 1 and 365")
	}
	if err := os.MkdirAll(cfg.Directory, 0700); err != nil {
		return nil, err
	}
	s := &VoiceRecordStore{directory: cfg.Directory, token: token, retentionDays: cfg.RetentionDays}
	if data, err := os.ReadFile(filepath.Join(cfg.Directory, "settings.json")); err == nil {
		var setting struct {
			RetentionDays int `json:"retention_days"`
		}
		if json.Unmarshal(data, &setting) != nil || setting.RetentionDays < 1 || setting.RetentionDays > 365 {
			return nil, errors.New("invalid persisted voice record settings")
		}
		s.retentionDays = setting.RetentionDays
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := s.pruneLocked(time.Now()); err != nil {
		return nil, err
	}
	// A process restart cannot resume an unfinished ASR/agent/TTS operation.
	rows, err := s.readLocked()
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if row.Status == "processing" {
			now := time.Now()
			row.Status = "interrupted"
			row.CompletedAt = &now
			for _, stage := range []*VoiceStage{&row.VAD, &row.VP, &row.ASR, &row.Nanobot, &row.TTS} {
				if stage.Status == "processing" {
					stage.Status = "interrupted"
					stage.Error = "service restarted before this stage completed"
				}
			}
			if err := s.saveLocked(row); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}

func newVoiceRecord(device, task, source string) VoiceRecord {
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	return VoiceRecord{ID: hex.EncodeToString(id[:]), DeviceID: device, TaskID: task, Source: source, StartedAt: time.Now(), Status: "processing"}
}

func (s *VoiceRecordStore) Save(row VoiceRecord) error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, stage := range []*VoiceStage{&row.VAD, &row.VP, &row.ASR, &row.Nanobot, &row.TTS} {
		for _, secret := range s.secrets {
			if secret != "" {
				stage.Error = strings.ReplaceAll(stage.Error, secret, "[redacted]")
			}
		}
		stage.Error = truncateRunes(stage.Error, 1000)
	}
	if row.StartedAt.Before(time.Now().Add(-time.Duration(s.retentionDays) * 24 * time.Hour)) {
		return nil
	}
	return s.saveLocked(row)
}

func (s *VoiceRecordStore) saveLocked(row VoiceRecord) error {
	if !recordIDPattern.MatchString(row.ID) || row.StartedAt.IsZero() {
		return errors.New("invalid voice record")
	}
	return atomicRecordJSON(filepath.Join(s.directory, row.ID+".json"), row)
}

func atomicRecordJSON(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".record-*")
	if err != nil {
		return err
	}
	name := file.Name()
	defer os.Remove(name)
	if _, err = file.Write(data); err != nil {
		file.Close()
		return err
	}
	if err = file.Sync(); err != nil {
		file.Close()
		return err
	}
	if err = file.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func (s *VoiceRecordStore) readLocked() ([]VoiceRecord, error) {
	files, err := os.ReadDir(s.directory)
	if err != nil {
		return nil, err
	}
	rows := make([]VoiceRecord, 0, len(files))
	for _, file := range files {
		name := strings.TrimSuffix(file.Name(), ".json")
		if !file.Type().IsRegular() || !strings.HasSuffix(file.Name(), ".json") || !recordIDPattern.MatchString(name) {
			continue
		}
		f, err := os.Open(filepath.Join(s.directory, file.Name()))
		if err != nil {
			return nil, err
		}
		var row VoiceRecord
		err = json.NewDecoder(io.LimitReader(f, 8<<20)).Decode(&row)
		f.Close()
		if err != nil || row.ID != name || row.StartedAt.IsZero() {
			log.Printf("skipping invalid voice record: %s", file.Name())
			continue
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func (s *VoiceRecordStore) pruneLocked(now time.Time) error {
	rows, err := s.readLocked()
	if err != nil {
		return err
	}
	cutoff := now.Add(-time.Duration(s.retentionDays) * 24 * time.Hour)
	for _, row := range rows {
		if row.StartedAt.Before(cutoff) {
			if err := os.Remove(filepath.Join(s.directory, row.ID+".json")); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	return nil
}

func (s *VoiceRecordStore) Run(ctx context.Context) {
	ticker := time.NewTicker(time.Hour)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.mu.Lock()
			err := s.pruneLocked(time.Now())
			s.mu.Unlock()
			if err != nil {
				log.Printf("voice record retention failed: %v", err)
			}
		}
	}
}

func voiceRecordsPage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/voice-records" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	io.WriteString(w, voiceRecordsHTML)
}

func (s *VoiceRecordStore) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.token == "" || subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+s.token)) != 1 {
		http.Error(w, "unauthorized", 401)
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.URL.Path == "/v1/voice/records/settings" {
		if r.Method == http.MethodPut {
			var setting struct {
				RetentionDays int `json:"retention_days"`
			}
			if json.NewDecoder(http.MaxBytesReader(w, r.Body, 1024)).Decode(&setting) != nil || setting.RetentionDays < 1 || setting.RetentionDays > 365 {
				http.Error(w, "retention_days must be between 1 and 365", 400)
				return
			}
			if err := atomicRecordJSON(filepath.Join(s.directory, "settings.json"), setting); err != nil {
				http.Error(w, "save failed", 500)
				return
			}
			s.retentionDays = setting.RetentionDays
			if err := s.pruneLocked(time.Now()); err != nil {
				http.Error(w, "cleanup failed", 500)
				return
			}
		} else if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", 405)
			return
		}
		writeJSON(w, 200, map[string]any{"retention_days": s.retentionDays})
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	if err := s.pruneLocked(time.Now()); err != nil {
		http.Error(w, "record cleanup failed", 500)
		return
	}
	rows, err := s.readLocked()
	if err != nil {
		http.Error(w, "read records failed", 500)
		return
	}
	if r.URL.Path != "/v1/voice/records" {
		id := strings.TrimPrefix(r.URL.Path, "/v1/voice/records/")
		if !recordIDPattern.MatchString(id) {
			http.NotFound(w, r)
			return
		}
		for _, row := range rows {
			if row.ID == id {
				writeJSON(w, 200, row)
				return
			}
		}
		http.NotFound(w, r)
		return
	}
	q := r.URL.Query()
	parseTime := func(value string) (time.Time, error) {
		if value == "" {
			return time.Time{}, nil
		}
		return time.Parse(time.RFC3339, value)
	}
	start, err := parseTime(q.Get("start"))
	if err != nil {
		http.Error(w, "invalid start", 400)
		return
	}
	end, err := parseTime(q.Get("end"))
	if err != nil || (!start.IsZero() && !end.IsZero() && end.Before(start)) {
		http.Error(w, "invalid end", 400)
		return
	}
	page, limit := 1, 20
	if q.Get("page") != "" {
		page, err = strconv.Atoi(q.Get("page"))
		if err != nil || page < 1 || page > 1000000 {
			http.Error(w, "invalid page", 400)
			return
		}
	}
	if q.Get("limit") != "" {
		limit, err = strconv.Atoi(q.Get("limit"))
		if err != nil || limit < 1 || limit > 100 {
			http.Error(w, "invalid limit", 400)
			return
		}
	}
	filtered := make([]VoiceRecord, 0)
	keyword := strings.ToLower(q.Get("q"))
	var vadMS, asrMS, vpMS, agentMS, ttsMS int64
	var vadN, asrN, vpN, agentN, ttsN, successN, errorN int
	for _, row := range rows {
		if q.Get("device") != "" && row.DeviceID != q.Get("device") {
			continue
		}
		if q.Get("status") != "" && row.Status != q.Get("status") {
			continue
		}
		if (!start.IsZero() && row.StartedAt.Before(start)) || (!end.IsZero() && row.StartedAt.After(end)) {
			continue
		}
		if keyword != "" && !strings.Contains(strings.ToLower(row.VP.Output+"\n"+row.ASR.Output+"\n"+row.Nanobot.Input+"\n"+row.Nanobot.Output+"\n"+row.TTS.Input), keyword) {
			continue
		}
		filtered = append(filtered, row)
		if row.VAD.Status == "success" || row.VAD.Status == "error" {
			vadMS += row.VAD.DurationMS
			vadN++
		}
		if row.VP.Status == "success" || row.VP.Status == "unknown" || row.VP.Status == "error" {
			vpMS += row.VP.DurationMS
			vpN++
		}
		if row.ASR.Status == "success" || row.ASR.Status == "error" {
			asrMS += row.ASR.DurationMS
			asrN++
		}
		if row.Nanobot.Status == "success" || row.Nanobot.Status == "error" {
			agentMS += row.Nanobot.DurationMS
			agentN++
		}
		if row.TTS.Status == "success" || row.TTS.Status == "error" {
			ttsMS += row.TTS.DurationMS
			ttsN++
		}
		if row.Status == "success" {
			successN++
		}
		if row.Status == "error" || row.Status == "interrupted" {
			errorN++
		}
	}
	sort.Slice(filtered, func(i, j int) bool { return filtered[i].StartedAt.After(filtered[j].StartedAt) })
	from := (page - 1) * limit
	if from > len(filtered) {
		from = len(filtered)
	}
	to := from + limit
	if to > len(filtered) {
		to = len(filtered)
	}
	items := make([]VoiceRecord, 0, to-from)
	for _, row := range filtered[from:to] {
		for _, stage := range []*VoiceStage{&row.VAD, &row.VP, &row.ASR, &row.Nanobot, &row.TTS} {
			stage.Input = truncateRunes(stage.Input, 160)
			stage.Output = truncateRunes(stage.Output, 160)
		}
		items = append(items, row)
	}
	mean := func(sum int64, n int) int64 {
		if n == 0 {
			return 0
		}
		return sum / int64(n)
	}
	writeJSON(w, 200, map[string]any{"records": items, "total": len(filtered), "page": page, "limit": limit, "retention_days": s.retentionDays,
		"stats": map[string]any{"success": successN, "errors": errorN, "vad_ms": mean(vadMS, vadN), "asr_ms": mean(asrMS, asrN), "vp_ms": mean(vpMS, vpN), "nanobot_ms": mean(agentMS, agentN), "tts_ms": mean(ttsMS, ttsN)}})
}
