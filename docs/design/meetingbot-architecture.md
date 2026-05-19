# Meeting Bot Architecture

## Goal

The meeting bot follows a timestamped meeting transcript while the meeting is
in progress, builds background context through research agents, answers
discussion-relevant questions quickly, and generates a post-meeting summary
cross-referenced with research.

The implementation lives in:

- `bramble/meetingbot/` — core transcript, research, answer, summary, and
  evaluation logic
- `bramble/cmd/meetingbot/` — CLI harness exposed as `bramble meetingbot`
- `bramble/main.go` — command registration

Boundary rules:

| Layer | Owns | May depend on | Must not own |
|-------|------|---------------|--------------|
| Transcript parsing | `MeetingEvent` normalization from files or streams | standard library only | agent calls, research policy, CLI output |
| Bot orchestration | ordered events, evidence cache, topic selection, answer/summary flow | `AgentClient`, transcript types | provider-specific SDKs or command flags |
| Provider adapter | mapping `AgentRequest` to `multiagent/agent` execution | `multiagent/agent` | transcript parsing or answer policy |
| CLI harness | file discovery, flag parsing, evaluation printing | `bramble/meetingbot` | provider internals or mutable bot state |

The stable package contract is `MeetingEvent`, `Evidence`, `Answer`,
`Summary`, `Config`, and `AgentClient`. New transcript transports, research
scopes, and provider families should extend those contracts instead of
importing command code or reaching into `Bot` internals.

The single `bramble/meetingbot` package is intentional for v1. The supported
cross-package API is limited to the exported contract types above plus `New`,
`LoadTranscriptFile`, `ParseTranscript`, `EvaluateFile`, and `DefaultConfig`.
The file-group ownership below is convention-based until a subpackage split; it
is not enforced by the Go import graph. Reviewers should block changes that
violate these invariants:

- transcript parsing must not call providers or inspect `Config`
- provider adaptation must not mutate `Bot` state or parse transcript files
- CLI code may configure the bot, but core bot policy must not import command
  packages or cobra types

If those boundaries start failing in review, the next step is to split provider
adaptation and transcript ingestion into subpackages behind the same exported
facades.

Subpackage split trigger:

- split `transcript` when parsing needs live transport state, external
  dependencies, or anything beyond `MeetingEvent` normalization
- split `provider` when more than one adapter exists, when provider capability
  validation grows beyond `AgentRequest`, or when provider code needs tests that
  should not import bot orchestration
- split `runtime`/`bot` when answer, summary, and research orchestration need
  independent build targets or import-cycle pressure appears

Until then, durability is enforced by code review, package-level tests, and the
invariant list above, not by the Go compiler.

Production milestone: before any live production phase, add either real
subpackages or an architecture test that enforces the same import rules. The
minimum hard boundary is:

- transcript code cannot import provider, config, or prompt-building symbols
- provider code cannot import bot orchestration or transcript file loading
- CLI/evaluation code cannot be imported by core runtime code
- queued research code crosses the bot boundary only through a scheduler
  interface, not by sharing private bot helpers
- `Observe` must not synchronously call provider-facing research code in any
  live profile; synchronous research is restricted to replay/evaluation
  construction

Internal ownership inside `bramble/meetingbot/` is deliberately narrower than
the package boundary:

| File group | Owns | Crosses boundary through |
|------------|------|--------------------------|
| `transcript.go` | parsing and formatting transcript turns | `MeetingEvent` |
| `bot.go` | bot state, topic selection, research, answer, summary orchestration | `Bot`, `Config`, `Evidence`, `Answer`, `Summary` |
| `agent_client.go` | provider-neutral request execution | `AgentClient`, `AgentRequest`, `AgentResponse` |
| `fallback.go` | deterministic local answer and summary fallbacks | `MeetingEvent`, `Evidence` |
| `eval.go` | replay evaluation and metrics aggregation | `InteractionResult`, `FileEvaluation` |

Opening construction is core bot policy; refinement scheduling is the provider
adapter boundary. Metrics may attach to `Answer` and `InteractionResult`, but
CLI printing and UI streaming policy must stay outside `bramble/meetingbot`.

Concurrency contract:

- `Bot` methods are safe for concurrent callers through its internal mutex.
- `Observe` is the only writer for transcript events. It appends the event under
  lock, releases the lock, and — when `AutoResearch` is enabled — enqueues
  research work to the configured `ResearchScheduler`. It never runs
  `BuildBackground` itself; that remains a separate explicit batch call.
- `BuildBackground` snapshots events, performs provider work without holding the
  bot lock, and publishes evidence under lock one row at a time.
- `AnswerQuestion` and `SummarizeMeeting` read snapshots. They see evidence that
  was published before their snapshot; they do not wait for in-flight research.
- Agent clients must not call back into the same `Bot` while a request is in
  flight. Re-entrant bot calls belong in an external orchestrator.

## Command

```bash
bazel run //bramble:bramble -- meetingbot \
  --agent=real \
  --notes-glob '<private-transcripts-glob>' \
  --evaluate \
  --work-dir /path/to/repo
```

