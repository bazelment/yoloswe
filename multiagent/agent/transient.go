package agent

import (
	"strings"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
)

// IsTransient reports whether err originates from a known retryable provider
// failure, such as stream-idle, rate limiting, or a temporary network break.
func IsTransient(err error) bool {
	if err == nil {
		return false
	}
	return claude.IsTransient(err) || codex.IsTransient(err) || matchesTransientText(err.Error())
}

// TransientReason returns a stable, low-cardinality reason for retry logs.
func TransientReason(err error) string {
	if err == nil {
		return "unknown_transient"
	}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "stream idle"), strings.Contains(msg, "stream timeout"), strings.Contains(msg, "stream disconnected"), strings.Contains(msg, "stream closed"):
		return "stream_idle"
	case strings.Contains(msg, "turn never reached turn/completed"):
		return "codex_incomplete"
	case strings.Contains(msg, "429"), strings.Contains(msg, "rate limit"):
		return "http_429"
	case strings.Contains(msg, "500"), strings.Contains(msg, "502"), strings.Contains(msg, "503"), strings.Contains(msg, "504"), strings.Contains(msg, "529"):
		return "http_5xx"
	case strings.Contains(msg, "connection reset"), strings.Contains(msg, "broken pipe"), strings.Contains(msg, "unexpected eof"), strings.Contains(msg, "websocket"):
		return "connection_reset"
	default:
		return "unknown_transient"
	}
}

func matchesTransientText(msg string) bool {
	s := strings.ToLower(msg)
	for _, pattern := range []string{
		"429",
		"503",
		"529",
		"connection reset",
		"broken pipe",
		"i/o timeout",
		"unexpected eof",
	} {
		if strings.Contains(s, pattern) {
			return true
		}
	}
	return false
}
