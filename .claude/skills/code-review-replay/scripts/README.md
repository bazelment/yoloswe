# code-review eval dataset

These scripts back the `/code-review-replay` skill. The skill has two modes:

- **Collection mode** builds a per-PR eval dataset and freezes a *judged
  ground truth* into it. The skill drives a per-PR round loop; `collect.py`
  is stateless glue with one sub-command per step:
  1. `collect.py setup` harvests the PR (via `harvest.py`) if needed and
     pins one git worktree at the diff's `head_before`.
  2. Each round: the skill re-runs `bramble code-review`, then
     `collect.py build-prompt` writes a judge prompt and the skill spawns an
     independent judge sub-agent, then `collect.py fold` merges the verdict.
  3. When the judge's full-diff bug census *saturates*, `collect.py freeze`
     writes a `ground_truth_v3` block into the dataset JSON.
- **Replay mode** (`replay.py`) runs a reviewer-under-test once and scores it
  **mechanically** against the frozen `ground_truth_v3` — precision / recall
  / F1, no judge sub-agents. This is the cheap, repeatable path.

A good reviewer **catches real issues at the right severity (recall)** and
**stays quiet on near-clean code (precision)**.

> **The dataset is stored OUTSIDE the repo.** It is derived from real
> private PRs — file paths, commit SHAs, and reviewer findings — so it
> must never be committed. The canonical home is
> `~/.bramble/code-review-eval/`, next to the `~/.bramble/projects/`
> pr-polish data it is built from. `.gitignore` also blocks the
> in-repo `data/dataset/` and `data/replays/` paths as a safety net.

## Scripts

| Script | Mode | Role |
|--------|------|------|
| `harvest.py` / `harvest_lib.py` | collection | past `/pr-polish` run → v2 dataset record; repo-root auto-discovery |
| `collect.py` / `collect_lib.py` | collection | scan/setup/build-prompt/fold/freeze/validate; GT model + convergence |
| `replay.py` / `replay_lib.py` | replay | run a reviewer-under-test, score mechanically vs the frozen GT |

## Why this exists

Every `/pr-polish` invocation leaves behind a rich audit trail in
`~/.bramble/projects/<repo>-<pr>/`:

- `pr-polish-state.json` — per-PR state, including per-round
  `comment_actions[]` recording the engineer's verdict on each finding
  (`fixed`, `false_positive`, `wont_fix`, `ack`, `stale`, …).
- `r{N}/<backend>-envelope.json` — raw per-backend review envelope with
  the original `severity / message / suggestion / file / line` fields.

We harvest a subset of this — R1 and the final round per PR — into one
JSON per PR so:

- the dataset is small enough to grep but rich enough to replay;
- each finding carries a derived `is_real_issue` label plus the raw
  `comment_actions.action`;
- a future replay script has everything it needs to re-run
  `bramble code-review` apple-to-apple against the same commit and the
  same `--goal` text.

## Per-PR file schema (v2)

```jsonc
{
  "schema_version": 2,
  "harvested_at": "<ISO 8601 UTC>",
  "harvester_git_sha": "<sha of yoloswe at harvest time>",
  "pr": {
    "repo_name": "kernel",
    "repo_url": "https://github.com/anthropics/kernel",
    "pr_number": "3945",
    "pr_url": "https://github.com/anthropics/kernel/pull/3945",
    "branch": null,
    "started_at": "...",
    "completed": true,
    "exit_reason": "converged",
    "total_rounds": 5
  },
  "pr_comments_attribution_basis": "created_at",  // see "PR comments" below
  "pr_comments_fetch_error": null,
  "harvested_rounds": [
    {
      "round": 1,
      "signal_tier": "r1",                 // r1 | final | final_incomplete | r1_only
      "head_before": "<sha>",
      "head_after": "<sha>",
      "base_branch": "origin/main",
      "merge_base_sha": "<sha>",           // git merge-base origin/main head_before
      "merge_base_resolved": true,
      "merge_base_error": null,
      "files_changed": ["path/to/file.py", ...],
      "goal_text": "...",                  // what was passed to --goal
      "goal_recoverable": true,
      "scope_hints_present": false,
      "raw_comment_actions": [ /* see "PR comments" below */ ],
      "review_runs": [
        {
          "backend": "codex",              // codex | cursor | gemini | lint
          "model": "gpt-5.4-mini",
          "session_id": "...",
          "review_mode": "code",
          "resume_status": null,           // null on R1; ok|fallback|unverified on final
          "envelope_status": "ok",         // ok | error | (missing-rows are omitted)
          "envelope_error": null,
          "verdict": "rejected",
          "summary": "...",
          "duration_ms": 555302,
          "input_tokens": 0,
          "output_tokens": 0,
          "schema_version": 1,
          "findings": [
            {
              "severity": "high",
              "message": "...",
              "suggestion": "...",
              "file": "...",
              "line": 94,
              "confidence": 0.98,
              "invariant": null,
              "sites": null,
              "ground_truth": {
                "matched_comment_action": true,
                "match_strategy": "exact",   // exact|topic_path_line|topic_path|topic_only|none
                "action": "fixed",           // raw comment_actions.action
                "reason": null,
                "is_real_issue": true,       // derived: see table below
                "fixed_in_commit": "5d3904bd9",
                "comment_actions_source": "codex"
              }
            }
          ]
        }
      ]
    }
  ]
}
```