The same command supports `--agent=local` for deterministic offline evaluation.

Key CLI knobs:

| Flag | Default | Purpose |
|------|---------|---------|
| `--fast-model` | `gpt-5.3-codex` | Live question answering |
| `--research-model` | `sonnet` | Internal-context research |
| `--code-model` | `gpt-5.3-codex` | Codebase research |
| `--web-model` | `gpt-5.3-codex` | Public-web research |
| `--summary-model` | `gpt-5.5` | Final summary synthesis |
| `--latency-budget` | `10s` | Target opening-readiness latency |
| `--answer-timeout` | `45s` | Timeout for full fast-answer model synthesis |
| `--research-timeout` | `90s` | Timeout for each background research model call |
| `--summary-timeout` | `2m` | Timeout for final summary model synthesis |

## Data Flow

```text
Timestamped transcript
        |
        v
ParseTranscript / Observe
        |
        v
Topic extraction + relevant snippet selection
        |
        +--> Internal research agent
        +--> Codebase research agent
        +--> Public-web research agent
        |
        v
Evidence cache
        |
        +--> Fast live answer path
        |
        +--> Post-meeting summary path
```

## Core Components

### Transcript Ingestion

`ParseTranscript` reads lines shaped like:

```text
[00:02-00:05] Speaker: text
```

Non-matching continuation lines are appended to the previous event. Each parsed
event carries start/end time, speaker, text, raw line, and event index.

`Bot.Observe` appends events as the meeting progresses. When `AutoResearch` is
enabled, it triggers bounded background research every
`ResearchChunkEvents` transcript turns.

`ParseTranscript` and `LoadTranscriptFile` are ingestion adapters, not the live
transport abstraction. A future microphone, websocket, or note-tailer should
normalize input into `MeetingEvent` values and call `Bot.Observe`; it should not
duplicate topic selection, evidence caching, or answer policy. This keeps live
transport failures isolated from the research and synthesis layers.

V1 `AutoResearch` is enqueue-only: when enabled, the triggering `Observe` call
appends the event and then enqueues bounded research jobs to the configured
`ResearchScheduler` before returning. It never runs `BuildBackground` inline, so
`Observe` does not block on provider latency. `AutoResearch=true` therefore
*requires* a `ResearchScheduler`; without one `Observe` fails closed with
`errAutoResearchScheduler` rather than degrading to synchronous provider calls
on the transcript hot path. The scheduler (an external queue or worker) owns
running the work via `RunResearch` and publishing the resulting evidence.
Cancellation is inherited from the `Observe` context; if enqueue is cancelled or
fails, the event remains observed. Provider failures during scheduled research
become miss evidence and do not escape as ingestion errors; only caller
cancellation or a scheduler `Enqueue` error should cause `Observe` to return an
error.

Default profiles:

| Profile | Intended use | `AutoResearch` | Scopes |
|---------|--------------|----------------|--------|
| replay/evaluation | deterministic CLI playback and fixture generation | enabled or explicit batch `BuildBackground` | configured test scopes |
| live-safe | microphone, websocket, or note-tailer transport | disabled; queue research externally | `internal`, `codebase` by default |
| live-web | production web research after admission checks | disabled; queue research externally | explicit opt-in `web` |

The `AutoResearch` column above follows the enqueue-only contract described in
the "V1 `AutoResearch` is enqueue-only" paragraph: replay/evaluation may enable
it (with a scheduler) or just call `BuildBackground` explicitly, while live
profiles leave it disabled and queue research externally. The contract is stated
once there; this table only records the per-profile default.

Profiles should be typed constructors or an explicit `Profile` field, not a
bag of independent knobs:

| Constructor | Guarantees |
|-------------|------------|
| `DefaultConfig` | development/evaluation defaults only; not a production profile |
| `ReplayConfig` | deterministic replay defaults with explicit `BuildBackground` calls |
| `LiveSafeConfig` | `AutoResearch=false`, no web scope, queue-ready research settings |
| `LiveWebConfig` | starts from `LiveSafeConfig`, enables web only after admission checks |

Live entrypoints should accept only `LiveSafeConfig` or `LiveWebConfig`-shaped
profiles, so callers cannot accidentally inherit replay defaults.

Queued research boundary:

```go
type ResearchJob struct {
    Topic           string
    Scopes          []ResearchScope
    TranscriptStart int
    TranscriptEnd   int
}

type ResearchScheduler interface {
    Enqueue(ctx context.Context, work ResearchWork) error
}
```

The bot creates one work item per new `topic/scope` transcript range and tracks
topic/scope high-water marks so chunked intake does not repeatedly enqueue the
same prefix. The scheduler owns durable idempotency, ordering by transcript
index range, and queue backpressure. Context cancellation before enqueue returns
an error to the transport; provider failures after enqueue become miss evidence,
not ingestion failures.

The production queue boundary needs an executor contract, not a shared bot
pointer:

