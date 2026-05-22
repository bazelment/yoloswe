---
name: code-review-replay
description: "Build and use a ground-truth eval dataset for bramble code-review. Two modes. COLLECTION scans past /pr-polish'd PRs and judges each into a frozen ground truth — multiple rounds of bramble re-review + an independent judge sub-agent per round, until the judge's full-diff bug census saturates. REPLAY runs a reviewer-under-test and scores it mechanically against that frozen ground truth — precision/recall/F1, no sub-agents, cheap and repeatable."
argument-hint: "collect [<repo>-<pr> ...] | [<repo>-<pr>] [--sample N] [--config NAME ...]"
---

# Code Review Replay

`/code-review-replay` builds a **ground-truth eval dataset** for bramble
code-review and scores reviewers against it. It has two modes.

| Mode | What it does | Cost |
|------|--------------|------|
| **collection** (`collect`) | Scan past PRs, judge each into a frozen ground truth | Expensive — bramble + judge sub-agents over several rounds per PR. |
| **replay** (default) | Run a reviewer-under-test, score it against the frozen ground truth | Cheap — one bramble run per config, mechanical scoring, no sub-agents. |

The companion `/code-review-eval` skill compares backends side-by-side on a
**live branch**. This skill instead asks, against **past PRs**: *would this
reviewer catch the real issues and stay quiet on the noise?*

## Why two modes

- **Collection does the judging once.** It runs bramble over a PR's diff
  several times across backends, and an independent judge sub-agent per
  round verdicts every finding *and* censuses the real bugs in the diff.
  When the census **saturates** — no new real bug, every censused bug
  covered by a reviewer finding, no contested verdict left unresolved — the
  judged true/false-positive set is frozen as `ground_truth_v3`.
- **Replay just scores.** With the ground truth frozen, evaluating a
  reviewer config is one mechanical pass: run it, match findings to the
  frozen set, compute precision/recall/F1. No sub-agents.

