# Prdozer

## Context

Once a PR is opened, keeping it merge-ready is a manual chore: rebase when
`main` moves, address bot/reviewer comments, fix CI failures, push, repeat.
The `/pr-polish` skill already automates one round of that loop — but it has
to be invoked by hand and doesn't watch in the background.

`prdozer` is the watcher around `/pr-polish`. It polls one or more PRs at a
configured cadence, detects when work is needed, and hands the PR to the
skill. The Go side is orchestration only; the actual fixing is delegated.

This is the PR-driven sibling to `jiradozer` (issue-driven). They share the
same shape: poll an external system → spawn agents → react to feedback.

## Goals & Non-Goals

**Goals.**

- Watch a single PR, an explicit list, or all open PRs in the current repo.
- Re-check at a configured interval (default 30m).
- Detect three triggers per tick: base moved, CI failed, new review comments.
- Bring each PR to a merge-ready state by invoking `/pr-polish`, then idle.
- Optional auto-merge when a PR is approved + green + base unchanged.
- Same look-and-feel as `jiradozer` — `render.Renderer` + klogfmt + slog.

**Non-goals (v1).**

- Reimplementing rebase/conflict-resolution/CI-fix logic in Go. The skill
  already does it.
- Multi-repo orchestration in one process. Run one prdozer per repo.
- Cross-PR coordination (e.g. stacking, dependency graphs). Each PR is
  independent.
- A daemon/service wrapper. `--once` is the friendly mode for cron/systemd;
  the long-running mode is just `--once` in a loop.

## Workflow

```
   ┌─────────────┐
   │ Discover PRs │   gh pr list  /  gh pr view N
   └──────┬───────┘
          │ for each PR (≤ source.max_concurrent in parallel)
          ▼
   ┌──────────────┐
   │   Snapshot   │   pr view + statusCheckRollup + run list +
   │              │   comments (inline & issue) + base SHA
   └──────┬───────┘
          ▼
   ┌──────────────┐
   │  Changeset   │   compare to last-tick state:
   │              │   BaseMoved? CIFailed? NewComments?
   │              │   Mergeable? PRClosed?
   └──────┬───────┘
          ▼
   ┌──────────────────────────────────────────────────┐
   │ Decide:                                           │
   │   PRClosed       → Merged (record, stop)         │
   │   Mergeable+AM   → gh pr merge                   │
   │   Mergeable      → Idle                           │
   │   No change      → Idle                           │
   │   dryRun         → Log "would polish"             │
   │   else           → polish.Run(/pr-polish [N])     │
   └──────┬───────────────────────────────────────────┘
          ▼
   ┌──────────────┐
   │  Save state  │   last-seen SHAs, comment IDs,
   │              │   run IDs, action, failure count
   └──────────────┘
          │
          ▼  sleep poll_interval, then re-discover & repeat
```

## Why Standalone, Not a Bramble Subcommand

- Long-running watcher: belongs in its own process so it can be supervised
  (systemd, tmux, daemonized) without entangling with bramble's TUI lifecycle.
- Same shape as jiradozer; sharing the structure makes both easier to reason
  about.
- Bramble's subcommands today are session-coupled (TUI client ops); a poller
  doesn't fit that model.

## Why a Thin Go Orchestrator + Existing Skill

`/pr-polish` already implements rebase, CI wait, review-comment fetching
(`github-inline`/`github-issue`/`github-review`), bramble code-review, the
fix loop, its own state file, and convergence detection. Re-implementing any
of that in Go would be churn. Go's job is just: detect when work is needed,
set up the worktree, invoke the skill, report status. Same split as
jiradozer (Go orchestrates; Claude does the work).

## Package Structure

