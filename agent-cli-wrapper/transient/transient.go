package transient

import (
	"regexp"
	"strings"
)

const (
	ReasonUnknown         = "unknown_transient"
	ReasonStreamIdle      = "stream_idle"
	ReasonCodexIncomplete = "codex_incomplete"
	ReasonHTTP429         = "http_429"
	ReasonHTTP5xx         = "http_5xx"
	ReasonConnectionReset = "connection_reset"
	ReasonTimeout         = "timeout"
)

var httpStatusPattern = regexp.MustCompile(`(^|[^[:alnum:]])(429|500|502|503|504|529)([^[:alnum:]]|$)`)

// ClassifyText reports whether msg describes a retryable provider failure and
// returns a stable reason for logs.
func ClassifyText(msg string) (string, bool) {
	s := strings.ToLower(msg)
	switch {
	case strings.Contains(s, "stream idle"),
		strings.Contains(s, "stream timeout"),
		strings.Contains(s, "stream disconnected"),
		strings.Contains(s, "stream closed"):
		return ReasonStreamIdle, true
	case strings.Contains(s, "turn never reached turn/completed"):
		return ReasonCodexIncomplete, true
	case httpStatusPattern.MatchString(s) && strings.Contains(s, "429"),
		strings.Contains(s, "http 429"),
		strings.Contains(s, "status 429"),
		strings.Contains(s, "status code 429"),
		strings.Contains(s, "rate limit"),
		strings.Contains(s, "rate limited"):
		return ReasonHTTP429, true
	case httpStatusPattern.MatchString(s):
		return ReasonHTTP5xx, true
	case strings.Contains(s, "connection reset"),
		strings.Contains(s, "broken pipe"),
		strings.Contains(s, "unexpected eof"),
		strings.Contains(s, "websocket"):
		return ReasonConnectionReset, true
	case strings.Contains(s, "i/o timeout"),
		strings.Contains(s, "timeout"),
		strings.Contains(s, "timed out"):
		return ReasonTimeout, true
	default:
		return "", false
	}
}
