package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/cliapp"
	"github.com/bazelment/yoloswe/jiradozer"
	"github.com/bazelment/yoloswe/jiradozer/tracker"
	"github.com/bazelment/yoloswe/jiradozer/tracker/local"
)

func testMainLogger(_ testing.TB) *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelError + 1}))
}

type restoreDiscoveryTracker struct {
	listed chan struct{}
	issues []*tracker.Issue
}

func (r *restoreDiscoveryTracker) FetchIssue(_ context.Context, _ string) (*tracker.Issue, error) {
	return nil, nil
}

func (r *restoreDiscoveryTracker) ListIssues(_ context.Context, _ tracker.IssueFilter) ([]*tracker.Issue, error) {
	if r.listed != nil {
		select {
		case r.listed <- struct{}{}:
		default:
		}
	}
	return r.issues, nil
}

func (r *restoreDiscoveryTracker) FetchComments(_ context.Context, _ string, _ time.Time) ([]tracker.Comment, error) {
	return nil, nil
}

func (r *restoreDiscoveryTracker) FetchWorkflowStates(_ context.Context, _ string) ([]tracker.WorkflowState, error) {
	return nil, nil
}

func (r *restoreDiscoveryTracker) PostComment(_ context.Context, _ string, _ string) (tracker.Comment, error) {
	return tracker.Comment{}, nil
}

func (r *restoreDiscoveryTracker) UpdateIssueState(_ context.Context, _ string, _ string) error {
	return nil
}

func (r *restoreDiscoveryTracker) AddLabel(_ context.Context, _ string, _ string) error {
	return nil
}

func (r *restoreDiscoveryTracker) RemoveLabel(_ context.Context, _ string, _ string) error {
	return nil
}

type recordingRunTracker struct {
	comments []recordedComment
}

type recordedComment struct {
	issueID string
	body    string
}

func (r *recordingRunTracker) PostComment(_ context.Context, issueID string, body string) (tracker.Comment, error) {
	r.comments = append(r.comments, recordedComment{issueID: issueID, body: body})
	return tracker.Comment{CreatedAt: time.Now()}, nil
}

type failingRunTracker struct{}

func (failingRunTracker) PostComment(_ context.Context, _ string, _ string) (tracker.Comment, error) {
	return tracker.Comment{}, errors.New("tracker unavailable")
}

func runSingleStepForTest(t testing.TB, stepName string, issue *tracker.Issue, cfg *jiradozer.Config, poster jiradozer.CommentPoster, postResult bool, output string) error {
	t.Helper()
	return (&singleStepRun{
		ctx:        context.Background(),
		stepName:   stepName,
		issue:      issue,
		cfg:        cfg,
		poster:     poster,
		postResult: postResult,
		logger:     testMainLogger(t),
		runAgent: func(_ context.Context, _ string, _ jiradozer.PromptData, _ jiradozer.StepConfig, _ string, _ string, _ string, _ *render.Renderer, _ *slog.Logger) (jiradozer.StepAgentResult, error) {
			return jiradozer.StepAgentResult{Output: output, SessionID: "session-1"}, nil
		},
	}).run()
}

type restoreWTManager struct{}

func (restoreWTManager) NewWorktree(_ context.Context, _, _, _ string) (string, error) {
	return "", nil
}

func (restoreWTManager) RemoveWorktree(_ context.Context, _ string, _ bool) error {
	return nil
}

func TestValidateDryRunMode(t *testing.T) {
	dryRunCfg := func() *jiradozer.Config {
		return &jiradozer.Config{Source: jiradozer.SourceConfig{DryRun: true}}
	}
	tests := []struct {
		cfg     *jiradozer.Config
		name    string
		wantErr string
		args    runArgs
	}{
		{
			name: "dry-run off: any args accepted",
			cfg:  &jiradozer.Config{Source: jiradozer.SourceConfig{DryRun: false}},
			args: runArgs{issueID: "ENG-1", description: "local task"},
		},
		{
			name: "dry-run + team mode: accepted",
			cfg:  dryRunCfg(),
			args: runArgs{},
		},
		{
			name:    "dry-run + single-issue: rejected",
			cfg:     dryRunCfg(),
			args:    runArgs{issueID: "ENG-1"},
			wantErr: "--dry-run only applies to team mode",
		},
		{
			name:    "dry-run + description: rejected",
			cfg:     dryRunCfg(),
			args:    runArgs{description: "local task"},
			wantErr: "--dry-run only applies to team mode",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateDryRunMode(tt.cfg, tt.args)
			if tt.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, tt.wantErr)
		})
	}
}

