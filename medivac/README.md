# Medivac

Automated CI failure remediation system. Medivac scans GitHub Actions for failures, triages them using an LLM, tracks issues across runs, groups related failures, launches Claude agents to create fix PRs, and manages the full lifecycle through merge and verification.

## Architecture

```
                    CI Provider (GitHub Actions)
                          |
                    [1. Fetch runs]         ← Scanner interface
                          |
                    [2. Fetch logs + annotations]
                          |
                    [3. LLM Batch Triage]   ← single call for all runs
                          |
                    [4. Reconcile]          ← dedup, track seen count, skip reviewed runs
                          |
                    [5. Group issues]       ← by error code, package, or root cause
                          |
                    [6. Fix agents]         ← parallel Claude sessions via AgentSession
                          |
                    [7. Create PRs]         ← one PR per group
                          |
                    [8. Merge + Verify]
```

### Packages

- **`github/`** — GitHub Actions data access (`gh` CLI) and LLM-powered log triage. Implements the `Scanner` interface.
- **`engine/`** — Core orchestrator: scan, fix, merge, verify workflows. Defines `Scanner` and `AgentSession` interfaces for testability.
- **`issue/`** — JSON-backed issue tracker with lifecycle state machine. Owns domain types (`FailureCategory`, `CIFailure`, `Issue`).
- **`cmd/medivac/`** — CLI commands (scan, fix, status, merge, dismiss, reopen).

### LLM Triage

The scanner uses Claude (via `agent-cli-wrapper/claude.Query()`) to triage CI logs instead of regex patterns. This makes it work across any CI environment — Go, TypeScript, Python, Docker, Dependabot, etc.

For a batch of failed runs, medivac makes a **single LLM call** containing all runs' data:
- All failed job names per run
- Annotations (best-effort context from the CI system)
- The combined cleaned log (ANSI stripped, timestamps removed, truncated to head/tail)

The LLM returns a JSON array of structured failures with category, file, line, error code, summary, and which job each error belongs to. The LLM is explicitly instructed to deduplicate errors that appear across multiple jobs in the same run.

Categories: `lint/go`, `lint/bazel`, `lint/ts`, `lint/python`, `build`, `build/docker`, `test`, `infra/dependabot`, `infra/ci`, `unknown`

Already-reviewed runs are skipped on subsequent scans, avoiding redundant LLM calls.

### Issue Grouping

After triage, related issues are grouped so a single fix agent can address the root cause in one PR:

- **TypeScript errors**: grouped by error code + project directory (e.g., all `TS7006` errors in `src/`)
- **Dependabot**: grouped by package name
- **Fallback**: singleton groups (one issue per agent)

### Fix Agents

Fix agents run through a unified `runAgentCore` flow for both single-issue and group fixes:

1. Create a worktree from the base branch
2. Build a prompt with failure context (category, summary, file, error code, run URL, job name, details)
3. If previous attempts exist, include their reasoning and errors in the prompt so the agent avoids repeating failed approaches
4. Launch a Claude session (real or mock via `SessionFactory`)
5. Parse a structured JSON `<ANALYSIS>` block from the agent's response (with legacy key-value fallback)
6. If the agent changed files, create a PR. If analysis-only (`fix_applied: false`), record the analysis and reset the issue for future retry.

### Issue Lifecycle

```
new → in_progress → fix_pending → fix_approved → fix_merged → verified
         |                                            ↓
         |                                        recurred → (re-enters fix cycle)
         ↓
      wont_fix (via dismiss)  ←→  new (via reopen)
```

Issues are identified by a stable signature (`{hash}:{file}`) that survives across runs. Signatures normalize away line numbers, hex hashes, timestamps, and Docker build context prefixes. The tracker persists to `.medivac/issues.json`.

## Usage

Build with Bazel:

```bash
bazel build //medivac/cmd/medivac
```

### Scan

Scan CI failures and categorize them:

```bash
medivac scan --repo-root /path/to/repo
```

Flags:
- `--branch` — Branch to scan (default: `main`)
- `--limit` — Number of recent failed runs to check (default: `5`)
- `--triage-model` — Claude model for triage (default: `haiku`)

### Fix

Scan + launch parallel Claude agents to fix actionable issues:

```bash
medivac fix --branch main --model sonnet --budget 1.0 --max-parallel 3
```

Each agent creates a worktree, investigates the failure, applies a fix, and creates a PR. Related issues are grouped and fixed together in a single PR. Use `--skip-scan` to launch agents from existing tracker state without re-scanning.

### Merge

Merge approved fix PRs and clean up worktrees:

```bash
medivac merge
```

### Status

Show tracked issue status:

```bash
medivac status
medivac status --json
medivac status --verbose
```

### Dismiss / Reopen

Mark issues as won't-fix or reopen them:

```bash
medivac dismiss <issue-id> --reason "not actionable"
medivac reopen <issue-id>
```

### Global Flags

- `--repo-root` — Repository worktree root (auto-detected if unset)
- `--tracker` — Path to issues.json (default: `<repo-root>/.medivac/issues.json`)
- `--dry-run` — Show what would be done without making changes
- `-v` / `--verbose` — Enable debug logging

## Cost

Triage uses Claude Haiku by default (~$0.03 per run). A typical 5-run scan costs ~$0.15. Fix agents use Sonnet by default with a configurable per-agent budget. Already-reviewed runs are skipped on re-scan, so repeated scans are free when no new failures appear.

## Future Improvements

- **LLM-powered grouping**: Use the LLM to identify which failures share a root cause instead of relying on hardcoded heuristics (error codes, package names). A single cheap LLM call after triage could correctly group cross-language errors that heuristics miss.
- **Parallel data gathering**: Fetch annotations, logs, and jobs for multiple runs concurrently instead of sequentially. Each run's data is independent.
- **Streaming triage**: Use `claude.QueryStream()` for real-time progress feedback during long scans.
- **PR review feedback loop**: When a fix PR gets review comments, feed them back to the agent for a follow-up attempt.
- **Budget-aware model selection**: Auto-downgrade to a cheaper model when budget is running low, or skip triage for runs that look similar to already-triaged ones.
- **Fix verification**: After a fix PR is merged, automatically re-run the specific failing CI job to verify the fix instead of waiting for the next full CI run.
- **Triage confidence scoring**: Add a confidence field to triage responses. Skip low-confidence issues, prioritize high-confidence ones, and flag uncertain ones for human review.
- **Fix knowledge base**: After verified fixes, store the analysis data (root cause, approach) to speed up future triage of similar failures.
- **Multi-CI provider support**: The `Scanner` interface is in place; implement it for GitLab CI, Jenkins, CircleCI, etc.
