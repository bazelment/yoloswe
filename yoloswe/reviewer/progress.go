package reviewer

import (
	"encoding/json"
	"io"
	"sync"
	"time"
)

// ProgressEvent is a single machine-readable progress record. Serialized as
// one line of JSON on stdout by NDJSONProgressEmitter, interleaved with (but
// distinguishable from) the final ResultEnvelope. The envelope has a
// "schema_version" top-level key; progress events have "event":"progress".
// Consumers (e.g. /pr-polish Monitor wrappers) scan the stream for the last
// line with "schema_version" to find the terminal envelope, and treat every
// other line with "event":"progress" as an intermediate event.
type ProgressEvent struct {
	// Event is always "progress". Serialized first so consumers can cheaply
	// distinguish progress events from the envelope by looking at the first
	// JSON key without parsing the whole line.
	Event string `json:"event"`
	// Kind is the progress category. See ProgressKind* constants.
	Kind string `json:"kind"`
	// Backend is the reviewer backend name (codex, cursor); empty if not
	// known when the event fires (shouldn't happen after SessionInfo).
	Backend string `json:"backend,omitempty"`
	// Model is the backend model name when known.
	Model string `json:"model,omitempty"`
	// SessionID is the reviewer session id when known.
	SessionID string `json:"session_id,omitempty"`
	// Tool is the name of the tool invoked, for tool-use events.
	Tool string `json:"tool,omitempty"`
	// Detail is a short human-readable hint (e.g. file being read). Never
	// contains secrets or full paths; see summarizeToolInput for the sanitizer.
	Detail string `json:"detail,omitempty"`
	// IssueCount is populated for verdict events.
	IssueCount int `json:"issue_count,omitempty"`
}

// Progress event kinds. Keep narrow — every new kind is a new contract with
// consumers. Prefer adding Detail variants over new kinds.
const (
	ProgressKindSessionStarted = "session-started"
	ProgressKindToolUse        = "tool-use"
	ProgressKindTurnComplete   = "turn-complete"
	ProgressKindVerdict        = "verdict"
	ProgressKindError          = "error"
)

// ProgressEmitter is the seam between the reviewer event bridge and the
// structured-event consumer. The default implementation (NDJSONProgressEmitter)
// writes to stdout when --json is set; otherwise a noopProgressEmitter is used
// and the human-readable renderer.Renderer remains the only output surface.
type ProgressEmitter interface {
	Emit(ev ProgressEvent)
}

// NDJSONProgressEmitter serializes events as one-line JSON to w. It is safe
// for concurrent use across goroutines — backends spawn event bridges and tool
// handlers that may race — and coalesces bursts of the same kind at the same
// call site to stay within Monitor's event budget.
//
// Coalescing strategy: each kind+tool pair has an independent minimum
// interval. session-started and verdict always pass through (interval 0) so
// the terminal bookends never get suppressed. tool-use passes the first event
// immediately, then drops further events for the same tool for the
// configured interval — this matches Monitor's "one notification per event"
// contract while still surfacing new tool invocations quickly.
type NDJSONProgressEmitter struct {
	w        io.Writer
	now      func() time.Time
	last     map[string]time.Time
	interval time.Duration
	mu       sync.Mutex
}

// NewNDJSONProgressEmitter constructs an emitter writing to w. Callers should
// wrap os.Stdout when the --json contract is in force. interval is the
// per-(kind,tool) minimum gap for coalesced kinds; pass 0 to disable
// coalescing (useful in tests). A nil writer yields a no-op emitter.
func NewNDJSONProgressEmitter(w io.Writer, interval time.Duration) *NDJSONProgressEmitter {
	return &NDJSONProgressEmitter{
		w:        w,
		interval: interval,
		now:      time.Now,
		last:     map[string]time.Time{},
	}
}

// SetNow overrides the time source; tests use this to drive the coalescer
// deterministically.
func (e *NDJSONProgressEmitter) SetNow(now func() time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.now = now
}

// Emit writes ev to w unless the coalescer suppresses it. Writes are best-
// effort — a serialization or write error is swallowed rather than letting a
// progress-stream hiccup bring down the review. The error would otherwise have
// to propagate up through the event bridge and we'd lose the review itself to
// a reporting problem.
func (e *NDJSONProgressEmitter) Emit(ev ProgressEvent) {
	if e == nil || e.w == nil {
		return
	}
	ev.Event = "progress"
	if e.shouldSuppress(ev) {
		return
	}
	encoded, err := json.Marshal(ev)
	if err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	_, _ = e.w.Write(encoded)
	_, _ = e.w.Write([]byte{'\n'})
}

// shouldSuppress checks whether ev falls inside the coalesce window for its
// (kind, tool) key. It also updates the last-seen timestamp on pass-through.
// Kinds that are never coalesced (session-started, verdict, turn-complete,
// error) bypass the check entirely so the stream's structural markers always
// land promptly.
func (e *NDJSONProgressEmitter) shouldSuppress(ev ProgressEvent) bool {
	if e.interval <= 0 {
		return false
	}
	switch ev.Kind {
	case ProgressKindSessionStarted, ProgressKindVerdict,
		ProgressKindTurnComplete, ProgressKindError:
		return false
	}
	key := ev.Kind + ":" + ev.Tool
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	if last, ok := e.last[key]; ok {
		if now.Sub(last) < e.interval {
			return true
		}
	}
	e.last[key] = now
	return false
}

type noopProgressEmitter struct{}

func (noopProgressEmitter) Emit(ProgressEvent) {}

// NoopProgressEmitter returns an emitter that discards every event. Used when
// --json is not set or the caller wants to opt out of structured output.
func NoopProgressEmitter() ProgressEmitter { return noopProgressEmitter{} }
