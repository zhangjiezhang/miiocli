package main

import (
	"archive/zip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultWebReleaseDirectory = "./web-releases"
	defaultWebPublishDirectory = "./ipad-show"
	maxWebArtifactSize         = 32 * 1024 * 1024
	maxWebExtractedSize        = 128 * 1024 * 1024
	maxWebArchiveFiles         = 2048
	webAppPath                 = "/ipad-show/"
)

var requiredWebFiles = map[string]struct{}{
	"index.html":           {},
	"manifest.webmanifest": {},
	"sw.js":                {},
	"version.json":         {},
}

type WebReleaseConfig struct {
	Directory        string `yaml:"directory"`
	PublishDirectory string `yaml:"publishDirectory"`
	AdminToken       string `yaml:"adminToken"`
}

type WebRelease struct {
	ID             string    `json:"id"`
	Version        string    `json:"version"`
	ControlVersion uint64    `json:"control_version"`
	Notes          string    `json:"notes,omitempty"`
	Size           int64     `json:"size"`
	SHA256         string    `json:"sha256"`
	CreatedAt      time.Time `json:"created_at"`
	FileName       string    `json:"-"`
}

type ActiveWebRelease struct {
	ReleaseID      string    `json:"release_id"`
	Version        string    `json:"version"`
	ControlVersion uint64    `json:"control_version"`
	ActivatedAt    time.Time `json:"activated_at"`
	URL            string    `json:"url"`
}

type webReleaseIndex struct {
	Releases []WebRelease      `json:"releases"`
	Current  *ActiveWebRelease `json:"current,omitempty"`
}

type WebReleaseService struct {
	cfg      WebReleaseConfig
	mu       sync.RWMutex
	releases []WebRelease
	current  *ActiveWebRelease
}

func NewWebReleaseService(cfg WebReleaseConfig) (*WebReleaseService, error) {
	if cfg.AdminToken == "" {
		return nil, errors.New("webApp.adminToken is required")
	}
	if cfg.Directory == "" {
		cfg.Directory = defaultWebReleaseDirectory
	}
	if cfg.PublishDirectory == "" {
		cfg.PublishDirectory = defaultWebPublishDirectory
	}
	releaseDirectory, err := filepath.Abs(cfg.Directory)
	if err != nil {
		return nil, fmt.Errorf("resolve web release directory: %w", err)
	}
	publishDirectory, err := filepath.Abs(cfg.PublishDirectory)
	if err != nil {
		return nil, fmt.Errorf("resolve web publish directory: %w", err)
	}
	if pathsOverlap(releaseDirectory, publishDirectory) {
		return nil, errors.New("webApp.directory and webApp.publishDirectory must be separate directories")
	}
	cfg.Directory = releaseDirectory
	cfg.PublishDirectory = publishDirectory
	if err := os.MkdirAll(cfg.Directory, 0755); err != nil {
		return nil, fmt.Errorf("create web release directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.PublishDirectory), 0755); err != nil {
		return nil, fmt.Errorf("create web publish parent directory: %w", err)
	}

	service := &WebReleaseService{cfg: cfg}
	if err := service.load(); err != nil {
		return nil, err
	}
	return service, nil
}

// HandleReleases exposes the first resource path: GET lists uploaded builds and
// POST stores a validated ZIP artifact. Both operations are administrative.
func (s *WebReleaseService) HandleReleases(w http.ResponseWriter, r *http.Request) {
	if !secureBearer(r, s.cfg.AdminToken) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		releases := append([]WebRelease{}, s.releases...)
		s.mu.RUnlock()
		sort.Slice(releases, func(i, j int) bool { return releases[i].CreatedAt.After(releases[j].CreatedAt) })
		writeJSON(w, http.StatusOK, map[string]any{"releases": releases})
	case http.MethodPost:
		s.upload(w, r)
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPost)
	}
}