```go
type ResearchSnapshot struct {
    Events []MeetingEvent
}

type ResearchWork struct {
    Job      ResearchJob
    Snapshot ResearchSnapshot
}

type ResearchExecutor interface {
    RunResearch(ctx context.Context, work ResearchWork) ([]Evidence, error)
}
```

`ResearchScheduler` may depend on `ResearchExecutor`, `AgentClient`, and
research prompt builders. It must not import CLI command code, live transport
adapters, or private `Bot` mutation helpers. `ResearchExecutor` receives an
immutable transcript snapshot and returns evidence rows, including miss rows for
provider failures. Publishing returned evidence is the only mutation allowed
after enqueue, and it happens through the bot's evidence-publish boundary.

`NewValidated` enforces this boundary at startup: it normalizes the config and
then runs `Config.Validate`, so live profiles reject `AutoResearch=true` and
replay/default profiles only accept `AutoResearch` when a `ResearchScheduler` is
configured. `New` only normalizes the config (it has no error return and is the
infallible constructor); callers that need the boundary enforced before
transcript intake must use `NewValidated`. Either way `Observe` itself fails
closed with `errAutoResearchScheduler` if `AutoResearch=true` and no scheduler is
configured, so an unvalidated `New` cannot reach a synchronous provider path.

### Background Understanding

`BuildBackground` extracts candidate topics from the observed meeting and runs
research for each topic across configured scopes:

| Scope | Role | Default model | Permission posture |
|-------|------|---------------|--------------------|
| `internal` | `RoleInternalResearch` | `sonnet` | transcript-only reasoning |
| `codebase` | `RoleCodebaseResearch` | `gpt-5.3-codex` | plan/read-only |
| `web` | `RoleWebResearch` | `gpt-5.3-codex` | provider/tool dependent |

Each result is stored as `Evidence` with scope, topic, text, timestamp, and
source anchors. Failed research is cached as an explicit miss instead of being
silently ignored; this prevents summaries from inventing unavailable findings.

Research execution is bounded by the parent `context.Context`, a per-call
`--research-timeout`, `MaxResearchTopics`, and `ResearchScopes`. V1 executes
scope calls as bounded jobs and records one evidence row per scope/topic
outcome:

| Outcome | Evidence state |
|---------|----------------|
| success | trimmed model text plus transcript/source anchors |
| provider error, including partial text | explicit miss evidence with the provider error |
| timeout or empty result | explicit "research unavailable" or "no findings" evidence |

Partial success is acceptable. One unavailable scope must not erase other
scopes for the same topic, and summaries must be able to distinguish "not
searched", "searched and empty", and "searched but failed".

Repeated topics should refresh only when new transcript evidence changes the
topic's transcript index range. A final summary path should either run a final
bounded `BuildBackground` pass or explicitly mark the summary as based on the
last published evidence snapshot. Long meetings should track skipped refreshes
and stale evidence so the evaluator can tell whether a summary is current.

### Provider Architecture

The bot uses an `AgentClient` interface:

```go
type AgentClient interface {
    Run(ctx context.Context, req AgentRequest) (AgentResponse, error)
}
```

Production uses `ProviderAgentClient`, which dispatches through the existing
`multiagent/agent` abstraction. That means the meeting bot reuses the repo's
Codex, Claude, Gemini, and model registry plumbing rather than introducing a
new provider stack.

Offline tests and deterministic evaluations use `LocalAgentClient`, which
exercises the same orchestration path without calling external models.

Provider safety is part of the `AgentRequest` contract:

| Role | Permission mode | Effort | Timeout | Max turns | Failure surface |
|------|-----------------|--------|---------|-----------|-----------------|
| `RoleFastAnswer` | `plan` | low | `--answer-timeout` | 4 | fallback answer plus `Answer.Error` |
| `RoleInternalResearch` | `plan` | medium | `--research-timeout` | 4 | explicit miss evidence |
| `RoleCodebaseResearch` | `plan` | medium | `--research-timeout` | 4 | explicit miss evidence |
| `RoleWebResearch` | read-only web, or disabled | medium | `--research-timeout` | 4 | explicit miss evidence |
| `RoleSummary` | `plan` | high | `--summary-timeout` | 4 | fallback summary plus `Summary.Error` |

Unknown model IDs fail before execution unless the model prefix maps to a known
provider family. Provider responses that contain both text and an error keep the
partial text in `AgentResponse.Text` and return the error separately, so callers
can decide whether partial content is usable or should be replaced by a local
fallback.

Role-specific partial-output policy:

| Role | Error or timeout policy | Partial text policy |
|------|-------------------------|---------------------|
| Fast answer | return fallback answer and set `Answer.Error` | discard in v1; future acceptance requires the local opening and provenance checks |
| Summary | return fallback summary and set `Summary.Error` | discard in v1; future acceptance requires section and provenance checks |
| Research | cache explicit miss evidence for the scope/topic | do not promote partial text in v1; keep adapter behavior available for future evaluators |

