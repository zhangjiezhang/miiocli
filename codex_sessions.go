package main

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"cn.pascall/gomiio/internal/codexprotocol"
)

const (
	defaultCodexRegisterPath = "/v1/codex/register"
	defaultCodexEventPath    = "/v1/codex/events"
	defaultSessionTTL        = 90 * time.Second
	maxCodexRequestBody      = 64 * 1024
	maxSessionEvents         = 200
	maxSessionMessageRunes   = 4000
)

type CodexSessionService struct {
	cfg            CodexConfig
	apiToken       string
	connectorToken string
	callbackURL    string
	devices        map[string]VoiceDeviceConfig
	httpClient     *http.Client
	nextMessageID  uint64
	mu             sync.Mutex
	sessions       map[string]*codexSession
	bindings       map[string]string
	pending        map[string]*codexPendingMessage
	events         map[string][]codexprotocol.SessionEvent
}

type codexSession struct {
	registration codexprotocol.SessionRegistration
	lastSeen     time.Time
}

type codexPendingMessage struct {
	progress func(string)
	result   chan codexMessageResult
}

type codexMessageResult struct {
	result string
	err    error
}

type codexSessionView struct {
	SessionID    string   `json:"session_id"`
	ThreadID     string   `json:"thread_id"`
	TurnID       string   `json:"turn_id,omitempty"`
	Surface      string   `json:"surface"`
	Title        string   `json:"title"`
	Project      string   `json:"project"`
	Branch       string   `json:"branch"`
	State        string   `json:"state"`
	Online       bool     `json:"online"`
	Capabilities []string `json:"capabilities"`
	BoundDevices []string `json:"bound_devices"`
	LastSeen     string   `json:"last_seen"`
}

type codexMessageInput struct {
	Text     string `json:"text"`
	DeviceID string `json:"device_id,omitempty"`
}

type codexBindingInput struct {
	DeviceID string `json:"device_id"`
}

type codexDeviceOption struct {
	ID string `json:"id"`
}

func NewCodexSessionService(cfg CodexConfig, apiToken string,
	devices map[string]VoiceDeviceConfig) (*CodexSessionService, error) {
	if !cfg.Enabled {
		return nil, errors.New("voice.codex.enabled is false")
	}
	if apiToken == "" {
		return nil, errors.New("voice.apiToken is required for Codex session management")
	}
	connectorToken := cfg.Token
	if connectorToken == "" {
		return nil, errors.New("voice.codex.token is required for Codex session connector authentication")
	}
	if cfg.RegisterPath == "" {
		cfg.RegisterPath = defaultCodexRegisterPath
	}
	if cfg.EventPath == "" {
		cfg.EventPath = defaultCodexEventPath
	}
	cfg.RegisterPath = normalizePath(cfg.RegisterPath)
	cfg.EventPath = normalizePath(cfg.EventPath)
	if err := validateHTTPURL(cfg.CallbackBaseURL, "voice.codex.callbackBaseUrl"); err != nil {
		return nil, err
	}
	return &CodexSessionService{
		cfg: cfg, apiToken: apiToken, connectorToken: connectorToken,
		callbackURL: strings.TrimRight(cfg.CallbackBaseURL, "/") + cfg.EventPath,
		devices:     devices, httpClient: &http.Client{Timeout: 15 * time.Second},
		sessions: make(map[string]*codexSession), bindings: make(map[string]string),
		pending: make(map[string]*codexPendingMessage), events: make(map[string][]codexprotocol.SessionEvent),
	}, nil
}

func (s *CodexSessionService) RegisterPath() string { return s.cfg.RegisterPath }
func (s *CodexSessionService) EventPath() string    { return s.cfg.EventPath }

