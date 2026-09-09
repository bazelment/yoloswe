package state

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// realLedger is a lane dict exactly as ledger.py writes it: the fixed key set
// from its `add` literal, with none of swarm-queen's extended fields.
const realLedger = `{
  "config": {
    "goal": "Make Forge app deploys measurably more reliable and faster",
    "phases": [
      {"name": "swe", "model": "sonnet"},
      {"name": "clean", "model": "gpt-5.6-terra"},
      {"name": "local-review", "model": "opus"}
    ],
    "base": "main",
    "target": "swarm/deploy-harden"
  },
  "tasks": [
    {
      "id": "gaps",
      "title": "Measure telemetry across envs",
      "branch": "swarm/deploy-harden-gaps-0908",
      "brief": "",
      "depends_on": [],
      "priority": "p1",
      "status": "done",
      "phase": "swe",
      "sessions": {"swe": "deploy-harden-gaps-0908-builder-45615e61"},
      "worktree": "/home/ubuntu/worktrees/kernel/swarm/deploy-harden-gaps-0908",
      "window_id": "@1322",
      "merge_sha": "",
      "notes": ["Telemetry gap analysis complete"]
    }
  ]
}`

// probeDoc inspects raw on-disk JSON without going through the typed model.
type probeDoc struct {
	Tasks []map[string]json.RawMessage `json:"tasks"`
}

func seed(t *testing.T, body string) *Store {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, StateName), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return NewStore(dir)
}

func TestReadParsesRealLedger(t *testing.T) {
	t.Parallel()
	st, err := seed(t, realLedger).Read()
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got, want := st.Config.Target, "swarm/deploy-harden"; got != want {
		t.Errorf("target = %q, want %q", got, want)
	}
	if len(st.Lanes) != 1 {
		t.Fatalf("got %d lanes, want 1", len(st.Lanes))
	}
	l := st.Lanes[0]
	if l.ID != "gaps" || l.Status != StatusDone || l.WindowID != "@1322" {
		t.Errorf("unexpected lane: %+v", l)
	}
	if l.Sessions["swe"] != "deploy-harden-gaps-0908-builder-45615e61" {
		t.Errorf("session not parsed: %v", l.Sessions)
	}
	// Extended fields are absent-by-default; they must read as zero, and code
	// must never treat their absence as a signal.
	if l.PR != 0 || l.ApprovalSHA != "" {
		t.Errorf("expected extended fields unset, got pr=%d approval=%q", l.PR, l.ApprovalSHA)
	}
}

// A missing field is not evidence of approval. Absence must read as stale.
func TestApprovalStaleTreatsUnknownAsStale(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		head     string
		approval string
		wantStal bool
	}{
		{"both empty", "", "", true},
		{"head known, no approval", "abc", "", true},
		{"approval known, no head", "", "abc", true},
		{"approval behind head", "def", "abc", true},
		{"approval at head", "abc", "abc", false},
	}
	for _, tc := range cases {
		l := Lane{PRHead: tc.head, ApprovalSHA: tc.approval}
		if got := l.ApprovalStale(); got != tc.wantStal {
			t.Errorf("%s: ApprovalStale() = %v, want %v", tc.name, got, tc.wantStal)
		}
	}
}

// ledger.py rewrites the whole file on every subcommand, so any key swarm-queen
// does not model must survive a round-trip or the other tool's data is deleted.
func TestUpdatePreservesUnknownKeys(t *testing.T) {
	t.Parallel()
	body := `{"config":{"goal":"g","phases":[{"name":"swe","model":"opus"}],"base":"main","target":"t"},
	  "tasks":[{"id":"a","title":"A","branch":"b","brief":"","depends_on":[],"priority":"p1",
	            "status":"planned","phase":"","sessions":{},"worktree":"","window_id":"",
	            "merge_sha":"","notes":[],"future_field_from_ledger_py":{"nested":42}}]}`
	s := seed(t, body)

	if err := s.Update(func(st *State) error {
		st.Lanes[0].PR = 11968
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(s.Dir, StateName))
	if err != nil {
		t.Fatal(err)
	}
	var probe probeDoc
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	if _, ok := probe.Tasks[0]["future_field_from_ledger_py"]; !ok {
		t.Errorf("unknown key was dropped on write-back; file:\n%s", raw)
	}
	if _, ok := probe.Tasks[0]["pr"]; !ok {
		t.Error("our own new field was not written")
	}
}

// The lost-update race: concurrent read-modify-writes must all survive. Without
// a lock spanning read and write, whoever writes second silently discards the
// other's changes -- exactly what ledger.py does today.
func TestUpdateIsSerialisedUnderConcurrency(t *testing.T) {
	t.Parallel()
	s := seed(t, realLedger)

	const writers = 8
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := range writers {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			errs <- s.Update(func(st *State) error {
				st.Lanes[0].Notes = append(st.Lanes[0].Notes, "note")
				st.Lanes[0].Round++
				return nil
			})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent Update: %v", err)
		}
	}

	st, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got := st.Lanes[0].Round; got != writers {
		t.Errorf("Round = %d, want %d — updates were lost", got, writers)
	}
}

