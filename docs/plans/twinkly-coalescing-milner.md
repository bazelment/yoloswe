# Jiradozer E2E Integration Tests with Fake Tracker

## Context

Jiradozer has unit tests (mock tracker, no real agents) and integration tests (real Linear API, real Claude for individual steps), but nothing tests `Workflow.Run()` end-to-end with real Claude execution. The gap: we don't know if the full plan->build->validate->ship pipeline actually works when all steps run real agents and the tracker records the right call sequence. This plan adds an E2E test using a stateful fake tracker backend with auto-approve, running real Claude (haiku) for all 4 steps.

## Files to Create/Modify

| File | Action |
|------|--------|
| `jiradozer/integration/fake_tracker.go` | **Create** — stateful in-memory `IssueTracker` implementation |
| `jiradozer/integration/e2e_workflow_test.go` | **Create** — end-to-end workflow test |
| `jiradozer/integration/BUILD.bazel` | **Modify** — add new srcs |

## 1. FakeTracker (`fake_tracker.go`)

`//go:build integration` build tag. Package `integration`.

### Struct

```go
type FakeTracker struct {
    mu       sync.Mutex
    issues   map[string]*fakeIssue  // keyed by issue.ID
    states   []tracker.WorkflowState
    calls    []FakeTrackerCall
    nextID   int
}

type fakeIssue struct {
    issue    tracker.Issue
    stateID  string
    comments []tracker.Comment
}

type FakeTrackerCall struct {
    Method string
    Args   []string
}
```

### Constructor and helpers

- `NewFakeTracker(states []tracker.WorkflowState) *FakeTracker`
- `AddIssue(issue tracker.Issue)` — stores issue by ID
- `Calls() []FakeTrackerCall` — returns all recorded calls
- `CallsFor(method string) []FakeTrackerCall` — filtered by method name
- `IssueStateID(issueID string) string` — returns current state ID
- `IssueComments(issueID string) []tracker.Comment` — returns stored comments
- `InjectHumanComment(issueID, body string)` — adds comment with `IsSelf: false` (for future non-auto-approve tests)

### IssueTracker interface (6 methods)

| Method | Behavior |
|--------|----------|
| `FetchIssue` | Lookup by `Identifier` (iterate values), return copy |
| `ListIssues` | Return all issues (filter ignored for now) |
| `FetchComments` | Filter stored comments by `CreatedAt.After(since)` |
| `FetchWorkflowStates` | Return pre-configured states slice |
| `PostComment` | Append comment with `IsSelf: true`, `CreatedAt: time.Now()`, auto-incrementing ID |
| `UpdateIssueState` | Update `fakeIssue.stateID` |

All methods record calls. All are mutex-protected.

### Key design decisions

- `PostComment` stores comments with `IsSelf: true` — matches real Linear behavior where bot-posted comments are self-attributed
- `FetchComments` respects `since` parameter (existing `mockWorkflowTracker` ignores it — this is an improvement)
- `InjectHumanComment` uses `IsSelf: false` for simulating human review feedback in future tests

## 2. E2E Workflow Test (`e2e_workflow_test.go`)

`//go:build integration` build tag. Package `integration`.

### Test helpers

**`e2eIssue()`** — trivial issue to minimize agent complexity:
- Identifier: `"TEST-1"`, Title: `"Create hello.txt with hello world"`
- Description: `"Create a file named hello.txt that contains the text 'hello world'"`
- TeamID: `"team-fake"`

**`e2eWorkflowStates()`** — three states matching config defaults:
- `{ID: "state-ip", Name: "In Progress"}`, `{ID: "state-ir", Name: "In Review"}`, `{ID: "state-done", Name: "Done"}`

