package adopt

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/recipe"
	"github.com/bazelment/yoloswe/orca/store"
)

const testRun = "20261113-000000-host"

// TestDryRunDirFixtures is deliberately built from scrubbed ledger shapes. It
// exercises both host formats without reading any real ~/.swarm or checkout
// run, and proves the preview does not touch files, store rows, or adapters.
func TestDryRunDirFixtures(t *testing.T) {
	t.Parallel()
	//nolint:govet // table keeps each scrubbed run fixture and expected verdict together.
	fixtures := []struct {
		name, format, verdict string
		make                  func(*testing.T) string
	}{
		{"subagent no-pr hold", FormatSubagentSwarm, "JUDGES: lane-hold:judge", fixtureSubagentNoPRHold},
		{"subagent pr", FormatSubagentSwarm, "JUDGES: lane-pr:judge", fixtureSubagentPR},
		{"nebula active", FormatNebulaSwarm, "SAFE", fixtureNebulaActive},
		{"nebula closed", FormatNebulaSwarm, "JUDGES: lane-closed:judge", fixtureNebulaClosed},
	}
	for _, tc := range fixtures {
		t.Run(tc.name, func(t *testing.T) {
			dir := tc.make(t)
			beforeFiles := hashRunFiles(t, dir)
			clk := clock.NewFakeClock(time.Date(2026, 11, 15, 5, 0, 0, 0, time.UTC))
			st := store.NewMemoryStore(clk)
			t.Cleanup(func() { _ = st.Close() })
			if err := st.Runs().Create(t.Context(), store.Run{ID: "store-row", Objective: "must remain"}); err != nil {
				t.Fatal(err)
			}
			beforeRows, err := st.Runs().List(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			label := &dryRunMutationLabeler{t: t}
			watcher := &dryRunMutationWatcher{t: t}
			svc := &Service{Labeler: label, Watcher: watcher, Driver: dryRunMutationDriver{t: t}, Importer: Importer{Roots: []string{t.TempDir()}}}
			result, err := svc.DryRunDir(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			if result.Run.Format != tc.format {
				t.Fatalf("format = %q, want %q", result.Run.Format, tc.format)
			}
			if result.Verdict != tc.verdict {
				t.Fatalf("verdict = %q, want %q; result=%+v", result.Verdict, tc.verdict, result)
			}
			if len(result.Lanes) != 1 {
				t.Fatalf("lanes = %+v, want one lane", result.Lanes)
			}
			ln := result.Lanes[0]
			if ln.Phase == "" || ln.Status == "" || len(ln.Evidence) == 0 || ln.Move == "" || ln.Reason == "" {
				t.Fatalf("lane preview lacks phase/status/evidence/move/reason: %+v", ln)
			}
			afterFiles := hashRunFiles(t, dir)
			if !reflect.DeepEqual(afterFiles, beforeFiles) {
				t.Fatalf("run files changed:\n before=%v\n after=%v", beforeFiles, afterFiles)
			}
			afterRows, err := st.Runs().List(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(afterRows, beforeRows) {
				t.Fatalf("store rows changed:\n before=%v\n after=%v", beforeRows, afterRows)
			}
		})
	}
}

func TestDryRunNeedsInputUsesHostAnswerAndLaneQuestion(t *testing.T) {
	t.Parallel()
	deadline := "2030-01-01T13:00:00Z"
	tests := []struct {
		name, waiting, deadline, marker string
		wantMove, wantReason            string
	}{
		{name: "answered wait", waiting: "chaos-24h-final", deadline: deadline, marker: "handled-0251", wantMove: "wait", wantReason: "waiting on chaos-24h-final"},
		{name: "marker-only wait is bounded", marker: "answered-0251", wantMove: "wait", wantReason: "waiting on lane progress"},
		{name: "unanswered lane question", wantMove: "judge", wantReason: "lane question", marker: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.MkdirAll(filepath.Join(dir, "reports"), 0o755); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(dir, "state.json"), fmt.Sprintf(`{"config":{"goal":"fixture","phases":[{"name":"swe"}]},"tasks":[{"id":"lane","phase":"swe","status":"running","round":1,"waiting_on":%q,"deadline":%q}]}`, tc.waiting, tc.deadline))
			write(t, filepath.Join(dir, "OBJECTIVE.md"), "# Objective\nfixture\n")
			report := filepath.Join(dir, "lane.swe.md")
			write(t, report, "Outcome: needs-input\nAsk: Should the host provide the final chaos result?\n")
			if tc.marker != "" {
				marker := filepath.Join(dir, "lane.swe.nebula-attn."+tc.marker)
				write(t, marker, "answered")
				old := time.Now().Add(-time.Minute)
				if err := os.Chtimes(report, old, old); err != nil {
					t.Fatal(err)
				}
			}
			result, err := (&Service{}).DryRunDir(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Lanes) != 1 || result.Lanes[0].Move != tc.wantMove || !strings.Contains(result.Lanes[0].Reason, tc.wantReason) {
				t.Fatalf("preview = %+v, want move=%q reason containing %q", result.Lanes, tc.wantMove, tc.wantReason)
			}
			if tc.marker == "" && !strings.Contains(result.Lanes[0].Reason, "Should the host provide the final chaos result?") {
				t.Fatalf("unanswered reason = %q, want quoted question", result.Lanes[0].Reason)
			}
		})
	}
}

func TestMarkerOnlyHostWaitHasDeadline(t *testing.T) {
	t.Parallel()
	now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
	ln := Lane{Report: "Outcome: needs-input\nAsk: wait for host", AnsweredMarker: true}
	waitingOn, deadline := ln.expectedHostWait(now)
	if waitingOn != "lane progress" || !deadline.Equal(now.Add(markerOnlyHostWaitTTL)) {
		t.Fatalf("marker-only wait = %q until %s, want lane progress until %s", waitingOn, deadline, now.Add(markerOnlyHostWaitTTL))
	}
}

func TestEscalationFallbackKeepsOneOpenAskPerLane(t *testing.T) {
	t.Parallel()
	clk := clock.NewFakeClock(time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC))
	st := store.NewMemoryStore(clk)
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Lanes().Upsert(t.Context(), lane.Lane{RunID: "run", ID: "lane", Status: lane.StatusBlocked, Round: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Decisions().Append(t.Context(), store.Decision{RunID: "run", LaneID: "lane", Tool: controller.ToolLaneEscalate, Reason: "needs owner", Seq: 1}); err != nil {
		t.Fatal(err)
	}
	ctrls := NewControllers(t.Context(), st, lane.NewFakeDriver(), clk, nil)
	events := []store.Event{{RunID: "run", LaneID: "lane"}}
	if err := ctrls.askOwnerOnEscalation(t.Context(), "run", events); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Decisions().Append(t.Context(), store.Decision{RunID: "run", LaneID: "lane", Tool: controller.ToolLaneEscalate, Reason: "needs owner again", Seq: 2}); err != nil {
		t.Fatal(err)
	}
	if err := ctrls.askOwnerOnEscalation(t.Context(), "run", events); err != nil {
		t.Fatal(err)
	}
	dirs, err := st.Directives().List(t.Context(), store.DirectiveFilter{RunID: "run", ActiveOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(dirs) != 1 || dirs[0].Scope != "lane" || !strings.HasPrefix(dirs[0].Ref, ownerAskRef) {
		t.Fatalf("open fallback asks = %+v, want one owner ask for lane", dirs)
	}
}

func TestDryRunIntegrationEvidenceTable(t *testing.T) {
	t.Parallel()
	//nolint:govet // compact table readability is more useful than field packing in a test.
	type fixture struct {
		name, event, target string
		status              string
		pr                  int
		blocked             bool
		wantMove            string
		wantReason          string
	}
	fixtures := []fixture{
		{name: "historical merged event", event: "historical", target: "main", wantMove: "none"},
		{name: "lane head ancestor", event: "ancestor", target: "main", wantMove: "none"},
		{name: "squash subject", event: "squash", target: "main", pr: 42, wantMove: "none"},
		{name: "feature target", event: "feature", target: "feat/run", wantMove: "none"},
		{name: "missing target evidence", event: "missing", target: "main", wantMove: "judge", wantReason: "integration gate:"},
		{name: "fetch failure", event: "missing", target: "does-not-exist", wantMove: "judge", wantReason: "integration unknown: fetch target does-not-exist failed"},
		{name: "fetch failure judges mid-pipeline", event: "missing", target: "does-not-exist", status: "running", wantMove: "judge", wantReason: "integration unknown: fetch target does-not-exist failed"},
		{name: "mid-pipeline lane advances", event: "missing", target: "main", status: "running", wantMove: "advance"},
		{name: "blocked report", event: "blocked", target: "main", blocked: true, wantMove: "judge", wantReason: `lane question: "credentials are unavailable"`},
	}
	for _, tc := range fixtures {
		t.Run(tc.name, func(t *testing.T) {
			runDir, _, laneHead := integrationFixture(t, tc.event, tc.target, tc.pr, tc.blocked, tc.status)
			result, err := (&Service{}).DryRunDir(t.Context(), runDir)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Lanes) != 1 {
				t.Fatalf("lanes=%+v", result.Lanes)
			}
			got := result.Lanes[0]
			t.Logf("dry-run %s: move=%s reason=%s evidence=%v head=%s", tc.name, got.Move, got.Reason, got.Evidence, laneHead)
			if got.Move != tc.wantMove {
				t.Fatalf("move=%q, want %q; result=%+v", got.Move, tc.wantMove, result)
			}
			if tc.wantReason != "" && got.Reason != tc.wantReason && !strings.Contains(got.Reason, tc.wantReason) {
				t.Fatalf("reason=%q, want %q; result=%+v", got.Reason, tc.wantReason, result)
			}
			if tc.blocked {
				adopted, err := Read(runDir)
				if err != nil {
					t.Fatal(err)
				}
				_, lanes, _, err := controllerState(adopted)
				if err != nil {
					t.Fatal(err)
				}
				if len(lanes) != 1 || lanes[0].Status != lane.StatusBlocked || lanes[0].HostHoldReason != "credentials are unavailable" {
					t.Fatalf("blocked report imported as %+v; want orchestrator hold with blocker text", lanes)
				}
			}
		})
	}
}

//nolint:govet // compact table keeps each synthetic ledger shape readable.
type g52Fixture struct {
	name, event, mode, status, report, repoPath, url string
	pr                                               int
	wantMove, wantReason                             string
}

func TestDryRunIntegrationEvidenceG52Table(t *testing.T) {
	t.Parallel()
	root, bare, other, laneHead := g52RemoteFixture(t)
	mirror := filepath.Join(root, "mirror")
	urlTemplate := "file://" + bare
	fixtures := []g52Fixture{
		{name: "a squash merged to main with feature target", pr: 41, report: "Outcome: done\n", wantMove: "none"},
		{name: "b squash merged to feature without checkout", pr: 99, report: "Outcome: done\n", wantMove: "none"},
		{name: "c recorded checkout remains byte identical", repoPath: other, pr: 41, report: "Outcome: done\n", wantMove: "none"},
		{name: "d analysis blocked report is finished", mode: "analysis", status: "done", report: "Outcome: blocked\nReason: report is blocked\n", wantMove: "none", wantReason: "analysis lane finished; no PR to integrate"},
		{name: "e host note with matching squash", event: "note7", report: "Outcome: done\n", wantMove: "none"},
		{name: "f host note without matching squash", event: "note700", report: "Outcome: done\n", wantMove: "judge", wantReason: "integration gate:"},
		{name: "g open decision does not nominate", event: "open8", report: "Outcome: done\n", wantMove: "judge", wantReason: "integration gate:"},
		{name: "h orch merge event", event: "orch-merge", report: "Outcome: done\n", wantMove: "none"},
		{name: "i host blocked open PR waits", status: "blocked", event: "open", pr: 41, report: "Outcome: done\n", wantMove: "wait", wantReason: "host-held: blocked"},
		{name: "j unreachable remote is judged", report: "Outcome: done\n", url: "file://" + filepath.Join(root, "missing.git"), wantMove: "judge", wantReason: "integration unknown: fetch target feat/x failed"},
	}
	for _, tc := range fixtures {
		t.Run(tc.name, func(t *testing.T) {
			runDir := g52RunFixture(t, root, tc, laneHead)
			beforeRefs, beforeHead := "", ""
			if tc.repoPath != "" {
				beforeRefs = gitTest(t, tc.repoPath, "for-each-ref", "--format=%(refname)=%(objectname)")
				beforeHead = gitTest(t, tc.repoPath, "rev-parse", "HEAD")
			}
			svc := &Service{Mirror: GitMirror{CacheDir: mirror, URLTemplate: urlTemplate}}
			if tc.url != "" {
				svc.Mirror.URLTemplate = tc.url
			}
			result, err := svc.DryRunDir(t.Context(), runDir)
			if err != nil {
				t.Fatal(err)
			}
			got := result.Lanes[0]
			t.Logf("dry-run %s: move=%s reason=%s evidence=%v", tc.name, got.Move, got.Reason, got.Evidence)
			if got.Move != tc.wantMove {
				t.Fatalf("move=%q, want %q; result=%+v", got.Move, tc.wantMove, result)
			}
			if tc.wantReason != "" && !strings.Contains(got.Reason, tc.wantReason) {
				t.Fatalf("reason=%q, want substring %q", got.Reason, tc.wantReason)
			}
			if tc.repoPath != "" {
				afterRefs := gitTest(t, tc.repoPath, "for-each-ref", "--format=%(refname)=%(objectname)")
				afterHead := gitTest(t, tc.repoPath, "rev-parse", "HEAD")
				if beforeRefs != afterRefs || beforeHead != afterHead {
					t.Fatalf("recorded checkout changed: refs %q -> %q, HEAD %q -> %q", beforeRefs, afterRefs, beforeHead, afterHead)
				}
			}
			if tc.mode == "analysis" {
				adopted, err := Read(runDir)
				if err != nil {
					t.Fatal(err)
				}
				_, lanes, _, err := controllerState(adopted)
				if err != nil {
					t.Fatal(err)
				}
				if len(lanes) != 1 || lanes[0].Status != lane.StatusDone {
					t.Fatalf("analysis lane imported as %+v, want done", lanes)
				}
			}
		})
	}
}

func TestEnsureGitMirrorCreatesCacheAndKeepsInvalidSlugContained(t *testing.T) {
	t.Parallel()
	root := filepath.Join(t.TempDir(), "nested", "cache")
	r := &Run{Repo: "../escape"}
	path, err := ensureGitMirror(t.Context(), r, GitMirror{CacheDir: root}, "file:///tmp/remote.git")
	if err != nil {
		t.Fatal(err)
	}
	cleanRoot := filepath.Clean(root) + string(os.PathSeparator)
	if !strings.HasPrefix(filepath.Clean(path), cleanRoot) {
		t.Fatalf("mirror path %q escaped cache root %q", path, root)
	}
	if _, err := os.Stat(filepath.Join(path, "HEAD")); err != nil {
		t.Fatalf("mirror was not initialized: %v", err)
	}
}

func g52RemoteFixture(t *testing.T) (root, bare, other, laneHead string) {
	t.Helper()
	root = t.TempDir()
	bare = filepath.Join(root, "remote.git")
	seed := filepath.Join(root, "seed")
	gitTest(t, root, "init", "--bare", bare)
	gitTest(t, root, "init", seed)
	gitTest(t, seed, "config", "user.email", "orca-test@example.invalid")
	gitTest(t, seed, "config", "user.name", "orca test")
	gitTest(t, seed, "remote", "add", "origin", bare)
	write(t, filepath.Join(seed, "base.txt"), "base\n")
	gitTest(t, seed, "add", ".")
	gitTest(t, seed, "commit", "-m", "base")
	gitTest(t, seed, "branch", "-M", "main")
	gitTest(t, seed, "push", "-u", "origin", "main")
	base := strings.TrimSpace(gitTest(t, seed, "rev-parse", "HEAD"))
	gitTest(t, seed, "checkout", "-B", "lane-41", base)
	write(t, filepath.Join(seed, "lane.txt"), "lane\n")
	gitTest(t, seed, "add", ".")
	gitTest(t, seed, "commit", "-m", "lane work")
	laneHead = strings.TrimSpace(gitTest(t, seed, "rev-parse", "HEAD"))
	gitTest(t, seed, "checkout", "main")
	write(t, filepath.Join(seed, "squash.txt"), "squash\n")
	gitTest(t, seed, "add", ".")
	gitTest(t, seed, "commit", "-m", "lane work (#41)")
	write(t, filepath.Join(seed, "seven.txt"), "seven\n")
	gitTest(t, seed, "add", ".")
	gitTest(t, seed, "commit", "-m", "x (#7)")
	write(t, filepath.Join(seed, "eight.txt"), "eight\n")
	gitTest(t, seed, "add", ".")
	gitTest(t, seed, "commit", "-m", "x (#8)")
	gitTest(t, seed, "push", "origin", "main")
	gitTest(t, seed, "checkout", "-B", "feat/x", base)
	write(t, filepath.Join(seed, "feature.txt"), "feature\n")
	gitTest(t, seed, "add", ".")
	gitTest(t, seed, "commit", "-m", "lane work (#99)")
	gitTest(t, seed, "push", "-u", "origin", "feat/x")
	gitTest(t, root, "--git-dir", bare, "symbolic-ref", "HEAD", "refs/heads/main")
	other = filepath.Join(root, "other")
	gitTest(t, root, "clone", bare, other)
	return root, bare, other, laneHead
}

func g52RunFixture(t *testing.T, root string, tc g52Fixture, laneHead string) string {
	t.Helper()
	dir := filepath.Join(root, "run-"+strings.ReplaceAll(strings.ToLower(tc.name), " ", "-"))
	if err := os.MkdirAll(filepath.Join(dir, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	status := tc.status
	if status == "" {
		status = "done"
	}
	pr := ""
	if tc.pr > 0 {
		pr = fmt.Sprintf(",\"pr\":%d", tc.pr)
	}
	mode := ""
	if tc.mode != "" {
		mode = fmt.Sprintf(",\"mode\":%q", tc.mode)
	}
	repo := "\"repo-uuid\""
	if tc.repoPath != "" {
		repo = fmt.Sprintf("%q", tc.repoPath)
	}
	head := fmt.Sprintf(",\"pr_head\":%q", laneHead)
	if tc.mode == "analysis" || tc.event == "note7" || tc.event == "note700" || tc.event == "open8" || tc.event == "orch-merge" {
		head = ""
	}
	write(t, filepath.Join(dir, ".repo-slug"), "acme/widgets\n")
	write(t, filepath.Join(dir, "state.json"), fmt.Sprintf(`{"config":{"goal":"G52 fixture","repo":%s,"target":"feat/x","base":"feat/x","orchestrator_session_id":"fixture","phases":[{"name":"review"}]},"tasks":[{"id":"lane-1","phase":"review","status":%q%s%s%s}]}`, repo, status, mode, head, pr))
	report := tc.report
	if report == "" {
		report = "Outcome: done\n"
	}
	write(t, filepath.Join(dir, "reports", "lane-1.review.r1.md"), report)
	detail := "started review"
	kind := "started"
	switch tc.event {
	case "note7":
		kind, detail = "orch-note", "#7 MERGED to main abc1234"
	case "note700":
		kind, detail = "orch-note", "#700 merged to main abc1234"
	case "open8":
		kind, detail = "decision", "own lane #8: PR is OPEN, not merged: hold"
	case "orch-merge":
		kind, detail = "orch-merge", "#5367 pinned 0b5de6f5 (host merge)"
	case "open":
		kind, detail = "decision", "PR is OPEN"
	}
	write(t, filepath.Join(dir, "EVENTS"), "2026-11-16T02:00:00Z\tlane-1\t"+kind+"\t"+detail+"\n")
	return dir
}

func integrationFixture(t *testing.T, kind, target string, pr int, blocked bool, status ...string) (runDir, repo, laneHead string) {
	t.Helper()
	root := t.TempDir()
	bare := filepath.Join(root, "target.git")
	work := filepath.Join(root, "work")
	gitTest(t, root, "init", "--bare", bare)
	gitTest(t, root, "init", work)
	gitTest(t, work, "config", "user.email", "orca-test@example.invalid")
	gitTest(t, work, "config", "user.name", "orca test")
	gitTest(t, work, "remote", "add", "origin", bare)
	write(t, filepath.Join(work, "base.txt"), "base\n")
	gitTest(t, work, "add", ".")
	gitTest(t, work, "commit", "-m", "base")
	gitTest(t, work, "branch", "-M", "main")
	gitTest(t, work, "push", "-u", "origin", "main")
	gitTest(t, root, "--git-dir", bare, "symbolic-ref", "HEAD", "refs/heads/main")
	base := strings.TrimSpace(gitTest(t, work, "rev-parse", "HEAD"))
	gitTest(t, work, "checkout", "-B", "lane-1", base)
	write(t, filepath.Join(work, "lane.txt"), kind+"\n")
	gitTest(t, work, "add", ".")
	subject := "lane work"
	if kind == "squash" {
		subject = "lane work (#42)"
	}
	gitTest(t, work, "commit", "-m", subject)
	laneHead = strings.TrimSpace(gitTest(t, work, "rev-parse", "HEAD"))
	switch kind {
	case "ancestor":
		gitTest(t, work, "checkout", "main")
		gitTest(t, work, "merge", "--ff-only", "lane-1")
		gitTest(t, work, "push", "origin", "main")
	case "squash":
		gitTest(t, work, "checkout", "main")
		write(t, filepath.Join(work, "squash.txt"), "squash\n")
		gitTest(t, work, "add", ".")
		gitTest(t, work, "commit", "-m", subject)
		gitTest(t, work, "push", "origin", "main")
	case "feature":
		gitTest(t, work, "checkout", "-B", target, base)
		gitTest(t, work, "merge", "--ff-only", "lane-1")
		gitTest(t, work, "push", "origin", target)
	}
	runDir = filepath.Join(root, "run")
	if err := os.MkdirAll(filepath.Join(runDir, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	prJSON := ""
	prLine := ""
	if pr > 0 {
		prJSON = fmt.Sprintf(",\"pr\":%d", pr)
		prLine = fmt.Sprintf("PR: %d\n", pr)
	}
	laneStatus := "done"
	if len(status) > 0 && status[0] != "" {
		laneStatus = status[0]
	}
	write(t, filepath.Join(runDir, "state.json"), fmt.Sprintf(`{"config":{"goal":"integration fixture","repo":%q,"target":%q,"orchestrator_session_id":"fixture","phases":[{"name":"review"}]},"tasks":[{"id":"lane-1","phase":"review","status":%q,"round":1,"head":%q%s}]}`, work, target, laneStatus, laneHead, prJSON))
	report := fmt.Sprintf("Outcome: done\nHEAD: %s\n%s", laneHead, prLine)
	if blocked {
		report = "Outcome: blocked\nReason: credentials are unavailable\n"
	}
	write(t, filepath.Join(runDir, "reports", "lane-1.review.r1.md"), report)
	events := ""
	if kind == "historical" {
		events = "2026-11-16T01:00:00Z\tlane-1\tmerged\t#99 old merge\n2026-11-16T02:00:00Z\tlane-1\tstarted\treview\n"
	} else {
		events = "2026-11-16T02:00:00Z\tlane-1\tstarted\treview\n"
	}
	write(t, filepath.Join(runDir, "EVENTS"), events)
	return runDir, work, laneHead
}

func gitTest(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git -C %s %s: %v\n%s", dir, strings.Join(args, " "), err, out)
	}
	return string(out)
}

func TestAdoptDecisionHostHolds(t *testing.T) {
	t.Parallel()
	// These are scrubbed ledger shapes, deliberately created under t.TempDir.
	fixtures := []struct {
		name, status, phase, taskExtra, report, hold string
		wantMove, wantReason                         string
	}{
		{
			name:   "swe-only done open PR is a merge judgment",
			status: "done", phase: "swe", taskExtra: `,"pr":6113`, report: "Outcome: done\nPR: #6113\nHEAD: abc\n",
			wantMove: "judge",
		},
		{
			name:   "blocked host note wins",
			status: "blocked", phase: "swe", taskExtra: `,"blocked_on":"held behind #6113 deploy"`, hold: "held behind #6113 deploy; then rebase, drop n2, re-measure at 80%\n",
			report: "Outcome: done\n", wantMove: "wait", wantReason: "host-held: held behind #6113 deploy",
		},
		{
			name:   "hold advance is orca judgment",
			status: "running", phase: "swe", taskExtra: `,"pr":6113`, hold: "orchestrator integrates by hand; require CI green on exact head\n",
			report: "Outcome: done\nPR: #6113\nHEAD: abc\n", wantMove: "judge",
		},
		{
			name:   "integrated lane is terminal",
			status: "done", phase: "integrate", taskExtra: `,"pr":6113,"merge_sha":"def"`, hold: "orchestrator integrates by hand\n",
			report: "Outcome: done\n", wantMove: "none",
		},
	}
	for _, tc := range fixtures {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), ".shared", "subagent-swarm", "fixture")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"adopt decision","phases":[{"name":"`+tc.phase+`"}]},"tasks":[{"id":"lane","phase":"`+tc.phase+`","status":"`+tc.status+`","round":1`+tc.taskExtra+`}]}`)
			write(t, filepath.Join(dir, "OBJECTIVE.md"), "Goal: adopt decision\n")
			write(t, filepath.Join(dir, "lane."+tc.phase+".md"), tc.report)
			if tc.hold != "" {
				name := "lane.hold"
				if tc.name == "hold advance is orca judgment" || tc.name == "integrated lane is terminal" {
					name = "lane.hold-advance"
				}
				write(t, filepath.Join(dir, name), tc.hold)
			}
			result, err := (&Service{}).DryRunDir(t.Context(), dir)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.Lanes) != 1 {
				t.Fatalf("lanes=%+v", result.Lanes)
			}
			got := result.Lanes[0]
			if got.Move != tc.wantMove {
				t.Fatalf("move=%q, want %q; result=%+v", got.Move, tc.wantMove, result)
			}
			if tc.wantReason != "" && got.Reason != tc.wantReason {
				t.Fatalf("reason=%q, want %q", got.Reason, tc.wantReason)
			}
		})
	}
}

func TestMergeGateEvidenceUsesLaneEvidenceOnly(t *testing.T) {
	t.Parallel()
	r := Run{Objective: "Merge rule: require soak PASS", Context: "The gate is soak PASS.", IntegrateRule: "require soak PASS"}
	ln := Lane{Report: "Outcome: done\n", LastEvent: "completed", Checks: ""}
	if got := mergeGateEvidence(r, ln); got {
		t.Fatal("merge gate evidence was inferred from the gate prose")
	}
	ln.Report = "Outcome: done\nSoak: PASS\n"
	if got := mergeGateEvidence(r, ln); !got {
		t.Fatal("lane-owned soak evidence was not recognized")
	}
}

func TestExplicitHostHoldLine(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		line string
		want bool
	}{
		{"lane hold: behind deploy", true},
		{"lane parked behind deploy", true},
		{"lane blocked on: CI", true},
		{"lane hold: none", false},
		{"lane unblocked after deploy", false},
		{"lane hold-advance contract", false},
	} {
		t.Run(tc.line, func(t *testing.T) {
			if got := explicitHostHoldLine(tc.line); got != tc.want {
				t.Fatalf("explicitHostHoldLine(%q) = %v, want %v", tc.line, got, tc.want)
			}
		})
	}
}

func TestControllerStateIntegratedLaneWinsHostHold(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "state.json"), `{"config":{"phases":[{"name":"swe"}]},"tasks":[{"id":"lane","phase":"swe","status":"running","merge_sha":"merged-sha"}]}`)
	r := Run{ID: "adopted", Dir: dir, Format: FormatSubagentSwarm, Lanes: []Lane{{ID: "lane", Phase: "swe", Status: "running", HostHold: "held behind deploy", MergeSHA: "merged-sha", Integrated: true, PRState: "merged"}}}
	_, lanes, _, err := controllerState(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(lanes) != 1 || lanes[0].Status != lane.StatusDone || lanes[0].HostHoldReason != "" {
		t.Fatalf("integrated held lane = %+v, want terminal without host hold", lanes)
	}
}

type dryRunMutationLabeler struct{ t *testing.T }

func (f *dryRunMutationLabeler) RunOrchestrator(context.Context, string) (string, error) {
	return "claude", nil
}

func (f *dryRunMutationLabeler) SetRunOrchestrator(context.Context, string, string) error {
	f.t.Fatal("dry run called Nebula label mutation")
	return nil
}

type dryRunMutationWatcher struct{ t *testing.T }

func (f *dryRunMutationWatcher) Running(context.Context, string) ([]int, error) { return []int{1}, nil }
func (f *dryRunMutationWatcher) Stop(context.Context, []int) error {
	f.t.Fatal("dry run stopped a host watcher")
	return nil
}
func (f *dryRunMutationWatcher) Start(context.Context, string) error {
	f.t.Fatal("dry run started a host watcher")
	return nil
}

type dryRunMutationDriver struct{ t *testing.T }

func (f dryRunMutationDriver) Start(context.Context, Run) error {
	f.t.Fatal("dry run started a lane driver")
	return nil
}
func (f dryRunMutationDriver) Stop(context.Context, Run) error {
	f.t.Fatal("dry run stopped a lane driver")
	return nil
}

func hashRunFiles(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		sum := sha256.Sum256(data)
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		out[rel] = hex.EncodeToString(sum[:])
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func fixtureSubagentNoPRHold(t *testing.T) string {
	dir := filepath.Join(t.TempDir(), ".shared", "subagent-swarm", "copied-subagent-no-pr")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"hold no-pr","no_pr":true,"phases":[{"name":"swe"}]},"tasks":[{"id":"lane-hold","title":"hold","phase":"swe","status":"running","round":1}]}`)
	write(t, filepath.Join(dir, "OBJECTIVE.md"), "Goal: hold no-pr\nNext: owner judgment\n")
	write(t, filepath.Join(dir, "lane-hold.swe.md"), "Outcome: done\nHEAD: scrubbed\n")
	write(t, filepath.Join(dir, "lane-hold.hold-advance"), "owner must approve the advance\n")
	return dir
}

func fixtureSubagentPR(t *testing.T) string {
	dir := filepath.Join(t.TempDir(), ".shared", "subagent-swarm", "copied-subagent-pr")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"pr review","phases":[{"name":"swe"},{"name":"review"}]},"tasks":[{"id":"lane-pr","title":"pr","phase":"review","status":"running","round":1}]}`)
	write(t, filepath.Join(dir, "OBJECTIVE.md"), "Goal: pr review\nMerge rule: integrate after review\n")
	write(t, filepath.Join(dir, "lane-pr.review.md"), "Outcome: done\nPR: 42\nHEAD: scrubbed\n")
	return dir
}

func fixtureNebulaActive(t *testing.T) string {
	dir := filepath.Join(t.TempDir(), ".swarm", "copied-nebula-active")
	if err := os.MkdirAll(filepath.Join(dir, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"active nebula","orchestrator_session_id":"scrubbed-session","phases":[{"name":"swe"}]},"tasks":[{"id":"lane-active","title":"active","phase":"swe","status":"running","round":1,"session_id":"scrubbed-lane","terminals":{"swe":{"1":"scrubbed-terminal"}},"terminal_state":"working"}]}`)
	write(t, filepath.Join(dir, "OBJECTIVE.md"), "Goal: active nebula\n")
	write(t, filepath.Join(dir, "EVENTS"), "2026-11-15T05:00:00Z\tlane-active\tstarted\tworking\n")
	write(t, filepath.Join(dir, "reports", "lane-active.swe.r1.md"), "Outcome: done\nHEAD: scrubbed\n")
	return dir
}

