package orchestrator

import (
	"testing"

	"github.com/bazelment/yoloswe/symphony/agent"
)

func TestNewAgent_UnsupportedType(t *testing.T) {
	t.Parallel()
	cfg := agent.SessionConfig{Type: "unknown-backend"}
	_, err := newAgent(t.Context(), cfg, nil)
	if err == nil {
		t.Fatal("expected error for unsupported agent type")
	}
	want := `unsupported agent_session.type: "unknown-backend"`
	if err.Error() != want {
		t.Errorf("error = %q, want %q", err.Error(), want)
	}
}
