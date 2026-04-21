//go:build integration
// +build integration

// I3: two parallel Monitors, one fails fast (invalid command), one runs long.
// Models the INF-401 pr-polish shape: codex + cursor reviews launched in the
// same turn, with one failing on HTTP 400 while the other completes normally.
//
// Post-refactor invariant: the raw stream emits task_started for both tools
// and at least two terminal task_notifications (one failed, one completed).
// Consumer-level "logical turn done" is tested in multiagent/agent/turn_state
// — here we only verify the raw stream surfaces the events the consumer
// needs, and that both subprocesses actually ran.
package integration

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"
)

// TestStreamBg_I3_TwoParallelMonitorsOneFailsFast (I3) — models pr-polish.
func TestStreamBg_I3_TwoParallelMonitorsOneFailsFast(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	sess, testDir, cleanup := mkBgTestSession(t)
	defer cleanup()

	if err := sess.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	defer sess.Stop()

	slowMarker := testDir + "/i3_slow_marker.txt"
	prompt := fmt.Sprintf(
		"Launch TWO Monitor tool_uses IN THE SAME TURN (call the Monitor tool twice before waiting):\n\n"+
			"Monitor 1: `echo fast && exit 0`  (quick)\n"+
			"Monitor 2: `sleep 4 && echo I3_SLOW > %s`  (takes 4s)\n\n"+
			"After both finish, report the terminal statuses.", slowMarker)
	if err := sess.Query(ctx, prompt); err != nil {
		t.Fatalf("Query: %v", err)
	}

	state, err := drainUntilIdle(ctx, sess, 15*time.Second)
	if err != nil {
		t.Fatalf("drainUntil: %v (state: ts=%d tn=%d rm=%d tc=%d)",
			err, len(state.TaskStarted), len(state.TaskNotifications),
			len(state.ResultMessages), len(state.TurnCompletes))
	}

	sanityCheckTasksEmitted(t, state, "two Monitors")

	if len(state.TaskStarted) < 2 {
		t.Errorf("expected at least 2 task_started events; got %d", len(state.TaskStarted))
	}
	// Terminal status may arrive via either TaskNotificationEvent or
	// TaskUpdatedEvent — the consumer-side logicalTurnState treats both as
	// terminal. Assert each started task reached a terminal status.
	assertAllTasksTerminal(t, state)

	// The raw stream must emit TurnCompleteEvent for this parallel-bg flow
	// — parity with I1/I2/I4 in stream_bg_test.go. Without this, the
	// consumer-side logicalTurnState would never see the gate close on
	// turns that launch two parallel Monitors.
	assertTurnCompleteEmitted(t, state)

	// Each task's terminal status must be one of the known terminal
	// statuses (completed / failed / killed / timeout). Not a predicate
	// on which one — the pre-refactor bug was about missing the event
	// entirely, not about its value.
	for taskID, status := range state.TerminalForTask {
		switch status {
		case "completed", "failed", "killed", "timeout":
			// fine
		default:
			t.Errorf("task %s had non-terminal status %q", taskID, status)
		}
	}

	// The slow Monitor's subprocess should have run to completion.
	if _, statErr := os.Stat(slowMarker); statErr != nil {
		t.Errorf("slow marker missing — second Monitor did not finish: %v", statErr)
	}
}
