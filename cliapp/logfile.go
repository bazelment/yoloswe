package cliapp

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// LogDir returns the standard log directory for the given tool name
// ($HOME/.<toolName>/logs), creating it if necessary. Tools that need to
// hand the directory path to a child subprocess can use this instead of
// re-deriving the path.
func LogDir(toolName string) (string, error) {
	dir, _, err := resolveLogPath(toolName)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create log dir %q: %w", dir, err)
	}
	return dir, nil
}

// resolveLogPath returns the path of the log file to open and the directory
// it lives in. The log directory is $HOME/.<toolName>/logs. The filename
// always includes a timestamp and pid to keep concurrent runs from
// clobbering each other.
func resolveLogPath(toolName string) (logDir, logPath string, err error) {
	home, herr := os.UserHomeDir()
	if herr != nil {
		return "", "", fmt.Errorf("determine home directory: %w", herr)
	}
	logDir = filepath.Join(home, "."+toolName, "logs")
	logPath = filepath.Join(logDir, fmt.Sprintf("%s-%s-%d.log",
		toolName, time.Now().Format("20060102-150405"), os.Getpid()))
	return logDir, logPath, nil
}

// openLogFile creates the log directory if needed and opens the log file
// for append. Callers must Close the returned file.
func openLogFile(logDir, logPath string) (*os.File, error) {
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return nil, fmt.Errorf("create log dir %q: %w", logDir, err)
	}
	f, err := os.OpenFile(logPath, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open log file %q: %w", logPath, err)
	}
	return f, nil
}