func fixtureNebulaClosed(t *testing.T) string {
	dir := filepath.Join(t.TempDir(), ".swarm", "copied-nebula-closed")
	if err := os.MkdirAll(filepath.Join(dir, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"closed nebula","orchestrator_session_id":"scrubbed-session","phases":[{"name":"review"}]},"tasks":[{"id":"lane-closed","title":"closed","phase":"review","status":"done","round":1,"terminals":{"review":{"1":"scrubbed-terminal"}}}]}`)
	write(t, filepath.Join(dir, "OBJECTIVE.md"), "Goal: closed nebula\nNext: done\n")
	write(t, filepath.Join(dir, "EVENTS"), "2026-11-15T05:00:00Z\tlane-closed\tcompleted\tdone\n")
	write(t, filepath.Join(dir, "reports", "lane-closed.review.r1.md"), "Outcome: done\nHEAD: scrubbed\n")
	return dir
}

var errInjected = errors.New("injected")

type fakeLabeler struct {
	failSet error
	label   string
	mu      sync.Mutex
}

func (f *fakeLabeler) RunOrchestrator(context.Context, string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.label, nil
}

func (f *fakeLabeler) SetRunOrchestrator(_ context.Context, _ string, label string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failSet != nil {
		return f.failSet
	}
	f.label = label
	return nil
}

// fakeWatcher models one host watcher process. A failing Stop still kills it,
// the harder case for an undo.
type fakeWatcher struct {
	failStop, failStart error
	mu                  sync.Mutex
	running             bool
}

func (f *fakeWatcher) Running(context.Context, string) ([]int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running {
		return []int{4242}, nil
	}
	return nil, nil
}

func (f *fakeWatcher) Stop(context.Context, []int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.running = false
	return f.failStop
}

func (f *fakeWatcher) Start(context.Context, string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failStart != nil {
		return f.failStart
	}
	f.running = true
	return nil
}

type world struct {
	label   *fakeLabeler
	watcher *fakeWatcher
	root    string
	dir     string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, ".shared", "subagent-swarm", testRun)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"host goal","phases":[{"name":"swe"}]},"tasks":[{"id":"a","title":"A","phase":"swe","status":"running","round":1,"sessions":{}}]}`)
	write(t, filepath.Join(dir, "OBJECTIVE.md"), "objective\n")
	// PROGRESS.md is absent: the handover must create and later remove it.
	return &world{root: root, dir: dir, label: &fakeLabeler{label: "claude"}, watcher: &fakeWatcher{running: true}}
}

