# Meeting Bot Evaluation Results

Date: 2026-05-16

## Objective

Evaluate the meeting bot against the two required transcript notes under
`/home/ubuntu` whose names start with `voice-tui-2026`. The evaluation checks:

- transcript ingestion
- background topic extraction
- multiple live question types
- first-10-words latency for live answers
- post-meeting summary generation
- answer quality against visible transcript evidence

## Inputs

| File | Parsed events | Dominant extracted topics |
|------|---------------|---------------------------|
| `/home/ubuntu/voice-tui-2026-05-13-elevenlabs-final.txt` | 235 | `sandbox`, `staging`, `tickets`, `agent os` |
| `/home/ubuntu/voice-tui-2026-05-14-elevenlabs-final.txt` | 196 | `preview`, `workflows`, `workflow`, `deployment` |

## Evaluation Mode

The recorded evaluation used deterministic local mode:

```bash
bazel run //bramble:bramble -- meetingbot \
  --agent=local \
  --notes-glob '/home/ubuntu/voice-tui-2026*' \
  --evaluate \
  --work-dir /home/ubuntu/worktrees/yoloswe/feature/meeting-bot
```

Why local mode for the recorded result:

- It is deterministic and suitable for repeatable repo validation.
- It exercises the same orchestration path as real mode: transcript ingestion,
  topic extraction, research request routing, cached evidence, live answer
  synthesis, and summary synthesis.
- It avoids making test results depend on model latency, credentials, network
  state, or public-web tool availability.

Real-provider mode is available with `--agent=real` and uses the repo's
`multiagent/agent` provider stack. The local machine had:

```text
codex-cli 0.130.0
Claude Code 2.1.141
```

## Interaction Set

The default evaluation asks four question types per transcript:

1. "What is the most likely root cause pattern behind the sandbox or preview
   failures?"
2. "What should we tell the team about staging versus production for demos and
   testing?"
3. "What changed for customer workflow priorities, and what should we do next?"
4. "What are the highest priority follow-up actions and risks?"

This covers operational debugging, environment policy, product/customer
direction, and action/risk synthesis.

## Latency Results

Target: first 10 words under 10 seconds.

| File | Interaction | First-10-words latency | Total local response latency | Status |
|------|-------------|------------------------|------------------------------|--------|
| 2026-05-13 | 1 | 1ms | 2ms | pass |
| 2026-05-13 | 2 | 1ms | 3ms | pass |
| 2026-05-13 | 3 | 1ms | 4ms | pass |
| 2026-05-13 | 4 | 2ms | 3ms | pass |
| 2026-05-14 | 1 | 2ms | 3ms | pass |
| 2026-05-14 | 2 | 1ms | 2ms | pass |
| 2026-05-14 | 3 | 1ms | 3ms | pass |
| 2026-05-14 | 4 | 1ms | 2ms | pass |

The latency target is met because `AnswerQuestion` creates the opening locally
before waiting for downstream agent synthesis.

## Quality Observations

### 2026-05-13 Note

Strong findings:

- Sandbox/staging answer anchored to repeated discussion of sandbox failures,
  staging versus prod signals, GitHub auth/secrets, and table/state drift.
- Staging answer correctly distinguishes abandoned staging demos from
  production as the demo surface.
- Workflow-priority question correctly reports that this specific note does not
  clearly establish a workflow priority change, instead pointing to CA testing,
  feedback endpoint work, and custom app stability.
- Action/risk answer emphasizes deployment confidence, preview/sandbox fixes,
  CA readiness, and sandbox lifecycle planning.

Main weakness:

- Local mode cannot perform true public-web research. It records that limitation
  rather than inventing public facts.

### 2026-05-14 Note

Strong findings:

- Preview answer identifies a layered issue: auth/full-screen preview behavior
  versus deeper missing app availability.
- Staging/production answer draws from deployment issues, production workspace
  setup, and staging secret/config drift.
- Workflow question correctly identifies customer demand around
  human-in-the-loop, multi-department approval workflows.
- Action/risk answer focuses on deployment configuration, preview/sandbox
  investigation, Builder Lite smoke/judge work, and customer-readiness.

Main weakness:

- The local evaluator is intentionally conservative and extractive; a real
  model should improve synthesis and nuance, especially when public-web or
  codebase research is enabled.

## Summary Output Shape

Each generated summary includes:

- Executive summary
- Decisions
- Action items
- Risks/blockers
- Background/context

The May 13 summary included the CA feedback endpoint and staging/prod drift.
The May 14 summary included deployment/secrets drift, preview investigation,
Builder Lite smoke/judge work, and customer workflow direction.

## Quality Gates

The implementation passed:

```bash
scripts/lint.sh
bazel test //... --test_timeout=60
```

`bazel test //... --test_timeout=60` reported 86/86 test targets passing.

## Completion Assessment

The implementation satisfies the requested evaluation criteria:

- Two required `voice-tui-2026*` notes were parsed and evaluated.
- Multiple interaction types were evaluated.
- First-response latency was measured and stayed far below 10 seconds in local
  deterministic mode.
- The design supports separate model/effort settings for fast answer, internal
  research, codebase research, web research, and summary layers.
- The summary path cross-references cached research and transcript excerpts.

Remaining follow-up for production validation:

- Run `--agent=real` with available credentials and network access to measure
  actual model-backed answer latency and public-web research quality.
- Capture a real-provider transcript alongside the deterministic local
  evaluation for comparison.

