package claude

import "testing"

// newTestSession creates a minimal Session suitable for unit-testing
// handleUser/handleResult without a real process. The session's state
// machine and turn manager are initialised so the result-handling code
// path works.
func newTestSession(t *testing.T, opts ...SessionOption) *Session {
	t.Helper()
	cfg := defaultConfig()
	for _, o := range opts {
		o(&cfg)
	}
	s := &Session{
		config:      cfg,
		events:      make(chan Event, 100),
		turnManager: newTurnManager(),
		state:       newSessionState(),
		done:        make(chan struct{}),
	}
	s.accumulator = newStreamAccumulator(s)
	_ = s.state.Transition(TransitionStarted)
	_ = s.state.Transition(TransitionInitReceived)
	return s
}