There is intentionally **no `stats` block**: every count (by action,
by severity, consensus, unmatched) is derivable from the rows. A
downstream consumer that wants aggregates computes them on read.

## The `ground_truth_v3` block (frozen, judged)

Collection mode adds one extra top-level key to the dataset JSON,
alongside `harvested_rounds`. It is the **authority replay mode scores
against** — built by judging, not by reading `comment_actions`:

```jsonc
"ground_truth_v3": {           // key name unchanged; schema_version is 4
  "schema_version": 4,
  "frozen_at": "<ISO 8601 UTC>",
  "collector_git_sha": "<sha>",
  "rounds_run": 3,
  "census_converged": true,
  "true_positives": [
    {
      "file": "scripts/deploy.py", "line": 679,
      "severity": "high",            // the JUDGE's canonical severity
      "reviewer_severity": "medium", // what the reviewer reported
      "topic": "off-by-one in rollout index",
      "first_seen_round": 1, "surfaced_by": ["codex", "cursor"],
      "judge_reason": "confirmed at deploy.py:679",
      "verdict_history": [           // per-round trail for this defect
        {"round": 1, "verdict": "true_positive", "reason": "..."}
      ],
      "resolved": true,              // meaningful for contested entries
      "comment_action_xref": true    // harvested is_real_issue, or null
    }
  ],
  "false_positives": [ /* same shape; judge_reason explains why not real */ ],
  "contested": [ /* same shape; defects judged inconsistently across rounds */ ],
  "per_round_diff": [ /* census size + new + uncovered + contested per round */ ],
  "dataset_xref": {
    "comparisons": 9, "agreements": 7,
    "comment_action_agreement_rate": 0.78,
    "disagreements": [ /* entries where judge != harvested is_real_issue */ ]
  }
}
```

`true_positives` is the **complete real-bug census**. The convergence test
(see below) guarantees every censused bug is covered by a reviewer finding,
so there is no separate "missed issues" field. When a PR exhausts the round
budget without saturating, `census_converged` is `false` and the still-
uncovered census items are recorded in `per_round_diff`.

The judge assigns each finding's `severity` itself — `reviewer_severity` is
the reviewer's reported value, kept alongside so the divergence is
auditable; replay scores a finding's severity against the judge's.

### `contested` — findings judged inconsistently across rounds

A defect can be judged `true_positive` in one round and `false_positive` in
another. The accumulator never silently keeps one — it moves the defect to
`contested` with both verdicts in `verdict_history` and `resolved: false`.
A later round's judge re-rules it (the defect appears in that round's
`contested_findings` prompt input); the binding verdict sets `resolved:
true` and moves the entry into `true_positives` / `false_positives`. An
unresolved contested entry blocks convergence — the ground truth on that
defect is genuinely unsettled.

### Census-saturation convergence

Each collection round, the judge sub-agent verdicts every reviewer finding
(`true_positive` / `false_positive` / `unsure`) **and** independently
censuses the real bugs in the diff. `collect_lib.census_converged` returns
true when, after a round, **all three** hold:

1. the cumulative census set is unchanged versus the prior round (no new
   real bug surfaced), and
2. every census item is covered by a `true_positive` finding (the reviewers,
   in aggregate, caught every real bug the judge censused), and
3. no contested finding is still unresolved.

At least two rounds are always required. `unsure` verdicts carry no ground
truth and are dropped — they are neither a true nor a false positive, and
they neither flip nor resolve a defect.

### `dataset_xref` — a signal on the harvest, not a label

`dataset_xref` compares each judged verdict to the harvested `is_real_issue`
label (below) where both have an opinion. A low
`comment_action_agreement_rate` means the harvested dataset is unreliable —
worth re-triaging the original PR or fixing the harvester. It is *reported*,
never *acted on*: the judge's verdict is the ground truth.

## Harvest-time `is_real_issue` (v2 — a reference, not the label)

`is_real_issue` is a coarse boolean derived from the raw
`comment_actions.action`. Collection mode keeps it only as the
`comment_action_xref` cross-check — the judged `ground_truth_v3` block is
what replay mode scores against.

