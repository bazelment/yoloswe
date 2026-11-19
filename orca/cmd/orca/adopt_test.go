package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/adopt"
	chatui "github.com/bazelment/yoloswe/orca/chat"
	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/dispatcher"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/mailbox"
	mailboxfake "github.com/bazelment/yoloswe/orca/mailbox/fake"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/store"
)

func TestAdoptDryRunDirUsesExplicitCopiedRun(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"copied goal","no_pr":true,"phases":[{"name":"swe"}]},"tasks":[{"id":"copied-lane","phase":"swe","status":"running","round":1}]}`)
	writeFile(t, filepath.Join(dir, "OBJECTIVE.md"), "Goal: copied goal\n")
	writeFile(t, filepath.Join(dir, "copied-lane.swe.md"), "Outcome: done\n")
	var stdout, stderr bytes.Buffer
	if err := Execute(t.Context(), []string{"adopt", "--dry-run", "--dir", dir, "ignored-discovery-id"}, strings.NewReader(""), &stdout, &stderr); err != nil {
		t.Fatalf("Execute adopt dry-run: %v\nstderr:\n%s", err, stderr.String())
	}
	out := stdout.String()
	for _, want := range []string{
		"adopt dry-run " + filepath.Base(dir),
		"format=subagent-swarm-nebula",
		"lane copied-lane: phase=swe status=running",
		"evidence:",
		"move: judge; reason:",
		"JUDGES: copied-lane:judge",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("output missing %q:\n%s", want, out)
		}
	}
}

func TestValidateShadowPathsRejectsOverlappingDirectories(t *testing.T) {
	root := t.TempDir()
	runDir := filepath.Join(root, "run")
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, out := range []string{runDir, filepath.Join(runDir, "out"), root} {
		if err := validateShadowPaths(runDir, out); err == nil {
			t.Fatalf("validateShadowPaths(%q, %q) succeeded, want overlap error", runDir, out)
		}
	}
	if err := validateShadowPaths(runDir, filepath.Join(root, "shadow-out")); err != nil {
		t.Fatalf("validateShadowPaths disjoint output: %v", err)
	}
}

// The shipped chat adopts, persists, and releases a host swarm lease.
func TestShippedChatAdoptsAndReleasesHostSwarm(t *testing.T) {
	for _, tc := range []struct {
		name, holder string
		bare         bool
	}{
		{name: "explicit run", holder: "orca-chat:chat-run"},
		{name: "bare orca", holder: "orca-chat:" + frontDeskRunID, bare: true},
	} {
		t.Run(tc.name, func(t *testing.T) { testShippedChatAdoption(t, tc.holder, tc.bare) })
	}
}

func testShippedChatAdoption(t *testing.T, holder string, bare bool) {
	root := t.TempDir()
	runID := "20261113-000000-host"
	runDir := filepath.Join(root, ".shared", "subagent-swarm", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(runDir, "state.json"), `{"config":{"goal":"host goal","phases":[{"name":"swe"}]},"tasks":[{"id":"a","title":"A","phase":"swe","status":"running","round":1,"sessions":{}}]}`)
	writeFile(t, filepath.Join(runDir, "OBJECTIVE.md"), "objective\n")
	writeFile(t, filepath.Join(runDir, "PROGRESS.md"), "progress\n")

	// watch_lanes.sh --status reports no watcher, so the handover has nothing
	// to stop and nothing to restart.
	script := filepath.Join(t.TempDir(), "watch_lanes.sh")
	writeFile(t, script, "#!/bin/bash\necho 'armed: none'\necho 'bridges: none'\nexit 1\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	label := "claude"
	var puts []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/code-runs/"+runID {
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		switch r.Method {
		case http.MethodGet:
			_ = json.NewEncoder(w).Encode(map[string]string{"id": runID, "externalOrchestratorLabel": label})
		case http.MethodPut:
			b, _ := io.ReadAll(r.Body)
			var body struct {
				Label *string `json:"externalOrchestratorLabel"`
			}
			if err := json.Unmarshal(b, &body); err != nil || body.Label == nil {
				http.Error(w, "bad body "+string(b), http.StatusBadRequest)
				return
			}
			label = *body.Label
			puts = append(puts, label)
			_, _ = w.Write([]byte(`{}`))
		default:
			http.Error(w, "method", http.StatusMethodNotAllowed)
		}
	}))
	defer srv.Close()

	cfgPath := filepath.Join(t.TempDir(), "orca.json")
	cfg, _ := json.Marshal(map[string]string{"nebula_api_url": srv.URL, "nebula_token": "test-token"})
	writeFile(t, cfgPath, string(cfg))
	t.Setenv("ORCA_SWARM_ROOTS", root)
	t.Setenv("ORCA_WATCH_LANES", script)

	dbPath := filepath.Join(t.TempDir(), "chat.db")
	storeDir := t.TempDir()
	args := []string{"chat", "--config", cfgPath, "--run", "chat-run", "--store", "sqlite:" + dbPath}
	if bare {
		args = []string{"chat", "--config", cfgPath, "--store-dir", storeDir, "--repo", "bazelment/yoloswe"}
	}
	args = append(args, "--replay", "testdata/concierge_status_replay.json")
	chat := func(input string) string {
		var out, errOut bytes.Buffer
		if err := Execute(t.Context(), args, strings.NewReader(input), &out, &errOut); err != nil {
			t.Fatalf("Execute chat: %v\nstderr:\n%s", err, errOut.String())
		}
		return out.String()
	}
	// Adopt in one chat process, release from a restarted one: the bare
	// /release must find the run through the stable holder id.
	got := chat("/adopt\n/adopt " + runID + "\n")
	for _, want := range []string{runID + ": host goal", "adopted " + runID} {
		if !strings.Contains(got, want) {
			t.Fatalf("chat output missing %q:\n%s", want, got)
		}
	}
	var lease struct{ Holder string }
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(runDir, ".orca-lease.json"))), &lease); err != nil || lease.Holder != holder {
		t.Fatalf("lease holder = %q (%v), want %q", lease.Holder, err, holder)
	}
	if line := strings.SplitN(readFile(t, filepath.Join(runDir, "OBJECTIVE.md")), "\n", 2)[0]; !strings.Contains(line, "("+holder+")") {
		t.Fatalf("stand-down line = %q, want holder %s", line, holder)
	}
	if bare {
		// The restarted bare chat attaches this newer run, not the front desk,
		// without claiming a lease held by an explicit --run run-later chat.
		path, err := store.RunDBPath(storeDir, "run-later")
		if err != nil {
			t.Fatal(err)
		}
		st, err := store.OpenSQLite(path, clock.NewRealClock())
		if err != nil {
			t.Fatal(err)
		}
		err = errors.Join(st.Runs().Create(t.Context(), store.Run{ID: "run-later", Repository: "bazelment/yoloswe", Objective: "later"}), st.Close())
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range []string{".orca-lease.json", "OBJECTIVE.md", "PROGRESS.md"} {
			p := filepath.Join(runDir, n)
			writeFile(t, p, strings.ReplaceAll(readFile(t, p), holder, "orca-chat:run-later"))
		}
		if got := chat("/release\n"); !strings.Contains(got, "no adopted swarm") {
			t.Fatalf("bare chat released foreign explicit-run lease:\n%s", got)
		}
		if err := json.Unmarshal([]byte(readFile(t, filepath.Join(runDir, ".orca-lease.json"))), &lease); err != nil || lease.Holder != "orca-chat:run-later" {
			t.Fatalf("foreign lease holder after bare chat startup = %q (%v), want orca-chat:run-later", lease.Holder, err)
		}
		for _, n := range []string{".orca-lease.json", "OBJECTIVE.md", "PROGRESS.md"} {
			p := filepath.Join(runDir, n)
			writeFile(t, p, strings.ReplaceAll(readFile(t, p), "orca-chat:run-later", holder))
		}
	}
	if got := chat("/release\n"); !strings.Contains(got, "released "+runID) {
		t.Fatalf("restarted chat /release:\n%s", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(puts, ",") != "orca,claude" {
		t.Fatalf("label writes = %q, want orca then claude", puts)
	}
	for _, n := range []string{"OBJECTIVE.md", "PROGRESS.md"} {
		b, _ := os.ReadFile(filepath.Join(runDir, n))
		if strings.Contains(string(b), "ORCHESTRATOR:") {
			t.Fatalf("%s still carries the stand-down line: %q", n, b)
		}
	}
}

func TestChatAdoptionDiscoversSiblingRepos(t *testing.T) {
	testWorkingDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprint("worktrees=", nested), func(t *testing.T) {
			parent := t.TempDir()
			roots := []string{filepath.Join(parent, "one"), filepath.Join(parent, "two")}
			for i := range roots {
				if nested {
					roots[i] = filepath.Join(roots[i], "worktrees", filepath.Base(roots[i]))
				}
				runDir := filepath.Join(roots[i], ".shared", "subagent-swarm", fmt.Sprintf("sibling-%d", i))
				if err := os.MkdirAll(runDir, 0o755); err != nil {
					t.Fatal(err)
				}
				writeFile(t, filepath.Join(runDir, "state.json"), `{"config":{"goal":"sibling goal"},"tasks":[]}`)
				if out, err := exec.Command("git", "init", roots[i]).CombinedOutput(); err != nil {
					t.Fatalf("git init: %v: %s", err, out)
				}
			}
			t.Chdir(roots[0])
			t.Setenv("ORCA_SWARM_ROOTS", "")
			script := filepath.Join(parent, "watch_lanes.sh")
			writeFile(t, script, "#!/bin/bash\n")
			t.Setenv("ORCA_WATCH_LANES", script)
			srv := httptest.NewServer(http.NotFoundHandler())
			t.Cleanup(srv.Close)
			t.Setenv("NEBULA_API_URL", srv.URL)
			t.Setenv("NEBULA_TOKEN", "test-token")
			var out, errOut bytes.Buffer
			err := Execute(t.Context(), []string{
				"chat", "--run", "chat-run", "--store", "memory", "--replay", filepath.Join(testWorkingDir, "testdata/concierge_status_replay.json"),
			}, strings.NewReader("/adopt\n"), &out, &errOut)
			if err != nil {
				t.Fatalf("Execute chat: %v: %s", err, errOut.String())
			}
			for i := range roots {
				want := fmt.Sprintf("sibling-%d: sibling goal", i)
				if !strings.Contains(out.String(), want) {
					t.Fatalf("/adopt output missing %q:\n%s", want, out.String())
				}
			}
		})
	}
}

func TestChatAdoptionListsRunsFromMultipleRepoRoots(t *testing.T) {
	roots := []string{t.TempDir(), t.TempDir()}
	wants := []string{"repo-one-run", "repo-two-run"}
	for i, root := range roots {
		runDir := filepath.Join(root, ".shared", "subagent-swarm", wants[i])
		if err := os.MkdirAll(runDir, 0o755); err != nil {
			t.Fatal(err)
		}
		writeFile(t, filepath.Join(runDir, "state.json"), `{"config":{"goal":"`+wants[i]+` goal"},"tasks":[]}`)
	}
	script := filepath.Join(t.TempDir(), "watch_lanes.sh")
	writeFile(t, script, "#!/bin/bash\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCA_SWARM_ROOTS", strings.Join(roots, string(os.PathListSeparator)))
	t.Setenv("ORCA_WATCH_LANES", script)
	t.Setenv("NEBULA_API_URL", "http://127.0.0.1:1")
	t.Setenv("NEBULA_TOKEN", "test-token")
	var out, errOut bytes.Buffer
	err := Execute(t.Context(), []string{
		"chat", "--run", "chat-run", "--store", "memory", "--replay", "testdata/concierge_status_replay.json",
	}, strings.NewReader("/adopt\n"), &out, &errOut)
	if err != nil {
		t.Fatalf("Execute chat: %v\nstderr:\n%s", err, errOut.String())
	}
	for _, want := range wants {
		if !strings.Contains(out.String(), want+": "+want+" goal") {
			t.Fatalf("/adopt output missing %q:\n%s", want, out.String())
		}
	}
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeFile(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestBareChatHolderIsStable(t *testing.T) {
	t.Parallel()
	for _, run := range []string{"", "  "} {
		holder, former := chatHolder(run)
		if holder != "orca-chat:"+frontDeskRunID || !slices.Equal(former, []string{"orca-chat:"}) {
			t.Fatalf("chatHolder(%q) = %q, %q; want the front desk, owning the empty legacy id", run, holder, former)
		}
	}
	if holder, former := chatHolder("chat-run"); holder != "orca-chat:chat-run" || former != nil {
		t.Fatalf("chatHolder(chat-run) = %q, %q", holder, former)
	}
}

// TestChatAdoptionDisabledOnlyWithReason: /adopt is off only without the
// Nebula client, and chat says why. A missing loop script fails only the
// handover of the run format that needs it, with its reason.
func TestChatAdoptionDisabledOnlyWithReason(t *testing.T) {
	t.Setenv("NEBULA_API_URL", "")
	t.Setenv("NEBULA_TOKEN", "")
	t.Setenv("ORCA_WATCH_LANES", filepath.Join(t.TempDir(), "missing.sh"))
	clk := clock.NewRealClock()
	st := store.NewMemoryStore(clk)
	if s, why := newAdoption(t.Context(), Config{RunID: "c"}, newHostSwarms(Config{RunID: "c"}, st, clk), "orca-chat:c", nil, st, clk, nil, nil); s != nil || !strings.Contains(why, "NEBULA_API_URL") {
		t.Fatalf("no credentials: service=%v reason=%q", s, why)
	}
	cfg := Config{RunID: "c", NebulaAPIURL: "http://127.0.0.1:1", NebulaToken: "t"}
	h := newHostSwarms(cfg, st, clk)
	if s, why := newAdoption(t.Context(), cfg, h, "orca-chat:c", nil, st, clk, nil, nil); s == nil || why != "" {
		t.Fatalf("Nebula client available: service=%v reason=%q", s, why)
	}
	sub := filepath.Join(t.TempDir(), ".shared", "subagent-swarm", "r")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(sub, "state.json"), `{"config":{},"tasks":[]}`)
	if _, err := h.watcher.Running(t.Context(), sub); err == nil || !strings.Contains(err.Error(), "watch_lanes.sh not found") {
		t.Fatalf("subagent-swarm run without watch_lanes.sh: %v", err)
	}
	script := filepath.Join(t.TempDir(), "watch_lanes.sh")
	writeFile(t, script, "#!/bin/bash\n")
	t.Setenv("ORCA_WATCH_LANES", script)
	if h := newHostSwarms(cfg, st, clk); h.watcher.Subagent == nil || h.importer.Orchestrators == nil || h.probe.TerminalState == nil {
		t.Fatalf("adapters available: %+v", h)
	}
}

func TestRestartRestartsHeldControllers(t *testing.T) {
	root := t.TempDir()
	runID := "20261114-trial-m1"
	dir := filepath.Join(root, ".swarm", runID)
	if err := os.MkdirAll(filepath.Join(dir, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"trial m1","base":"main","target":"main","repo":"repo-1","merge":"none",
"phases":[{"name":"swe"},{"name":"clean"},{"name":"review"}]},
"tasks":[{"id":"m2-default","title":"default prompts","branch":"swarm-lane/`+runID+`/m2-default","priority":"p1","status":"running","phase":"review","round":1,"session_id":"sess-m2","terminals":{"review":{"1":"term-m2-review"}},"pr":5705,"pr_head":"bbb"}]}`)
	writeFile(t, filepath.Join(dir, "OBJECTIVE.md"), "# "+runID+"\n")
	writeFile(t, filepath.Join(dir, "PROGRESS.md"), "Next: m2-default review.\n")
	writeFile(t, filepath.Join(dir, "EVENTS"), "2026-11-14T06:35:00Z\tm2-default\tstarted\treview term-m2-review\n")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/code-runs/" + runID:
			_, _ = io.WriteString(w, `{"id":"`+runID+`","externalOrchestratorLabel":"claude"}`)
		case "/api/code-sessions":
			_, _ = io.WriteString(w, `{"sessions":[]}`)
		case "/api/code-terminals/term-m2-review/phase-report":
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfgPath := filepath.Join(root, "orca.json")
	cfg, _ := json.Marshal(map[string]string{"nebula_api_url": srv.URL, "nebula_token": "test-token"})
	writeFile(t, cfgPath, string(cfg))
	t.Setenv("ORCA_SWARM_ROOTS", filepath.Join(root, ".swarm"))
	t.Setenv("ORCA_SWARM_NEBULA", filepath.Join(root, "unused_swarm_nebula.sh"))

	storeDir := filepath.Join(root, "store")
	baseArgs := []string{"chat", "--config", cfgPath, "--store-dir", storeDir, "--repo", "sycamore-labs/nebula"}

	// First bare orca session adopts the run.
	var out1, errOut1 bytes.Buffer
	if err := Execute(t.Context(), append(slices.Clone(baseArgs), "--replay", "testdata/concierge_status_replay.json"), strings.NewReader("/adopt "+runID+"\n"), &out1, &errOut1); err != nil {
		t.Fatalf("first Execute chat: %v\nstderr:\n%s", err, errOut1.String())
	}
	if !strings.Contains(out1.String(), "adopted "+runID) {
		t.Fatalf("first session output missing adoption confirmation:\n%s", out1.String())
	}

	// Simulate a journal written with the old empty holder ("orca-chat:").
	for _, n := range []string{".orca-lease.json", "OBJECTIVE.md", "PROGRESS.md"} {
		p := filepath.Join(dir, n)
		writeFile(t, p, strings.ReplaceAll(readFile(t, p), "orca-chat:"+frontDeskRunID, "orca-chat:"))
	}
	dbPath, err := store.RunDBPath(storeDir, frontDeskRunID)
	if err != nil {
		t.Fatal(err)
	}

	// Replay fixture that routes to the concierge and submits a directive to the held run without a second /adopt.
	var exchanges []model.RecordedExchange
	exchanges = append(exchanges,
		model.RecordedExchange{Response: textAnswer(`{"handler":"concierge","confidence":0.9}`)},
		model.RecordedExchange{Response: toolUse("call-dir-1", ToolDirectiveSubmit, `{"kind":"instruction","text":"Hold new lanes until m2-default merges."}`)},
		model.RecordedExchange{Response: textAnswer("Recorded directive on 20261114-trial-m1.")},
	)
	replayPath := filepath.Join(root, "directive_replay.json")
	if err := model.SaveRecording(replayPath, exchanges); err != nil {
		t.Fatal(err)
	}

	// Second bare orca session starts and immediately sends an owner instruction
	// (no second /adopt). The held run's controller must be live and migrated.
	var out2, errOut2 bytes.Buffer
	if err := Execute(t.Context(), append(slices.Clone(baseArgs), "--replay", replayPath), strings.NewReader("hold new lanes until m2-default merges\n"), &out2, &errOut2); err != nil {
		t.Fatalf("restarted Execute chat: %v\nstderr:\n%s", err, errOut2.String())
	}
	if !strings.Contains(out2.String(), "Recorded directive on 20261114-trial-m1.") {
		t.Fatalf("restarted chat output = %q", out2.String())
	}

	// Verify the journal migrated to the stable holder and the directive was
	// recorded without "adopted run ... has no live controller".
	var lease struct{ Holder string }
	if err := json.Unmarshal([]byte(readFile(t, filepath.Join(dir, ".orca-lease.json"))), &lease); err != nil || lease.Holder != "orca-chat:"+frontDeskRunID {
		t.Fatalf("migrated lease holder = %q (%v), want orca-chat:%s", lease.Holder, err, frontDeskRunID)
	}
	st2, err := store.OpenSQLite(dbPath, clock.NewRealClock())
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	dirs, err := st2.Directives().List(t.Context(), store.DirectiveFilter{RunID: runID})
	if err != nil || len(dirs) != 1 || !strings.Contains(dirs[0].Text, "Hold new lanes until m2-default merges.") {
		t.Fatalf("directives on held run after restart = %+v, err = %v", dirs, err)
	}
}

