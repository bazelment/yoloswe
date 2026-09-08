# Default PR lane

Use this lifecycle only when the invocation does not define another one. Prompt-supplied
models, effort, tools, permissions, skips, gates, and approvals always win.

| Phase | Session | Exit evidence | Next |
|---|---|---|---|
| `swe` | implementation agent | committed scoped change plus requested proof | `clean` |
| `clean` | fresh agent, same worktree | lane-owned diff cleaned and committed | `review` |
| `review` | fresh reviewer | requested review gate reports major or clear | `swe` or `integrate` |
| `integrate` | orchestrator | authorized PR/merge/handoff gate satisfied | done |

Skip or replace phases when the prompt says so. A major review finding re-enters `swe`
on the same lane and records the finding in the ledger. Replace the lane when repeated
redirects have changed the task's scope or ownership.

## Brief contract

Give each phase only what it cannot derive:

- the mission and smallest useful proof boundary;
- non-derivable context and settled decisions;
- owned files or mutable state, sibling exclusions, and dependencies;
- operations reserved for the orchestrator;
- the phase gate and literal notes/`.done` paths.

Do not paste generic coding, testing, or review advice. If the prompt names a command or
skill, pass it as requested rather than expanding it into a different workflow.
When a guard refuses input, require the lane to establish which side is wrong before
coding. Never weaken an assertion or fabricate data to make a gate green; a supported
finding that the fix belongs elsewhere is a valid phase outcome.

## Mechanics

- Reuse the lane's worktree and branch across phases, but use a fresh cleanup and review
  session so each pass arrives cold.
- Scope cleanup and review to `FORK_SHA..HEAD`, not the remote base containing siblings.
- Record the reviewed HEAD; verify it again before integration. The gate is
  `approval_sha == pr_head` in the ledger, not `reviewDecision: APPROVED` — an approval
  pinned to an older commit is not an approval for what would merge, and a missing
  `pr_head` or `approval_sha` is stale by default. `ledger.py doctor` reports both.
- After integrating parallel lanes, test their affected modules together; a clean merge
  does not prove their contracts compose.
- A phase that changes the branch commits before writing `.done`, and writes `.done`
  last. The orchestrator verifies both.
- PR creation, pushing, merging, live-system mutation, and human notification happen only
  when the invocation authorizes them and assigns the responsible phase.
