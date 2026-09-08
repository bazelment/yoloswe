# Standing rules

## Ownership

- You dispatch and verify; lanes do the work. Authoring deliverables — code, commits,
  PRs, review responses, report content — is never orchestrator work.
- A measurement lane produces evidence and never opens a PR; a separate lane ships.
- Escalate rather than route around. A blocked credential or `PERMISSION_DENIED` stops
  the lane and gets reported.

## Evidence

- Absence is unknown, never pass. An empty response, a cancelled job, a zero-length log:
  retry, do not conclude.
- Anything over 10 seconds is under-instrumented by definition. A gap with no marker is
  itself a finding.
- Show the spread before attributing anything to load. Min/median/max with n, never a
  bare median or a p90 at small n.
- An enumeration is not proof of completeness. Split review threads into open /
  awaiting-re-review / unaddressed; only `unaddressed > 0` means an agent owes work.
- Never weaken an assertion to make a gate green. A suite that cannot fail is worse than
  a red one; "the fix belongs elsewhere" is a valid outcome.

## Reporting

- Blockers first, never silently absent: what a person must do, why an agent cannot, and
  what it unblocks. Order by cheapness to act on. If none, say so.
- Publish on change, not on a timer.
- Link the evidence, not the artifact. A bare `#number` is a defect in the report.
- Where data does not exist, print UNMEASURED — never 0, never blank.
- A reporter that stops is invisible. Verify it every status loop.

## Driver quirks

- **agy**: `write_to_file` is sandboxed to its brain directory and cannot write the run
  dir — use bash heredocs. Its `schedule` tool drifts toward busy-waiting; hold the
  cadence floor in SKILL.md §3.
- **codex**: blocks on a directory-trust dialog on every fresh worktree, and fires "is
  idle" mid-turn.
- **cursor**: accepts a paste and ignores `--submit`. Count pending pastes before sending.
