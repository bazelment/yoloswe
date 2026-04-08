# Plan: Shell Command Support for Jiradozer Rounds

## Context

Issue #126 requests that jiradozer rounds support running shell commands in addition to agent sessions. The motivating use case is preparatory commands like `git pull origin main` before agent rounds like `/simplify`. Currently, every round must be an agent session — there's no way to run a simple shell command as part of a multi-round step.

## Approach

Add a `command` field to `RoundConfig`, mutually exclusive with `prompt`. When a round has `command` set, it runs via `sh -c` instead of launching an agent session. The command string supports the same Go template variables as prompts (`{{.Identifier}}`, `{{.BaseBranch}}`, etc.).

Example config:
```yaml
build:
  permission_mode: bypass
  rounds:
    - command: "git pull origin {{.BaseBranch}}"
    - prompt: "Implement changes for {{.Identifier}}"
      max_turns: 30
```

## Files to Modify

### 1. `jiradozer/config.go` — Add `Command` field + validation

- Add `Command string` to `RoundConfig` struct (yaml tag: `command`)
- Add `IsCommand() bool` method on `RoundConfig`
- Update `validate()` round loop:
  - Change "prompt is required" to "prompt or command is required"
  - Add mutual exclusivity check: `prompt` and `command` cannot both be set
  - Validate whichever is set as a Go template

### 2. `jiradozer/agent.go` — Add `RunCommand()` function

New function:
```go
func RunCommand(ctx context.Context, stepName string, data PromptData, commandTmpl string, workDir string, logger *slog.Logger) (string, error)
```
- Renders command template with `renderPrompt()` (reuses existing function)
- Executes via `exec.CommandContext(ctx, "sh", "-c", renderedCommand)` with `cmd.Dir = workDir`
- Captures combined stdout+stderr
- Returns output string; non-zero exit returns error
- Add `"os/exec"` import

### 3. `jiradozer/workflow.go` — Branch on round type in `runStepRounds()`

In the round loop (lines 145-181), branch on `round.IsCommand()`:
- **Command round**: call `RunCommand()`, skip feedback injection
- **Agent round**: existing logic (resolve round config, inject feedback if first agent round)

Adjust feedback injection: instead of `if i == 0`, track whether feedback has been injected and apply it to the **first agent round** (since a command round at index 0 can't use feedback).

### 4. `jiradozer/cmd/jiradozer/main.go` — Branch in `runSingleStepRounds()`

Same pattern as workflow.go: in the round loop (lines 485-498), branch on `round.IsCommand()` to call `RunCommand()` vs `RunStepAgent()`.

### 5. `jiradozer/jiradozer.example.yaml` — Document new field

Add a command round example to the validate section showing mixed command + agent rounds.

## Test Changes

### `jiradozer/config_test.go`
- `TestLoadConfig_WithCommandRounds` — loads fixture, verifies `Command` field parsed correctly
- `TestLoadConfig_RoundCommandAndPromptConflict` — expects validation error
- `TestLoadConfig_RoundNoPromptNoCommand` — expects validation error
- `TestRoundConfig_IsCommand` — unit test for helper

### `jiradozer/agent_test.go`
- `TestRunCommand_Success` — runs `echo hello`, asserts output
- `TestRunCommand_Failure` — runs `exit 1`, asserts error returned with output
- `TestRunCommand_TemplateRendering` — runs `echo {{.Identifier}}`, asserts rendered value in output

### `jiradozer/workflow_test.go`
- `TestWorkflow_RunStepRounds_CommandRound` — mixed command + agent rounds, verifies both outputs captured and joined

### New test fixtures
- `jiradozer/testdata/with_command_rounds.yaml` — valid config with mixed command/agent rounds
- `jiradozer/testdata/round_command_and_prompt.yaml` — invalid: both fields set

## Edge Cases

1. **Feedback on redo with command-first rounds**: Feedback injects into the first *agent* round, not the first round overall. A command round at index 0 simply re-runs the command.
2. **Empty command output**: Handled by existing `nonEmpty` filtering in output joining.
3. **Command timeout**: `exec.CommandContext` kills the process on context cancellation.
4. **Agent-only fields on command rounds** (`model`, `max_turns`, etc.): Silently ignored — `ResolveRound()` is never called for command rounds.

## Verification

1. `scripts/lint.sh` — lint passes
2. `bazel test //jiradozer/... --test_timeout=60` — all unit tests pass
3. Manual: create a test YAML with a command round (`echo hello`) and run `jiradozer --description "test" --run-step build --config test.yaml` to verify end-to-end
