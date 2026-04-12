# Jiradozer

Issue-driven development workflow CLI. Takes an issue from a tracker (Linear, GitHub Issues, etc.) or a plain text description, runs an AI agent through plan/build/validate/ship steps, and gates each step on human approval via issue comments.

## Quick start

```bash
# Build
bazel build //jiradozer/cmd/jiradozer

# Run from a tracker issue
bazel-bin/jiradozer/cmd/jiradozer/jiradozer --issue ENG-123

# Run from a description (no tracker needed)
bazel-bin/jiradozer/cmd/jiradozer/jiradozer \
  --description "Add retry logic to the HTTP client for 5xx errors" \
  --work-dir ~/myproject
```

## Prerequisites

- A supported AI agent CLI installed (`claude`, `codex`, `gemini`, or `agent` for Cursor)
- `gh` CLI authenticated (`gh auth login`)
- A Linear API key, or `gh` CLI authenticated for GitHub Issues — **or** use `--description` for local mode (no tracker needed)

## Configuration

Create a `jiradozer.yaml` in your working directory:

```yaml
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY    # environment variable reference

agent:
  model: sonnet               # or opus, haiku, gpt-5.3-codex, gemini-3.1-pro-preview, cursor-default

work_dir: .
base_branch: main
poll_interval: 15s
max_budget_usd: 50.0

plan:
  max_turns: 10
  permission_mode: plan       # default for plan step

build:
  max_turns: 30
  permission_mode: bypass     # default for build step

validate:
  max_turns: 10
  permission_mode: bypass     # default for validate step

ship:
  max_turns: 10
  permission_mode: bypass     # default for ship step

states:
  in_progress: "In Progress"
  in_review: "In Review"
  done: "Done"
```

The `states` section maps logical workflow states to your tracker's state names. Jiradozer uses these to transition the issue as it moves through the workflow.

### Step configuration

Each step (`plan`, `build`, `validate`, `ship`) is a self-contained agent session definition. All fields are optional with sensible defaults; unset `model` and `max_budget_usd` inherit from top-level config.

| Field | plan | build | validate | ship | Description |
|-------|------|-------|----------|------|-------------|
| `prompt` | built-in | built-in | built-in | built-in | Go `text/template` for the initial prompt |
| `system_prompt` | | | | | Optional system prompt |
| `model` | inherit | inherit | inherit | inherit | Model override for this step |
| `permission_mode` | `plan` | `bypass` | `bypass` | `bypass` | Agent permission mode |
| `max_turns` | `10` | `30` | `10` | `10` | Max agent turns (Claude only) |
| `max_budget_usd` | inherit | inherit | inherit | inherit | Budget override (Claude only) |

> **Note**: `max_turns`, `max_budget_usd`, and session resume (redo/feedback) are currently only honored by the Claude provider. Other providers (Codex, Gemini, Cursor) accept these options without error but do not enforce them.

### Prompt templates

The `prompt` field supports Go `text/template` syntax with issue data:

| Variable | Description |
|----------|-------------|
| `{{.Identifier}}` | Issue identifier (e.g. `ENG-123`) |
| `{{.Title}}` | Issue title |
| `{{.Description}}` | Issue description (empty string if unset) |
| `{{.URL}}` | Issue URL (empty string if unset) |
| `{{.Labels}}` | Comma-separated labels |
| `{{.Plan}}` | Approved plan text (available after plan step) |
| `{{.BuildOutput}}` | Build output text (available after build step) |
| `{{.BaseBranch}}` | Base branch name (e.g. `main`) |

Example custom prompt:

```yaml
plan:
  prompt: |
    Implement {{.Identifier}}: {{.Title}}
    {{if .Description}}{{.Description}}{{end}}
    Focus on backend changes only. Skip frontend.
  model: opus
  max_turns: 5
```

If `prompt` is omitted, a built-in default is used. The prompt is only rendered for the **first** execution in a phase. When a reviewer requests a redo or leaves feedback, the existing agent session is resumed and the feedback text is sent directly — no re-rendering.

## CLI flags

```
jiradozer --issue ENG-123 [flags]
jiradozer --description "task description" [flags]
```

One of `--issue`, `--filter`, or `--description`/`--description-file` is required. `--issue` and `--description` are mutually exclusive; `--issue` takes precedence over `source.filters` in the config file (so you can keep `source.filters` for multi-issue mode and still use `--issue` for single issues).

| Flag | Default | Description |
|------|---------|-------------|
| `--issue` | | Issue identifier (e.g. `ENG-123` or `owner/repo#42`) |
| `--description` | | Task description for local mode (no external tracker needed) |
| `--description-file` | | Read task description from file (use `-` for stdin) |
| `--filter` | | Issue filter as `key=value` (repeatable, e.g. `--filter team=ENG --filter state=Todo,Backlog`) |
| `--config` | `jiradozer.yaml` | Path to config file |
| `--work-dir` | from config | Working directory for the agent |
| `--model` | from config | Agent model ID (overrides config) |
| `--poll-interval` | from config | How often to check for new comments |
| `--max-budget` | from config | Max spend in USD |
| `--max-concurrent` | from config | Max concurrent workflows |
| `--branch-prefix` | from config | Worktree branch prefix |
| `--auto-approve` | | Auto-approve review steps (`plan,build,validate,ship` or `all`) |
| `--run-step` | | Run a single step and exit (for debugging): `plan`, `build`, `validate`, `ship` |
| `--verbose` | `false` | Debug logging |

