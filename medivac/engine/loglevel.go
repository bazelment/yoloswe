package engine

import "log/slog"

// Custom slog levels for graduated verbosity.
// slog.LevelDebug is -4; lower values are more verbose.
const (
	// LevelTrace is used for -vv: prompts, trimmed CI logs, LLM responses.
	LevelTrace slog.Level = slog.LevelDebug - 4 // -8

	// LevelDump is used for -vvv: raw CI logs, full gh stdout.
	LevelDump slog.Level = slog.LevelDebug - 8 // -12
)
