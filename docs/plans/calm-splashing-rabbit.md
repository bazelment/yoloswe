# Plan: klogfmt — slog Handler with klog-style output

## Context

We want all CLIs in this monorepo to emit structured logs in klog's compact, scannable format:
```
I0404 12:34:56.789012   12345 handler.go:42] order placed order_id="abc" latency_ms=200
```

Currently logging is fragmented: symphony uses `slog.NewTextHandler`, medivac has custom slog setup with verbosity levels, bramble uses stdlib `log`, and most CLIs have no logging init at all.

## Approach

### 1. Create `logging/` module at repo root with `klogfmt` subpackage

New module at `logging/` with **zero external deps** (stdlib only). `klogfmt` is a subpackage within it.

**Files:**
- `logging/go.mod` — module declaration only
- `logging/klogfmt/handler.go` — core `slog.Handler` implementation
- `logging/klogfmt/init.go` — `Init()` convenience + `Option` types
- `logging/klogfmt/handler_test.go` — unit tests

**Public API:**
```go
// import "github.com/bazelment/yoloswe/logging/klogfmt"
func Init(opts ...Option)                        // sets slog.SetDefault
func New(w io.Writer, opts ...Option) *Handler   // for custom wiring
func WithLevel(l slog.Leveler) Option
```

Handler implements `slog.Handler` interface: `Enabled`, `Handle`, `WithAttrs`, `WithGroup`.

Key details:
- Severity: D/I/W/E (Debug/Info/Warn/Error)
- PID cached via `os.Getpid()`, right-justified 7 chars
- Source from `runtime.CallersFrames(record.PC)`, basename only
- Values quoted only when containing spaces/quotes
- `sync.Mutex` on writer for concurrent safety
- `WithAttrs`/`WithGroup` return new Handler with accumulated state

### 2. Wire into go.work

Add `./logging` to `go.work` use block.

### 3. Update `symphony/logging/logging.go`

Change `NewLogger()` to use `klogfmt.New(os.Stderr)` instead of `slog.NewTextHandler`. `WithIssue`/`WithSession` helpers stay unchanged.

### 4. Wire `klogfmt.Init()` into all 16 CLI entry points

| CLI | Module | Change |
|-----|--------|--------|
| `symphony/cmd/symphony/main.go` | symphony | Gets klogfmt via updated `logging.NewLogger()` |
| `medivac/cmd/medivac/main.go` | medivac | Replace `slog.NewTextHandler` with `klogfmt.New()` in `newLogger()`/`newFileLogger()` |
| `bramble/cmd/sessanalyze/main.go` | bramble | Replace `slog.NewTextHandler` with `klogfmt.Init(WithLevel(slog.LevelError))` |
| `bramble/main.go` | bramble | Add `klogfmt.Init()`, replace `log.Printf` with `slog.Warn` |
| `wt/cmd/wt/main.go` | wt | Add `klogfmt.Init()` |
| `multiagent/cmd/swarm/main.go` | multiagent | Add `klogfmt.Init()` |
| `yoloswe/cmd/yoloswe/main.go` | yoloswe | Add `klogfmt.Init()` |
| `yoloswe/cmd/code-review/main.go` | yoloswe | Add `klogfmt.Init()` |
| `yoloswe/cmd/sessionplayer/main.go` | yoloswe | Add `klogfmt.Init()` |
| `bramble/cmd/logview/main.go` | bramble | Add `klogfmt.Init()` |
| `bramble/cmd/codexlogview/main.go` | bramble | Add `klogfmt.Init()` |
| `bramble/cmd/sessview/main.go` | bramble | Add `klogfmt.Init()` |
| `bramble/cmd/tmuxwatch/main.go` | bramble | Add `klogfmt.Init()` |
| `bramble/cmd/readline-voice-spike/main.go` | bramble | Add `klogfmt.Init()` |
| `voice/cmd/voicetest/main.go` | voice | Add `klogfmt.Init()` |

### 5. Bazel + deps

- Run `bazel run //:tidy` to update go.mod/go.sum across workspace
- Run `bazel run //:gazelle` to generate BUILD.bazel files
- Verify with `bazel build //...` and `bazel test //...`

## Verification

1. `bazel test //logging/...` — unit tests pass
2. `bazel build //...` — full repo builds
3. `bazel test //...` — all existing tests pass
4. `scripts/lint.sh` — lint clean
5. Manual: run any binary and confirm klog-format output on stderr for log lines