func (s *CodexSessionService) HandleRegister(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != s.cfg.RegisterPath {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !constantBearerToken(r, s.connectorToken) {
		unauthorized(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxCodexRequestBody)
	defer r.Body.Close()
	var registration codexprotocol.SessionRegistration
	if err := json.NewDecoder(r.Body).Decode(&registration); err != nil ||
		!validSessionRegistration(registration) {
		http.Error(w, "invalid session registration", http.StatusBadRequest)
		return
	}
	registration.Title = truncateRunes(strings.TrimSpace(registration.Title), 120)
	registration.Project = truncateRunes(strings.TrimSpace(registration.Project), 160)
	registration.Branch = truncateRunes(strings.TrimSpace(registration.Branch), 120)

	s.mu.Lock()
	s.sessions[registration.SessionID] = &codexSession{registration: registration, lastSeen: time.Now()}
	s.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{
		"status": "registered", "session_id": registration.SessionID,
	})
}

func (s *CodexSessionService) HandleEvent(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != s.cfg.EventPath {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !constantBearerToken(r, s.connectorToken) {
		unauthorized(w)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxCodexRequestBody)
	defer r.Body.Close()
	var event codexprotocol.SessionEvent
	if err := json.NewDecoder(r.Body).Decode(&event); err != nil || !validSessionEvent(event) {
		http.Error(w, "invalid session event", http.StatusBadRequest)
		return
	}
	if event.CreatedAt == "" {
		event.CreatedAt = time.Now().UTC().Format(time.RFC3339)
	}

	s.mu.Lock()
	if session := s.sessions[event.SessionID]; session != nil {
		if event.ThreadID != "" {
			session.registration.ThreadID = event.ThreadID
		}
		if event.TurnID != "" {
			session.registration.TurnID = event.TurnID
		}
		if event.State == codexprotocol.StateWorking {
			session.registration.State = "running"
		} else if event.State == codexprotocol.StateDone || event.State == codexprotocol.StateError {
			session.registration.State = "idle"
			session.registration.TurnID = ""
		}
		session.lastSeen = time.Now()
	}
	s.appendEventLocked(event.SessionID, event)
	pending := s.pending[event.MessageID]
	s.mu.Unlock()

	if pending != nil {
		switch event.State {
		case codexprotocol.StateAccepted, codexprotocol.StateWorking:
			if pending.progress != nil {
				message := strings.TrimSpace(event.Message)
				if message == "" {
					message = "Codex 正在处理"
				}
				pending.progress(message)
			}
		case codexprotocol.StateDone:
			select {
			case pending.result <- codexMessageResult{result: event.Result}:
			default:
			}
		case codexprotocol.StateError:
			message := strings.TrimSpace(event.Error)
			if message == "" {
				message = "Codex session message failed"
			}
			select {
			case pending.result <- codexMessageResult{err: errors.New(message)}:
			default:
			}
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *CodexSessionService) HandleSessions(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/v1/codex/sessions" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !constantBearerToken(r, s.apiToken) {
		unauthorized(w)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sessions": s.sessionViews(), "devices": s.configuredDevices(),
	})
}

func (s *CodexSessionService) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !constantBearerToken(r, s.apiToken) {
		unauthorized(w)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/v1/codex/sessions/")
	parts := strings.Split(path, "/")
	if len(parts) != 2 {
		http.NotFound(w, r)
		return
	}
	sessionID, err := url.PathUnescape(parts[0])
	if err != nil || sessionID == "" {
		http.Error(w, "invalid session ID", http.StatusBadRequest)
		return
	}
	switch parts[1] {
	case "events":
		s.handleEvents(w, r, sessionID)
	case "messages":
		s.handleMessage(w, r, sessionID)
	case "bind":
		s.handleBinding(w, r, sessionID)
	default:
		http.NotFound(w, r)
	}
}

func (s *CodexSessionService) handleEvents(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.mu.Lock()
	events := append([]codexprotocol.SessionEvent(nil), s.events[sessionID]...)
	_, exists := s.sessions[sessionID]
	s.mu.Unlock()
	if !exists {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"events": events})
}

func (s *CodexSessionService) handleMessage(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
	defer r.Body.Close()
	var input codexMessageInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		http.Error(w, "invalid message", http.StatusBadRequest)
		return
	}
	input.Text = strings.TrimSpace(input.Text)
	if input.Text == "" || utf8.RuneCountInString(input.Text) > maxSessionMessageRunes {
		http.Error(w, "text must contain 1 to 4000 characters", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	session := s.sessions[sessionID]
	online := session != nil && s.onlineLocked(session)
	s.mu.Unlock()
	if session == nil {
		http.Error(w, "Codex session not found", http.StatusNotFound)
		return
	}
	if !online {
		http.Error(w, "Codex session is offline", http.StatusConflict)
		return
	}
	messageID := s.newMessageID()
	ctx, cancel := context.WithTimeout(context.Background(), s.timeout())
	go func() {
		defer cancel()
		_, _ = s.sendToSession(ctx, messageID, sessionID, "web", input.DeviceID, input.Text, nil)
	}()
	writeJSON(w, http.StatusAccepted, map[string]any{"message_id": messageID, "status": "accepted"})
}

func (s *CodexSessionService) handleBinding(w http.ResponseWriter, r *http.Request, sessionID string) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var input codexBindingInput
	if err := json.NewDecoder(r.Body).Decode(&input); err != nil || s.devices[input.DeviceID].Token == "" {
		http.Error(w, "invalid device ID", http.StatusBadRequest)
		return
	}
	s.mu.Lock()
	_, exists := s.sessions[sessionID]
	if exists {
		s.bindings[input.DeviceID] = sessionID
	}
	s.mu.Unlock()
	if !exists {
		http.NotFound(w, r)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"device_id": input.DeviceID, "session_id": sessionID, "status": "bound",
	})
}

