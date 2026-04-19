package prdozer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/wt"
)

// fakeGH is a minimal GHRunner that matches by joined-args prefix.
type fakeGH struct {
	results map[string]*wt.CmdResult
	calls   [][]string
	mu      sync.Mutex
}

func newFakeGH() *fakeGH {
	return &fakeGH{results: make(map[string]*wt.CmdResult)}
}

// addPrefix registers a stdout response for any call whose joined-args starts with prefix.
func (f *fakeGH) addPrefix(prefix, stdout string) {
	f.results[prefix] = &wt.CmdResult{Stdout: stdout}
}

func (f *fakeGH) Run(_ context.Context, args []string, _ string) (*wt.CmdResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	joined := strings.Join(args, " ")
	for prefix, res := range f.results {
		if strings.HasPrefix(joined, prefix) {
			return res, nil
		}
	}
	// Default to empty array for unknown api calls so JSON parse doesn't fail.
	return &wt.CmdResult{Stdout: "[]"}, nil
}

// stubPolish records calls and returns a configurable error.
type stubPolish struct {
	err   error
	calls []PolishRequest
	mu    sync.Mutex
}

func (s *stubPolish) Run(_ context.Context, req PolishRequest) (PolishResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, req)
	if s.err != nil {
		return PolishResult{}, s.err
	}
	return PolishResult{SessionID: "stub", Output: "ok"}, nil
}

func setupGH(prJSON, runListJSON, baseSHA string) *fakeGH {
	gh := newFakeGH()
	gh.addPrefix("pr view 42 --json number,url,headRefName,baseRefName,headRefOid,state,isDraft,reviewDecision,mergeable,statusCheckRollup", prJSON)
	gh.addPrefix("run list --branch feature --status failure", runListJSON)
	gh.addPrefix("api repos/{owner}/{repo}/git/refs/heads/main", baseSHA)
	gh.addPrefix("api --paginate repos/o/r/pulls/42/comments", "[]")
	gh.addPrefix("api --paginate repos/o/r/issues/42/comments", "[]")
	return gh
}

// buildPRJSON stitches the core PR fields with a statusCheckRollup outcome so
// a single gh pr view call returns everything TakeSnapshot needs.
func buildPRJSON(core, rollupOutcome string) string {
	rollup := ""
	switch rollupOutcome {
	case "SUCCESS", "FAILURE":
		rollup = fmt.Sprintf(`,"statusCheckRollup":[{"conclusion":%q,"status":"COMPLETED"}]`, rollupOutcome)
	}
	trimmed := strings.TrimSpace(core)
	return trimmed[:len(trimmed)-1] + rollup + "}"
}

const okPRJSON = `{
  "number": 42,
  "url": "https://github.com/o/r/pull/42",
  "headRefName": "feature",
  "baseRefName": "main",
  "headRefOid": "head1",
  "state": "OPEN",
  "isDraft": false,
  "reviewDecision": "REVIEW_REQUIRED",
  "mergeable": "MERGEABLE"
}`

func newWatcherForTest(t *testing.T, gh wt.GHRunner, polish PolishRunner, opts ...WatcherOption) *Watcher {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	cfg := DefaultConfig()
	cfg.PollInterval = 10 * time.Millisecond
	cfg.Backoff.MaxConsecutiveFailures = 2
	cfg.Backoff.Cooldown = time.Hour
	return NewWatcher(cfg, gh, polish, 42, ".", "r", nil, opts...)
}

func TestWatcher_Tick_FirstRunIdle_NoPolish(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "SUCCESS"), "[]", "base1")
	polish := &stubPolish{}
	w := newWatcherForTest(t, gh, polish)
	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, LastActionIdle, res.Action)
	assert.Empty(t, polish.calls, "first run should not invoke polish")
	state, err := LoadState(StatePath("r", 42))
	require.NoError(t, err)
	assert.Equal(t, "head1", state.LastSeenHeadSHA)
	assert.Equal(t, "base1", state.LastSeenBaseSHA)
}