// service is a fresh chat process: nothing carries over but the holder id.
func (w *world) service() *Service {
	return &Service{
		Importer: Importer{Roots: []string{w.root}},
		Labeler:  w.label,
		Watcher:  w.watcher,
		Holder:   ChatHolder("chat-1"),
		Now:      func() time.Time { return time.Date(2026, 11, 13, 1, 2, 3, 0, time.UTC) },
	}
}

// snapshot is everything the handover may change: every file in the run
// directory byte for byte, the Nebula label and whether a watcher runs.
type snapshot struct {
	files   map[string]string
	label   string
	running bool
}

func (w *world) snapshot(t *testing.T) snapshot {
	t.Helper()
	ents, err := os.ReadDir(w.dir)
	if err != nil {
		t.Fatal(err)
	}
	s := snapshot{files: map[string]string{}, label: w.label.label, running: w.watcher.running}
	for _, e := range ents {
		b, err := os.ReadFile(filepath.Join(w.dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		s.files[e.Name()] = string(b)
	}
	return s
}

func assertSame(t *testing.T, what string, got, want snapshot) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("%s changed state:\n got %+v\nwant %+v", what, got, want)
	}
}

func assertAdopted(t *testing.T, w *world) {
	t.Helper()
	s := w.snapshot(t)
	if s.label != "orca" || s.running {
		t.Fatalf("adopted label=%q watcher running=%v", s.label, s.running)
	}
	for _, n := range []string{"OBJECTIVE.md", "PROGRESS.md"} {
		if !strings.HasPrefix(s.files[n], "ORCHESTRATOR: orca since 2026-11-13T01:02:03Z (orca-chat:chat-1)") {
			t.Fatalf("%s lacks the stand-down line: %q", n, s.files[n])
		}
	}
	if _, ok := s.files[leaseFile]; !ok {
		t.Fatal("lease missing")
	}
}

