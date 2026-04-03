package orchestrator

// Metrics and token accounting are handled inline in state.go:
// - handleAgentUpdate: delta-based token accounting per session
// - handleWorkerExit: add runtime seconds + session tokens to aggregate totals
// - buildSnapshot: compute live totals with active session elapsed time
