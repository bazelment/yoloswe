# Why /pr-polish defers push until loop exit

Every push to a PR's branch triggers configured GitHub bots (CodeRabbit, Cursor Bugbot, etc.) to re-review. Mid-loop pushes burn bot budget on intermediate commits and reliably generate new comments on round-N-fix diffs — even when the fix was correct.

Batching has two payoffs:

1. Bots see the final, polished tree once instead of N intermediate trees, so the comment stream represents what's actually being merged.
2. CI runs once on the final state instead of N times on transient WIP, freeing runner capacity for other PRs.

The trade-off is recoverability: if the loop crashes mid-run, local commits exist but no remote backup. The state file at `~/.bramble/projects/<repo>-<pr>/pr-polish-state.json` captures the per-round commit SHAs so the next invocation can pick up where it left off; full audit trail survives even when push never happens.

This is a deliberate inversion of the "push early, push often" default: pr-polish optimizes for bot signal-to-noise, not git resilience, because the loop is short (minutes, not days) and the work is reproducible from the same review inputs.