| raw action      | is_real_issue | rationale                                           |
|-----------------|---------------|-----------------------------------------------------|
| `fixed`         | `true`        | reviewer was right; code change applied             |
| `wont_fix`      | `true`        | reviewer was right; we chose not to fix             |
| `false_positive`| `false`       | reviewer was wrong (refuted at the cited file:line) |
| `stale`         | `false`       | cited code no longer exists                         |
| `ack`           | `null`        | low/nit acknowledged; insufficient signal           |
| `flake`         | `null`        | CI-only; environmental                              |
| `pre_existing`  | `null`        | CI-only; pre-dates this PR                          |
| (unmatched)     | `null`        | no `comment_actions` entry matched this finding     |

`action` and `reason` are preserved verbatim so consumers can apply
their own labeling rule if they want a different cut.

## Finding ↔ comment_action matching

`comment_actions[i].topic` is a short summary, not the verbatim envelope
`message`. Matching has five tiers (highest precision first):

| Tier | Strategy           | Rule                                                                                                  |
|------|--------------------|-------------------------------------------------------------------------------------------------------|
| 1    | `exact`            | same path + same line + same severity + (`source==backend` OR `source` in `{sweep, consensus}`)       |
| 2    | `topic_path_line`  | same normalized path + line within ±3 + topic substring in `message[:100]` (case-insensitive)         |
| 3    | `topic_path`       | same normalized path + topic substring in message                                                     |
| 4    | `topic_only`       | topic-token Jaccard overlap > 0.5                                                                     |
| 5    | `none`             | no match (`is_real_issue: null`, recorded as `matched_comment_action: false`)                          |

`source: sweep` (and `consensus`) are wildcard backends in Tier 1 —
that's how `/pr-polish` records consensus-merged findings. Tie-breaking:
higher tier wins; within a tier, `fixed > wont_fix > false_positive > others`;
within an action, earliest in the candidate list.

## Round selection

We only harvest R1 and the final round per PR (highest signal):

- **R1** = fresh-eyes recall on the original diff.
- **Final** = precision on near-converged code. A good reviewer should
  find ~nothing.

Single-round PRs collapse to one entry with `signal_tier="r1_only"`.
Incomplete loops mark the last round as `signal_tier="final_incomplete"`
so consumers can filter them out if they want a clean precision signal.

## PR comments

`raw_comment_actions` on each harvested round holds two kinds of rows.

**Reviewer findings** (`source` ∈ `codex / cursor / gemini / lint / sweep /
consensus / ci`) are the bramble + CI signals pr-polish recorded. They stay
keyed to the round they were recorded in — unchanged from v1.

**GitHub PR comments** (`source` ∈ `github-inline / github-issue /
github-review`) are the comments humans and review bots authored on the PR.
The harvester **fetches these fresh from GitHub** (`gh api` on the three
comment endpoints) rather than trusting pr-polish state, because:

- pr-polish only fetches PR comments at the *start of each series*, so on a
  long, many-times-resumed PR they land in scattered middle rounds — and the
  harvester only emits R1 + final, dropping the rest.
- pr-polish re-fetches *all* open comments every series, so the same comment
  recurs across many rounds' `comment_actions` as stale duplicates.

Each fetched github comment is:

1. **Verdict-joined** — matched back to the `comment_actions` row that recorded
   the engineer's verdict, by `comment_id` (precise), else by the recorded
   `topic` being a substring of / well-contained in the body. `action` is
   `null` when GitHub returned a comment pr-polish never triaged — the dataset
   is a *complete census*, not just the triaged subset.
2. **Round-attributed** — `attributed_round` is the round whose
   `[head_before(n), head_before(n+1))` commit-time window contains the
   comment's `created_at`.
3. **Folded** onto a harvested round — since only R1 + final are emitted, a
   comment attributed to a middle round folds onto the nearest harvested round
   without crossing forward (`attributed_round <= r1.n` → R1, else → final).
   Every PR comment thus appears exactly once.

GitHub-comment rows carry: `comment_id, source, author, is_bot, path, line,
body, created_at, original_commit_id, action, reason, comment_actions_source,
attributed_round`.

`pr.pr_comments_attribution_basis` records how attribution was done:

| value                   | meaning                                                              |
|-------------------------|----------------------------------------------------------------------|
| `created_at`            | round-boundary commit times resolved; comments bucketed by timestamp |
| `unmapped_repo_fallback`| repo checkout not discovered; every comment folds onto R1            |
| `no_timestamp`          | `--skip-pr-comments` or the fetch failed; the state-recorded github `comment_actions` are used instead, attributed to their recorded round |

`pr.pr_comments_fetch_error` is the `gh` error string when the fetch was
partial or failed, else `null`.

## Goal text

