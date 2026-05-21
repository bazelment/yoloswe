---
name: code-review-replay
description: "Replay a past /pr-polish'd PR through bramble code-review and judge the result independently. A mechanical phase re-runs the reviewer and captures its execution trace; a judgment phase uses sub-agents to verdict each finding against the real diff — treating the harvested dataset as a reference, not ground truth. Produces precision/recall/F1 plus a dataset-agreement signal and per-reviewer execution analysis."
argument-hint: "<repo>-<pr> [--tier r1|final] [--config NAME ...]"
---

# Code Review Replay

`/code-review-replay` measures **how good a code reviewer is** by re-running
it on a PR we already `/pr-polish`'d, then judging the result.

The companion `/code-review-eval` skill compares backends side-by-side on a
live branch. This skill instead asks: **"if we re-ran this reviewer on a PR
we already polished, would it catch the real issues and stay quiet on the
noise?"**

## Why this skill was redesigned

The old replay **trusted the harvested dataset as ground truth**. That was
wrong on three counts, and the current design fixes all three:

1. **Findings are judged, not looked up.** The dataset only labels findings
   the *original* review surfaced *and* an engineer acted on. It is
   incomplete, and the original triage can itself be wrong. So replay no
   longer reads `is_real_issue` off the dataset to score. A judge sub-agent
   reads the **actual diff** and decides true-positive / false-positive /
   unsure on the code's merits. The dataset label is shown to the judge as a
   *reference* it may disagree with.
2. **The goal is built independently.** Replay no longer feeds the dataset's
   recorded `goal_text` into `--goal`. It reconstructs the goal itself (R1
   from the live PR title/body + diffstat; R2+ deterministically from
   pr-polish state). The dataset goal is kept only as a cross-check — when
   the two diverge, the run is flagged `goal_divergence`.
3. **The reviewer's process is inspected.** Replay captures each reviewer's
   klogfmt execution trace — which files it read, where it spent time, what
   it skipped — and the judge sub-agent turns that into concrete
   improvement notes.

## Two-phase workflow

```
Phase A (mechanical, replay.py)     Phase B (judgment, sub-agents + replay.py)
┌──────────────────────────┐       ┌────────────────────────────────────┐
│ build independent goal   │       │ 1 judge sub-agent per (round,config)│
│ run bramble per config   │  ───▶ │ reads real diff, verdicts findings  │
│ capture execution trace  │       │ censuses missed issues              │
│ emit NEUTRAL artifact    │       │ analyses execution trace            │
│ + per-run prompt JSONs   │       │ writes verdict JSON                 │
└──────────────────────────┘       │ replay.py --fold-verdicts → scored  │
                                   └────────────────────────────────────┘
```

Phase A makes **no** true/false-positive judgment and computes **no**
precision/recall. All judgment happens in Phase B.

## How to run

### Step 0 — confirm the dataset exists

```bash
ls ~/.bramble/code-review-eval/dataset/<repo>-<pr>.json
```

If it is missing, see **"If the dataset is missing"** below.

### Step 1 — build bramble from this repo

Always replay against the bazel-built binary, never whatever is on `PATH`
(a stale `~/bin/bramble` silently changes reviewer behavior).

```bash
bazel build //bramble:bramble
```

This produces `bazel-bin/bramble/bramble_/bramble`.

### Step 2 — Phase A: run the reviewer, emit the neutral artifact

```bash
python3 .claude/skills/code-review-eval/scripts/replay.py <repo>-<pr> \
  --bramble-bin "$(bazel info bazel-bin)/bramble/bramble_/bramble" \
  --repos-root kernel=/home/ubuntu/worktrees/kernel/main \
  --repos-root yoloswe=/home/ubuntu/worktrees/yoloswe/main \
  --repos-root nebula=/home/ubuntu/worktrees/nebula/main \
  --verbose --print-markdown
```

Default configs are `codex-5.4-mini` + `cursor-composer2`. Add gemini with
`--config gemini-3.1-flash-lite-preview` (repeatable). Filter rounds with
`--tier r1` or `--tier final`.

Phase A writes:
- `<log-root>/<repo>-<pr>-<id>/artifact.json` — the neutral artifact.
- `<log-root>/<repo>-<pr>-<id>/prompts/r<n>-<config>-prompt.json` — one
  prompt-input per `(round, config)` run.
- per-config `*-envelope.json`, `*-runlog.log` (klogfmt), and
  `*-protocol.jsonl` (codex protocol log, copied for codex runs).

The `--print-markdown` output ends with the exact Phase-B commands. Note the
**artifact path** and the **prompt-input dir**.

### Step 3 — Phase B: spawn one judge sub-agent per prompt-input

For **each** `r<n>-<config>-prompt.json` in the prompt dir, spawn a
`general-purpose` sub-agent (it needs Bash for `git diff`/`git show` and
Read). Run them in **batches** so context stays clean. Use this prompt
template verbatim, substituting the prompt-input path:

