# Meeting Bot Evaluation

## Objective

Evaluate the meeting bot against private transcript fixtures without committing
meeting contents to the repository. The evaluation checks:

- transcript ingestion
- background topic extraction
- multiple live question types
- grounded opening latency for live answers
- post-meeting summary generation
- answer quality against transcript evidence

## Privacy Boundary

Evaluation inputs are private meeting transcripts stored outside the repository.
Docs must not include transcript filenames, participant names, customer names,
raw excerpts, generated answers, generated summaries, or meeting-specific facts.

Only aggregate, non-content-bearing metrics are safe to record here.

## Automated Deterministic Evaluation

The deterministic eval is automated through:

```bash
MEETINGBOT_NOTES_GLOB='<private-transcripts-glob>' bramble/meetingbot/eval.sh
```

The script runs the meeting bot in local replay mode, evaluates every transcript
matched by `MEETINGBOT_NOTES_GLOB`, writes a JSON report to
`/tmp/meetingbot-eval-report.json` by default, and exits non-zero if the quality
gate fails.

Equivalent direct command:

```bash
bazel run //bramble:bramble -- meetingbot \
  --agent=local \
  --notes-glob '<private-transcripts-glob>' \
  --evaluate \
  --quality-gate \
  --eval-report /tmp/meetingbot-eval-report.json \
  --work-dir /path/to/repo
```

Why local mode is the default recorded path:

- It is deterministic and suitable for repeatable repo validation.
- It exercises the same orchestration path as real mode: transcript ingestion,
  topic extraction, research request routing, cached evidence, live answer
  synthesis, and summary synthesis.
- It avoids making test results depend on model latency, credentials, network
  state, or public-web tool availability.

## Interaction Set

The default evaluation asks four generalized question types per transcript:

1. operational root-cause analysis
2. environment/demo/testing policy
3. product or stakeholder priority changes
4. follow-up actions and risks

The actual prompt text is implementation data in `bramble/meetingbot/eval.go`.
Do not paste prompt outputs or transcript-specific answers into docs.

## Aggregate Results

Latest local deterministic run:

| Metric | Result |
|--------|--------|
| Private transcripts evaluated | 2 |
| Interactions per transcript | 4 |
| Parsed events | 235 and 196 |
| Opening readiness target | <= 10s |
| Observed opening readiness | 1-2ms |
| Observed total local answer latency | 1-3ms |
| Summary validation status | normal for both transcripts |
| Quality gate | pass |

These metrics intentionally omit transcript titles, extracted topics, answer
text, summary text, and meeting-specific interpretation.

## Quality Gate

The automated meeting-bot quality gate checks:

- at least one transcript file was evaluated
- each file parsed events and ran the default interaction count
- every answer produced a non-empty grounded opening and final answer
- opening readiness stayed under the configured budget
- total answer and summary latency stayed within configured budgets
- answers and summaries returned `normal` validation status
- summaries contained the required sections
- no model errors or fallback errors were recorded

The implementation passed:

```bash
MEETINGBOT_NOTES_GLOB='<private-transcripts-glob>' bramble/meetingbot/eval.sh
scripts/lint.sh
bazel build //...
bazel test //... --test_timeout=60
```

## Retention Policy

Do not check in:

- raw meeting transcripts
- terminal captures containing generated meeting answers
- model-generated summaries of private meetings
- named participants, customer names, or internal incident details
- JSON eval reports if they include transcript paths, prompts, answers, or
  summaries

Temporary eval reports may be written under `/tmp` for local validation, but
they should be treated as private artifacts and not copied into `docs/`.
