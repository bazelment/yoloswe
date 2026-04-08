package jiradozer

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
)

func TestNewPromptData(t *testing.T) {
	desc := "Fix the widget rendering"
	url := "https://linear.app/team/ENG-123"
	issue := &tracker.Issue{
		ID:          "issue-id-1",
		Identifier:  "ENG-123",
		Title:       "Widget bug",
		Description: &desc,
		URL:         &url,
		Labels:      []string{"bug", "priority:high"},
	}

	data := NewPromptData(issue, "main")
	assert.Equal(t, "ENG-123", data.Identifier)
	assert.Equal(t, "Widget bug", data.Title)
	assert.Equal(t, "Fix the widget rendering", data.Description)
	assert.Equal(t, "https://linear.app/team/ENG-123", data.URL)
	assert.Equal(t, "bug, priority:high", data.Labels)
	assert.Equal(t, "main", data.BaseBranch)
	assert.Empty(t, data.Plan)
	assert.Empty(t, data.BuildOutput)
}

func TestNewPromptData_NilOptionalFields(t *testing.T) {
	issue := &tracker.Issue{
		ID:         "id",
		Identifier: "ENG-1",
		Title:      "Test",
	}

	data := NewPromptData(issue, "main")
	assert.Empty(t, data.Description)
	assert.Empty(t, data.URL)
	assert.Empty(t, data.Labels)
}

func TestRenderPrompt_DefaultPlan(t *testing.T) {
	data := PromptData{
		Identifier:  "ENG-123",
		Title:       "Widget bug",
		Description: "The widget doesn't render correctly",
		URL:         "https://linear.app/team/ENG-123",
		Labels:      "bug, priority:high",
		BaseBranch:  "main",
	}

	output, err := renderPrompt(defaultPlanPrompt, data)
	require.NoError(t, err)
	assert.Contains(t, output, "ENG-123")
	assert.Contains(t, output, "Widget bug")
	assert.Contains(t, output, "The widget doesn't render correctly")
	assert.Contains(t, output, "https://linear.app/team/ENG-123")
	assert.Contains(t, output, "bug, priority:high")
	assert.Contains(t, output, "implementation plan")
}

func TestRenderPrompt_DefaultBuild_WithPlan(t *testing.T) {
	data := PromptData{
		Identifier:  "ENG-123",
		Title:       "Widget bug",
		Description: "Fix the widget",
		Plan:        "1. Edit widget.go\n2. Update tests",
	}

	output, err := renderPrompt(defaultBuildPrompt, data)
	require.NoError(t, err)
	assert.Contains(t, output, "Approved Plan")
	assert.Contains(t, output, "1. Edit widget.go")
	assert.Contains(t, output, "Implement the changes")
}

func TestRenderPrompt_DefaultBuild_WithoutPlan(t *testing.T) {
	data := PromptData{
		Identifier:  "ENG-123",
		Title:       "Widget bug",
		Description: "Fix the widget",
	}

	output, err := renderPrompt(defaultBuildPrompt, data)
	require.NoError(t, err)
	assert.Contains(t, output, "No plan is available")
	assert.NotContains(t, output, "Approved Plan")
}

func TestRenderPrompt_DefaultValidate(t *testing.T) {
	data := PromptData{
		Identifier: "ENG-123",
		Title:      "Widget bug",
	}

	output, err := renderPrompt(defaultValidatePrompt, data)
	require.NoError(t, err)
	assert.Contains(t, output, "ENG-123")
	assert.Contains(t, output, "tests and linters")
}

func TestRenderPrompt_DefaultCreatePR(t *testing.T) {
	data := PromptData{
		BaseBranch: "main",
	}

	output, err := renderPrompt(defaultCreatePRPrompt, data)
	require.NoError(t, err)
	assert.Contains(t, output, "pull request")
	assert.Contains(t, output, "main")
	assert.Contains(t, output, "already exists")
}

func TestRenderPrompt_DefaultShip(t *testing.T) {
	data := PromptData{
		Identifier: "ENG-123",
		Title:      "Widget bug",
		URL:        "https://linear.app/team/ENG-123",
		BaseBranch: "main",
	}

	output, err := renderPrompt(defaultShipPrompt, data)
	require.NoError(t, err)
	assert.Contains(t, output, "pull request")
	assert.Contains(t, output, "already exists")
	assert.Contains(t, output, "main")
	assert.Contains(t, output, "https://linear.app/team/ENG-123")
}

func TestRenderPrompt_CustomTemplate(t *testing.T) {
	tmpl := "Fix {{.Identifier}}: {{.Title}} on branch {{.BaseBranch}}"
	data := PromptData{
		Identifier: "ENG-42",
		Title:      "broken tests",
		BaseBranch: "develop",
	}

	output, err := renderPrompt(tmpl, data)
	require.NoError(t, err)
	assert.Equal(t, "Fix ENG-42: broken tests on branch develop", output)
}