> You are an independent code-review judge. Your job is to assess the
> quality of one `bramble code-review` run by inspecting the **real diff**
> it reviewed — **not** by trusting any pre-existing labels.
>
> Read the prompt-input JSON at `<PROMPT_INPUT_PATH>`. It contains:
> `repo_path`, `diff_ref` (with a ready-to-run `diff_command`),
> `replay_findings` (what the reviewer reported), `mechanical_match_hints`
> (token-overlap guesses linking each finding to a dataset finding — a
> HINT only), `dataset_findings_reference` (the harvested dataset — a
> REFERENCE only), and `execution_trace` (how the reviewer worked).
>
> Do the following:
>
> 1. **Reconstruct the diff.** Run the `diff_command` from `diff_ref`.
>    Restrict your attention to `files_changed`. Read the surrounding code
>    in `repo_path` as needed (the repo is checked out; `git show` any
>    commit).
> 2. **Judge every replay finding independently.** For each entry in
>    `replay_findings`, read the cited code and decide: `true_positive`
>    (a real defect or a legitimate concern), `false_positive` (wrong,
>    or not an issue), or `unsure` (genuinely cannot tell). Write a
>    one-line `reason` grounded in the code. The `mechanical_match_hints`
>    and `dataset_findings_reference` show what the original triage
>    concluded — **agree only when the code supports it.** When your
>    verdict differs from the dataset label, set `dataset_disagreement:
>    true`.
>
>    **Verify the finding's premise, not just its topic.** A finding is a
>    `false_positive` when its *claim* is wrong even if the area is
>    plausible. Two recurring FP shapes to actively check:
>    - **"Missing test" claims** — read the *whole* test file (or grep the
>      feature/function name across it) before agreeing. Reviewers
>      routinely cite one test and miss a sibling test a few lines below
>      that already provides the coverage.
>    - **"Unreachable" / "wrong value" claims** — trace the data flow.
>      If a finding flags a divergence on a field, confirm the bad value
>      is actually reachable (e.g. a non-nullable column always set to a
>      valid value makes the divergence dead code → `false_positive`).
> 3. **Census missed real issues.** Scan the diff for real bugs that **no**
>    finding in `replay_findings` caught. List them — this is true recall,
>    not dataset recall.
> 4. **Analyse the execution trace.** `execution_trace` is parsed from the
>    klogfmt run log for cursor/gemini and from the codex protocol JSONL
>    for codex — so `n_tool_calls`, `tool_kind_counts`, and `files_read`
>    are populated for **all** backends; you should not see a codex run
>    with `n_tool_calls=0`. If you need raw detail, the copied logs are at
>    `runlog_path` (klogfmt) and `protocol_log_path` (codex JSONL); render
>    a protocol JSONL with `bazel run //bramble/cmd/logview -- <file>`.
>    Identify concrete process problems: changed files never read (see
>    `files_changed_not_read`), time sunk in dead-end greps, premature
>    termination (investigation greps landed on a real defect's code path
>    but no finding was reported), redundant work (the same file re-read
>    in many overlapping ranges). Produce actionable improvement notes.
> 5. **Write your verdict** as JSON to the `verdict_output_path` named in
>    the prompt-input, using exactly this schema:
>
> ```json
> {
>   "round": 1,
>   "config": "codex-5.4-mini",
>   "finding_verdicts": [
>     {"index": 0, "verdict": "true_positive",
>      "reason": "off-by-one confirmed at deploy.py:679",
>      "dataset_disagreement": false}
>   ],
>   "missed_real_issues": [
>     {"file": "scripts/deploy.py", "line": 12,
>      "description": "unhandled None from get_rollout()", "severity": "high"}
>   ],
>   "execution_analysis": [
>     {"observation": "never read the changed test file",
>      "improvement": "read all files_changed before verdict",
>      "severity": "medium"}
>   ]
> }
> ```
>
> `index` is the position in `replay_findings`. `verdict` must be one of
> `true_positive` / `false_positive` / `unsure`. Judge on the code, not
> the labels.

### Step 4 — fold the verdicts into the scored result

Once every sub-agent has written its verdict JSON:

```bash
python3 .claude/skills/code-review-eval/scripts/replay.py <repo>-<pr> \
  --fold-verdicts <PROMPT_INPUT_DIR> \
  --artifact <ARTIFACT_PATH> \
  --print-markdown
```

This writes `~/.bramble/code-review-eval/replays/<repo>-<pr>-<ts>-scored.json`
and prints the summary.

## Scoring rubric

All metrics come from the **judge's verdicts**, never the dataset:

- **Precision** = `judged_TP / (judged_TP + judged_FP)` — of the reviewer's
  findings the judge could rule on, how many were real.
- **Recall** = `judged_TP / (judged_TP + missed_real_issues)` — the
  denominator is the judge's **independent** census of real issues in the
  diff, so a reviewer is penalised for bugs the *original* review also
  missed.
- **F1** = harmonic mean.
- `judged_unsure` findings are excluded from precision (neither TP nor FP).
- A finding the judge skipped entirely counts as `unsure`, never as TP/FP.

Plus a signal **on the dataset itself**:

