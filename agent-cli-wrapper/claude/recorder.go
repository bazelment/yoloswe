package claude

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// RecordingMetadata contains metadata about a recording.
type RecordingMetadata struct {
	SessionID         string   `json:"session_id"`
	Model             string   `json:"model"`
	WorkDir           string   `json:"cwd"`
	ClaudeCodeVersion string   `json:"claude_code_version,omitempty"`
	PermissionMode    string   `json:"permission_mode"`
	StartTime         string   `json:"start_time"`
	Tools             []string `json:"tools"`
	TotalTurns        int      `json:"total_turns"`
}

// TurnSummary contains a summary of a recorded turn.
type TurnSummary struct {
	StartTime   time.Time `json:"start_time"`
	EndTime     time.Time `json:"end_time,omitempty"`
	UserMessage string    `json:"user_message,omitempty"`
	Number      int       `json:"number"`
	DurationMs  int64     `json:"duration_ms,omitempty"`
	CostUSD     float64   `json:"cost_usd,omitempty"`
	Success     bool      `json:"success"`
}

// SessionRecording contains a complete recording.
type SessionRecording struct {
	Turns    []TurnSummary     `json:"turns"`
	Metadata RecordingMetadata `json:"metadata"`
}

// TotalCost returns the total cost of all turns.
func (r *SessionRecording) TotalCost() float64 {
	var total float64
	for _, turn := range r.Turns {
		total += turn.CostUSD
	}
	return total
}

// RecordedMessage is the container format for recorded messages.
// Each message is wrapped with a timestamp and direction indicator.
type RecordedMessage struct {
	Timestamp time.Time   `json:"timestamp"`
	Message   interface{} `json:"message"`
	Direction string      `json:"direction"`
}

// MarshalJSON implements custom JSON marshaling with millisecond timestamp precision.
func (r RecordedMessage) MarshalJSON() ([]byte, error) {
	type alias struct {
		Message   interface{} `json:"message"`
		Timestamp string      `json:"timestamp"`
		Direction string      `json:"direction"`
	}
	return json.Marshal(alias{
		Timestamp: r.Timestamp.UTC().Format("2006-01-02T15:04:05.000Z"),
		Direction: r.Direction,
		Message:   r.Message,
	})
}

// UnmarshalJSON implements custom JSON unmarshaling for timestamps.
func (r *RecordedMessage) UnmarshalJSON(data []byte) error {
	type alias struct {
		Timestamp string          `json:"timestamp"`
		Direction string          `json:"direction"`
		Message   json.RawMessage `json:"message"`
	}
	var a alias
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}

	t, err := time.Parse("2006-01-02T15:04:05.000Z", a.Timestamp)
	if err != nil {
		// Try RFC3339 as fallback
		t, err = time.Parse(time.RFC3339, a.Timestamp)
		if err != nil {
			return err
		}
	}

	r.Timestamp = t
	r.Direction = a.Direction
	r.Message = a.Message
	return nil
}

// sessionRecorder records session messages to disk.
type sessionRecorder struct {
	messagesFile    *os.File
	baseDir         string
	sessionID       string
	dirPath         string
	turns           []TurnSummary
	pendingMessages []RecordedMessage
	metadata        RecordingMetadata
	mu              sync.Mutex
	initialized     bool
}

// newSessionRecorder creates a new session recorder.
func newSessionRecorder(baseDir string) *sessionRecorder {
	if baseDir == "" {
		baseDir = ".claude-sessions"
	}
	return &sessionRecorder{
		baseDir:         baseDir,
		turns:           make([]TurnSummary, 0),
		pendingMessages: make([]RecordedMessage, 0),
	}
}

// Initialize initializes the recorder with session metadata.
func (sr *sessionRecorder) Initialize(meta RecordingMetadata) error {
	sr.mu.Lock()
	defer sr.mu.Unlock()

	if sr.initialized {
		return nil
	}

	sr.sessionID = meta.SessionID
	sr.metadata = meta
	sr.metadata.StartTime = time.Now().Format(time.RFC3339)

	// Create session directory
	sr.dirPath = filepath.Join(sr.baseDir, fmt.Sprintf("session-%s-%d", sr.sessionID, time.Now().Unix()))
	if err := os.MkdirAll(sr.dirPath, 0755); err != nil {
		return err
	}

	// Open single messages file
	var err error
	sr.messagesFile, err = os.Create(filepath.Join(sr.dirPath, "messages.jsonl"))
	if err != nil {
		return err
	}

	sr.initialized = true

	// Sort pending messages by timestamp and flush
	sort.Slice(sr.pendingMessages, func(i, j int) bool {
		return sr.pendingMessages[i].Timestamp.Before(sr.pendingMessages[j].Timestamp)
	})

	for _, record := range sr.pendingMessages {
		sr.writeRecord(record)
	}
	sr.pendingMessages = nil // Clear the buffer

	return nil
}

