package progress

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/multiagent/checkpoint"
)

func TestConsoleReporterRespectsOutputMode(t *testing.T) {
	t.Parallel()

	t.Run("minimal", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		reporter := NewConsoleReporter(WithOutput(&buf), WithMode(OutputMinimal))

		reporter.Event(NewPhaseChangeEvent(checkpoint.PhaseDesigning, checkpoint.PhaseBuilding, 2))
		reporter.Event(NewAgentStartEvent(agent.RoleBuilder, "task-1", "implement feature"))
		reporter.Event(NewAgentCompleteEvent(agent.RoleBuilder, "task-1", true, 0.25, 1500*time.Millisecond, nil))

		want := "\n[BUILD] Implementing changes (iteration 2)\n"
		if got := buf.String(); got != want {
			t.Fatalf("minimal output = %q, want %q", got, want)
		}
	})

	t.Run("normal", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		reporter := NewConsoleReporter(WithOutput(&buf), WithMode(OutputNormal))

		reporter.Event(NewAgentStartEvent(agent.RoleReviewer, "task-2", "review changes"))
		reporter.Event(NewAgentCompleteEvent(agent.RoleReviewer, "task-2", false, 0.125, 2500*time.Millisecond, errors.New("failed")))
		reporter.Event(NewToolStartEvent(agent.RoleReviewer, "Read", "tool-1"))

		want := "  [Reviewer] review changes...\n  [Reviewer] failed (2.5s, $0.1250)\n"
		if got := buf.String(); got != want {
			t.Fatalf("normal output = %q, want %q", got, want)
		}
	})

	t.Run("verbose", func(t *testing.T) {
		t.Parallel()

		var buf bytes.Buffer
		reporter := NewConsoleReporter(WithOutput(&buf), WithMode(OutputVerbose))

		reporter.Event(NewToolStartEvent(agent.RoleBuilder, "Write", "tool-2"))
		reporter.Event(NewToolCompleteEvent(agent.RoleBuilder, "Glob", "tool-2", map[string]interface{}{
			"pattern": "**/*.go",
		}))

		want := "    [Write] starting...\n    [Glob] **/*.go\n"
		if got := buf.String(); got != want {
			t.Fatalf("verbose output = %q, want %q", got, want)
		}
	})
}

func TestConsoleReporterEvents(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	reporter := NewConsoleReporter(WithOutput(&buf), WithMode(OutputVerbose))

	reporter.Event(NewAgentThinkingEvent(agent.RolePlanner, "planning"))
	reporter.Event(NewIterationEvent(2, 3, "review requested changes"))
	reporter.Event(NewCostUpdateEvent(1.25, 5, map[agent.AgentRole]float64{
		agent.RolePlanner: 1.25,
	}))
	reporter.Event(NewErrorEvent(errors.New("boom"), "planner"))

	got := buf.String()
	for _, want := range []string{
		"\n[Planner] planning...\n",
		"\n>>> Iteration 2/3: review requested changes\n",
		"Cost: $1.2500 / $5.00 (25.0%)\n",
		"  [ERROR] planner: boom\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("console output %q missing %q", got, want)
		}
	}
}

func TestAgentReporterForwardsOnlyProgressEvents(t *testing.T) {
	t.Parallel()

	recorder := &recordingReporter{}
	adapter := NewAgentReporter(recorder)

	event := NewIterationEvent(1, 2, "start")
	adapter.Event("not a progress event")
	adapter.Event(event)
	adapter.Close()

	if len(recorder.events) != 1 {
		t.Fatalf("forwarded events = %d, want 1", len(recorder.events))
	}
	if recorder.events[0].Type() != EventIteration {
		t.Fatalf("forwarded event type = %v, want %v", recorder.events[0].Type(), EventIteration)
	}
	if !recorder.closed {
		t.Fatal("Close() did not close the wrapped reporter")
	}
}

func TestEventConstructors(t *testing.T) {
	t.Parallel()

	phase := NewPhaseChangeEvent(checkpoint.PhaseDesigning, checkpoint.PhaseReviewing, 3)
	if phase.Type() != EventPhaseChange || phase.From != checkpoint.PhaseDesigning || phase.To != checkpoint.PhaseReviewing || phase.Iteration != 3 {
		t.Fatalf("phase event = %+v", phase)
	}
	if phase.Timestamp().IsZero() {
		t.Fatal("phase event timestamp is zero")
	}

	toolStart := NewToolStartEvent(agent.RoleBuilder, "Bash", "tool-1")
	if toolStart.Type() != EventToolStart || !toolStart.Started {
		t.Fatalf("tool start event = %+v", toolStart)
	}

	toolComplete := NewToolCompleteEvent(agent.RoleBuilder, "Bash", "tool-1", map[string]interface{}{
		"command": "bazel test //...",
	})
	if toolComplete.Type() != EventToolComplete || toolComplete.Started {
		t.Fatalf("tool complete event = %+v", toolComplete)
	}
}

type recordingReporter struct {
	events []Event
	closed bool
}

func (r *recordingReporter) Event(event Event) {
	r.events = append(r.events, event)
}

func (r *recordingReporter) Close() {
	r.closed = true
}
