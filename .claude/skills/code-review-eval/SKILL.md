---
name: code-review-eval
description: "Compare bramble code-review output across reviewer configs (cursor with composer-2, codex with gpt-5.4/gpt-5.4-mini). Runs bramble code-review for each config, compares findings side-by-side, and logs results."
argument-hint: "[branch]"
---

# Code Review Eval

Run `bramble code-review` with multiple backend/model configs against the same branch,
then compare their findings side-by-side.

## Configs to evaluate

| Name | Backend | Model | Flags |
|------|---------|-------|-------|
| codex-5.4 | codex | gpt-5.4 | `--backend codex --model gpt-5.4` |
| codex-5.4-mini | codex | gpt-5.4-mini | `--backend codex --model gpt-5.4-mini` |
| cursor-composer2 | cursor | composer-2 | `--backend cursor --model composer-2` |

## Step 1: Build and identify target

```bash
bazel build //bramble:bramble
```

Identify the branch to review. If an argument is given, `git checkout` that branch
first. Otherwise use the current branch. Get the diff summary:

```bash
git diff $(git merge-base origin/main HEAD)..HEAD --stat
```

## Step 2: Run each config

Run `bramble code-review --envelope-file` for each config using the Monitor tool.
`--envelope-file` writes the final ResultEnvelope to a dedicated file; stdout carries
only NDJSON progress events — exactly what Monitor streams line-by-line. Codex
defaults to `--read-only` (denies file writes via approval handler). Cursor has no
read-only mode, so run it last.

Create a fresh `$LOG_DIR` under `/tmp/code-review-eval-{timestamp}/`. For each config:

```bash
ENVELOPE_FILE="$LOG_DIR/{NAME}-envelope.json"
WORK_DIR=$(pwd) bazel-bin/bramble/bramble_/bramble code-review \
  {FLAGS} --verbose --timeout 10m --envelope-file "$ENVELOPE_FILE" \
  2>"$LOG_DIR/{NAME}-stderr.txt"
```

Where `{FLAGS}` are taken from the config table (e.g. `--backend codex --model gpt-5.4`).

Use the **Monitor tool** with the above command so you can observe progress events
mid-run and react to them. Set `timeout_ms=600000`. After Monitor signals completion
for each config, read `$ENVELOPE_FILE` to get the ResultEnvelope.

### Parsing output

Progress events arrive on Monitor's stdout stream (one JSON object per line):

- **Progress events**: `{"event":"progress","kind":"...","tool":"...","detail":"...",...}`
  - Kinds: `session_info`, `tool_use`, `verdict`
  - The `verdict` progress event carries `issue_count` (emitted just before the envelope;
    always emitted even if the envelope file write fails)

After Monitor completes, read the envelope file:

- **ResultEnvelope** (`$ENVELOPE_FILE`): `{"schema_version":1,"verdict":"...","issues":[...],...}`

The always-emit envelope guard ensures a ResultEnvelope is written even if the review
goroutine panics mid-stream; check stderr for any "envelope guard" messages that indicate
the guard fired rather than the normal path.

Parse the final envelope to extract: `verdict`, `confidence`, `issues[]`, `summary`.

## Step 3: Compare findings

After all configs complete, read each output and extract the findings. For each config list:
- Issues found (file, line, severity, description)
- Verdict (correct/incorrect)
- Confidence score
- Wall clock time (token counts are only reported by codex backends, not cursor)
- Whether the always-emit envelope guard fired (check stderr for "envelope guard" lines)

Then produce a comparison:

| Finding | cursor-composer2 | codex-5.4 | codex-5.4-mini |
|---------|-----------------|-----------|----------------|
| Issue X | found (medium) | found (high) | missed |
| Issue Y | missed | found (low) | found (low) |
| FP: ... | flagged | — | — |

Identify:
- **Consensus findings**: flagged by all configs (high confidence these are real)
- **Unique findings**: only one config caught it (investigate — real issue or FP?)
- **False positives**: findings that are clearly not issues
- **Disagreements**: findings where configs differ on severity or applicability

Check `.claude/skills/code-review-eval/references/known-blind-spots.md` for previously
identified blind spots and note if any recur.

## Step 4: Log results

Append to `.claude/skills/code-review-eval/data/eval-runs.log`:

```
## {DATE} — {BRANCH}

Diff: {N} files, {+/-} lines

| Config | Findings | FPs | Verdict | Confidence | Time |
|--------|----------|-----|---------|------------|------|
| cursor-composer2 | N | N | ... | ... | ... |
| codex-5.4 | N | N | ... | ... | ... |
| codex-5.4-mini | N | N | ... | ... | ... |

Consensus: ...
Unique to cursor: ...
Unique to codex-5.4: ...
Unique to codex-5.4-mini: ...
Disagreements: ...
```

## Final Summary

```
## Code Review Eval Summary

| Config | Findings | FPs | Verdict | Time |
|--------|----------|-----|---------|------|
| ... | | | | |

Best config: ...
Recommendation: ...
```

## Key Files

| Area | Files |
|------|-------|
| Review CLI | `bramble/cmd/codereview/codereview.go` |
| Reviewer impl | `yoloswe/reviewer/reviewer.go` |
| Progress stream | `yoloswe/reviewer/progress.go` |
| Cursor backend | `yoloswe/reviewer/backend_cursor.go` |
| Codex backend | `yoloswe/reviewer/backend_codex.go` |
| Run history | `.claude/skills/code-review-eval/data/eval-runs.log` |
