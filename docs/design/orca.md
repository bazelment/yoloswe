# Nebula Swarm Orchestrator Harness (`orca/`)

Reference design for the native Go swarm orchestrator harness in [`orca/`](../../orca/) (module `github.com/bazelment/yoloswe/orca`), distilled from Nebula design post `5aaec193-a1f1-4009-aa55-f6174d583653`.

---

## Usage

The primary entry point, a bare `orca` with no arguments, opens the owner
chat; flags without a subcommand still mean `orca run`. From a GitHub
checkout, chat reattaches to the live (or most recently active) run for the
inferred repository; `/runs` lists that repository's runs and `/runs <id>`
switches only to one of them. A goal stated in chat starts a detached
controller run in its own session, with the chat's configuration flags.
Instructions routed to the Planner or Dispatcher are confirmed as queued only
while a controller holds the run's lease; otherwise chat says they wait for one
to start.

```text
orca
```

To start a run directly, use the explicit form:

```text
orca run --goal "<text>"
```

The repository is inferred from `origin`; use `--repo owner/name` to override it. Goal runs exit when settled; add `--standing` to keep one alive. `--once` remains a no-op compatibility alias.

---

## 1. Problem & Motivation

Long-lived chat-session orchestrators degrade over multi-day runs because they hold the control loop, state, and history inside a single growing conversation context:

| Orchestrator | Turns | Cache-read Tokens | Cost | Failure Modes Observed |
|---|---|---|---|---|
| `infra cc0fa651` (~8 d) | 11.8k | 3.3B | ~$1.16k | Hit compaction ceiling |
| `s1-main 7f0c8546` (~8 d) | 6.2k | 1.6B | ~$0.50k | Idle lanes, repeated status checks |
| `local supervisor a63a6844` (80 h) | 8.6k | 3.0B | ~$1.60k | 18 compactions, ~350k context/turn |

Key findings from transcript and terminal usage analysis:
1. **Cost = turns × resident context**: 98–99% of token spend is cache reads. ~30% of turns are wake/poll turns and ~30% read mail one item at a time; actual brief writing and replanning account for <2% of turns.
2. **Compaction amnesia**: Long contexts and compactions drop owner asks, leave lanes idle for 4–11 hours, invent stale facts in summaries, and repeat identical checks.
3. **Prose rules drift**: ~24% of owner messages (55/227) re-correct the same invariants ("why ask", "delegate, don't do it", "why wait for CI") because rules live in skill prose and `NOTES.md`.
4. **Expensive status queries**: ~25% of owner traffic is "where are we?" answered by the largest, most expensive context in the system.

---

## 2. Core Architecture

Split orchestration into a **deterministic Go Controller** that owns all state and waiting, and **short, fresh-context LLM turns** invoked on the Messages API only when judgment is needed. Nothing is "remembered" in a conversation; if a fact is not in the `Store`, it does not exist.

```
           events (lane report, phase exit, CI, mail, timer, owner msg)
                                │
                     ┌──────────▼──────────┐
                     │ Controller (Go, no  │  owns: store, lease, timers, intake
                     │ LLM) — deterministic│  does: phases, merge, reap, stall,
                     │ state machine       │        retry, fallback chains
                     └──┬────────┬───────┬─┘
        judgment batch  │        │ owner │ plan trigger
                 ┌──────▼───┐ ┌──▼─────┐ ┌▼─────────┐
                 │Dispatcher│ │Concierge│ │ Planner  │   all: fresh context, small prompt,
                 │ (tick)   │ │ (chat)  │ │ (replan) │   typed tools, bounded turns
                 └──────────┘ └─────────┘ └──────────┘
                        └─► typed tool calls back to Controller (vetted, logged)
```

### Roles

1. **Controller (deterministic, no LLM)**:
   - Holds the run `Lease` (with heartbeat) so the process is stateless and replaceable (`orca --run <id>`).
   - Performs all waiting and event intake (`lane_report`, `phase_exit`, `ci`, `mail`, `timer`, `owner_msg`, `owner_ask_expired`, `stall`).
   - Classifies every event into **auto** (mechanical recipe transition, single CI rerun, agent fallback chain, merge on pinned review approval + green CI) vs **judgment** (stall needing re-delegation, round limit reached, lane question, empty backlog, owner directive).
   - Batches judgment events (debounced 1–5 min, bypassed on `Urgent: true`) into a single Dispatcher turn.
   - Enforces owner policies in code: merge gate, "decide don't ask" (every `owner.ask` requires options + default + deadline, auto-applying the default on expiry), brief size caps, and concurrency caps.

