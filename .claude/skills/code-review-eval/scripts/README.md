# code-review eval dataset

The harvester (`harvest.py`) turns past `/pr-polish` runs into a structured
eval dataset. Each PR becomes one JSON file under
`~/.bramble/code-review-eval/dataset/<repo>-<pr>.json`, plus a top-level
`index.json`. The dataset is the ground-truth source for evaluating future
reviewer-quality changes: a good reviewer **catches real issues at the right
severity (recall)** and **stays quiet on near-clean code (precision)**.

> **The dataset is stored OUTSIDE the repo.** It is derived from real
> private PRs — file paths, commit SHAs, and reviewer findings — so it
> must never be committed. The canonical home is
> `~/.bramble/code-review-eval/`, next to the `~/.bramble/projects/`
> pr-polish data it is built from. `.gitignore` also blocks the
> in-repo `data/dataset/` and `data/replays/` paths as a safety net.

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

## Per-PR file schema (v1)

```jsonc
{
  "schema_version": 1,
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
      "raw_comment_actions": [ /* this round's comment_actions verbatim */ ],
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

## Ground-truth derivation

`is_real_issue` is a coarse boolean derived from the raw
`comment_actions.action`:

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

## Goal text

`--goal` text is **not** persisted on disk (the run log only emits
`goal_len`). The harvester reconstructs it deterministically via
`/.claude/skills/pr-polish/scripts/bramble_ops.py#goal_for_round`:

- R1 → the PR title+body fetched once via `gh pr view`.
- R2+ → built from the state file itself; no network needed.

If `gh` is missing or the PR's repo is not in `--repos-root`,
`goal_recoverable=false` and `goal_text=null` for that round.

## CLI

```
python3 .claude/skills/code-review-eval/scripts/harvest.py \
  --source-dir ~/.bramble/projects \                          # default
  --out-dir   ~/.bramble/code-review-eval/dataset \           # default
  --repos-root kernel=/home/ubuntu/g/kernel \                 # repeatable NAME=PATH
  --repos-root yoloswe=/home/ubuntu/worktrees/yoloswe/main \
  --only kernel-3945 \                                        # optional, repeatable
  --skip-pr-summary \                                         # optional; null R1 goal
  --dry-run --verbose
```

`--repos-root NAME=PATH` is what lets the harvester compute the
`merge_base_sha`, `files_changed`, and `repo_url` (the URL is read from
`git config --get remote.origin.url`). Without it, those fields are left
null / `merge_base_resolved=false` and the dataset still emits — replay
just becomes harder.

Exit codes: **0** on full success, **1** when some PRs had non-fatal
issues (e.g. `pr_summary` not fetchable), **2** when nothing harvested.

## Testing

```
python3 -m unittest discover -s .claude/skills/code-review-eval/scripts/tests -v
```

The integration test against `~/.bramble/projects/kernel-3945/` is
skipped when the fixture isn't present.