The parent context and the per-request timeout are composed so whichever cancels
first stops the provider request. `ProviderAgentClient` closes the provider after
execution; any in-flight provider or tool work after cancellation is treated as a
provider error and routed through the role policy above.

Web research is disabled unless the provider exposes a documented read-only
search/browse capability. Provider-default permissions are acceptable only for
local experimentation and must not be used by production profiles. The
compensating controls are still required after admission: web findings must cite
durable source names or URLs, and uncited web output is treated as unusable
evidence.

Production admission is fail-closed:

| Check | Enforcement point | Failure behavior |
|-------|-------------------|------------------|
| role/model family is allowed | config validation before first request | reject real-mode run |
| role permission mode is supported | `ProviderAgentClient` request validation | return role-specific error/fallback |
| web provider is read-only search/browse | provider capability metadata or startup probe | remove `ScopeWeb` or reject real-mode run |
| web output has source names or URLs | provenance validator | cache explicit miss evidence |

Until provider capability metadata exists in `multiagent/agent`, real-mode web
research remains experimental and should be disabled in production configs.
`DefaultConfig` is the development/evaluation baseline; production construction
must choose a named profile and should not inherit web scope by accident.

Web capability checks run at startup and before each web request until provider
metadata is stable. A startup failure rejects the real-mode run or removes
`ScopeWeb`, depending on the selected profile. A per-request drift failure
caches miss evidence with a tool-permission reason and disables further web
requests for that bot instance. Evidence should record whether a scope was
blocked by admission, disabled by drift, or failed after a permitted read-only
tool call.

Web gating is a per-bot state machine guarded by the provider adapter:

| State | New web requests | In-flight requests | Evidence outcome |
|-------|------------------|--------------------|------------------|
| admitted | allowed after preflight | may complete if their preflight still matches the admitted capability generation | success or normal provider miss |
| admission-blocked | rejected before provider call | none | miss evidence with admission reason |
| drift-disabled | rejected after the first drift signal | requests that detect drift are converted to miss rows; requests that already completed still require provenance validation | miss evidence with drift reason |

Each web request records the capability generation it was admitted under. Drift
increments the generation, blocks new web work, and prevents stale in-flight
results from being promoted unless the adapter can prove the same read-only
capability remained active for that request. Counters are emitted once per
`scope/topic` job so concurrent failures do not double-count one blocked
research outcome.

Web telemetry keeps fail-closed behavior distinguishable from provider flakiness:

| Counter | Meaning |
|---------|---------|
| `web_admission_blocked` | provider was not allowed before request start |
| `web_capability_drift_blocked` | provider lost or failed read-only capability at request time |
| `web_provenance_blocked` | provider returned text but lacked usable source anchors |
| `web_provider_failed` | allowed provider errored or timed out |

`ProviderAgentClient` is stateless and creates one provider instance per
request. Concurrent bot calls therefore do not share a provider object. The
underlying `multiagent/agent` provider must support independent instances; if a
provider family requires serialized execution, that serialization belongs in the
provider adapter and must be covered by provider-adapter contract tests.

### Live Answer Path

`AnswerQuestion` is intentionally split into two phases:

1. Compute an immediate evidence-aware opening from local transcript snippets
   and cached research.
2. Send the question, opening, snippets, and research evidence to the
   fast-answer agent for refinement.

The measured `OpeningReadinessLatency` is the time to produce the opening, not
the time for the downstream model to finish. An interactive UI that promises
live response latency must emit `Answer.Opening` before waiting for the
refinement call; the current CLI evaluation prints after `AnswerQuestion`
returns, so `OpeningReadinessLatency` is not by itself the CLI-visible response
latency.

The latency budget and model completion timeout are separate. The default
latency budget remains 10 seconds, while `--answer-timeout` gives the real model
up to 45 seconds to refine the answer. If the model times out, the bot records
the model error and returns the transcript/evidence-grounded fallback rather
than aborting the whole evaluation.

Evaluation reports four SLOs:

| Metric | Includes | Excludes | Failure meaning |
|--------|----------|----------|-----------------|
| `OpeningReadinessLatency` | local snippet/evidence selection and construction of a streamable opening that meets the minimum opening contract | provider execution | a grounded opening was not ready quickly enough to stream |
| `FirstVisibleTextLatency` | question receipt through the transport-visible opening write/flush/ack boundary | provider refinement after the opening | the user did not see grounded text quickly enough |
| `TotalLatency` | complete `AnswerQuestion` wall-clock time | post-meeting summary | final answer or fallback arrived too slowly |
| `TimeToFinalValidatedAnswer` | provider generation, validation, one allowed retry, and fallback/degraded construction | post-meeting summary | the complete validated answer was too slow or never reached normal quality |

`--latency-budget` is an evaluation threshold for opening readiness.
`--answer-timeout` is the hard stop for provider refinement. Slow successful
refinement is reported as high `TotalLatency`; timeout or provider failure is
reported through `Answer.Error` and the fallback text.

