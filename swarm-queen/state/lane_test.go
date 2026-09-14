package state

import (
	"strings"
	"testing"
)

// Closure is decided from MEASUREMENTS. Every unmeasured probe leaves the lane
// not-closed, because "we could not look" is not evidence that nothing is there.
func TestClosedLaneRequiresEveryProbeToHaveRun(t *testing.T) {
	t.Parallel()
	done := &Lane{ID: "a", Status: StatusDone, Branch: "b-a"}

	// The fully-measured, fully-released case.
	if c := done.ClosedLane(true, false, true, false, true, 0, false); !c.Closed {
		t.Errorf("a measured, fully released lane is closed: %s", c.Why)
	}

	cases := []struct {
		name string
		got  Closure
	}{
		{"worktree unmeasured", done.ClosedLane(false, false, true, false, true, 0, false)},
		{"worktree still there", done.ClosedLane(true, true, true, false, true, 0, false)},
		{"branch unmeasured", done.ClosedLane(true, false, false, false, true, 0, false)},
		{"branch still there", done.ClosedLane(true, false, true, true, true, 0, false)},
		{"fleet unmeasured", done.ClosedLane(true, false, true, false, false, 0, false)},
		{"session still live", done.ClosedLane(true, false, true, false, true, 1, false)},
	}
	for _, tc := range cases {
		if tc.got.Closed {
			t.Errorf("%s: must not read as closed (%s)", tc.name, tc.got.Why)
		}
		if tc.got.Why == "" {
			t.Errorf("%s: a refusal must name its evidence", tc.name)
		}
	}

	// A non-terminal lane is never closed, however empty its resources look.
	running := &Lane{ID: "b", Status: StatusRunning}
	if c := running.ClosedLane(true, false, true, false, true, 0, false); c.Closed {
		t.Errorf("a running lane is not closed: %s", c.Why)
	}
}

// A retained backup is reported, never treated as a reason to call the lane
// unclosed: the uncommitted work it holds exists nowhere else, so making the
// audit pass by deleting it would be the check arguing for the data loss.
func TestClosedLaneReportsARetainedBackupWithoutRefusingClosure(t *testing.T) {
	t.Parallel()
	done := &Lane{ID: "a", Status: StatusDone, Branch: "b-a"}

	c := done.ClosedLane(true, false, true, false, true, 0, true)
	if !c.Closed {
		t.Fatalf("a retained snapshot must not make a released lane unclosed: %s", c.Why)
	}
	if !c.BackupRetained {
		t.Error("the retained snapshot must be reported")
	}
	if !strings.Contains(c.Why, "backup retained") {
		t.Errorf("the evidence must name the retained backup, got %q", c.Why)
	}
}
