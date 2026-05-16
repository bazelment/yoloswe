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
| `--latency-budget` | `10s` | Target first-10-words latency |

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

### Live Answer Path

`AnswerQuestion` is intentionally split into two phases:

1. Compute an immediate evidence-aware opening from local transcript snippets
   and cached research.
2. Send the question, opening, snippets, and research evidence to the
   fast-answer agent for refinement.

The measured `First10WordsLatency` is the time to produce the opening, not the
time for the downstream model to finish. This is the layer that makes the live
answer path fit a sub-10-second first-response target even when deeper model
synthesis takes longer.

### Summary Path

`SummarizeMeeting` sends representative transcript excerpts plus cached
research to the summary agent. The system prompt requires decisions, action
items, risks/blockers, and background/context. If the agent fails or returns an
empty response, a deterministic fallback summary is generated from transcript
signals and cached evidence.

## Error Handling

- Empty or unavailable research becomes explicit evidence such as
  "Research unavailable..." rather than implicit silence.
- Fast answer failures fall back to a transcript/evidence-grounded answer.
- Summary failures fall back to a structured local summary.
- Unknown model IDs fail early unless their prefix maps to a known provider
  family.

## Testing Coverage

`bramble/meetingbot:meetingbot_test` covers:

- transcript parsing
- layered research dispatch across internal, codebase, and web scopes
- automatic background research at transcript chunk boundaries
- fast-answer opening latency before the slow agent returns
- summary routing to the high-effort summary agent

Full-repo validation is still via:

```bash
scripts/lint.sh
bazel test //... --test_timeout=60
```

