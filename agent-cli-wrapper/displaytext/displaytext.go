package displaytext

import "strings"

// Truncate returns s shortened to at most max runes, appending "..." when the
// suffix fits within max.
func Truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max || runeCountAtMost(s, max) {
		return s
	}
	if max <= 3 {
		return s[:byteIndexAfterRunes(s, max)]
	}
	return s[:byteIndexAfterRunes(s, max-3)] + "..."
}

// TruncatePath returns path shortened to at most max runes, preferring to keep
// the last two path components visible.
func TruncatePath(path string, max int) string {
	return TruncatePathComponents(path, max, 2)
}

// TruncatePathComponents returns path shortened to at most max runes, preferring
// to keep the requested number of trailing path components visible.
func TruncatePathComponents(path string, max int, components int) string {
	if max <= 0 {
		return ""
	}
	if len(path) <= max || runeCountAtMost(path, max) {
		return path
	}
	if components > 0 {
		if suffix := trailingPathComponents(path, components); suffix != "" {
			if candidate := ".../" + suffix; runeCountAtMost(candidate, max) {
				return candidate
			}
		}
	}
	if suffix := trailingPathComponents(path, 1); suffix != "" {
		if candidate := ".../" + suffix; runeCountAtMost(candidate, max) {
			return candidate
		}
	}
	return Truncate(path, max)
}

func trailingPathComponents(path string, components int) string {
	end := strings.TrimRight(path, "/")
	if end == "" {
		return ""
	}

	start := len(end)
	for i := 0; i < components; i++ {
		slash := strings.LastIndexByte(end[:start], '/')
		if slash < 0 {
			return strings.TrimLeft(end, "/")
		}
		start = slash
	}
	return strings.TrimLeft(end[start:], "/")
}

func runeCountAtMost(s string, max int) bool {
	if max < 0 {
		return false
	}
	count := 0
	for range s {
		count++
		if count > max {
			return false
		}
	}
	return true
}

func byteIndexAfterRunes(s string, n int) int {
	if n <= 0 {
		return 0
	}
	count := 0
	for i := range s {
		if count == n {
			return i
		}
		count++
	}
	return len(s)
}