2. **Dispatcher (single writer to backlog and lanes)**:
   - Input per call: a generated **snapshot** (~10–30k tokens) containing the objective summary, active directives, top-N backlog items, lane table (phase, round, age, last event), and the current judgment event batch with bounded excerpts. No conversation history is carried across ticks.
   - Fixed, versioned system prompt (~3–5 KB) and ~10 typed tools (`backlog.add/update/reorder/close`, `lane.spawn`, `lane.steer`, `lane.retry`, `lane.stop`, `owner.ask`, `directive.note`, `planner.request`, `read.lane_report/pr/ci`).
   - Every proposed tool call is checked against ledger/observation invariants (`Vet` pattern) and appended to `DecisionStore`.
   - Bounded to ~10 tool turns per batch; defaults to Sonnet-class model with escalation to Opus for hard batches.

3. **Concierge (owner chat)**:
   - Fresh thread per owner conversation (`orca chat --run <id>`).
   - Read-only tools over the `Store` (`status`, `lane.show`, `decisions.since`, `pr/ci`) plus a single write tool: `directive.submit`, which writes a typed `Directive` (`standing`, `instruction`, `recipe_edit`, `plan_import`) for the Controller/Dispatcher.

4. **Planner (replanning & plan import)**:
   - Triggered by `plan_import` or `recipe_edit` directives, empty/blocked backlog, milestone completion, or repeated failure.
   - Runs in a fresh thread to produce a structured reconciliation: objective update, backlog diff, lanes to stop or re-brief, and any unmappable/conflicting items.

---

## 3. Module & Package Layout (`orca/`)

The module lives at `orca/` (`github.com/bazelment/yoloswe/orca`). Each package owns a narrow boundary and is consumed by other lanes only through its exported API:

```
orca/
├── go.mod
├── clock/          Clock, Timer, Ticker interfaces; RealClock and deterministic FakeClock
├── recipe/         Recipe, Phase, Transition, Condition, Limits, MergePolicy, and Default()
├── lane/           Lane, Status, PhaseReport parser, LaneDriver interface, and FakeDriver
├── model/          ModelClient interface, Message/ContentBlock/Tool types, ToolRegistry, FakeClient
└── store/          Store + 8 sub-store interfaces, domain records, MemoryStore, RunContractTests
```

### 3.1 `orca/clock`
- `Clock` (`Now`, `NewTimer`, `NewTicker`, `AfterFunc`), `Timer`, and `Ticker`.
- `RealClock` wraps standard `time`.
- `FakeClock` provides deterministic manual time advancement (`Advance`, `Set`, `Waiters`), firing timers, `AfterFunc` callbacks, and tickers in chronological order without `time.Sleep`.

### 3.2 `orca/recipe`
- `Recipe`: versioned natural-language `Source` paired with compiled `Phases`, `Transitions`, `Limits`, `MergePolicy`, and `Unmapped` phrases.
- `Phase`: `Name`, `Agents` fallback chain (`[]AgentSpec{Harness, Model, Effort}`), `BriefTemplate`, `ExitContract`, `Gates`, and `Mutating`.
- `Transition` & `Condition`: mechanical predicates (`Verdict`, `CIGreen`, `MinRound`, `MaxRound`) plus optional `JudgmentRule` (natural-language condition evaluated by the Dispatcher).
- `Default()`: shipped default recipe (`swe -> clean -> review -> integrate`) as data, with rework transitions (`review -> swe` on `needs-swe` up to round 2, escalating on round 3) and controller-gated integration.

### 3.3 `orca/lane`
- `Lane`: tracks `ID`, `RunID`, `ItemID`, `Status` (`planned`, `running`, `blocked`, `failed`, `done`), `Phase`, `Round`, `Attempt`, pinned `RecipeName` + `RecipeVersion`, active `SessionID` / `TerminalID`, per-attempt `Sessions` / `Terminals` maps keyed by `PhaseRoundKey` (`swe`, `swe2`, ...), PR metadata (`PR`, `PRHead`, `ApprovalSHA`, `MergeSHA`, `Checks`), and `LastEventID` / `LastEventAt`.
- `PhaseReport` & `ParsePhaseReport`: parses structured Nebula phase reports (`Outcome`, `HEAD`, `Reason`, `For`, `Ask`, `Breaks`, `Riskiest step`, `Observed`, `Verdict`, `Reach`, `Reach observed`, and summary body).
- `LaneDriver` (`SpawnSession`, `StartPhaseTerminal`, `Steer`, `Stop`, `ReadReport`) and `FakeDriver`.

### 3.4 `orca/model`
- `ModelClient`: `CreateMessage(ctx, Request) (*Response, error)` over the Messages API.
- `Message`, `ContentBlock` (`text`, `thinking`, `tool_use`, `tool_result`), `ToolDefinition`, `ToolCall`, `ToolResult`, and `Usage` (`InputTokens`, `OutputTokens`, `CacheCreationInputTokens`, `CacheReadInputTokens`, `CostUSD`).
- `ToolRegistry` & generic `AddTool[T]` for deriving JSON schemas from Go structs and dispatching tool calls in-process.
- `FakeClient` for deterministic unit tests and recorded replay evaluations.

