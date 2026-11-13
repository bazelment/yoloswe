package adopt

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImporterFixture(t *testing.T) {
	d := t.TempDir()
	os.WriteFile(filepath.Join(d, "state.json"), []byte(`{"config":{"goal":"ship it","phases":[{"name":"build"}]},"tasks":[{"id":"lane-a","title":"A","branch":"b","phase":"build","status":"running","round":2,"sessions":{"build":"sess"},"notes":["n"]}]}`), 0644)
	os.WriteFile(filepath.Join(d, "OBJECTIVE.md"), []byte("objective"), 0644)
	r, e := (Importer{}).ReadOnly(d)
	if e != nil {
		t.Fatal(e)
	}
	if r.Goal != "ship it" || len(r.Lanes) != 1 || r.Lanes[0].Session != "sess" {
		t.Fatalf("%+v", r)
	}
}
func TestHandoverRelease(t *testing.T) {
	d := t.TempDir()
	for _, n := range []string{"OBJECTIVE.md", "PROGRESS.md"} {
		os.WriteFile(filepath.Join(d, n), []byte("body\n"), 0644)
	}
	h := Handover{Host: "h", PID: 1}
	r := Run{ID: "x", Dir: d}
	if e := h.Adopt(r); e != nil {
		t.Fatal(e)
	}
	b, _ := os.ReadFile(filepath.Join(d, "OBJECTIVE.md"))
	if !strings.HasPrefix(string(b), "ORCHESTRATOR:") {
		t.Fatal(string(b))
	}
	if e := h.Release(r); e != nil {
		t.Fatal(e)
	}
	b, _ = os.ReadFile(filepath.Join(d, "OBJECTIVE.md"))
	if string(b) != "body\n" {
		t.Fatal(string(b))
	}
}
func TestDoubleAdopt(t *testing.T) {
	d := t.TempDir()
	r := Run{ID: "x", Dir: d}
	h := Handover{}
	if e := h.Adopt(r); e != nil {
		t.Fatal(e)
	}
	if e := h.Adopt(r); e != nil {
		t.Fatalf("same holder should reattach: %v", e)
	}
	if e := (Handover{Host: "other", PID: 2}).Release(r); e != ErrLeaseNotHeld {
		t.Fatalf("release by another holder: %v", e)
	}
}

func TestImporterPreservesSpawnOwnership(t *testing.T) {
	d := t.TempDir()
	state := `{"config":{"goal":"g","phases":[{"name":"build"}]},"tasks":[{"id":"a","title":"A","phase":"build","status":"running","round":1,"sessions":{}},{"id":"b","title":"B","phase":"build","status":"running","round":1,"sessions":{}}]}`
	if err := os.WriteFile(filepath.Join(d, "state.json"), []byte(state), 0644); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b"} {
		p := filepath.Join(d, id+".build.spawn.json")
		body := `{"session_id":"sess-` + id + `","terminal_id":"term-` + id + `","pr":` + map[string]string{"a": "123", "b": "456"}[id] + `}`
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	r, err := (Importer{}).ReadOnly(d)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range r.Lanes {
		want := l.ID
		if l.Session != "sess-"+want || l.Terminal != "term-"+want || l.PR == "" {
			t.Fatalf("lane %+v", l)
		}
	}
}

type failingAdapters struct {
	labelErr, stopErr, startErr error
	labels                      []string
	stopped, started            int
}

func (f *failingAdapters) SetOrchestrator(id, value string) error {
	f.labels = append(f.labels, id+"="+value)
	return f.labelErr
}
func (f *failingAdapters) StopWatcher(string) error  { f.stopped++; return f.stopErr }
func (f *failingAdapters) StartWatcher(string) error { f.started++; return f.startErr }

func TestHandoverFailureRestoresExactState(t *testing.T) {
	//nolint:govet // failure matrix readability
	for _, tc := range []struct {
		name                        string
		adopt                       bool
		labelErr, stopErr, startErr error
	}{
		{"adopt-label", true, os.ErrPermission, nil, nil},
		{"adopt-stop", true, nil, os.ErrPermission, nil},
		{"release-label", false, os.ErrPermission, nil, nil},
		{"release-start", false, nil, nil, os.ErrPermission},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := t.TempDir()
			for _, n := range []string{"OBJECTIVE.md", "PROGRESS.md"} {
				if err := os.WriteFile(filepath.Join(d, n), []byte("body\n"), 0644); err != nil {
					t.Fatal(err)
				}
			}
			ad := &failingAdapters{stopErr: tc.stopErr}
			h := Handover{Host: "h", PID: 1, Labeler: ad, Process: ad}
			r := Run{ID: "r", Dir: d}
			if !tc.adopt {
				if err := h.Adopt(r); err != nil {
					t.Fatal(err)
				}
			}
			ad.labelErr = tc.labelErr
			ad.startErr = tc.startErr
			before := map[string][]byte{}
			for _, n := range []string{"OBJECTIVE.md", "PROGRESS.md", ".orca-lease.json"} {
				before[n], _ = os.ReadFile(filepath.Join(d, n))
			}
			var err error
			if tc.adopt {
				err = h.Adopt(r)
			} else {
				err = h.Release(r)
			}
			if err == nil {
				t.Fatal("expected failure")
			}
			for n, want := range before {
				got, _ := os.ReadFile(filepath.Join(d, n))
				if string(got) != string(want) {
					t.Errorf("%s changed: %q != %q", n, got, want)
				}
			}
		})
	}
}

