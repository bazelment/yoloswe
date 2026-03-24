---
name: code-review-eval
description: "Evaluate code review quality across different reviewer backends (cursor, codex) and configurations. Runs review on a known PR/branch, scores findings against a ground-truth checklist, compares backends, and tracks quality over time."
argument-hint: "<cursor|codex|all> [branch or PR]"
---

# Review Eval Loop

Evaluate code review quality by running different reviewer backends against the same
code changes and scoring their findings against a ground-truth checklist. **Max 3 rounds**
per backend. Exit early when scoring is stable.

## Purpose

Code review bots (Cursor Bugbot, Codex reviewer, etc.) vary in what they catch.
This skill provides a structured way to:

1. Benchmark reviewer quality on real PRs
2. Compare backends head-to-head on the same diff
3. Track reviewer accuracy over time (precision/recall against known issues)
4. Identify blind spots that need human review focus

## Step 0: Parse Arguments and Identify Target

Parse `` for the review backend(s) and an optional branch/PR reference.

- Backends: `cursor`, `codex`, or `all` (runs both sequentially)
- Target: PR number, branch name, or omit to use current branch

If a PR number is given, fetch its details:
```bash
gh pr view <PR> --json number,title,baseRefName,headRefName,url \
  --jq '{number: .number, title: .title, base: .baseRefName, head: .headRefName, url: .url}'
```

If no PR, use the current branch vs its base:
```bash
BASE=$(git rev-parse --abbrev-ref @{upstream} 2>/dev/null | sed 's|origin/||' || echo main)
```

Extract repo info:
```bash
gh repo view --json owner,name --jq '"\(.owner.login)/\(.name)"'
```

## Step 1: Build Ground Truth

Before running any reviewer, build the ground-truth checklist of known issues in the diff.
This is the scoring key — reviewers are graded against it.

### 1a: Understand the diff

```bash
git diff origin/{BASE}..HEAD --stat
git diff origin/{BASE}..HEAD
```

Read all changed files in full. Understand the intent of each change.

### 1b: Manual audit

Perform your own thorough review of the diff. For each issue found, record:

```
## Issue: <short title>
- **File**: path:line_range
- **Severity**: high | medium | low | nit
- **Category**: correctness | security | performance | maintainability | style | test-coverage
- **Description**: What's wrong and why it matters
- **Expected reviewer behavior**: Should the reviewer flag this? What should it say?
```

Also note areas where the code is **correct** — these are true negatives that a good
reviewer should NOT flag. Record 2-3 explicit "non-issues" to catch false positive patterns.

### 1c: Write the ground truth file

Write the audit to `.claude/skills/code-review-eval/data/ground-truth-{BRANCH}.md` with:
- Issue list (the scoring key)
- Non-issue list (false positive traps)
- Diff summary (for context)

Check `.claude/skills/code-review-eval/references/scoring-rubric.md` for category weights.

## Step 2: Run Reviewer(s)

For each backend to evaluate:

### 2a: Build the review prompt

Write a prompt to `/tmp/review-eval-prompt.txt` that includes:
- The PR summary (goal, key changes, files)
- Instructions to review for correctness, security, performance, maintainability,
  test coverage, and style
- Request to cite file:line for each finding with severity and confidence

### 2b: Execute the reviewer

**Cursor:**
```bash
cursor-agent --trust -p "$(cat /tmp/review-eval-prompt.txt)" 2>/tmp/review-eval-cursor-stderr.txt | tee /tmp/review-eval-cursor-output.txt
```

**Codex:**
```bash
codex exec "$(cat /tmp/review-eval-prompt.txt)" 2>/tmp/review-eval-codex-stderr.txt | tee /tmp/review-eval-codex-output.txt
```

Run with `timeout=600000` (10 min max per reviewer).

### 2c: Parse findings

Read the reviewer output. Extract each finding into a structured list:
- Title
- File and line range
- Severity (as assessed by the reviewer)
- Category
- Description
- Suggested fix (if any)

## Step 3: Score Against Ground Truth

For each reviewer's findings, compare against the ground truth:

### 3a: Classify each finding

| Classification | Meaning |
|---------------|---------|
| **True Positive (TP)** | Reviewer found a real issue from the ground truth |
| **False Positive (FP)** | Reviewer flagged something that isn't actually an issue |
| **False Negative (FN)** | Ground truth issue that the reviewer missed |
| **True Negative (TN)** | Non-issue correctly not flagged |
| **Partial Match (PM)** | Reviewer found the right area but wrong diagnosis |