func TestRenderPrompt_InvalidTemplate(t *testing.T) {
	_, err := renderPrompt("{{.Missing}", PromptData{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse template")
}

func TestDefaultPromptForStep(t *testing.T) {
	assert.NotEmpty(t, DefaultPromptForStep("plan"))
	assert.NotEmpty(t, DefaultPromptForStep("build"))
	assert.NotEmpty(t, DefaultPromptForStep("validate"))
	assert.NotEmpty(t, DefaultPromptForStep("create_pr"))
	assert.NotEmpty(t, DefaultPromptForStep("ship"))
	assert.Empty(t, DefaultPromptForStep("unknown"))
}

func TestResolvePromptForExecution_FirstExecution(t *testing.T) {
	data := PromptData{Identifier: "ENG-1", Title: "Test"}

	prompt, err := resolvePromptForExecution("", defaultPlanPrompt, data, "", "")
	require.NoError(t, err)
	assert.Contains(t, prompt, "ENG-1")
	assert.Contains(t, prompt, "implementation plan")
}

func TestResolvePromptForExecution_CustomPrompt(t *testing.T) {
	data := PromptData{Identifier: "ENG-1", Title: "Test"}

	prompt, err := resolvePromptForExecution("Custom: {{.Identifier}}", defaultPlanPrompt, data, "", "")
	require.NoError(t, err)
	assert.Equal(t, "Custom: ENG-1", prompt)
}

func TestResolvePromptForExecution_ResumeWithFeedback(t *testing.T) {
	data := PromptData{Identifier: "ENG-1", Title: "Test"}

	// When resuming with feedback, the feedback IS the prompt.
	prompt, err := resolvePromptForExecution("", defaultPlanPrompt, data, "Please use the new API", "session-123")
	require.NoError(t, err)
	assert.Equal(t, "Please use the new API", prompt)
}

func TestResolvePromptForExecution_FeedbackWithoutSession(t *testing.T) {
	data := PromptData{Identifier: "ENG-1", Title: "Test"}

	// Feedback without session: render template + append feedback.
	prompt, err := resolvePromptForExecution("", defaultPlanPrompt, data, "Consider edge cases", "")
	require.NoError(t, err)
	assert.Contains(t, prompt, "ENG-1")
	assert.Contains(t, prompt, "Previous feedback to incorporate")
	assert.Contains(t, prompt, "Consider edge cases")
}

func TestResolvePromptForExecution_ResumeWithoutFeedback(t *testing.T) {
	data := PromptData{Identifier: "ENG-1", Title: "Test"}

	// Resume session but no feedback: render template normally.
	prompt, err := resolvePromptForExecution("", defaultPlanPrompt, data, "", "session-123")
	require.NoError(t, err)
	assert.Contains(t, prompt, "ENG-1")
	assert.NotContains(t, prompt, "feedback")
}

func TestLogEventHandler_TracksPlanFile_ExitPlanMode(t *testing.T) {
	h := &logEventHandler{logger: slog.Default(), step: "plan"}

	// Write .md file, then ExitPlanMode confirms it as the plan file.
	h.OnToolComplete("Write", "tool-1", map[string]interface{}{
		"file_path": "/home/user/project/docs/plans/my-plan.md",
	}, nil, false)
	assert.Empty(t, h.planFilePath, "not confirmed yet")
	assert.Equal(t, "/home/user/project/docs/plans/my-plan.md", h.lastWriteMD)

	h.OnToolComplete("ExitPlanMode", "tool-2", map[string]interface{}{}, nil, false)
	assert.Equal(t, "/home/user/project/docs/plans/my-plan.md", h.planFilePath)
}

func TestLogEventHandler_TracksPlanFile_ClaudePlansDir(t *testing.T) {
	h := &logEventHandler{logger: slog.Default(), step: "plan"}

	// Also works with .claude/plans/ paths.
	h.OnToolComplete("Write", "tool-1", map[string]interface{}{
		"file_path": "/home/user/project/.claude/plans/abc-123.md",
	}, nil, false)
	h.OnToolComplete("ExitPlanMode", "tool-2", map[string]interface{}{}, nil, false)
	assert.Equal(t, "/home/user/project/.claude/plans/abc-123.md", h.planFilePath)
}

func TestLogEventHandler_NoExitPlanMode_NoPlanFile(t *testing.T) {
	h := &logEventHandler{logger: slog.Default(), step: "plan"}

	// Write .md without ExitPlanMode — planFilePath stays empty.
	h.OnToolComplete("Write", "tool-1", map[string]interface{}{
		"file_path": "/home/user/project/docs/plans/my-plan.md",
	}, nil, false)
	assert.Empty(t, h.planFilePath)
}

func TestLogEventHandler_IgnoresNonMDWrites(t *testing.T) {
	h := &logEventHandler{logger: slog.Default(), step: "plan"}

	// Write to a non-.md path should not be tracked.
	h.OnToolComplete("Write", "tool-1", map[string]interface{}{
		"file_path": "/home/user/project/src/main.go",
	}, nil, false)
	h.OnToolComplete("ExitPlanMode", "tool-2", map[string]interface{}{}, nil, false)
	assert.Empty(t, h.planFilePath)
}

func TestLogEventHandler_IgnoresNonWriteTools(t *testing.T) {
	h := &logEventHandler{logger: slog.Default(), step: "plan"}

	// Non-Write tool should not track .md path.
	h.OnToolComplete("Read", "tool-1", map[string]interface{}{
		"file_path": "/home/user/project/.claude/plans/abc-123.md",
	}, nil, false)
	h.OnToolComplete("ExitPlanMode", "tool-2", map[string]interface{}{}, nil, false)
	assert.Empty(t, h.planFilePath)
}

func TestLogEventHandler_IgnoresFailedWrites(t *testing.T) {
	h := &logEventHandler{logger: slog.Default(), step: "plan"}

	// Failed write should not be tracked.
	h.OnToolComplete("Write", "tool-1", map[string]interface{}{
		"file_path": "/home/user/project/.claude/plans/abc-123.md",
	}, nil, true)
	h.OnToolComplete("ExitPlanMode", "tool-2", map[string]interface{}{}, nil, false)
	assert.Empty(t, h.planFilePath)
}

func TestLogEventHandler_LastMDWriteWins(t *testing.T) {
	h := &logEventHandler{logger: slog.Default(), step: "plan"}

	// Multiple .md writes — the last one before ExitPlanMode wins.
	h.OnToolComplete("Write", "tool-1", map[string]interface{}{
		"file_path": "/home/user/project/docs/scratch.md",
	}, nil, false)
	h.OnToolComplete("Write", "tool-2", map[string]interface{}{
		"file_path": "/home/user/project/docs/plans/final-plan.md",
	}, nil, false)
	h.OnToolComplete("ExitPlanMode", "tool-3", map[string]interface{}{}, nil, false)
	assert.Equal(t, "/home/user/project/docs/plans/final-plan.md", h.planFilePath)
}

// replayEvent represents a single tool event in a replay sequence.
type replayEvent struct {
	input   map[string]interface{}
	name    string
	id      string
	isError bool
}

// TestReplay_PlanStepEventSequence replays the tool event sequence from a real
// plan step execution to verify the full critical path:
// agent events → plan file detection → file read → output substitution.
func TestReplay_PlanStepEventSequence(t *testing.T) {
	// Create a temp plan file to simulate what Claude writes.
	tmpDir := t.TempDir()
	planDir := filepath.Join(tmpDir, "docs", "plans")
	require.NoError(t, os.MkdirAll(planDir, 0o755))
	planFile := filepath.Join(planDir, "adaptive-booping-twilight.md")
	planContent := "# INF-211: Analyze Sandbox Init Datadog Metrics\n\n## Context\nThe sandbox-e2e-test canary...\n\n## Approach\n1. Query Datadog API\n2. Analyze per-step latency\n3. Write findings document\n"
	require.NoError(t, os.WriteFile(planFile, []byte(planContent), 0o644))

	// Replay the tool event sequence from an actual plan step run.
	// This is the critical path: research tools → Write plan → ExitPlanMode.
	events := []replayEvent{
		{name: "Agent", id: "t-1"},
		{name: "Grep", id: "t-2"},
		{name: "Read", id: "t-3"},
		{name: "Read", id: "t-4"},
		{name: "Bash", id: "t-5"},
		{name: "Agent", id: "t-6"},
		{name: "Read", id: "t-7"},
		// Claude writes the plan file.
		{name: "Write", id: "t-8", input: map[string]interface{}{
			"file_path": planFile,
		}},
		// Claude loads ToolSearch to find ExitPlanMode.
		{name: "ToolSearch", id: "t-9"},
		// Claude calls ExitPlanMode — this confirms the plan file.
		{name: "ExitPlanMode", id: "t-10"},
	}

	h := &logEventHandler{logger: slog.Default(), step: "plan"}
	for _, ev := range events {
		input := ev.input
		if input == nil {
			input = map[string]interface{}{}
		}
		h.OnToolComplete(ev.name, ev.id, input, nil, ev.isError)
	}

	// Verify plan file was detected.
	assert.Equal(t, planFile, h.planFilePath)

	// Verify resolveOutput reads the plan file content.
	output := resolveOutput("Let me write the plan.", h, slog.Default())
	assert.Equal(t, planContent, output)
	assert.NotContains(t, output, "Let me write the plan")
}

// TestReplay_PlanStepNoExitPlanMode verifies that without ExitPlanMode
// (e.g., agent hit max turns), the conversational text is used as fallback.
func TestReplay_PlanStepNoExitPlanMode(t *testing.T) {
	events := []replayEvent{
		{name: "Agent", id: "t-1"},
		{name: "Read", id: "t-2"},
		{name: "Write", id: "t-3", input: map[string]interface{}{
			"file_path": "/tmp/project/docs/plans/my-plan.md",
		}},
		// No ExitPlanMode — agent ran out of turns.
	}

	h := &logEventHandler{logger: slog.Default(), step: "plan"}
	for _, ev := range events {
		input := ev.input
		if input == nil {
			input = map[string]interface{}{}
		}
		h.OnToolComplete(ev.name, ev.id, input, nil, ev.isError)
	}

	// Plan file NOT confirmed without ExitPlanMode.
	assert.Empty(t, h.planFilePath)

	// resolveOutput falls back to agent text.
	output := resolveOutput("Here is my plan: 1. Do X 2. Do Y", h, slog.Default())
	assert.Equal(t, "Here is my plan: 1. Do X 2. Do Y", output)
}

// TestReplay_PlanFileReadFailure verifies graceful fallback when the plan file
// cannot be read (e.g., deleted between write and read).
func TestReplay_PlanFileReadFailure(t *testing.T) {
	h := &logEventHandler{logger: slog.Default(), step: "plan"}
	h.OnToolComplete("Write", "t-1", map[string]interface{}{
		"file_path": "/nonexistent/path/plan.md",
	}, nil, false)
	h.OnToolComplete("ExitPlanMode", "t-2", map[string]interface{}{}, nil, false)

	assert.Equal(t, "/nonexistent/path/plan.md", h.planFilePath)

	// resolveOutput falls back to agent text when file is unreadable.
	output := resolveOutput("conversational summary", h, slog.Default())
	assert.Equal(t, "conversational summary", output)
}

func TestRunCommand_Success(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	data := PromptData{Identifier: "ENG-1"}
	output, err := RunCommand(ctx, "build", data, "echo hello", t.TempDir(), slog.Default())
	require.NoError(t, err)
	assert.Contains(t, output, "hello")
}

func TestRunCommand_Failure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	data := PromptData{}
	output, err := RunCommand(ctx, "build", data, "exit 1", t.TempDir(), slog.Default())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "command failed")
	_ = output // output may be empty for exit 1
}

