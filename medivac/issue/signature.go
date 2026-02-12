package issue

import (
	"regexp"
	"strings"
)

var (
	normalizeLineCol     = regexp.MustCompile(`:\d+:\d+`)
	normalizeHex         = regexp.MustCompile(`\b[0-9a-f]{7,40}\b`)
	normalizeTS          = regexp.MustCompile(`\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}`)
	normalizeWhitespace  = regexp.MustCompile(`\s+`)
	buildContextPrefixRe = regexp.MustCompile(`^services/[^/]+/[^/]+/`)
)

// normalizeMessage strips volatile parts (line numbers, hashes, timestamps) from error messages.
func normalizeMessage(msg string) string {
	msg = normalizeLineCol.ReplaceAllString(msg, "")
	msg = normalizeHex.ReplaceAllString(msg, "")
	msg = normalizeTS.ReplaceAllString(msg, "")
	msg = strings.TrimRight(msg, ".!,;:?")               // strip trailing punctuation
	msg = normalizeWhitespace.ReplaceAllString(msg, " ") // collapse whitespace
	msg = strings.ToLower(msg)                           // case-insensitive matching
	return strings.TrimSpace(msg)
}

// canonicalizePath strips known build-context prefixes from file paths
// so the same file produces the same signature regardless of the build
// context (e.g. Docker build vs lint step).
func canonicalizePath(path string) string {
	// Strip leading "services/<type>/<name>/" prefix that appears in Docker builds.
	// This regex matches patterns like "services/typescript/forge-v2/src/..."
	// and reduces them to just "src/...".
	path = stripBuildContextPrefix(path)
	// Normalize to forward slashes and remove leading ./
	path = strings.ReplaceAll(path, "\\", "/")
	path = strings.TrimPrefix(path, "./")
	return path
}

func stripBuildContextPrefix(path string) string {
	// If path matches "services/<type>/<project>/src/...", strip the prefix.
	// Only strip if the remainder looks like a source path (starts with src/, lib/, etc.)
	loc := buildContextPrefixRe.FindStringIndex(path)
	if loc == nil {
		return path
	}
	remainder := path[loc[1]:]
	// Only strip if the remainder starts with a common source directory.
	// This prevents stripping legitimate paths like "services/api/handler.go".
	srcPrefixes := []string{"src/", "lib/", "test/", "tests/", "pkg/", "cmd/", "internal/"}
	for _, p := range srcPrefixes {
		if strings.HasPrefix(remainder, p) {
			return remainder
		}
	}
	return path
}
