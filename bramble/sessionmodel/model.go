package sessionmodel

import "sync"

const defaultMaxLines = 1000

// SessionModel is the single source of truth for a session's state.
// The write API is called by MessageParser / Controller; the read API
// is called by the View.
type SessionModel struct {
	meta      SessionMeta
	output    *OutputBuffer
	progress  ProgressSnapshot
	mu        sync.RWMutex
	observers []Observer
}

// NewSessionModel creates a model with the given output buffer capacity.
func NewSessionModel(maxLines int) *SessionModel {
	if maxLines <= 0 {
		maxLines = defaultMaxLines
	}
	return &SessionModel{
		output: NewOutputBuffer(maxLines),
	}
}

// --- Write API (called by MessageParser / Controller) -----------------------

// SetMeta replaces the session metadata and notifies observers.
func (m *SessionModel) SetMeta(meta SessionMeta) {
	m.mu.Lock()
	m.meta = meta
	m.mu.Unlock()
	m.notify(MetaUpdated{})
}

// UpdateStatus changes the session status and notifies observers.
func (m *SessionModel) UpdateStatus(status SessionStatus) {
	m.mu.Lock()
	old := m.meta.Status
	m.meta.Status = status
	m.mu.Unlock()
	m.notify(StatusChanged{Old: old, New: status})
}

// AppendOutput adds a complete output line and notifies observers.
func (m *SessionModel) AppendOutput(line OutputLine) {
	m.output.Append(line)
	m.notify(OutputAppended{})
}

// AppendStreamingText appends a text delta to the output buffer.
func (m *SessionModel) AppendStreamingText(delta string) {
	m.output.AppendStreamingText(delta)
	m.notify(OutputAppended{})
}

// AppendStreamingThinking appends a thinking delta to the output buffer.
func (m *SessionModel) AppendStreamingThinking(delta string) {
	m.output.AppendStreamingThinking(delta)
	m.notify(OutputAppended{})
}

// UpdateTool finds a tool_start line by ID and applies fn.
func (m *SessionModel) UpdateTool(toolID string, fn func(*OutputLine)) {
	if m.output.UpdateToolByID(toolID, fn) {
		m.notify(OutputAppended{})
	}
}

// UpdateProgress applies fn to the session progress under a lock.
func (m *SessionModel) UpdateProgress(fn func(*ProgressSnapshot)) {
	m.mu.Lock()
	fn(&m.progress)
	m.mu.Unlock()
	m.notify(ProgressUpdated{})
}

// --- Read API (called by View) ----------------------------------------------

// Meta returns a snapshot of the session metadata.
func (m *SessionModel) Meta() SessionMeta {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.meta
}

// Output returns a deep-copied snapshot of all output lines.
func (m *SessionModel) Output() []OutputLine {
	return m.output.Snapshot()
}

// Progress returns a snapshot of the session progress.
func (m *SessionModel) Progress() ProgressSnapshot {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.progress
}

// --- Observer management ----------------------------------------------------

// AddObserver registers an observer that will be notified on model mutations.
func (m *SessionModel) AddObserver(o Observer) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.observers = append(m.observers, o)
}

// notify sends an event to all registered observers.
// Observers are called synchronously; keep handlers fast.
func (m *SessionModel) notify(event ModelEvent) {
	m.mu.RLock()
	obs := m.observers
	m.mu.RUnlock()
	for _, o := range obs {
		o.OnModelEvent(event)
	}
}
