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
- provenance checks that final answers and summaries preserve timestamps, file
  paths, URLs, or uncertainty labels for material claims
- a manual, credential-gated real-provider smoke test that verifies model
  routing, permission modes, max-turn limits, and timeout behavior

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
- Evidence schema changes should be additive until a persisted store exists.
- Provider swaps belong behind `AgentClient`; command flags and bot logic should
  stay provider-neutral.