The opening must be useful enough to stream: it should include either a
timestamped transcript anchor, a cached evidence reference, or an explicit
statement that no supporting evidence is available yet. Background research and
summary generation are bounded by `--research-timeout` and `--summary-timeout`,
but they are not live-response SLOs in v1. Evaluation reports their wall-clock
latency and failure rate separately from answer latency.

The opening metric cannot be satisfied by arbitrary first words. The minimum
opening contract is one complete sentence or bullet with a transcript timestamp,
evidence reference, or explicit no-evidence marker. An opening that starts with
generic filler, omits grounding, or defers the answer entirely is a quality
failure even if it arrives within budget.

Live transports must also track observe-path health:

| Metric | Meaning | Interpretation |
|--------|---------|----------------|
| `ObserveLatency` | wall-clock time for one `Observe` call | initial live target p95 <= 1s when `AutoResearch` is disabled |
| `AutoResearchEnqueueLatency` | time spent creating and enqueueing research work | queue/backpressure on the ingestion path |
| `TranscriptLag` | receiver wall-clock age of the newest unobserved source event | initial live target p95 <= 5s |
| `FirstVisibleTextLatency` | time from question receipt to first bytes written to the UI/transport | user-visible live answer SLO, target <= `--latency-budget` |

A healthy answer path does not imply a healthy live system. If observe-path
latency or transcript lag is unacceptable for a transport, that transport must
tune queue backpressure and transcript batching; provider calls must stay off the
`Observe` path.

An interactive transport must additionally measure `FirstVisibleTextLatency` at
the write boundary. The reference recipe is: receive question, build
`Answer.Opening`, write opening bytes to the transport, then start or await
refinement. Serialization, batching, terminal rendering, and network flush time
count toward `FirstVisibleTextLatency`; model refinement does not. For transcript
lag, the clock basis is receiver monotonic time: each source event is stamped
when received, and lag is the worst outstanding
receive-to-observe delay. Static replay files and batch-delivered events report
lag as not applicable; non-monotonic speaker timestamps do not affect this
metric.

The transport contract is that only `Answer.Opening` may be emitted before the
refined answer is ready. Refined content is buffered until validation passes or
the fallback/degraded path is selected.

The live transport API should make that boundary explicit:

```go
type AnswerStream interface {
    OnOpening(ctx context.Context, opening string, t time.Time) error
    OnFinal(ctx context.Context, answer Answer, t time.Time) error
}
```

`OnOpening` is the write boundary for `FirstVisibleTextLatency`; the timer starts
when the question is received and stops after `OnOpening` returns. `OnOpening`
must block until the transport has accepted the bytes at its real visibility
boundary: terminal write returned, websocket send acknowledged or flushed, HTTP
stream flushed, or UI event queued to the renderer with a synchronous ack.
Asynchronous transports must not stop the timer at an internal enqueue that can
still wait behind batching or backpressure. Rendering and transport buffering
count. `OnFinal` records `TimeToFinalValidatedAnswer`,
which is reported separately from the first-visible-text SLO and must not be
used to declare the live opening healthy.

### Summary Path

`SummarizeMeeting` sends representative transcript excerpts plus cached
research to the summary agent. The system prompt requires decisions, action
items, risks/blockers, and background/context. If the agent fails or returns an
empty response, a deterministic fallback summary is generated from transcript
signals and cached evidence.

Summary generation has the same two-phase shape as live answers, without the
streaming SLO. Core bot policy selects and budgets representative events plus
evidence; provider execution only synthesizes from that bounded prompt. The
fallback path uses the same selected inputs, so summary policy remains testable
without a provider call.

The summary result must expose research coverage, not only prose. For every
selected topic/scope pair, the summary path records one of:

| Coverage state | Meaning |
|----------------|---------|
| `not_searched` | no research job was scheduled for the selected topic/scope |
| `fresh` | evidence covers the latest transcript index range used by the summary |
| `stale` | evidence exists but predates later transcript turns for that topic |
| `empty` | the scope was searched and returned no findings |
| `failed` | provider, timeout, validation, or web-admission failure produced miss evidence |

Before final synthesis, `SummarizeMeeting` either drains a bounded final
research pass or marks the summary with stale/not-searched coverage. A summary
with failed, stale, or missing coverage can still be useful, but it is not a
normal result; it should carry degraded status plus the coverage reasons so the
UI and evaluator do not mistake "no evidence available" for "no issue exists."

## Evidence and Provenance

The anti-hallucination contract is evidence binding, not only fallback text:

- Transcript-backed claims should cite timestamps or speaker turns when they
  affect a decision, action item, or risk.
- Research-backed claims should cite the evidence scope/topic and any source
  anchor, such as a file path or URL returned by the researcher.
- Unsupported but useful hypotheses must be labeled as uncertain and paired
  with a verification step, or omitted from the final answer.
- Contradictory evidence must be surfaced explicitly instead of collapsed into
  a single confident conclusion.