func TestRestartBetweenAdoptAndRelease(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	before := w.snapshot(t)
	r, err := w.service().Adopt(t.Context(), testRun)
	if err != nil {
		t.Fatal(err)
	}
	if r.ID != testRun || len(r.Lanes) != 1 {
		t.Fatalf("imported %+v", r)
	}
	assertAdopted(t, w)

	// The chat restarts: a new process knows only its holder id.
	restarted := w.service()
	ids, err := restarted.Adopted()
	if err != nil || !reflect.DeepEqual(ids, []string{testRun}) {
		t.Fatalf("Adopted() = %v, %v", ids, err)
	}
	other := w.service()
	other.Holder = ChatHolder("chat-2")
	adopted := w.snapshot(t)
	if _, err := other.Adopt(t.Context(), testRun); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("other holder adopt = %v", err)
	}
	if err := other.Release(t.Context(), testRun); !errors.Is(err, ErrLeaseNotHeld) {
		t.Fatalf("other holder release = %v", err)
	}
	assertSame(t, "other holder", w.snapshot(t), adopted)

	if err := restarted.Release(t.Context(), testRun); err != nil {
		t.Fatal(err)
	}
	assertSame(t, "adopt then release across a restart", w.snapshot(t), before)
}

func TestAdoptFailureAtEachStepLeavesPriorState(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"import", "marker", "label", "watcher"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			s := w.service()
			switch step {
			case "label":
				w.label.failSet = errInjected
			case "watcher":
				w.watcher.failStop = errInjected
			default:
				s.fault = func(got string) error {
					if got == "adopt:"+step {
						return errInjected
					}
					return nil
				}
			}
			before := w.snapshot(t)
			if _, err := s.Adopt(t.Context(), testRun); !errors.Is(err, errInjected) {
				t.Fatalf("adopt = %v, want injected failure", err)
			}
			assertSame(t, "failed adopt at "+step, w.snapshot(t), before)
		})
	}
}

