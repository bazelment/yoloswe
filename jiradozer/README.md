# Jiradozer

Issue-driven development workflow CLI. Takes an issue from a tracker (Linear, etc.), runs an AI agent through plan/build/validate/ship steps, and gates each step on human approval via issue comments.

## Quick start

```bash
# Build
bazel build //jiradozer/cmd/jiradozer

# Run
bazel-bin/jiradozer/cmd/jiradozer/jiradozer --issue ENG-123
```

## Prerequisites

- A supported AI agent CLI installed (`claude`, `codex`, `gemini`, or `agent` for Cursor)
- `gh` CLI authenticated (`gh auth login`)
- A Linear API key (or another supported tracker)

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
  system_prompt: |
    You are analyzing an issue and creating an implementation plan.

build:
  max_turns: 30
  system_prompt: |
    You are implementing changes based on an approved plan.

validation:
  commands:
    - "bazel test //..."
    - "scripts/lint.sh"
  timeout_seconds: 300

states:
  in_progress: "In Progress"
  in_review: "In Review"
  done: "Done"
```

The `states` section maps logical workflow states to your tracker's state names. Jiradozer uses these to transition the issue as it moves through the workflow.

## CLI flags

```
jiradozer --issue ENG-123 [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--issue` | (required) | Issue identifier (e.g. `ENG-123`) |
| `--config` | `jiradozer.yaml` | Path to config file |
| `--work-dir` | from config | Working directory for the agent |
| `--model` | from config | Agent model ID (overrides config) |
| `--poll-interval` | from config | How often to check for new comments |
| `--max-budget` | from config | Max spend in USD |
| `--skip-to` | | Skip to a step: `plan`, `build`, `validate`, `ship` |
| `--verbose` | `false` | Debug logging |
| `--dry-run` | `false` | Run plan step only, print to stdout without posting |

## Workflow

Each run goes through four steps. After each step, results are posted as a comment on the issue and the tool waits for human feedback:

1. **Plan** -- Agent runs in plan mode, produces an implementation plan
2. **Build** -- Agent executes the approved plan
3. **Validate** -- Runs configured validation commands (tests, lint, etc.)
4. **Ship** -- Creates a GitHub PR via `gh`

At each review gate, comment on the issue:

- `approve` or `lgtm` -- proceed to the next step
- `redo` -- re-run the current step
- Any other text -- treated as feedback, incorporated into the next agent run

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

To add a new tracker, implement the `IssueTracker` interface in a new subpackage under `tracker/` and add a case to `createTracker()` in `cmd/jiradozer/main.go`.

## Examples

```bash
# Dry run -- plan only, no posting
jiradozer --issue ENG-123 --dry-run

# Use a specific model
jiradozer --issue ENG-123 --model opus

# Use Codex instead of Claude
jiradozer --issue ENG-123 --model gpt-5.3-codex

# Custom config and working directory
jiradozer --issue ENG-123 --config ~/myproject/jiradozer.yaml --work-dir ~/myproject

# Skip planning, jump straight to build (e.g. resuming after a crash)
jiradozer --issue ENG-123 --skip-to build
```
