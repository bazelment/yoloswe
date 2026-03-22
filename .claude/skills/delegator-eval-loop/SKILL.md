---
name: delegator-eval-loop
description: "Fully autonomous eval loop for the delegator agent. Runs real-mode tests, analyzes output for usability gaps, spawns fix agents, rebuilds, and re-tests until clean.
disable-model-invocation: true
---

# Delegator Eval Loop

Autonomous test→diagnose→fix cycle for the delegator agent. **Max 5 rounds.** Exit early when all gaps are closed.

## Loop

1. **Build:** `bazel build //bramble:bramble`
2. **Read context:** `docs/design/delegator-real-mode-testing.md` and `bramble/cmd/delegator/delegator.go`
3. **Check prior runs:** Read `.claude/skills/delegator-eval-loop/data/eval-runs.log` for patterns from past iterations
4. **Run tests** — choose the right harness for the eval type:

   **Codewalk eval** (multi-turn, questions from file):
   ```bash
   python3 scripts/delegator-eval.py \
     --questions-file scripts/delegator-eval-questions.txt \
     --work-dir /path/to/repo \
     --model sonnet --child-model gemini-3-flash-preview \
     --log-dir "$LOG_DIR" --timeout 900 2>"$LOG_DIR/stderr.txt"
   ```
   The script drives interactive mode via PTY + `--status-fd` pipe. It sends
   questions one per idle, waits for children to complete, then sends `quit`.
   Edit `scripts/delegator-eval-questions.txt` to change the question set.

   **Single-prompt eval** (one-shot task, non-interactive):
   ```bash
   echo "<PROMPT>" | bazel-bin/bramble/bramble_/bramble delegator \
     --mode real --work-dir "$TEST_DIR" --model sonnet \
     --log-dir "$LOG_DIR" --timeout 8m 2>"$LOG_DIR/stderr.txt"
   ```
   Each single-prompt test needs a fresh `git init`'d directory with a minimal
   `go.mod` + `main.go`.

   Run sequentially (real API calls). Use `run_in_background` + `timeout=600000`
   so you can analyze completed results while the next runs.
5. **Analyze** — read `.claude/skills/delegator-eval-loop/references/checklist.md` and check every item against both rendered output and JSONL logs. Check `.claude/skills/delegator-eval-loop/references/known-gaps.md` for regressions.
6. **Exit check** — zero high/medium findings, tool restriction correct, no regressions, costs reasonable → exit to summary
7. **Fix** — spawn Plan agents for diagnosis, then worker agents (sonnet) for implementation. Fix root causes, not symptoms.
8. **Quality gates** (must pass before re-testing):
   ```bash
   scripts/lint.sh
   bazel test //... --test_timeout=60
   bazel build //bramble:bramble
   ```
9. **Commit**, then go to step 4 with the rebuilt binary and fresh test directories.

## Final Summary

Always produce a summary table (rounds, findings fixed, remaining gaps, verdict). After a successful loop, add a "Test Run N" section to `docs/design/delegator-real-mode-testing.md`. Append results to `.claude/skills/delegator-eval-loop/data/eval-runs.log`.

## Gotchas

1. **JSONL is truth, not rendered output.** The JSONL session logs are ground truth for tool counts, session lifecycle, and costs. Rendered output can mask issues (e.g., a tool might execute but not display).

2. **Text fragmentation is subtle.** Streaming `TextEvent` chunks have arbitrary boundaries. Look for concatenated words like `"TheI"` or `"GreatHere"` — these indicate the render layer isn't buffering at word boundaries. See CLAUDE.md's "Streaming Text Event Pipeline" section.

3. **Fix root causes, not symptoms.** If the delegator calls ToolSearch, don't filter it from display — fix why it's available. If stderr is noisy, fix log levels at the source, don't redirect.

4. **The 8-minute timeout is real.** Complex prompts can hit it. If a test times out, check whether the child session hung (JSONL will show no `end_turn` after the last message) vs. the task was genuinely too large.

5. **Fresh directories for single-prompt evals.** Never reuse test directories between single-prompt tests. Git state from a prior run will confuse the delegator's child sessions. Codewalk evals use the real repo as work-dir (read-only codetalk sessions).

6. **Tests must run sequentially.** Each test makes real Claude API calls. Run one at a time, but use `run_in_background` + `timeout=600000` so you can analyze completed results while the next runs.

7. **Capture stderr separately.** The delegator's stderr contains log output that won't appear in stdout or JSONL. Always use `2>"$LOG_DIR/stderr.txt"` and check it for unexpected warnings.

8. **Model name format matters for non-Claude children.** Child sessions inherit the model from delegator config. Verify via JSONL `init` messages that children got the expected model, not a default.

## Key Files

| Area | Files |
|------|-------|
| Harness & rendering | `bramble/cmd/delegator/delegator.go` |
| Multi-turn eval | `scripts/delegator-eval.py` + `scripts/delegator-eval-questions.txt` |
| Session setup | `bramble/session/delegator_runner.go` |
| Tool handlers | `bramble/session/delegator_tools.go` |
| Session lifecycle | `bramble/session/manager.go` |
| SDK options | `agent-cli-wrapper/claude/session_options.go` |
| CLI arg building | `agent-cli-wrapper/claude/process.go` |
| Design doc | `docs/design/delegator-real-mode-testing.md` |
| Analysis checklist | `.claude/skills/delegator-eval-loop/references/checklist.md` |
| Known gaps | `.claude/skills/delegator-eval-loop/references/known-gaps.md` |
| Run history | `.claude/skills/delegator-eval-loop/data/eval-runs.log` |
