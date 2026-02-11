package engine

import (
	"bytes"
	"fmt"
	"strings"
	"text/template"

	"github.com/bazelment/yoloswe/medivac/issue"
)

// maxClaudeMDLen is the maximum number of characters of CLAUDE.md to include
// in the fix prompt. This prevents the prompt from becoming excessively large.
const maxClaudeMDLen = 4000

var fixTmpl = template.Must(template.New("fix").Parse(fixPromptTmpl))
var groupFixTmpl = template.Must(template.New("groupfix").Parse(groupFixPromptTmpl))

// fixPromptData holds all data needed to render the single-issue fix prompt.
type fixPromptData struct {
	Issue     *issue.Issue
	Branch    string
	BuildInfo BuildInfo
	FileWithLine string
	VerifyCmds   []verifyCmd
}

// groupFixPromptData holds all data needed to render the group fix prompt.
type groupFixPromptData struct {
	Group      IssueGroup
	Branch     string
	BuildInfo  BuildInfo
	Failures   []groupFailure
	VerifyCmds []verifyCmd
}

// groupFailure is a template-friendly view of an issue in a group.
type groupFailure struct {
	Index        int
	Issue        *issue.Issue
	FileWithLine string
	Details      string
}

// verifyCmd is a labeled command for the verify step.
type verifyCmd struct {
	Label string
	Cmd   string
}

// buildFixPrompt constructs the prompt for a fix agent given an issue.
// It uses detected build system information to generate correct commands.
func buildFixPrompt(iss *issue.Issue, branch string, buildInfo BuildInfo) string {
	data := fixPromptData{
		Issue:      iss,
		Branch:     branch,
		BuildInfo:  buildInfo,
		FileWithLine: fileWithLine(iss),
		VerifyCmds: verifyCmds(string(iss.Category), buildInfo),
	}
	if buildInfo.ClaudeMD != "" {
		data.BuildInfo.ClaudeMD = TruncateClaudeMD(buildInfo.ClaudeMD, maxClaudeMDLen)
	}
	var buf bytes.Buffer
	fixTmpl.Execute(&buf, data)
	return buf.String()
}

// buildGroupFixPrompt constructs the prompt for a fix agent handling multiple
// related issues. It lists all issues in the group so the agent can fix them
// in a single pass.
func buildGroupFixPrompt(group IssueGroup, branch string, buildInfo BuildInfo) string {
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
		Group:      group,
		Branch:     branch,
		BuildInfo:  buildInfo,
		Failures:   failures,
		VerifyCmds: verifyCmds(string(group.Leader().Category), buildInfo),
	}
	if buildInfo.ClaudeMD != "" {
		data.BuildInfo.ClaudeMD = TruncateClaudeMD(buildInfo.ClaudeMD, maxClaudeMDLen)
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

// verifyCmds returns the verification commands based on failure category.
func verifyCmds(category string, info BuildInfo) []verifyCmd {
	switch {
	case strings.HasPrefix(category, "lint/"):
		return []verifyCmd{{Label: "Run", Cmd: info.LintCmd}}
	case strings.HasPrefix(category, "build/"):
		return []verifyCmd{{Label: "Run", Cmd: info.BuildCmd}}
	case strings.HasPrefix(category, "test/"):
		return []verifyCmd{{Label: "Run", Cmd: info.TestCmd}}
	default:
		return []verifyCmd{
			{Label: "Lint", Cmd: info.LintCmd},
			{Label: "Build", Cmd: info.BuildCmd},
		}
	}
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

## Build System

Detected build system: **{{.BuildInfo.System}}**

## Instructions

1. **Read CLAUDE.md** in the repo root first if it exists. Follow its instructions for build/test/lint commands.
2. **Investigate** the failure by reading the relevant files and understanding the root cause.
3. **Fix** the issue with the minimal change needed.
4. **Verify** your fix by running the appropriate checks:
{{- range .VerifyCmds}}
   - {{.Label}}: ` + "`{{.Cmd}}`" + `
{{- end}}
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
{{- range .BuildInfo.ExtraRules}}
- {{.}}
{{- end}}
{{- if .BuildInfo.ClaudeMD}}

## Repository Instructions (from CLAUDE.md)

The following are the project-specific instructions from CLAUDE.md. **Follow these instructions as your primary guide for build, test, and lint commands.**

` + "```" + `
{{.BuildInfo.ClaudeMD}}
` + "```" + `
{{- end}}
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
## Build System

Detected build system: **{{.BuildInfo.System}}**

## Instructions

1. **Read CLAUDE.md** in the repo root first if it exists. Follow its instructions for build/test/lint commands.
2. **Investigate** ALL failures listed above. They share a common root cause or error pattern.
3. **Fix** all issues with the minimal set of changes needed.
4. **Verify** your fix by running the appropriate checks:
{{- range .VerifyCmds}}
   - {{.Label}}: ` + "`{{.Cmd}}`" + `
{{- end}}
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
{{- range .BuildInfo.ExtraRules}}
- {{.}}
{{- end}}
{{- if .BuildInfo.ClaudeMD}}

## Repository Instructions (from CLAUDE.md)

The following are the project-specific instructions from CLAUDE.md. **Follow these instructions as your primary guide for build, test, and lint commands.**

` + "```" + `
{{.BuildInfo.ClaudeMD}}
` + "```" + `
{{- end}}
`