**`e2eConfig(t, workDir)`** — builds config via `jiradozer.DefaultConfigForTest()` with overrides:
- `Agent.Model = "haiku"` (cheapest/fastest)
- `WorkDir = workDir` (t.TempDir)
- `MaxBudgetUSD = 5.0`
- All 4 steps: `AutoApprove = true`, `MaxTurns = 3`, `MaxBudgetUSD = 2.0`
- Custom validate prompt: "Check if hello.txt exists and contains 'hello world'" (avoids running test frameworks)
- Custom ship prompt: "Write SHIP_SUMMARY.md summarizing what was built" (avoids `gh pr create` which needs a real git remote)
- `PollInterval = 50ms` (fast for tests, though auto-approve skips polling)

### Test: `TestE2E_HappyPath_AllAutoApprove`

The primary test — runs the full 4-step workflow with real Claude execution.

**Setup:**
1. Skip if `testing.Short()`
2. `context.WithTimeout(ctx, 10*time.Minute)` — 4 haiku steps at ~1-2 min each
3. Create FakeTracker with states, add issue
4. Create Workflow with FakeTracker, set `OnTransition` callback to collect transitions
5. Call `wf.Run(ctx)`

**Assertions:**
- `wf.Run` returns nil error
- Final tracker state is `"state-done"`
- Transition sequence matches: Planning → PlanReview → Building → BuildReview → Validating → ValidateReview → Shipping → ShipReview → Done
- `FetchWorkflowStates` called exactly once
- First `UpdateIssueState` is in-progress, last is done
- `PostComment` called with bodies containing `"## Plan Complete"`, `"## Build Complete"`, `"## Validate Complete"`, `"## Ship Complete"`
- `FetchComments` never called (auto-approve bypasses polling)

### Test: `TestE2E_PlanStep_Smoke`

Fast smoke test (~2 min) using `jiradozer.RunStepAgent` directly for plan step only. Verifies the model/agent setup works before committing to the full 10-minute E2E. Complements existing `session_resumption_test.go` but uses FakeTracker's issue data pattern.

## 3. BUILD.bazel Update

Add `fake_tracker.go` and `e2e_workflow_test.go` to `srcs`. No new external deps needed (all stdlib + existing testify + `//jiradozer` + `//jiradozer/tracker`).

```python
srcs = [
    "e2e_workflow_test.go",    # NEW
    "fake_tracker.go",         # NEW
    "orchestrator_test.go",
    "session_resumption_test.go",
    "workflow_test.go",
],
```

## 4. Implementation Order

1. Create `fake_tracker.go` — independent, can verify compilation alone
2. Create `e2e_workflow_test.go` — imports FakeTracker
3. Update `BUILD.bazel` — add both files to srcs
4. Run `TestE2E_PlanStep_Smoke` first (fast, ~2 min) to validate setup
5. Run `TestE2E_HappyPath_AllAutoApprove` (full pipeline, ~10 min)

## 5. Verification

```bash
# Quick smoke test
bazel test //jiradozer/integration:integration_test \
    --test_env=HOME="$HOME" --test_env=PATH="$PATH" \
    --test_filter=TestE2E_PlanStep_Smoke \
    --test_output=streamed --test_timeout=300

# Full E2E
bazel test //jiradozer/integration:integration_test \
    --test_env=HOME="$HOME" --test_env=PATH="$PATH" \
    --test_filter=TestE2E_HappyPath \
    --test_output=streamed --test_timeout=900

# Or via test-manual.sh (runs all integration tests)
scripts/test-manual.sh
```

## Key Reuse

- `jiradozer.DefaultConfigForTest()` (`config.go:109`) — base config
- `jiradozer.NewWorkflow()` (`workflow.go`) — workflow constructor
- `jiradozer.RunStepAgent()` (`agent.go`) — for smoke test
- `jiradozer.NewPromptData()` (`agent.go`) — for smoke test
- `tracker.IssueTracker` interface (`tracker/tracker.go`) — FakeTracker implements this
- Existing BUILD.bazel pattern from `jiradozer/integration/BUILD.bazel`