func (s *CodexSessionService) Send(ctx context.Context, messageID, deviceID, text string,
	progress func(string)) (string, error) {
	sessionID, err := s.sessionForDevice(deviceID)
	if err != nil {
		return "", err
	}
	return s.sendToSession(ctx, messageID, sessionID, "esp32", deviceID, text, progress)
}

func (s *CodexSessionService) DeviceSessionOptions(deviceID string) []voiceSessionOption {
	views := s.sessionViews()
	s.mu.Lock()
	selectedID := s.bindings[deviceID]
	s.mu.Unlock()
	options := make([]voiceSessionOption, 0, len(views))
	for _, view := range views {
		if !view.Online {
			continue
		}
		title := view.Title
		if title == "" {
			title = view.Project
		}
		if title == "" {
			title = view.SessionID
		}
		options = append(options, voiceSessionOption{
			ID: view.SessionID, Title: truncateRunes(title, 48), Surface: view.Surface,
			State: view.State, Selected: view.SessionID == selectedID,
		})
	}
	sort.Slice(options, func(i, j int) bool {
		if options[i].Title == options[j].Title {
			return options[i].ID < options[j].ID
		}
		return options[i].Title < options[j].Title
	})
	return options
}

func (s *CodexSessionService) SelectNextDeviceSession(deviceID string) (voiceSessionOption, error) {
	options := s.DeviceSessionOptions(deviceID)
	if len(options) == 0 {
		return voiceSessionOption{}, errors.New("no online Codex session")
	}
	next := 0
	for index, option := range options {
		if option.Selected {
			next = (index + 1) % len(options)
			break
		}
	}
	selected := options[next]
	selected.Selected = true
	s.mu.Lock()
	s.bindings[deviceID] = selected.ID
	s.mu.Unlock()
	return selected, nil
}

func (s *CodexSessionService) SelectDeviceSession(deviceID, sessionID string) (voiceSessionOption, error) {
	for _, option := range s.DeviceSessionOptions(deviceID) {
		if option.ID == sessionID {
			option.Selected = true
			s.mu.Lock()
			s.bindings[deviceID] = sessionID
			s.mu.Unlock()
			return option, nil
		}
	}
	return voiceSessionOption{}, errors.New("Codex session is unavailable")
}