func writeFakeOrcaBinary(t *testing.T, path, version string, exitCode int) {
	t.Helper()
	tmp := path + ".tmp"
	script := fmt.Sprintf("#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then\n  echo \"orca %s\"\n  exit %d\nfi\nexit 0\n", version, exitCode)
	if err := os.WriteFile(tmp, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, path); err != nil {
		t.Fatal(err)
	}
}

func TestRestartExecsStartupPathWithSameArgvAndReplacedBinary(t *testing.T) {
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "orca")
	writeFakeOrcaBinary(t, binPath, "commit-old-111", 0)

	origPath, origVer, origCommit, origExec := startupPath, startupVersion, buildCommit, execProcess
	t.Cleanup(func() {
		startupPath, startupVersion, buildCommit, execProcess = origPath, origVer, origCommit, origExec
	})

	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	resolved := resolveStartupPath([]string{"orca"})
	if resolved != binPath {
		t.Fatalf("resolveStartupPath(orca) = %q, want %q", resolved, binPath)
	}
	startupPath = resolved
	startupVersion = "commit-old-111"
	buildCommit = ""

	oldStat, err := os.Stat(binPath)
	if err != nil {
		t.Fatal(err)
	}
	oldIno := oldStat.Sys().(*syscall.Stat_t).Ino

	// Replace the binary on disk (rm + mv to a new inode) before /restart.
	if err := os.Remove(binPath); err != nil {
		t.Fatal(err)
	}
	writeFakeOrcaBinary(t, binPath, "commit-new-222", 0)
	newStat, err := os.Stat(binPath)
	if err != nil {
		t.Fatal(err)
	}
	newIno := newStat.Sys().(*syscall.Stat_t).Ino

	var gotPath string
	var gotArgv, gotEnv []string
	var execIno uint64
	execProcess = func(path string, argv []string, env []string) error {
		gotPath = path
		gotArgv = slices.Clone(argv)
		gotEnv = slices.Clone(env)
		st, statErr := os.Stat(path)
		if statErr != nil {
			return statErr
		}
		execIno = st.Sys().(*syscall.Stat_t).Ino
		return nil
	}

	storeDir := t.TempDir()
	// Also record a live detached `orca run` child in the store to verify /restart reports it.
	detachedID := "run-detached-1"
	detachedDB, err := store.RunDBPath(storeDir, detachedID)
	if err != nil {
		t.Fatal(err)
	}
	detachedStore, err := store.OpenSQLite(detachedDB, clock.NewRealClock())
	if err != nil {
		t.Fatal(err)
	}
	if err := detachedStore.Runs().Create(t.Context(), store.Run{ID: detachedID, Repository: "bazelment/yoloswe", Objective: "detached goal"}); err != nil {
		t.Fatal(err)
	}
	if _, err := detachedStore.Runs().AcquireLease(t.Context(), detachedID, "orca-host-999", time.Hour); err != nil {
		t.Fatal(err)
	}
	_ = detachedStore.Close()

	args := []string{"chat", "--store-dir", storeDir, "--repo", "bazelment/yoloswe", "--replay", "testdata/concierge_status_replay.json"}
	var out, errOut bytes.Buffer
	if err := Execute(t.Context(), args, strings.NewReader("/restart\n/status\n"), &out, &errOut); err != nil {
		t.Fatalf("Execute /restart: %v\nstderr:\n%s", err, errOut.String())
	}
	if gotPath != binPath {
		t.Fatalf("exec path = %q, want %q", gotPath, binPath)
	}
	wantArgv := append([]string{binPath}, args...)
	if !slices.Equal(gotArgv, wantArgv) {
		t.Fatalf("exec argv = %q, want %q", gotArgv, wantArgv)
	}
	if execIno != newIno || (oldIno != newIno && execIno == oldIno) {
		t.Fatalf("exec inode = %d, want new inode %d (old %d)", execIno, newIno, oldIno)
	}
	if !slices.ContainsFunc(gotEnv, func(kv string) bool { return strings.HasPrefix(kv, restartEnvVar+"=") }) {
		t.Fatalf("exec env missing %s", restartEnvVar)
	}
	for _, want := range []string{
		"restarting orca (commit-old-111 -> commit-new-222): " + binPath,
		"detached orca run " + detachedID + " continues on old binary; to roll: stop old process and run `orca run --run " + detachedID,
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("restart output missing %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), "you> /status") {
		t.Fatalf("chat processed input after /restart:\n%s", out.String())
	}

	// Verify the restarted session with no held runs prints "orca restarted (version a -> b)"
	// instead of "reattached front-desk".
	for _, kv := range gotEnv {
		if k, v, ok := strings.Cut(kv, "="); ok && k == restartEnvVar {
			t.Setenv(k, v)
		}
	}
	startupVersion = "commit-new-222"
	var outRestarted, errRestarted bytes.Buffer
	if err := Execute(t.Context(), args, strings.NewReader("/exit\n"), &outRestarted, &errRestarted); err != nil {
		t.Fatalf("Execute restarted session: %v\nstderr:\n%s", err, errRestarted.String())
	}
	if !strings.Contains(outRestarted.String(), "orca restarted (version commit-old-111 -> commit-new-222)") {
		t.Fatalf("restarted output missing 'orca restarted (version commit-old-111 -> commit-new-222)':\n%s", outRestarted.String())
	}
	if strings.Contains(outRestarted.String(), "reattached front-desk") {
		t.Fatalf("restarted output with no held runs must not say 'reattached front-desk':\n%s", outRestarted.String())
	}

	// Verify ORCA_RESTARTED_FROM_PID without ":<version>" uses the running version recorded at start so the restart line names both.
	t.Setenv(restartEnvVar, "12345")
	startupVersion = "commit-new-333"
	var outLegacy, errLegacy bytes.Buffer
	if err := Execute(t.Context(), args, strings.NewReader("/exit\n"), &outLegacy, &errLegacy); err != nil {
		t.Fatalf("Execute legacy restarted session: %v\nstderr:\n%s", err, errLegacy.String())
	}
	if !strings.Contains(outLegacy.String(), "orca restarted (version commit-new-222 -> commit-new-333)") {
		t.Fatalf("legacy restarted output missing 'orca restarted (version commit-new-222 -> commit-new-333)':\n%s", outLegacy.String())
	}
}

