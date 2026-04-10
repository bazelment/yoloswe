package render

// Verbosity controls how much detail is printed to the terminal.
type Verbosity int

const (
	// VerbosityQuiet shows only errors and final results.
	VerbosityQuiet Verbosity = iota
	// VerbosityNormal shows text, tool names, errors, turn summaries,
	// rate limit warnings, and auth status. Tool results are hidden
	// unless they are errors.
	VerbosityNormal
	// VerbosityVerbose shows all tool results (truncated), thinking,
	// task/hook lifecycle, API retries, and compact boundaries.
	VerbosityVerbose
	// VerbosityDebug shows everything: full tool results, streaming tool
	// input, state changes, API retries, compact boundaries.
	VerbosityDebug
)

// String returns a human-readable name for the verbosity level.
func (v Verbosity) String() string {
	switch v {
	case VerbosityQuiet:
		return "quiet"
	case VerbosityNormal:
		return "normal"
	case VerbosityVerbose:
		return "verbose"
	case VerbosityDebug:
		return "debug"
	default:
		return "unknown"
	}
}

// ParseVerbosity converts a string to a Verbosity level.
// Returns VerbosityNormal for unrecognized values.
func ParseVerbosity(s string) Verbosity {
	switch s {
	case "quiet", "q":
		return VerbosityQuiet
	case "normal", "n", "":
		return VerbosityNormal
	case "verbose", "v":
		return VerbosityVerbose
	case "debug", "d":
		return VerbosityDebug
	default:
		return VerbosityNormal
	}
}