func TestResolveRepoName(t *testing.T) {
	tests := []struct {
		name string
		cfg  *jiradozer.Config
		want string
	}{
		{
			name: "no team filter defaults to jiradozer",
			cfg: &jiradozer.Config{
				Source: jiradozer.SourceConfig{Filters: map[string]string{}},
			},
			want: "jiradozer",
		},
		{
			name: "linear team filter used verbatim",
			cfg: &jiradozer.Config{
				Tracker: jiradozer.TrackerConfig{Kind: "linear"},
				Source: jiradozer.SourceConfig{Filters: map[string]string{
					tracker.FilterTeam: "ENG",
				}},
			},
			want: "ENG",
		},
		{
			name: "github owner/repo collapsed to repo portion",
			cfg: &jiradozer.Config{
				Tracker: jiradozer.TrackerConfig{Kind: "github"},
				Source: jiradozer.SourceConfig{Filters: map[string]string{
					tracker.FilterTeam: "bazelment/yoloswe",
				}},
			},
			want: "yoloswe",
		},
		{
			name: "github malformed team falls through to raw value",
			cfg: &jiradozer.Config{
				Tracker: jiradozer.TrackerConfig{Kind: "github"},
				Source: jiradozer.SourceConfig{Filters: map[string]string{
					tracker.FilterTeam: "not-an-owner-repo",
				}},
			},
			want: "not-an-owner-repo",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveRepoName(tt.cfg)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestRunSingleStepPostResultPostsRenderedComment(t *testing.T) {
	cfg := jiradozer.DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.Plan = jiradozer.StepConfig{
		Prompt:          "plan {{.Identifier}}",
		CommentTemplate: "## {{.Heading}} Complete\n\nstep={{.Step}}\n{{.Output}}",
	}
	issue := &tracker.Issue{ID: "issue-id", Identifier: "INF-703", Title: "Test issue"}
	recorder := &recordingRunTracker{}

	err := runSingleStepForTest(t, "plan", issue, cfg, recorder, true, "\n planned output \n")
	require.NoError(t, err)
	require.Len(t, recorder.comments, 1)
	assert.Equal(t, "issue-id", recorder.comments[0].issueID)
	assert.Equal(t, "## Plan Complete\n\nstep=plan\n\n planned output \n", recorder.comments[0].body)
}

func TestRunSingleStepPostResultPostsEmptyOutput(t *testing.T) {
	cfg := jiradozer.DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.Plan = jiradozer.StepConfig{
		Prompt:          "plan {{.Identifier}}",
		CommentTemplate: "## {{.Heading}} Complete\n\n{{.Output}}",
	}
	issue := &tracker.Issue{ID: "issue-id", Identifier: "INF-703", Title: "Test issue"}
	recorder := &recordingRunTracker{}

	err := runSingleStepForTest(t, "plan", issue, cfg, recorder, true, "")
	require.NoError(t, err)
	require.Len(t, recorder.comments, 1)
	assert.Equal(t, "## Plan Complete\n\n", recorder.comments[0].body)
}

func TestRunSingleStepPostResultReturnsPostError(t *testing.T) {
	cfg := jiradozer.DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.Plan = jiradozer.StepConfig{
		Prompt:          "plan {{.Identifier}}",
		CommentTemplate: "## {{.Heading}} Complete\n\n{{.Output}}",
	}
	issue := &tracker.Issue{ID: "issue-id", Identifier: "INF-703", Title: "Test issue"}

	err := runSingleStepForTest(t, "plan", issue, cfg, failingRunTracker{}, true, "planned output")
	require.Error(t, err)
	assert.ErrorContains(t, err, "post step result comment")
	assert.ErrorContains(t, err, "tracker unavailable")
}

func TestRunSingleStepPostResultRequiresPoster(t *testing.T) {
	cfg := jiradozer.DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.Plan = jiradozer.StepConfig{
		Prompt:          "plan {{.Identifier}}",
		CommentTemplate: "## {{.Heading}} Complete\n\n{{.Output}}",
	}
	issue := &tracker.Issue{ID: "issue-id", Identifier: "INF-703", Title: "Test issue"}

	err := runSingleStepForTest(t, "plan", issue, cfg, nil, true, "planned output")
	require.Error(t, err)
	assert.ErrorContains(t, err, "--post-result requires a comment-capable tracker")
}

func TestRunSingleStepPostResultRequiresCommentTemplate(t *testing.T) {
	cfg := jiradozer.DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.Plan = jiradozer.StepConfig{Prompt: "plan {{.Identifier}}"}
	issue := &tracker.Issue{ID: "issue-id", Identifier: "INF-703", Title: "Test issue"}
	recorder := &recordingRunTracker{}

	err := runSingleStepForTest(t, "plan", issue, cfg, recorder, true, "planned output")
	require.Error(t, err)
	assert.ErrorContains(t, err, "no comment_template configured")
	assert.Empty(t, recorder.comments)
}

func TestRunSingleStepPostResultFalseDoesNotPost(t *testing.T) {
	cfg := jiradozer.DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.Plan = jiradozer.StepConfig{
		Prompt:          "plan {{.Identifier}}",
		CommentTemplate: "## {{.Heading}} Complete\n\n{{.Output}}",
	}
	issue := &tracker.Issue{ID: "issue-id", Identifier: "INF-703", Title: "Test issue"}
	recorder := &recordingRunTracker{}

	err := runSingleStepForTest(t, "plan", issue, cfg, recorder, false, "planned output")
	require.NoError(t, err)
	assert.Empty(t, recorder.comments)
}

func TestRunSingleStepRoundsPostResultPostsCombinedRoundComment(t *testing.T) {
	cfg := jiradozer.DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.Validate = jiradozer.StepConfig{
		RoundCommentTemplate: "## {{.Heading}} Round {{.Round}}/{{.TotalRounds}}\n\n{{.Output}}",
		Rounds: []jiradozer.RoundConfig{
			{Command: "printf 'round one'"},
			{Command: "printf 'round two'"},
		},
	}
	issue := &tracker.Issue{ID: "issue-id", Identifier: "INF-703", Title: "Test issue"}
	recorder := &recordingRunTracker{}

	err := runSingleStep(context.Background(), "validate", issue, cfg, "", recorder, true, nil, testMainLogger(t))
	require.NoError(t, err)
	require.Len(t, recorder.comments, 1)
	assert.Equal(t, "issue-id", recorder.comments[0].issueID)
	assert.Equal(t, "## Validate Round 2/2\n\nround one\n\n---\n\nround two", recorder.comments[0].body)
}

func TestRunSingleStepRoundsPostResultRequiresRoundCommentTemplate(t *testing.T) {
	cfg := jiradozer.DefaultConfig()
	cfg.WorkDir = t.TempDir()
	cfg.Validate = jiradozer.StepConfig{
		Rounds: []jiradozer.RoundConfig{
			{Command: "printf 'round output'"},
		},
	}
	issue := &tracker.Issue{ID: "issue-id", Identifier: "INF-703", Title: "Test issue"}
	recorder := &recordingRunTracker{}

	err := runSingleStep(context.Background(), "validate", issue, cfg, "", recorder, true, nil, testMainLogger(t))
	require.Error(t, err)
	assert.ErrorContains(t, err, "no round_comment_template configured")
	assert.Empty(t, recorder.comments)
}

func TestPostResultRequiresRunStep(t *testing.T) {
	app := &cliapp.App{Logger: testMainLogger(t)}

	err := run(context.Background(), app, runArgs{postResult: true})
	require.Error(t, err)
	assert.ErrorContains(t, err, "--post-result requires --run-step")
}

func TestRunCommandPostResultFlagReachesSingleStepPath(t *testing.T) {
	workDir := t.TempDir()
	cfgPath := writeRunConfig(t, "local", workDir)
	prev := runStepAgentDetailed
	runStepAgentDetailed = func(_ context.Context, _ string, data jiradozer.PromptData, _ jiradozer.StepConfig, _ string, _ string, _ string, _ *render.Renderer, _ *slog.Logger) (jiradozer.StepAgentResult, error) {
		assert.Equal(t, "LOCAL-1", data.Identifier)
		return jiradozer.StepAgentResult{Output: "cli planned output", SessionID: "session-1"}, nil
	}
	t.Cleanup(func() { runStepAgentDetailed = prev })

	opts := &cliapp.Options{ToolName: "jiradozer"}
	cmd := newRootCommand(opts)
	app := &cliapp.App{Logger: testMainLogger(t)}
	cmd.SetArgs([]string{
		"--config", cfgPath,
		"run",
		"--description", "CLI post result task",
		"--run-step", "plan",
		"--post-result",
	})

	require.NoError(t, cmd.ExecuteContext(cliapp.WithApp(context.Background(), app)))

	lt, err := local.NewTracker(filepath.Join(workDir, ".jiradozer", "issues"))
	require.NoError(t, err)
	issue, err := lt.FetchIssue(context.Background(), "LOCAL-1")
	require.NoError(t, err)
	comments, err := lt.FetchComments(context.Background(), issue.ID, time.Time{})
	require.NoError(t, err)
	require.Len(t, comments, 1)
	assert.Equal(t, "## Plan Complete\n\ncli planned output", comments[0].Body)
}

// TestRunFromDescriptionWiresReportTarget pins that a --description run keeps
// the created local issue reportable: runFromDescription must populate the
// report issue ID / target so a failing description run posts a failure comment
// on that issue instead of going Slack/log-only.
func TestRunFromDescriptionWiresReportTarget(t *testing.T) {
	workDir := t.TempDir()
	cfgPath := writeRunConfig(t, "local", workDir)
	cfg, err := loadRunConfig(runArgs{configPath: cfgPath, description: "x", workDir: workDir})
	require.NoError(t, err)

	lt, err := local.NewTracker(filepath.Join(workDir, ".jiradozer", "issues"))
	require.NoError(t, err)

	prev := runStepAgentDetailed
	runStepAgentDetailed = func(_ context.Context, _ string, _ jiradozer.PromptData, _ jiradozer.StepConfig, _ string, _ string, _ string, _ *render.Renderer, _ *slog.Logger) (jiradozer.StepAgentResult, error) {
		return jiradozer.StepAgentResult{}, errors.New("boom")
	}
	t.Cleanup(func() { runStepAgentDetailed = prev })

	var reportIssueID, reportTarget string
	err = runFromDescription(context.Background(), "build a widget", "plan", "", lt, false, cfg, nil, testMainLogger(t), &reportIssueID, &reportTarget)
	require.Error(t, err)

	// The created local issue must be wired for failure reporting.
	assert.NotEmpty(t, reportIssueID, "report issue ID should be populated from the created local issue")
	assert.Equal(t, "LOCAL-1", reportTarget, "report target should be the local issue identifier")
}

func TestLoadRunConfigAppliesCLIOverrides(t *testing.T) {
	cfgPath := writeRunConfig(t, "linear", t.TempDir())
	cfg, err := loadRunConfig(runArgs{
		configPath:    cfgPath,
		sourceFilters: []string{"team=ENG"},
		modelID:       "opus",
		pollInterval:  2 * time.Second,
		maxConcurrent: 7,
		branchPrefix:  "hotfix",
		dryRunSet:     true,
		dryRun:        true,
		skipPhases:    "plan, validate",
	})
	require.NoError(t, err)

	require.Equal(t, "opus", cfg.Agent.Model)
	require.Equal(t, 2*time.Second, cfg.PollInterval)
	require.Equal(t, "ENG", cfg.Source.Filters[tracker.FilterTeam])
	require.Equal(t, 7, cfg.Source.MaxConcurrent)
	require.Equal(t, "hotfix", cfg.Source.BranchPrefix)
	require.True(t, cfg.Source.DryRun)
	require.Equal(t, []string{"plan", "validate"}, cfg.SkipPhases)
}

func TestLoadRunConfigSkipPhasesCLIBeatsConfig(t *testing.T) {
	cfgPath := writeRunConfig(t, "linear", t.TempDir())
	content, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	content = append(content, []byte("\nskip_phases: [plan]\n")...)
	require.NoError(t, os.WriteFile(cfgPath, content, 0o600))

	cfg, err := loadRunConfig(runArgs{
		configPath:    cfgPath,
		sourceFilters: []string{"team=ENG"},
		skipPhases:    "validate",
	})
	require.NoError(t, err)

	require.Equal(t, []string{"validate"}, cfg.SkipPhases)
}

func TestValidateReloadCompatibleRejectsTrackerChanges(t *testing.T) {
	oldCfg := &jiradozer.Config{
		Tracker: jiradozer.TrackerConfig{Kind: "linear", APIKey: "old"},
		Source:  jiradozer.SourceConfig{Filters: map[string]string{tracker.FilterTeam: "ENG"}},
	}
	newCfg := &jiradozer.Config{
		Tracker: jiradozer.TrackerConfig{Kind: "github"},
		Source:  jiradozer.SourceConfig{Filters: map[string]string{tracker.FilterTeam: "ENG"}},
	}

	require.ErrorContains(t, validateReloadCompatible(oldCfg, newCfg), "tracker kind change")

	newCfg = &jiradozer.Config{
		Tracker: jiradozer.TrackerConfig{Kind: "linear", APIKey: "new"},
		Source:  jiradozer.SourceConfig{Filters: map[string]string{tracker.FilterTeam: "ENG"}},
	}
	require.ErrorContains(t, validateReloadCompatible(oldCfg, newCfg), "tracker config changes")
}

func TestValidateReloadCompatibleRejectsSourceModeChanges(t *testing.T) {
	oldCfg := &jiradozer.Config{
		Tracker: jiradozer.TrackerConfig{Kind: "linear", APIKey: "key"},
		Source:  jiradozer.SourceConfig{Filters: map[string]string{tracker.FilterTeam: "ENG"}},
	}
	newCfg := &jiradozer.Config{
		Tracker: jiradozer.TrackerConfig{Kind: "linear", APIKey: "key"},
	}

	require.ErrorContains(t, validateReloadCompatible(oldCfg, newCfg), "source mode change")
}

func TestValidateReloadCompatibleRejectsDryRunChanges(t *testing.T) {
	oldCfg := &jiradozer.Config{
		Tracker: jiradozer.TrackerConfig{Kind: "linear", APIKey: "key"},
		Source:  jiradozer.SourceConfig{DryRun: false, Filters: map[string]string{tracker.FilterTeam: "ENG"}},
	}
	newCfg := &jiradozer.Config{
		Tracker: jiradozer.TrackerConfig{Kind: "linear", APIKey: "key"},
		Source:  jiradozer.SourceConfig{DryRun: true, Filters: map[string]string{tracker.FilterTeam: "ENG"}},
	}

	require.ErrorContains(t, validateReloadCompatible(oldCfg, newCfg), "source dry-run change")
}

func TestValidateReloadCompatibleRejectsWorkDirChanges(t *testing.T) {
	oldCfg := &jiradozer.Config{
		Tracker: jiradozer.TrackerConfig{Kind: "local"},
		Source:  jiradozer.SourceConfig{Filters: map[string]string{tracker.FilterTeam: "ENG"}},
		WorkDir: "/repo/old",
	}
	newCfg := &jiradozer.Config{
		Tracker: jiradozer.TrackerConfig{Kind: "local"},
		Source:  jiradozer.SourceConfig{Filters: map[string]string{tracker.FilterTeam: "ENG"}},
		WorkDir: "/repo/new",
	}

	require.ErrorContains(t, validateReloadCompatible(oldCfg, newCfg), "work_dir change")
}

func TestValidateReloadCompatibleRejectsRepoNameChanges(t *testing.T) {
	oldCfg := &jiradozer.Config{
		Tracker: jiradozer.TrackerConfig{Kind: "linear"},
		Source:  jiradozer.SourceConfig{Filters: map[string]string{tracker.FilterTeam: "ENG"}},
	}
	newCfg := &jiradozer.Config{
		Tracker: jiradozer.TrackerConfig{Kind: "linear"},
		Source:  jiradozer.SourceConfig{Filters: map[string]string{tracker.FilterTeam: "OPS"}},
	}

	require.ErrorContains(t, validateReloadCompatible(oldCfg, newCfg), "team/repository filter change")
}

func TestRestoreFromEnvMarksRestoredIssuesSeen(t *testing.T) {
	issue := &tracker.Issue{ID: "issue-1", Identifier: "ENG-1", Title: "Restored"}
	cmd := exec.Command("sh", "-c", "exit 0")
	require.NoError(t, cmd.Start())

	statePath := filepath.Join(t.TempDir(), "state.json")
	require.NoError(t, jiradozer.WriteRuntimeStateAtomically(statePath, jiradozer.RuntimeState{
		ActiveWorkflow: []jiradozer.ManagedWorkflowSnapshot{
			{Issue: issue, PID: cmd.Process.Pid, StartedAt: time.Now()},
		},
	}))
	t.Setenv(restoreStateEnv, statePath)

	listed := make(chan struct{}, 1)
	issueTracker := &restoreDiscoveryTracker{listed: listed, issues: []*tracker.Issue{issue}}
	cfg := &jiradozer.Config{Source: jiradozer.SourceConfig{MaxConcurrent: 1}}
	disc := jiradozer.NewDiscovery(issueTracker, tracker.IssueFilter{}, time.Hour, testMainLogger(t))
	s := &teamSupervisor{
		cfg:    cfg,
		logger: testMainLogger(t),
		disc:   disc,
		orch:   jiradozer.NewOrchestrator(issueTracker, cfg, restoreWTManager{}, "", testMainLogger(t)),
	}

	require.NoError(t, s.restoreFromEnv())
	select {
	case status := <-s.orch.StatusUpdates():
		require.Equal(t, jiradozer.StepInit, status.Step)
		require.Equal(t, issue.Identifier, status.Issue.Identifier)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for restored StepInit")
	}
	s.orch.Wait()
	select {
	case status := <-s.orch.StatusUpdates():
		require.True(t, status.IsDone())
		require.Equal(t, issue.Identifier, status.Issue.Identifier)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for restored terminal status")
	}
	s.orch.Shutdown()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := disc.Run(ctx)
	require.Eventually(t, func() bool {
		select {
		case <-listed:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)

	select {
	case got := <-ch:
		t.Fatalf("restored issue was rediscovered: %s", got.Identifier)
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	for range ch {
	}
}

func TestRestoreFromEnvDoesNotMarkSkippedSnapshotsSeen(t *testing.T) {
	issue := &tracker.Issue{ID: "issue-1", Identifier: "ENG-1", Title: "Skipped"}
	statePath := filepath.Join(t.TempDir(), "state.json")
	require.NoError(t, jiradozer.WriteRuntimeStateAtomically(statePath, jiradozer.RuntimeState{
		ActiveWorkflow: []jiradozer.ManagedWorkflowSnapshot{
			{Issue: issue},
		},
	}))
	t.Setenv(restoreStateEnv, statePath)

	listed := make(chan struct{}, 1)
	issueTracker := &restoreDiscoveryTracker{listed: listed, issues: []*tracker.Issue{issue}}
	cfg := &jiradozer.Config{Source: jiradozer.SourceConfig{MaxConcurrent: 1}}
	disc := jiradozer.NewDiscovery(issueTracker, tracker.IssueFilter{}, time.Hour, testMainLogger(t))
	s := &teamSupervisor{
		cfg:    cfg,
		logger: testMainLogger(t),
		disc:   disc,
		orch:   jiradozer.NewOrchestrator(issueTracker, cfg, restoreWTManager{}, "", testMainLogger(t)),
	}

	require.NoError(t, s.restoreFromEnv())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ch := disc.Run(ctx)
	require.Eventually(t, func() bool {
		select {
		case <-listed:
			return true
		default:
			return false
		}
	}, time.Second, 10*time.Millisecond)

	select {
	case got := <-ch:
		require.Equal(t, issue.Identifier, got.Identifier)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for skipped restored issue to rediscover")
	}
	cancel()
	for range ch {
	}
}

func TestTeamSupervisorReloadUpdatesConfigAndOrchestrator(t *testing.T) {
	workDir := t.TempDir()
	cfgPath := writeRunConfig(t, "linear", workDir)
	args := runArgs{configPath: cfgPath, sourceFilters: []string{tracker.FilterTeam + "=ENG"}}
	cfg, err := loadRunConfig(args)
	require.NoError(t, err)
	cfg.Source.MaxConcurrent = 1

	issueTracker := &restoreDiscoveryTracker{}
	s := &teamSupervisor{
		cfg:    cfg,
		args:   args,
		logger: testMainLogger(t),
		disc:   jiradozer.NewDiscovery(issueTracker, cfg.Source.ToFilter(), cfg.PollInterval, testMainLogger(t)),
		orch:   jiradozer.NewOrchestrator(issueTracker, cfg, restoreWTManager{}, "", testMainLogger(t)),
	}

	// Apply failure reporting once up front (as newTeamSupervisor does) so we
	// can assert that reload re-applies it when the webhook changes.
	s.applyFailureReporting()
	require.Nil(t, s.orch.FailureNotifier(), "no webhook configured initially")

	content, err := os.ReadFile(cfgPath)
	require.NoError(t, err)
	content = bytes.Replace(content, []byte("poll_interval: 15s"), []byte("poll_interval: 1s"), 1)
	content = append(content, []byte(`
source:
    filters:
        team: ENG
    max_concurrent: 2
notify:
    slack_webhook: https://hooks.example.com/abc
`)...)
	require.NoError(t, os.WriteFile(cfgPath, content, 0o600))

	s.reload()

	require.Equal(t, 2, s.cfg.Source.MaxConcurrent)
	require.Equal(t, time.Second, s.cfg.PollInterval)
	require.Equal(t, 2, s.orch.ConfigSnapshot().Source.MaxConcurrent)
	require.NotNil(t, s.orch.FailureNotifier(), "reload must re-apply the failure notifier when slack_webhook changes")
}

// TestDryRunFlagPlacement verifies --dry-run is honored regardless of where
// the user puts it relative to the run subcommand. Because the flag is
// registered on both root and `run` (via registerRunFlags) but cobra only
// records `Changed=true` on whichever FlagSet actually parsed it, a naive
// `cmd.Flags().Changed("dry-run")` in run's RunE silently drops the flag
// when the user wrote `jiradozer --dry-run run …`. Both invocation paths
// run through dryRunChanged: the run-subcommand RunE consults runCmd, and
// the back-compat root RunE (`jiradozer --dry-run` with no subcommand)
// consults rootCmd directly.
func TestDryRunFlagPlacement(t *testing.T) {
	tests := []struct {
		name     string
		checkCmd string // "run" or "root" — which command's RunE actually fires
		argv     []string
		want     bool
	}{
		{name: "no dry-run", argv: []string{"run"}, checkCmd: "run", want: false},
		{name: "dry-run on run subcommand", argv: []string{"run", "--dry-run"}, checkCmd: "run", want: true},
		{name: "dry-run before run subcommand", argv: []string{"--dry-run", "run"}, checkCmd: "run", want: true},
		// Back-compat path: bare `jiradozer --dry-run --filter team=ENG`
		// (no `run`) lands on root's RunE, so dryRunChanged is invoked
		// against rootCmd. registerRunFlags is bound on root for this case.
		{name: "dry-run on root, no subcommand", argv: []string{"--dry-run"}, checkCmd: "root", want: true},
		{name: "no dry-run on root, no subcommand", argv: []string{}, checkCmd: "root", want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var rargs runArgs
			rootCmd := &cobra.Command{
				Use:  "jiradozer",
				RunE: func(cmd *cobra.Command, _ []string) error { return nil },
			}
			runCmd := &cobra.Command{
				Use:  "run",
				RunE: func(cmd *cobra.Command, _ []string) error { return nil },
			}
			registerRunFlags(rootCmd, &rargs)
			registerRunFlags(runCmd, &rargs)
			rootCmd.AddCommand(runCmd)
			rootCmd.SetArgs(tt.argv)
			require.NoError(t, rootCmd.Execute())

			target := runCmd
			if tt.checkCmd == "root" {
				target = rootCmd
			}
			got := dryRunChanged(target)
			assert.Equal(t, tt.want, got)
		})
	}
}

// Redaction is tested in cliapp/redact_test.go; jiradozer just composes its
// sensitive flag list into cliapp.Options.SensitiveFlags.

func TestBuildChildArgs(t *testing.T) {
	tests := []struct {
		wantContain []string
		wantOmit    []string
		name        string
		args        runArgs
	}{
		{
			name:        "thinking-level set is propagated",
			args:        runArgs{thinkingLevel: "high"},
			wantContain: []string{"--thinking-level", "high"},
		},
		{
			name:     "thinking-level empty is omitted",
			args:     runArgs{},
			wantOmit: []string{"--thinking-level"},
		},
		{
			name:        "model + thinking-level both propagated",
			args:        runArgs{modelID: "opus", thinkingLevel: "max"},
			wantContain: []string{"--model", "opus", "--thinking-level", "max"},
		},
	}

	app := &cliapp.App{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := buildChildArgs(app, tt.args, "/tmp/jiradozer.yaml")
			joined := ""
			for _, a := range got {
				joined += a + " "
			}
			for _, want := range tt.wantContain {
				assert.Contains(t, joined, want)
			}
			for _, want := range tt.wantOmit {
				assert.NotContains(t, joined, want)
			}
		})
	}
}

func writeRunConfig(t *testing.T, trackerKind, workDir string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "jiradozer.yaml")
	t.Setenv("LINEAR_API_KEY", "test-key")
	content, err := bootstrapYAML("")
	require.NoError(t, err)
	content = bytes.Replace(content, []byte("kind: linear"), []byte("kind: "+trackerKind), 1)
	content = bytes.Replace(content, []byte("work_dir: ."), []byte("work_dir: "+workDir), 1)
	if trackerKind == "github" || trackerKind == "local" {
		content = bytes.Replace(content, []byte("api_key: $LINEAR_API_KEY"), []byte("api_key: \"\""), 1)
	}
	require.NoError(t, os.WriteFile(path, content, 0o600))
	return path
}

// TestBuildChildArgsOrdering pins the argv layout: persistent (root-level)
// flags — --config and the standard --verbose/--verbosity/--color set —
// must appear before the `run` subcommand token, and run-only flags
// (--model, --thinking-level, --max-budget, --auto-approve) must appear
// after it. Cobra's PersistentFlags inheritance means either side parses
// today, but mixing breaks the rule that a flag is declared adjacent to
// the command that owns it; this test fails fast if a future edit puts a
// run-only flag before `run` or vice versa.
func TestBuildChildArgsOrdering(t *testing.T) {
	app := &cliapp.App{Verbosity: render.VerbosityVerbose, Color: render.ColorAlways}
	args := runArgs{
		modelID:       "opus",
		thinkingLevel: "max",
		maxBudget:     12.5,
		autoApprove:   "all",
		skipPhases:    "plan,validate",
	}
	got := buildChildArgs(app, args, "/tmp/jiradozer.yaml")

	indexOf := func(needle string) int {
		for i, a := range got {
			if a == needle {
				return i
			}
		}
		return -1
	}
	runIdx := indexOf("run")
	require.NotEqual(t, -1, runIdx, "argv must contain `run` subcommand token; got %v", got)

	persistentBeforeRun := []string{"--config", "--verbose", "--color"}
	for _, flag := range persistentBeforeRun {
		idx := indexOf(flag)
		if idx == -1 {
			continue
		}
		assert.Lessf(t, idx, runIdx,
			"persistent flag %s must appear before `run` (got argv: %v)", flag, got)
	}

	runOnlyAfterRun := []string{"--model", "--thinking-level", "--max-budget", "--auto-approve", "--skip-phases"}
	for _, flag := range runOnlyAfterRun {
		idx := indexOf(flag)
		require.NotEqualf(t, -1, idx, "argv must contain run-only flag %s; got %v", flag, got)
		assert.Greaterf(t, idx, runIdx,
			"run-only flag %s must appear after `run` (got argv: %v)", flag, got)
	}
}

func TestBootstrapUsesConfigPathFromRoot(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "test")

	tests := []struct {
		name     string
		wantFile string
		args     []string
	}{
		{
			name:     "config before bootstrap",
			wantFile: "custom.yaml",
			args:     []string{"--config", "custom.yaml", "bootstrap"},
		},
		{
			name:     "config after bootstrap",
			wantFile: "custom.yaml",
			args:     []string{"bootstrap", "--config", "custom.yaml"},
		},
		{
			name:     "output overrides config",
			wantFile: "output.yaml",
			args:     []string{"bootstrap", "--config", "config.yaml", "--output", "output.yaml"},
		},
		{
			name:     "output still works without config",
			wantFile: "output.yaml",
			args:     []string{"bootstrap", "--output", "output.yaml"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)

			cmd := newRootCommand(&cliapp.Options{ToolName: "jiradozer"})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetArgs(tt.args)
			require.NoError(t, cmd.Execute())

			got, err := os.ReadFile(tt.wantFile)
			require.NoError(t, err)
			assert.Contains(t, string(got), "jiradozer bootstrap")

			if tt.wantFile == "output.yaml" {
				_, err := os.Stat("config.yaml")
				assert.ErrorIs(t, err, os.ErrNotExist)
			}
		})
	}
}

