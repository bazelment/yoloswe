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
	if e := h.Adopt(r); e != ErrLeaseHeld {
		t.Fatalf("%v", e)
	}
	if e := (Handover{Host: "other", PID: 2}).Release(r); e != ErrLeaseNotHeld {
		t.Fatalf("release by another holder: %v", e)
	}
}