func TestRestartTargetFailingVersionCheckLeavesChatRunning(t *testing.T) {
	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "orca")
	writeFakeOrcaBinary(t, binPath, "broken-build", 1)

	origPath, origVer, origExec := startupPath, startupVersion, execProcess
	t.Cleanup(func() {
		startupPath, startupVersion, execProcess = origPath, origVer, origExec
	})
	startupPath = binPath
	startupVersion = "commit-good"
	execCalled := false
	execProcess = func(string, []string, []string) error {
		execCalled = true
		return nil
	}

	storeDir := t.TempDir()
	args := []string{"chat", "--store-dir", storeDir, "--repo", "bazelment/yoloswe", "--replay", "testdata/concierge_status_replay.json"}
	var out, errOut bytes.Buffer
	if err := Execute(t.Context(), args, strings.NewReader("/restart\n/status\n/exit\n"), &out, &errOut); err != nil {
		t.Fatalf("Execute: %v\nstderr:\n%s", err, errOut.String())
	}
	if execCalled {
		t.Fatal("execProcess called when --version check failed")
	}
	for _, want := range []string{
		"error: restart target " + binPath + " --version failed",
		"you> /status",
		"front desk: no run yet | run: front-desk",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("output missing %q:\n%s", want, out.String())
		}
	}
}

