---
name: code-review-eval
description: "Compare bramble code-review output across reviewer configs (cursor with composer-2, codex with gpt-5.4-mini, gemini with gemini-2.5-pro). Runs bramble code-review for each config, compares findings side-by-side, and logs results."
argument-hint: "[branch]"
---

# Code Review Eval

Run `bramble code-review` with multiple backend/model configs against the same branch,
then compare their findings side-by-side.

## Configs to evaluate

| Name | Backend | Model | Flags |
|------|---------|-------|-------|
| codex-5.4-mini | codex | gpt-5.4-mini | `--backend codex --model gpt-5.4-mini` |
| cursor-composer2 | cursor | composer-2 | `--backend cursor --model composer-2` |
| gemini-2.5-pro | gemini | gemini-2.5-pro | `--backend gemini --model gemini-2.5-pro` |

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

Use the **Monitor tool** for each config. `--envelope-file` writes the final
ResultEnvelope to a file; stdout carries plain-text progress lines for Monitor to
stream. Codex defaults to `--read-only`. Cursor and Gemini have no read-only mode,
so they run with default permissions.

Create a fresh `$LOG_DIR` under `/tmp/code-review-eval-{timestamp}/`. For each config:

```bash
ENVELOPE_FILE="$LOG_DIR/{NAME}-envelope.json"
WORK_DIR=$(pwd) bazel-bin/bramble/bramble_/bramble code-review \
  {FLAGS} --verbose --timeout 10m --envelope-file "$ENVELOPE_FILE" \
  2>"$LOG_DIR/{NAME}-stderr.txt"
```

Arm all three Monitors in the same turn so configs run in parallel. Set Monitor
`timeout_ms=600000`. After all Monitors complete, read each `$ENVELOPE_FILE` for the
ResultEnvelope (`verdict`, `issues[]`, `summary`). Note: codex only reports shell
command tool calls on stdout; file reads are internal to the codex SDK and not surfaced.
Gemini reports tool calls via ACP.

## Step 3: Compare findings

After all configs complete, read each output and extract the findings. For each config list:
- Issues found (file, line, severity, description)
- Verdict (correct/incorrect)
- Confidence score
- Wall clock time (token counts are only reported by codex backends, not cursor)
- Whether the always-emit envelope guard fired (check stderr for "envelope guard" lines)

Then produce a comparison:

| Finding | cursor-composer2 | codex-5.4-mini | gemini-2.5-pro |
|---------|-----------------|----------------|----------------|
| Issue X | found (medium) | missed | found (high) |
| Issue Y | missed | found (low) | missed |
| FP: ... | flagged | — | flagged |

Identify:
- **Consensus findings**: flagged by all three configs (high confidence these are real)
- **Majority findings**: flagged by two of three configs (likely real)
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

| Config | Findings | FPs | Verdict | Confidence | Time | Tokens (in/out) |
|--------|----------|-----|---------|------------|------|-----------------|
| cursor-composer2 | N | N | ... | ... | ...s | — |
| codex-5.4-mini | N | N | ... | ... | ...s | N/N |
| gemini-2.5-pro | N | N | ... | ... | ...s | — |

Consensus (all 3): ...
Majority (2 of 3): ...
Unique to cursor: ...
Unique to codex-5.4-mini: ...
Unique to gemini-2.5-pro: ...
Disagreements: ...
```

Schema note: the `Tokens (in/out)` column was added when Gemini support was introduced
(2026-04-21). Historical rows without this column remain valid — treat missing as `—`.

## Final Summary

```
## Code Review Eval Summary

| Config | Findings | FPs | Verdict | Time |
|--------|----------|-----|---------|------|
| cursor-composer2 | | | | |
| codex-5.4-mini | | | | |
| gemini-2.5-pro | | | | |

Best config: ...
Recommendation: ...
```

## Key Files

| Area | Files |
|------|-------|
| Review CLI | `bramble/cmd/codereview/codereview.go` |
| Reviewer impl | `yoloswe/reviewer/reviewer.go` |
| Cursor backend | `yoloswe/reviewer/backend_cursor.go` |
| Codex backend | `yoloswe/reviewer/backend_codex.go` |
| Gemini backend | `yoloswe/reviewer/backend_gemini.go` |
| Run history | `.claude/skills/code-review-eval/data/eval-runs.log` |