`Answer.Evidence`, `Answer.ResearchRefs`, and `Summary.Evidence` preserve the
inputs used for post-hoc inspection. Happy-path model output and fallback output
are held to the same provenance rules; a model response that cannot preserve the
opening or cite available anchors should be treated as lower quality even if the
provider call succeeds.

Current v1 evidence anchors are strings. Before production use, the design
should upgrade them to structured citations:

```go
type Citation struct {
    Kind   string // transcript, code, web, internal
    Label  string // timestamp, file path, URL, or source name
    Detail string // optional speaker, line, title, or note
}
```

Prompt construction must pass citation labels alongside each transcript snippet
and evidence item. A post-generation validator owns enforcement:

| Failure | Validator action |
|---------|------------------|
| material claim without citation | mark output invalid; regenerate or fall back |
| hypothesis presented as fact | require uncertainty language or drop claim |
| contradiction collapsed into one conclusion | mark output invalid and surface both sides |
| missing required summary section | regenerate or fall back |

In offline evaluation, validator failures are quality failures even when the
provider call succeeds. In real-mode smoke tests, uncited material claims should
block promotion to production behavior.

Validation status is first-class result data, not only fallback text:

```go
type OutputStatus string

const (
    OutputStatusNormal   OutputStatus = "normal"
    OutputStatusDegraded OutputStatus = "degraded"
    OutputStatusInvalid  OutputStatus = "invalid"
)

type ValidationResult struct {
    Status        OutputStatus
    Reason        string
    MissingInputs []string
}
```

`Answer` and `Summary` carry `Status`, `Validation`, and the evidence/coverage
metadata used to decide that status. `normal` means the output passed the active
validator for its profile. `degraded` means the user sees a grounded fallback or
partial local result with explicit reasons. `invalid` is internal-only and must
not be rendered as a normal answer or summary.

Rejected outputs are never shown as normal answers. The user-visible degraded
shape is:

```text
Status: degraded
Reason: <validation failure or provider failure>
Grounded fallback: <locally generated answer from cited snippets/evidence>
Missing evidence: <what could not be validated>
```

Before the validator exists, real-mode provider output remains evaluation-only;
production transports may show only local fallback/degraded responses generated
from selected snippets and cached evidence.

MVP enforcement profile:

| Claim class | V1 representation | V1 enforcement | Production gate |
|-------------|-------------------|----------------|-----------------|
| transcript fact | formatted timestamp in snippet text | deterministic check for timestamp or explicit no-evidence language | structured transcript citation |
| research fact | `scope/topic` reference plus evidence text | deterministic check for `ResearchRefs` or uncertainty language | structured internal/code/web citation |
| hypothesis | prose marked uncertain | deterministic check for uncertainty wording | validator requires verification step |

Fallback answers and summaries are not exempt from validation. In v1, local
fallbacks are allowed because they are generated only from selected snippets and
cached evidence. Real-mode production promotion requires the validator; until it
exists, real-mode output is evaluation-only. Once the structured validator
exists, both provider output and fallback output use the same promotion path:
validate, try at most one regeneration within the original timeout budget, then
return an explicit degraded result rather than uncited normal prose.
Validation and regeneration share the same parent deadline as the original
answer or summary call. `TotalLatency` includes opening construction, provider
generation, validation, any single retry, and fallback/degraded result
construction.

A material claim is any sentence or bullet that states a decision, action item,
owner, deadline, risk, blocker, status, root cause, external fact, or
recommendation. V1 claim extraction can be sentence/bullet based with
deterministic matchers for those classes; production enforcement needs a golden
transcript set with human-labeled material claims and citations.

Claim-to-evidence acceptance means the cited transcript turn, file path, URL, or
internal evidence text semantically supports the claim, not merely that the text
contains a citation-shaped label. Calibration should include at least 100
labeled material claims across internal-only, codebase, web, contradiction, and
missing-evidence examples. Promotion requires zero accepted high-risk unsupported
claims in the fixed suite, reviewer sign-off on borderline cases, and a recurring
regression run when prompts or validator rules change. If the gate cannot meet
that bar, production scopes stay disabled rather than shipping under a waiver.

Streaming policy:

- only the local `Answer.Opening` may be streamed before validation
- provider refinement is buffered as complete text, then validated
- partial refinement tokens, speculative bullets, and mid-stream corrections are
  not user-visible in v1
- if validation fails after the retry budget is exhausted, the UI receives an
  explicit degraded result with the validation reason and grounded fallback text

Validator roadmap:

| Stage | Enforcement | Use |
|-------|-------------|-----|
| deterministic v1 | timestamp/reference/uncertainty matchers | local replay and CI |
| shadow semantic | semantic scorer runs but cannot block output | eval-only calibration |
| production gate | semantic claim-to-evidence validator blocks output | real-mode promotion |

Borderline semantic cases are adjudicated in the golden suite. False positives
consume retry budget and may degrade the answer; false negatives are treated as
release blockers when they involve decisions, owners, deadlines, root causes, or
external facts.

## Error Handling

- Empty or unavailable research becomes explicit evidence such as
  "Research unavailable..." rather than implicit silence.
