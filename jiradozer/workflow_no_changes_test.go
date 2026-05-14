package jiradozer

import (
	"context"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

func TestWorkflow_BuildProducedNoChanges(t *testing.T) {
	t.Run("clean tree at base", func(t *testing.T) {
		workDir := newGitRepoAtOriginMain(t)
		wf := workflowForGitStateTest(workDir)

		assert.True(t, wf.buildProducedNoChanges(context.Background()))
	})

	t.Run("clean tree ahead of base", func(t *testing.T) {
		workDir := newGitRepoAtOriginMain(t)
		require.NoError(t, os.WriteFile(filepath.Join(workDir, "feature.txt"), []byte("feature\n"), 0o644))
		gitTest(t, workDir, "add", "feature.txt")
		gitTest(t, workDir, "commit", "-m", "feature")
		wf := workflowForGitStateTest(workDir)

		assert.False(t, wf.buildProducedNoChanges(context.Background()))
	})

	t.Run("dirty tree", func(t *testing.T) {
		workDir := newGitRepoAtOriginMain(t)
		require.NoError(t, os.WriteFile(filepath.Join(workDir, "dirty.txt"), []byte("dirty\n"), 0o644))
		wf := workflowForGitStateTest(workDir)

		assert.False(t, wf.buildProducedNoChanges(context.Background()))
	})

	t.Run("missing origin base", func(t *testing.T) {
		workDir := newGitRepo(t)
		wf := workflowForGitStateTest(workDir)

		assert.False(t, wf.buildProducedNoChanges(context.Background()))
	})
}

func TestWorkflow_BuildNoChangesSkipsRemainingSteps(t *testing.T) {
	workDir := newGitRepoAtOriginMain(t)
	PersistPlan(workDir, "persisted plan", discardLogger())
	gitTest(t, workDir, "add", ".jiradozer/plan.md")
	gitTest(t, workDir, "commit", "-m", "persist plan")
	gitTest(t, workDir, "update-ref", "refs/remotes/origin/main", "HEAD")

	issue := testIssue()
	issue.Labels = []string{"jiradozer-plan-done"}
	mt := &mockWorkflowTracker{
		fetchIssueReply: &tracker.Issue{Labels: issue.Labels},
		workflowStates:  testWorkflowStates(),
	}
	cfg := testConfig()
	cfg.WorkDir = workDir
	wf := NewWorkflow(mt, issue, cfg, discardLogger())

	var steps []string
	wf.runStepAgent = func(_ context.Context, stepName string, _ PromptData, _ StepConfig, _ string, _ string, _ string, _ *render.Renderer, _ *slog.Logger) (StepAgentResult, error) {
		steps = append(steps, stepName)
		return StepAgentResult{Output: stepName + " output"}, nil
	}

	require.NoError(t, wf.Run(context.Background()))

	assert.Equal(t, StepDone, wf.state.Current())
	assert.Equal(t, []string{"build"}, steps)
	assert.Equal(t, phaseDone, wf.phases[PhaseBuild])
	assert.Equal(t, phaseDone, wf.phases[PhaseValidate])
	assert.Equal(t, phaseDone, wf.phases[PhaseShip])
	seq := labelSequence(mt)
	assert.Contains(t, seq, "AddLabel:jiradozer-build-done")
	assert.Contains(t, seq, "AddLabel:jiradozer-validate-done")
	assert.Contains(t, seq, "AddLabel:jiradozer-ship-done")

	postCalls := mt.getCalls("PostComment")
	require.GreaterOrEqual(t, len(postCalls), 2)
	assert.Equal(t, "Build produced no changes; nothing to ship. Marking issue done.", postCalls[1].args[1])
}

func TestWorkflow_BuildDirtyTreeProceedsToCreatePR(t *testing.T) {
	workDir := newGitRepoAtOriginMain(t)
	mt := &mockWorkflowTracker{workflowStates: testWorkflowStates()}
	cfg := testConfig()
	cfg.WorkDir = workDir
	wf := NewWorkflow(mt, testIssue(), cfg, discardLogger())
	walkTo(t, wf.state, StepBuilding)

	wf.runStepAgent = func(_ context.Context, _ string, _ PromptData, _ StepConfig, workDir string, _ string, _ string, _ *render.Renderer, _ *slog.Logger) (StepAgentResult, error) {
		return StepAgentResult{Output: "dirty build"}, os.WriteFile(filepath.Join(workDir, "change.txt"), []byte("change\n"), 0o644)
	}

	wf.runStep(context.Background(), "build", cfg.Build, StepCreatingPR, "build_complete")

	assert.Equal(t, StepCreatingPR, wf.state.Current())
}

func workflowForGitStateTest(workDir string) *Workflow {
	cfg := testConfig()
	cfg.WorkDir = workDir
	return NewWorkflow(&mockWorkflowTracker{}, testIssue(), cfg, discardLogger())
}

func newGitRepoAtOriginMain(t *testing.T) string {
	t.Helper()
	workDir := newGitRepo(t)
	gitTest(t, workDir, "update-ref", "refs/remotes/origin/main", "HEAD")
	return workDir
}

func newGitRepo(t *testing.T) string {
	t.Helper()
	workDir := t.TempDir()
	gitTest(t, workDir, "init")
	gitTest(t, workDir, "config", "user.email", "test@example.com")
	gitTest(t, workDir, "config", "user.name", "Test User")
	require.NoError(t, os.WriteFile(filepath.Join(workDir, "README.md"), []byte("# test\n"), 0o644))
	gitTest(t, workDir, "add", "README.md")
	gitTest(t, workDir, "commit", "-m", "initial")
	gitTest(t, workDir, "branch", "-M", "main")
	return workDir
}

func gitTest(t *testing.T, workDir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = workDir
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v failed:\n%s", args, string(out))
}
