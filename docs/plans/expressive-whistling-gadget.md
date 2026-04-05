# Plan: Comprehensive Test Coverage for Jiradozer Workflow

## Context

Jiradozer is a well-architected issue-driven development workflow CLI that automates plan → build → validate → ship with human-in-the-loop approval. The module was recently merged (bazelment/yoloswe#101) but lacks comprehensive test coverage for:

- Config loading and validation
- Complete workflow orchestration with all feedback loops
- LGTM and feedback handling edge cases
- Real-world scenarios against the actual Linear backend

This plan adds both **unit tests** and **integration tests** to ensure jiradozer works correctly end-to-end.

---

## Implementation Plan: Two-Tier Test Coverage

### Tier 1: Unit Tests (Mock-Based)

#### `jiradozer/config_test.go` — Config Loading & Validation

**Test Cases**:
1. Load valid YAML with all fields
2. Load YAML with minimal required fields
3. Environment variable expansion (`$LINEAR_API_KEY`)
4. Path expansion (`~/projects`)
5. CLI flag overrides (model, poll-interval, max-budget, work-dir)
6. Validation: invalid work_dir (non-existent, not a directory)
7. Validation: invalid model ID
8. Validation: invalid permission_mode
9. Per-step field resolution (inherit from top-level)
10. State name mapping to tracker states (in_progress, in_review, done)
11. Template validation (invalid Go templates caught at load)
12. Config with empty optional fields (defaults applied)

**Fixtures** (`jiradozer/testdata/`):
- `valid_complete.yaml` — all fields
- `valid_minimal.yaml` — required fields only
- `with_overrides.yaml` — per-step customization
- `invalid_model.yaml` — unknown model
- `invalid_workdir.yaml` — non-existent path
- `invalid_template.yaml` — bad Go template syntax

**Coverage**: 100% of config.go code paths

#### `jiradozer/poller_test.go` — Feedback Parsing (EXPAND)

**Current coverage** is basic. Expand with:

1. **LGTM Variants**: "lgtm", "LGTM", "lgtm!", "looks good to me", etc.
2. **Approve Variants**: "approve", "APPROVE", "approved", "ship it", "ship!", etc.
3. **Redo Variants**: "redo", "REDO", "retry", "retry with feedback"
4. **Multiline Feedback**:
   - "redo\n\nPlease fix X" → FeedbackRedo with embedded feedback
   - "lgtm\n\nAdditional context" → FeedbackApprove (first line wins)
   - Comment without keyword → FeedbackComment
5. **Edge Cases**:
   - Empty comment → FeedbackComment("")
   - Whitespace-only lines
   - Comments with code blocks (backticks)
   - Comments from bot account (`IsSelf=true`) — should be filtered
6. **Case Insensitivity**: All keywords case-insensitive

**Coverage**: 100% of poller.go

#### `jiradozer/state_test.go` — State Machine (EXPAND)

**Current coverage** is good. Add:

1. **All Valid Transitions**:
   - Init → Planning → PlanReview → Building → BuildReview → Validating → ValidateReview → Shipping → ShipReview → Done
   - PlanReview → (redo) → Planning
   - BuildReview → (redo) → Building
   - ValidateReview → (redo) → Validating
   - BuildReview → (feedback) → Planning (backtrack to start)
   - ValidateReview → (feedback) → Planning (backtrack to start)

2. **Invalid Transitions**: Verify rejection of:
   - Planning → Done (skipping steps)
   - PlanReview → Validating (skipping build)
   - Building → PlanReview (backward)

3. **Failed State**: Can fail from any state, transitions to Failed
4. **History Tracking**: Each transition recorded with timestamp
5. **Step Categorization**: Review steps, execution steps, terminal steps
6. **String Representation**: All steps have names

**Coverage**: 100% of state.go

#### `jiradozer/agent_test.go` — Prompt Rendering & Agent Execution (NEW)

**Test Cases**:
1. Render plan prompt with issue data (template variables resolved)
2. Render build/validate/ship prompts with plan output passed downstream
3. Template variable substitution:
   - `{{.Identifier}}` → issue ID
   - `{{.Title}}`, `{{.Description}}` → issue text
   - `{{.Plan}}` → plan output from step 1
   - `{{.BuildOutput}}` → build output from step 2
   - etc.
4. Custom prompt overrides built-in defaults
5. Agent execution captures output and session ID
6. Agent error handling: execution error propagates
7. Resume with feedback: sends `ResumeSessionID` if feedback loop
8. Budget enforcement: `MaxBudgetUSD` passed to agent config

**Mocks**:
- `MockAgentProvider` — records calls, returns fake outputs
- Verify `ExecuteConfig.ResumeSessionID` is set when resuming

**Coverage**: 100% of agent.go

#### `jiradozer/workflow_test.go` — Workflow Orchestration with Mock Tracker (NEW)

**Test Cases**:

1. **Happy Path**: Plan → (approve) → Build → (approve) → Validate → (approve) → Ship → (approve) → Done
   - Verify correct step order
   - Verify tracker state updates at correct times
   - Verify comments posted with expected content

2. **Feedback Loops with LGTM**:
   - PlanReview receives "lgtm" → transitions to Building
   - BuildReview receives "LGTM" (case insensitive) → transitions to Validating
   - Validate step produces failures, ValidateReview receives "lgtm" → transitions to Shipping

3. **Redo Loops**:
   - BuildReview receives "redo" → back to Building (re-execute same step)
   - ValidateReview receives "retry" → back to Validating
   - Redo with feedback: "redo\n\nFix the test failures" → agent receives feedback text

4. **Backtrack on Feedback**:
   - BuildReview receives "redo: need to rethink approach" → back to Planning
   - ValidateReview receives "redo: restart from plan" → back to Planning
   - Verify full context passed to plan step on restart

5. **Comment Filtering**:
   - Bot comments (jiradozer's own) are ignored
   - Only first human comment after workflow posts is processed
   - Pagination: handles multiple comments, filters correctly

6. **Error Handling**:
   - Agent execution fails → workflow transitions to Failed
   - Tracker operation fails → error propagated, workflow stops
   - Polling timeout → defaults to FeedbackComment

7. **Session Resumption**:
   - Plan step captures SessionID
   - Redo feedback loop sends same SessionID to agent
   - Verify workflow doesn't create new sessions on feedback

8. **Tracker State Updates**:
   - Enter PlanReview → "In Progress"
   - Enter BuildReview/ValidateReview → "In Review"
   - Transition to Done → "Done"
   - State changes are in order

9. **Multi-Step Workflows**:
   - Plan with feedback loop → Build with feedback loop → done
   - Validate step fails, redo, pass, then ship

**Mocks**:
- `MockIssueTracker` — records all calls, injects responses
- `MockAgentProvider` — returns step outputs, tracks session IDs
- `MockPoller` — injects scripted feedback (approve, redo, etc.) without waiting

**Coverage**: 90%+ of workflow.go (all paths except edge cases)

---

### Tier 2: Integration Tests (Real Linear Backend)

#### `jiradozer/integration/session_resumption_test.go` — Session Resumption with Real Agent (FOCUSED)

**Purpose**: Isolated integration test for agent session resumption functionality — the core mechanism that enables feedback loops to preserve context.

**Setup**:
1. Real Claude agent (via multiagent/agent.Provider)
2. Mock Linear tracker (no external API calls)
3. Real bramble session recording (to verify session data is preserved)

**Test Scenario: Session Resumption Across Feedback Loop**:
1. **First Execution (Planning Step)**:
   - Call `RunStepAgent()` with issue "ENG-123" and empty `ResumeSessionID`
   - Agent creates a plan
   - Capture returned `AgentResult.SessionID` (e.g., "sess_abc123")

2. **Feedback Comment**:
   - Simulate human feedback: "redo\n\nPlease use the new architecture pattern"
   - Store feedback text

3. **Resume Execution (Feedback Loop)**:
   - Call `RunStepAgent()` again with:
     - Same issue
     - `ResumeSessionID: "sess_abc123"` (from step 1)
     - Feedback text appended to prompt
   - Agent resumes from session, receives context from step 1
   - Verify new plan incorporates feedback AND remembers prior reasoning

4. **Assertions**:
   - `AgentResult.SessionID` returned from both calls
   - Second call's `SessionID` same or evolved (session continues)
   - Second plan output mentions feedback explicitly
   - No repetition of step 1 reasoning (agent skips re-planning what was already done)

**Verification**:
- Inspect bramble session logs (`bazel-bin/` output) — verify session ID persists
- Verify Claude API logs show `ResumeSessionID` in ExecuteConfig on second call
- Manual inspection: second plan should be notably different from first (not regurgitated)

**Why Isolated**: Session resumption is the critical dependency for feedback loops. If this breaks, all feedback-loop tests fail. By isolating it, we can quickly diagnose session management bugs without debugging entire workflow.

---

#### `jiradozer/integration/workflow_test.go` — Real Linear + Real Codebase

**Purpose**: Verify jiradozer works end-to-end against actual Linear workspace and can execute real code changes.

**Setup**:
1. Use environment variable to point to **test Linear workspace** (separate from production)
2. Use environment variable to point to **test git repository** (local clone of a real repo)
3. Use environment variable to enable test mode (skip waiting for human feedback, auto-approve)

**Test Scenario 1: Simple Plan-Build-Validate-Ship**
1. Create a test issue in Linear (or use a dedicated test issue)
2. Run jiradozer against that issue
3. Auto-approve each step (via env var or mock)
4. Verify:
   - Plan step completes, posts plan as comment
   - Build step completes, makes actual code changes to the test repo
   - Validate step runs tests, posts results
   - Ship step creates an actual PR (or posts ship comment)
   - Issue state transitions correctly in Linear

**Test Scenario 2: Feedback Loop - Redo Build**
1. Create issue, run plan step → approve
2. Build step: simulate failure (test repo has a failing test)
3. Jiradozer posts build output comment with error
4. Manually post "redo" comment (or simulate with auto-script)
5. Jiradozer re-runs build with feedback context
6. Verify build passes on retry

**Test Scenario 3: Feedback Loop - Redo with Guidance**
1. Run plan step → plan output is suboptimal
2. Manually post feedback: "redo\n\nPlease use the new API instead"
3. Jiradozer re-runs plan with feedback embedded
4. Verify new plan incorporates guidance

**Test Scenario 4: Backtrack - Restart from Plan**
1. Plan → approve → Build → approve → Validate fails
2. Post comment: "redo: restart from plan with different approach"
3. Jiradozer resets to Planning step with feedback
4. Plan step re-executes with feedback
5. Then builds, validates, ships with new approach

**Test Scenario 5: Comments & LGTM Handling**
1. Run workflow with real human-like comments:
   - "lgtm" → transitions
   - "LGTM! Great work." → transitions
   - "lgtm\n\nBut please also fix X" → transitions with embedded feedback
2. Verify all comment variants handled correctly
3. Verify bot's own comments not processed twice

**Test Scenario 6: Concurrent Comments**
1. During workflow, multiple comments posted quickly
2. Verify jiradozer processes only the relevant ones
3. Filters bot comments, processes first human comment after step completes

**Fixtures**:
- **Test Linear Workspace**: Dedicated workspace for integration tests
- **Test Git Repo**: Cloned real repo with passing tests, known structure
- **Test Issue**: Predefined issue (or create fresh per test)

**Environment Variables**:
- `JIRADOZER_TEST_LINEAR_API_KEY` — Linear API key for test workspace
- `JIRADOZER_TEST_REPO_DIR` — path to test git repo
- `JIRADOZER_TEST_AUTO_APPROVE` — "1" to auto-approve without waiting
- `JIRADOZER_TEST_ISSUE_ID` — test issue ID (e.g., "TEST-123")
- `UPDATE_INTEGRATION_FIXTURES=1` — to regenerate test fixtures

**Execution**:
```bash
# Run integration tests (manually)
bazel test //jiradozer/integration:integration_test --test_env=JIRADOZER_TEST_LINEAR_API_KEY=... --test_env=JIRADOZER_TEST_REPO_DIR=/path/to/repo

# Or via script
scripts/test-manual.sh --test_env=JIRADOZER_TEST_LINEAR_API_KEY=... //jiradozer/integration:integration_test
```

**Build Configuration**:
- `# gazelle:ignore` in `jiradozer/integration/BUILD.bazel` (manual, not auto-generated)
- `tags = ["manual", "local"]` (skip in `bazel test //...`, require explicit run)
- `gotags = ["integration"]` (compile with build tag)

**Coverage**:
- End-to-end workflow correctness
- Real Linear API integration
- Real git operations
- Comment parsing in production

---

## Files to Create/Modify

| File | Status | Purpose |
|------|--------|---------|
| `jiradozer/config_test.go` | **Create** | Config loading, validation, field resolution |
| `jiradozer/poller_test.go` | **Expand** | LGTM/feedback variants, edge cases, bot filtering |
| `jiradozer/state_test.go` | **Expand** | All valid/invalid transitions, history tracking |
| `jiradozer/agent_test.go` | **Create** | Prompt rendering, agent execution, session resumption |
| `jiradozer/workflow_test.go` | **Create** | Mock-based workflow orchestration, all feedback loops |
| `jiradozer/integration/session_resumption_test.go` | **Create** | Focused integration test: agent session resumption across feedback loops (real agent + mock tracker) |
| `jiradozer/integration/workflow_test.go` | **Create** | Real Linear + real code, all end-to-end scenarios |
| `jiradozer/integration/BUILD.bazel` | **Create** | Manual test config with integration tag |
| `jiradozer/testdata/` | **Create** | YAML fixtures for config tests |
| `jiradozer/tracker/tracker.go` | **Minor** | Ensure interface is mock-friendly (likely no changes needed) |

---

## Validation & Testing

**Unit Tests** (fast, no external dependencies):
```bash
bazel test //jiradozer:jiradozer_test
```

**Session Resumption Integration Test** (uses Claude CLI with existing OAuth token):
```bash
bazel test //jiradozer/integration:session_resumption_test
```

**Full Integration Tests** (requires Linear API key and test repo):
```bash
bazel test //jiradozer/integration:workflow_test --test_env=JIRADOZER_TEST_LINEAR_API_KEY=... --test_env=JIRADOZER_TEST_REPO_DIR=/path/to/repo
```

**Coverage Goals**:
- Config loading: 100%
- State machine: 100%
- Poller/feedback: 100%
- Agent execution: 90%+
- Workflow orchestration: 95%+ (all main paths, happy path, feedback loops, error cases)
- Real Linear integration: All critical scenarios

**End-to-End Verification**:
1. All tests pass: `bazel test //jiradozer/...`
2. Lint passes: `scripts/lint.sh`
3. Integration tests pass against real Linear (manual)
4. Run real jiradozer against test issue (manual)

---

## Why This Approach

1. **Comprehensive Coverage** — unit tests cover all code paths, integration tests validate real-world scenarios
2. **LGTM & Feedback Focus** — detailed test cases for comment parsing, feedback loops, backtracking
3. **Real Backend Validation** — integration tests ensure actual Linear API interaction works
4. **Reproducible & Safe** — unit tests run in CI/CD, integration tests are manual/optional
5. **Future-Proof** — tests document expected behavior, enable safe refactoring
6. **Scales to Features** — test infrastructure reusable for Option A (dry-run), Option B (GitHub), etc.
