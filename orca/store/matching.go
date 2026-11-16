package store

import (
	"strings"
	"unicode"
	"unicode/utf8"
)

// Mentions reports whether id appears as a complete identifier in text.
func Mentions(text, id string) bool {
	text = strings.ToLower(text)
	id = strings.ToLower(strings.TrimSpace(id))
	if id == "" {
		return false
	}
	idChar := func(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) || r == '-' || r == '_' }
	for i := 0; ; {
		j := strings.Index(text[i:], id)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(id)
		before, _ := utf8.DecodeLastRuneInString(text[:start])
		after, _ := utf8.DecodeRuneInString(text[end:])
		if (start == 0 || !idChar(before)) && (end == len(text) || !idChar(after)) {
			return true
		}
		i = start + 1
	}
}
