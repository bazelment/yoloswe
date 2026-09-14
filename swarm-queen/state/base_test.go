package state

import "testing"

// A lane's own base overrides the run's. Without this a lane whose work builds
// on another branch was forked from the run-wide base and handed a tree that
// does not contain the code it was staffed to work on -- which looks like an
// ordinary `-f main` in the dry run and fails only once the agent is live.
func TestForkBasePrefersTheLanesOwnBase(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, laneBase, runBase, want string
	}{
		{"lane base wins", "feat/swarm-queen", "main", "feat/swarm-queen"},
		{"absent lane base falls back to the run's", "", "main", "main"},
		{"both absent stays empty", "", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			l := &Lane{Base: c.laneBase}
			if got := l.ForkBase(c.runBase); got != c.want {
				t.Errorf("ForkBase(%q) with Base=%q = %q, want %q",
					c.runBase, c.laneBase, got, c.want)
			}
		})
	}
}

// Base is absent-by-default: an existing ledger written before the field
// existed must round-trip unchanged rather than gaining an empty key.
func TestLaneBaseIsAbsentByDefault(t *testing.T) {
	t.Parallel()
	var l Lane
	if l.Base != "" {
		t.Errorf("zero Lane.Base = %q, want empty", l.Base)
	}
	if got := l.ForkBase("main"); got != "main" {
		t.Errorf("a lane with no Base must use the run's: got %q", got)
	}
}
