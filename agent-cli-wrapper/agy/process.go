package agy

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/internal/procattr"
)

type processManager struct {
	cmd      *exec.Cmd
	prompt   string
	config   SessionConfig
	mu       sync.Mutex
	started  bool
	stopping bool
}

func newProcessManager(prompt string, config SessionConfig) *processManager {
	return &processManager{
		config: config,
		prompt: prompt,
	}
}

// agyEffortLevels is the reasoning vocabulary agy accepts, in the order the
// model-id split must try them: longest first, so "-medium" matches before any
// shorter overlap. This slice is the single owner of the set - SplitModelEffort
// reads it as id suffixes and isAgyEffortLevel as bare --effort values, so the
// two cannot disagree about which levels exist.
var agyEffortLevels = []string{"medium", "high", "low"}

// SplitModelEffort separates an agy model id from the reasoning level it pins.
// agy's catalog spells the level as a trailing -low/-medium/-high
// (gemini-3.8-flash-low, gemini-3.1-pro-high, ...). pinned is empty when the id
// leaves the level open.
//
// Exported as the single owner of this spelling rule: multiagent/agent's
// catalog-aware reconciliation needs the same split, and two copies of "how an
// agy id encodes its level" would be free to drift.
func SplitModelEffort(model string) (base, pinned string) {
	for _, level := range agyEffortLevels {
		suffix := "-" + level
		if strings.HasSuffix(model, suffix) {
			return strings.TrimSuffix(model, suffix), level
		}
	}
	return model, ""
}

// reconcileModelEffort settles the two ways one agy command line can carry a
// reasoning level, so that exactly one representation reaches the CLI.
//
// agy encodes the level in the model id AND offers a separate --effort flag,
// and rejects a command line carrying two that disagree:
//
//	Error: invalid model selection (--model "gemini-3.8-flash-medium"
//	--effort "high"): --model gemini-3.8-flash-medium conflicts with --effort=high
//
// This runs in BuildCLIArgs because that is the single producer of the argv:
// every caller reaching the agy CLI goes through it, so none of them can
// assemble a self-conflicting command line. It resolves the conflict the way
// the caller asked for - the requested effort wins by RETARGETING the model id
// to the variant that encodes it, never by silently dropping the request - and
// then drops the now-redundant --effort.
//
// The decision is purely SYNTACTIC, which is what keeps it here: whether an id
// pins a level is a property of its spelling, so this wrapper needs no model
// catalog and stays free of any dependency on the higher-level registry.
// Whether the retargeted variant actually EXISTS is a catalog question this
// layer cannot answer; multiagent/agent's reconcileAgyEffort owns that, and
// rejects an unrepresentable pair up front with ErrEffortUnsupported. When a
// caller has already reconciled, this is a no-op (the pinned level and the
// requested effort agree, so only the model survives). For a caller that has
// not, the retarget is taken optimistically and agy itself judges the result -
// strictly better than shipping a command line already known to be rejected.
func reconcileModelEffort(model, effort string) (string, string) {
	if model == "" || effort == "" {
		return model, effort
	}
	base, pinned := SplitModelEffort(model)
	if pinned == "" {
		// Nothing pinned by the model: --effort alone carries the level.
		return model, effort
	}
	if pinned == effort {
		return model, "" // Same level twice; keep one representation.
	}
	if !isAgyEffortLevel(effort) {
		// Not a level agy spells, so there is no variant to retarget onto and
		// splicing it in would forge a model id that does not exist
		// ("gemini-3.8-flash-max"). agy would then reject the MODEL, hiding
		// that the effort was the bad input. Leave both as the caller set them
		// and let agy report the real problem against the real flag.
		return model, effort
	}
	return base + "-" + effort, ""
}

// isAgyEffortLevel reports whether level is one agy spells, i.e. one that has a
// model-id variant to retarget onto. agy's --effort flag documents exactly
// low|medium|high, which is what agyEffortLevels holds.
func isAgyEffortLevel(level string) bool {
	return slices.Contains(agyEffortLevels, level)
}

// EffectiveModel is the model id BuildCLIArgs actually passes to agy, after
// reconciliation. It is what callers should report as "the model we ran".
func (pm *processManager) EffectiveModel() string {
	model, _ := reconcileModelEffort(pm.config.Model, pm.config.Effort)
	return model
}

