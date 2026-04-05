# Memory

## Key documents
- Session analysis & LLM summarization learnings: `memory/session-analysis-learnings.md`
- Engineering principles (abstraction invariants, parallel path drift, property testing):
  `docs/design/engineering-principles.md`
- Bramble session data pipeline design: `docs/design/sessionmodel-architecture.md`
- Adding new JSONL message types: `docs/design/bramble-jsonl-practices.md`
- Tmux pane capture learnings: `memory/tmux-capture-learnings.md`

## Tmux pane capture (feature/cc-tmux)
- `tmux capture-pane -t <target> -p -J -S -30` captures last 30 lines with joined wrapping
- Claude Code TUI status bar: separator → info line (path/branch/model/ctx/tokens) → permissions line
- `ParseClaudeStatusBar()` extracts structured data; `ContentLines()` strips TUI chrome
- Token regex must use `\d+[kKmMbB]?` (not `\S+`) — trailing context-compaction warning text
- Completion chars: ✻ ✢ ✽ ✹; Spinner chars: * · ⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏; Tool execution: ● (kept as content)
- Working state is sub-second transient; 15s polling almost never catches spinners
- `tmuxwatch` tool (`bramble/cmd/tmuxwatch/`) for live monitoring and validation
- IPC: `capture-pane` request type + `bramble capture-pane` CLI subcommand
- Command center `[p]` key toggles inline preview of tmux pane content

## Codex integration
- [Codex integration requirements](codex-integration.md) — OAuth auth (not API key), bwrap warning is harmless, sandbox defaults

## Multi-repo support
- Pattern: save/load context in `bramble/app/repocontext.go` — avoids restructuring 2000+ line update.go
- Key fields on Model: `repos map[string]*RepoContext`, `openedRepos []string`, `repoDropdown`, `sharedEvents`
- `NewModel()` takes a `session.ManagerConfig` template for creating new managers mid-session
- All test files using `NewModel()` need the extra `session.ManagerConfig{}` arg
- `SessionInfo.RepoName` field added to tag sessions with their repo
