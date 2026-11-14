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
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/adopt"
	chatui "github.com/bazelment/yoloswe/orca/chat"
	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/store"
)

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

// The imported terminal deliberately has no swarm metadata in the session list.
func TestAdoptMidReviewAdvances(t *testing.T) {
	testAdoptTransition(t, "done", "", "integrate", 2, false)
}

func TestAdoptReviewNeedsSWE(t *testing.T) {
	testAdoptTransition(t, "needs-swe", "", "swe", 3, false)
}

func TestAdoptCompletionReleasesSession(t *testing.T) {
	testAdoptTransition(t, "done", "", "integrate", 2, true)
}

func TestAdoptManualMergeDoesNotLaunchIntegrator(t *testing.T) {
	testAdoptTransition(t, "done", `,"merge_policy":{"mode":"manual","require_review_pass":true}`, "review", 2, true)
}

type adoptionOutput struct {
	adopted chan struct{}
	once    sync.Once
}

func (w *adoptionOutput) Write(p []byte) (int, error) {
	if strings.Contains(string(p), "adopted mid-review") {
		w.once.Do(func() { close(w.adopted) })
	}
	return len(p), nil
}

func testAdoptTransition(t *testing.T, outcome, policy, wantPhase string, wantRound int, complete bool) {
	t.Helper()
	root := t.TempDir()
	runID := "mid-review"
	dir := filepath.Join(root, ".shared", "subagent-swarm", runID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"finish review","target":"release","merge_rule":"Use the repository merge queue."`+policy+`},"tasks":[{"id":"a","title":"A","branch":"lane-a","phase":"review","status":"running","round":2,"pr":42,"host_only":"keep","sessions":{"review":"nebula:old-review"}}]}`)
	if err := os.MkdirAll(filepath.Join(dir, "missions"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(dir, "missions", "a.md"), "Preserve zero-copy packet handling.")
	writeFile(t, filepath.Join(dir, "a.review.spawn.json"), `{"session_id":"imported-session","terminal_id":"imported-review"}`)
	script := filepath.Join(root, "watch_lanes.sh")
	writeFile(t, script, "#!/bin/bash\necho 'armed: none'\necho 'bridges: none'\nexit 1\n")
	if err := os.Chmod(script, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("ORCA_SWARM_ROOTS", root)
	t.Setenv("ORCA_WATCH_LANES", script)
	integrated := make(chan string, 1)
	prompts := make(chan string, 1)
	reportReady := make(chan struct{})
	released := make(chan struct{}, 1)
	completeReport := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/code-runs/"+runID:
			_, _ = io.WriteString(w, `{"id":"mid-review","externalOrchestratorLabel":"claude"}`)
		case r.URL.Path == "/api/code-sessions":
			_, _ = io.WriteString(w, `{"sessions":[]}`)
		case r.URL.Path == "/api/code-terminals/imported-review/phase-report":
			select {
			case <-reportReady:
				_ = json.NewEncoder(w).Encode(map[string]any{"terminalId": "imported-review", "outcome": outcome, "head": "reviewed-head", "pr": 42, "body": "Outcome: " + outcome + "\nHEAD: reviewed-head\nPR: 42"})
			default:
				http.NotFound(w, r)
			}
		case r.URL.Path == "/api/code-terminals/integrate-terminal/phase-report":
			select {
			case <-completeReport:
				_, _ = io.WriteString(w, `{"terminalId":"integrate-terminal","outcome":"done","head":"reviewed-head","pr":42,"body":"Outcome: done\nHEAD: reviewed-head\nPR: 42"}`)
			default:
				http.NotFound(w, r)
			}
		case r.URL.Path == "/api/code-sessions/imported-session/release":
			released <- struct{}{}
			_, _ = io.WriteString(w, `{}`)
		case r.URL.Path == "/api/code-sessions/imported-session/terminals" && r.Method == http.MethodPost:
			var body struct {
				Title  string `json:"title"`
				Prompt string `json:"prompt"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			prompts <- body.Prompt
			integrated <- body.Title
			_, _ = io.WriteString(w, `{"id":"integrate-terminal"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	cfgPath := filepath.Join(root, "orca.json")
	cfg, _ := json.Marshal(map[string]string{"nebula_api_url": srv.URL, "nebula_token": "test-token"})
	writeFile(t, cfgPath, string(cfg))
	in, input := io.Pipe()
	defer in.Close()
	defer input.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	output := &adoptionOutput{adopted: make(chan struct{})}
	runChat := func(input io.Reader) error {
		return Execute(ctx, []string{"chat", "--config", cfgPath, "--run", "adoption-chat", "--store", "sqlite:" + filepath.Join(root, "chat.db"), "--replay", "testdata/concierge_status_replay.json"}, input, output, io.Discard)
	}
	go func() { finished <- runChat(in) }()
	if _, err := io.WriteString(input, "/adopt "+runID+"\n"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-output.adopted:
	case <-time.After(5 * time.Second):
		t.Fatal("chat did not adopt run")
	}
	close(reportReady)
	if wantPhase != "review" {
		select {
		case title := <-integrated:
			want := fmt.Sprintf("a %s r%d", wantPhase, wantRound)
			if title != want {
				t.Fatalf("started %q, want %s", title, want)
			}
			prompt := <-prompts
			if !strings.Contains(prompt, "Preserve zero-copy packet handling.") {
				t.Fatalf("phase lost host mission: %s", prompt)
			}
			if wantPhase == "integrate" && (!strings.Contains(prompt, "target=release") || !strings.Contains(prompt, "Use the repository merge queue.")) {
				t.Fatalf("integrate lost run merge rule: %s", prompt)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("adopted mid-review lane did not advance to " + wantPhase)
		}
	}
	if complete {
		close(completeReport)
		select {
		case <-released:
		case <-time.After(5 * time.Second):
			t.Fatal("completed lane session was not released")
		}
	}
	if complete {
		if _, err := io.WriteString(input, "/release "+runID+"\n"); err != nil {
			t.Fatal(err)
		}
	}
	_ = input.Close()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("chat did not checkpoint")
	}
	if !complete {
		// A new chat must attach to the advanced phase rather than the lease's
		// original mid-review import.
		if err := runChat(strings.NewReader("/release " + runID + "\n")); err != nil {
			t.Fatal(err)
		}
	}
	if len(integrated) != 0 {
		t.Fatal("launched an extra phase terminal")
	}
	b, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var st struct {
		Tasks []struct {
			Sessions       map[string]string
			Phase          string
			NebulaTerminal string `json:"nebula_terminal"`
			HostOnly       string `json:"host_only"`
			Status         string
			Round          int
		}
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	if len(st.Tasks) != 1 || st.Tasks[0].Phase != wantPhase || st.Tasks[0].Round != wantRound || st.Tasks[0].HostOnly != "keep" {
		t.Fatalf("released ledger: %s", b)
	}
	wantStatus := "running"
	if complete {
		wantStatus = "done"
	}
	wantTerminal := "integrate-terminal"
	if wantPhase == "review" {
		wantTerminal = "imported-review"
	}
	if st.Tasks[0].Status != wantStatus || st.Tasks[0].NebulaTerminal != wantTerminal || st.Tasks[0].Sessions["review"] != "nebula:old-review" || st.Tasks[0].Sessions["review2"] != "nebula:imported-review" {
		t.Fatalf("lost handback state: %s", b)
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

func TestAdoptedPRProbeResolvesRunRepoDirAndMergeSHA(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	const runID = "20261114-trial-m1"
	runDir := filepath.Join(root, ".swarm", runID)
	if err := os.MkdirAll(runDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(runDir, "state.json"), `{"config":{"goal":"trial m1","repo":"sycamore-labs/nebula"},"tasks":[]}`)
	writeFile(t, filepath.Join(runDir, "EVENTS"), "")
	writeFile(t, filepath.Join(runDir, ".repo-slug"), "sycamore-labs/nebula\n")

	clk := clock.NewRealClock()
	st := store.NewMemoryStore(clk)
	defer st.Close()
	if err := st.Runs().Create(t.Context(), store.Run{ID: runID, Repository: "sycamore-labs/nebula", Objective: "trial m1"}); err != nil {
		t.Fatal(err)
	}

	var gotRepo, gotDir, gotTarget string
	h := hostSwarms{
		importer: adopt.Importer{Roots: []string{filepath.Join(root, ".swarm")}},
		probe: chatui.HostProbe{
			PR: func(_ context.Context, repo, dir, number string) (chatui.PRStatus, error) {
				gotRepo, gotDir, gotTarget = repo, dir, number
				return chatui.PRStatus{Number: 5705, State: "MERGED", MergeSHA: "merged-oid-5705"}, nil
			},
		},
	}
	probe := adoptedPRProbe(h, st)
	ln := lane.Lane{
		ID: "m2-default", ItemID: "m2-default", RunID: runID,
		Status: lane.StatusRunning, Phase: "review", Round: 1, PR: 5705,
		Branch: "swarm-lane/trial-m1/m2-default",
	}
	if err := st.Lanes().Upsert(t.Context(), ln); err != nil {
		t.Fatal(err)
	}
	settled, state, err := controller.ReconcileLanePRState(t.Context(), probe, st, nil, clk.Now(), runID, controller.ActorController, "ev-1", &ln)
	if err != nil || !settled || state != "MERGED" {
		t.Fatalf("ReconcileLanePRState = (%v, %q, %v), want (true, MERGED, nil)", settled, state, err)
	}
	if gotRepo != "sycamore-labs/nebula" || gotDir != runDir || gotTarget != "5705" {
		t.Fatalf("probe called with repo=%q dir=%q target=%q, want sycamore-labs/nebula, %q, 5705", gotRepo, gotDir, gotTarget, runDir)
	}
	if ln.Status != lane.StatusDone || ln.MergeSHA != "merged-oid-5705" {
		t.Fatalf("lane after reconcile = %+v, want StatusDone and MergeSHA=merged-oid-5705", ln)
	}
}
