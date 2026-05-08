---
name: external-pr-review
description: One-shot review of an external GitHub PR. Checks out the PR into an isolated worktree (or temp clone), runs `bramble code-review` against the diff, and produces an approval-biased verdict — APPROVE unless there is a blocking correctness issue, with optional improvements listed separately. Read-only on the remote PR, never pushes. User-invoked only via `/external-pr-review`.
argument-hint: "<pr-url-or-number> [--backend codex|cursor|gemini] [--model NAME] [--effort low|medium|high]"
disable-model-invocation: true
---

# External PR Review

Read-only review of someone else's PR. One `bramble code-review` pass against the diff, approval-biased verdict, optionally posted to GitHub.

Companion to `/pr-polish` (which iterates fixes on your *own* branch). This one never edits the PR's files, never pushes.

## Approval bias

Default verdict is **APPROVE**. Downgrade only on a blocking correctness issue:

- Real bug introduced by the diff (wrong condition, off-by-one, missing await).
- Security regression introduced by the diff.
- Missing/broken tests for behavior the diff explicitly changes.
- Obvious perf regression (N+1 on a hot path, sync I/O in async hot loop).
- Change to a documented contract without a matching doc/test update.

Everything else — naming, structure, style, optional refactors, pre-existing issues — is **Optional**.

The blocking test: *if this merges as-is, will something observable break or regress?* Code-smell answers no. Wrong SQL predicate answers yes.

## Arguments

- `<pr>` (required): full URL `https://github.com/owner/repo/pull/N`, or bare `#N` / `N` when invoked from inside a clone of that repo.
- `--backend`: `codex` (default), `cursor`, or `gemini`.
- `--model`: backend-specific override.
- `--effort`: codex-only. `low` / `medium` (default) / `high`. `medium` catches cross-call-site coverage gaps that `low` misses; `high` for genuinely subtle correctness.

## Step 1: Resolve the PR

```bash
mkdir -p /tmp/external-pr-review-$PR_NUM
gh pr view "$PR_INPUT" --repo "$OWNER/$REPO" \
  --json number,title,body,headRefOid,headRefName,baseRefName,url,author,isDraft,state \
  > /tmp/external-pr-review-$PR_NUM/pr.json
```

Closed/merged → ask before continuing. Draft → proceed, note in verdict.

## Step 2: Check out the code

Need an isolated worktree at the head SHA. Try in order.

**Method A — worktree from local clone** (when `git config remote.origin.url` matches `$OWNER/$REPO`):

```bash
git fetch origin "pull/$PR_NUM/head:refs/remotes/origin/pr-$PR_NUM"
WORKTREE_DIR="/tmp/external-pr-review-$PR_NUM-$(date +%s)"
git worktree add --detach "$WORKTREE_DIR" "refs/remotes/origin/pr-$PR_NUM"
```

**Method B — fresh shallow clone**:

```bash
WORKTREE_DIR="/tmp/external-pr-review-$OWNER-$REPO-$PR_NUM-$(date +%s)"
gh repo clone "$OWNER/$REPO" "$WORKTREE_DIR" -- --depth 50
cd "$WORKTREE_DIR"
gh pr checkout "$PR_NUM" || \
  git checkout -b "pr-$PR_NUM" "origin/$(jq -r .headRefName /tmp/external-pr-review-$PR_NUM/pr.json)"
```

`gh pr checkout` fails on shallow clones with `cannot set up tracking information` — the fallback works because the fetch already created the remote-tracking ref. If bramble later complains about missing base, `git fetch --unshallow`.

Verify `git rev-parse HEAD` matches the head SHA from step 1 before continuing.

If both methods fail, report the error and stop.

## Step 3: Build the review goal

`--goal` is the highest-leverage knob. Without a tight goal, codex/cursor wander into infra/lockfile/cross-repo audits and burn 5–8 minutes on a small PR.

Write this to `$WORKTREE_DIR/.external-review-goal.txt`:

```
Review of GitHub PR #<N>: "<title>"
Repo: <owner>/<repo>
Base: <baseRef>  Head: <headRefShort>
Author: <login>

## What the author says this PR does
<verbatim PR body, capped at ~50 lines; truncate with "(truncated)" note>

## Diff stat
<git diff --stat origin/<base>..HEAD, capped at 10 lines>

## Review brief
Bias toward APPROVE. Flag a finding as blocking only if merging this diff
as-is would cause something OBSERVABLE to break or regress: real bug,
security regression, missing/broken test for changed behavior, obvious
perf regression, or a change to a documented contract without matching
doc/test update.

Style nits and pre-existing issues are OPTIONAL — do NOT downgrade verdict.

Stay inside the diff. Do not audit Pulumi/infra, lockfiles, generated
code, or cross-repo prompts unless this PR explicitly changes them. If
you find yourself reading a file not in `git diff --name-only`, stop.

One pass. Don't re-verify the same plumbing across multiple turns.
```

If the PR body is empty or one-liner, substitute title + first commit subject. Don't let the model invent intent from the branch name.

