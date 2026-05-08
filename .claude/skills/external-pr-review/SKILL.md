---
name: external-pr-review
description: One-shot review of an external GitHub PR. Checks out the PR into an isolated worktree (or temp clone), runs `bramble code-review` against the diff, and produces an approval-biased verdict — APPROVE unless there is a blocking correctness issue, with optional improvements listed separately. Read-only on the remote PR, never pushes. User-invoked only via `/external-pr-review`.
argument-hint: "<pr-url-or-number> [--backend codex|cursor|gemini] [--model NAME] [--effort low|medium|high]"
disable-model-invocation: true
---

# External PR Review

Read-only review of someone else's PR. The user passes a GitHub PR (URL like `https://github.com/owner/repo/pull/3591`, or a bare `#3591` when they're inside the matching repo). The skill checks the code out in isolation, runs **one** `bramble code-review` pass against it, and prints an approval-biased verdict.

This skill is intentionally simple — single round, single backend by default, no GitHub side effects unless the user opts in at the end.

## What this skill is *not*

- Not `/pr-polish`. That skill iterates rounds of review-then-fix on your *own* branch and force-pushes at the end. This skill is read-only on a PR you may not own — it never edits files in the PR's branch, never pushes.
- Not a CI gate. It's a second-opinion read for a human reviewer.
- Not a security audit. For that, use `/cso`.

## Approval bias — the core rule

Default verdict is **APPROVE**. Only downgrade when there is a *blocking correctness issue*:

- Real bug in the change (wrong condition, off-by-one, missing await, type confusion that the runtime will hit).
- Security regression introduced by the diff (SQL injection, secret leak, auth bypass).
- Broken or missing tests for behavior the diff explicitly changes.
- Obvious perf/scaling regression (N+1 added on a hot path, sync I/O in an async hot loop).
- Behavior change to a documented contract without a corresponding doc/test update.

Everything else — naming, structure, "could be cleaner", style nits, optional refactors, pre-existing issues in touched files, "you could also handle X" — goes into a non-blocking **Optional improvements** section. These do not affect the verdict.

**Why this bias matters.** Reviewers who flag everything teach authors to ignore reviews. The signal-to-noise ratio is the product. Treat suggestions as gifts the author can accept or decline; treat blockers as rare and load-bearing.

A finding is blocking only when you can answer yes to: *if this merges as-is, will something observable break or regress?* Code-smell answers no. A wrong SQL predicate answers yes.

## Arguments

| Arg | Required | Meaning |
|---|---|---|
| `<pr>` | yes | Either a full GitHub PR URL (`https://github.com/owner/repo/pull/N`) or a bare PR number (`#N` or `N`) when invoked from inside a clone of the target repo. |
| `--backend` | no | `codex` (default), `cursor`, or `gemini`. Maps directly to `bramble code-review --backend`. |
| `--model` | no | Backend-specific model override. Passed straight through to bramble. |
| `--effort` | no | Codex-only. `low`, `medium` (default), or `high`. The default balances speed against catching cross-call-site coverage gaps; `low` is for quick smoke reads (often ~30–60s but may miss subtler findings); `high` for a deeper pass on subtle correctness issues. Ignored for non-codex backends. |

## Step 1: Resolve the PR

Parse the user's input into `OWNER`, `REPO`, `PR_NUM`. Use `gh pr view` to confirm the PR exists and to grab the head SHA, base ref, branch name, and title:

```bash
gh pr view "$PR_INPUT" --repo "$OWNER/$REPO" \
  --json number,title,body,headRefOid,headRefName,baseRefName,url,author,isDraft,state \
  > /tmp/external-pr-review-$PR_NUM/pr.json
```

Pull `body` too — the PR description is the author's stated intent and is the single biggest signal for keeping the reviewer focused on the diff's purpose instead of wandering into adjacent code. Step 3 puts it into `--goal` verbatim.

If the PR is closed or merged, tell the user and ask whether to proceed anyway (sometimes useful for retro reviews). If it's a draft, proceed but note it in the final verdict.

## Step 2: Check out the code

The goal is an isolated working tree at the PR's head SHA, in a path you can `cd` into for bramble. Try methods in order; fall through on failure.

### Method A — git worktree from a local clone (preferred when available)

