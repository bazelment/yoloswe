package agent

import (
	"errors"
	"fmt"
	"testing"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
	transientmeta "github.com/bazelment/yoloswe/agent-cli-wrapper/transient"
)

func TestIsTransient(t *testing.T) {
	tests := []struct {
		err  error
		name string
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{name: "claude transient", err: &claude.TransientError{Message: "stream idle"}, want: true},
		{name: "codex transient", err: &codex.TransientError{Message: "connection reset"}, want: true},
		{name: "wrapped transient", err: fmt.Errorf("agent execution: %w", &codex.TransientError{Message: "429"}), want: true},
		{name: "raw 429", err: errors.New("429 Too Many Requests"), want: true},
		{name: "raw connection reset", err: errors.New("read tcp: connection reset by peer"), want: true},
		// Verbatim from jiradozer cron plan-step failures (2026-06-04/05).
		{name: "raw socket closed", err: errors.New("API Error: The socket connection was closed unexpectedly. For more information, pass `verbose: true` in the second argument to fetch()"), want: true},
		{name: "wrapped socket closed", err: fmt.Errorf("agent execution: %w", errors.New("API Error: The socket connection was closed unexpectedly")), want: true},
		{name: "plain error", err: errors.New("syntax error"), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsTransient(tt.err); got != tt.want {
				t.Fatalf("IsTransient() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestIsOutOfCredits(t *testing.T) {
	tests := []struct {
		err  error
		name string
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{
			name: "claude turn error out of credits",
			err:  &claude.TurnError{Message: "Your workspace is out of credits. Ask your workspace owner to refill."},
			want: true,
		},
		{
			name: "codex turn error out of credits",
			err:  &codex.TurnError{Message: "request failed: out of credits"},
			want: true,
		},
		{
			name: "wrapped out of credits",
			err:  fmt.Errorf("agent execution: %w", &codex.TurnError{Message: "please ask your Workspace Owner to refill"}),
			want: true,
		},
		{
			name: "transient stream idle is not out of credits",
			err:  &claude.TransientError{Message: "stream idle"},
			want: false,
		},
		{name: "plain error", err: errors.New("syntax error"), want: false},
		// Claude.ai plan limit windows. Wording varies by which window tripped,
		// but each carries a "· resets … (UTC)" clause. See INF-1807/1854/etc.
		{
			name: "claude session limit",
			err:  errors.New("You've hit your session limit · resets 9:30am (UTC)"),
			want: true,
		},
		{
			name: "claude session limit varying reset time",
			err:  errors.New("You've hit your session limit · resets 4:10pm (UTC)"),
			want: true,
		},
		{
			name: "claude general/weekly limit",
			err:  errors.New("You've hit your limit · resets May 16, 5pm (UTC)"),
			want: true,
		},
		{
			name: "claude usage limit defensive",
			err:  errors.New("You've hit your usage limit · resets 1am (UTC)"),
			want: true,
		},
		{
			name: "claude session limit wrapped in typed turn error",
			err:  fmt.Errorf("agent execution: %w", &claude.TurnError{Message: "You've hit your session limit · resets 9:30am (UTC)"}),
			want: true,
		},
		// Phrasing-inverted form emitted by some CLI versions (claude-code#8926):
		// "... limit reached · resets ..." carries no "hit your" prefix.
		{
			name: "claude session limit reached phrasing",
			err:  errors.New("Session limit reached · resets 9pm (UTC)"),
			want: true,
		},
		{
			name: "claude usage limit reached phrasing wrapped",
			err:  fmt.Errorf("agent execution: %w", &claude.TurnError{Message: "Usage limit reached · resets 11:00pm (UTC)"}),
			want: true,
		},
		{
			name: "org membership limit is not out of credits",
			err:  errors.New("reached your limit of 5 organization memberships"),
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsOutOfCredits(tt.err); got != tt.want {
				t.Fatalf("IsOutOfCredits() = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestClaudePlanLimitIsNotTransient guards the routing invariant: a plan-limit
// error must route to model fallback (IsOutOfCredits), never to a same-model
// transient retry — a retry can neither refill credits nor reset the window.
func TestClaudePlanLimitIsNotTransient(t *testing.T) {
	planLimitErrs := []error{
		errors.New("You've hit your session limit · resets 9:30am (UTC)"),
		errors.New("You've hit your limit · resets May 16, 5pm (UTC)"),
		errors.New("You've hit your usage limit · resets 1am (UTC)"),
		errors.New("Session limit reached · resets 9pm (UTC)"),
		fmt.Errorf("agent execution: %w", &claude.TurnError{Message: "You've hit your session limit · resets 9:30am (UTC)"}),
	}
	for _, err := range planLimitErrs {
		if IsTransient(err) {
			t.Fatalf("IsTransient(%q) = true, want false (plan limit must not same-model retry)", err)
		}
	}
}

func TestClassifyTransientReason(t *testing.T) {
	requireTransientReason(t,
		&codex.TransientError{Message: "turn never reached turn/completed", Reason: transientmeta.ReasonCodexIncomplete},
		true,
		transientmeta.ReasonCodexIncomplete,
	)
	requireTransientReason(t, &claude.TransientError{Message: "stream idle"}, true, transientmeta.ReasonStreamIdle)
	requireTransientReason(t, errors.New("503 Service Unavailable"), true, transientmeta.ReasonHTTP5xx)
	requireTransientReason(t, errors.New("API Error: The socket connection was closed unexpectedly"), true, transientmeta.ReasonConnectionReset)
	requireTransientReason(t, errors.New("syntax error"), false, transientmeta.ReasonUnknown)
}

func TestClassifyTransientDoesNotMatchEmbeddedHTTPStatusDigits(t *testing.T) {
	t.Parallel()

	_, ok := transientmeta.ClassifyText("processed 1500 records on port :5004")
	if ok {
		t.Fatal("ClassifyText matched embedded status-like digits")
	}
}

func requireTransientReason(t *testing.T, err error, wantOK bool, wantReason string) {
	t.Helper()
	gotOK, gotReason := ClassifyTransient(err)
	if gotOK != wantOK || gotReason != wantReason {
		t.Fatalf("ClassifyTransient() = (%v, %q), want (%v, %q)", gotOK, gotReason, wantOK, wantReason)
	}
}
