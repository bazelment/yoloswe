# Jiradozer

## Context

We need a CLI tool that drives a complete development workflow from an issue tracker through planning, building, validation, and PR creation — with human-in-the-loop approval at each step via issue comments. The tool is provider-agnostic on two axes:

1. **Issue tracker**: Pluggable interface (Linear first, but extensible to GitHub Issues, Jira, etc.)
2. **Agent backend**: Uses bramble's `multiagent/agent.Provider` interface — supports Claude, Codex, Gemini, Cursor out of the box

## Workflow

```
                    ┌──────────────────────────────────────────┐
                    │                                          │
  ┌─────────┐  ┌───▼────┐  ┌──────────┐  ┌─────────────┐  ┌──▼──┐
  │  Fetch   ├─►│  Plan  ├─►│  Build   ├─►│  Validate   ├─►│ Ship│──► Done
  │  Issue   │  │        │  │          │  │             │  │     │
  └─────────┘  └───┬────┘  └────┬─────┘  └──────┬──────┘  └──┬──┘
                    │            │               │            │
                    ▼            ▼               ▼            ▼
               Post plan   Post summary    Post results   Post PR
               as comment  as comment      as comment     as comment
                    │            │               │            │
                    ▼            ▼               ▼            ▼
               Wait for    Wait for        Wait for      Wait for
               approval    approval        approval      CI + approval
                    │            │               │            │
              ┌────┴────┐ ┌────┴────┐    ┌────┴────┐   ┌──┴───┐
              │redo/feed│ │redo/feed│    │redo/feed│   │redo  │
              │  back   │ │  back   │    │  back   │   │      │
              └─────────┘ └─────────┘    └─────────┘   └──────┘
```

At each review step, humans comment on the issue:
- `approve` / `lgtm` → proceed to next step
- `redo` → re-run current step
- Any other text → incorporate as feedback and redo

## Package Structure

```
jiradozer/
  cmd/jiradozer/
    main.go              # Cobra CLI entrypoint
    BUILD.bazel
  tracker/
    tracker.go           # IssueTracker interface (read + write)
    types.go             # Issue, Comment, WorkflowState types
    linear/
      client.go          # Linear GraphQL implementation
      client_test.go
      queries.go         # GraphQL queries + mutations
    BUILD.bazel
  config.go              # Config struct + YAML loading
  state.go               # Workflow state machine
  state_test.go
  workflow.go            # Core workflow engine
  agent.go               # Thin wrapper around agent.Provider for plan/build
  validation.go          # Validation command runner
  pr.go                  # PR creation via gh CLI
  poller.go              # Comment polling for feedback
  poller_test.go
  go.mod
  BUILD.bazel
```

New top-level module — same pattern as `yoloswe/`. Depends on `multiagent/agent` (provider interface) and `wt` (PR creation).

## Key Components

### 1. Issue Tracker Interface (`tracker/tracker.go`)

The core abstraction — separates the workflow engine from any specific issue system.

```go
package tracker

// IssueTracker is the read+write interface for issue tracking systems.
// Linear is the first implementation; GitHub Issues, Jira, etc. can follow.
type IssueTracker interface {
    // Read operations
    FetchIssue(ctx context.Context, identifier string) (*Issue, error)
    FetchComments(ctx context.Context, issueID string, since time.Time) ([]Comment, error)
    FetchWorkflowStates(ctx context.Context, teamID string) ([]WorkflowState, error)

    // Write operations
    PostComment(ctx context.Context, issueID string, body string) error
    UpdateIssueState(ctx context.Context, issueID string, stateID string) error
}
```

Types in `tracker/types.go`:

```go
type Issue struct {
    ID          string
    Identifier  string   // e.g. "ENG-123"
    Title       string
    Description *string
    State       string
    BranchName  *string
    URL         *string
    Labels      []string
    TeamID      string
}

type Comment struct {
    ID        string
    Body      string
    CreatedAt time.Time
    UserName  string
    IsBot     bool  // true if posted by the API key owner (our bot)
}

type WorkflowState struct {
    ID   string
    Name string
    Type string // "started", "unstarted", "completed", "canceled"
}
```

### 2. Linear Implementation (`tracker/linear/`)

Implements `IssueTracker` using Linear's GraphQL API. Self-contained client (does NOT modify the read-only `symphony/tracker/linear/` — that package's contract is explicitly read-only per spec).

GraphQL operations:
- **Read**: `issue(id:)` query, `issue.comments` query with pagination, `team.states` query
- **Write**: `commentCreate` mutation, `issueUpdate` mutation (state transition)

Follows same HTTP patterns as `symphony/tracker/linear/client.go`: Bearer auth, `application/json`, 30s timeout, cursor-based pagination, typed errors.