// A tick that cannot acquire the lock must abort and report, never proceed on a
// possibly-stale read. This is the .done-is-a-claim discipline applied to our
// own state, and it is the one place this design can silently regress into the
// failure it exists to prevent.
func TestUpdateAbortsWhenLockHeld(t *testing.T) {
	t.Parallel()
	s := seed(t, realLedger)
	s.LockTimeout = 150 * time.Millisecond

	held, err := acquire(filepath.Join(s.Dir, LockName), true, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer held.release()

	called := false
	err = s.Update(func(*State) error { called = true; return nil })
	if !errors.Is(err, ErrLockTimeout) {
		t.Fatalf("err = %v, want ErrLockTimeout", err)
	}
	if called {
		t.Error("mutation ran despite failing to acquire the lock")
	}
}

// Readers take a shared lock, so concurrent reads must not block each other --
// watch_lanes.sh polls the ledger on every interval and would otherwise stall
// the reconcile writer.
func TestConcurrentReadsDoNotBlock(t *testing.T) {
	t.Parallel()
	s := seed(t, realLedger)

	first, err := acquire(filepath.Join(s.Dir, LockName), false, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer first.release()

	s.LockTimeout = 250 * time.Millisecond
	if _, err := s.Read(); err != nil {
		t.Fatalf("second shared reader blocked: %v", err)
	}
}

// A lane id containing a dot silently misroutes every signal file that lane
// emits, because watch_lanes.sh splits <lane>.<phase>.<sig> on the first dot.
func TestValidateRejectsDottedLaneID(t *testing.T) {
	t.Parallel()
	l := &Lane{ID: "deploy.gaps", Status: StatusPlanned}
	if err := l.Validate(); err == nil {
		t.Fatal("expected dotted lane id to be rejected")
	}
}

func TestReadyOrdersByPriorityAndRespectsDeps(t *testing.T) {
	t.Parallel()
	st := &State{Lanes: []*Lane{
		{ID: "c", Status: StatusPlanned, Priority: P2},
		{ID: "a", Status: StatusPlanned, Priority: P0},
		{ID: "b", Status: StatusPlanned, Priority: P1},
		{ID: "blocked", Status: StatusPlanned, Priority: P0, DependsOn: []string{"never"}},
		{ID: "never", Status: StatusRunning},
	}}
	got := st.Ready()
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("Ready() returned %d lanes, want %d", len(got), len(want))
	}
	for i, l := range got {
		if l.ID != want[i] {
			t.Errorf("Ready()[%d] = %q, want %q", i, l.ID, want[i])
		}
	}
}

// The config object needs the same unknown-key protection as lanes: the skill's
// contract work may add run-level keys, and a swarm-queen write-back must not
// delete them.
func TestUpdatePreservesUnknownConfigKeys(t *testing.T) {
	t.Parallel()
	body := `{"config":{"goal":"g","phases":[{"name":"swe","model":"opus"}],"base":"main",
	           "target":"t","terminal_proof":"edge 200 on a real preview"},
	          "tasks":[]}`
	s := seed(t, body)

	if err := s.Update(func(st *State) error {
		st.Config.Goal = "changed"
		return nil
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}

	raw, err := os.ReadFile(filepath.Join(s.Dir, StateName))
	if err != nil {
		t.Fatal(err)
	}
	var probe struct {
		Config map[string]json.RawMessage `json:"config"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		t.Fatal(err)
	}
	if _, ok := probe.Config["terminal_proof"]; !ok {
		t.Errorf("unknown config key was dropped; file:\n%s", raw)
	}
	if string(probe.Config["goal"]) != `"changed"` {
		t.Errorf("goal = %s, want \"changed\"", probe.Config["goal"])
	}
}
