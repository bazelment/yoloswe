---
name: conductor-swarm
description: Orchestrate parallel coding lanes in Conductor cloud workspaces with durable goal tracking, phase gates, and remote session supervision. Use for a requested Conductor swarm or multi-lane Conductor run.
---

# Conductor swarm

Own the goal, work graph, dispatch, verification, and integration. Conductor lanes run in separate cloud workspaces on separate machines. Brief each lane as an independent agent; never assume it can see the orchestrator's files, another lane's files, or local tmux sessions.

## Run contract

Take the goal, terminal proof, repository, models, concurrency, lifecycle, authority boundaries, and approval gates from the invocation. Choose and record a modest concurrency cap if none is supplied. Do not broaden permission for pushes, PRs, merges, deployments, or notifications. If no lifecycle is given, use `swe -> clean -> review -> integrate` as described in [references/pr-lane.md](references/pr-lane.md). A prompt can replace or skip any phase.

Each lane has one task, one Conductor workspace and branch, dependencies, priority (`p0` blocks proof or several lanes; `p1` removes major uncertainty; `p2` is remaining ready work), phase, and status (`planned/running/done/blocked/failed`). Reuse its workspace across phases; start a fresh **session in that workspace** for cleanup and review. Different lanes get different workspaces. Do not put independent lanes in different sessions of one workspace: those sessions share its checkout.

Before creating a workspace, read [references/conductor-api.md](references/conductor-api.md). Require `CONDUCTOR_API_KEY` without printing it. If it is absent, ask the user to create a key at [Conductor API keys](https://app.conductor.build/home/api-keys) and provide it through their environment or secret store. Resolve the exact project and source branch, and check the current API schema for valid agent/model combinations. If the project is unavailable, report the precise blocker; do not substitute a different repository. Creating cloud workspaces consumes resources, so spawn only lanes justified by the run contract and keep within its concurrency limit.

## Durable state and dispatch

Create a local run directory outside the repository, such as `~/.local/state/conductor-swarm/<timestamp>/`. Keep `OBJECTIVE.md` as the recovery page and `lanes.json` as the lane ledger. Initialize and update the ledger with `scripts/ledger.py`; run `show` or `ready` after compaction instead of rebuilding state from memory. Record the goal, terminal proof and uncovered dimensions, context links, permissions, phases, concurrency, due work, next action, and for each lane: project ID, workspace ID, session ID per phase, deep link, branch, last transcript message ID, observed `working` state, artifact/PR URL, reviewed commit, and verification result. Never put API keys or other secrets in these files.

```bash
python3 scripts/ledger.py init "$RUN" --goal "$GOAL" --proof "$PROOF" --phases swe,clean,review,integrate --concurrency 3
python3 scripts/ledger.py add "$RUN" --id lane-a --title "Scoped task" --project-id "$PROJECT_ID" --branch "$SOURCE_BRANCH" --priority p1
python3 scripts/ledger.py ready "$RUN"
```

Turn every proof gap into a briefable lane or record why it cannot yet run. Dispatch dependency-ready lanes by priority and refill slots as they free. Give each lane a self-contained brief with its mission, source branch and shared interface decisions, owned scope, sibling exclusions, required proof, authorized external operations, and a request to report commit SHA, clean/dirty state, test results, and artifact/PR URL. Local run-directory paths are **not** report channels for remote lanes. Store the brief locally; deliver its contents via the Conductor API.

Record the `workspaceId`, `sessionId`, and `deepLink` returned by creation before sending any further requests. The workspace creation request can include the first brief as `message`; if it does, record `initialMessage.messageId` too. Do not create a duplicate initial prompt. Have the lane report its initial `git rev-parse HEAD` before implementation and compare it with the intended reachable source commit; investigate a mismatch before accepting its change. For later phases, create a new session in the same workspace with an explicit agent and model, then send its self-contained phase brief. If another lane depends on a commit, give it a reachable ref or PR and the exact commit SHA; a local branch name or file path on the first lane's machine is insufficient.

## Supervision loop

After dispatch, check each active workspace and session through the API. Read new transcript messages using `after=<last message id>` and update the cursor only after processing them. A newly queued prompt can show `idle`: do not call it finished until `working` has been observed and then returns to `idle`, or a completed reply is visible in the transcript. Treat `error`, missing replies, questions, and stalled progress as decisions to investigate. Reply in the same session to clarify or steer; do not stack speculative duplicate prompts. Use a finite supervision cadence appropriate to the run, typically one to two minutes, and avoid busy polling. Persist state before a long wait or handoff.

The transcript is the report channel, not proof by itself. Before advancing a phase, verify its requested artifact and gate: inspect the PR/remote branch and commit when available, check the claimed tests and review result, and compare the reviewed SHA with the current PR head before integration. When no independently reachable artifact exists, ask the lane for concrete command output and mark the boundary of that verification. A stopped or idle agent with no verified result is not `done`. A major review finding returns the lane to `swe`; record the finding and use a fresh session in the same workspace if needed.

On each pass: reconcile API state and transcripts with the ledger; verify phase claims; advance, rework, or mark blocked; reassess uncovered proof; dispatch ready lanes; then update `OBJECTIVE.md` and `lanes.json`. Record the next check time in `OBJECTIVE.md`. If the host supports scheduled continuation, arm one reminder with the run path and recovery commands; otherwise keep supervising during the active turn and report when supervision cannot continue automatically. After compaction, read `OBJECTIVE.md`, run `scripts/ledger.py show "$RUN"`, then query recorded workspace and session IDs before changing anything.

Do not use local worktree, pane, process, or file watchers as evidence of a Conductor lane. Do not delete, sleep, or archive a workspace merely because a session is idle: first account for uncommitted work and any required handoff. Cleanup is bounded by the run's retention intent and verified by workspace/session API responses.

Stop when terminal proof and required integration or approval gates hold, all material goal dimensions are evidenced or explicitly removed by the user, every lane is terminal, and retained/cleaned workspaces are recorded. Report outcomes, verified evidence, blockers with unblock conditions, follow-ups, PRs, Conductor deep links, and the ledger path.
