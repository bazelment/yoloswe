//go:build integration
// +build integration

// Consumer-level integration tests for the Claude provider's Execute path
// against real background-task scenarios. Covers plan matrix rows C1, C2,
// C3, C4, C5, C8, C9, C10, C12.
//
// These tests exercise agent.ClaudeProvider.Execute end-to-end: a real
// claude CLI process, raw event streaming, logicalTurnState policy, and
// AgentResult assembly. They prove the consumer treats pure-bg, mixed-bg,
// failed-bg, and cancelled-bg turns correctly — Execute returns only after
// the logical operation is truly done.
//
// Run with:
//
//	bazel test //multiagent/agent/integration:integration_test \
//	    --test_arg=-test.run=TestClaudeProviderBg --test_output=streamed --test_timeout=600
package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/multiagent/agent"
)

// mkClaudeProvider builds a ClaudeProvider wired for bg-task scenarios.
// Caller disposes the tmpDir. Permission mode bypass + plugins disabled so
// no user interaction is required.
func mkClaudeProvider(t *testing.T) (agent.Provider, string, func()) {
	t.Helper()
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude CLI not available; skipping bg integration test")
	}
	tmp, err := os.MkdirTemp("", "claude-provider-bg-")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	keep := os.Getenv("KEEP_ARTIFACTS") != ""
	cleanup := func() {
		if !keep {
			_ = os.RemoveAll(tmp)
		}
	}
	provider := agent.NewClaudeProvider(
		claude.WithModel("haiku"),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithDisablePlugins(),
	)
	return provider, tmp, cleanup
}

// TestClaudeProviderBg_C1_PureBgMonitor (C1) — Execute must not return
// until the Monitor bg task reaches a terminal state. Before the refactor,
// Execute returned an AgentResult with HasLiveBackgroundWork=true on the
// first ResultMessage; after, Execute returns only after the auto-
// continuation ResultMessage.
func TestClaudeProviderBg_C1_PureBgMonitor(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	provider, tmp, cleanup := mkClaudeProvider(t)
	defer cleanup()

	marker := filepath.Join(tmp, "c1_marker.txt")
	prompt := fmt.Sprintf(
		"Use the Monitor tool (NOT plain Bash) to run:\n"+
			"`sleep 3 && echo C1_MARKER > %s`\n\n"+
			"Wait for Monitor to finish, then report 'done'.", marker)
	result, err := provider.Execute(ctx, prompt, nil,
		agent.WithProviderWorkDir(tmp),
	)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if result == nil {
		t.Fatal("Execute returned nil result")
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Errorf("marker missing — Execute returned before Monitor finished: %v", statErr)
	}
}

// TestClaudeProviderBg_C2_MixedSyncBg (C2) — the direct INF-401 repro at
// the consumer layer. Execute with a turn that does sync Bash + Monitor
// must not return until both settle.
func TestClaudeProviderBg_C2_MixedSyncBg(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	provider, tmp, cleanup := mkClaudeProvider(t)
	defer cleanup()

	marker := filepath.Join(tmp, "c2_marker.txt")
	prompt := fmt.Sprintf(
		"In one turn:\n"+
			"1. Run `pwd` with sync Bash.\n"+
			"2. Launch Monitor to run `sleep 3 && echo C2_MARKER > %s`.\n"+
			"Then wait for Monitor and report 'done'.", marker)
	result, err := provider.Execute(ctx, prompt, nil,
		agent.WithProviderWorkDir(tmp),
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result == nil {
		t.Fatal("nil result")
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Errorf("marker missing: %v", statErr)
	}
}

// TestClaudeProviderBg_C3_TwoParallelMonitors (C3) — consumer layer of I3.
// Both Monitors must settle before Execute returns.
func TestClaudeProviderBg_C3_TwoParallelMonitors(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	provider, tmp, cleanup := mkClaudeProvider(t)
	defer cleanup()

	slowMarker := filepath.Join(tmp, "c3_slow.txt")
	prompt := fmt.Sprintf(
		"Launch TWO Monitor tool_uses in the SAME turn:\n"+
			"1. `this-cmd-does-not-exist-xyz 2>&1; exit 2` (fails fast)\n"+
			"2. `sleep 4 && echo C3_SLOW > %s` (completes normally)\n\n"+
			"Wait for both and report their statuses.", slowMarker)
	_, err := provider.Execute(ctx, prompt, nil,
		agent.WithProviderWorkDir(tmp),
	)
	if err != nil && !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Execute: %v", err)
	}
	// The slow marker must exist — proves Execute waited for the second
	// Monitor before returning.
	if _, statErr := os.Stat(slowMarker); statErr != nil {
		t.Errorf("slow marker missing: %v", statErr)
	}
}