### 3.5 `orca/store`
All state access goes through `store.Store`:
```go
type Store interface {
    Runs()       RunStore       // objective, plan ref, lease (acquire/heartbeat/release)
    Backlog()    BacklogStore   // items: priority, order, acceptance, status, lane, history
    Lanes()      LaneStore      // lane state, phase/round, session/terminal ids, last event
    Directives() DirectiveStore // owner rules: kind, text, ref, source, expiry, scope
    Decisions()  DecisionStore  // append-only: seq, category, actor, tool, args, reason, usage
    Events()     EventStore     // append-only inbox + per-consumer monotonic cursor
    Threads()    ThreadStore    // thread/turn/item records, live/queued steer, thread fork
    Recipes()    RecipeStore    // versioned NL source + compiled Recipe per run
    Close()      error
}
```
- `MemoryStore` (`NewMemoryStore(clk)`) provides an in-memory reference implementation.
- `RunContractTests(t, Factory)` exports the shared contract test suite that `MemoryStore`, the local SQLite store, and the Nebula API store all execute.

---

## 4. Process Model, Recipes, and Plan Import

### Process & Thread Model
Borrowed from Codex `app-server`:
- One daemon process hosts controller loops and role **threads** (`Thread`, `Turn`, `Item`).
- The **Dispatcher thread** rebuilds its context from a fresh store snapshot on each judgment batch turn (equivalent to forking from a clean base every turn) while persisting turn items for audit and replay evals.
- **Steer queue**: `ThreadStore.SteerTurn` injects a steering message directly into a running turn (recording an `ItemKindSteer` item) or queues it on `Thread.QueuedSteers` to be drained when the next turn starts.
- **Crash recovery**: restarting `orca --run <id>` acquires/renews the run lease (once any dead holder's TTL expires), reloads open state and cursors from `Store`, and resumes without duplicate actions.

### Natural-Language Recipes & Overrides
- Owners define or tune workflows in plain language (e.g., *"skip clean for docs"*, *"lanes merge themselves once CI is green"*, *"add security review for auth changes"*).
- A Planner turn compiles NL source into a versioned `recipe.Recipe` and echoes `FormatTable()` back for confirmation, surfacing any `Unmapped` phrases explicitly.
- Newly spawned lanes pin the latest `RecipeVersion`; in-flight lanes keep their existing version unless explicitly migrated.

### Plan Import
- Submitting `directive.submit(kind=plan_import, ref=<post/url/path>)` triggers a Planner turn in import mode.
- Because the owner authored the plan, non-conflicting objective updates and backlog diffs apply immediately without an approval round; only unmappable or conflicting points trigger `owner.ask` with a default and deadline.

---

## 5. Validation Gates

1. **Gate 1 — Deterministic unit & contract tests (`bazel test //orca/...`, no network/LLM calls)**:
   - `Store` contract suite (`RunContractTests`) green on `MemoryStore` and SQLite.
   - Controller tests with `FakeClock` and `FakeDriver`: mechanical transitions, rework round limits, stalls, crash/restart idempotency, and owner-ask deadline auto-application.
   - Recipe compiler golden tests using recorded `FakeClient` responses.
2. **Gate 2 — Offline replay evaluation (`manual` tag, real model)**:
   - ~150 historical judgment moments and ~40 concierge questions replayed against recorded run state.
   - Pass bars: owner-agreement >= baseline, unnecessary owner asks <= 1/3 of baseline, zero invariant violations, >= 90% factual concierge accuracy, median input tokens < 30k/decision.
   - Evidence rules (`orca/replay`): only moments *verified* against the source run's records count toward the agreement and unnecessary-ask bars (rubric in the dataset's `provenance_rubric`); the rest are `synthetic`, still run and reported, never counted. The baseline is the old orchestrator's recorded action, not an assumption. Event text, directives and the Concierge prompt must not contain the answer: the dispatcher sees facts only, and the Concierge must read the Store through its tools. Verdicts are PASS / FAIL / INCONCLUSIVE; INCONCLUSIVE means every bar passed but no real owner correction is evidenced, so the ask bar is not established.
   - `orca/replay/adversarial_test.go` guards the grader: always-`lane.stop`, always-`owner.ask`, never-acts, an event-text scraper and a prompt-parroting Concierge must all fail the bars.
3. **Gate 3 — Live sandbox run**:
   - 5–10 real low-risk backlog items across `default` and custom recipes, surviving a forced restart, < 15 min max unhandled event latency, total model cost < $25/day.
4. **Gate 4 — Shadow mode & cutover**:
   - Shadow a live run for 2–3 days (proposing decisions without acting), then cut over via run lease acquisition with >= 5x daily cost reduction and zero dropped asks.

### Environment variables

The project is named `orca`; environment variables retain their existing `ORCH_` prefix for now.