// HandleCurrent exposes the second resource path: GET is public for installed
// clients, while PUT activates an uploaded release for administrators.
func (s *WebReleaseService) HandleCurrent(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.mu.RLock()
		current := cloneActiveWebRelease(s.current)
		s.mu.RUnlock()
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{"current": current})
	case http.MethodPut:
		if !secureBearer(r, s.cfg.AdminToken) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
		var request struct {
			ReleaseID string `json:"release_id"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil || strings.TrimSpace(request.ReleaseID) == "" {
			http.Error(w, "release_id is required", http.StatusBadRequest)
			return
		}
		if err := decoder.Decode(&struct{}{}); err != io.EOF {
			http.Error(w, "request body must contain one JSON object", http.StatusBadRequest)
			return
		}
		current, err := s.activate(strings.TrimSpace(request.ReleaseID))
		if errors.Is(err, os.ErrNotExist) {
			http.Error(w, "release not found", http.StatusNotFound)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, http.StatusOK, map[string]any{"current": current})
	default:
		methodNotAllowed(w, http.MethodGet, http.MethodPut)
	}
}

func (s *WebReleaseService) HandleApp(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		methodNotAllowed(w, http.MethodGet, http.MethodHead)
		return
	}
	if r.URL.Path == strings.TrimSuffix(webAppPath, "/") {
		http.Redirect(w, r, webAppPath, http.StatusPermanentRedirect)
		return
	}
	if !strings.HasPrefix(r.URL.Path, webAppPath) {
		http.NotFound(w, r)
		return
	}

	s.mu.RLock()
	hasCurrent := s.current != nil
	s.mu.RUnlock()
	if !hasCurrent {
		http.NotFound(w, r)
		return
	}

	relative := strings.TrimPrefix(r.URL.Path, webAppPath)
	if relative == "" {
		relative = "index.html"
	}
	clean := path.Clean(relative)
	if !safeArchivePath(clean) {
		http.NotFound(w, r)
		return
	}
	filePath := filepath.Join(s.cfg.PublishDirectory, filepath.FromSlash(clean))
	info, err := os.Stat(filePath)
	if err != nil || info.IsDir() {
		if path.Ext(clean) != "" {
			http.NotFound(w, r)
			return
		}
		filePath = filepath.Join(s.cfg.PublishDirectory, "index.html")
	}

	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; connect-src 'self'; worker-src 'self'; manifest-src 'self'; object-src 'none'; base-uri 'self'; frame-ancestors 'none'")
	switch clean {
	case "version.json":
		w.Header().Set("Cache-Control", "no-store")
	case "sw.js", "index.html":
		w.Header().Set("Cache-Control", "no-cache")
	default:
		if strings.HasPrefix(clean, "assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		}
	}
	if clean == "install.mobileconfig" {
		w.Header().Set("Content-Type", "application/x-apple-aspen-config")
	}
	http.ServeFile(w, r, filePath)
}

func (s *WebReleaseService) upload(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxWebArtifactSize+1024*1024)
	if err := r.ParseMultipartForm(512 * 1024); err != nil {
		http.Error(w, "invalid multipart request", http.StatusBadRequest)
		return
	}
	if r.MultipartForm != nil {
		defer r.MultipartForm.RemoveAll()
	}
	version := strings.TrimSpace(r.FormValue("version"))
	controlVersion, err := strconv.ParseUint(strings.TrimSpace(r.FormValue("control_version")), 10, 64)
	if !validVersion(version) || err != nil || controlVersion == 0 {
		http.Error(w, "invalid version or control_version", http.StatusBadRequest)
		return
	}
	notes := strings.TrimSpace(r.FormValue("notes"))
	if len(notes) > 500 {
		http.Error(w, "notes must not exceed 500 bytes", http.StatusBadRequest)
		return
	}
	file, header, err := r.FormFile("artifact")
	if err != nil {
		http.Error(w, "artifact is required", http.StatusBadRequest)
		return
	}
	defer file.Close()
	if !strings.EqualFold(filepath.Ext(header.Filename), ".zip") {
		http.Error(w, "artifact must be a ZIP file", http.StatusBadRequest)
		return
	}

	s.mu.RLock()
	duplicate := s.hasControlVersionLocked(controlVersion)
	s.mu.RUnlock()
	if duplicate {
		http.Error(w, "control_version already exists", http.StatusConflict)
		return
	}

	temp, err := os.CreateTemp(s.cfg.Directory, ".web-upload-*.zip")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	hash := sha256.New()
	size, copyErr := io.Copy(io.MultiWriter(temp, hash), io.LimitReader(file, maxWebArtifactSize+1))
	closeErr := temp.Close()
	if copyErr != nil || closeErr != nil || size == 0 || size > maxWebArtifactSize {
		http.Error(w, "artifact must be between 1 byte and 32 MiB", http.StatusBadRequest)
		return
	}
	if err := validateWebArchive(tempName, version); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	id := fmt.Sprintf("%d-%d-%s", time.Now().UTC().UnixMilli(), controlVersion, safeReleasePart.ReplaceAllString(version, "-"))
	fileName := id + ".zip"
	finalPath := filepath.Join(s.cfg.Directory, fileName)
	if err := os.Rename(tempName, finalPath); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	release := WebRelease{
		ID: id, Version: version, ControlVersion: controlVersion, Notes: notes,
		Size: size, SHA256: hex.EncodeToString(hash.Sum(nil)), CreatedAt: time.Now().UTC(), FileName: fileName,
	}

	s.mu.Lock()
	if s.hasControlVersionLocked(controlVersion) {
		s.mu.Unlock()
		_ = os.Remove(finalPath)
		http.Error(w, "control_version already exists", http.StatusConflict)
		return
	}
	s.releases = append(s.releases, release)
	if err := s.saveLocked(); err != nil {
		s.releases = s.releases[:len(s.releases)-1]
		s.mu.Unlock()
		_ = os.Remove(finalPath)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	s.mu.Unlock()
	writeJSON(w, http.StatusCreated, release)
}

func (s *WebReleaseService) activate(id string) (*ActiveWebRelease, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var selected *WebRelease
	for i := range s.releases {
		if s.releases[i].ID == id {
			selected = &s.releases[i]
			break
		}
	}
	if selected == nil {
		return nil, os.ErrNotExist
	}
	archivePath := filepath.Join(s.cfg.Directory, selected.FileName)
	if err := validateWebArchive(archivePath, selected.Version); err != nil {
		return nil, fmt.Errorf("validate release artifact: %w", err)
	}

	parent := filepath.Dir(s.cfg.PublishDirectory)
	tempDirectory, err := os.MkdirTemp(parent, ".web-activate-*")
	if err != nil {
		return nil, fmt.Errorf("create activation directory: %w", err)
	}
	defer os.RemoveAll(tempDirectory)
	if err := extractWebArchive(archivePath, tempDirectory); err != nil {
		return nil, fmt.Errorf("extract release artifact: %w", err)
	}

	backupDirectory := fmt.Sprintf("%s.backup-%d", s.cfg.PublishDirectory, time.Now().UTC().UnixNano())
	hadPrevious := false
	if _, err := os.Lstat(s.cfg.PublishDirectory); err == nil {
		if err := os.Rename(s.cfg.PublishDirectory, backupDirectory); err != nil {
			return nil, fmt.Errorf("stage current web app: %w", err)
		}
		hadPrevious = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect current web app: %w", err)
	}
	if err := os.Rename(tempDirectory, s.cfg.PublishDirectory); err != nil {
		if hadPrevious {
			_ = os.Rename(backupDirectory, s.cfg.PublishDirectory)
		}
		return nil, fmt.Errorf("publish web app: %w", err)
	}

	previousCurrent := cloneActiveWebRelease(s.current)
	next := &ActiveWebRelease{
		ReleaseID: selected.ID, Version: selected.Version, ControlVersion: selected.ControlVersion,
		ActivatedAt: time.Now().UTC(), URL: webAppPath,
	}
	s.current = next
	if err := s.saveLocked(); err != nil {
		s.current = previousCurrent
		_ = os.RemoveAll(s.cfg.PublishDirectory)
		if hadPrevious {
			_ = os.Rename(backupDirectory, s.cfg.PublishDirectory)
		}
		return nil, fmt.Errorf("save active web release: %w", err)
	}
	if hadPrevious {
		_ = os.RemoveAll(backupDirectory)
	}
	return cloneActiveWebRelease(next), nil
}

func validateWebArchive(archivePath, expectedVersion string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return errors.New("artifact is not a valid ZIP file")
	}
	defer reader.Close()
	if len(reader.File) == 0 || len(reader.File) > maxWebArchiveFiles {
		return fmt.Errorf("artifact must contain between 1 and %d entries", maxWebArchiveFiles)
	}

	required := make(map[string]bool, len(requiredWebFiles))
	seen := make(map[string]struct{}, len(reader.File))
	var extractedSize uint64
	for _, file := range reader.File {
		clean := path.Clean(file.Name)
		if !safeArchivePath(clean) {
			return fmt.Errorf("unsafe archive path %q", file.Name)
		}
		if _, exists := seen[clean]; exists {
			return fmt.Errorf("duplicate archive path %q", file.Name)
		}
		seen[clean] = struct{}{}
		if file.Mode()&os.ModeSymlink != 0 || (!file.FileInfo().IsDir() && !file.Mode().IsRegular()) {
			return fmt.Errorf("unsupported archive entry %q", file.Name)
		}
		extractedSize += file.UncompressedSize64
		if extractedSize > maxWebExtractedSize {
			return errors.New("artifact expands beyond 128 MiB")
		}
		if _, ok := requiredWebFiles[clean]; ok && !file.FileInfo().IsDir() {
			required[clean] = true
		}
	}
	for name := range requiredWebFiles {
		if !required[name] {
			return fmt.Errorf("artifact is missing %s at its root", name)
		}
	}

	versionFile, err := reader.Open("version.json")
	if err != nil {
		return errors.New("read version.json")
	}
	defer versionFile.Close()
	var releaseInfo struct {
		Version string `json:"version"`
	}
	decoder := json.NewDecoder(io.LimitReader(versionFile, 64*1024))
	if err := decoder.Decode(&releaseInfo); err != nil || releaseInfo.Version != expectedVersion {
		return fmt.Errorf("version.json version is %q, expected %q", releaseInfo.Version, expectedVersion)
	}
	return nil
}

func extractWebArchive(archivePath, destination string) error {
	reader, err := zip.OpenReader(archivePath)
	if err != nil {
		return err
	}
	defer reader.Close()

	var extractedSize uint64
	for _, file := range reader.File {
		clean := path.Clean(file.Name)
		if !safeArchivePath(clean) || file.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("unsafe archive entry %q", file.Name)
		}
		target := filepath.Join(destination, filepath.FromSlash(clean))
		if file.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0755); err != nil {
				return err
			}
			continue
		}
		extractedSize += file.UncompressedSize64
		if extractedSize > maxWebExtractedSize {
			return errors.New("artifact expands beyond 128 MiB")
		}
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			return err
		}
		source, err := file.Open()
		if err != nil {
			return err
		}
		destinationFile, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0644)
		if err != nil {
			source.Close()
			return err
		}
		written, copyErr := io.Copy(destinationFile, io.LimitReader(source, int64(file.UncompressedSize64)+1))
		closeDestinationErr := destinationFile.Close()
		closeSourceErr := source.Close()
		if copyErr != nil || closeDestinationErr != nil || closeSourceErr != nil || uint64(written) != file.UncompressedSize64 {
			return errors.New("extract archive entry")
		}
	}
	return nil
}

func safeArchivePath(name string) bool {
	return name != "" && name != "." && name != ".." && !path.IsAbs(name) &&
		!strings.HasPrefix(name, "../") && !strings.Contains(name, "\\") && !strings.ContainsRune(name, '\x00')
}

func pathsOverlap(left, right string) bool {
	contains := func(parent, child string) bool {
		relative, err := filepath.Rel(parent, child)
		return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
	}
	return contains(left, right) || contains(right, left)
}

func (s *WebReleaseService) hasControlVersionLocked(version uint64) bool {
	for _, release := range s.releases {
		if release.ControlVersion == version {
			return true
		}
	}
	return false
}

func (s *WebReleaseService) load() error {
	data, err := os.ReadFile(filepath.Join(s.cfg.Directory, "index.json"))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read web release index: %w", err)
	}
	var index webReleaseIndex
	if err := json.Unmarshal(data, &index); err != nil {
		return fmt.Errorf("parse web release index: %w", err)
	}
	for i := range index.Releases {
		index.Releases[i].FileName = index.Releases[i].ID + ".zip"
	}
	s.releases = index.Releases
	s.current = cloneActiveWebRelease(index.Current)
	return nil
}

func (s *WebReleaseService) saveLocked() error {
	data, err := json.MarshalIndent(webReleaseIndex{Releases: s.releases, Current: s.current}, "", "  ")
	if err != nil {
		return err
	}
	temp, err := os.CreateTemp(s.cfg.Directory, ".web-index-*.json")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer os.Remove(tempName)
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Chmod(0644); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, filepath.Join(s.cfg.Directory, "index.json"))
}

func cloneActiveWebRelease(value *ActiveWebRelease) *ActiveWebRelease {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
