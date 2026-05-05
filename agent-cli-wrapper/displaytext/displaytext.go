package displaytext

import "strings"

// Truncate truncates s to at most max Unicode code points, appending "..." if
// truncation occurred (the suffix counts toward max). Rune-based indexing avoids
// splitting multi-byte UTF-8 sequences. If max <= 3, returns max runes with no
// suffix.
func Truncate(s string, max int) string {
	// Fast path: byte length <= max implies rune length <= max.
	if len(s) <= max {
		return s
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 3 {
		return string(runes[:max])
	}
	return string(runes[:max-3]) + "..."
}

// TruncatePath truncates a file path, keeping the end visible. For paths longer
// than max, prefers ".../" + last two path components; falls back to "..." +
// filename, then to rune-truncation.
func TruncatePath(path string, max int) string {
	// Fast path: byte length <= max implies rune length <= max.
	if len(path) <= max {
		return path
	}
	runes := []rune(path)
	if len(runes) <= max {
		return path
	}
	// Try to keep the last two components (e.g. "foo/bar.go").
	parts := strings.Split(path, "/")
	if len(parts) >= 2 {
		suffix := strings.Join(parts[len(parts)-2:], "/")
		prefixed := ".../" + suffix
		if len(prefixed) <= max { // ASCII-safe: ".../" + path segments
			return prefixed
		}
	}
	// Fall back to keeping just the filename.
	if lastSlash := strings.LastIndexByte(path, '/'); lastSlash > 0 {
		suffix := path[lastSlash:]
		if len(suffix) <= max-3 { // ASCII-safe: path components
			return "..." + suffix
		}
	}
	if max <= 3 {
		return string(runes[:max])
	}
	return string(runes[:max-3]) + "..."
}
