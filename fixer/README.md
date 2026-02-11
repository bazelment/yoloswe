# Fixer

Automated CI failure remediation system. Fixer scans GitHub Actions for failures, triages them using an LLM, tracks issues across runs, launches Claude agents to create fix PRs, and manages the full lifecycle through merge and verification.

## Architecture

```
                    GitHub Actions API
                          |
                    [1. Fetch runs]
                          |
                    [2. Fetch logs + annotations]
                          |
                    [3. LLM Triage]  ← claude.Query() per run
                          |
                    [4. Reconcile]   ← dedup, track seen count
                          |
                    [5. Fix agents]  ← parallel Claude sessions
                          |
                    [6. Create PRs]
                          |
                    [7. Merge + Verify]
```

### Packages

- **`github/`** — GitHub Actions data access (`gh` CLI) and LLM-powered log triage
- **`engine/`** — Core orchestrator: scan, fix, merge, verify workflows
- **`issue/`** — JSON-backed issue tracker with lifecycle state machine
- **`cmd/fixer/`** — CLI commands

### LLM Triage

The scanner uses Claude (via `agent-cli-wrapper/claude.Query()`) to triage CI logs instead of regex patterns. This makes it work across any CI environment — Go, TypeScript, Python, Docker, Dependabot, etc.

For each failed workflow run, fixer makes a **single LLM call** containing:
- All failed job names
- Annotations (best-effort context from the CI system)
- The combined cleaned log (ANSI stripped, timestamps removed, truncated to 50KB)

The LLM returns a JSON array of structured failures with category, file, line, summary, and which job each error belongs to. The LLM is explicitly instructed to deduplicate errors that appear across multiple jobs in the same run.

Categories: `lint/go`, `lint/bazel`, `lint/ts`, `lint/python`, `build`, `build/docker`, `test`, `infra/dependabot`, `infra/ci`, `unknown`

### Issue Lifecycle

```
new → in_progress → fix_pending → fix_approved → fix_merged → verified
                                                      ↓
                                                   recurred → (re-enters fix cycle)
```

Issues are identified by a stable signature (`{category}:{hash}:{file}`) that survives across runs. The tracker persists to `.fixer/issues.json`.

## Usage

Build with Bazel:

```bash
bazel build //fixer/cmd/fixer
```

### Scan

Scan CI failures and categorize them:

```bash
fixer scan --repo-root /path/to/repo --branch main --limit 5 -v
```

Flags:
- `--branch` — Branch to scan (default: `main`)
- `--limit` — Number of recent failed runs to check (default: `5`)
- `--triage-model` — Claude model for triage (default: `haiku`)
- `--triage-budget` — Max USD spend on triage per scan (default: `$0.50`)

### Fix

Scan + launch parallel Claude agents to fix actionable issues:

```bash
fixer fix --branch main --model sonnet --budget 1.0 --max-parallel 3
```

Each agent creates a worktree, investigates the failure, applies a fix, and creates a PR.

### Merge

Merge approved fix PRs:

```bash
fixer merge
```

### Status

Show tracked issue status:

```bash
fixer status
fixer status --json
```

### Global Flags

- `--repo-root` — Repository worktree root (auto-detected if unset)
- `--tracker` — Path to issues.json (default: `<repo-root>/.fixer/issues.json`)
- `--dry-run` — Show what would be done without making changes
- `-v` / `--verbose` — Enable debug logging

## Cost

Triage uses Claude Haiku by default (~$0.03 per run). A typical 5-run scan costs ~$0.15. Fix agents use Sonnet by default with a configurable per-agent budget.

## Future Improvements

- **Smarter dedup across runs**: The LLM can produce slightly different summaries for the same underlying error across runs. A fuzzy signature or embedding-based matching would reduce duplicate issues.
- **Root cause grouping**: Multiple lint errors in the same file often share a root cause (e.g., a missing dependency). Grouping related failures into a single issue would reduce noise and let fix agents address the root cause.
- **Streaming triage**: Use `claude.QueryStream()` for real-time progress feedback during long scans.
- **PR review feedback loop**: When a fix PR gets review comments, feed them back to the agent for a follow-up attempt.
- **Selective re-scan**: Only re-triage runs that are newer than the last scan, avoiding redundant LLM calls on already-seen runs.
- **Budget-aware model selection**: Auto-downgrade to a cheaper model when budget is running low, or skip triage for runs that look similar to already-triaged ones.
- **Fix verification**: After a fix PR is merged, automatically re-run the specific failing CI job to verify the fix instead of waiting for the next full CI run.