## Step 4: Run bramble code-review

```bash
export BRAMBLE_BIN="$([ -x "$(pwd)/bazel-bin/bramble/bramble_/bramble" ] \
    && echo "$(pwd)/bazel-bin/bramble/bramble_/bramble" \
    || echo bramble)"

ENVELOPE="$WORKTREE_DIR/.external-review-envelope.json"
LOG_DIR="$WORKTREE_DIR/.external-review-logs"
mkdir -p "$LOG_DIR"

BACKEND="${BACKEND:-codex}"
MODEL_FLAG=""; [ -n "$MODEL" ] && MODEL_FLAG="--model $MODEL"
EFFORT_FLAG=""
[ "$BACKEND" = "codex" ] && EFFORT_FLAG="--effort ${EFFORT:-medium}"

cd "$WORKTREE_DIR" && \
  BRAMBLE_RUN_TAG="external-pr-review:$OWNER/$REPO:$PR_NUM:$BACKEND" \
  "$BRAMBLE_BIN" code-review \
    --backend "$BACKEND" $MODEL_FLAG $EFFORT_FLAG \
    --skip-test-execution --verbose --timeout 10m \
    --goal "$(cat "$WORKTREE_DIR/.external-review-goal.txt")" \
    --envelope-file "$ENVELOPE" \
    2> "$LOG_DIR/stderr.txt"
```

Run it under `Monitor` (typical 2–6 min):

```
Monitor({
  description: "external PR review (bramble code-review)",
  timeout_ms: 720000,
  persistent: false,
  command: "<the bramble invocation above>"
})
```

**Do not also call `ScheduleWakeup` or any sleep loop while waiting.** Monitor's completion event is the only signal needed. `ScheduleWakeup` with a slash-command prompt will re-fire the whole skill and double-post the review.

If the envelope reports `status: "error"` but `review.raw_text` contains a fenced ```json``` block, recover by extracting the inner JSON. Otherwise report stderr path and stop.

## Step 5: Triage

Each envelope finding has a `severity` (`high`/`medium`/`low`/`nit`), `path`/`line`, and `description`. For each:

- Apply the blocking test from "Approval bias" above.
- Cross-reference the cited file:line. If it isn't in the diff, demote to optional (pre-existing) or drop (stale).
- Disagree with the model's severity when warranted — a `high` style nit is optional; a `low` real bug is blocking.

## Step 6: Print the verdict

```
# Review of <owner>/<repo>#<N> — "<title>"

**Verdict:** ✅ APPROVE   (or  ⚠️ APPROVE with suggestions  /  🛑 REQUEST CHANGES)

**Summary:** <one sentence>

## Blocking issues
<omit when none>

- **`path:line`** — <one-liner>
  <2–3 sentences: why blocking, what to change>

## Optional improvements
<omit when none>

- **`path:line`** — <one-liner>
  <1–2 sentences, phrased as a suggestion>

## What I looked at
- <terse list>

---
Reviewed with bramble (`<backend>`/`<model>`) at SHA `<short-sha>`. Worktree: `<path>`.
```

Verdict:
- ≥1 blocking → **REQUEST CHANGES**.
- ≥1 substantive optional (real non-blocking bug, missed migration site, likely-unintended behavior) → **APPROVE with suggestions**.
- Otherwise → **APPROVE**. If only pure nits remain, list them under a "Nits (take or leave)" footer and keep the headline plain APPROVE.

## Step 7: Ask before posting

First, idempotency check — has the current user already reviewed this SHA?

```bash
gh pr view "$PR_NUM" --repo "$OWNER/$REPO" --json reviews,headRefOid \
  --jq '.headRefOid as $sha | .reviews[] | select(.commit.oid == $sha) | {author: .author.login, state}'
```

If yes (likely a re-fire), offer *Update existing*, *Post anyway*, *Skip* — default Skip.

Otherwise ask via `AskUserQuestion`:

- **Approve on GitHub** (only when verdict is APPROVE or APPROVE-with-suggestions): `gh pr review --approve --body-file <file>`.
- **Comment only**: `gh pr review --comment --body-file <file>`.
- **Request changes** (only when verdict is REQUEST CHANGES): `gh pr review --request-changes --body-file <file>`.
- **Skip** (default).

Always `--body-file`, never `--body "..."` — long markdown breaks shell quoting.

## Step 8: Cleanup

- Worktree → `git worktree remove --force <path>` then `git update-ref -d refs/remotes/origin/pr-<N>`.
- Temp clone → `rm -rf <path>`.

Keep `$ENVELOPE` and `$LOG_DIR` on cleanup failure and tell the user where they are.

## Edge cases

- **>500 lines or >20 files**: review will be partial; say so in the summary.
- **Generated code** (lockfiles, `*.pb.go`, snapshots): skip findings on these; note in "What I looked at".
- **Revert PR**: confirm revert target matches the description; approve unless the revert itself introduces a bug.
- **Docs-only PR**: read for accuracy and broken links; default APPROVE.
- **Stacked PR**: verdict is on this diff in isolation; note the dependency.
