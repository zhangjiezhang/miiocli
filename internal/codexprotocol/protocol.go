package codexprotocol

const (
	StateUser     = "user"
	StateAccepted = "accepted"
	StateWorking  = "working"
	StateDone     = "done"
	StateError    = "error"
)

type SessionRegistration struct {
	SessionID    string   `json:"session_id"`
	ThreadID     string   `json:"thread_id"`
	TurnID       string   `json:"turn_id,omitempty"`
	Surface      string   `json:"surface"`
	Title        string   `json:"title,omitempty"`
	Project      string   `json:"project,omitempty"`
	Branch       string   `json:"branch,omitempty"`
	State        string   `json:"state"`
	Endpoint     string   `json:"endpoint"`
	Capabilities []string `json:"capabilities,omitempty"`
}

type SessionMessage struct {
	MessageID      string `json:"message_id"`
	SessionID      string `json:"session_id"`
	ThreadID       string `json:"thread_id"`
	ExpectedTurnID string `json:"expected_turn_id,omitempty"`
	Source         string `json:"source"`
	DeviceID       string `json:"device_id,omitempty"`
	Text           string `json:"text"`
	CallbackURL    string `json:"callback_url"`
}

type SessionEvent struct {
	MessageID string `json:"message_id"`
	SessionID string `json:"session_id"`
	ThreadID  string `json:"thread_id,omitempty"`
	TurnID    string `json:"turn_id,omitempty"`
	State     string `json:"state"`
	Message   string `json:"message,omitempty"`
	Text      string `json:"text,omitempty"`
	Result    string `json:"result,omitempty"`
	Error     string `json:"error,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
}
