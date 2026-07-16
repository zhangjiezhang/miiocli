package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

const (
	defaultOTADirectory = "./releases"
	maxFirmwareSize     = 3 * 1024 * 1024
)

var safeReleasePart = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

type OTAConfig struct {
	Directory  string `yaml:"directory"`
	AdminToken string `yaml:"adminToken"`
}

type OTARelease struct {
	ID        string    `json:"id"`
	Version   string    `json:"version"`
	Channel   string    `json:"channel"`
	Mandatory bool      `json:"mandatory"`
	Notes     string    `json:"notes,omitempty"`
	Size      int64     `json:"size"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
	FileName  string    `json:"-"`
}

type otaIndex struct {
	Releases []OTARelease `json:"releases"`
}

type OTAService struct {
	cfg      OTAConfig
	devices  map[string]VoiceDeviceConfig
	mu       sync.RWMutex
	releases []OTARelease
}

func NewOTAService(cfg OTAConfig, devices map[string]VoiceDeviceConfig) (*OTAService, error) {
	if cfg.AdminToken == "" {
		return nil, errors.New("ota.adminToken is required")
	}
	if cfg.Directory == "" {
		cfg.Directory = defaultOTADirectory
	}
	if err := os.MkdirAll(cfg.Directory, 0755); err != nil {
		return nil, fmt.Errorf("create OTA directory: %w", err)
	}
	s := &OTAService{cfg: cfg, devices: devices}
	if err := s.load(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *OTAService) HandleCheck(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if !s.authorizeDevice(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	current := strings.TrimSpace(r.Header.Get("X-Firmware-Version"))
	channel := strings.TrimSpace(r.Header.Get("X-OTA-Channel"))
	if channel == "" {
		channel = "stable"
	}

	s.mu.RLock()
	var selected *OTARelease
	for i := range s.releases {
		release := s.releases[i]
		if release.Channel != channel || compareVersions(release.Version, current) <= 0 {
			continue
		}
		if selected == nil || compareVersions(release.Version, selected.Version) > 0 {
			copy := release
			selected = &copy
		}
	}
	s.mu.RUnlock()
	if selected == nil {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	response := struct {
		ID        string `json:"id"`
		Version   string `json:"version"`
		Channel   string `json:"channel"`
		Mandatory bool   `json:"mandatory"`
		Notes     string `json:"notes,omitempty"`
		Size      int64  `json:"size"`
		SHA256    string `json:"sha256"`
		URL       string `json:"url"`
	}{
		ID: selected.ID, Version: selected.Version, Channel: selected.Channel,
		Mandatory: selected.Mandatory, Notes: selected.Notes, Size: selected.Size,
		SHA256: selected.SHA256, URL: requestBaseURL(r) + "/v1/ota/firmware/" + selected.ID + ".bin",
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *OTAService) HandleFirmware(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		methodNotAllowed(w, http.MethodGet)
		return
	}
	if !s.authorizeDevice(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/ota/firmware/"), ".bin")
	release, ok := s.releaseByID(id)
	if !ok {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="firmware-%s.bin"`, release.Version))
	w.Header().Set("X-Content-SHA256", release.SHA256)
	http.ServeFile(w, r, filepath.Join(s.cfg.Directory, release.FileName))
}

func (s *OTAService) HandleReleases(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeAdmin(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		releases := append([]OTARelease{}, s.releases...)
		s.mu.RUnlock()
		sort.Slice(releases, func(i, j int) bool { return releases[i].CreatedAt.After(releases[j].CreatedAt) })
		writeJSON(w, http.StatusOK, map[string]any{"releases": releases})
	case http.MethodPost:
		s.upload(w, r)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

func (s *OTAService) HandleRelease(w http.ResponseWriter, r *http.Request) {
	if !s.authorizeAdmin(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodDelete {
		methodNotAllowed(w, http.MethodDelete)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/v1/ota/releases/")
	if id == "" || strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}

	s.mu.Lock()
	index := -1
	for i := range s.releases {
		if s.releases[i].ID == id {
			index = i
			break
		}
	}
	if index < 0 {
		s.mu.Unlock()
		http.NotFound(w, r)
		return
	}
	release := s.releases[index]
	s.releases = append(s.releases[:index], s.releases[index+1:]...)
	err := s.saveLocked()
	if err != nil {
		s.releases = append(s.releases, OTARelease{})
		copy(s.releases[index+1:], s.releases[index:])
		s.releases[index] = release
	}
	s.mu.Unlock()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = os.Remove(filepath.Join(s.cfg.Directory, release.FileName))
	w.WriteHeader(http.StatusNoContent)
}

func (s *OTAService) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxFirmwareSize+1024*1024)
	if err := r.ParseMultipartForm(256 * 1024); err != nil {
		http.Error(w, "invalid multipart request", http.StatusBadRequest)
		return
	}
	version := strings.TrimSpace(r.FormValue("version"))
	channel := strings.TrimSpace(r.FormValue("channel"))
	if !validVersion(version) || (channel != "stable" && channel != "beta") {
		http.Error(w, "invalid version or channel", http.StatusBadRequest)
		return
	}
	file, _, err := r.FormFile("firmware")
	if err != nil {
		http.Error(w, "firmware is required", http.StatusBadRequest)
		return
	}
	defer file.Close()

	id := fmt.Sprintf("%d-%s-%s", time.Now().UTC().UnixMilli(), channel, safeReleasePart.ReplaceAllString(version, "-"))
	fileName := id + ".bin"
	temp, err := os.CreateTemp(s.cfg.Directory, ".upload-*.bin")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(temp, hash), io.LimitReader(file, maxFirmwareSize+1))
	closeErr := temp.Close()
	if copyErr != nil || closeErr != nil || size == 0 || size > maxFirmwareSize {
		http.Error(w, "firmware must be between 1 byte and 3 MiB", http.StatusBadRequest)
		return
	}
	imageVersion, err := espImageVersion(tempName)
	if err != nil || imageVersion != version {
		http.Error(w, fmt.Sprintf("firmware image version is %q, expected %q", imageVersion, version), http.StatusBadRequest)
		return
	}
	if err := os.Rename(tempName, filepath.Join(s.cfg.Directory, fileName)); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	release := OTARelease{
		ID: id, Version: version, Channel: channel,
		Mandatory: r.FormValue("mandatory") == "true", Notes: strings.TrimSpace(r.FormValue("notes")),
		Size: size, SHA256: hex.EncodeToString(hash.Sum(nil)), CreatedAt: time.Now().UTC(), FileName: fileName,
	}
	s.mu.Lock()
	s.releases = append(s.releases, release)
	err = s.saveLocked()
	if err != nil {
		s.releases = s.releases[:len(s.releases)-1]
	}
	s.mu.Unlock()
	if err != nil {
		_ = os.Remove(filepath.Join(s.cfg.Directory, fileName))
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, http.StatusCreated, release)
}