func (s *CodexSessionService) sendToSession(ctx context.Context, messageID, sessionID, source,
	deviceID, text string, progress func(string)) (string, error) {
	s.mu.Lock()
	session := s.sessions[sessionID]
	if session == nil || !s.onlineLocked(session) {
		s.mu.Unlock()
		return "", errors.New("Codex session is offline")
	}
	registration := session.registration
	if _, exists := s.pending[messageID]; exists {
		s.mu.Unlock()
		return "", errors.New("duplicate Codex message ID")
	}
	pending := &codexPendingMessage{progress: progress, result: make(chan codexMessageResult, 1)}
	s.pending[messageID] = pending
	s.appendEventLocked(sessionID, codexprotocol.SessionEvent{
		MessageID: messageID, SessionID: sessionID, ThreadID: registration.ThreadID,
		TurnID: registration.TurnID, State: codexprotocol.StateUser, Text: text,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	})
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.pending, messageID)
		s.mu.Unlock()
	}()

	message := codexprotocol.SessionMessage{
		MessageID: messageID, SessionID: sessionID, ThreadID: registration.ThreadID,
		ExpectedTurnID: registration.TurnID, Source: source, DeviceID: deviceID,
		Text: text, CallbackURL: s.callbackURL,
	}
	if err := s.postMessage(ctx, registration.Endpoint, message); err != nil {
		s.recordDispatchError(sessionID, messageID, err)
		return "", err
	}
	select {
	case result := <-pending.result:
		return result.result, result.err
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (s *CodexSessionService) postMessage(ctx context.Context, endpoint string,
	message codexprotocol.SessionMessage) error {
	body, err := json.Marshal(message)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.Header.Set("Authorization", "Bearer "+s.connectorToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", message.MessageID)
	response, err := s.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("send session message: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		payload, _ := io.ReadAll(io.LimitReader(response.Body, 2048))
		return fmt.Errorf("session connector returned HTTP %d: %s",
			response.StatusCode, strings.TrimSpace(string(payload)))
	}
	return nil
}

func (s *CodexSessionService) sessionForDevice(deviceID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sessionID := s.bindings[deviceID]; sessionID != "" {
		if session := s.sessions[sessionID]; session != nil && s.onlineLocked(session) {
			return sessionID, nil
		}
	}
	available := make([]string, 0, len(s.sessions))
	for sessionID, session := range s.sessions {
		if s.onlineLocked(session) {
			available = append(available, sessionID)
		}
	}
	if len(available) == 1 {
		s.bindings[deviceID] = available[0]
		return available[0], nil
	}
	if len(available) == 0 {
		return "", errors.New("no online Codex session")
	}
	return "", errors.New("multiple Codex sessions are online; select one first")
}

func (s *CodexSessionService) sessionViews() []codexSessionView {
	s.mu.Lock()
	defer s.mu.Unlock()
	views := make([]codexSessionView, 0, len(s.sessions))
	for sessionID, session := range s.sessions {
		registration := session.registration
		view := codexSessionView{
			SessionID: sessionID, ThreadID: registration.ThreadID, TurnID: registration.TurnID,
			Surface: registration.Surface, Title: registration.Title, Project: registration.Project,
			Branch: registration.Branch, State: registration.State, Online: s.onlineLocked(session),
			Capabilities: append([]string(nil), registration.Capabilities...),
			BoundDevices: []string{}, LastSeen: session.lastSeen.UTC().Format(time.RFC3339),
		}
		if !view.Online {
			view.State = "offline"
		}
		for deviceID, boundSessionID := range s.bindings {
			if boundSessionID == sessionID {
				view.BoundDevices = append(view.BoundDevices, deviceID)
			}
		}
		sort.Strings(view.BoundDevices)
		views = append(views, view)
	}
	sort.Slice(views, func(i, j int) bool {
		if views[i].Online != views[j].Online {
			return views[i].Online
		}
		return views[i].LastSeen > views[j].LastSeen
	})
	return views
}

func (s *CodexSessionService) configuredDevices() []codexDeviceOption {
	devices := make([]codexDeviceOption, 0, len(s.devices))
	for deviceID := range s.devices {
		devices = append(devices, codexDeviceOption{ID: deviceID})
	}
	sort.Slice(devices, func(i, j int) bool { return devices[i].ID < devices[j].ID })
	return devices
}

func (s *CodexSessionService) recordDispatchError(sessionID, messageID string, err error) {
	event := codexprotocol.SessionEvent{
		MessageID: messageID, SessionID: sessionID, State: codexprotocol.StateError,
		Error: truncateRunes(err.Error(), 500), CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	s.mu.Lock()
	s.appendEventLocked(sessionID, event)
	s.mu.Unlock()
}

func (s *CodexSessionService) appendEventLocked(sessionID string, event codexprotocol.SessionEvent) {
	events := append(s.events[sessionID], event)
	if len(events) > maxSessionEvents {
		events = append([]codexprotocol.SessionEvent(nil), events[len(events)-maxSessionEvents:]...)
	}
	s.events[sessionID] = events
}

func (s *CodexSessionService) onlineLocked(session *codexSession) bool {
	ttl := defaultSessionTTL
	if s.cfg.SessionTTLSeconds > 0 {
		ttl = time.Duration(s.cfg.SessionTTLSeconds) * time.Second
	}
	return time.Since(session.lastSeen) <= ttl
}

func (s *CodexSessionService) timeout() time.Duration {
	if s.cfg.TimeoutSeconds > 0 {
		return time.Duration(s.cfg.TimeoutSeconds) * time.Second
	}
	return defaultCodexTimeout
}

func (s *CodexSessionService) newMessageID() string {
	next := atomic.AddUint64(&s.nextMessageID, 1)
	return fmt.Sprintf("web-%d-%d", time.Now().UnixMilli(), next)
}

func validSessionRegistration(registration codexprotocol.SessionRegistration) bool {
	if registration.SessionID == "" || registration.ThreadID == "" || registration.Surface == "" ||
		registration.State == "" {
		return false
	}
	return validateHTTPURL(registration.Endpoint, "endpoint") == nil
}

func validSessionEvent(event codexprotocol.SessionEvent) bool {
	if event.MessageID == "" || event.SessionID == "" {
		return false
	}
	switch event.State {
	case codexprotocol.StateAccepted, codexprotocol.StateWorking,
		codexprotocol.StateDone, codexprotocol.StateError:
		return true
	default:
		return false
	}
}

func validateHTTPURL(value, name string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("%s must be an http or https URL", name)
	}
	return nil
}

func normalizePath(path string) string {
	if strings.HasPrefix(path, "/") {
		return path
	}
	return "/" + path
}

func constantBearerToken(r *http.Request, expected string) bool {
	provided := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	return len(provided) == len(expected) &&
		subtle.ConstantTimeCompare([]byte(provided), []byte(expected)) == 1
}

func unauthorized(w http.ResponseWriter) {
	w.Header().Set("WWW-Authenticate", "Bearer")
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}
