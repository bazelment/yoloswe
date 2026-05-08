---
name: design-doc-polish
description: Iterative review of a markdown design document. Reads the doc, proposes a tailored grilling rubric, runs N rounds of codex+cursor (optionally +gemini) against the doc with that rubric, edits between rounds, commits locally each round. Never pushes.
argument-hint: "<doc-path> [--rounds N] [--gemini] [--rubric-file <path>]"
disable-model-invocation: true
---

Helpers under `scripts/`. `doc_ops.py` handles identity + state I/O; triage uses `bramble_ops.py` from `../pr-polish/scripts/`.

Loop exits on convergence (zero findings, or all low/nit) or `--rounds N` (default `5`).

## Arguments

| Flag | Default | Meaning |
|---|---|---|
| `<doc-path>` | required | Markdown design doc, must live under the repo worktree. |
| `--rounds N` | `5` | Round cap. |
| `--gemini` | off | Add a third reviewer. |
| `--rubric-file <path>` | off | Use this rubric verbatim instead of orchestrator-proposed. User still confirms. |

## Triage rules

- Consensus (≥2 reviewers at same `(section, dimension)`): mandatory fix.
- High/medium: fix unless provably false positive.
- Low/nit: fix if trivial, else `wont_fix` with one-line reason.
- Open questions only the author can answer: drop a `> **TODO**: …` blockquote, log as `wont_fix` with `reason: "needs author input"`.

Group findings by underlying systemic issue; fix at the framing level.

## Step 0: Identify

```
python3 $SKILL_DIR/scripts/doc_ops.py identify <doc-path>
```

Returns `{doc_path, doc_path_abs, doc_slug, state_dir, state_file, ctx}`. Pin those.

```bash
export BRAMBLE_BIN="$([ -x "$(pwd)/bazel-bin/bramble/bramble_/bramble" ] \
    && echo "$(pwd)/bazel-bin/bramble/bramble_/bramble" || echo bramble)"
```

## Step 1: Rubric

The rubric questions become the `dimension` field on every issue, so they need to match the doc's actual axes. Default path: read the doc, classify it, propose 3–7 tailored questions.

Suggested axes per doc kind:

- **architecture** — long-term fit, system boundaries, scalability, alternatives
- **detailed-design** — decisions vs alternatives, contract consistency, test realism, silent-success paths, rollout risk frontloading, API evolution
- **migration** — rollback, blast radius, mid-flight failures, cutover observability
- **API contract** — versioning, compatibility, error surface, deprecation
- **RFC** — premise, what's *not* being decided, exit criteria

`AskUserQuestion` showing the proposal. Two options: `Accept` or `Use 4 starter questions`. Set `RUBRIC_SOURCE="orchestrator-proposed"` (or `user-edited` if they answer Other).

If `--rubric-file <path>` was passed: validate with `python3 $SKILL_DIR/scripts/doc_ops.py read-rubric-file <path>`, show, confirm. Set `RUBRIC_SOURCE="--rubric-file <path>"`.

Starter questions (fallback, biased toward architecture):

- "Is this the best long-term choice?"
- "Can we make it simpler?"
- "Does the milestone plan create clear boundaries?"
- "Does the milestone plan frontload risk discovery?"

Persist:

```bash
mkdir -p "$STATE_DIR"
printf '%s\n' "${RUBRIC[@]}" > "$STATE_DIR/rubric.txt"
```

## Step 2: Round loop

For round = 1..N:

### a) Snapshot pending edits

```bash
if ! git diff --quiet "$DOC_PATH"; then
    git add "$DOC_PATH" && git commit -m "design-doc-polish: round $ROUND snapshot"
fi
```

### b) Launch reviewers

`$GOAL` round 1: `"Reviewing design doc <path>"`. Round 2+: brief summary of last round's fixed/skipped, ≤500 chars.

Launch codex + cursor (+ gemini with `--gemini`) as parallel `Monitor` calls:

```
Monitor({
  description: "bramble <backend> r{ROUND}",
  timeout_ms: 720000,
  persistent: false,
  command: "cd $(pwd) && BRAMBLE_RUN_TAG=design-doc-polish:$DOC_SLUG:<backend>:r{ROUND} \
    $BRAMBLE_BIN code-review --backend <backend> --model <model> \
    --review-mode design-doc --review-rubric-file \"$STATE_DIR/rubric.txt\" \
    --verbose --timeout 10m \
    --goal \"$GOAL\" \
    --envelope-file \"$STATE_DIR/r$ROUND/<backend>-envelope.json\" \
    2>\"$STATE_DIR/r$ROUND/<backend>-stderr.txt\""
})
```

Backends/models: `codex`/`gpt-5.4-mini`, `cursor`/`composer-2`, `gemini`/`gemini-3-flash-preview`.

A missing envelope or `status: "error"` is a high-severity finding — surface it with the stderr path.

### c) Triage

```
python3 $SKILL_DIR/scripts/bramble_ops.py triage \
    --mode design-doc \
    --stream codex=$STATE_DIR/r$ROUND/codex-envelope.json \
    --stream cursor=$STATE_DIR/r$ROUND/cursor-envelope.json \
    $( [ "$USE_GEMINI" = "1" ] && echo --stream gemini=$STATE_DIR/r$ROUND/gemini-envelope.json )
```

Output buckets:
- `consensus` (≥2 sources at same key) → must_fix
- `single_critical` (single high) → must_fix
- `single_medium` → consider_fix
- `low_acks` → batch_ack

Empty action plan → converged, jump to Step 3.

### d) Apply fixes

Edit the doc. For each triaged finding, append to `$STATE_DIR/actions-r$ROUND.json`:

```json
{
  "source": "codex",
  "section": "Milestone 2: Multi-tenant rollout",
  "dimension": "q4",
  "severity": "high",
  "topic": "milestone 2 doesn't frontload risk",
  "action": "fixed",
  "reason": null,
  "commit_sha": "abc1234"
}
```

`source`: `codex` | `cursor` | `gemini` | `sweep`. `action`: `fixed` (needs `commit_sha`) | `false_positive` | `wont_fix` | `stale` (last three need `reason`). For class-level fixes touching other sections, write one `sweep` row per swept site with that site's section/dimension.

### e) Commit

```bash
git add "$DOC_PATH"
git commit -m "design-doc-polish round $ROUND: <summary>" -m "$BODY"
```

### f) Finalize

```
python3 $SKILL_DIR/scripts/doc_ops.py state-finalize-round $CTX $ROUND \
    $STATE_DIR/actions-r$ROUND.json \
    --doc-path "$DOC_PATH" --doc-path-abs "$DOC_PATH_ABS" \
    --rubric-file "$STATE_DIR/rubric.txt" \
    --rubric-source "$RUBRIC_SOURCE" \
    --envelope codex=$STATE_DIR/r$ROUND/codex-envelope.json \
    --envelope cursor=$STATE_DIR/r$ROUND/cursor-envelope.json \
    $( [ "$USE_GEMINI" = "1" ] && echo --envelope gemini=$STATE_DIR/r$ROUND/gemini-envelope.json )
```

### g) Convergence

Stop when zero findings, all low/nit, empty action plan, or `round == ROUNDS` (then `AskUserQuestion`).

## Step 3: Final summary

```
python3 $SKILL_DIR/scripts/doc_ops.py state-mark-complete $CTX <reason>
```

`<reason>`: `converged` | `all-low` | `capped-at-max` | `user-paused`.

Print:
- Top metrics + verdict (Ready / Revise / Rethink).
- Round-by-round table.
- Comment Actions table sorted by round then severity desc.

State file path is preserved; nothing was pushed.