- Fast answer failures fall back to a transcript/evidence-grounded answer.
- Summary failures fall back to a structured local summary.
- Unknown model IDs fail early unless their prefix maps to a known provider
  family.
- Every fallback records enough error text or evidence state for the evaluator
  to distinguish provider failure, timeout, empty output, and unsupported
  content.

## Testing Coverage

`bramble/meetingbot:meetingbot_test` covers:

- transcript parsing
- layered research dispatch across internal, codebase, and web scopes
- automatic background research at transcript chunk boundaries
- fast-answer opening latency before the slow agent returns
- summary routing to the high-effort summary agent

Additional coverage required before treating real-mode output as production
ready:

- streaming transcript fixtures that append partial and continuation lines while
  the bot observes events
- failure injection at the `AgentClient` boundary for timeout, empty text,
  partial text with error, unknown model, and unavailable web tools
- concurrency tests for `Observe`, `BuildBackground`, `AnswerQuestion`, and
  `SummarizeMeeting` running against the same bot state
- `AutoResearch=true` tests with a controllable fake client that prove the
  blocking/backpressure behavior, context cancellation, and event-retention
  invariant
- provenance checks that final answers and summaries preserve timestamps, file
  paths, URLs, or uncertainty labels for material claims
- provider-adapter contract tests for permission mode, timeout propagation,
  partial-output handling, max-turn limits, and provider close behavior
- a manual, credential-gated real-provider smoke test that verifies model
  routing, permission modes, max-turn limits, timeout behavior, and citation
  validation
- an integration target such as
  `//bramble/meetingbot/integration:meetingbot_real_provider_test`, tagged
  `manual`/`local` with `gotags = ["integration"]`, for the real role/provider
  matrix when credentials are present
- long-meeting tests for noisy topic growth, repeated failures for the same
  topic, and memory retention across a worst-case transcript
- finalization tests that run a final research drain or explicitly verify a
  stale-evidence summary marker
- prompt-injection and tool-abuse tests at the transcript-to-research boundary,
  including attempts to override system prompts, exfiltrate repo context, force
  writes, or poison topic selection; expected behavior is fail-closed evidence
  with the blocked reason recorded
- transport-level tests with a fake `AnswerStream` that assert `OnOpening`
  happens before refinement, measures `FirstVisibleTextLatency` at the write
  boundary, and buffers final text until validation passes
- queue integration tests for `ResearchScheduler` covering dropped/reordered
  jobs, duplicate topic scheduling, cancellation before enqueue, provider
  failure after enqueue, and summaries that start before research drains

Deterministic streaming harness:

- create one shared `Bot` with a channel-controlled fake `AgentClient`
- append transcript events in one goroutine while another asks questions and a
  third cancels a context mid-research
- assert no deadlock, no duplicate `scope/topic` evidence, stable event indexes,
  and documented stale-snapshot behavior for answers issued before research
  publishes evidence
- run the same harness with `AutoResearch=true` and `AutoResearch=false` so both
  backpressure and queued-research modes stay covered

The harness must include at least one composed end-to-end scenario: transcript
ingestion, queued research, answer streaming, validation, summary generation,
and evidence coverage reporting all run against the same bot instance. Unit
tests can prove individual contracts, but rollout gates should fail on broken
composition even when the isolated tests still pass.

Test tiers:

| Tier | Runs in | Required before |
|------|---------|-----------------|
| deterministic unit/contract tests | `bazel test //...` | every commit and PR |
| local composed streaming harness | `bazel test //...` with fake providers | live transport phase |
| provider adapter contract matrix | scheduled/manual credential-gated target | real-provider phase and recurring provider upgrades |
| real-provider smoke | manual/local integration target with credentials | promotion of any real-mode profile |
| soak/performance | manual or scheduled environment | production live rollout |

Every test item above should be tagged to one of those tiers when implemented.
Provider contract tests are recurring release gates, not one-time launch tests:
run them when model IDs, provider families, permission modes, max-turn handling,
or timeout behavior change.

Monorepo integration checklist:

- `bramble/main.go` command registration still exposes `meetingbot`
- cobra flags map to `meetingbot.Config` without hidden provider defaults
- `multiagent/agent` model registry and permission options keep the role table
  valid
- recording/logging paths used by other bramble commands do not change the
  meetingbot replay contract
- Bazel targets and Gazelle output keep the command and library targets visible
  to `bazel test //...`
- the release checklist includes the role/provider matrix, web admission status,
  and validator calibration status even when real-provider tests are skipped

CI posture:

- deterministic local tests run in `bazel test //...`
- credential-gated provider tests are manual or explicitly skipped with a reason
  when credentials are absent
- long-running live-mode soak tests are not part of default CI until their
  provider dependencies are stable enough to avoid flakes

Operational limits for v1:

- evidence is unbounded within one bot session except for deduplication by
  `scope/topic`
- production use should cap one bot to one meeting transcript and set
  `MaxResearchTopics`, `MaxSnippetsPerPrompt`, and `ResearchScopes` explicitly
