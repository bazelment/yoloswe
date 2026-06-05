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
