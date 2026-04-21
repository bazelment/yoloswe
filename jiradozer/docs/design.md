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
  agent.go               # Unified agent runner for all steps (plan/build/validate/ship)
  poller.go              # Comment polling for feedback
  poller_test.go
  go.mod
  BUILD.bazel
```

New top-level module — same pattern as `yoloswe/`. Depends on `multiagent/agent` (provider interface).

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

All four workflow steps (plan, build, validate, ship) are uniform agent sessions driven by a single `RunStepAgent` function. Uses `multiagent/agent.Provider` interface — all bramble-supported agents work automatically.

**Prompt system**: Each step has a `prompt` field (Go `text/template`) rendered with issue data on first execution. If omitted, a built-in default is used. On follow-up (redo/feedback), the existing agent session is resumed via `WithProviderResumeSessionID` and the feedback text is sent directly — no prompt re-rendering.

```go
// PromptData is the template context for rendering step prompts.
type PromptData struct {
    Identifier  string // e.g. "ENG-123"
    Title       string
    Description string // empty string if nil
    URL         string // empty string if nil
    Labels      string // comma-separated
    BaseBranch  string // e.g. "main"
    Plan        string // plan output from the planning step
    BuildOutput string // build output from the build step
}

// RunStepAgent is the single entry point for all steps.
// First call: renders prompt template. Follow-up: resumes session with feedback.
// Returns StepAgentResult (Output, SessionID).
func RunStepAgent(ctx context.Context, stepName string, data PromptData,
    cfg StepConfig, workDir string, feedback string, resumeSessionID string,
    renderer *render.Renderer, logger *slog.Logger) (StepAgentResult, error)
```

Each step has a built-in default prompt template:
- **plan**: asks the agent to create a detailed implementation plan
- **build**: gives the agent the approved plan to implement
- **validate**: asks the agent to run tests/linters and fix failures
- **ship**: asks the agent to create a PR against the base branch

Prompt resolution logic:
1. **Resume** (`resumeSessionID != ""` and `feedback != ""`): send feedback as prompt, resume session
2. **First execution**: render config prompt (or built-in default) with issue data
3. **Fallback** (feedback but no session): render template + append feedback

The `ExecuteConfig.PermissionMode` maps correctly for each backend:
- Claude: `"plan"` → `PermissionModePlan`, `"bypass"` → `PermissionModeBypass`
- Codex: `"plan"` → `ApprovalPolicyOnRequest`, `"bypass"` → `ApprovalPolicyNever`
- Gemini/Cursor: equivalent mappings

Key files:
- `multiagent/agent/provider.go` — `Provider` interface, `ExecuteConfig` (with `ResumeSessionID`), `AgentResult` (with `SessionID`)
- `multiagent/agent/query.go` — `NewProviderForModel()` factory
- `multiagent/agent/model_registry.go` — `AllModels`, `ModelByID()`
- `multiagent/agent/claude_provider.go` — wires `WithResume`, extracts `SessionID`

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
    StepValidating     // Agent running tests/linters
    StepValidateReview
    StepShipping       // Agent creating PR
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

### 5. Comment Poller (`poller.go`)

Polls tracker every N seconds for new comments. Filters out bot comments. Parses keywords:
- `approve` / `lgtm` / `ship it` → `FeedbackApprove`
- `redo` / `retry` → `FeedbackRedo`
- Anything else → `FeedbackComment` (incorporated into next agent prompt)

### 6. Workflow Engine (`workflow.go`)

All four steps are driven by the same `runStep` method. The workflow tracks session IDs per step so redo/feedback resumes the agent session instead of starting fresh.

```go
type Workflow struct {
    tracker    tracker.IssueTracker
    issue      *tracker.Issue
    state      *StateMachine
    config     *Config
    logger     *slog.Logger
    sessionIDs map[WorkflowStep]string // per-step session IDs for resume
    plan       string
    buildOutput string
}

