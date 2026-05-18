# Meeting Bot Real-Model Evaluation: 2026-05-16

## Objective

Validate the meeting bot with real providers while keeping private meeting
content out of repository documents.

## Privacy Boundary

The real-model run used private transcript fixtures stored outside the repo.
This document intentionally records only process and aggregate pass/fail data.
It must not include transcript filenames, participant names, customer names,
questions containing private topics, generated answers, generated summaries, or
meeting-specific findings.

The raw real-model terminal capture and full generated transcript are private
artifacts and are not checked in.

## Refinement Before Final Run

The first bounded real-provider smoke run failed with `context deadline
exceeded`. The issue was architectural rather than model quality: the CLI was
using the live opening latency budget as the model completion timeout.

Refinement made before the final run:

- split opening latency budget from model synthesis timeout
- added `--answer-timeout`, `--research-timeout`, and `--summary-timeout`
- changed answer/summary fallback behavior so model timeout is recorded as
  `model_error` instead of aborting the evaluation

## Sanitized Command Shape

```bash
bazel run //bramble:bramble -- meetingbot \
  --agent=real \
  --notes-glob '<private-transcripts-glob>' \
  --evaluate \
  --max-topics=1 \
  --max-snippets=12 \
  --answer-timeout=45s \
  --research-timeout=45s \
  --summary-timeout=90s \
  --work-dir /path/to/repo
```

## Aggregate Results

Target: grounded opening readiness under 10 seconds.

| Metric | Result |
|--------|--------|
| Private transcripts evaluated | 2 |
| Interactions per transcript | 4 |
| Opening readiness | 1-3ms |
| Full answer synthesis | 12-19s |
| Summary synthesis | 25-27s |
| Answer timeout | 45s |
| Summary timeout | 90s |
| Model fallback errors | 0 |
| Quality verdict | pass |

## Overall Assessment

The real-model evaluation met the latency and quality goals at an aggregate
level:

- opening readiness stayed far below the configured budget
- all full model answers completed within the configured answer timeout
- summaries completed within the configured summary timeout
- no model fallback errors were recorded in the final run

The main product caveat is that full answer latency remains model-bound. The
architecture handles that by making the grounded opening available immediately
and tracking final-answer latency separately.