```
prdozer/
  cmd/prdozer/
    main.go           # cobra entry: flags, logging setup, orchestrator wiring
    BUILD.bazel
  state.go            # per-PR JSON state file (LoadState, Save, StatePath)
  config.go           # YAML config + defaults + validation
  snapshot.go         # TakeSnapshot: pr view + rollup + runs + comments + base
  changeset.go        # ComputeChangeset: diff snapshot vs state → flags
  agent.go            # PolishRunner interface + AgentPolisher (multiagent/agent)
  watcher.go          # Per-PR Tick loop + decision tree + back-off
  discovery.go        # DiscoverPRs (single/list/all modes)
  orchestrator.go     # Multi-PR fan-out with concurrency cap
  prdozer.example.yaml
  README.md
  docs/design.md      # this document
  BUILD.bazel
```

Standalone Go module under `go.work`; depends on `agent-cli-wrapper`,
`logging`, `multiagent`, `wt`. No bramble dependency.

## Key Components

### `Snapshot` (snapshot.go)

A point-in-time view of a PR:

- `PRDetails` — number, head/base names, head SHA, state, draft, review
  decision, mergeable.
- `BaseSHA` — `gh api repos/{o}/{r}/git/refs/heads/<base>` (best-effort;
  empty on error so the BaseMoved signal is suppressed rather than crashing).
- `StatusRollup` — coarse `SUCCESS` / `FAILURE` / `PENDING` from
  `statusCheckRollup`. Detail is fetched downstream by the skill.
- `FailedRunIDs` — `gh run list --status failure` databaseIds.
- `Comments` — both inline (`pulls/{n}/comments`) and issue
  (`issues/{n}/comments`), tagged with `IsBot` (User.Type == "Bot" or
  `[bot]` suffix) and `IsSelf` (matches the running login).

### `Changeset` (changeset.go)

Boolean flags derived from comparing a fresh `Snapshot` to the persisted
`State`:

- `BaseMoved` — base SHA differs from last seen.
- `HeadMoved` — informational; not a polish trigger on its own.
- `CIFailed` — `StatusRollup == "FAILURE"` or any unseen failed run ID.
- `NewComments` — comment IDs not in `LastSeenCommentIDs`, filtering out
  comments authored by `self` (the running GitHub login).
- `Mergeable` — review approved + checks green + base unchanged.
  Suppressed if `BaseMoved` or `CIFailed` so we never auto-merge a PR whose
  base moved out from under us.
- `PRClosed` — terminal; we record once and stop poking.

`NeedsPolish()` is `BaseMoved || CIFailed || len(NewComments) > 0`.

#### First-tick semantics

When `state.LastCheckAt.IsZero()`, this is the first time we've seen the PR.
Comments and head SHA we treat as "already known" — we don't want to fire on
historical state. But `CIFailed` from `StatusRollup == "FAILURE"` is
actionable on first run, since the PR was failing before we showed up and
that's exactly what we exist to fix.

### `Watcher.Tick` (watcher.go)

The single-PR control loop. Each tick:

1. Load state from `~/.bramble/projects/<repo>-<pr>/prdozer-state.json`.
2. If `CooldownUntil` is in the future, log and return idle (back-off).
3. `TakeSnapshot` and `ComputeChangeset`.
4. Decide:
   - `PRClosed` → return `LastActionMerged`.
   - `Mergeable && AutoMerge && !dryRun` → `gh pr merge --squash`.
   - `Mergeable` → idle.
   - `!NeedsPolish()` → idle.
   - `dryRun` → log "would polish".
   - else → `polish.Run(req)`.
5. `recordSnapshot` updates last-seen SHAs, dedup-merges comment & run IDs,
   bumps `ConsecutiveFailures` on failure, trips `CooldownUntil` after
   `Backoff.MaxConsecutiveFailures` reached.
6. Save state.

### `AgentPolisher` (agent.go)

Wraps `multiagent/agent` to invoke `/pr-polish [--local] <PR#>`:

```go
provider, _ := agent.NewProviderForModel(model)
provider.Execute(ctx, prompt, nil,
    agent.WithProviderWorkDir(req.WorkDir),
    agent.WithProviderPermissionMode("bypass"),
    agent.WithProviderModel(req.Model),
    agent.WithProviderKeepUserSettings(),     // skill discovery needs this
    agent.WithProviderEventHandler(handler),
    agent.WithProviderMaxTurns(req.Cfg.MaxTurns),
    agent.WithProviderMaxBudgetUSD(req.Cfg.MaxBudgetUSD),
)
```

`WithProviderKeepUserSettings` is non-obvious but required: without it, the
spawned agent loses access to user-level skills, so `/pr-polish` resolves to
nothing.

`PolishRunner` is an interface so unit tests substitute a `stubPolish`
recorder. `AgentPolisher` is the only production implementation.

#### Event handlers

Two handlers, composed via `compositeHandler`:

- `polishLogHandler` — drains agent events to `slog` at `Debug` (text,
  thinking, tools, turns) / `Info` (session init, retries). Buffers text
  and flushes on newline or 200-char threshold to avoid one-line-per-token
  log spam.
- `rendererHandler` — streams the same events into `render.Renderer` for
  live terminal output, the way jiradozer does.

### `Orchestrator` (orchestrator.go)

Fans out to per-PR `Watcher.Tick` calls with a buffered-channel semaphore
sized by `source.max_concurrent`. `RunOnce` returns per-PR `TickResult`s for
test inspection; `Run` loops on `cfg.poll_interval`. Both re-run discovery
each cycle so newly-opened PRs get picked up without restart.

### `DiscoverPRs` (discovery.go)

Three modes:

- `single` / `list` — `gh pr view <n> --json …` per number.
- `all` — `gh pr list --state open --json … --limit 200` with
  `--author` and `--label` from the filter; `exclude_labels` applied
  client-side after parsing.

Default filter: `author:@me` plus exclude labels `["wip", "do-not-watch"]`,
so prdozer never accidentally polishes other people's PRs or work-in-progress.

## State File

Per PR: `~/.bramble/projects/<repo>-<pr>/prdozer-state.json`.

```json
{
  "last_check_at":         "2026-04-19T05:33:55Z",
  "last_seen_head_sha":    "a679f22…",
  "last_seen_base_sha":    "0ea3c28…",
  "last_seen_comment_ids": ["123", "456", …],
  "last_seen_ci_run_ids":  [987654, …],
  "last_action":           "polished" | "idle" | "merged" | "failed" | "dry_run",
  "consecutive_failures":  0,
  "cooldown_until":        "0001-01-01T00:00:00Z",
  "pr_number":             1234,
  "repo":                  "yoloswe"
}
```

This file lives next to the `pr-polish-state.json` written by the skill
itself, in the same project directory. Two files, two owners — they don't
collide. prdozer's view: "what did I observe last tick"; the skill's view:
"what did the polish loop do".

## Configuration

YAML at `prdozer.yaml` (see `prdozer.example.yaml` for every field).
The defining choices:

- `poll_interval: 30m` — long enough that 100 PRs poll without rate-limit
  pressure; short enough that base-moves don't go unnoticed for a workday.
- `source.mode: all` + `filter.author: "@me"` — opt-in scope by default.
- `polish.local: false` — wait for CI; `--local` skips that wait, useful
  for fast local iteration.
