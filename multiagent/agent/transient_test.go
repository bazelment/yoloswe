package agent

import (
	"errors"
	"fmt"
	"testing"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
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
