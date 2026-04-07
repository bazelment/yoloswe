# Jiradozer Local File Tracker

## Context

Jiradozer currently requires a Linear issue tracker with API key to run. This limits adoption — users can't just point it at a task description and let it run. The goal is to add a `local` tracker backend so users can start a full plan→build→validate→ship workflow from just a `--description` flag on the CLI, with no external tracker needed. State persists to local JSON files so the workflow can be inspected/resumed.

## Files to Create/Modify

| File | Action |
|------|--------|
| `jiradozer/tracker/local/local.go` | **Create** — local file-backed IssueTracker implementation |
| `jiradozer/tracker/local/local_test.go` | **Create** — unit tests |
| `jiradozer/tracker/local/BUILD.bazel` | **Create** — Bazel build rule |
| `jiradozer/config.go` | **Modify** — relax API key validation for local kind |
| `jiradozer/cmd/jiradozer/main.go` | **Modify** — add `--description` flag, wire local tracker |
| `jiradozer/cmd/jiradozer/BUILD.bazel` | **Modify** — add local tracker dep |

## 1. Local Tracker (`jiradozer/tracker/local/local.go`)

### Storage Layout

```
<work-dir>/.jiradozer/
  issues/
    local-1.json    # one file per issue
    local-2.json
  next_id           # counter file: "3\n"
```

Each issue file:
```json
{
  "issue": {
    "id": "local-1",
    "identifier": "LOCAL-1",
    "title": "Add retry logic to HTTP client",
    "description": "When HTTP requests fail with 5xx...",
    "state": "In Progress",
    "team_id": "local",
    "labels": []
  },
  "comments": [
    {
      "id": "c-1",
      "body": "## Plan Complete\n\n...",
      "user_name": "jiradozer",
      "is_self": true,
      "created_at": "2026-04-07T10:00:00Z"
    }
  ]
}
```

### Struct

```go
type Tracker struct {
    dir string // path to .jiradozer/issues/
    mu  sync.Mutex
}
```

### Constructor

`NewTracker(dir string) (*Tracker, error)` — creates dir if needed, reads next_id counter.

### IssueTracker Interface Methods

| Method | Behavior |
|--------|----------|
| `FetchIssue` | Glob `dir/*.json`, unmarshal, match by Identifier |
| `ListIssues` | Glob all, filter by state names if filter.States is non-empty |
| `FetchComments` | Read issue file, filter comments by `CreatedAt.After(since)` |
| `FetchWorkflowStates` | Return fixed set: In Progress (started), In Review (started), Done (completed) |
| `PostComment` | Append comment with `IsSelf: true`, persist to file |
| `UpdateIssueState` | Update state field, persist to file |

### Extra Method (not on interface)

`CreateIssue(title, description string) (*tracker.Issue, error)` — assigns next LOCAL-N identifier, writes initial JSON file with state "Todo", returns the Issue.

### Key Design Decisions

- **Read-through, write-through**: Every method reads from disk and writes back. No in-memory cache. This keeps the code simple and lets external tools inspect/modify state.
- **Fixed workflow states**: Always returns the same 3 states (In Progress, In Review, Done). No team concept.
- **Thread-safe**: Mutex protects all file operations.
- **Counter file**: `next_id` is a plain text file with an integer. Atomic via mutex.

## 2. Config Changes (`jiradozer/config.go`)

### Relax API key validation

Current `validate()` unconditionally requires `Tracker.APIKey`. Change to:

```go
if c.Tracker.Kind != "local" && c.Tracker.APIKey == "" {
    return fmt.Errorf("tracker.api_key is required")
}
```

### Skip env resolution for local

In `LoadConfig()`, wrap the `resolveEnv(cfg.Tracker.APIKey)` call to skip when kind is local:

```go
if cfg.Tracker.Kind != "local" {
    apiKey, err := resolveEnv(cfg.Tracker.APIKey)
    // ...
}
```

## 3. CLI Changes (`jiradozer/cmd/jiradozer/main.go`)

### New flags

```
--description "task description text"
--description-file path/to/file  (or - for stdin)
```

### Logic in `run()`

1. Resolve description: if `--description-file` is set, read from file/stdin into `args.description`.
2. Validate mutual exclusivity: `--description`, `--issue`, `--team` are mutually exclusive.
3. When `--description` is set:
   - Force `cfg.Tracker.Kind = "local"` (ignore config file tracker settings)
   - Default all steps to auto-approve (user can override with `--auto-approve=none` or individual flags)
   - Generate title from description using haiku (1-turn agent call, no tools, ~$0.001)
   - Create local tracker, call `CreateIssue(title, description)`
   - Run the workflow with the created issue

### Title Generation

Use the existing `runAgent()` infrastructure with a minimal config:

```go
func generateTitle(ctx context.Context, description string, logger *slog.Logger) (string, error) {
    cfg := StepConfig{
        Model:          "haiku",
        PermissionMode: "plan",  // no tools
        MaxTurns:       1,
        MaxBudgetUSD:   0.01,
    }
    prompt := fmt.Sprintf("Generate a concise title (under 80 chars) for this task. Output ONLY the title, nothing else.\n\nTask: %s", description)
    output, _, err := runAgent(ctx, "title", prompt, cfg, os.TempDir(), "", logger)
    return strings.TrimSpace(output), err
}
```

This lives in `jiradozer/agent.go` or as a helper in `main.go`. The agent call is cheap (~pennies) and uses existing infrastructure.

### Factory update in `createTracker()`

```go
case "local":
    dir := filepath.Join(cfg.WorkDir, ".jiradozer", "issues")
    return local.NewTracker(dir)
```

## 4. Unit Tests (`jiradozer/tracker/local/local_test.go`)

Standard Go tests (no build tag — these are fast, no external deps):

1. **TestCreateAndFetchIssue** — create issue, fetch by identifier, verify fields
2. **TestListIssues** — create 2 issues, list all, verify count
3. **TestListIssuesFilterByState** — create issues in different states, filter
4. **TestComments** — post comment, fetch with since filtering, verify IsSelf
5. **TestUpdateState** — update state, fetch, verify new state
6. **TestWorkflowStates** — verify fixed states returned
7. **TestPersistence** — create tracker, write issue, create new Tracker pointing at same dir, verify data survives
8. **TestAutoIncrementID** — create 3 issues, verify LOCAL-1, LOCAL-2, LOCAL-3

## 5. Verification

```bash
# Unit tests
bazel test //jiradozer/tracker/local:local_test --test_timeout=60

# Lint
scripts/lint.sh

# Full build
bazel build //...

# Manual E2E (requires Claude API access):
bazel run //jiradozer/cmd/jiradozer -- \
  --description "Create a hello.txt file containing 'hello world'" \
  --work-dir /tmp/jiradozer-test \
  --auto-approve all \
  --verbose

# Verify local state was persisted:
cat /tmp/jiradozer-test/.jiradozer/issues/local-1.json | jq .
```

## Scope Cuts (intentional)

- **No interactive feedback in local mode**: Auto-approve all by default. File-watching for human feedback is a future extension.
- **No multi-issue mode for local**: `--description` is single-issue only.
- **No config-file-driven local mode**: `--description` forces local mode. Users don't set `tracker.kind: local` in YAML (though it would work once the tracker exists).
