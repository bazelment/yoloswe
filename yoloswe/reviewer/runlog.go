package reviewer

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/bazelment/yoloswe/logging/klogfmt"
)

// RunLogEnvTag is the env var used to tag review logs with an external run
// identifier (e.g., a /pr-polish round tag) so logs can be correlated later.
const RunLogEnvTag = "BRAMBLE_RUN_TAG"

// SetupRunLog installs a klogfmt slog handler writing to a timestamped file
// under ~/.bramble/logs/code-review/, matching the jiradozer pattern. Terminal
// output keeps flowing through the render.Renderer; this only captures a
// durable per-run record.
//
// Returns the log file path, a cleanup function to close the file, and any
// setup error. On error the returned path is empty, the cleanup is a no-op,
// and slog retains its previous default (typically the stderr text handler).
func SetupRunLog() (string, func(), error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", func() {}, fmt.Errorf("resolve home dir: %w", err)
	}
	logDir := filepath.Join(home, ".bramble", "logs", "code-review")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return "", func() {}, fmt.Errorf("create log dir %s: %w", logDir, err)
	}
	logPath := filepath.Join(logDir, fmt.Sprintf("code-review-%s-%d.log",
		time.Now().Format("20060102-150405"), os.Getpid()))
	prev := slog.Default()
	closeFile, err := klogfmt.InitWithLogFileAndLevels(logPath, slog.LevelDebug, slog.LevelError)
	if err != nil {
		return "", func() {}, fmt.Errorf("open log file %s: %w", logPath, err)
	}

	if tag := os.Getenv(RunLogEnvTag); tag != "" {
		slog.SetDefault(slog.Default().With("run_tag", tag))
	}

	cleanup := func() {
		slog.SetDefault(prev)
		_ = closeFile()
	}
	return logPath, cleanup, nil
}