If the user is currently inside a git repo whose `origin` matches the PR's repo, use a worktree. This is fastest (no re-clone) and reuses the local object database.

```bash
# Verify origin matches before committing to this method
ORIGIN_URL=$(git config --get remote.origin.url 2>/dev/null || true)
if [[ "$ORIGIN_URL" == *"$OWNER/$REPO"* ]]; then
  git fetch origin "pull/$PR_NUM/head:refs/remotes/origin/pr-$PR_NUM"
  WORKTREE_DIR="/tmp/external-pr-review-$PR_NUM-$(date +%s)"
  git worktree add --detach "$WORKTREE_DIR" "refs/remotes/origin/pr-$PR_NUM"
fi
```

Remember the worktree path so step 5 can clean it up.

### Method B — gh pr checkout in a fresh temp dir (fallback)

When the user isn't inside a matching clone, do a shallow clone of the base, then check out the PR head:

```bash
WORKTREE_DIR="/tmp/external-pr-review-$OWNER-$REPO-$PR_NUM-$(date +%s)"
gh repo clone "$OWNER/$REPO" "$WORKTREE_DIR" -- --depth 50
cd "$WORKTREE_DIR"
gh pr checkout "$PR_NUM"
```

`--depth 50` keeps the clone small while still giving bramble enough history to compute the diff against the base. If `bramble code-review` later complains about missing base, deepen with `git fetch --unshallow` and retry.

`gh pr checkout` can fail on a shallow clone with `cannot set up tracking information; starting point 'origin/<branch>' is not a branch` — the fetch already created the remote-tracking ref but the tracking-info step trips. Fall back to:

```bash
gh pr checkout "$PR_NUM" 2>/tmp/gh-checkout.err || \
  git checkout -b "pr-$PR_NUM" "origin/$(jq -r .headRefName /tmp/external-pr-review-$PR_NUM/pr.json)"
```

Then verify `git rev-parse HEAD` matches the head SHA from step 1 before proceeding.

### Method C — last resort

If both A and B fail (rare — usually a permissions issue), report the failure to the user with the underlying error and stop. Do not try to review without a real checkout — bramble needs a worktree.

## Step 3: Build the review goal

Bramble's `--goal` is the per-turn briefing the model sees. **This is the highest-leverage knob in this skill** — a vague goal lets the model wander into infra/Pulumi/lockfile/cross-repo audits that aren't this PR's job, doubling or tripling review time on big diffs.

Build the goal in this exact shape and write it to `$WORKTREE_DIR/.external-review-goal.txt`:

```
Review of GitHub PR #<N>: "<title>"
Repo: <owner>/<repo>
Base: <baseRef>  Head: <headRefShort>
Author: <login>

## What the author says this PR does
<verbatim PR description body from `gh pr view --json body`, capped at ~50 lines —
 if longer, take the first ~40 lines + a "(truncated, full body in PR)" note>

## Diff stat
<git diff --stat origin/<base>..HEAD, capped at 10 lines, plus a tail line
 like "and N more files" when truncated>

## Review brief
You are reviewing this diff for a maintainer-adjacent reviewer who will make a
ship/no-ship decision. Bias toward APPROVE.

- Only flag a finding as blocking if merging this diff as-is would cause
  something OBSERVABLE to break or regress: a real bug, a security regression,
  broken or missing tests for behavior the diff explicitly changes, an obvious
  perf/scaling regression, or a change to a documented contract without a
  matching doc/test update.
- Style nits, "could be cleaner", optional refactors, and pre-existing issues
  in touched files are OPTIONAL — surface them but do NOT downgrade the verdict.
- Stay inside the diff. Do not audit Pulumi/infra, lockfiles, generated code,
  or cross-repo prompts unless this PR explicitly changes them. If you find
  yourself reading a file that is not in `git diff --name-only`, stop and
  return to the diff.
- One pass is enough. Don't re-verify the same plumbing across multiple turns.
```

The PR description carries the *why* the reviewer model would otherwise hunt for in repo-wide reads. The "Stay inside the diff" rule is the single biggest speedup on multi-thousand-line PRs — without it, codex/cursor will happily spend 5–8 minutes auditing files that aren't in the change.

Without a real PR description (some PRs ship with empty or one-line bodies), substitute a one-liner derived from the title + first commit subject — anything is better than letting the model invent intent from the branch name.