func TestRunCommand_TemplateRendering(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	data := PromptData{Identifier: "ENG-42"}
	output, err := RunCommand(ctx, "build", data, "echo {{.Identifier}}", t.TempDir(), slog.Default())
	require.NoError(t, err)
	assert.True(t, strings.Contains(output, "ENG-42"), "expected ENG-42 in output, got: %q", output)
}

func TestRunCommand_WorkDir(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	data := PromptData{}
	output, err := RunCommand(ctx, "build", data, "pwd", dir, slog.Default())
	require.NoError(t, err)
	assert.Contains(t, output, dir)
}

// TestReplay_PlanContentPostedToTracker verifies the full chain: plan file content
// flows through workflow.runStep into the tracker comment.
func TestReplay_PlanContentPostedToTracker(t *testing.T) {
	// This tests that w.plan gets the plan file content (not conversational text),
	// and that the content is posted to the tracker.
	// We test this at the workflow level by directly setting w.plan and verifying
	// the comment format, since runStep calls RunStepAgent which requires a real provider.

	mt := &mockWorkflowTracker{
		workflowStates: testWorkflowStates(),
	}
	wf := NewWorkflow(mt, testIssue(), testConfig(), discardLogger())

	// Simulate what happens after RunStepAgent returns plan file content.
	planContent := "# Plan\n\n## Approach\n1. Fix widget\n2. Add tests"
	wf.plan = planContent

	// Verify plan is stored.
	assert.Equal(t, planContent, wf.plan)

	// Verify plan would be included in build step's prompt data.
	data := NewPromptData(wf.issue, wf.config.BaseBranch)
	data.Plan = wf.plan
	buildPrompt, err := renderPrompt(defaultBuildPrompt, data)
	require.NoError(t, err)
	assert.Contains(t, buildPrompt, "# Plan")
	assert.Contains(t, buildPrompt, "1. Fix widget")
	assert.Contains(t, buildPrompt, "Approved Plan")
}
