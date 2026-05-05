package jiradozer

// Canonical step prompts and comment templates emitted by `jiradozer
// bootstrap`. The runtime reads prompts from the user's YAML; these constants
// are the source of truth for what bootstrap writes and what tests render
// against.

const BootstrapPlanPrompt = `Issue: {{.Identifier}} — {{.Title}}
{{- if .Description}}

Description:
{{.Description}}
{{- end}}
{{- if .URL}}

URL: {{.URL}}
{{- end}}
{{- if .Labels}}
Labels: {{.Labels}}
{{- end}}

Create a detailed implementation plan for this issue. Include: files to modify, approach, testing strategy, and any risks.`

const BootstrapBuildPrompt = `Issue: {{.Identifier}} — {{.Title}}
{{- if .Description}}

Description:
{{.Description}}
{{- end}}
{{- if .Plan}}

Approved Plan:
{{.Plan}}

Implement the changes described in the approved plan above.
{{- else}}

No plan is available. Implement the changes based on the issue description above.
{{- end}}`

const BootstrapValidatePrompt = `Issue: {{.Identifier}} — {{.Title}}

Run the project's tests and linters to validate the changes. Fix any failures you find. Report what passed and what you fixed.`

const BootstrapCreatePRPrompt = `Your job is purely git + gh: commit and push, then create or update the PR. Do NOT run lint, build, or tests here — the next step (validate) is responsible for the full gate sweep, and re-running them now wastes minutes per issue without changing the code that ships.

First, check for any uncommitted changes (staged or unstaged, including untracked files).
- If there are uncommitted changes: stage them, commit with a clear message referencing the work done, and push to the remote.
- If there are no uncommitted changes but unpushed commits: push to the remote.

Then, check if a pull request already exists for the current branch against {{.BaseBranch}}.
- If a PR exists: update its description to reflect the current state of the code. Report the PR URL.
- If no PR exists: create one against {{.BaseBranch}} with a clear title and description. Report the PR URL.`

const BootstrapShipPrompt = `Issue: {{.Identifier}} — {{.Title}}
{{- if .URL}}

Linear: {{.URL}}
{{- end}}

Check if a pull request already exists for the current branch against {{.BaseBranch}}.
- If a PR exists: update its description if needed and ensure it is ready for review. Report the PR URL.
- If no PR exists: create one using gh pr create with "{{.Identifier}}: {{.Title}}" as the title.`

const BootstrapCompleteCommentTemplate = `## {{.Heading}} Complete

{{.Output}}`

const BootstrapRoundCommentTemplate = `## {{.Heading}} Round {{.Round}}/{{.TotalRounds}}

{{.Output}}`
