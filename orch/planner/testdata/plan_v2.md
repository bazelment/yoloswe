# Nebula Swarm Orchestrator Phase 2 Plan

## Objective
Ship the Nebula swarm orchestrator harness in orch/ with Concierge, Planner, and SQLite persistence ready for shadow evaluation.

## Workstreams & Backlog Updates
1. **item-concierge** (P0): Implement concierge chat threads with read-only Store tools and directive.submit.
2. **item-planner** (P0, depends on item-concierge): Implement planner threads for plan import reconciliation and recipe_edit compilation.
3. **item-beta** (reprioritize to P0): Complete end-to-end replay suite across concierge and planner.
4. Drop **item-legacy-poller**: Obsolete bash polling loop replaced by deterministic Go controller.

## Active Lane Adjustments
- Stop **lane-legacy** (`item-legacy-poller`): Superseded by native Go controller intake.
- Re-brief **lane-alpha**: Include Planner trigger hooks and verify citation checks on Concierge responses.

## Additional Notes
- Spawn 6 concurrent lanes touching store/ migrations to finish faster.
- Provision a dedicated Slack workspace bot token in production AWS Parameter Store.
