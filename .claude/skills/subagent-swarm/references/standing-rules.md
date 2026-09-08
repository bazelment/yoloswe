# Standing rules

Corrections a human has had to give a swarm more than once. A prompt may override any of
them; silence is not an override.

## Ownership

- You dispatch and verify; lanes do the work. Authoring deliverables — code, commits,
  PRs, review responses, report content — is never orchestrator work.
- A measurement lane produces evidence and never opens a PR; a separate lane ships.
- Escalate rather than route around. A blocked credential or `PERMISSION_DENIED` stops
  the lane and gets reported.

## Evidence

- Absence is unknown, never pass. An empty response, a cancelled job, a zero-length log:
  retry, do not conclude.
- Read the count, not the exit code. `pytest` exits 0 on a nonexistent path, and a green
  suite can mean deleted tests — check collected/deselected and the diffstat.
- Anything over 10 seconds is under-instrumented by definition. A gap with no marker is
  itself a finding.
- Show the spread before attributing anything to load. Min/median/max with n, never a
  bare median or a p90 at small n.
- An enumeration is not proof of completeness. Split review threads into open /
  awaiting-re-review / unaddressed; only `unaddressed > 0` means an agent owes work.
- Never weaken an assertion to make a gate green. A suite that cannot fail is worse than
  a red one; "the fix belongs elsewhere" is a valid outcome.

## Integration

- Approval must be at the exact head: `approval_sha == pr_head`, with either missing
  meaning stale. `reviewDecision: APPROVED` alone has shipped unapproved bytes.
- Merged is not deployed; deployed is not observed. Name each state separately.
- Verify integration by content, not ancestry — squash-merging makes ancestry lie.
- No auto-merge. The owning lane merges explicitly when the gate is satisfied.
- After integrating parallel lanes, test their affected modules together.

## Scope

- Three returns to the same phase forces a decision: ship the correct core and defer, or
  say why continuing is right. Record which.
- A recurring defect class is structural. Enumerate the surface and fix once.
- When the task changes shape, replace the lane rather than redirecting it again.

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