func espImageVersion(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	header := make([]byte, 80)
	if _, err := io.ReadFull(file, header); err != nil {
		return "", errors.New("firmware image is too small")
	}
	if header[0] != 0xE9 || binary.LittleEndian.Uint32(header[32:36]) != 0xABCD5432 {
		return "", errors.New("invalid ESP application image")
	}
	return strings.TrimRight(string(header[48:80]), "\x00"), nil
}

func (s *OTAService) authorizeDevice(r *http.Request) bool {
	deviceID := r.Header.Get("X-Device-ID")
	device, ok := s.devices[deviceID]
	return ok && secureBearer(r, device.Token)
}

func (s *OTAService) authorizeAdmin(r *http.Request) bool {
	return secureBearer(r, s.cfg.AdminToken)
}

func secureBearer(r *http.Request, expected string) bool {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if expected == "" || !strings.HasPrefix(header, prefix) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(header, prefix)), []byte(expected)) == 1
}

func (s *OTAService) releaseByID(id string) (OTARelease, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, release := range s.releases {
		if release.ID == id {
			return release, true
		}
	}
	return OTARelease{}, false
}

func (s *OTAService) load() error {
	data, err := os.ReadFile(filepath.Join(s.cfg.Directory, "index.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read OTA index: %w", err)
	}
	var index otaIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return fmt.Errorf("parse OTA index: %w", err)
	}
	for i := range index.Releases {
		index.Releases[i].FileName = index.Releases[i].ID + ".bin"
	}
	s.releases = index.Releases
	return nil
}

func (s *OTAService) saveLocked() error {
	data, err := json.MarshalIndent(otaIndex{Releases: s.releases}, "", "  ")
	if err != nil {
		return err
	}
	temp := filepath.Join(s.cfg.Directory, "index.json.tmp")
	if err := os.WriteFile(temp, data, 0644); err != nil {
		return err
	}
	index := filepath.Join(s.cfg.Directory, "index.json")
	_ = os.Remove(index)
	return os.Rename(temp, index)
}

func requestBaseURL(r *http.Request) string {
	scheme := r.Header.Get("X-Forwarded-Proto")
	if scheme == "" {
		if r.TLS != nil {
			scheme = "https"
		} else {
			scheme = "http"
		}
	}
	return scheme + "://" + r.Host
}

func validVersion(version string) bool {
	if len(version) == 0 || len(version) > 31 {
		return false
	}
	if strings.HasPrefix(version, "v") {
		version = version[1:]
	}
	parts := strings.SplitN(version, "-", 2)
	numbers := strings.Split(parts[0], ".")
	if len(numbers) < 1 || len(numbers) > 4 {
		return false
	}
	for _, number := range numbers {
		if number == "" {
			return false
		}
		if _, err := strconv.ParseUint(number, 10, 32); err != nil {
			return false
		}
	}
	return true
}

func compareVersions(left, right string) int {
	parse := func(value string) ([]int, string) {
		value = strings.TrimPrefix(strings.TrimSpace(value), "v")
		parts := strings.SplitN(value, "-", 2)
		numbers := strings.Split(parts[0], ".")
		values := make([]int, 4)
		for i := 0; i < len(numbers) && i < len(values); i++ {
			values[i], _ = strconv.Atoi(numbers[i])
		}
		pre := ""
		if len(parts) == 2 {
			pre = parts[1]
		}
		return values, pre
	}
	lv, lp := parse(left)
	rv, rp := parse(right)
	for i := range lv {
		if lv[i] < rv[i] {
			return -1
		}
		if lv[i] > rv[i] {
			return 1
		}
	}
	if lp == rp {
		return 0
	}
	if lp == "" {
		return 1
	}
	if rp == "" {
		return -1
	}
	return strings.Compare(lp, rp)
}

func methodNotAllowed(w http.ResponseWriter, methods ...string) {
	w.Header().Set("Allow", strings.Join(methods, ", "))
	http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