The harvested `comment_actions` (the original engineer's triage) are kept
only as an auditable **cross-check** — never the label. The judge's verdict,
grounded in the actual diff, is the ground truth.

## Mode dispatch

- First argument is `collect` → **collection mode**.
- Anything else (a `<repo>-<pr>` id, or no argument) → **replay mode**.

Repo checkouts are auto-discovered (`~/worktrees/<name>/main`, `~/g/<name>`)
— you never pass repo-root paths.

---

# Collection mode

`/code-review-replay collect [<repo>-<pr> ...]` — build frozen ground truth.

With no PR ids, collection **scans for all uncollected PRs** and processes
them. Pass specific `<repo>-<pr>` ids to target those, or rely on
`collect.py --sample N` semantics by collecting a capped random subset.
`collect.py` is stateless glue — the steps below are the per-PR loop the
**skill** drives.

## Step 0 — build bramble, see what is collectable

```bash
bazel build //bramble:bramble
python3 .claude/skills/code-review-replay/scripts/collect.py scan
```

`scan` prints the collectable PRs (those with `~/.bramble/projects/` history)
and which are already collected. Collect every uncollected one, or a chosen
subset. Build produces `bazel-bin/bramble/bramble_/bramble` — note the path.

## Step 1 — set up the PR

```bash
python3 .claude/skills/code-review-replay/scripts/collect.py setup <repo>-<pr>
```

`setup` harvests the PR if it has no dataset yet, auto-discovers its repo
checkout, and creates a session with **one** detached git worktree pinned at
the canonical round's `head_before`. It prints
`{session, worktree, canonical_round}` — record `session` (every later call
needs it) and `worktree` (the cwd for bramble re-runs and the judge's
`repo_path`). `canonical_round` carries `head_before`, `merge_base_sha`,
`goal_text`, and `files_changed` for the round loop. `setup` aborts if the
canonical round has no resolved merge base — collection cannot judge a diff
it cannot scope; re-harvest with the repo checked out.

## Step 2 — the ground-truth round loop

Collection establishes ground truth for **one diff** — the PR's original
fresh-eyes diff. Repeat the loop, `round` starting at 1, until `fold`
reports `census_converged: true` (or the round budget, default 10, is spent).

### 2a — re-review the diff

Every round re-runs `bramble code-review` once per backend in the session
`worktree` (already checked out at `head_before`). Arm one **Monitor** per
backend in the same turn so they run in parallel:

```
Monitor({
  description: "bramble codex r<round>",
  command: "cd <WORKTREE> && \
    BRAMBLE_RUN_TAG=code-review-replay:<repo>-<pr>:collect:r<round>:codex \
    WORK_DIR=$(pwd) <BRAMBLE_BIN> code-review \
    --backend codex --model gpt-5.4-mini --effort medium \
    --skip-test-execution --verbose --timeout 13m \
    --goal \"$GOAL\" \
    --envelope-file /tmp/crr-r<round>/codex-envelope.json \
    2>/tmp/crr-r<round>/codex-stderr.txt"
})
```

`GOAL` is `canonical_round.goal_text` from `setup`. Mirror the Monitor for
`cursor` (`--backend cursor --model composer-2`) and optionally `gemini`
(`--backend gemini --model gemini-3.1-flash-lite-preview`). `bramble
code-review` is read-only, so all backends share the one worktree safely.

### 2b — build the judge prompt

```bash
python3 .claude/skills/code-review-replay/scripts/collect.py build-prompt \
  --session <SESSION> --round <round> --include-harvested \
  --envelope codex=/tmp/crr-r<round>/codex-envelope.json \
  --envelope cursor=/tmp/crr-r<round>/cursor-envelope.json
```

Pass each backend's envelope from this round. `--include-harvested` folds
the original pr-polish review in as an extra data point — pass it every
round; it is not a special case. The call prints the `judge_prompt` path.
The prompt carries the *cumulative* union of reviewer findings (this round +
all prior), the running census, and any contested findings.

### 2c — spawn the judge sub-agent

Spawn one `general-purpose` sub-agent. Use this prompt, substituting the
judge-prompt path:

> You are an independent code-review judge building a **ground-truth
> dataset**. A reviewer was run against a real diff; your job is to decide,
> by inspecting the actual code, which of its findings are real — that
> verdict becomes the dataset other reviewers are scored against.
>
> The prompt-input JSON at `<JUDGE_PROMPT_PATH>` carries: `repo_path` (a git
> worktree checked out at the diff's `head_before` — its working tree is
> exactly the post-diff code, so read files there directly), `diff_ref`
> (the diff scope, with a ready `diff_command`), `reviewer_findings` (the
> cumulative union of every finding bramble surfaced — verdict each one),
> `cumulative_census` (real bugs censused in prior rounds — extend it),
> `contested_findings` (defects prior rounds judged inconsistently — give
> each a final verdict), and `harvested_comment_actions` (the original
> triage — reference only, may be wrong).
>
> Produce three things:
>
> 1. **A verdict on every reviewer finding** — `true_positive`,
>    `false_positive`, or `unsure`, with a one-line `reason` grounded in the
>    code. **You assign the `severity`** (`high`/`medium`/`low`/`nit`) on the
>    defect's real impact — the reviewer's reported severity is only an
>    input; copy it into `reviewer_severity`. Verify a finding's *premise*,
>    not just its topic — two false-positive shapes to actively check:
>    *"missing test"* claims (read the whole test file; reviewers miss a
>    sibling test that already covers it) and *"unreachable"/"wrong value"*
>    claims (trace the data flow; confirm the bad value is reachable).
> 2. **An independent census of the real bugs in the diff** — every real
>    defect, including ones no reviewer caught. Extend `cumulative_census`.
>    You are the authority on defect identity: if the census splits one
>    defect across two locations, declare them merged in `census_merges`.
> 3. **A final verdict on each `contested_findings` entry** — add it to
>    `finding_verdicts`; your verdict is binding and resolves the conflict.
>
> Write your verdict as JSON to the `verdict_output_path` named in the
> prompt-input, using exactly this schema:
>
> ```json
> {
>   "round": 2,
>   "finding_verdicts": [
>     {"file": "scripts/deploy.py", "line": 679,
>      "severity": "high", "reviewer_severity": "medium",
>      "topic": "off-by-one in rollout index",
>      "verdict": "true_positive", "reason": "confirmed at deploy.py:679",
>      "surfaced_by": ["codex", "cursor"]}
>   ],
>   "census": [
>     {"file": "scripts/deploy.py", "line": 679, "severity": "high",
>      "description": "off-by-one in rollout index"}
>   ],
>   "census_merges": [
>     {"members": [
>        {"file": "tests/test_x.py", "line": 8},
>        {"file": "tests/test_x.py", "line": 46}
>      ],
>      "reason": "both are the same missing-coverage defect"}
>   ]
> }
> ```
>
> `verdict` ∈ `true_positive`/`false_positive`/`unsure`; `severity` ∈
> `high`/`medium`/`low`/`nit` on every non-`unsure` verdict. Every
> non-`unsure` `finding_verdicts` entry, every `census` entry, and every
> `census_merges` member must carry `file` (and `line`, which may be `null`
> for a file-level finding) — an entry with no location is rejected, since
> it would freeze into the ground truth keyed on an empty location.
> `census_merges` is optional. Judge on the code, not the labels.

### 2d — fold the verdict and test convergence

```bash
python3 .claude/skills/code-review-replay/scripts/collect.py fold \
  --session <SESSION> --round <round>
```

(`--round-budget` defaults to 10.) `fold` prints `census_converged`,
`uncovered_census_items`, `contested`, `unresolved_contested`, and a
`should_continue` hint. Convergence requires the census stable + covered
**and** zero `unresolved_contested`. If `should_continue` is `true`,
increment `round` and loop to **2a**; otherwise go to Step 3.

## Step 3 — freeze the ground truth

```bash
python3 .claude/skills/code-review-replay/scripts/collect.py freeze \
  --session <SESSION>
```

`freeze` writes the `ground_truth_v3` block into the dataset JSON, refreshes
the PR's `index.json` entry, and tears down the worktree. If
`census_converged` is `false`, the budget was spent or a contested finding
was left unresolved — the ground truth is usable but known-incomplete.

## Step 4 — validate

```bash
python3 .claude/skills/code-review-replay/scripts/collect.py validate <repo>-<pr>
python3 .claude/skills/code-review-replay/scripts/collect.py validate --all
```

`validate` runs structural checks (schema, well-formed GT, every entry has
`file`/`line`/`severity`) and quality gates (not converged, unresolved
contested, low harvest agreement, budget-forced, empty TP set). Exit 0 =
clean, 1 = quality warnings, 2 = malformed. `--all` checks every PR in the
index and prints a tally.

---

# Replay mode

`/code-review-replay [<repo>-<pr>]` — score a reviewer-under-test.

With no PR id, replay **randomly samples** PRs that have a frozen ground
truth. Pass a `<repo>-<pr>` id to score one specific PR.

## Step 1 — build bramble

```bash
bazel build //bramble:bramble
```

Always replay against the bazel-built binary, never whatever is on `PATH` (a
stale `~/bin/bramble` silently changes reviewer behavior).

## Step 2 — run the replay scorer

```bash
# Sample 5 random GT-collected PRs (default):
python3 .claude/skills/code-review-replay/scripts/replay.py \
  --bramble-bin "$(bazel info bazel-bin)/bramble/bramble_/bramble" \
  --print-markdown

# Or score one specific PR:
python3 .claude/skills/code-review-replay/scripts/replay.py <repo>-<pr> \
  --bramble-bin "$(bazel info bazel-bin)/bramble/bramble_/bramble" \
  --print-markdown
```

`--sample N` sets how many PRs the no-target form draws (default 5).
`replay.py` runs `bramble code-review` per config (default `codex-5.4-mini`
+ `cursor-composer2`; add `--config gemini-3.1-flash-lite-preview`) at the
frozen diff's `head_before`, matches each finding to the frozen
`ground_truth_v3`, and writes a scored result to
`~/.bramble/code-review-eval/replays/`. Filter rounds with `--tier r1` or
`--tier final`. Replay never spawns judge sub-agents or modifies the
dataset; if it surfaces a real bug the frozen GT missed, re-run collection.

Before scoring, each dataset's frozen GT is run through the same
`validate_dataset` gate as `collect.py validate`: a structurally malformed
GT aborts that PR (the metrics would be meaningless), and quality warnings
(unconverged census, unresolved contested rows, low harvest agreement) are
printed to stderr. Pass `--strict` to also abort on quality warnings — use
it when a benchmark number must not be reported against a weak ground truth.

## Scoring rubric

All metrics come from matching the reviewer's findings to the **frozen**
`ground_truth_v3` — purely mechanically:

- **matched_tp** — a finding landed on a `true_positive` (a real bug).
- **matched_fp** — a finding landed on a `false_positive` (known noise).
- **unmatched** — a finding matched no GT entry; excluded from precision.
- **Precision** = `matched_tp / (matched_tp + matched_fp)`.
- **Recall** = `|distinct true_positives caught| / |true_positives|` —
  bounded by 1.0.
- **F1** = harmonic mean.
- **severity_mismatches** — matched TPs the reviewer reported at the wrong
  severity (a separate signal; does not move P/R/F1).
- **missed_true_positives** — GT real bugs no finding caught.

## How to interpret results

- **Low recall, high precision** — conservative reviewer: what it flags is
  real, but it misses bugs. See `missed_true_positives`.
- **Low precision** — noisy reviewer: it repeats known false positives.
- **Many `unmatched` findings** — the reviewer surfaced things the dataset
  never judged. It may have found real bugs collection missed (re-run
  collection with more rounds) or be noisy in a new way.
- **`census_converged: false` in the GT** — the recall denominator may be
  incomplete; treat recall as a lower bound.

## Key files

| Area | File |
|------|------|
| Collection: harvester | `.claude/skills/code-review-replay/scripts/harvest.py` |
| Collection: glue + GT model | `.claude/skills/code-review-replay/scripts/collect.py`, `collect_lib.py` |
| Replay: scorer | `.claude/skills/code-review-replay/scripts/replay.py` |
| Parsing / goal / scoring lib | `.claude/skills/code-review-replay/scripts/replay_lib.py` |
| Dataset schema + CLI reference | `.claude/skills/code-review-replay/scripts/README.md` |
| Dataset records (frozen GT) | `~/.bramble/code-review-eval/dataset/` (outside the repo) |
| Scored replay results | `~/.bramble/code-review-eval/replays/` (outside the repo) |
| Collection session state | `~/.bramble/code-review-eval/collect/` (outside the repo) |
