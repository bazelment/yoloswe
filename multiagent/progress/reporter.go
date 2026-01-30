package progress

// Reporter is the interface for progress reporting.
type Reporter interface {
	// Event sends a progress event.
	Event(event Event)

	// Close closes the reporter (flushes any buffered output).
	Close()
}

// NullReporter is a no-op implementation.
type NullReporter struct{}

// Event is a no-op.
func (NullReporter) Event(Event) {}

// Close is a no-op.
func (NullReporter) Close() {}

// AgentReporter wraps a Reporter to implement the agent.ProgressReporter interface
// which uses interface{} for events to avoid import cycles.
type AgentReporter struct {
	r Reporter
}

// NewAgentReporter creates an adapter that implements agent.ProgressReporter.
func NewAgentReporter(r Reporter) *AgentReporter {
	return &AgentReporter{r: r}
}

// Event sends a progress event. It expects an Event type but accepts interface{}.
func (a *AgentReporter) Event(event interface{}) {
	if e, ok := event.(Event); ok {
		a.r.Event(e)
	}
}

// Close closes the underlying reporter.
func (a *AgentReporter) Close() {
	a.r.Close()
}