func TestReleaseFailureAtEachStepLeavesAdoptedState(t *testing.T) {
	t.Parallel()
	for _, step := range []string{"import", "marker", "label", "watcher"} {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			original := w.snapshot(t)
			s := w.service()
			if _, err := s.Adopt(t.Context(), testRun); err != nil {
				t.Fatal(err)
			}
			switch step {
			case "label":
				w.label.failSet = errInjected
			case "watcher":
				w.watcher.failStart = errInjected
			default:
				s.fault = func(got string) error {
					if got == "release:"+step {
						return errInjected
					}
					return nil
				}
			}
			adopted := w.snapshot(t)
			if err := s.Release(t.Context(), testRun); !errors.Is(err, errInjected) {
				t.Fatalf("release = %v, want injected failure", err)
			}
			assertSame(t, "failed release at "+step, w.snapshot(t), adopted)

			w.label.failSet, w.watcher.failStart, s.fault = nil, nil, nil
			if err := s.Release(t.Context(), testRun); err != nil {
				t.Fatal(err)
			}
			assertSame(t, "release after the failure", w.snapshot(t), original)
		})
	}
}

// A crash leaves the journal mid-protocol; rerunning the command finishes it.
func TestRerunAfterCrashFinishesHandover(t *testing.T) {
	t.Parallel()
	crashAt := func(s *Service, step string) {
		s.fault = func(got string) error {
			if got == step {
				panic("crash")
			}
			return nil
		}
		defer func() { _ = recover() }()
		if strings.HasPrefix(step, "adopt:") {
			_, _ = s.Adopt(context.Background(), testRun)
		} else {
			_ = s.Release(context.Background(), testRun)
		}
	}
	w := newWorld(t)
	original := w.snapshot(t)
	crashAt(w.service(), "adopt:watcher")
	if _, err := w.service().Adopt(t.Context(), testRun); err != nil {
		t.Fatal(err)
	}
	assertAdopted(t, w)
	crashAt(w.service(), "release:marker")
	if err := w.service().Release(t.Context(), testRun); err != nil {
		t.Fatal(err)
	}
	assertSame(t, "crash and rerun", w.snapshot(t), original)
}

