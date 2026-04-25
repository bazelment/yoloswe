//go:build integration
// +build integration

package integration

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

func TestSession_SpecialCommandsRealClaude(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	testDir, err := os.MkdirTemp("", "claude-special-commands-")
	if err != nil {
		t.Fatalf("failed to create temp dir: %v", err)
	}
	defer os.RemoveAll(testDir)

	session := claude.NewSession(
		claude.WithModel("haiku"),
		claude.WithWorkDir(testDir),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithDisablePlugins(),
	)

	if err := session.Start(ctx); err != nil {
		t.Fatalf("failed to start real Claude session: %v", err)
	}
	defer session.Stop()

	if _, err := session.SendMessage(ctx, "Reply with OK only."); err != nil {
		t.Fatalf("failed to send seed prompt: %v", err)
	}
	events, err := CollectTurnEvents(ctx, session)
	if err != nil {
		t.Fatalf("failed to collect seed turn: %v", err)
	}
	if events.TurnComplete == nil || !events.TurnComplete.Success {
		t.Fatalf("seed turn failed: %+v", events.TurnComplete)
	}
	t.Logf("seed turn ok: cost=$%.6f text_events=%d", events.TurnComplete.Usage.CostUSD, len(events.TextEvents))

	contextUsage, err := session.ContextUsage(ctx)
	if err != nil {
		t.Fatalf("ContextUsage failed: %v", err)
	}
	if contextUsage.TotalTokens <= 0 {
		t.Fatalf("ContextUsage total tokens = %d, want > 0", contextUsage.TotalTokens)
	}
	if contextUsage.EffectiveMaxTokens() <= 0 {
		t.Fatalf("ContextUsage max tokens = %d, want > 0", contextUsage.EffectiveMaxTokens())
	}
	if len(contextUsage.Categories) == 0 {
		t.Fatal("ContextUsage categories empty")
	}
	t.Logf("context usage ok: total=%d max=%d categories=%d",
		contextUsage.TotalTokens, contextUsage.EffectiveMaxTokens(), len(contextUsage.Categories))

	t.Run("effort", func(t *testing.T) {
		effortSession := claude.NewSession(
			claude.WithModel("haiku"),
			claude.WithWorkDir(testDir),
			claude.WithPermissionMode(claude.PermissionModeBypass),
			claude.WithDisablePlugins(),
			claude.WithEnv(map[string]string{
				// Haiku is cheap but normally reports no effort support. Use a
				// separate no-turn session so the control path is tested without
				// sending an unsupported effort parameter to the API.
				"CLAUDE_CODE_ALWAYS_ENABLE_EFFORT": "1",
			}),
		)
		if err := effortSession.Start(ctx); err != nil {
			t.Fatalf("failed to start effort session: %v", err)
		}
		defer effortSession.Stop()

		beforeEffort, err := effortSession.GetEffort(ctx)
		if err != nil {
			t.Fatalf("GetEffort before set failed: %v", err)
		}
		afterSet, err := effortSession.SetEffort(ctx, claude.EffortLow)
		if err != nil {
			t.Fatalf("SetEffort(low) failed: %v", err)
		}
		if afterSet.Effort != claude.EffortLow {
			t.Fatalf("SetEffort(low) applied effort = %q, want %q", afterSet.Effort, claude.EffortLow)
		}
		afterClear, err := effortSession.ClearEffort(ctx)
		if err != nil {
			t.Fatalf("ClearEffort failed: %v", err)
		}
		if afterClear.Effort == claude.EffortLow {
			t.Fatalf("ClearEffort left effort at %q", afterClear.Effort)
		}
		t.Logf("effort ok: before=%s/%s after_set=%s/%s after_clear=%s/%s",
			beforeEffort.Model, beforeEffort.Effort,
			afterSet.Model, afterSet.Effort,
			afterClear.Model, afterClear.Effort)
	})

	beforeEffort, err := session.GetEffort(ctx)
	if err != nil {
		t.Logf("seed session effort status unavailable: %v", err)
	} else {
		t.Logf("seed session effort status: model=%s effort=%s", beforeEffort.Model, beforeEffort.Effort)
	}

	t.Run("usage", func(t *testing.T) {
		usage, err := session.Usage(ctx)
		if errors.Is(err, claude.ErrUsageUnavailable) {
			t.Skipf("usage unavailable with current credentials: %v", err)
		}
		if err != nil {
			t.Fatalf("Usage failed: %v", err)
		}
		t.Logf("usage ok: %s", summarizePlanUsage(usage))
	})
}

func summarizePlanUsage(usage *claude.PlanUsage) string {
	if usage == nil {
		return "usage unavailable"
	}
	return strings.ReplaceAll(usage.Report(), "\n", " | ")
}