- **`dataset_agreement_rate`** = how often the judge's verdict matched the
  dataset's `is_real_issue` label, over findings where both had an opinion.
  A low rate means the harvested dataset is unreliable — worth fixing the
  harvester, or re-triaging the original PR.

And per run:

- **`execution_analysis[]`** — concrete, actionable notes on how the
  reviewer worked.
- **`missed_real_issues[]`** — real bugs no reviewer surfaced.
- **`goal_divergence`** — set when the independently-reconstructed goal
  materially differed from the dataset's recorded goal.

## How to interpret results

- **Low `dataset_agreement_rate`** — the judge disagreed with the harvested
  labels often. Investigate: either the harvester's matcher is wrong or the
  original `/pr-polish` triage was. Do **not** assume the judge is wrong;
  that is the whole point of judging independently.
- **`missed_real_issues` non-empty across all configs** — a real bug the
  reviewers (and likely the original review) all missed. Highest-value
  output of the skill.
- **`execution_analysis` recurring across runs** — a systemic reviewer
  weakness (e.g. "never reads changed test files"). Feed it back into the
  reviewer prompt or backend.
- **`goal_divergence: true`** — the score may partly reflect a goal
  difference, not reviewer skill. Re-run with `--goal-source dataset` to
  isolate.
- **`fold_error` on a run** — the sub-agent's verdict JSON was missing or
  malformed; that run was not scored. Re-spawn the sub-agent for it.

## Independent goal construction

Phase A builds the `--goal` itself:

- **R1** — `gh pr view` for the live title + body, plus a `git diff --stat`.
- **R2+** — deterministic reconstruction via pr-polish's
  `bramble_ops.goal_for_round` (the same path the harvester uses).
- **Fallback** — if `gh` fails or state is unavailable, falls back to the
  dataset's recorded goal and notes it.

`--goal-source dataset` forces the old behavior (dataset goal verbatim) for
debugging.

## Picking a PR to replay

Recall is only meaningful when the diff actually contains real bugs. Any PR
replays fine — a near-clean final round just measures false-positive rate.
To find datasets where the *original* review found labeled real issues:

```bash
for f in ~/.bramble/code-review-eval/dataset/*.json; do
  python3 -c "import json,sys; d=json.load(open(sys.argv[1])); \
    n=sum(1 for r in d.get('harvested_rounds',[]) \
      for rr in r.get('review_runs',[]) for fi in rr.get('findings',[]) \
      if (fi.get('ground_truth') or {}).get('is_real_issue') is True); \
    print(f'{sys.argv[1].split(\"/\")[-1]}: {n} real')" "$f"
done
```

This is only a hint for picking a PR — the judge still censuses real issues
independently, so a PR with `0 real` here can still surface missed bugs.

## If the dataset is missing

The replay needs `~/.bramble/code-review-eval/dataset/<repo>-<pr>.json`,
built by the harvester from `/pr-polish` run history.

1. **If `~/.bramble/projects/<repo>-<pr>/` exists** — the PR was polished on
   this machine but not yet harvested. Harvest it:

   ```bash
   python3 .claude/skills/code-review-eval/scripts/harvest.py \
     --only <repo>-<pr> \
     --repos-root kernel=/home/ubuntu/worktrees/kernel/main \
     --repos-root yoloswe=/home/ubuntu/worktrees/yoloswe/main \
     --repos-root nebula=/home/ubuntu/worktrees/nebula/main \
     --verbose
   ```

   Auto-detect `--repos-root` paths with
   `find /home/ubuntu/worktrees -maxdepth 2 -name main -type d`.

2. **If `~/.bramble/projects/<repo>-<pr>/` does NOT exist** — the PR was
   never polished here; the dataset cannot be regenerated. `replay.py`'s
   error lists every PR that *can* be harvested. Pick a different PR.

3. **If the dataset is stale** — re-run `harvest.py` with no `--only`; it
   overwrites every per-PR file.

## What the replay does NOT do

- It does **not** modify the dataset. The judge's verdicts live in the
  scored result, not the source-of-truth dataset. If you want a judge
  verdict folded back into the dataset, edit the dataset by hand.
- It does **not** re-run the original pr-polish loop. One `bramble
  code-review` per backend per round — no resume, no R2+ follow-ups. We are
  measuring reviewer-as-classifier quality, not loop convergence.
- It does **not** auto-fix reviewer prompts. `execution_analysis` is advice
  for a human (or a follow-up skill).

## Key files

| Area | File |
|------|------|
| Replay driver (both phases) | `.claude/skills/code-review-eval/scripts/replay.py` |
| Parsing / goal / scoring lib | `.claude/skills/code-review-eval/scripts/replay_lib.py` |
| Dataset records | `~/.bramble/code-review-eval/dataset/` (outside the repo) |
| Scored results | `~/.bramble/code-review-eval/replays/` (outside the repo) |
| Dataset harvester | `.claude/skills/code-review-eval/scripts/harvest.py` (sibling skill) |
| Reviewer run logs | `~/.bramble/logs/code-review/` (klogfmt; replay copies them) |
| Session JSONL inspector | `bazel run //bramble/cmd/logview -- <file>.jsonl` |