### 3b: Compute metrics

```
Precision = TP / (TP + FP)          — "when it flags something, is it right?"
Recall    = TP / (TP + FN)          — "does it find the real issues?"
F1        = 2 * P * R / (P + R)     — harmonic mean
```

Weight by severity: high issues count 3x, medium 2x, low 1x, nits 0.5x.

### 3c: Qualitative scoring

For each TP, also score:
- **Location accuracy** (0-2): 0=wrong file, 1=right file wrong line, 2=exact
- **Diagnosis quality** (0-2): 0=wrong explanation, 1=vague, 2=precise root cause
- **Fix quality** (0-2): 0=no fix or wrong fix, 1=partial, 2=correct fix suggested

## Step 4: Compare Backends (if `all`)

If multiple backends were run, produce a head-to-head comparison:

| Metric | Cursor | Codex |
|--------|--------|-------|
| Precision | | |
| Recall | | |
| F1 (weighted) | | |
| True Positives | | |
| False Positives | | |
| Missed (FN) | | |
| Avg location accuracy | | |
| Avg diagnosis quality | | |
| Avg fix quality | | |
| Wall clock time | | |

Identify:
- Issues only Cursor caught
- Issues only Codex caught
- Issues neither caught (blind spots)
- False positives unique to each

## Step 5: Update Run History

Append results to `.claude/skills/code-review-eval/data/eval-runs.log`:

```
## Review Eval — {DATE}

### Target: {BRANCH} ({PR_TITLE})
Diff: {N} files changed, {additions}+/{deletions}-
Ground truth: {N} issues ({high}H/{medium}M/{low}L/{nit}N), {N} non-issues

### Results

| Backend | P | R | F1w | TP | FP | FN | Time |
|---------|---|---|-----|----|----|----|----- |
| cursor  |   |   |     |    |    |    |      |
| codex   |   |   |     |    |    |    |      |

### Blind spots (missed by all backends)
- ...

### False positive patterns
- ...
```

Check `.claude/skills/code-review-eval/references/known-blind-spots.md` for patterns
from prior runs and note any regressions.

## Final Summary

Always produce a summary before ending:

```
## Review Eval Summary

| Metric              | Value          |
|---------------------|----------------|
| Target              | branch/PR      |
| Ground truth issues | N              |
| Backends tested     | cursor, codex  |
| Best F1 (weighted)  | backend: score |
| Blind spots         | N issues       |
| Verdict             | ...            |

### Recommendation
Which backend to use for this type of code, and what human reviewers
should focus on (the blind spots).
```

## Gotchas

1. **Ground truth must be built BEFORE running reviewers.** If you build it after,
   you'll unconsciously anchor to what the reviewer found. Audit the diff yourself first.

2. **Reviewer prompts matter enormously.** Small changes in how you ask for the review
   can swing recall by 30%+. Keep the prompt identical across backends for fair comparison.

3. **False positives are as important as true positives.** A reviewer that flags everything
   has perfect recall but useless precision. Track both.

4. **Severity calibration differs across backends.** Cursor might call something "high"
   that Codex calls "medium." Normalize to the ground truth severity for scoring.

5. **Run time varies wildly.** Cursor Bugbot can take 10+ minutes. Codex is usually faster.
   Use background execution and don't compare wall clock as a primary quality metric.

6. **The same backend can give different results on the same diff.** LLMs are stochastic.
   For high-confidence comparisons, run each backend 2-3 times and average. Use the
   `--rounds` argument if you want repeated runs.

7. **Pre-existing issues pollute scoring.** If the diff touches a file with pre-existing
   bugs, a reviewer might flag those. Classify these as "out of scope" rather than FP —
   they're real issues, just not introduced by this diff.

## Key Files

| Area | Files |
|------|-------|
| Scoring rubric | `.claude/skills/code-review-eval/references/scoring-rubric.md` |
| Known blind spots | `.claude/skills/code-review-eval/references/known-blind-spots.md` |
| Run history | `.claude/skills/code-review-eval/data/eval-runs.log` |
| Ground truth (per branch) | `.claude/skills/code-review-eval/data/ground-truth-{branch}.md` |