### 3. Agent Integration (`agent.go`)

Uses `multiagent/agent.Provider` interface — all bramble-supported agents work automatically.

```go
// runPlanStep runs the agent in plan mode and returns the plan text.
func (w *Workflow) runPlanStep(ctx context.Context, feedback string) (string, error) {
    provider, err := agent.NewProviderForModel(w.agentModel)
    // ...
    result, err := provider.Execute(ctx, prompt, nil,
        agent.WithProviderWorkDir(w.config.WorkDir),
        agent.WithProviderSystemPrompt(w.config.Plan.SystemPrompt),
        agent.WithProviderPermissionMode("plan"),
        agent.WithProviderModel(w.agentModel.ID),
        agent.WithProviderEventHandler(w.eventHandler),
    )
    // Extract plan from result.Text
    return result.Text, nil
}

// runBuildStep runs the agent in bypass mode to execute the plan.
func (w *Workflow) runBuildStep(ctx context.Context, plan string, feedback string) error {
    provider, err := agent.NewProviderForModel(w.agentModel)
    // ...
    prompt := buildExecutionPrompt(w.issue, plan, feedback)
    result, err := provider.Execute(ctx, prompt, nil,
        agent.WithProviderWorkDir(w.config.WorkDir),
        agent.WithProviderSystemPrompt(w.config.Build.SystemPrompt),
        agent.WithProviderPermissionMode("bypass"),
        agent.WithProviderModel(w.agentModel.ID),
        agent.WithProviderEventHandler(w.eventHandler),
    )
    return nil
}
```

The `ExecuteConfig.PermissionMode` already maps correctly for each backend:
- Claude: `"plan"` → `PermissionModePlan`, `"bypass"` → `PermissionModeBypass`
- Codex: `"plan"` → `ApprovalPolicyOnRequest`, `"bypass"` → `ApprovalPolicyNever`
- Gemini/Cursor: equivalent mappings

Key files:
- `multiagent/agent/provider.go:121` — `Provider` interface
- `multiagent/agent/query.go:17` — `NewProviderForModel()` factory
- `multiagent/agent/model_registry.go` — `AllModels`, `ModelByID()`
- `multiagent/agent/claude_provider.go:48` — plan mode mapping
- `multiagent/agent/codex_provider.go:155` — plan mode mapping

For multi-turn (redo with feedback), use `LongRunningProvider` when available:
```go
if lrp, ok := provider.(agent.LongRunningProvider); ok {
    lrp.Start(ctx)
    defer lrp.Stop()
    result, _ := lrp.SendMessage(ctx, feedbackPrompt)
}
```

### 4. State Machine (`state.go`)

Based on `multiagent/planner/state.go` pattern — validated transitions, history, thread-safe.

```go
type WorkflowStep int
const (
    StepInit
    StepPlanning       // Agent running in plan mode
    StepPlanReview     // Plan posted, waiting for human
    StepBuilding       // Agent running in bypass mode
    StepBuildReview    // Build done, waiting for human
    StepValidating     // Running validation commands
    StepValidateReview
    StepShipping       // Creating PR
    StepShipReview     // Waiting for CI + human
    StepDone
    StepFailed
)
```

Transitions with feedback loops:
- `PlanReview → Planning` (redo with feedback)
- `BuildReview → Building` (redo) or `BuildReview → Planning` (back to plan)
- `ValidateReview → Building` (fix failures)
- `ShipReview → Building` (fix CI failures)

### 5. Validation (`validation.go`)

Runs configurable shell commands in the work directory via `os/exec`:

```go
func RunValidation(ctx context.Context, workDir string, commands []string, timeout time.Duration) ([]ValidationResult, error)
```

Commands from config or extracted from issue description (`<!-- validation: ... -->`). Results formatted as markdown for posting as a comment.

### 6. PR Creation (`pr.go`)

Reuses `wt.CreatePR()` (`wt/github.go:199`) and `wt.CheckGitHubAuth()`. Branch name from `issue.BranchName` or generated. Polls CI status via `gh pr checks`.

### 7. Comment Poller (`poller.go`)

Polls tracker every N seconds for new comments. Filters out bot comments. Parses keywords:
- `approve` / `lgtm` / `ship it` → `FeedbackApprove`
- `redo` / `retry` → `FeedbackRedo`
- Anything else → `FeedbackComment` (incorporated into next agent prompt)

### 8. Workflow Engine (`workflow.go`)