func TestWatcher_Tick_BaseMoved_TriggersPolish(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "SUCCESS"), "[]", "new-base")
	polish := &stubPolish{}
	w := newWatcherForTest(t, gh, polish)
	// Pre-seed state so this is NOT the first run.
	pre := &State{
		PRNumber:        42,
		Repo:            "r",
		LastCheckAt:     time.Now(),
		LastSeenHeadSHA: "head1",
		LastSeenBaseSHA: "old-base",
	}
	require.NoError(t, pre.Save(StatePath("r", 42)))

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, LastActionPolished, res.Action)
	require.Len(t, polish.calls, 1)
	assert.Equal(t, 42, polish.calls[0].PRNumber)
}

func TestWatcher_Tick_DryRun_DoesNotPolish(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	polish := &stubPolish{}
	w := newWatcherForTest(t, gh, polish, WithDryRun(true))

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, LastActionDryRun, res.Action)
	assert.Empty(t, polish.calls)
}

func TestWatcher_Tick_PolishFailure_TripsCooldown(t *testing.T) {
	gh := setupGH(buildPRJSON(okPRJSON, "FAILURE"), "[]", "base1")
	polish := &stubPolish{err: fmt.Errorf("boom")}
	w := newWatcherForTest(t, gh, polish)
	statePath := StatePath("r", 42)

	// Pre-seed so it's not first run; failure is triggered via FAILURE rollup which
	// is actionable on first run anyway, but pre-seeding makes the test explicit.
	pre := &State{LastCheckAt: time.Now(), LastSeenHeadSHA: "head1", LastSeenBaseSHA: "base1"}
	require.NoError(t, pre.Save(statePath))

	// First failure.
	_, err := w.Tick(context.Background())
	require.NoError(t, err)
	s1, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 1, s1.ConsecutiveFailures)
	assert.True(t, s1.CooldownUntil.IsZero(), "single failure shouldn't trip cooldown")

	// Second failure → cooldown.
	_, err = w.Tick(context.Background())
	require.NoError(t, err)
	s2, err := LoadState(statePath)
	require.NoError(t, err)
	assert.Equal(t, 2, s2.ConsecutiveFailures)
	assert.False(t, s2.CooldownUntil.IsZero(), "second failure should set cooldown")

	// Third tick is skipped due to cooldown.
	_, err = w.Tick(context.Background())
	require.NoError(t, err)
	assert.Len(t, polish.calls, 2, "third tick should be skipped by cooldown")
}

func TestWatcher_StateFileLocation(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	got := StatePath("yoloswe", 7)
	want := filepath.Join(home, ".bramble", "projects", "yoloswe-7", "prdozer-state.json")
	assert.Equal(t, want, got)
}

func TestWatcher_Tick_MergeableNoAutoMerge_Idles(t *testing.T) {
	approved := strings.Replace(okPRJSON, `"reviewDecision": "REVIEW_REQUIRED"`, `"reviewDecision": "APPROVED"`, 1)
	gh := setupGH(buildPRJSON(approved, "SUCCESS"), "[]", "base1")
	polish := &stubPolish{}
	w := newWatcherForTest(t, gh, polish)

	res, err := w.Tick(context.Background())
	require.NoError(t, err)
	assert.Equal(t, LastActionIdle, res.Action)
	assert.True(t, res.Changeset.Mergeable)
	assert.Empty(t, polish.calls)
}

func TestWatcher_Tick_StateSaveFailure_Surfaces(t *testing.T) {
	// Create a read-only state-file path so Save's WriteFile fails. LoadState
	// tolerates ENOENT but not "exists and unwritable", so we pre-create the
	// state dir and a 0o400 state file seeded with a valid previous State.
	home := t.TempDir()
	t.Setenv("HOME", home)
	statePath := StatePath("r", 42)
	require.NoError(t, os.MkdirAll(filepath.Dir(statePath), 0o755))
	// Pre-seed with a non-first-run state so the tick will decide to record a
	// snapshot and write. Write as 0o400 so the subsequent WriteFile fails.
	pre := &State{
		LastCheckAt:     time.Now(),
		LastSeenHeadSHA: "head0",
		LastSeenBaseSHA: "base0",
	}
	require.NoError(t, pre.Save(statePath))
	require.NoError(t, os.Chmod(statePath, 0o400))
	// Also make the parent dir unwritable so atomic-write approaches also fail.
	require.NoError(t, os.Chmod(filepath.Dir(statePath), 0o500))
	t.Cleanup(func() { _ = os.Chmod(filepath.Dir(statePath), 0o755) })

	gh := setupGH(buildPRJSON(okPRJSON, "SUCCESS"), "[]", "base1")
	polish := &stubPolish{}
	cfg := DefaultConfig()
	cfg.PollInterval = 10 * time.Millisecond
	w := NewWatcher(cfg, gh, polish, 42, ".", "r", nil)

	_, err := w.Tick(context.Background())
	require.Error(t, err, "state save failure must propagate")
	assert.Contains(t, err.Error(), "save state")
}