// Legacy bare-chat leases remain owned after restart.
func TestLeaseUnderFormerHolderIsOwn(t *testing.T) {
	t.Parallel()
	for _, reattach := range []bool{false, true} {
		t.Run(fmt.Sprint("reattach=", reattach), func(t *testing.T) {
			t.Parallel()
			w := newWorld(t)
			before := w.snapshot(t)
			legacy := w.service()
			legacy.Holder = ChatHolder("")
			if _, err := legacy.Adopt(t.Context(), testRun); err != nil {
				t.Fatal(err)
			}
			restarted := w.service()
			restarted.Holder, restarted.Former = ChatHolder("front-desk"), []string{ChatHolder("")}
			if ids, err := restarted.Adopted(); err != nil || !reflect.DeepEqual(ids, []string{testRun}) {
				t.Fatalf("Adopted() = %v, %v", ids, err)
			}
			if reattach {
				r, err := restarted.Adopt(t.Context(), testRun)
				if err != nil {
					t.Fatal(err)
				}
				if r.Holder != restarted.Holder {
					t.Fatalf("reattached holder = %q, want %q", r.Holder, restarted.Holder)
				}
				if _, err := legacy.Adopt(t.Context(), testRun); !errors.Is(err, ErrLeaseHeld) {
					t.Fatalf("legacy holder adopt after reattach = %v, want held", err)
				}
			}
			if err := restarted.Release(t.Context(), testRun); err != nil {
				t.Fatal(err)
			}
			assertSame(t, "release of a former holder's lease", w.snapshot(t), before)
		})
	}
}

func TestOldHolderJournalMigratesOnReattach(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	before := w.snapshot(t)
	legacy := w.service()
	legacy.Holder = ChatHolder("")
	if _, err := legacy.Adopt(t.Context(), testRun); err != nil {
		t.Fatal(err)
	}
	if j, err := readJournal(filepath.Join(w.dir, leaseFile)); err != nil || j.Holder != "orca-chat:" {
		t.Fatalf("legacy journal = %+v, err = %v", j, err)
	}

	// Reattach with a stable holder (without needing Former explicitly set):
	// Adopted() and Adopt() migrate the journal and stand-down lines on disk.
	restarted := w.service()
	restarted.Holder = ChatHolder("front-desk")
	r, err := restarted.Adopt(t.Context(), testRun)
	if err != nil {
		t.Fatal(err)
	}
	if r.Holder != "orca-chat:front-desk" {
		t.Fatalf("r.Holder = %q, want orca-chat:front-desk", r.Holder)
	}
	j, err := readJournal(filepath.Join(w.dir, leaseFile))
	if err != nil {
		t.Fatal(err)
	}
	if j.Holder != "orca-chat:front-desk" || j.Import == nil || j.Import.Holder != "orca-chat:front-desk" || !strings.Contains(j.Marker, "(orca-chat:front-desk)") {
		t.Fatalf("migrated journal = %+v, import = %+v", j, j.Import)
	}
	for _, n := range handoverFiles {
		body := readText(filepath.Join(w.dir, n))
		if !strings.HasPrefix(body, "ORCHESTRATOR: orca since 2026-11-13T01:02:03Z (orca-chat:front-desk)") || strings.Contains(body, "(orca-chat:)") {
			t.Fatalf("%s did not migrate stand-down line: %q", n, body)
		}
	}
	if err := restarted.Release(t.Context(), testRun); err != nil {
		t.Fatal(err)
	}
	assertSame(t, "release after old-holder journal migration", w.snapshot(t), before)
}

func TestLeaseHeldByAnotherHolderChangesNothing(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	write(t, filepath.Join(w.dir, leaseFile), `{"holder":"orca-chat:other","state":"adopted"}`+"\n")
	before := w.snapshot(t)
	for _, svc := range []*Service{
		w.service(),
		{
			Importer: Importer{Roots: []string{w.root}},
			Labeler:  w.label,
			Watcher:  w.watcher,
			Holder:   ChatHolder("front-desk"),
			Former:   []string{ChatHolder("")},
			Now:      w.service().Now,
		},
	} {
		if ids, err := svc.Adopted(); err != nil || len(ids) != 0 {
			t.Fatalf("Adopted() for %s = %v, %v; want none", svc.Holder, ids, err)
		}
		if _, err := svc.Adopt(t.Context(), testRun); !errors.Is(err, ErrLeaseHeld) {
			t.Fatalf("adopt for %s = %v", svc.Holder, err)
		}
		if err := svc.Release(t.Context(), testRun); !errors.Is(err, ErrLeaseNotHeld) {
			t.Fatalf("release for %s = %v", svc.Holder, err)
		}
		assertSame(t, "refused foreign lease for "+svc.Holder, w.snapshot(t), before)
	}
}

