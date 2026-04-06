package jiradozer

import (
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