- `polish.auto_merge: false` — explicit opt-in. Surprising-action risk is
  high (a PR could go in while the author isn't watching).
- `backoff.max_consecutive_failures: 3`, `backoff.cooldown: 2h` — three
  failures in a row trips a 2-hour cool-down for that PR. Other PRs are
  unaffected; re-poll resumes naturally.

A config file is optional. With no file, defaults apply and CLI flags take
over. CLI precedence: flags beat config beats defaults.

## CLI Surface

```
prdozer [--config path] [flags]
  --pr <num>[,<num>…]       watch specific PR(s); skips discovery
  --all                     watch all PRs matching source.filter
  --once                    one tick per PR then exit (for cron / systemd)
  --dry-run                 detect changes, log decisions, don't invoke agent
  --local                   pass --local to /pr-polish
  --auto-merge              merge PRs that become mergeable
  --poll-interval <dur>     override config poll_interval
  --model <id>              override agent model
  --max-budget <usd>        override per-polish budget
  --work-dir <path>         working directory (default: cwd)
  --repo <name>             short repo name for state-file path (default: derive from cwd)
  --verbosity quiet|normal|verbose|debug
  --color auto|always|never
  -v / --verbose            shorthand for --verbosity=verbose
```

`--once` matters: it lets the same binary run as a long-lived poller OR be
driven by an external scheduler without keeping a process alive.

## Logging & Console Output

Mirrors `jiradozer` exactly so the two tools feel identical:

- **User-facing status** → `render.Renderer` to stderr. Per-PR ticks
  ("Discovered N PR(s)", "PR #X polishing", "PR #X is mergeable — idle")
  and agent stream output.
- **Structured logs** → `slog` via `klogfmt`, written to
  `~/.prdozer/logs/prdozer-<ts>-<pid>.log` at `Debug`. Stderr gets `Info`+
  (or `Debug`+ at `--verbosity=debug`).
- **Verbosity flags** identical to jiradozer:
  `render.ParseVerbosity` / `render.ParseColorMode`.

## Concurrency

- Within one prdozer process: `source.max_concurrent` PRs ticked in
  parallel via a buffered-channel semaphore. Default 3.
- Across PRs: state files are per-PR, so writes never contend.
- Within one PR: a single goroutine. Two prdozer processes watching the
  same PR would race on the state file; assume the user runs one process
  per repo.

## Testing Strategy

Two-tier, same shape as jiradozer:

**Unit (`prdozer/*_test.go`).**

- `changeset_test.go` — table-driven snapshot pairs → expected flags. No I/O.
- `watcher_test.go` — `fakeGH` (prefix-matched stdout map) + `stubPolish`
  drives the per-PR loop through first-run-idle, base-moved-polish, dry-run,
  cooldown-after-N-failures, mergeable-no-auto-merge.
- `orchestrator_test.go` — concurrency cap with a `concurrencyTrackingPolish`
  that records peak in-flight; exclude-label discovery filtering.
- `state_test.go` — JSON round-trip.
- `config_test.go` — defaults, YAML override, invalid mode rejection.

**Integration (future, `prdozer/integration/` with `# gazelle:ignore`).**

- `--once --dry-run` against a real PR; assert correct classification.
- One end-to-end with `/pr-polish` on a tiny scripted-failure PR (real
  Claude CLI via existing OAuth, no API key).

## Trade-offs

- **Skill vs native rebase code.** All-in on `/pr-polish` for v1. Slower
  per-tick than a Go fast-path (Claude startup cost on every action), but
  one source of truth for the fix loop. Revisit if tick latency becomes the
  bottleneck.
- **Auto-merge default off.** Surprising-action risk is high. Make the
  user opt in per-run or per-config.
- **`--all` scoped to `@me`.** prdozer should not silently start polishing
  other people's PRs. Broader scopes are a config opt-in.
- **Single-process per repo.** No cross-process locking on state files.
  If two prdozer processes target the same PR, the last writer wins; a
  rare misordering of state is acceptable since the next tick re-derives
  from gh.

## Future Work

- Worktree manager integration: for `--all` mode, look up the PR's local
  worktree by branch (`git worktree list --porcelain`) and create one via
  `wt.WorktreeManager.NewWorktree` if missing. Today, prdozer assumes the
  user runs from the PR's worktree.
- Multi-repo: today one prdozer per repo. A multi-repo orchestrator is
  natural future work — it'd just be a `cfg.repos: []` list and a
  per-repo `Orchestrator`.
- Failure introspection: when polish fails, surface *why* (CI bot output,
  unresolved conflicts) in the status line instead of just "polish failed:
  <error>".