func TestImportsCurrentRoundOfTwoLaneRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"g","phases":[{"name":"swe"},{"name":"review"}]},"tasks":[
{"id":"a","title":"A","branch":"ba","phase":"swe","status":"running","round":2,"backend":"nebula","sessions":{"swe":"nebula:term-a1","review":"nebula:term-ar1","swe2":"nebula:term-a2"},"nebula_session":"sess-a2","nebula_terminal":"term-a2"},
{"id":"b","title":"B","branch":"bb","phase":"review","status":"running","round":2,"pr":22,"sessions":{"review":"nebula:term-br1"}}]}`)
	base := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	files := []struct {
		name, body string
		age        int
	}{
		{"a.swe.spawn.json", `{"session_id":"sess-a2","terminal_id":"term-a2"}`, 0},
		{"a.review.spawn.json", `{"session_id":"sess-a1","terminal_id":"term-ar1"}`, 2},
		{"a.swe.md", "Outcome: done\nPR: 11\n", 0},
		{"a.swe.md.prev-20261112T000000Z", "Outcome: needs-swe\nPR: 9\n", 3},
		{"b.review.spawn.json", `{"session_id":"sess-b2","terminal_id":"term-b2"}`, 0},
		{"b.swe.spawn.json", `{"session_id":"sess-b1","terminal_id":"term-b1"}`, 3},
		{"b.review.md.prev-20261112T000000Z", "Outcome: needs-swe\nPR: 22\n", 2},
	}
	for _, f := range files {
		p := filepath.Join(dir, f.name)
		write(t, p, f.body)
		mt := base.Add(-time.Duration(f.age) * time.Hour)
		if err := os.Chtimes(p, mt, mt); err != nil {
			t.Fatal(err)
		}
	}
	r, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []Lane{
		{ID: "a", Title: "A", Branch: "ba", Phase: "swe", Status: "running", Round: 2, Session: "sess-a2", Terminal: "term-a2", ReportPath: filepath.Join(dir, "a.swe.md"), PR: "11", Report: "Outcome: done", Next: "next phase: review"},
		{ID: "b", Title: "B", Branch: "bb", Phase: "review", Status: "running", Round: 2, Session: "sess-b2", Terminal: "term-b2", PR: "22", Next: "next: merge"},
	}
	if !reflect.DeepEqual(r.Lanes, want) {
		t.Fatalf("lanes:\n got %+v\nwant %+v", r.Lanes, want)
	}
}

func TestControllerStateRejectsBriefOutsideRunDirectory(t *testing.T) {
	t.Parallel()
	for _, brief := range []string{"../outside.md", "/tmp/outside.md"} {
		t.Run(brief, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "outside.md"), []byte("secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			write(t, filepath.Join(dir, "state.json"), fmt.Sprintf(`{"config":{"goal":"g"},"tasks":[{"id":"a","title":"A","phase":"swe","status":"running","brief":%q}]}`, brief))
			r, err := Read(dir)
			if err != nil {
				t.Fatal(err)
			}
			_, _, _, err = controllerState(r)
			if err == nil || !strings.Contains(err.Error(), "brief path") || !strings.Contains(err.Error(), "run directory") {
				t.Fatalf("controllerState error = %v, want a clear run-directory brief-path error", err)
			}
		})
	}
}

func TestControllerStateRejectsLedgerPathComponents(t *testing.T) {
	t.Parallel()
	for _, task := range []string{`{"id":"../outside","phase":"swe"}`, `{"id":"a","phase":"../outside"}`} {
		t.Run(task, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			write(t, filepath.Join(dir, "state.json"), fmt.Sprintf(`{"config":{"goal":"g"},"tasks":[%s]}`, task))
			r, err := Read(dir)
			if err != nil {
				t.Fatal(err)
			}
			_, _, _, err = controllerState(r)
			if err == nil || !strings.Contains(err.Error(), "single path component") {
				t.Fatalf("controllerState error = %v, want path-component rejection", err)
			}
		})
	}
}

func TestAdoptedReviewRoundCapMatchesDefault(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"g","phases":[{"name":"swe"},{"name":"review"}]},"tasks":[{"id":"a","title":"A","phase":"review","status":"running","round":3}]}`)
	r, err := Read(dir)
	if err != nil {
		t.Fatal(err)
	}
	workflow, lanes, _, err := controllerState(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(lanes) != 1 || lanes[0].Round != 3 {
		t.Fatalf("imported lanes = %+v, want review round 3", lanes)
	}
	mechanical, judgment := workflow.MatchTransitions(recipe.PhaseReview, recipe.OutcomeNeedsSWE, false, lanes[0].Round)
	if mechanical == nil || mechanical.To != recipe.TargetEscalate || len(judgment) != 0 {
		t.Fatalf("review round 3 needs-swe transition = mechanical=%+v judgment=%+v, want escalation", mechanical, judgment)
	}
}

const fakeWatchScript = `#!/bin/bash
if [ "$1" = --status ]; then
  a=""; b=""
  for f in "$2"/.fake-pid-*; do
    [ -e "$f" ] || continue
    pid=${f##*-}
    kill -0 "$pid" 2>/dev/null || continue
    read -r kind owner < "$f"
    if [ "$kind" = bridge ]; then b="$b $pid"; else a="$a $pid"; fi
  done
  echo "armed:${a:- none}"
  echo "bridges:${b:- none}"
  [ -n "$a" ] && exit 0 || exit 1
fi
kind=armed
if [ "$1" = --bridge ]; then kind=bridge; shift; fi
echo "$kind ${WATCH_BRIDGE_OF:-}" > "$1/.fake-pid-$$"
exec sleep 300
`

