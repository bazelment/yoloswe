# Default PR lane

Use this lifecycle when the invocation supplies none. Use its model for each phase without an explicit model override. The invocation's tools, skips, approvals, and authority boundaries take precedence. Resolve the current Conductor API model IDs before dispatch.

| Phase | Session default |
|---|---|
| `swe` | Claude Code, Sonnet 5.5; initial session in a new workspace |
| `clean` | Claude Code, Sonnet 5.5; fresh session in the same workspace; run `/simplify` on the current commit |
| `review` | Claude Code, Opus 5.5; fresh session in the same workspace; run `/review` |
| `integrate` | Orchestrator, unless assigned otherwise |

The same workspace preserves branch and working files across phase sessions. A fresh session has no guaranteed conversational context, but it can inspect its checkout. Brief from the current artifact, not a narrative of earlier sessions. For example:

| Phase | Concise brief shape |
|---|---|
| `swe` | "Change the root README Nx alias to `pnpm exec nx`." |
| `clean` | "`/simplify`" |
| `review` | "`/review`" |

Start the actual clean and review briefs with `/simplify` and `/review` so Claude Code invokes those commands. Add a pinned SHA, cross-lane interface, or authority boundary only when it changes the decision. Do not repeat the agent's identity, tell it to read repository instructions, or prescribe routine validation commands.

A major finding goes back to `swe` with the finding recorded. If the task changes scope or ownership, create a new lane. Recheck the current remote head after review; a verdict on an older SHA is stale. After integrating parallel lanes, test affected modules together to prove their contracts compose.

Pushing, PR creation, merging, and live-system changes happen only under invocation authority. If a gate cannot be verified without a reachable remote artifact, record that limitation and request an appropriate handoff rather than asserting success.
