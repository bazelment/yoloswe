package toolbox

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	raw := `{"config":{"goal":"synthetic run-5 shapes","phases":[{"name":"swe"}]},"tasks":[{"id":"unfunded","status":"planned","depends_on":["working"]},{"id":"working","status":"running","phase":"swe","session_id":"s1","round":1,"terminals":{"swe":{"1":"t1"}},"terminal_state":"working"},{"id":"funded","status":"planned","phase":"swe","brief":"Implement the funded task"},{"id":"done","status":"done","merge_sha":"abc"},{"id":"answered","status":"running","waiting_on":"host","terminal_state":"waiting","session_id":"s2","nebula_terminal":"t2"}]}`
	if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "EVENTS"), []byte("2026-11-18T00:00:00Z\tanswered\twait-resolved\thost already answered\n"), 0600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestStartFunding(t *testing.T) {
	dir := fixture(t)
	var out bytes.Buffer
	err := Run(t.Context(), []string{"lane", "start", "unfunded", "--dir", dir}, nil, &out)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "brief") {
		t.Fatalf("unfunded: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "state.json"), bytes.Replace(raw, []byte(`"id":"unfunded"`), []byte(`"id":"unfunded","brief":"blocked mission"`), 1), 0600); err != nil {
		t.Fatal(err)
	}
	err = Run(t.Context(), []string{"lane", "start", "unfunded", "--dir", dir}, nil, &out)
	if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), "unfinished") {
		t.Fatalf("dependency: %v", err)
	}
	// A funded start must use the real skill helper, never manufacture a running row.
	script := filepath.Join(t.TempDir(), "ops.sh")
	if err := os.WriteFile(script, []byte("sw_spawn() { printf 'helper started %s\\n' \"$2\"; }\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Run(context.Background(), []string{"lane", "start", "funded", "--dir", dir, "--script", script}, nil, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "helper started funded") {
		t.Fatalf("skill helper was not invoked: %s", &out)
	}
}

func TestSteerHostAuthority(t *testing.T) {
	t.Parallel()
	dir := fixture(t)
	for _, id := range []string{"done", "answered"} {
		err := Run(t.Context(), []string{"lane", "steer", id, "continue", "--dir", dir}, nil, &bytes.Buffer{})
		if !errors.Is(err, ErrRefused) {
			t.Fatalf("%s: %v", id, err)
		}
	}
}

func TestShadowNoLiveWrites(t *testing.T) {
	t.Parallel()
	dir := fixture(t)
	intent := t.TempDir()
	before, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	events, err := os.ReadFile(filepath.Join(dir, "EVENTS"))
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := Run(t.Context(), []string{"lane", "start", "funded", "--dir", dir, "--shadow", "--intent-dir", intent}, nil, &out); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	latest, err := os.ReadFile(filepath.Join(dir, "EVENTS"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) || !bytes.Equal(events, latest) {
		t.Fatal("shadow changed live run")
	}
	if out.String() != "shadow: recorded\n" {
		t.Fatalf("output: %q", out.String())
	}
	actions, err := os.ReadFile(filepath.Join(intent, "actions.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(actions), "lane.start") {
		t.Fatalf("intent: %s", actions)
	}
}

func TestWaitEvidence(t *testing.T) {
	t.Parallel()
	for _, name := range []string{"EVENTS", "funded.swe.done"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := fixture(t)
			before, err := snapshot(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, name), []byte("new synthetic evidence\n"), 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			changed, err := waitChanged(ctx, dir, before)
			if err != nil {
				t.Fatal(err)
			}
			if len(changed) != 1 || changed[0] != name {
				t.Fatalf("changed=%v", changed)
			}
		})
	}
}

func TestStartRejectsMissingBriefFileAndOccupiedPhase(t *testing.T) {
	t.Parallel()
	for _, row := range []string{
		`{"id":"funded","status":"planned","brief":"missing.brief"}`,
		`{"id":"funded","status":"planned","brief":"work","session_id":"already-running"}`,
	} {
		dir := fixture(t)
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"tasks":[`+row+`]}`), 0600); err != nil {
			t.Fatal(err)
		}
		err := Run(t.Context(), []string{"lane", "start", "funded", "--dir", dir, "--shadow", "--intent-dir", t.TempDir()}, nil, &bytes.Buffer{})
		if !errors.Is(err, ErrRefused) {
			t.Fatalf("unsafe start accepted: %s: %v", row, err)
		}
	}
}

func TestSteerRefusesMergedHeldAndDead(t *testing.T) {
	t.Parallel()
	for _, row := range []string{
		`{"id":"funded","status":"running","pr_state":"merged"}`,
		`{"id":"funded","status":"running","waiting_on":"owner","terminal_state":"working"}`,
		`{"id":"funded","status":"running","terminal_state":"ended"}`,
	} {
		dir := fixture(t)
		if err := os.WriteFile(filepath.Join(dir, "state.json"), []byte(`{"tasks":[`+row+`]}`), 0600); err != nil {
			t.Fatal(err)
		}
		err := Run(t.Context(), []string{"lane", "steer", "funded", "continue", "--dir", dir, "--shadow", "--intent-dir", t.TempDir()}, nil, &bytes.Buffer{})
		if !errors.Is(err, ErrRefused) {
			t.Fatalf("unsafe steer accepted: %s: %v", row, err)
		}
	}
}