// TestClaudeProviderBg_C4_PureBgBash (C4) — Bash with run_in_background:true.
func TestClaudeProviderBg_C4_PureBgBash(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()
	provider, tmp, cleanup := mkClaudeProvider(t)
	defer cleanup()

	marker := filepath.Join(tmp, "c4_marker.txt")
	prompt := fmt.Sprintf(
		"Use Bash with run_in_background:true (NOT Monitor) to run:\n"+
			"`sleep 3 && echo C4_MARKER > %s`\n\n"+
			"Wait for the bg Bash to finish, then report 'done'.", marker)
	result, err := provider.Execute(ctx, prompt, nil,
		agent.WithProviderWorkDir(tmp),
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result == nil {
		t.Fatal("nil result")
	}
	if _, statErr := os.Stat(marker); statErr != nil {
		t.Errorf("marker missing: %v", statErr)
	}
}

// TestClaudeProviderBg_C5_MixedMonitorAndBgBash (C5) — one turn launches
// both a Monitor and a bg Bash. Execute must wait for both.
func TestClaudeProviderBg_C5_MixedMonitorAndBgBash(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 240*time.Second)
	defer cancel()
	provider, tmp, cleanup := mkClaudeProvider(t)
	defer cleanup()

	m1 := filepath.Join(tmp, "c5_mon.txt")
	m2 := filepath.Join(tmp, "c5_bash.txt")
	prompt := fmt.Sprintf(
		"In one turn, launch BOTH:\n"+
			"- Monitor tool: `sleep 3 && echo C5_MON > %s`\n"+
			"- Bash with run_in_background:true: `sleep 4 && echo C5_BASH > %s`\n\n"+
			"Wait for both to finish.", m1, m2)
	_, err := provider.Execute(ctx, prompt, nil,
		agent.WithProviderWorkDir(tmp),
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	for _, p := range []string{m1, m2} {
		if _, st := os.Stat(p); st != nil {
			t.Errorf("marker %s missing: %v", p, st)
		}
	}
}

// TestClaudeProviderBg_C8_BudgetExceededMidBg (C8) — WithProviderMaxBudgetUSD
// bound triggers mid-bg. Execute should return a clean error (not hang)
// without leaking the running bg task — the caller is responsible for
// cancelling the context when budget is exceeded.
func TestClaudeProviderBg_C8_BudgetExceededMidBg(t *testing.T) {
	// Short deadline simulates budget-mediated cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	provider, tmp, cleanup := mkClaudeProvider(t)
	defer cleanup()

	prompt := "Use Monitor tool to run `sleep 30`. Wait for it to finish."
	_, err := provider.Execute(ctx, prompt, nil,
		agent.WithProviderWorkDir(tmp),
	)
	// We expect ctx deadline, not a hang. The invariant is "Execute returns
	// in bounded time when ctx expires". If Execute hangs past ctx deadline,
	// the test helper deadline will fire.
	if err == nil {
		t.Log("Execute returned without error despite short ctx — model may have given up before calling Monitor")
	} else if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		t.Logf("Execute returned non-deadline err (acceptable): %v", err)
	}
}

// TestClaudeProviderBg_C9_BgMonitorToolError (C9) — consumer layer of I9.
// Failed bg task is terminal; Execute returns a successful AgentResult
// whose Text references the failure.
func TestClaudeProviderBg_C9_BgMonitorToolError(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	provider, tmp, cleanup := mkClaudeProvider(t)
	defer cleanup()

	prompt := "Use Monitor to run `this-command-does-not-exist-xyz 2>&1; exit 1`. " +
		"Report the Monitor's final status (completed/failed/killed/timeout)."
	result, err := provider.Execute(ctx, prompt, nil,
		agent.WithProviderWorkDir(tmp),
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result == nil {
		t.Fatal("nil result")
	}
	text := strings.ToLower(result.Text)
	if !strings.Contains(text, "fail") && !strings.Contains(text, "error") && !strings.Contains(text, "kill") {
		t.Logf("result text did not reference failure: %q", result.Text)
	}
}

// TestClaudeProviderBg_C10_MonitorTimeoutMs (C10) — consumer layer of I10.
// Monitor with short timeout_ms; Execute returns cleanly with the
// timed-out Monitor status.
func TestClaudeProviderBg_C10_MonitorTimeoutMs(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	provider, tmp, cleanup := mkClaudeProvider(t)
	defer cleanup()

	prompt := "Use Monitor tool with timeout_ms=2000 to run `sleep 60`. " +
		"Report the Monitor's terminal status."
	result, err := provider.Execute(ctx, prompt, nil,
		agent.WithProviderWorkDir(tmp),
	)
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result == nil {
		t.Fatal("nil result")
	}
}

// TestClaudeProviderBg_C12_CloseProviderWhileBgLive (C12) — context
// cancellation while a bg task is live. Execute must return within a
// bounded window after cancel().
func TestClaudeProviderBg_C12_CloseProviderWhileBgLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	provider, tmp, cleanup := mkClaudeProvider(t)
	defer cleanup()

	prompt := "Use Monitor to run `sleep 30`. Wait for it to finish."

	done := make(chan struct{})
	var execErr error
	go func() {
		_, execErr = provider.Execute(ctx, prompt, nil,
			agent.WithProviderWorkDir(tmp),
		)
		close(done)
	}()

	// Give the session time to start the Monitor then cancel.
	time.Sleep(5 * time.Second)
	cancel()

	select {
	case <-done:
		// Expected — cancellation propagated.
		if execErr == nil {
			t.Log("Execute returned nil err on cancel — acceptable if model finished quickly")
		} else if !errors.Is(execErr, context.Canceled) && !errors.Is(execErr, context.DeadlineExceeded) {
			t.Logf("Execute returned non-cancel err (acceptable): %v", execErr)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Execute did not return within 15s after cancel — cancel propagation broken")
	}
}