func TestAdoptReattachesOwnLeaseAndRejectsOther(t *testing.T) {
	d := t.TempDir()
	r := Run{ID: "r", Dir: d}
	h := Handover{Host: "h", PID: 1}
	if err := h.Adopt(r); err != nil {
		t.Fatal(err)
	}
	if err := h.Adopt(r); err != nil {
		t.Fatalf("same holder should reattach: %v", err)
	}
	if err := (Handover{Host: "other", PID: 2}).Adopt(r); err != ErrLeaseHeld {
		t.Fatalf("other adopt=%v", err)
	}
}

func TestReleaseOwnedAfterChatRestart(t *testing.T) {
	d := t.TempDir()
	r := Run{ID: "swarm-y", Dir: d}
	h := Handover{HolderID: "chat-x", Labeler: &failingAdapters{}, Process: &failingAdapters{}}
	if err := h.Adopt(r); err != nil {
		t.Fatal(err)
	}
	s := Service{Importer: Importer{Roots: []string{filepath.Dir(filepath.Dir(d))}}, Handover: h}
	// Direct directory release exercises the durable holder lookup contract.
	if err := s.ReleaseOwned([]Run{r}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d, ".orca-lease.json")); !os.IsNotExist(err) {
		t.Fatalf("lease remains: %v", err)
	}
}

func TestReleaseTargetsAdoptedSwarmAndLeavesChatRun(t *testing.T) {
	chatDir, swarmDir := t.TempDir(), t.TempDir()
	for _, d := range []string{chatDir, swarmDir} {
		if err := os.WriteFile(filepath.Join(d, "OBJECTIVE.md"), []byte(d+" body\n"), 0644); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "PROGRESS.md"), []byte("progress\n"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	h := Handover{HolderID: "chat-x"}
	swarm := Run{ID: "y", Dir: swarmDir}
	if err := h.Adopt(swarm); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadFile(filepath.Join(chatDir, "OBJECTIVE.md"))
	if err := h.Release(swarm); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(filepath.Join(chatDir, "OBJECTIVE.md"))
	if string(after) != string(before) {
		t.Fatalf("chat run changed: %q", after)
	}
}

func TestHandoverMarkerFailureIsAtomic(t *testing.T) {
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "OBJECTIVE.md"), []byte("body\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(d, "PROGRESS.md"), 0755); err != nil {
		t.Fatal(err)
	}
	h := Handover{Host: "h", PID: 1}
	if err := h.Adopt(Run{ID: "r", Dir: d}); err == nil {
		t.Fatal("expected marker failure")
	}
	b, _ := os.ReadFile(filepath.Join(d, "OBJECTIVE.md"))
	if string(b) != "body\n" {
		t.Fatalf("objective changed: %q", b)
	}
	if _, err := os.Stat(filepath.Join(d, ".orca-lease.json")); !os.IsNotExist(err) {
		t.Fatalf("lease remains: %v", err)
	}
}

func TestLeaseFailureLeavesStateUntouched(t *testing.T) {
	d := t.TempDir()
	for _, n := range []string{"OBJECTIVE.md", "PROGRESS.md"} {
		_ = os.WriteFile(filepath.Join(d, n), []byte("body\n"), 0644)
	}
	leasePath := filepath.Join(d, ".orca-lease.json")
	original := []byte(`{"holder":"other"}\n`)
	if err := os.WriteFile(leasePath, original, 0600); err != nil {
		t.Fatal(err)
	}
	h := Handover{HolderID: "chat-x"}
	if err := h.Adopt(Run{ID: "r", Dir: d}); err != ErrLeaseHeld {
		t.Fatalf("adopt lease failure=%v", err)
	}
	got, _ := os.ReadFile(leasePath)
	if string(got) != string(original) {
		t.Fatalf("lease changed: %q", got)
	}
}

func TestReleaseMarkerFailureRestoresLeaseAndFiles(t *testing.T) {
	d := t.TempDir()
	for _, n := range []string{"OBJECTIVE.md", "PROGRESS.md"} {
		_ = os.WriteFile(filepath.Join(d, n), []byte("body\n"), 0644)
	}
	h := Handover{HolderID: "chat-x"}
	r := Run{ID: "r", Dir: d}
	if err := h.Adopt(r); err != nil {
		t.Fatal(err)
	}
	_ = os.Remove(filepath.Join(d, "PROGRESS.md"))
	if err := os.Mkdir(filepath.Join(d, "PROGRESS.md"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := h.Release(r); err == nil {
		t.Fatal("expected marker failure")
	}
	if _, err := os.Stat(filepath.Join(d, ".orca-lease.json")); err != nil {
		t.Fatalf("lease lost: %v", err)
	}
	b, _ := os.ReadFile(filepath.Join(d, "OBJECTIVE.md"))
	if !strings.HasPrefix(string(b), "ORCHESTRATOR:") {
		t.Fatalf("marker not restored: %q", b)
	}
}