## Workflow

Each run goes through four steps. After each step, results are posted as a comment on the issue and the tool waits for human feedback:

1. **Plan** -- Agent creates an implementation plan
2. **Build** -- Agent implements the approved plan
3. **Validate** -- Agent runs tests/linters and fixes any failures
4. **Ship** -- Agent creates a GitHub PR

At each review gate, comment on the issue:

- `approve` or `lgtm` -- proceed to the next step
- `redo` -- re-run the current step
- Any other text -- treated as feedback, incorporated into the next agent run

### Worktree lifecycle (team mode)

In team mode (`--filter`), each issue gets its own git worktree under a dedicated branch (`<branch-prefix>/<identifier>`). The worktree lifecycle is:

- **Created** when the issue is picked up and `Start()` is called.
- **Left intact** when the workflow completes successfully (`StepDone`). The ship step typically opens a PR but does not merge it — the branch must remain until the PR is reviewed and merged.
- **Removed** only on workflow failure (`StepFailed`). Failed runs clean up the worktree and branch so a future retry starts from a clean state.

After merging the PR, remove the worktree manually:

```bash
# Remove worktree and branch after merge
git worktree remove ~/worktrees/<repo>/<branch-prefix>/<identifier>
git branch -d <branch-prefix>/<identifier>
```

Or use `wt remove` if you have the wt tool configured.

## Supported agents

Jiradozer uses the `multiagent/agent.Provider` interface, so any agent backend that bramble supports works out of the box:

| Provider | Model IDs |
|----------|-----------|
| Claude | `opus`, `sonnet`, `haiku` |
| Codex | `gpt-5.3-codex`, `gpt-5.2`, `gpt-5.1-codex-max` |
| Gemini | `gemini-3.1-pro-preview`, `gemini-3-pro-preview`, `gemini-2.5-pro`, ... |
| Cursor | `cursor-default` |

The provider is auto-detected from the model ID.

## Supported trackers

The `tracker.IssueTracker` interface is pluggable. Currently implemented:

- **Linear** (`tracker.kind: linear`) -- reads/writes issues and comments via GraphQL
- **GitHub Issues** (`tracker.kind: github`) -- reads/writes issues and comments via `gh` CLI
- **Local** (`--description` flag) -- file-backed tracker with no external dependencies

### Local mode

When you pass `--description` (or `--description-file`), jiradozer runs in local mode:

- No config file or API key required (uses sensible defaults)
- All steps auto-approve by default (override with `--auto-approve=none`)
- A concise title is generated automatically from the description via a lightweight LLM call
- State is persisted to `<work-dir>/.jiradozer/issues/` as JSON files, so you can inspect or resume workflows
- The fixed workflow states are: Todo → In Progress → In Review → Done

```bash
# From a string
jiradozer --description "Add rate limiting to the /api/v2 endpoints" --work-dir ~/myproject

# From a file
jiradozer --description-file spec.md --work-dir ~/myproject

# From stdin
cat spec.md | jiradozer --description-file - --work-dir ~/myproject

# Inspect state after a run
cat ~/myproject/.jiradozer/issues/local-1.json | jq .
```

### GitHub Issues mode

When `tracker.kind` is `github`, jiradozer uses the `gh` CLI for authentication and API access. No API key is needed — just `gh auth login`.

```yaml
tracker:
  kind: github

source:
  filters:
    team: "owner/repo"
    state: "Todo"
    label: "jiradozer"         # optional: only pick up issues with this label
  max_concurrent: 3
```

```bash
# Single issue
jiradozer --issue owner/repo#42

# Multi-issue mode (discovers open issues)
jiradozer --filter team=owner/repo --filter state=Todo
```

GitHub Issues only has open/closed states, so jiradozer maps workflow states using labels:

| Logical state | GitHub action |
|---------------|---------------|
| In Progress | Issue is open |
| In Review | `in-review` label added |
| Done | Issue is closed |

The default `states` config ("In Progress", "In Review", "Done") works without changes.

To add a new tracker, implement the `IssueTracker` interface in a new subpackage under `tracker/` and add a case to `createTracker()` in `cmd/jiradozer/main.go`.

## Examples

```bash
# Run from a description (no tracker, no config needed)
jiradozer --description "Create a hello.txt file containing 'hello world'" --work-dir /tmp/demo

# Run from a tracker issue
jiradozer --issue ENG-123

# Run a single step for debugging (no tracker interaction)
jiradozer --issue ENG-123 --run-step plan
jiradozer --issue ENG-123 --run-step build

# Use a specific model
jiradozer --issue ENG-123 --model opus

# Use Codex instead of Claude
jiradozer --issue ENG-123 --model gpt-5.3-codex

# Custom config and working directory
jiradozer --issue ENG-123 --config ~/myproject/jiradozer.yaml --work-dir ~/myproject
```
