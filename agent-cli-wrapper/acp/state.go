package acp

import "sync"

// ClientState represents the state of the ACP client.
type ClientState int

const (
	ClientStateUninitialized ClientState = iota
	ClientStateStarting
	ClientStateReady
	ClientStateClosed
)

func (s ClientState) String() string {
	switch s {
	case ClientStateUninitialized:
		return "uninitialized"
	case ClientStateStarting:
		return "starting"
	case ClientStateReady:
		return "ready"
	case ClientStateClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// clientStateManager manages thread-safe client state transitions.
type clientStateManager struct {
	mu    sync.RWMutex
	state ClientState
}

func newClientStateManager() *clientStateManager {
	return &clientStateManager{state: ClientStateUninitialized}
}

func (m *clientStateManager) Current() ClientState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

func (m *clientStateManager) SetStarting() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != ClientStateUninitialized {
		return ErrInvalidState
	}
	m.state = ClientStateStarting
	return nil
}

func (m *clientStateManager) SetReady() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != ClientStateStarting {
		return ErrInvalidState
	}
	m.state = ClientStateReady
	return nil
}

func (m *clientStateManager) SetClosed() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = ClientStateClosed
}

// SessionState represents the state of an ACP session.
type SessionState int

const (
	SessionStateCreated    SessionState = iota
	SessionStateReady                   // Session created, ready for prompts
	SessionStateProcessing              // A prompt is in progress
	SessionStateClosed
)

func (s SessionState) String() string {
	switch s {
	case SessionStateCreated:
		return "created"
	case SessionStateReady:
		return "ready"
	case SessionStateProcessing:
		return "processing"
	case SessionStateClosed:
		return "closed"
	default:
		return "unknown"
	}
}

// sessionStateManager manages thread-safe session state transitions.
type sessionStateManager struct {
	mu    sync.RWMutex
	state SessionState
}

func newSessionStateManager() *sessionStateManager {
	return &sessionStateManager{state: SessionStateCreated}
}

func (m *sessionStateManager) Current() SessionState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state
}

func (m *sessionStateManager) SetReady() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != SessionStateCreated && m.state != SessionStateProcessing {
		return ErrInvalidState
	}
	m.state = SessionStateReady
	return nil
}

func (m *sessionStateManager) SetProcessing() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != SessionStateReady {
		return ErrInvalidState
	}
	m.state = SessionStateProcessing
	return nil
}

func (m *sessionStateManager) SetClosed() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = SessionStateClosed
}

func (m *sessionStateManager) IsReady() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state == SessionStateReady
}

func (m *sessionStateManager) IsClosed() bool {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.state == SessionStateClosed
}
