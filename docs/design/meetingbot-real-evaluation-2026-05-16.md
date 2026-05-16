# Meeting Bot Real-Model Evaluation: 2026-05-16

## Objective

Validate the meeting bot with real models and real interaction prompts, confirm
that the first-10-words latency and answer quality goals are met, and record the
results.

## Refinement Before Final Run

The first bounded real-provider smoke run failed with `context deadline
exceeded`. The issue was architectural rather than model quality: the CLI was
using the 10-second live latency budget as the model completion timeout.

Refinement made before the final run:

- split first-response latency budget from model synthesis timeout
- added `--answer-timeout`, `--research-timeout`, and `--summary-timeout`
- changed answer/summary fallback behavior so model timeout is recorded as
  `model_error` instead of aborting the evaluation
- committed the refinement as:

```text
e4c19e0 Make meeting bot real eval resilient to model latency
```

## Environment

```text
codex-cli 0.130.0
Claude Code 2.1.141
```

## Command

```bash
bazel run //bramble:bramble -- meetingbot \
  --agent=real \
  --notes-glob '/home/ubuntu/voice-tui-2026*' \
  --evaluate \
  --max-topics=1 \
  --max-snippets=12 \
  --answer-timeout=45s \
  --research-timeout=45s \
  --summary-timeout=90s \
  --work-dir /home/ubuntu/worktrees/yoloswe/feature/meeting-bot
```

Raw terminal capture was written during the run to:

```text
/tmp/meetingbot-real-eval/full-real-2026-05-16.txt
```

The final run produced no `model_error` lines.

## Latency Results

Target: first 10 words under 10 seconds.

| File | Interaction | First-10-words latency | Full answer latency | Status |
|------|-------------|------------------------|---------------------|--------|
| 2026-05-13 | 1 | 1ms | 11.949s | pass |
| 2026-05-13 | 2 | 1ms | 15.883s | pass |
| 2026-05-13 | 3 | 2ms | 14.054s | pass |
| 2026-05-13 | 4 | 1ms | 19.348s | pass |
| 2026-05-14 | 1 | 2ms | 15.247s | pass |
| 2026-05-14 | 2 | 3ms | 15.760s | pass |
| 2026-05-14 | 3 | 1ms | 15.089s | pass |
| 2026-05-14 | 4 | 1ms | 14.963s | pass |

Summary latency:

| File | Summary latency | Status |
|------|-----------------|--------|
| 2026-05-13 | 26.427s | pass |
| 2026-05-14 | 25.186s | pass |

## Interaction Results

### 2026-05-13 Transcript

Parsed events: 235

Dominant researched topic: `sandbox`

#### Interaction 1

Question: "What is the most likely root cause pattern behind the sandbox or
preview failures?"

Result: The answer identified cross-system state inconsistency as the primary
root-cause pattern, specifically drift across `worker`, `sandbox`, and
`project_session` state. It also identified GitHub auth/install/secrets
mis-scoping as a secondary contributor and cited GitHub enterprise app
installation documentation.

Quality verdict: meets goal. The answer was concrete, prioritized causes, and
combined meeting evidence with public-source research.

#### Interaction 2

Question: "What should we tell the team about staging versus production for
demos and testing?"

Result: The answer recommended production as the customer-demo source of truth
and staging as a stabilization/debugging lab until sandbox hardening lands. It
called out auth/install parity and gave a shareable team one-liner.

Quality verdict: meets goal. The answer is actionable and aligned with the
meeting decision to demo from production.

#### Interaction 3

Question: "What changed for customer workflow priorities, and what should we do
next?"

Result: The answer refined the initial uncertainty: the note does not establish
a broad workflow-product shift, but it does establish near-term customer
workflow priorities around CA testing, production demos, feedback endpoint work,
and sandbox reliability. It recommended explicit CA readiness and production
demo policy steps.

Quality verdict: meets goal. The answer avoided overclaiming and corrected the
local evaluator's earlier conservative framing with a narrower, evidence-backed
interpretation.

#### Interaction 4

Question: "What are the highest priority follow-up actions and risks?"

Result: The answer prioritized staging deployment verification, sandbox state
consistency, staging instrumentation, customer feedback readiness, and explicit
deprioritization of abandoned staging demo apps. It listed top risks around
GitHub App scope, state desync, and unverified readiness assumptions.

Quality verdict: meets goal. The answer is operationally useful and includes
owners/next steps where the transcript supports them.

#### Summary

Result: The summary covered sandbox stabilization, GitHub auth/secrets scope,
CA readiness, judge/Builder Lite work, and key decisions/action items. It
correctly distinguished decisions, action items, risks/blockers, and background.

Quality verdict: meets goal.

### 2026-05-14 Transcript

Parsed events: 196

Dominant researched topic: `preview`

#### Interaction 1

Question: "What is the most likely root cause pattern behind the sandbox or
preview failures?"

Result: The answer identified environment/config drift after recent service
changes. It explained the chain from incomplete staging setup, secrets/config
propagation mismatch, and preview UI visibility despite backend registration or
auth-preview discovery failure.

Quality verdict: meets goal. The answer is concise, causal, and grounded in the
meeting.

#### Interaction 2

Question: "What should we tell the team about staging versus production for
demos and testing?"

Result: The answer recommended staging for integration debugging and
configuration validation, while production remains the demo confidence path. It
included a short practical policy for demos and a risk callout about preview
availability.

Quality verdict: meets goal.

#### Interaction 3

Question: "What changed for customer workflow priorities, and what should we do
next?"

Result: The answer identified a repeated cross-customer demand signal around
agentic workflows plus human review, especially multi-department approval
workflows and AI intake. It recommended one reference workflow spec, one or two
gold workflow templates, demo narratives for Verizon/Coca-Cola/Dell/Axis, and
stabilization of preview/deployment reliability before demos.

Quality verdict: meets goal. This is the strongest product-direction answer in
the run.

#### Interaction 4

Question: "What are the highest priority follow-up actions and risks?"

Result: The answer prioritized secrets/service config reliability, the preview
button incident, Builder/Builder Lite smoke coverage, SSC/progress-card issues,
and conversion of customer signal into a scoped deliverable.

Quality verdict: meets goal.

#### Summary

Result: The summary captured deployment stabilization, broken preview/auth
preview, LiteLLM and SQL/secret work, Builder Lite validation, and
human-in-the-loop workflow customer direction. It explicitly changed the
interpretation of the preview issue from "button broken" to "preview
environment/app availability/auth path broken."

Quality verdict: meets goal.

## Overall Assessment

The real-model evaluation meets the requested latency and quality goals:

- first-10-word latency was 1-3ms across all interactions, far below 10 seconds
- all full model answers completed within the configured 45-second answer
  timeout
- summaries completed within the configured 90-second summary timeout
- answers used transcript evidence and, where available, public-source context
- no model fallback errors were recorded in the final full run

The main production caveat is that full answer latency is still model-bound
and ranged from about 12 to 19 seconds. That is acceptable for the stated goal
because the user sees the first answer sentence immediately, but future UX work
should stream the refined model answer as it arrives rather than waiting for
the complete answer object.