// writeRecord writes a single record to the messages file.
// Must be called with lock held.
func (sr *sessionRecorder) writeRecord(record RecordedMessage) {
	if sr.messagesFile == nil {
		return
	}

	data, err := json.Marshal(record)
	if err != nil {
		return
	}

	sr.messagesFile.Write(data)
	sr.messagesFile.Write([]byte("\n"))
}

// RecordSent records a message sent to the CLI.
func (sr *sessionRecorder) RecordSent(msg interface{}) {
	sr.mu.Lock()
	defer sr.mu.Unlock()

	record := RecordedMessage{
		Timestamp: time.Now(),
		Direction: "sent",
		Message:   msg,
	}

	if !sr.initialized {
		sr.pendingMessages = append(sr.pendingMessages, record)
		return
	}

	sr.writeRecord(record)
}

// RecordReceived records raw bytes received from the CLI.
// The raw JSON is preserved verbatim using json.RawMessage, ensuring
// unknown message types are never lost during recording.
func (sr *sessionRecorder) RecordReceived(raw []byte) {
	sr.mu.Lock()
	defer sr.mu.Unlock()

	// Copy the raw bytes so the caller can reuse the buffer.
	copied := make(json.RawMessage, len(raw))
	copy(copied, raw)

	record := RecordedMessage{
		Timestamp: time.Now(),
		Direction: "received",
		Message:   copied,
	}

	if !sr.initialized {
		sr.pendingMessages = append(sr.pendingMessages, record)
		return
	}

	sr.writeRecord(record)
}

// StartTurn starts recording a new turn.
func (sr *sessionRecorder) StartTurn(turnNumber int, userMessage interface{}) {
	sr.mu.Lock()
	defer sr.mu.Unlock()

	msgStr := ""
	if s, ok := userMessage.(string); ok {
		msgStr = s
	}

	summary := TurnSummary{
		Number:      turnNumber,
		UserMessage: msgStr,
		StartTime:   time.Now(),
	}

	sr.turns = append(sr.turns, summary)
}

// CompleteTurn marks a turn as complete.
func (sr *sessionRecorder) CompleteTurn(turnNumber int, result TurnResult) {
	sr.mu.Lock()
	defer sr.mu.Unlock()

	for i := range sr.turns {
		if sr.turns[i].Number == turnNumber {
			sr.turns[i].EndTime = time.Now()
			sr.turns[i].DurationMs = result.DurationMs
			sr.turns[i].CostUSD = result.Usage.CostUSD
			sr.turns[i].Success = result.Success
			break
		}
	}

	sr.metadata.TotalTurns = len(sr.turns)
	sr.saveMeta()
}

// saveMeta saves the metadata file.
func (sr *sessionRecorder) saveMeta() {
	if !sr.initialized {
		return
	}

	recording := SessionRecording{
		Metadata: sr.metadata,
		Turns:    sr.turns,
	}

	data, err := json.MarshalIndent(recording, "", "  ")
	if err != nil {
		return
	}

	metaPath := filepath.Join(sr.dirPath, "meta.json")
	os.WriteFile(metaPath, data, 0644)
}

// GetRecording returns the current recording.
func (sr *sessionRecorder) GetRecording() *SessionRecording {
	sr.mu.Lock()
	defer sr.mu.Unlock()

	if !sr.initialized {
		return nil
	}

	return &SessionRecording{
		Metadata: sr.metadata,
		Turns:    sr.turns,
	}
}

// Path returns the recording directory path.
func (sr *sessionRecorder) Path() string {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	return sr.dirPath
}

// Close closes the recorder and saves final metadata.
func (sr *sessionRecorder) Close() error {
	sr.mu.Lock()
	defer sr.mu.Unlock()

	if !sr.initialized {
		return nil
	}

	sr.saveMeta()

	if sr.messagesFile != nil {
		sr.messagesFile.Close()
	}

	return nil
}

// LoadRecording loads a recording from disk.
func LoadRecording(dirPath string) (*SessionRecording, error) {
	metaPath := filepath.Join(dirPath, "meta.json")
	data, err := os.ReadFile(metaPath)
	if err != nil {
		return nil, err
	}

	var recording SessionRecording
	if err := json.Unmarshal(data, &recording); err != nil {
		return nil, err
	}

	return &recording, nil
}

// LoadMessages loads recorded messages from a session directory.
// Returns all messages in chronological order with timestamps and direction.
func LoadMessages(dirPath string) ([]RecordedMessage, error) {
	messagesPath := filepath.Join(dirPath, "messages.jsonl")
	file, err := os.Open(messagesPath)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var messages []RecordedMessage
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var record RecordedMessage
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			continue // skip malformed lines
		}
		messages = append(messages, record)
	}
	return messages, scanner.Err()
}