func TestValidateConfigUsesConfigPathFromRoot(t *testing.T) {
	t.Setenv("LINEAR_API_KEY", "test")

	tests := []struct {
		name string
		args []string
	}{
		{
			name: "config before validate-config",
			args: []string{"--config", "custom.yaml", "validate-config"},
		},
		{
			name: "config after validate-config",
			args: []string{"validate-config", "--config", "custom.yaml"},
		},
	}

	content, err := bootstrapYAML("")
	require.NoError(t, err)

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			t.Chdir(dir)
			require.NoError(t, os.WriteFile("custom.yaml", content, 0o644))

			cmd := newRootCommand(&cliapp.Options{ToolName: "jiradozer"})
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetArgs(tt.args)
			require.NoError(t, cmd.Execute())
			assert.Contains(t, out.String(), "ok: custom.yaml")
		})
	}
}

// TestShouldReportFailure pins the run() failure-reporting guard: a real step
// error must alert even when the context was cancelled (the fail-loudly goal),
// while a bare cancellation/deadline is an expected stop and stays silent.
func TestShouldReportFailure(t *testing.T) {
	t.Parallel()
	cases := []struct {
		err  error
		name string
		want bool
	}{
		{nil, "nil error", false},
		{context.Canceled, "bare cancellation", false},
		{context.DeadlineExceeded, "bare deadline", false},
		{fmt.Errorf("run-step plan: %w", context.Canceled), "wrapped cancellation", false},
		{errors.New("plan step: agent execution: API Error"), "real step error", true},
		{errors.New("validate round 2/3: agent execution: boom"), "real error during cancellation", true},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := shouldReportFailure(tc.err); got != tc.want {
				t.Errorf("shouldReportFailure(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

func TestDescribeTarget(t *testing.T) {
	t.Parallel()
	// Newlines collapse to spaces; over-long input truncates rune-safely.
	if got := describeTarget("  /sy:forge-prod-health dev\nsecond line  "); got != "/sy:forge-prod-health dev second line" {
		t.Errorf("describeTarget = %q", got)
	}
	long := strings.Repeat("x", 200)
	got := describeTarget(long)
	if len([]rune(got)) != 83 { // 80 runes + "..."
		t.Errorf("describeTarget truncated length = %d runes, want 83", len([]rune(got)))
	}
}