// TestScriptWatcherHandsOverRealProcesses runs the handover against real
// processes: adopt stops the armed watcher with a signal, release starts a
// bridge owned by the last armed watcher.
func TestScriptWatcherHandsOverRealProcesses(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	script := filepath.Join(t.TempDir(), "watch_lanes.sh")
	write(t, script, fakeWatchScript)
	sw := ScriptWatcher{Script: script, StopTimeout: 5 * time.Second}

	armed := exec.Command("bash", script, w.dir)
	if err := armed.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan struct{})
	go func() { _ = armed.Wait(); close(exited) }()
	t.Cleanup(func() { _ = armed.Process.Kill() })
	write(t, filepath.Join(w.dir, ".watch-owner"), fmt.Sprintf("%d\n", armed.Process.Pid))
	waitFor(t, func() bool { pids, _ := sw.Running(t.Context(), w.dir); return len(pids) == 1 })

	s := w.service()
	s.Watcher = sw
	if _, err := s.Adopt(t.Context(), testRun); err != nil {
		t.Fatal(err)
	}
	select {
	case <-exited:
	case <-time.After(10 * time.Second):
		t.Fatal("armed watcher still running after adopt")
	}
	if err := s.Release(t.Context(), testRun); err != nil {
		t.Fatal(err)
	}
	var bridge []int
	waitFor(t, func() bool { bridge, _ = sw.Running(t.Context(), w.dir); return len(bridge) == 1 })
	t.Cleanup(func() { _ = sw.Stop(context.Background(), bridge) })
	owner, err := os.ReadFile(filepath.Join(w.dir, fmt.Sprintf(".fake-pid-%d", bridge[0])))
	if err != nil {
		t.Fatal(err)
	}
	if want := fmt.Sprintf("bridge %d\n", armed.Process.Pid); string(owner) != want {
		t.Fatalf("bridge identity = %q, want %q", owner, want)
	}
	if err := sw.Stop(t.Context(), bridge); err != nil {
		t.Fatal(err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 10s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func write(t *testing.T, p, s string) {
	t.Helper()
	if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
		t.Fatal(err)
	}
}

type runDriverFuncs struct {
	start func(context.Context, Run) error
	stop  func(context.Context, Run) error
}

func (d runDriverFuncs) Start(ctx context.Context, r Run) error { return d.start(ctx, r) }
func (d runDriverFuncs) Stop(ctx context.Context, r Run) error  { return d.stop(ctx, r) }

func TestFailedReleaseResumesController(t *testing.T) {
	w := newWorld(t)
	svc := w.service()
	starts, stops := 0, 0
	svc.Driver = runDriverFuncs{
		start: func(context.Context, Run) error {
			if w.watcher.running {
				t.Fatal("host watcher still running at controller start")
			}
			starts++
			return nil
		},
		stop: func(context.Context, Run) error { stops++; return nil },
	}
	if _, err := svc.Adopt(t.Context(), testRun); err != nil {
		t.Fatal(err)
	}
	svc.fault = func(step string) error {
		if step == "release:label" {
			return errInjected
		}
		return nil
	}
	if err := svc.Release(t.Context(), testRun); !errors.Is(err, errInjected) {
		t.Fatalf("release error = %v", err)
	}
	if starts != 2 || stops != 1 || w.watcher.running {
		t.Fatalf("handover after failure: starts=%d stops=%d watcher=%v", starts, stops, w.watcher.running)
	}
	svc.fault = nil
	if err := svc.Release(t.Context(), testRun); err != nil {
		t.Fatal(err)
	}
	if stops != 2 || !w.watcher.running {
		t.Fatalf("successful handback: stops=%d watcher=%v", stops, w.watcher.running)
	}
}

func TestLiveAdoptImportsHostWaitAndNamesLaneQuestions(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, outcome     string
		answered, waiting bool
	}{
		{name: "answered with ledger wait", outcome: "needs-input", answered: true, waiting: true},
		{name: "unanswered", outcome: "needs-input"},
		{name: "completed with handled marker", outcome: "done", answered: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			now := time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)
			waitFields := ""
			if tc.waiting {
				waitFields = `,"waiting_on":"chaos-24h-final","deadline":"2030-01-01T13:00:00Z"`
			}
			write(t, filepath.Join(dir, "state.json"), `{"config":{"goal":"fixture","phases":[{"name":"swe"}]},"tasks":[{"id":"lane","title":"Chaos result","phase":"swe","status":"running","round":1`+waitFields+`}]}`)
			write(t, filepath.Join(dir, "OBJECTIVE.md"), "# Objective\nfixture\n")
			write(t, filepath.Join(dir, "lane.swe.md"), "Outcome: "+tc.outcome+"\nAsk: Should the host provide the final chaos result?\n")
			if tc.answered {
				write(t, filepath.Join(dir, "lane.swe.nebula-attn.answered-123"), "answered")
				old := now.Add(-time.Hour)
				if err := os.Chtimes(filepath.Join(dir, "lane.swe.md"), old, old); err != nil {
					t.Fatal(err)
				}
				if err := os.Chtimes(filepath.Join(dir, "lane.swe.nebula-attn.answered-123"), now, now); err != nil {
					t.Fatal(err)
				}
			}
			write(t, filepath.Join(dir, leaseFile), `{"holder":"orca-chat:fixture","state":"adopted","marker":"ORCHESTRATOR: orca since 2030-01-01T12:00:00Z"}`)
			run, err := Read(dir)
			if err != nil {
				t.Fatal(err)
			}
			clk := clock.NewFakeClock(now)
			st := store.NewMemoryStore(clk)
			t.Cleanup(func() { _ = st.Close() })
			ctrls := NewControllers(t.Context(), st, lane.NewFakeDriver(), clk, nil)
			t.Cleanup(func() { _ = ctrls.Close(context.Background()) })
			if err := ctrls.Start(t.Context(), run); err != nil {
				t.Fatal(err)
			}
			events, err := st.Events().List(t.Context(), store.EventFilter{RunID: run.ID})
			if err != nil {
				t.Fatal(err)
			}
			var questions []store.Event
			for _, event := range events {
				if strings.HasPrefix(event.ID, "adopt-") && event.Class == store.EventClassJudgment {
					questions = append(questions, event)
				}
			}
			if tc.outcome == "done" {
				if _, err := st.Directives().Get(t.Context(), run.ID, "wait-adopt-lane"); !errors.Is(err, store.ErrNotFound) {
					t.Fatalf("completed report retained an indefinite host wait: %v", err)
				}
			} else if tc.answered {
				if len(questions) != 0 {
					t.Fatalf("answered report re-judged: %+v", questions)
				}
				wait, err := st.Directives().Get(t.Context(), run.ID, "wait-adopt-lane")
				if err != nil || wait.Text != "waiting on chaos-24h-final" || !wait.ExpiresAt.Equal(now.Add(time.Hour)) {
					t.Fatalf("host wait=%+v err=%v", wait, err)
				}
			} else if len(questions) != 1 || !strings.HasPrefix(questions[0].Summary, "lane question:") || !strings.Contains(questions[0].Summary, `"Should the host provide the final chaos result?"`) {
				t.Fatalf("unanswered report must be a quoted lane question: %+v", questions)
			}
		})
	}
}

func TestAdoptedWaitSuppressesOnlyTheAnsweredReport(t *testing.T) {
	t.Parallel()
	clk := clock.NewFakeClock(time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC))
	st := store.NewMemoryStore(clk)
	t.Cleanup(func() { _ = st.Close() })
	deadline := clk.Now().Add(time.Hour)
	if err := st.Directives().Add(t.Context(), store.Directive{ID: "wait-adopt-lane", RunID: "run", Scope: "lane", Ref: "named-wait:lane", Text: "waiting on chaos-24h-final", ExpiresAt: &deadline, Status: store.DirectiveActive}); err != nil {
		t.Fatal(err)
	}
	const raw = "Outcome: needs-input\nAsk: Is the final chaos result available?"
	old, err := lane.ParsePhaseReport(raw)
	if err != nil {
		t.Fatal(err)
	}
	base := lane.NewFakeDriver()
	base.SetReport("terminal", *old)
	drv := &adoptedRunDriver{LaneDriver: base, store: st, clock: clk, run: Run{ID: "run", Lanes: []Lane{{ID: "lane", reportText: raw}}}}
	if rep, err := drv.ReadReport(t.Context(), "run", "lane", "terminal"); err != nil || rep != nil {
		t.Fatalf("answered report replayed: %+v err=%v", rep, err)
	}
	watch := drv.Watch(t.Context(), "run")
	base.Emit(lane.Event{Kind: lane.EventPhaseReported, RunID: "run", LaneID: "lane", Report: old})
	fresh := &lane.PhaseReport{Outcome: lane.OutcomeNeedsInput, Raw: "Outcome: needs-input\nAsk: A new host question?"}
	base.Emit(lane.Event{Kind: lane.EventPhaseReported, RunID: "run", LaneID: "lane", Report: fresh})
	select {
	case event := <-watch:
		if event.Report != fresh {
			t.Fatalf("expected fresh report, got %+v", event)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("fresh report was not forwarded")
	}
	clk.Advance(2 * time.Hour)
	if rep, err := drv.ReadReport(t.Context(), "run", "lane", "terminal"); err != nil || rep == nil {
		t.Fatalf("expired wait swallowed report: %+v err=%v", rep, err)
	}
}
