package engine

import (
	"bytes"
	"fmt"
	"text/template"

	"github.com/bazelment/yoloswe/medivac/issue"
)

var fixTmpl = template.Must(template.New("fix").Parse(fixPromptTmpl))
var groupFixTmpl = template.Must(template.New("groupfix").Parse(groupFixPromptTmpl))

// fixPromptData holds all data needed to render the single-issue fix prompt.
type fixPromptData struct {
	Issue        *issue.Issue
	Branch       string
	FileWithLine string
}

// groupFixPromptData holds all data needed to render the group fix prompt.
type groupFixPromptData struct {
	Group    IssueGroup
	Branch   string
	Failures []groupFailure
}

// groupFailure is a template-friendly view of an issue in a group.
type groupFailure struct {
	Issue        *issue.Issue
	FileWithLine string
	Details      string
	Index        int
}

// buildFixPrompt constructs the prompt for a fix agent given an issue.
func buildFixPrompt(iss *issue.Issue, branch string) string {
	data := fixPromptData{
		Issue:        iss,
		Branch:       branch,
		FileWithLine: fileWithLine(iss),
	}
	var buf bytes.Buffer
	fixTmpl.Execute(&buf, data)
	return buf.String()
}

// buildGroupFixPrompt constructs the prompt for a fix agent handling multiple
// related issues. It lists all issues in the group so the agent can fix them
// in a single pass.
func buildGroupFixPrompt(group IssueGroup, branch string) string {
	var failures []groupFailure
	for i, iss := range group.Issues {
		failures = append(failures, groupFailure{
			Index:        i + 1,
			Issue:        iss,
			FileWithLine: fileWithLine(iss),
			Details:      truncate(iss.Details, 200),
		})
	}
	data := groupFixPromptData{
		Group:    group,
		Branch:   branch,
		Failures: failures,
	}
	var buf bytes.Buffer
	groupFixTmpl.Execute(&buf, data)
	return buf.String()
}

func fileWithLine(iss *issue.Issue) string {
	if iss.File == "" {
		return ""
	}
	if iss.Line > 0 {
		return fmt.Sprintf("%s:%d", iss.File, iss.Line)
	}
	return iss.File
}

// truncate shortens a string to maxLen characters with ellipsis.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

const fixPromptTmpl = `You are a CI failure medivac agent. Your goal is to fix a specific CI failure in this repository.

## Failure Details

- **Category:** {{.Issue.Category}}
- **Summary:** {{.Issue.Summary}}
{{- if .Issue.File}}
- **File:** {{.FileWithLine}}
{{- end}}
- **Branch:** {{.Branch}}
- **Times seen:** {{.Issue.SeenCount}}
{{- if .Issue.Details}}

### Error Details

` + "```" + `
{{.Issue.Details}}
` + "```" + `
{{- end}}

## Instructions

1. **Read CLAUDE.md** in the repo root first if it exists. Follow its instructions for build/test/lint commands.
2. **Investigate** the failure by reading the relevant files and understanding the root cause.
3. **Fix** the issue with the minimal change needed.
4. **Verify** your fix by running the appropriate lint, build, or test commands as described in CLAUDE.md or as appropriate for this project.
5. **Commit** your changes with a clear commit message explaining the fix.
6. **Push** to the remote branch.
7. **Report your analysis** at the very end of your response, in exactly this format:

` + "```" + `
<ANALYSIS>
reasoning: <1-2 sentences explaining why the failure happened>
root_cause: <the underlying cause, not just the symptom>
fix_applied: <yes or no>
fix_options:
- <label>: <description of this approach>
- <label>: <description of another approach>
</ANALYSIS>
` + "```" + `

If you cannot fix the issue with code changes (e.g., it requires manual config, secrets, external service changes), set fix_applied to "no" and list the options anyway. This analysis is critical -- always include it.

## Important Rules

- Read and follow the CLAUDE.md instructions in the repo root (if present)
- Make minimal, focused changes -- do not refactor unrelated code
- If you cannot fix the issue, explain why clearly
`

const groupFixPromptTmpl = `You are a CI failure medivac agent. Your goal is to fix a GROUP of related CI failures in this repository.

## Failure Group

- **Group key:** {{.Group.Key}}
- **Issues in group:** {{len .Group.Issues}}
- **Branch:** {{.Branch}}

### Individual Failures
{{range .Failures}}
#### Failure {{.Index}}
- **Category:** {{.Issue.Category}}
- **Summary:** {{.Issue.Summary}}
{{- if .Issue.File}}
- **File:** {{.FileWithLine}}
{{- end}}
{{- if .Details}}
- **Details:** ` + "`{{.Details}}`" + `
{{- end}}
{{end}}
## Instructions

1. **Read CLAUDE.md** in the repo root first if it exists. Follow its instructions for build/test/lint commands.
2. **Investigate** ALL failures listed above. They share a common root cause or error pattern.
3. **Fix** all issues with the minimal set of changes needed.
4. **Verify** your fix by running the appropriate lint, build, or test commands as described in CLAUDE.md or as appropriate for this project.
5. **Commit** your changes with a clear commit message explaining the fix.
6. **Push** to the remote branch.
7. **Report your analysis** at the very end of your response, in exactly this format:

` + "```" + `
<ANALYSIS>
reasoning: <1-2 sentences explaining why the failure happened>
root_cause: <the underlying cause, not just the symptom>
fix_applied: <yes or no>
fix_options:
- <label>: <description of this approach>
- <label>: <description of another approach>
</ANALYSIS>
` + "```" + `

If you cannot fix the issue with code changes (e.g., it requires manual config, secrets, external service changes), set fix_applied to "no" and list the options anyway. This analysis is critical -- always include it.

## Important Rules

- Read and follow the CLAUDE.md instructions in the repo root (if present)
- Make minimal, focused changes -- do not refactor unrelated code
- Fix ALL listed failures, not just the first one
- If you cannot fix the issue, explain why clearly
`
