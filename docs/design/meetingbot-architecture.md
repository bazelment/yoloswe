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
The file-group ownership below is a maintainability rule, not a compile-time
sub-package split. If those boundaries start failing in review, the next step is
to split provider adaptation and transcript ingestion into subpackages behind
the same exported facades.

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
  lock, releases the lock, and only then runs any synchronous `BuildBackground`
  work triggered by `AutoResearch`.
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
  --notes-glob '/home/ubuntu/voice-tui-2026*' \
  --evaluate \
  --work-dir /home/ubuntu/worktrees/yoloswe/feature/meeting-bot
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

V1 `AutoResearch` is synchronous: the triggering `Observe` call appends the
event, then runs `BuildBackground` before returning. This behavior is acceptable
for deterministic CLI replay, but a true live transport has to treat it as
backpressure. If provider latency cannot block transcript intake, the caller
should disable `AutoResearch` and run background research from an external queue
or worker. Cancellation is inherited from the `Observe` context; if research is
cancelled or fails, the event remains observed and the caller receives the
research error.

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
| `RoleWebResearch` | provider default | medium | `--research-timeout` | 4 | explicit miss evidence |
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

Web research uses provider-default permissions because public-web tools vary by
backend and may require capabilities that do not map cleanly to `plan`. The
compensating control is that web findings must cite durable source names or URLs;
uncited web output is treated as unusable evidence. If a web-capable provider
supports a read-only or plan permission mode, that should replace the default.
For production use, `ScopeWeb` is admitted only for providers whose effective
web behavior is documented as read-only search/browsing. Providers that cannot
make that guarantee are rejected for `RoleWebResearch` or run with `ScopeWeb`
disabled.

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

The measured `First10WordsLatency` is the time to produce the opening, not the
time for the downstream model to finish. It is an opening-readiness metric. An
interactive UI that promises live response latency must emit `Answer.Opening`
before waiting for the refinement call; the current CLI evaluation prints after
`AnswerQuestion` returns, so `First10WordsLatency` is not by itself the
CLI-visible response latency.

The latency budget and model completion timeout are separate. The default
latency budget remains 10 seconds, while `--answer-timeout` gives the real model
up to 45 seconds to refine the answer. If the model times out, the bot records
the model error and returns the transcript/evidence-grounded fallback rather
than aborting the whole evaluation.

Evaluation reports two SLOs:

| Metric | Includes | Excludes | Failure meaning |
|--------|----------|----------|-----------------|
| `First10WordsLatency` | local snippet/evidence selection and opening construction | provider execution | opening was not ready quickly enough to stream |
| `TotalLatency` | complete `AnswerQuestion` wall-clock time | post-meeting summary | final answer or fallback arrived too slowly |

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

Live transports must also track observe-path health:

| Metric | Meaning | Interpretation |
|--------|---------|----------------|
| `ObserveLatency` | wall-clock time for one `Observe` call | includes synchronous `AutoResearch` backpressure |
| `AutoResearchLatency` | time spent in a triggered `BuildBackground` call | provider slowness on the ingestion path |
| `TranscriptLag` | age of the newest observed event relative to transcript source time | freshness of the meeting context |

A healthy answer path does not imply a healthy live system. If observe-path
latency or transcript lag is unacceptable for a transport, that transport must
disable synchronous `AutoResearch` and run research from a queue.

### Summary Path

`SummarizeMeeting` sends representative transcript excerpts plus cached
research to the summary agent. The system prompt requires decisions, action
items, risks/blockers, and background/context. If the agent fails or returns an
empty response, a deterministic fallback summary is generated from transcript
signals and cached evidence.

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

MVP enforcement profile:

| Claim class | V1 representation | V1 enforcement | Production gate |
|-------------|-------------------|----------------|-----------------|
| transcript fact | formatted timestamp in snippet text | deterministic check for timestamp or explicit no-evidence language | structured transcript citation |
| research fact | `scope/topic` reference plus evidence text | deterministic check for `ResearchRefs` or uncertainty language | structured internal/code/web citation |
| hypothesis | prose marked uncertain | deterministic check for uncertainty wording | validator requires verification step |

Fallback answers and summaries are not exempt from validation. In v1, local
fallbacks are allowed because they are generated only from selected snippets and
cached evidence. Once the structured validator exists, both provider output and
fallback output use the same promotion path: validate, try at most one
regeneration for provider output, then return an explicit degraded result rather
than uncited normal prose.

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

Deterministic streaming harness:

- create one shared `Bot` with a channel-controlled fake `AgentClient`
- append transcript events in one goroutine while another asks questions and a
  third cancels a context mid-research
- assert no deadlock, no duplicate `scope/topic` evidence, stable event indexes,
  and documented stale-snapshot behavior for answers issued before research
  publishes evidence
- run the same harness with `AutoResearch=true` and `AutoResearch=false` so both
  backpressure and queued-research modes stay covered

Monorepo integration checklist:

- `bramble/main.go` command registration still exposes `meetingbot`
- cobra flags map to `meetingbot.Config` without hidden provider defaults
- `multiagent/agent` model registry and permission options keep the role table
  valid
- recording/logging paths used by other bramble commands do not change the
  meetingbot replay contract
- Bazel targets and Gazelle output keep the command and library targets visible
  to `bazel test //...`

CI posture:

- deterministic local tests run in `bazel test //...`
- credential-gated provider tests are manual or explicitly skipped with a reason
  when credentials are absent
- long-running live-mode soak tests are not part of default CI until their
  provider dependencies are stable enough to avoid flakes

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

Extension points:

- New ingestion sources convert external events into `MeetingEvent` and call
  `Bot.Observe`.
- New research corpora add a `ResearchScope`, `AgentRole`, prompt, and
  permission row without changing answer or summary contracts.
- Provider swaps belong behind `AgentClient`; command flags and bot logic should
  stay provider-neutral.

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
