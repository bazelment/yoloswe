---
name: code-review-eval
description: "Compare bramble code-review output across reviewer configs (cursor with composer-2, codex with gpt-5.4/gpt-5.4-nano). Runs bramble code-review for each config, compares findings side-by-side, and logs results."
argument-hint: "[branch or PR]"
---

# Code Review Eval

Run `bramble code-review` with multiple backend/model configs against the same branch,
then compare their findings side-by-side.

## Configs to evaluate

| Name | Backend | Model | Flags |
|------|---------|-------|-------|
| cursor-composer2 | cursor | composer-2 | `--backend cursor --model composer-2` |
| codex-5.4 | codex | gpt-5.4 | `--backend codex --model gpt-5.4` |
| codex-5.4-nano | codex | gpt-5.4-nano | `--backend codex --model gpt-5.4-nano` |

## Step 1: Build and identify target

```bash
bazel build //bramble:bramble
```

Identify the branch to review. If an argument is given, use it. Otherwise use the
current branch. Get the diff summary for context:

```bash
git diff origin/main..HEAD --stat
```

## Step 2: Run each config

Run `bramble code-review` for each config sequentially. Each run uses the same
working directory and sees the same diff.

```bash
WORK_DIR=$(pwd) bazel-bin/bramble/bramble_/bramble code-review \
  --backend {BACKEND} --model {MODEL} --verbose --timeout 10m \
  2>"$LOG_DIR/{NAME}-stderr.txt" | tee "$LOG_DIR/{NAME}-stdout.txt"
```

Use `run_in_background` + `timeout=600000` so you can read completed results while
the next config runs. Create a fresh `$LOG_DIR` under `/tmp/code-review-eval-{timestamp}/`.

## Step 3: Compare findings

After all configs complete, read each output and extract the findings. For each config list:
- Issues found (file, line, severity, description)
- Verdict (correct/incorrect)
- Confidence score
- Wall clock time and token usage

Then produce a comparison:

| Finding | cursor-composer2 | codex-5.4 | codex-5.4-nano |
|---------|-----------------|-----------|----------------|
| Issue X | found (medium) | found (high) | missed |
| Issue Y | missed | found (low) | found (low) |
| FP: ... | flagged | — | — |

Identify:
- **Consensus findings**: flagged by all configs (high confidence these are real)
- **Unique findings**: only one config caught it (investigate — real issue or FP?)
- **False positives**: findings that are clearly not issues
- **Blind spots**: real issues that no config caught

## Step 4: Log results

Append to `.claude/skills/code-review-eval/data/eval-runs.log`:

```
## {DATE} — {BRANCH}

Diff: {N} files, {+/-} lines

| Config | Findings | FPs | Verdict | Confidence | Time | Tokens |
|--------|----------|-----|---------|------------|------|--------|
| cursor-composer2 | N | N | ... | ... | ... | ... |
| codex-5.4 | N | N | ... | ... | ... | ... |
| codex-5.4-nano | N | N | ... | ... | ... | ... |

Consensus: ...
Unique to cursor: ...
Unique to codex-5.4: ...
Unique to codex-5.4-nano: ...
Blind spots: ...
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
| Cursor backend | `yoloswe/reviewer/backend_cursor.go` |
| Codex backend | `yoloswe/reviewer/backend_codex.go` |
| Run history | `.claude/skills/code-review-eval/data/eval-runs.log` |