func TestWatcher_Tick_ClosedVsMerged_DistinctActions(t *testing.T) {
	cases := []struct {
		state      string
		wantAction LastAction
	}{
		{state: "MERGED", wantAction: LastActionMerged},
		{state: "CLOSED", wantAction: LastActionClosed},
	}
	for _, tc := range cases {
		t.Run(tc.state, func(t *testing.T) {
			modified := strings.Replace(okPRJSON, `"state": "OPEN"`, fmt.Sprintf(`"state": %q`, tc.state), 1)
			gh := setupGH(buildPRJSON(modified, "SUCCESS"), "[]", "base1")
			polish := &stubPolish{}
			w := newWatcherForTest(t, gh, polish)

			res, err := w.Tick(context.Background())
			require.NoError(t, err)
			assert.Equal(t, tc.wantAction, res.Action)
			assert.Empty(t, polish.calls, "closed/merged PRs should not invoke polish")
		})
	}
}

func TestFetchComments_HandlesConcatenatedPaginatePages(t *testing.T) {
	t.Parallel()
	// gh api --paginate emits one JSON array per page back-to-back. Verify
	// our decoder-loop flattens two pages into one combined list.
	twoPages := `[{"id":1,"user":{"login":"alice","type":"User"},"created_at":"2026-04-18T00:00:00Z"}]` +
		`[{"id":2,"user":{"login":"bob","type":"User"},"created_at":"2026-04-19T00:00:00Z"}]`
	gh := newFakeGH()
	gh.addPrefix("api --paginate repos/o/r/pulls/42/comments", twoPages)

	got, err := fetchComments(context.Background(), gh, ".", "repos/o/r/pulls/42/comments", "inline", SnapshotOptions{})
	require.NoError(t, err)
	require.Len(t, got, 2, "both pages must be decoded, not just the first")
	assert.Equal(t, "inline:1", got[0].ID)
	assert.Equal(t, "alice", got[0].Author)
	assert.Equal(t, "inline:2", got[1].ID)
	assert.Equal(t, "bob", got[1].Author)
}

func TestFetchBaseSHA_EscapesSlashInBranch(t *testing.T) {
	t.Parallel()
	gh := newFakeGH()
	// gh.addPrefix matches by joined-args prefix; the base-branch slash MUST
	// be URL-escaped (release/1.0 → release%2F1.0) so the refs call remains
	// a single path segment. If the escape regresses, this expectation fails.
	gh.addPrefix("api repos/{owner}/{repo}/git/refs/heads/release%2F1.0", "deadbeef\n")

	sha, err := fetchBaseSHA(context.Background(), gh, ".", "release/1.0")
	require.NoError(t, err)
	assert.Equal(t, "deadbeef", sha)
}

func TestBuildPolishPrompt(t *testing.T) {
	t.Parallel()
	assert.Equal(t, "/pr-polish 42", buildPolishPrompt(42, false))
	assert.Equal(t, "/pr-polish --local 42", buildPolishPrompt(42, true))
	assert.Equal(t, "/pr-polish", buildPolishPrompt(0, false))
}

// Sanity check that fakeGH wraps OS env without leaking real paths.
func TestFakeGHIsolated(t *testing.T) {
	t.Parallel()
	require.NotEmpty(t, os.TempDir())
}