// BuildCLIArgs builds the agy print-mode argument list.
//
// Two agy argument rules shape this list (verified against agy 1.1.24):
//
//  1. -p/--print rejects a prompt value that is a registered agy flag name:
//     `agy -p --sandbox "task"` fails with `-p took "--sandbox" as its prompt`.
//     This is a property of the prompt value itself, so it applies to the
//     attached form (`--print=--sandbox`) too, and neither argument ordering
//     nor an attached prompt can prevent it. It is not guarded here: the
//     prompt is caller data, and agy's own error names the problem clearly.
//  2. A stray positional argument is a hard error in any position
//     (`Error: unexpected argument "..."`), so every token must be a flag or
//     a flag's value.
//
// A flag placed after `-p <prompt>` does parse correctly, so the ordering
// below is not a correctness fix. Emitting every flag - ExtraArgs included -
// before the trailing `-p <prompt>` pair keeps the list in one canonical
// shape, which is what the argument-order tests pin. Keep -p <prompt> last.
//
// JSON output provides conversation_id and usage without changing event timing:
// this wrapper already buffers the entire print-mode response before emitting it.
//
// It also reconciles --model against --effort so the level reaches agy exactly
// once; see reconcileModelEffort.
func (pm *processManager) BuildCLIArgs() []string {
	args := []string{"--output-format", "json"}

	model, effort := reconcileModelEffort(pm.config.Model, pm.config.Effort)
	if model != "" {
		args = append(args, "--model", model)
	}
	if effort != "" {
		args = append(args, "--effort", effort)
	}
	if pm.config.PrintTimeout > 0 {
		args = append(args, "--print-timeout", formatDuration(pm.config.PrintTimeout))
	}
	if pm.config.ConversationID != "" {
		args = append(args, "--conversation", pm.config.ConversationID)
	}
	if pm.config.LogFile != "" {
		args = append(args, "--log-file", pm.config.LogFile)
	}
	for _, dir := range pm.config.AddDirs {
		args = append(args, "--add-dir", dir)
	}
	if pm.config.DangerouslySkipPermissions {
		args = append(args, "--dangerously-skip-permissions")
	}
	if pm.config.Sandbox {
		args = append(args, "--sandbox")
	}
	args = append(args, pm.config.ExtraArgs...)
	args = append(args, "-p", pm.prompt)
	return args
}

func formatDuration(d time.Duration) string {
	if d%time.Second == 0 {
		return strconv.FormatInt(int64(d/time.Second), 10) + "s"
	}
	return d.String()
}

func (pm *processManager) Start(ctx context.Context) (stdout []byte, stderr []byte, err error) {
	pm.mu.Lock()
	if pm.started {
		pm.mu.Unlock()
		return nil, nil, ErrAlreadyStarted
	}
	pm.started = true
	pm.mu.Unlock()

	cliPath := pm.config.CLIPath
	if cliPath == "" {
		cliPath = "agy"
	}

	cmd := exec.CommandContext(ctx, cliPath, pm.BuildCLIArgs()...)
	cmd.Env = os.Environ()
	for k, v := range pm.config.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	if pm.config.WorkDir != "" {
		cmd.Dir = pm.config.WorkDir
	}
	procattr.Set(cmd)

	var outBuf, errBuf bytes.Buffer
	cmd.Stdout = &outBuf
	cmd.Stderr = &errBuf

	pm.mu.Lock()
	pm.cmd = cmd
	pm.mu.Unlock()

	err = cmd.Run()
	stderr = errBuf.Bytes()
	if len(stderr) > 0 && pm.config.StderrHandler != nil {
		pm.config.StderrHandler(stderr)
	}
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return outBuf.Bytes(), stderr, &CLINotFoundError{Path: cliPath, Cause: err}
		}
		return outBuf.Bytes(), stderr, &ProcessError{
			Message: fmt.Sprintf("agy exited with stderr: %s", bytes.TrimSpace(stderr)),
			Cause:   err,
		}
	}
	return outBuf.Bytes(), stderr, nil
}

func (pm *processManager) Stop() error {
	pm.mu.Lock()
	if !pm.started || pm.stopping {
		pm.mu.Unlock()
		return nil
	}
	pm.stopping = true
	cmd := pm.cmd
	pm.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return nil
	}
	_ = procattr.SignalGroup(cmd.Process, syscall.SIGTERM)
	time.Sleep(100 * time.Millisecond)
	_ = procattr.KillGroup(cmd.Process)
	return nil
}
