package orchestrator

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/bazelment/yoloswe/symphony/agent"
	"github.com/bazelment/yoloswe/symphony/agent/codex"
)

// newAgent creates an agent session of the configured type.
// Empty or "codex" type creates a Codex app-server session.
func newAgent(ctx context.Context, cfg agent.SessionConfig, logger *slog.Logger) (agent.Agent, error) {
	switch cfg.Type {
	case "", "codex":
		return codex.NewSession(ctx, cfg, logger)
	default:
		return nil, fmt.Errorf("unsupported agent type: %q", cfg.Type)
	}
}