```go
type Workflow struct {
    tracker    tracker.IssueTracker
    issue      *tracker.Issue
    state      *StateMachine
    agentModel agent.AgentModel
    config     *Config
    logger     *slog.Logger
}

func (w *Workflow) Run(ctx context.Context) error {
    w.state.Transition(StepPlanning, "start")
    var plan string
    var feedback string

    for {
        switch w.state.Current() {
        case StepPlanning:
            var err error
            plan, err = w.runPlanStep(ctx, feedback)
            w.tracker.PostComment(ctx, w.issue.ID, formatPlan(plan))
            w.state.Transition(StepPlanReview)
            feedback = ""

        case StepPlanReview:
            fb := w.pollForFeedback(ctx)
            if fb.Action == FeedbackApprove {
                w.state.Transition(StepBuilding)
            } else {
                feedback = fb.Message
                w.state.Transition(StepPlanning)
            }

        case StepBuilding:
            err := w.runBuildStep(ctx, plan, feedback)
            w.tracker.PostComment(ctx, w.issue.ID, formatBuildResult(err))
            w.state.Transition(StepBuildReview)
            feedback = ""

        // ... Validate, Ship steps follow same pattern

        case StepDone:
            return nil
        case StepFailed:
            return w.lastError
        }
    }
}
```

## Configuration (`jiradozer.yaml`)

```yaml
tracker:
  kind: linear           # pluggable: "linear", future: "github", "jira"
  api_key: $LINEAR_API_KEY

agent:
  model: sonnet           # any model from agent.AllModels
  # provider auto-detected from model ID

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

## CLI Flags

```
jiradozer --issue ENG-123 [--config jiradozer.yaml] [--work-dir .]
              [--model sonnet] [--poll-interval 15s] [--max-budget 50]
              [--skip-to build] [--verbose] [--dry-run]
```

`--model` accepts any model ID from `agent.AllModels` (opus, sonnet, haiku, gpt-5.3-codex, gemini-3.1-pro-preview, cursor-default, etc.). Provider is auto-detected.

## Files to Create

| File | Purpose |
|------|---------|
| `jiradozer/go.mod` | Module definition |
| `jiradozer/tracker/tracker.go` | `IssueTracker` interface |
| `jiradozer/tracker/types.go` | Issue, Comment, WorkflowState types |
| `jiradozer/tracker/linear/client.go` | Linear GraphQL implementation |
| `jiradozer/tracker/linear/queries.go` | GraphQL query/mutation strings |
| `jiradozer/tracker/linear/client_test.go` | Tests with httptest |
| `jiradozer/config.go` | Config struct + YAML loading |
| `jiradozer/state.go` | State machine |
| `jiradozer/state_test.go` | State machine tests |
| `jiradozer/workflow.go` | Core workflow engine |
| `jiradozer/agent.go` | Provider wrapper for plan/build |
| `jiradozer/validation.go` | Command runner |
| `jiradozer/pr.go` | PR creation via wt.CreatePR |
| `jiradozer/poller.go` | Comment polling |
| `jiradozer/poller_test.go` | Poller tests |
| `jiradozer/cmd/jiradozer/main.go` | CLI entrypoint |
| `jiradozer/BUILD.bazel` | Library build target |
| `jiradozer/cmd/jiradozer/BUILD.bazel` | Binary build target |
| `jiradozer/tracker/BUILD.bazel` | Tracker interface build target |
| `jiradozer/tracker/linear/BUILD.bazel` | Linear impl build target |

## Files to Modify

- `go.work` — add `./jiradozer` entry

## Dependencies (existing in monorepo)

- `multiagent/agent` — Provider interface, NewProviderForModel, ModelRegistry
- `wt` — CreatePR, GHRunner, CheckGitHubAuth
- `cobra` — CLI framework
- `gopkg.in/yaml.v3` — config loading
- Standard library: `net/http`, `encoding/json`, `os/exec`, `log/slog`

## Implementation Order

1. `tracker/tracker.go` + `tracker/types.go` — interface and types
2. `state.go` + `state_test.go` — state machine
3. `config.go` — config loading
4. `tracker/linear/` — Linear implementation + tests
5. `poller.go` + `poller_test.go` — comment polling
6. `agent.go` — provider wrapper for plan/build
7. `validation.go` — command runner
8. `pr.go` — PR creation
9. `workflow.go` — compose everything
10. `cmd/jiradozer/main.go` — CLI entrypoint
11. Build files: `go.mod`, `BUILD.bazel`, update `go.work`, run gazelle

## Verification

1. **Unit tests**: `bazel test //jiradozer/...` — state machine transitions, config parsing, comment action parsing, validation formatting
2. **Integration test with httptest**: Linear client against mock GraphQL server
3. **Manual test**: `jiradozer --issue ENG-123 --dry-run` against real Linear
4. **Full run**: `jiradozer --issue ENG-123 --model sonnet`
5. **Build**: `bazel build //jiradozer/...`
6. **Lint**: `scripts/lint.sh`
