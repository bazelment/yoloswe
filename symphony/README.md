# Symphony

Orchestrator daemon that polls a Linear project for issues and dispatches them to Codex agents running in isolated workspaces. Each issue gets its own workspace directory, a rendered prompt from a configurable template, and an agent session that runs until the task completes, fails, or times out. Failed tasks retry with exponential backoff.

## Architecture

```
WORKFLOW.md (config + prompt template)
        |
        v
  ┌─────────────┐       ┌──────────────┐
  │ Orchestrator │──────►│ Linear API   │  poll for candidate issues
  │  event loop  │◄──────│ (GraphQL)    │  reconcile running issues
  └──────┬───────┘       └──────────────┘
         │
    dispatch per issue
         │
  ┌──────┴───────┐
  │  Worker      │  1. create/reuse workspace
  │  goroutine   │  2. run lifecycle hooks
  │              │  3. render prompt template
  │              │  4. start Codex session
  │              │  5. stream events until done
  └──────────────┘
         │
  ┌──────┴───────┐
  │ HTTP server  │  GET /           dashboard (auto-refresh)
  │ (optional)   │  GET /api/v1/state   JSON snapshot
  │              │  GET /api/v1/refresh  trigger poll
  └──────────────┘
```

## Packages

| Package | Purpose |
|---------|---------|
| `cmd/symphony` | CLI entry point. Loads WORKFLOW.md, creates tracker + orchestrator, starts HTTP server |
| `orchestrator` | Single-authority event loop: poll, dispatch, reconcile, retry |
| `agent` | Manages Codex app-server subprocess (JSON-RPC over stdio) |
| `config` | Parses WORKFLOW.md config section with env var expansion and hot reload |
| `model` | Domain types: Issue, Workspace, RunAttempt, RunStatus, LiveSession |
| `tracker` | Issue tracker interface |
| `tracker/linear` | Linear GraphQL client with cursor-based pagination |
| `workspace` | Per-issue directory creation, path safety, lifecycle hooks |
| `prompt` | Jinja2-like template rendering for agent prompts |
| `http` | HTTP server with JSON API and HTML dashboard |
| `logging` | Structured logging helpers |
| `integration` | Integration tests with fake Codex binary |

## Configuration

Symphony reads a `WORKFLOW.md` file with YAML front matter for config and markdown body for the prompt template.

```yaml
---
config:
  tracker:
    kind: linear
    endpoint: https://api.linear.app/graphql
    api_key: $LINEAR_API_KEY
    project_slug: myteam/myproject
    active_states: [Todo, "In Progress"]
    terminal_states: [Done, Cancelled, Closed]

  polling:
    interval_ms: 30000

  workspace:
    root: ~/symphony_workspaces

  hooks:
    after_create: "bash .symphony/setup.sh"
    before_run: "source .venv/bin/activate"
    after_run: "git push origin"
    timeout_ms: 120000

  agent:
    max_concurrent_agents: 10
    max_turns: 20
    max_retry_backoff_ms: 300000

  codex:
    command: "codex app-server"
    approval_policy: auto
    turn_timeout_ms: 3600000
    stall_timeout_ms: 300000

  server:
    port: 8080
---

You are a software engineer working on issue {{ issue.identifier }}.
Title: {{ issue.title }}

{% if issue.description %}
Description:
{{ issue.description }}
{% endif %}
```

Environment variables (`$VAR_NAME`) are resolved at load time. Config hot-reloads without restarting the daemon.

## Usage

```bash
# Build
bazel build //symphony/cmd/symphony

# Run with default ./WORKFLOW.md
symphony

# Custom workflow file and HTTP port
symphony -port 8080 /path/to/workflow.md
```

The HTTP dashboard at `http://127.0.0.1:{port}/` shows running sessions, retry queue, and aggregate token usage. Auto-refreshes every 5 seconds.

## Prompt Template Variables

| Variable | Description |
|----------|-------------|
| `{{ issue.id }}` | Tracker-internal ID |
| `{{ issue.identifier }}` | Human-readable identifier (e.g., `PROJ-123`) |
| `{{ issue.title }}` | Issue title |
| `{{ issue.description }}` | Issue body text |
| `{{ issue.priority }}` | Priority level |
| `{{ issue.state }}` | Current state name |
| `{{ issue.branch_name }}` | Suggested branch name |
| `{{ issue.url }}` | Web URL to the issue |
| `{{ issue.labels }}` | Comma-separated labels |
| `{{ issue.blocked_by }}` | Blocking issue identifiers |
| `{{ attempt }}` | Current retry attempt number |

Conditionals: `{% if issue.description %}...{% endif %}`