// runStep is the uniform handler for all workflow steps.
func (w *Workflow) runStep(ctx context.Context, stepName string, stepCfg StepConfig,
    reviewStep WorkflowStep, trigger string) {
    cfg := w.config.ResolveStep(stepCfg)
    data := newPromptData(w.issue, w.config.BaseBranch)
    data.Plan = w.plan
    data.BuildOutput = w.buildOutput

    res, err := RunStepAgent(ctx, stepName, data, cfg,
        w.config.WorkDir, w.feedback, w.sessionIDs[w.state.Current()], nil, w.logger)
    w.sessionIDs[w.state.Current()] = res.SessionID
}
```

On approve → next phase starts fresh (session ID is ""). On redo/feedback → same phase resumes the stored session.

## Configuration (`jiradozer.yaml`)

Each step (`plan`, `build`) is a self-contained agent session config. All step fields are optional — unset `model` and `max_budget_usd` inherit from top-level config via `Config.ResolveStep()`.

```yaml
tracker:
  kind: linear           # pluggable: "linear", future: "github", "jira"
  api_key: $LINEAR_API_KEY

agent:
  model: sonnet           # any model from agent.AllModels; fallback for steps
  # provider auto-detected from model ID

work_dir: .
base_branch: main
poll_interval: 15s
max_budget_usd: 50.0          # fallback for steps

plan:
  # prompt: Go text/template; omit to use built-in default
  # system_prompt: optional system prompt
  # model: override agent.model for this step
  permission_mode: plan        # default
  max_turns: 10
  # max_budget_usd: override top-level for this step

build:
  permission_mode: bypass      # default
  max_turns: 30

validate:
  permission_mode: bypass      # default
  max_turns: 10

ship:
  permission_mode: bypass      # default
  max_turns: 10

states:
  in_progress: "In Progress"
  in_review: "In Review"
  done: "Done"
```

### Prompt templates

The `prompt` field uses Go `text/template` syntax. Available variables: `{{.Identifier}}`, `{{.Title}}`, `{{.Description}}`, `{{.URL}}`, `{{.Labels}}`, `{{.BaseBranch}}`, `{{.Plan}}`, `{{.BuildOutput}}`. Templates are validated at config load time.

The prompt is only rendered for the first execution in a phase. On redo/feedback, the agent session is resumed and the feedback text is sent directly.

## CLI Flags

```
jiradozer --issue ENG-123 [--config jiradozer.yaml] [--work-dir .]
              [--model sonnet] [--poll-interval 15s] [--max-budget 50]
              [--run-step plan] [--verbose]
```

`--model` accepts any model ID from `agent.AllModels` (opus, sonnet, haiku, gpt-5.3-codex, gemini-3.1-pro-preview, cursor-default, etc.). Provider is auto-detected.

`--run-step` runs a single step and exits without tracker interaction — useful for debugging prompts and agent behavior.

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
| `jiradozer/agent.go` | Unified agent runner for all steps |
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
- `cobra` — CLI framework
- `gopkg.in/yaml.v3` — config loading
- Standard library: `net/http`, `encoding/json`, `text/template`, `log/slog`

## Implementation Order

1. `tracker/tracker.go` + `tracker/types.go` — interface and types
2. `state.go` + `state_test.go` — state machine
3. `config.go` — config loading
4. `tracker/linear/` — Linear implementation + tests
5. `poller.go` + `poller_test.go` — comment polling
6. `agent.go` — unified agent runner for all steps
7. `workflow.go` — compose everything
8. `cmd/jiradozer/main.go` — CLI entrypoint
9. Build files: `go.mod`, `BUILD.bazel`, update `go.work`, run gazelle

## Verification

1. **Unit tests**: `bazel test //jiradozer/...` — state machine transitions, config parsing, comment action parsing
2. **Integration test with httptest**: Linear client against mock GraphQL server
3. **Manual test**: `jiradozer --filter team=ENG --dry-run` against real Linear (team-mode only — `--dry-run` is not accepted with `--issue` or `--description`)
4. **Full run**: `jiradozer --issue ENG-123 --model sonnet`
5. **Build**: `bazel build //jiradozer/...`
6. **Lint**: `scripts/lint.sh`