func TestConciergeRunInteractiveExitPrefixAndRestart(t *testing.T) {
	clk := clock.NewRealClock()
	st := store.NewMemoryStore(clk)
	defer st.Close()
	if err := st.Runs().Create(t.Context(), store.Run{ID: "run-1", Objective: "test interactive"}); err != nil {
		t.Fatal(err)
	}
	c := NewConcierge("run-1", st, clk, nil, "")
	var restartPath string
	c.Restart = func(_ context.Context, path string) (string, error) {
		restartPath = path
		return "restarting orca (old -> new): " + path, nil
	}
	var out bytes.Buffer
	in := strings.NewReader("exit code handling is broken\n/restart /bin/orca-next\nshould not run\n")
	if err := c.RunInteractive(t.Context(), in, &out); err != nil {
		t.Fatalf("RunInteractive: %v", err)
	}
	if !strings.Contains(out.String(), "Objective: test interactive") {
		t.Fatalf("prompt starting with 'exit ' did not reach concierge:\n%s", out.String())
	}
	if restartPath != "/bin/orca-next" || !strings.Contains(out.String(), "restarting orca (old -> new): /bin/orca-next") {
		t.Fatalf("restartPath = %q, out =\n%s", restartPath, out.String())
	}
}

func waitForCondition(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	ticker := time.NewTicker(2 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for {
		if cond() {
			return
		}
		select {
		case <-timer.C:
			t.Fatalf("condition not met within %s", timeout)
		case <-ticker.C:
		}
	}
}

func TestDispatcherLaneMailIsolationAndVetting(t *testing.T) {
	t.Parallel()
	const runID = "run-mail-unit"
	clk := clock.NewFakeClock(time.Date(2026, 11, 14, 20, 0, 0, 0, time.UTC))
	st := store.NewMemoryStore(clk)
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Runs().Create(t.Context(), store.Run{ID: runID, Objective: "Keep lanes isolated."}); err != nil {
		t.Fatal(err)
	}
	if err := st.Lanes().Upsert(t.Context(), lane.Lane{
		ID:        "lane-a",
		RunID:     runID,
		Title:     "Lane A",
		Status:    lane.StatusBlocked,
		Phase:     "swe",
		Round:     1,
		SessionID: "sess-a",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.Lanes().Upsert(t.Context(), lane.Lane{
		ID:        "lane-b",
		RunID:     runID,
		Title:     "Lane B",
		Status:    lane.StatusRunning,
		Phase:     "swe",
		Round:     1,
		SessionID: "sess-b",
	}); err != nil {
		t.Fatal(err)
	}

	// 1. ParseAction aliases and VetAction checks for mail.answer.
	act, err := dispatcher.ParseAction("c1", dispatcher.ToolMailAnswer, json.RawMessage(`{"msg_id":"m-1","lane":"lane-a","body":"use /workspace"}`))
	if err != nil {
		t.Fatalf("ParseAction alias: %v", err)
	}
	if act.MailAnswer == nil || act.MailAnswer.MessageID != "m-1" || act.MailAnswer.LaneID != "lane-a" || act.MailAnswer.Answer != "use /workspace" {
		t.Fatalf("unexpected parsed MailAnswer: %+v", act.MailAnswer)
	}
	view, err := dispatcher.LoadStateView(t.Context(), st, clk, runID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.VetAction(view, act, dispatcher.DefaultInvariants()); err != nil {
		t.Fatalf("VetAction valid mail.answer: %v", err)
	}
	noTargetAct, err := dispatcher.ParseAction("c2", dispatcher.ToolMailAnswer, json.RawMessage(`{"answer":"orphan"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.VetAction(view, noTargetAct, dispatcher.DefaultInvariants()); err == nil {
		t.Fatal("expected VetAction to reject mail.answer without message_id or lane_id")
	}
	unknownLaneAct, err := dispatcher.ParseAction("c3", dispatcher.ToolMailAnswer, json.RawMessage(`{"lane_id":"no-such-lane","answer":"hi"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := dispatcher.VetAction(view, unknownLaneAct, dispatcher.DefaultInvariants()); err == nil {
		t.Fatal("expected VetAction to reject mail.answer for unknown lane")
	}

	// 2. Negative cases:
	// (a) An unrelated lane.steer on lane-a must NOT close or answer lane-a's open question.
	// (b) A directive.note in a batch with a lane note must NOT auto-reply to the lane note.
	// (c) An owner.ask for lane-b (or run-level) must NOT link to lane-a's open question.
	drv := lane.NewFakeDriver()
	box := mailboxfake.New(st.Events(), runID)
	qRaw, _ := json.Marshal(mailbox.Message{
		ID:       "q-lane-a",
		RunID:    runID,
		LaneID:   "lane-a",
		Kind:     "question",
		Class:    "needs-host",
		Blocking: true,
		Question: true,
		Body:     "Which checkout should lane-a use?",
	})
	qEv, err := st.Events().Append(t.Context(), store.Event{
		ID:        "mail:q-lane-a",
		RunID:     runID,
		LaneID:    "lane-a",
		Kind:      store.EventMail,
		Class:     store.EventClassJudgment,
		Source:    "nebula.mail",
		Summary:   "Which checkout should lane-a use?",
		Payload:   qRaw,
		CreatedAt: clk.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	noteRaw, _ := json.Marshal(mailbox.Message{
		ID:     "n-lane-a",
		RunID:  runID,
		LaneID: "lane-a",
		Kind:   "note",
		Body:   "Progress checkpoint",
	})
	noteEv, err := st.Events().Append(t.Context(), store.Event{
		ID:        "mail:n-lane-a",
		RunID:     runID,
		LaneID:    "lane-a",
		Kind:      store.EventMail,
		Class:     store.EventClassJudgment,
		Source:    "nebula.mail",
		Summary:   "Progress checkpoint",
		Payload:   noteRaw,
		CreatedAt: clk.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}

	client := model.NewFakeClient(
		// Turn 1: unrelated lane.steer on lane-a.
		model.Response{
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("c-steer", dispatcher.ToolLaneSteer, json.RawMessage(`{"lane_id":"lane-a","note":"rebase onto main","reason":"routine rebase"}`)),
			),
		},
		model.Response{
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock("Steered lane-a to rebase.")),
		},
		// Turn 2: directive.note in batch with noteEv.
		model.Response{
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("c-note", dispatcher.ToolDirectiveNote, json.RawMessage(`{"fact":"checkpoint noted","reason":"record checkpoint"}`)),
			),
		},
		model.Response{
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock("Recorded directive note.")),
		},
		// Turn 3: owner.ask on lane-b while qEv (lane-a) is in the batch.
		model.Response{
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("c-ask-b", dispatcher.ToolOwnerAsk, json.RawMessage(`{"id":"ask-lane-b","lane_id":"lane-b","question":"Approve lane-b budget?","options":["yes","no"],"default":"no","deadline":"1h","reason":"budget"}`)),
			),
		},
		model.Response{
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock("Asked owner about lane-b.")),
		},
		// Turn 4: mail.answer with mismatched lane_id ("lane-b" for lane-a's "q-lane-a") and unknown message_id ("no-such-msg"),
		// plus owner.ask with mismatched lane_id and unknown message_id — all must be rejected.
		model.Response{
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("c-ans-mismatch", dispatcher.ToolMailAnswer, json.RawMessage(`{"message_id":"q-lane-a","lane_id":"lane-b","answer":"wrong lane"}`)),
				model.NewToolUseBlock("c-ans-unknown", dispatcher.ToolMailAnswer, json.RawMessage(`{"message_id":"no-such-msg","lane_id":"lane-a","answer":"hallucinated"}`)),
				model.NewToolUseBlock("c-ask-mismatch", dispatcher.ToolOwnerAsk, json.RawMessage(`{"id":"ask-mismatch","message_id":"q-lane-a","lane_id":"lane-b","question":"Wrong lane?","options":["yes","no"],"default":"no","deadline":"1h","reason":"mismatch"}`)),
				model.NewToolUseBlock("c-ask-unknown", dispatcher.ToolOwnerAsk, json.RawMessage(`{"id":"ask-unknown","message_id":"no-such-msg","lane_id":"lane-a","question":"Unknown msg?","options":["yes","no"],"default":"no","deadline":"1h","reason":"unknown"}`)),
			),
		},
		model.Response{
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock("Rejected invalid mail IDs and lane mismatches.")),
		},
		// Turn 5: when two questions are on lane-a in the batch, mail.answer without message_id is rejected as ambiguous,
		// and a run-level owner.ask (no lane_id or message_id) succeeds without guessing a link.
		model.Response{
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("c-ans-ambig", dispatcher.ToolMailAnswer, json.RawMessage(`{"lane_id":"lane-a","answer":"ambiguous answer"}`)),
				model.NewToolUseBlock("c-ask-run", dispatcher.ToolOwnerAsk, json.RawMessage(`{"id":"ask-run","question":"Run-level policy?","options":["a","b"],"default":"a","deadline":"1h","reason":"run policy"}`)),
			),
		},
		model.Response{
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock("Registered run-level ask.")),
		},
		// Turn 6: an owner.ask with explicit message_id links to q-lane-a even when multiple questions are in the batch.
		model.Response{
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("c-ask-explicit", dispatcher.ToolOwnerAsk, json.RawMessage(`{"id":"ask-explicit","message_id":"q-lane-a","question":"Checkout for lane-a?","options":["main","dev"],"default":"main","deadline":"1h","reason":"explicit"}`)),
			),
		},
		model.Response{
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock("Escalated explicit message_id.")),
		},
		// Turn 7: reply to note n-lane-a via mail.answer — must reply via mailbox without steering or unblocking lane-a.
		model.Response{
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("c-reply-note", dispatcher.ToolMailAnswer, json.RawMessage(`{"message_id":"n-lane-a","lane_id":"lane-a","answer":"ack note","reason":"acknowledge note"}`)),
			),
		},
		model.Response{
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock("Replied to note.")),
		},
		// Turn 8: answer q-lane-a-2 once.
		model.Response{
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("c-ans-q2-1", dispatcher.ToolMailAnswer, json.RawMessage(`{"message_id":"q-lane-a-2","lane_id":"lane-a","answer":"first answer","reason":"answer q2"}`)),
			),
		},
		model.Response{
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock("Answered q2 once.")),
		},
		// Turn 9: attempt to answer q-lane-a-2 a second time — rejected as already answered.
		model.Response{
			StopReason: model.StopReasonToolUse,
			Message: model.NewAssistantMessage(
				model.NewToolUseBlock("c-ans-q2-2", dispatcher.ToolMailAnswer, json.RawMessage(`{"message_id":"q-lane-a-2","lane_id":"lane-a","answer":"duplicate answer","reason":"duplicate"}`)),
			),
		},
		model.Response{
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock("Rejected duplicate answer.")),
		},
	)

	d := dispatcher.New(dispatcher.Config{Model: "gpt-6.1-sol"}, st, drv, clk, client)
	d.SetMailboxes(box)

	if _, err := d.Dispatch(t.Context(), runID, []store.Event{qEv}); err != nil {
		t.Fatalf("Dispatch turn 1 (steer): %v", err)
	}
	if ans, ok := box.AnswerFor("q-lane-a"); ok || ans != "" {
		t.Fatalf("unrelated lane.steer must not answer lane question, got %q", ans)
	}
	openQs, err := mailbox.OpenLaneQuestions(t.Context(), st, runID, clk.Now())
	if err != nil || len(openQs) != 1 || openQs[0].ID != "q-lane-a" {
		t.Fatalf("question must remain open after unrelated steer, got %+v (err=%v)", openQs, err)
	}

	if _, err := d.Dispatch(t.Context(), runID, []store.Event{noteEv}); err != nil {
		t.Fatalf("Dispatch turn 2 (directive.note): %v", err)
	}
	if len(box.Sent()) != 0 {
		t.Fatalf("directive.note must not auto-reply to lane notes, got sent=%+v", box.Sent())
	}

	if _, err := d.Dispatch(t.Context(), runID, []store.Event{qEv}); err != nil {
		t.Fatalf("Dispatch turn 3 (owner.ask on lane-b): %v", err)
	}
	dirs, err := st.Directives().List(t.Context(), store.DirectiveFilter{RunID: runID, ActiveOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var askB controller.OwnerAsk
	foundAskB := false
	for i := range dirs {
		if dirs[i].ID == "ask-ask-lane-b" {
			foundAskB = true
			if err := json.Unmarshal([]byte(dirs[i].Text), &askB); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !foundAskB {
		t.Fatalf("expected directive ask-ask-lane-b to be created, got %+v", dirs)
	}
	if askB.SourceEventID != "" || askB.SourceMessageID != "" {
		t.Fatalf("owner.ask for lane-b must not link to lane-a mail, got %+v", askB)
	}

	if _, err := d.Dispatch(t.Context(), runID, []store.Event{qEv}); err != nil {
		t.Fatalf("Dispatch turn 4 (mismatch and unknown IDs): %v", err)
	}
	if ans, ok := box.AnswerFor("q-lane-a"); ok || ans != "" {
		t.Fatalf("mismatched lane_id must not answer q-lane-a, got %q", ans)
	}
	if ans, ok := box.AnswerFor("no-such-msg"); ok || ans != "" {
		t.Fatalf("unknown message_id must not be answered, got %q", ans)
	}

	q2Raw, _ := json.Marshal(mailbox.Message{
		ID:       "q-lane-a-2",
		RunID:    runID,
		LaneID:   "lane-a",
		Kind:     "question",
		Blocking: true,
		Question: true,
		Body:     "Second question on lane-a?",
	})
	q2Ev, err := st.Events().Append(t.Context(), store.Event{
		ID:        "mail:q-lane-a-2",
		RunID:     runID,
		LaneID:    "lane-a",
		Kind:      store.EventMail,
		Class:     store.EventClassJudgment,
		Source:    "nebula.mail",
		Summary:   "Second question on lane-a?",
		Payload:   q2Raw,
		CreatedAt: clk.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Dispatch(t.Context(), runID, []store.Event{qEv, q2Ev}); err != nil {
		t.Fatalf("Dispatch turn 5 (ambiguous mail.answer + run-level owner.ask): %v", err)
	}
	if _, err := d.Dispatch(t.Context(), runID, []store.Event{qEv, q2Ev}); err != nil {
		t.Fatalf("Dispatch turn 6 (explicit message_id owner.ask): %v", err)
	}
	dirsAfter5, err := st.Directives().List(t.Context(), store.DirectiveFilter{RunID: runID, ActiveOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var askRun, askExplicit controller.OwnerAsk
	foundRun := false
	foundExplicit := false
	for i := range dirsAfter5 {
		if dirsAfter5[i].ID == "ask-ask-mismatch" || dirsAfter5[i].ID == "ask-ask-unknown" {
			t.Fatalf("rejected owner.ask must not be persisted: %+v", dirsAfter5[i])
		}
		if dirsAfter5[i].ID == "ask-ask-run" {
			foundRun = true
			if err := json.Unmarshal([]byte(dirsAfter5[i].Text), &askRun); err != nil {
				t.Fatal(err)
			}
		}
		if dirsAfter5[i].ID == "ask-ask-explicit" {
			foundExplicit = true
			if err := json.Unmarshal([]byte(dirsAfter5[i].Text), &askExplicit); err != nil {
				t.Fatal(err)
			}
		}
	}
	if !foundRun || askRun.SourceMessageID != "" || askRun.SourceEventID != "" || askRun.LaneID != "" {
		t.Fatalf("expected run-level ask-run to succeed without linking to lane mail, got found=%v ask=%+v", foundRun, askRun)
	}
	if !foundExplicit || askExplicit.SourceMessageID != "q-lane-a" || askExplicit.SourceEventID != "mail:q-lane-a" || askExplicit.LaneID != "lane-a" {
		t.Fatalf("expected ask-explicit to link to q-lane-a on lane-a, got found=%v ask=%+v", foundExplicit, askExplicit)
	}

	// Failed/rejected mail.answer decisions from Turn 4 must NOT remove q-lane-a from OpenLaneQuestions.
	openAfterRejected, err := mailbox.OpenLaneQuestions(t.Context(), st, runID, clk.Now())
	if err != nil || len(openAfterRejected) < 1 {
		t.Fatalf("rejected mail.answer decision must not mark q-lane-a as answered, got %+v (err=%v)", openAfterRejected, err)
	}
	foundQLaneA := false
	for _, q := range openAfterRejected {
		if q.ID == "q-lane-a" {
			foundQLaneA = true
		}
	}
	if !foundQLaneA {
		t.Fatalf("expected q-lane-a to remain in OpenLaneQuestions after rejected mail.answer, got %+v", openAfterRejected)
	}

	// Turn 7: replying to note n-lane-a must not steer or unblock lane-a.
	if err := st.Lanes().Upsert(t.Context(), lane.Lane{
		ID:        "lane-a",
		RunID:     runID,
		Title:     "Lane A",
		Status:    lane.StatusBlocked,
		Phase:     "swe",
		Round:     1,
		SessionID: "sess-a",
	}); err != nil {
		t.Fatal(err)
	}
	steersBeforeNote := len(drv.Steers)
	if _, err := d.Dispatch(t.Context(), runID, []store.Event{noteEv}); err != nil {
		t.Fatalf("Dispatch turn 7 (note reply): %v", err)
	}
	if got := len(drv.Steers); got != steersBeforeNote {
		t.Fatalf("note reply must not steer lane-a, steers before=%d after=%d", steersBeforeNote, got)
	}
	lnAfterNote, err := st.Lanes().Get(t.Context(), runID, "lane-a")
	if err != nil || lnAfterNote.Status != lane.StatusBlocked {
		t.Fatalf("note reply must not unblock lane-a, got status=%v err=%v", lnAfterNote.Status, err)
	}

	// Turn 8 & 9: answering q-lane-a-2 across two turns must succeed once and reject the duplicate.
	if _, err := d.Dispatch(t.Context(), runID, nil); err != nil {
		t.Fatalf("Dispatch turn 8 (first mail.answer): %v", err)
	}
	res8, err := d.Dispatch(t.Context(), runID, nil)
	if err != nil {
		t.Fatalf("Dispatch turn 9 (duplicate mail.answer): %v", err)
	}
	if got, _ := box.AnswerFor("q-lane-a-2"); got != "first answer" {
		t.Fatalf("expected q-lane-a-2 answer to remain 'first answer', got %q", got)
	}
	rejectedDup := false
	for _, dec := range res8.Decisions {
		if dec.Tool == dispatcher.ToolMailAnswer && strings.Contains(dec.Result, "already answered") {
			rejectedDup = true
		}
	}
	if !rejectedDup {
		t.Fatalf("expected second mail.answer on q-lane-a-2 to be rejected as already answered, decisions=%+v", res8.Decisions)
	}

	// Resolving an owner_ask for an ID-less mail event must not pass the synthetic store event ID as a Nebula message ID,
	// and resolving an owner_ask with a "hold:" option keeps the question open in OpenLaneQuestions.
	idlessRaw, _ := json.Marshal(mailbox.Message{
		RunID:    runID,
		LaneID:   "lane-b",
		Kind:     "question",
		Blocking: true,
		Question: true,
		Body:     "ID-less question?",
	})
	idlessEv, err := st.Events().Append(t.Context(), store.Event{
		ID:        "mail:idless-1",
		RunID:     runID,
		LaneID:    "lane-b",
		Kind:      store.EventMail,
		Payload:   idlessRaw,
		CreatedAt: clk.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctrl := controller.New(controller.Config{RunID: runID, HolderID: "h1", LeaseTTL: time.Minute}, st, drv, clk, nil, box)
	if err := ctrl.AskOwner(t.Context(), controller.OwnerAsk{
		ID:            "ask-idless",
		RunID:         runID,
		LaneID:        "lane-b",
		Question:      "ID-less question?",
		Options:       []string{"hold: wait", "proceed"},
		Default:       "proceed",
		Deadline:      clk.Now().Add(time.Hour),
		SourceEventID: idlessEv.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.AnswerOwnerAsk(t.Context(), "ask-idless", "hold: wait"); err != nil {
		t.Fatalf("AnswerOwnerAsk hold: %v", err)
	}
	openAfterHold, err := mailbox.OpenUnescalatedQuestions(t.Context(), st, runID, clk.Now())
	if err != nil {
		t.Fatal(err)
	}
	foundHeld := false
	for _, q := range openAfterHold {
		if q.EventID == idlessEv.ID && q.EscalatedAskID == "" {
			foundHeld = true
		}
	}
	if !foundHeld {
		t.Fatalf("expected held owner_ask question %s to remain visible in OpenUnescalatedQuestions, got %+v", idlessEv.ID, openAfterHold)
	}
	if err := ctrl.AskOwner(t.Context(), controller.OwnerAsk{
		ID:            "ask-idless-resolve",
		RunID:         runID,
		LaneID:        "lane-b",
		Question:      "ID-less question?",
		Options:       []string{"proceed"},
		Default:       "proceed",
		Deadline:      clk.Now().Add(time.Hour),
		SourceEventID: idlessEv.ID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.AnswerOwnerAsk(t.Context(), "ask-idless-resolve", "proceed"); err != nil {
		t.Fatalf("AnswerOwnerAsk proceed on ID-less mail: %v", err)
	}
	if ans, ok := box.AnswerFor(idlessEv.ID); ok || ans != "" {
		t.Fatalf("ID-less mail resolution must not call Responder.Answer with store event ID %s, got %q", idlessEv.ID, ans)
	}
}

func TestAdoptedRunRestartNoDuplicateMailOrLaneReportEvents(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	const runID = "20261114-trial-m1"
	dir := filepath.Join(root, ".swarm", runID)
	if err := os.MkdirAll(filepath.Join(dir, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"Verify no duplicate events on restart.","base":"main","target":"main","repo":"bazelment/yoloswe","merge":"none","phases":[{"name":"swe"},{"name":"clean"},{"name":"review"}]},"tasks":[{"id":"m2-gaps2","title":"Gaps 2","branch":"swarm-lane/`+runID+`/m2-gaps2","priority":"p1","status":"running","phase":"swe","round":1,"session_id":"s-gaps2","terminals":{"swe":{"1":"t-gaps2"}}}]}`)
	writeFile(t, filepath.Join(dir, "OBJECTIVE.md"), "# Objective\nVerify no duplicate events on restart.\n")
	writeFile(t, filepath.Join(dir, "PROGRESS.md"), "# Progress\n- [ ] `m2-gaps2`\n")
	writeFile(t, filepath.Join(dir, "EVENTS"), "2026-11-14T19:00:00Z\tm2-gaps2\tstarted\tswe t-gaps2\n")

	var mailCursors []string
	var mailMu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/code-runs/"+runID:
			_, _ = io.WriteString(w, `{"id":"`+runID+`","externalOrchestratorLabel":"claude"}`)
		case r.Method == http.MethodPut && r.URL.Path == "/api/code-runs/"+runID:
			_, _ = io.WriteString(w, `{}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/code-sessions":
			_, _ = io.WriteString(w, `{"sessions":[{"id":"s-gaps2","metadata":{"swarmRun":"`+runID+`","swarmLane":"m2-gaps2"},"swarmRunId":"`+runID+`","swarmLaneId":"m2-gaps2","terminals":[{"id":"t-gaps2","phase":"swe","round":1,"status":"exited","exitCode":0,"report":{"outcome":"done","headSha":"abc1234","summary":"completed swe"}}]}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/code-sessions/s-gaps2":
			_, _ = io.WriteString(w, `{"id":"s-gaps2","terminals":[{"id":"t-gaps2","status":"exited"}]}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/code-sessions/s-gaps2/terminals":
			_, _ = io.WriteString(w, `{"id":"t-gaps2-clean","status":"running"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/code-terminals/t-gaps2-clean":
			_, _ = io.WriteString(w, `{"id":"t-gaps2-clean","status":"running"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/code-terminals/t-gaps2-clean/phase-report":
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"code":"NO_REPORT"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/code-terminals/t-gaps2-clean/events":
			w.Header().Set("Content-Type", "text/event-stream")
		case r.Method == http.MethodGet && r.URL.Path == "/api/code-terminals/t-gaps2":
			_, _ = io.WriteString(w, `{"id":"t-gaps2","status":"exited","exitCode":0,"report":{"outcome":"done","headSha":"abc1234","summary":"completed swe"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/code-terminals/t-gaps2/phase-report":
			_, _ = io.WriteString(w, `{"terminalId":"t-gaps2","outcome":"done","head":"abc1234","body":"Outcome: done\nHEAD: abc1234\nSummary: completed swe\n"}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/code-terminals/t-gaps2/events":
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(w, "data: {\"type\":\"phase.reported\",\"report\":{\"outcome\":\"done\",\"head\":\"abc1234\",\"body\":\"Outcome: done\\nHEAD: abc1234\\nSummary: completed swe\\n\"}}\n\n")
			_, _ = io.WriteString(w, "data: {\"type\":\"terminal.exited\",\"exitCode\":0}\n\n")
		case r.Method == http.MethodGet && r.URL.Path == "/api/mail":
			cur := r.URL.Query().Get("cursor")
			mailMu.Lock()
			mailCursors = append(mailCursors, cur)
			mailMu.Unlock()
			if cur == "cur-1" {
				_, _ = io.WriteString(w, `{"messages":[],"nextCursor":"cur-1"}`)
				return
			}
			_, _ = io.WriteString(w, `{"messages":[{"id":"m-note-1","runId":"`+runID+`","lane":"m2-gaps2","kind":"note","body":"progress note"}],"nextCursor":"cur-1"}`)
		case r.Method == http.MethodPost && r.URL.Path == "/api/mail":
			_, _ = io.WriteString(w, `{"id":"reply-1"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	clk := clock.NewFakeClock(time.Date(2026, 11, 14, 19, 15, 0, 0, time.UTC))
	st := store.NewMemoryStore(clk)
	t.Cleanup(func() { _ = st.Close() })

	cfg := Config{
		RunID:        frontDeskRunID,
		NebulaAPIURL: srv.URL,
		NebulaToken:  "test-token",
	}

	// First session: adopt the run and let it ingest the mail event and the phase report.
	host1 := newHostSwarms(cfg, st, clk)
	host1.importer.Roots = []string{filepath.Join(root, ".swarm")}
	host1.watcher = adopt.FormatWatcher{Nebula: noopAdoptWatcher{}}
	host1.importer.Loops = host1.watcher
	host1.probe.PR = nil
	adoption1, why := newAdoption(t.Context(), cfg, host1, "orca-chat:front-desk", nil, st, clk, nil, nil)
	if adoption1 == nil {
		t.Fatalf("newAdoption failed: %s", why)
	}
	if _, err := adoption1.Adopt(t.Context(), runID); err != nil {
		t.Fatalf("Adopt 1: %v", err)
	}

	waitForCondition(t, 3*time.Second, func() bool {
		mailEvs, _ := st.Events().List(t.Context(), store.EventFilter{RunID: runID, Kinds: []store.EventKind{store.EventMail}})
		repEvs, _ := st.Events().List(t.Context(), store.EventFilter{RunID: runID, Kinds: []store.EventKind{store.EventLaneReport}})
		return len(mailEvs) >= 1 && len(repEvs) >= 1
	})
	if closer, ok := adoption1.Driver.(interface{ Close(context.Context) error }); ok {
		if err := closer.Close(t.Context()); err != nil {
			t.Fatalf("Close 1: %v", err)
		}
	}

	mailBefore, _ := st.Events().List(t.Context(), store.EventFilter{RunID: runID, Kinds: []store.EventKind{store.EventMail}})
	repBefore, _ := st.Events().List(t.Context(), store.EventFilter{RunID: runID, Kinds: []store.EventKind{store.EventLaneReport}})
	if len(mailBefore) != 1 || len(repBefore) != 1 {
		t.Fatalf("before restart: mail=%d lane_report=%d, want 1 and 1", len(mailBefore), len(repBefore))
	}

	// Simulate /restart: construct fresh hostSwarms and adoption service on the same store and call RestartHeld.
	clk.Advance(time.Minute)
	host2 := newHostSwarms(cfg, st, clk)
	host2.importer.Roots = []string{filepath.Join(root, ".swarm")}
	host2.watcher = adopt.FormatWatcher{Nebula: noopAdoptWatcher{}}
	host2.importer.Loops = host2.watcher
	host2.probe.PR = nil
	adoption2, _ := newAdoption(t.Context(), cfg, host2, "orca-chat:front-desk", nil, st, clk, nil, nil)
	backend2 := chatui.NewStoreBackend(st, frontDeskRunID)
	backend2.FrontDesk = frontDeskRunID
	backend2.Adoption = adoption2
	restarted, err := backend2.RestartHeld(t.Context())
	if err != nil || len(restarted) != 1 {
		t.Fatalf("RestartHeld = %+v, err=%v", restarted, err)
	}
	// Wait for at least one mailbox poll after restart.
	mailMu.Lock()
	pollsBefore := len(mailCursors)
	mailMu.Unlock()
	waitForCondition(t, 3*time.Second, func() bool {
		mailMu.Lock()
		pollsNow := len(mailCursors)
		mailMu.Unlock()
		return pollsNow > pollsBefore
	})
	if closer, ok := adoption2.Driver.(interface{ Close(context.Context) error }); ok {
		_ = closer.Close(t.Context())
	}

	mailAfter, _ := st.Events().List(t.Context(), store.EventFilter{RunID: runID, Kinds: []store.EventKind{store.EventMail}})
	repAfter, _ := st.Events().List(t.Context(), store.EventFilter{RunID: runID, Kinds: []store.EventKind{store.EventLaneReport}})
	if len(mailAfter) != 1 {
		t.Fatalf("duplicate mail events after restart: got %d (%+v), want 1", len(mailAfter), mailAfter)
	}
	if len(repAfter) != 1 {
		t.Fatalf("duplicate lane_report events after restart: got %d (%+v), want 1", len(repAfter), repAfter)
	}
	mailMu.Lock()
	lastCursor := mailCursors[len(mailCursors)-1]
	mailMu.Unlock()
	if lastCursor != "cur-1" {
		t.Fatalf("expected restarted mailbox watch to use saved cursor cur-1, got %q (all=%v)", lastCursor, mailCursors)
	}
}

type noopAdoptWatcher struct{}

func (noopAdoptWatcher) Running(context.Context, string) ([]int, error) { return nil, nil }
func (noopAdoptWatcher) Stop(context.Context, []int) error              { return nil }
func (noopAdoptWatcher) Start(context.Context, string) error            { return nil }

func TestConfigMaxCostPerHourDefaultAndExplicitZero(t *testing.T) {
	// Default when unset is DefaultMaxCostPerHourUSD ($5.00/h).
	t.Setenv("ORCA_MAX_COST_PER_HOUR_USD", "")
	cfgDefault := (Config{}).withDefaults()
	if cfgDefault.MaxCostPerHourUSD != DefaultMaxCostPerHourUSD {
		t.Fatalf("default MaxCostPerHourUSD = %.2f, want %.2f", cfgDefault.MaxCostPerHourUSD, DefaultMaxCostPerHourUSD)
	}

	// Explicit --max-cost-per-hour-usd=0 disables the guard (stays 0).
	cfgFlagZero, _, err := parseFlags("run", []string{"--run", "r1", "--goal", "g", "--max-cost-per-hour-usd", "0"}, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags(--max-cost-per-hour-usd 0): %v", err)
	}
	cfgFlagZero = cfgFlagZero.withDefaults()
	if cfgFlagZero.MaxCostPerHourUSD != 0 {
		t.Fatalf("flag --max-cost-per-hour-usd 0 got %.2f, want 0", cfgFlagZero.MaxCostPerHourUSD)
	}

	// Explicit ORCA_MAX_COST_PER_HOUR_USD=0 disables the guard (stays 0).
	t.Setenv("ORCA_MAX_COST_PER_HOUR_USD", "0")
	cfgEnvZero := (Config{}).withDefaults()
	if cfgEnvZero.MaxCostPerHourUSD != 0 {
		t.Fatalf("env ORCA_MAX_COST_PER_HOUR_USD=0 got %.2f, want 0", cfgEnvZero.MaxCostPerHourUSD)
	}
	t.Setenv("ORCA_MAX_COST_PER_HOUR_USD", "")

	// Explicit "max_cost_per_hour_usd": 0 in config JSON disables the guard (stays 0).
	cfgPath := filepath.Join(t.TempDir(), "orca.json")
	writeFile(t, cfgPath, `{"run_id":"r1","goal":"g","max_cost_per_hour_usd":0}`)
	cfgFileZero, _, err := parseFlags("run", []string{"--config", cfgPath}, io.Discard)
	if err != nil {
		t.Fatalf("parseFlags(--config with max_cost_per_hour_usd:0): %v", err)
	}
	cfgFileZero = cfgFileZero.withDefaults()
	if cfgFileZero.MaxCostPerHourUSD != 0 {
		t.Fatalf("config file max_cost_per_hour_usd:0 got %.2f, want 0", cfgFileZero.MaxCostPerHourUSD)
	}
}

func TestShadowNebulaClientsGuardInnerWrites(t *testing.T) {
	var writes atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writes.Add(1)
		}
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	cfg := Config{NebulaAPIURL: srv.URL, NebulaToken: "synthetic"}
	clk := clock.RealClock{}
	st := store.NewMemoryStore(clk)
	defer st.Close()
	rec, err := adopt.NewActionRecorder(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer rec.Close()
	h := newHostSwarmsWithRecorder(cfg, st, clk, rec)
	if h.client == nil {
		t.Fatal(h.off)
	}
	if err := h.client.WakeSession(t.Context(), "sleeping"); err == nil {
		t.Fatal("inner wake allowed")
	}
	boxes := adoptedMailboxesWithRecorder(cfg, st, rec)("r")
	if len(boxes) != 1 {
		t.Fatalf("mailboxes=%d", len(boxes))
	}
	if err := boxes[0].SendLane(t.Context(), "lane", "hello"); err == nil {
		t.Fatal("inner mail write allowed")
	}
	if writes.Load() != 0 {
		t.Fatalf("shadow clients sent %d writes", writes.Load())
	}
}

func TestShadowAdoptOutputFlags(t *testing.T) {
	cfg, _, err := parseFlags("adopt", []string{"--shadow", "--dir", "run", "--out", "shadow-out", "--for", "1m"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Shadow || cfg.AdoptOut != "shadow-out" || cfg.ShadowFor != time.Minute || cfg.AgentBackend != model.AgentBackendCLI || cfg.MaxCostPerHourUSD != 0 {
		t.Fatalf("unexpected shadow/output flags: shadow=%t out=%q for=%s", cfg.Shadow, cfg.AdoptOut, cfg.ShadowFor)
	}
}