- repeated failures for the same `scope/topic` should produce one explicit miss,
  not unbounded duplicate evidence
- sensitive meetings may use only local replay, internal research, or codebase
  research until web admission, citation validation, retention, and deletion
  policies are defined

Full-repo validation is still via:

```bash
scripts/lint.sh
bazel test //... --test_timeout=60
```

## Alternatives, Rollout, and Extension

Rejected alternatives:

| Alternative | Why not v1 |
|-------------|------------|
| Separate meeting-bot service | Adds deployment and state management before the core transcript/evidence loop is proven |
| New provider stack | Duplicates `multiagent/agent` model registry, permission, effort, and lifecycle behavior |
| Single slow answer agent | Simpler, but cannot satisfy a live opening-readiness target when providers are slow |
| Persistent vector/database evidence store | Useful later, but the in-memory cache is enough for one meeting transcript and keeps tests deterministic |

Rollout boundaries:

- MVP is an opt-in CLI and package API over timestamped transcript notes.
- `--agent=local` remains the deterministic default for offline evaluation and
  fixture generation.
- Real-provider runs are manual or credential-gated until provenance and
  failure-injection coverage are in place.
- The first interactive UI must stream `Answer.Opening` before refinement if it
  reports live latency.

Capability rollout ladder:

| Phase | Enabled scopes | Exit criteria | Rollback trigger |
|-------|----------------|---------------|------------------|
| local replay | local agent only | deterministic tests pass | any parsing or fallback regression |
| internal research | `internal` | provenance checks pass on transcripts | uncited material claims |
| codebase research | `internal`, `codebase` | permission/timeout matrix passes | provider errors or stale context above threshold |
| web research | add `web` only after admission checks | read-only provider capability and citation validation | disable `ScopeWeb` on any admission or citation failure |
| live transport | queued research unless backpressure is acceptable | `FirstVisibleTextLatency`, `ObserveLatency`, and `TranscriptLag` targets hold | disable `AutoResearch` or fall back to queued research |

Phase-to-test gates:

| Phase | Minimum gate |
|-------|--------------|
| local replay | deterministic unit tests and local fallback/provenance checks |
| internal research | failure-injection tests plus transcript-backed citation checks |
| codebase research | provider-adapter contract matrix for permission, timeout, max turns, and close behavior |
| web research | read-only admission probe, drift handling tests, citation validation, and web kill-switch test |
| live transport | composed streaming harness with fake `AnswerStream`, queued research, and summary coverage assertions |

Web remains default-off for production profiles until its phase is explicitly
enabled. A global kill switch should remove `ScopeWeb` from every bot instance
without changing transcript ingestion, answer fallback, or summary generation.

Extension points:

- New ingestion sources convert external events into `MeetingEvent` and call
  `Bot.Observe`.
- New research corpora add a `ResearchScope`, `AgentRole`, prompt, and
  permission row without changing answer or summary contracts.
- Research executors can be synchronous in-process replay, asynchronous
  in-process queues, or external workers, but all live-safe executors must
  implement `ResearchScheduler` semantics and queue integration tests.
- Provider swaps belong behind `AgentClient`; command flags and bot logic should
  stay provider-neutral.

Extension checklist:

| Extension | Required additions |
|-----------|--------------------|
| new research scope | `ResearchScope`, `AgentRole`, prompt, permission row, evidence/citation rule, failure-injection test |
| new executor | `ResearchExecutor` implementation, queue semantics test, snapshot immutability test, provider-failure-to-miss mapping |
| external worker | serialized `ResearchJob`/`ResearchSnapshot` schema, idempotency key, result publication API, version compatibility test |
| new live transport | `MeetingEvent` normalizer, `AnswerStream` implementation, observe-lag metric, first-visible-text flush/ack test |
| new provider family | capability metadata, role permission mapping, timeout/max-turn/close contract test, web admission behavior |

External workers are cross-process adapters. They must not receive a `Bot`
reference or private in-process closures; they receive serialized jobs and
snapshots, return evidence rows with status and citations, and publish through a
single idempotent result boundary keyed by job ID plus transcript index range.

Evidence evolution:

- persistence is out of scope for v1; the only storage boundary is the in-memory
  evidence slice owned by one `Bot`
- in-memory v1 cache keys are normalized `scope/topic` pairs plus the transcript
  index range that produced the evidence
- persisted evidence requires a separate design before implementation, including
  store ownership, schema versioning, migration/rollback, stale-reader behavior,
  and cache invalidation rules

The staged path is single-process memory cache first. Persisted evidence and
multi-session reuse are deferred until citation schema and invalidation semantics
are stable.

Deferred non-goals:

- cross-meeting evidence reuse
- multi-tenant retention policy
- long-term storage or deletion workflows for sensitive transcript-derived
  evidence
- persisted evidence TTLs or migrations

Those require a separate design before any rollout phase handles sensitive
meetings with persisted or cross-session evidence.
