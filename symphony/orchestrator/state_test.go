package orchestrator

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/symphony/agent"
	"github.com/bazelment/yoloswe/symphony/model"
)

type stateTestClock struct {
	now time.Time
}

func (c stateTestClock) Now() time.Time               { return c.now }
func (stateTestClock) NewTimer(time.Duration) Timer   { panic("unexpected timer in state test") }
func (stateTestClock) NewTicker(time.Duration) Ticker { panic("unexpected ticker in state test") }
func (stateTestClock) AfterFunc(time.Duration, func()) Timer {
	panic("unexpected after-func in state test")
}

func newStateTestOrchestrator(now time.Time) *Orchestrator {
	return &Orchestrator{
		clock:   stateTestClock{now: now},
		running: make(map[string]*model.RunningEntry),
	}
}

func TestHandleAgentUpdateRecordsSessionProgress(t *testing.T) {
	t.Parallel()

	now := time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC)
	o := newStateTestOrchestrator(now)
	o.running["issue-1"] = &model.RunningEntry{
		Issue:      model.Issue{ID: "issue-1", Identifier: "SYM-1", State: "Todo"},
		Identifier: "SYM-1",
	}
	pid := 4242
	rateLimits := json.RawMessage(`{"primary": "ok"}`)

	o.handleAgentUpdate(AgentUpdate{
		IssueID: "issue-1",
		Event: agent.Event{
			Type:         agent.EventSessionStarted,
			SessionID:    "session-1",
			ThreadID:     "thread-1",
			TurnID:       "turn-1",
			PID:          &pid,
			Message:      "started",
			InputTokens:  40,
			OutputTokens: 12,
			TotalTokens:  52,
			RateLimits:   rateLimits,
		},
	})

	session := o.running["issue-1"].Session
	if session.SessionID != "session-1" || session.ThreadID != "thread-1" || session.TurnID != "turn-1" {
		t.Fatalf("session identity = (%q, %q, %q), want session/thread/turn IDs",
			session.SessionID, session.ThreadID, session.TurnID)
	}
	if session.AgentPID == nil || *session.AgentPID != "4242" {
		t.Fatalf("AgentPID = %v, want 4242", session.AgentPID)
	}
	if session.LastAgentTimestamp == nil || !session.LastAgentTimestamp.Equal(now) {
		t.Fatalf("LastAgentTimestamp = %v, want %v", session.LastAgentTimestamp, now)
	}
	if session.LastAgentEvent == nil || *session.LastAgentEvent != string(agent.EventSessionStarted) {
		t.Fatalf("LastAgentEvent = %v, want %q", session.LastAgentEvent, agent.EventSessionStarted)
	}
	if session.LastAgentMessage != "started" {
		t.Fatalf("LastAgentMessage = %q, want started", session.LastAgentMessage)
	}
	if session.TurnCount != 1 {
		t.Fatalf("TurnCount = %d, want 1", session.TurnCount)
	}
	if session.InputTokens != 40 || session.OutputTokens != 12 || session.TotalTokens != 52 {
		t.Fatalf("tokens = (%d, %d, %d), want (40, 12, 52)",
			session.InputTokens, session.OutputTokens, session.TotalTokens)
	}
	if string(o.rateLimits) != string(rateLimits) {
		t.Fatalf("rateLimits = %s, want %s", o.rateLimits, rateLimits)
	}
}

func TestHandleAgentUpdateAccumulatesTokenDeltas(t *testing.T) {
	t.Parallel()

	o := newStateTestOrchestrator(time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC))
	o.running["issue-1"] = &model.RunningEntry{Issue: model.Issue{ID: "issue-1"}}

	o.handleAgentUpdate(AgentUpdate{
		IssueID: "issue-1",
		Event: agent.Event{
			Type:         agent.EventTokenUsage,
			InputTokens:  10,
			OutputTokens: 5,
			TotalTokens:  15,
		},
	})
	o.handleAgentUpdate(AgentUpdate{
		IssueID: "issue-1",
		Event: agent.Event{
			Type:         agent.EventTokenUsage,
			InputTokens:  18,
			OutputTokens: 9,
			TotalTokens:  27,
		},
	})

	session := o.running["issue-1"].Session
	if session.InputTokens != 18 || session.OutputTokens != 9 || session.TotalTokens != 27 {
		t.Fatalf("tokens = (%d, %d, %d), want cumulative deltas (18, 9, 27)",
			session.InputTokens, session.OutputTokens, session.TotalTokens)
	}
	if session.LastReportedInputToks != 18 || session.LastReportedOutputToks != 9 || session.LastReportedTotalToks != 27 {
		t.Fatalf("last reported = (%d, %d, %d), want (18, 9, 27)",
			session.LastReportedInputToks, session.LastReportedOutputToks, session.LastReportedTotalToks)
	}
}

func TestBuildSnapshotIncludesLiveTotals(t *testing.T) {
	t.Parallel()

	startedAt := time.Date(2026, 5, 13, 10, 0, 0, 0, time.UTC)
	now := startedAt.Add(90 * time.Second)
	o := newStateTestOrchestrator(now)
	o.totals = model.AgentTotals{InputTokens: 100, OutputTokens: 50, TotalTokens: 150, SecondsRunning: 30}
	o.running["issue-1"] = &model.RunningEntry{
		Issue:      model.Issue{ID: "issue-1", Identifier: "SYM-1", State: "Todo"},
		Identifier: "SYM-1",
		StartedAt:  startedAt,
		Session: model.LiveSession{
			SessionID:    "session-1",
			InputTokens:  8,
			OutputTokens: 4,
			TotalTokens:  12,
		},
	}

	snap := o.buildSnapshot()

	if !snap.GeneratedAt.Equal(now) {
		t.Fatalf("GeneratedAt = %v, want %v", snap.GeneratedAt, now)
	}
	if len(snap.Running) != 1 {
		t.Fatalf("running count = %d, want 1", len(snap.Running))
	}
	if snap.Running[0].SessionID != "session-1" || snap.Running[0].Tokens.TotalTokens != 12 {
		t.Fatalf("running snapshot = %+v, want session ID and token totals", snap.Running[0])
	}
	if snap.Totals.SecondsRunning != 120 {
		t.Fatalf("SecondsRunning = %v, want 120", snap.Totals.SecondsRunning)
	}
}
