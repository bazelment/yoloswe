package reviewer

import (
	"path/filepath"
)

// toolDisplay renders a tool call (name + input map) into a human-readable
// one-line string for terminal output. Each backend declares a toolDisplay
// describing its tool names, the most informative input key for each tool,
// and a fallback rule for unknown tool names.
type toolDisplay struct {
	// tools maps backend-native tool names to display info. The zero value
	// (empty Display, empty ArgKey) falls through to fallback + no arg.
	tools map[string]toolInfo
	// fallback renames an unknown tool name for display. If nil, the raw
	// tool name is used.
	fallback func(string) string
}

// toolInfo describes how one tool is rendered. ArgKey names the input-map key
// whose value (if present, non-empty, and a string) is included in the display.
// ArgFormat selects how that value is formatted.
type toolInfo struct {
	Display   string
	ArgKey    string
	ArgFormat argFormat
}

// argFormat enumerates display formats for a tool argument value.
type argFormat int

const (
	argFormatPath           argFormat = iota // shorten to .../parent/file
	argFormatCommand                         // truncate at 50, prefix with ": "
	argFormatQuery                           // truncate at 60, prefix with " "
	argFormatPlain                           // include verbatim, prefix with " "
	argFormatLongIdentifier                  // truncate at 60, prefix with " " (used for URLs)
)

const (
	maxCommandDisplay = 50
	maxQueryDisplay   = 60
)

// format renders a tool call for display.
func (d *toolDisplay) format(name string, input map[string]interface{}) string {
	info, ok := d.tools[name]
	if !ok {
		display := name
		if d.fallback != nil {
			display = d.fallback(name)
		}
		return display
	}
	if info.ArgKey == "" || input == nil {
		return info.Display
	}
	raw, ok := input[info.ArgKey]
	if !ok {
		return info.Display
	}
	s, ok := raw.(string)
	if !ok || s == "" {
		return info.Display
	}
	switch info.ArgFormat {
	case argFormatPath:
		return info.Display + " " + shortPath(s)
	case argFormatCommand:
		return info.Display + ": " + truncate(s, maxCommandDisplay)
	case argFormatQuery, argFormatLongIdentifier:
		return info.Display + " " + truncate(s, maxQueryDisplay)
	default:
		return info.Display + " " + s
	}
}

// truncate shortens s to max runes (approximated as bytes — callers pass
// ASCII-only values) and appends "..." when truncated.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-3] + "..."
}

// shortPath returns the last 2 path components prefixed with ".../".
// e.g. "/home/user/project/pkg/file.go" → ".../pkg/file.go"
func shortPath(p string) string {
	dir, file := filepath.Split(p)
	if dir == "" {
		return file
	}
	parent := filepath.Base(filepath.Clean(dir))
	return ".../" + parent + "/" + file
}
