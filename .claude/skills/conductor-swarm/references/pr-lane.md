# Default PR lane

Use only when the invocation supplies no lifecycle. Its models, tools, skips, approvals, and authority boundaries take precedence.

| Phase | Remote session | Exit evidence |
|---|---|---|
| `swe` | Initial session in a new workspace | Scoped committed change and requested proof; reachable branch or PR if authorized. |
| `clean` | Fresh session in the same workspace | Simplified lane diff, committed, with new SHA and status. |
| `review` | Fresh session in the same workspace | Review against the lane's fork commit and current HEAD, with findings or a clear verdict. |
| `integrate` | Orchestrator, unless assigned otherwise | Authorized PR, merge, or handoff gate satisfied and verified. |

The same workspace preserves branch and working files across phase sessions. A fresh session has no guaranteed conversational context, but it can inspect its checkout. Brief from the current artifact, not a narrative of earlier sessions. For example:

| Phase | Concise brief shape |
|---|---|
| `swe` | "Change the root README Nx alias to use the installed workspace binary. Commit the README-only diff and report SHA and checks." |
| `clean` | "Inspect `FORK_SHA..HEAD` for unnecessary changes. Commit only if you simplify it; report HEAD and status." |
| `review` | "Review `FORK_SHA..HEAD` for correctness and scope. Report findings or clear, with reviewed HEAD." |

Add a pinned SHA, cross-lane interface, or authority boundary only when it changes the decision. Do not repeat the agent's identity, tell it to read repository instructions, or prescribe routine validation commands.

A major finding goes back to `swe` with the finding recorded. If the task changes scope or ownership, create a new lane. Recheck the current remote head after review; a verdict on an older SHA is stale. After integrating parallel lanes, test affected modules together to prove their contracts compose.

Pushing, PR creation, merging, and live-system changes happen only under invocation authority. If a gate cannot be verified without a reachable remote artifact, record that limitation and request an appropriate handoff rather than asserting success.