`--goal` text is **not** persisted on disk (the run log only emits
`goal_len`). The harvester reconstructs it deterministically via
`/.claude/skills/pr-polish/scripts/bramble_ops.py#goal_for_round`:

- R1 → the PR title+body fetched once via `gh pr view`.
- R2+ → built from the state file itself; no network needed.

If `gh` is missing or the PR's repo checkout was not discovered,
`goal_recoverable=false` and `goal_text=null` for that round.

## CLI

Repo checkouts are **auto-discovered** (`~/worktrees/<name>/main`,
`~/g/<name>`). No script needs a `--repos-root`; pass `--repo-root NAME=PATH`
only to override discovery for a repo checked out somewhere non-standard.

### `harvest.py` — raw-dataset harvester

```
python3 .claude/skills/code-review-replay/scripts/harvest.py \
  --only kernel-3945 \          # optional, repeatable; default = all PRs
  --skip-pr-comments \          # optional; offline — skip the gh api fetch
  --dry-run --verbose
```

Walks `~/.bramble/projects/` and emits one v2 per-PR JSON. `--skip-pr-comments`
skips the `gh api` PR-comment fetch (github comments fall back to the
state-recorded set). Exit codes: **0** full success, **1** non-fatal issues,
**2** nothing harvested. Collection mode runs this for you via `setup`.

### `collect.py` — collection driver

`collect.py` is stateless glue; the **skill** runs `bramble code-review` and
spawns the judge sub-agents. Sub-commands:

```
# Default (no subcommand) / `scan` — list collectable PRs.
python3 .../collect.py scan

# Prepare a PR: harvest if needed, discover the repo, pin one worktree.
python3 .../collect.py setup kernel-3945

# Build a round's judge prompt-input (cumulative findings + census +
# contested). --include-harvested folds the original pr-polish review in.
python3 .../collect.py build-prompt --session <SESSION> --round 1 \
  --include-harvested \
  --envelope codex=/tmp/r1/codex-envelope.json \
  --envelope cursor=/tmp/r1/cursor-envelope.json

# Merge the round's judge verdict; report convergence.
python3 .../collect.py fold --session <SESSION> --round 1   # --round-budget 10

# Write the ground_truth_v3 block; refresh the index; tear down the worktree.
python3 .../collect.py freeze --session <SESSION>

# Structural + quality check of a frozen dataset (exit 0/1/2).
python3 .../collect.py validate kernel-3945
python3 .../collect.py validate --all          # every PR in the index
```

**Worktree.** `setup` creates **one** detached worktree under the session
dir, pinned at the canonical round's `head_before`, reused by every round —
it is both the cwd for the skill's `bramble code-review` re-runs and the
judge's `repo_path`. Pinning is load-bearing: a judge reading the live
checkout at today's HEAD would see already-fixed code and wrongly call real
findings false positives. `freeze` tears it down.

`setup` prints `{target, session, worktree, canonical_round}`. `fold` prints
`census_converged`, `contested`, `unresolved_contested`, and a
`should_continue` hint. Every round re-reviews uniformly (no special round
1); `--include-harvested` folds the harvested pr-polish review in as a free
extra data point. `freeze` refreshes the PR's `index.json` entry.

`validate` reports structural errors (malformed GT, missing
`file`/`line`/`severity`) and quality warnings (not converged, unresolved
contested, harvest agreement < 0.6, round budget exhausted, empty
`true_positives`). Exit **0** clean / **1** warnings / **2** malformed.
`--all` iterates the index with a per-PR verdict and tally.

### `index.json` — dataset manifest

`harvest.py` writes a top-level `index.json` listing every PR. Each entry
carries `repo_name`, `pr_number`, `pr_url`, `file`, `completed`,
`total_rounds`, `harvested_rounds`, plus two collection-quality fields:
`ground_truth_collected` (has `collect` run) and `census_converged`
(`true`/`false`, or `null` when not collected). Replay's no-target sampling
draws its pool straight from this.

### `replay.py` — replay mode

```
# Sample 5 random GT-collected PRs (default):
python3 .../replay.py \
  --bramble-bin "$(bazel info bazel-bin)/bramble/bramble_/bramble" \
  --print-markdown

# Or score one PR:
python3 .../replay.py kernel-3945 \
  --bramble-bin "$(bazel info bazel-bin)/bramble/bramble_/bramble" \
  --config codex-5.4-mini --config cursor-composer2 --print-markdown
```

With no positional target, replay samples `--sample N` (default 5) PRs that
have a frozen `ground_truth_v3`. A PR without that block exits 2 with a
pointer to run collection mode.

## Testing

```
python3 -m unittest discover -s .claude/skills/code-review-replay/scripts/tests -v
```

The integration test against `~/.bramble/projects/kernel-3945/` is
skipped when the fixture isn't present.