## Step 4: Run bramble code-review

Pick the bramble binary the same way `/pr-polish` does — prefer the freshly-built worktree artifact when present, else fall back to PATH:

```bash
export BRAMBLE_BIN="$([ -x "$(pwd)/bazel-bin/bramble/bramble_/bramble" ] \
    && echo "$(pwd)/bazel-bin/bramble/bramble_/bramble" \
    || echo bramble)"
```

Then run a single review pass. Default backend is `codex` with `gpt-5.4-mini` at `--effort medium`; the user can override any of these. `--skip-test-execution` keeps it read-only — you're not on the hook for running the project's test suite.

**Why `--effort medium`.** This skill produces an approval-biased read for a human who will make a ship/no-ship decision. The goal isn't just "is the code wrong on the cited line" — it's also "did the author miss a sibling call site of this change". Empirically, `--effort low` returned in ~45s on a 28-file/1442-line PR but found zero findings, missing two real cross-call-site coverage gaps that `medium` did catch. `medium` is the sweet spot: deep enough to surface cross-file gaps, fast enough that a one-shot review feels snappy. Users who want a quick smoke read can pass `--effort low`; users who want maximum depth on a subtle PR can pass `--effort high`.

```bash
ENVELOPE="$WORKTREE_DIR/.external-review-envelope.json"
LOG_DIR="$WORKTREE_DIR/.external-review-logs"
mkdir -p "$LOG_DIR"

BACKEND="${BACKEND:-codex}"
MODEL_FLAG=""
[ -n "$MODEL" ] && MODEL_FLAG="--model $MODEL"

# --effort applies only to codex. Default to "medium" — see "Why --effort medium" above.
EFFORT_FLAG=""
if [ "$BACKEND" = "codex" ]; then
  EFFORT_FLAG="--effort ${EFFORT:-medium}"
fi

cd "$WORKTREE_DIR" && \
  BRAMBLE_RUN_TAG="external-pr-review:$OWNER/$REPO:$PR_NUM:$BACKEND" \
  "$BRAMBLE_BIN" code-review \
    --backend "$BACKEND" $MODEL_FLAG $EFFORT_FLAG \
    --skip-test-execution --verbose --timeout 10m \
    --goal "$(cat "$WORKTREE_DIR/.external-review-goal.txt")" \
    --envelope-file "$ENVELOPE" \
    2> "$LOG_DIR/stderr.txt"
```

Use `Monitor` so you can keep doing other work while it runs (typical run is 2–6 minutes):

```
Monitor({
  description: "external PR review (bramble code-review)",
  timeout_ms: 720000,
  persistent: false,
  command: "<the cd && bramble invocation above>"
})
```

**Do not also schedule a wakeup or sleep loop while waiting.** Monitor's completion notification is the only signal you need. This skill is a one-shot: re-firing `/external-pr-review` while a run is still in flight (or after it finished) produces a duplicate review, and `ScheduleWakeup` paired with a slash-command prompt does exactly that on every wake. If the user goes idle, the right answer is to wait silently — the harness will notify you when Monitor's stream ends.

If the envelope reports `status: "error"` but `review.raw_text` contains a fenced ```json``` block, recover by extracting the inner JSON the way pr-polish does. Otherwise, if the run failed outright, report the stderr path and stop — don't fabricate findings.

## Step 5: Triage with approval bias

Read the envelope. It contains a list of findings with `severity` (`high`, `medium`, `low`, `nit`), a `path`/`line`, and a `topic`/`description`.

Apply the **blocking-issue test** (see "Approval bias" above) to each finding. For each one, decide:

- **Blocking** — passes the blocking-issue test. Goes into the `## Blocking issues` section. Verdict downgrades to `REQUEST CHANGES`.
- **Optional** — everything else. Goes into `## Optional improvements`. Verdict unaffected.

Be willing to disagree with the reviewer's severity. A `high` from the model that's actually a style preference is optional. A `low` that's a real bug is blocking. The model's severity is input, not gospel — your job is the approval bias.

Cross-reference the cited file:line before classifying. If the finding cites code that isn't in the diff, demote to optional (it's pre-existing) or drop entirely if it's clearly stale.

## Step 6: Print the verdict

Format the chat output as Markdown:

```
# Review of <owner>/<repo>#<N> — "<title>"

**Verdict:** ✅ APPROVE   (or  ⚠️ APPROVE with suggestions  /  🛑 REQUEST CHANGES)

**Summary:** <one sentence on what the PR does and why it looks fine / what blocks it>

## Blocking issues
<omit this section entirely when there are none — that's the whole point of approval bias>

- **`path/to/file.go:42`** — <one-line description>
  <2–3 sentences on why this is blocking and what to change>

## Optional improvements
<also omit if empty>

- **`path/to/other.py:88`** — <one-line description>
  <1–2 sentences; phrase as a suggestion, not a demand>

## What I looked at
- <terse list: which files were the focus, what concerns drove the read>

---
Reviewed with bramble (`<backend>`/`<model>`) at SHA `<short-sha>`. Worktree: `<path>`.
```

Verdict mapping:
- No blocking, no substantive optional → **APPROVE**.
- No blocking, ≥1 *substantive* optional finding → **APPROVE with suggestions**.
- ≥1 blocking → **REQUEST CHANGES**.

A "substantive" optional is one the author would actually want to act on — a real (non-blocking) bug, a missed migration call site, a behavior the author probably didn't intend. Pure stylistic nits, naming preferences, and "could be cleaner" suggestions don't earn the with-suggestions tag; if the verdict would otherwise be a clean APPROVE and the only optional findings are nits, list them under a short "Nits (take or leave)" footer and keep the headline as plain APPROVE. Approval-bias means low-value findings shouldn't visually downgrade the verdict.

## Step 7: Ask before posting to GitHub

Before printing the post-or-skip prompt, check whether the current `gh` user has already submitted a review on this PR at the *current* head SHA:

```bash
gh pr view "$PR_NUM" --repo "$OWNER/$REPO" --json reviews,headRefOid \
  --jq '.headRefOid as $sha | .reviews[] | select(.commit.oid == $sha) | {author: .author.login, state, submittedAt}'
```

If a review by the current user already exists at this SHA (likely cause: this skill was re-fired by mistake), surface that to the user as part of the question — options become *Update existing review*, *Post anyway as a new review*, or *Skip*. Default to **Skip** in that case to avoid silently double-posting.

When there's no prior review at the current SHA, ask the user (via `AskUserQuestion`) whether to post it as a PR review. Three options:

- **Approve on GitHub** — only offered when verdict is APPROVE or APPROVE-with-suggestions; runs `gh pr review --approve --body-file <file>`.
- **Comment only** — runs `gh pr review --comment --body-file <file>`.
- **Skip** — leave the review in chat only.

For `REQUEST CHANGES`, also offer `--request-changes`. Default-highlight `Skip` so the user has to opt in to anything that touches GitHub — they may want to edit the body first. Always pass the body via `--body-file` (write to a temp file first), not `--body "..."`, to avoid shell-quoting hazards on long markdown.

## Step 8: Cleanup

After posting (or skipping), tear down the temp checkout:

- Worktree path → `git worktree remove --force <path>` then `git update-ref -d refs/remotes/origin/pr-<N>` to clean the fetched ref.
- Temp clone path → `rm -rf <path>`.

Keep `$ENVELOPE` and `$LOG_DIR` if cleanup fails, and tell the user where they are — bramble logs are useful for debugging.

## Edge cases worth handling well

- **Huge PRs (>500 lines diff or >20 files)**: bramble's review will be partial. Note this in the summary ("focused review on N of M files; high-confidence on the read areas, less coverage on the rest"). Don't pretend full coverage.
- **Generated code in the diff**: skip findings on lockfiles, `pb.go`, snapshot tests, etc. Note in "What I looked at" that they were ignored.
- **PR is a revert**: confirm the revert target is what the description says. Approve unless the revert itself introduces a new bug.
- **Documentation-only PR**: read for accuracy and broken links; default to APPROVE. Don't gate on prose style.
- **PR depends on another unmerged PR**: note it; verdict is on this diff in isolation.

## Why one round, one backend by default

`/pr-polish` runs multi-round, multi-backend because the *author* will fix what's found and re-review. For an external read, the reviewer (you) is making a single decision: ship or don't. A second backend mostly adds time and a longer optional list — neither moves the approval bar in this skill's design. The user can pass `--backend cursor` for a different perspective when they want one, but there's no auto-multi.
