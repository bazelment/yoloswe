# Standing rules

Run-independent corrections that a human has had to give a swarm more than once. They
are here so they arrive at tick 1 instead of being rediscovered per run. A prompt may
override any of them; silence is not an override.

## Ownership

**You dispatch and verify. Lanes do the work.** The orchestrator drifts into running
audits, analyses and reports itself; it recurred twice in a single run and stayed fixed
only once written down as a rule. If you catch yourself doing lane work, stop and
delegate. Authoring deliverables — code, commits, PRs, review responses, report content
— is never orchestrator work.

**Measurement and shipping are different lanes.** A testbed or microbenchmark lane
produces evidence and never opens a PR; a separate lane ships the fix. That split
produced one run's best negative result: a lane retracting its own 11x "win" as a
harness artifact.

**Escalate rather than route around.** A blocked credential, a reauth wall, a
`PERMISSION_DENIED` — stop the lane and report it. Never work around it silently.

## Evidence

**Absence is unknown, never pass.** An empty API response, a cancelled job, a
zero-length log: retry, do not conclude. Two false "all green" reports came from reading
an empty result as a passing one.

**Read the count, not the exit code.** `pytest` exits 0 on a nonexistent path, which is
indistinguishable from a clean run. A green suite can also mean deleted tests — read
collected/deselected counts and the diffstat, not the pass line.

**Anything over 10 seconds is under-instrumented by definition.** Push measurement
inside it until sub-phases are visible; a gap with no marker is itself a finding.

**Show the spread before attributing anything to load.** Small batches, every sample
retained, min/median/max with n. Never a bare median, never a p90 at small n. Known
noise sources (pool autoscaling, image pulls) are ruled out with spread, not asserted.

**Never weaken an assertion to make a gate green.** No deleted assertion, no added skip,
no widened matcher, no retry-until-pass. A suite that cannot fail is worse than a red
one. A supported finding that the fix belongs elsewhere is a valid outcome.

## Integration

**Approval must be at the exact head.** Compare the approval's commit against the
current head — `approval_sha` vs `pr_head` in the ledger. A review bot can dismiss its
own approval independently of branch protection. This fired three times in one run;
merging on `reviewDecision` alone would have shipped unapproved bytes.

**Merged is not deployed, and deployed is not observed.** Name each state separately. A
fix whose consumer has not deployed is MERGED, DEPLOY-UNVERIFIED, and says so in those
words. Verify a change at the running target's revision, not at the deploy command's
exit code — a successful deploy, readiness, or a downstream endpoint can all describe
the previous revision.

**Verify integration by content, not ancestry.** Branches are squash-merged, so
`git branch -d` refuses and a two-dot diff misleads. Gate deletion on merged==true AND
the change present on the base by content.

**Do not enable auto-merge.** A PR can auto-merge while a lane is still verifying it.
The owning lane merges explicitly when the gate is satisfied.

**After integrating parallel lanes, test their affected modules together.** A clean
merge does not prove their contracts compose.

## Scope

**Three returns to the same phase forces a decision.** Ship the correct core and defer
the rest, or state why continuing is right, and record which. Sunk rounds are not an
argument for another round.

**A recurring defect class is structural.** Stop patching instances; enumerate the whole
surface, then fix once. The same P1 was filed three times at three sites before this was
noticed.

**When the task changes shape, replace the lane rather than redirecting it again.**

## Reporting

**Blockers first, and never silently absent.** Open with what needs a person: what they
must do, why an agent cannot, and what it unblocks. Order by cheapness to act on, not by
severity — one `gcloud auth login` unblocked two other items. If there are none, say so
explicitly.

**Publish on change, not on a timer.** Poll, but publish only on a real change: a merge,
an approval, a thread delta, a new red, a lane finishing.

**Link the evidence, not the artifact.** A claim that a fix landed links the commit;
"3 checks pending" links the checks tab. A bare `#number` is a defect in the report.

**Where data does not exist, print UNMEASURED** — never 0, never blank.

**A reporter that stops is invisible** — its artifact just goes stale. Verify it every
status loop; one was dead ~20h before anyone noticed.

## Driver quirks

**agy** cannot write the run directory with its native file tool — `write_to_file` is
sandboxed to its own brain directory. Write `OBJECTIVE.md` and briefs with bash
heredocs. Its `schedule` tool also degrades toward busy-waiting; hold the cadence floor
in `SKILL.md` §3 explicitly.

**codex** stops on a directory-trust dialog on every fresh worktree and sits idle
forever unless answered in the pane. It also fires "is idle" mid-turn; verify against
git state and a live pane timer.

**cursor** accepts a paste and ignores `--submit`. Count pending pastes before sending;
one lane was found holding 465.
