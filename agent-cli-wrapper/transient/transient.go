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
	// ReasonGraceForced marks a turn that was force-completed because a
	// background tool_use outlived the per-turn grace period. It is still
	// retryable (a fresh turn may not stall), but the distinct reason lets logs
	// tell a grace-period force-complete apart from a true idle stream.
	ReasonGraceForced = "grace_forced"
	// ReasonOutOfCredits marks a workspace-wide out-of-credits failure. It is
	// deliberately NOT returned by ClassifyText (a same-model retry can't refill
	// the workspace); it exists for model-fallback logging in callers that swap
	// to a different provider. See multiagent/agent.IsOutOfCredits.
	ReasonOutOfCredits = "out_of_credits"
)

var httpStatusPattern = regexp.MustCompile(`(^|[^[:alnum:]])(429|500|502|503|504|529)([^[:alnum:]]|$)`)

// ClassifyText reports whether msg describes a retryable provider failure and
// returns a stable reason for logs.
func ClassifyText(msg string) (string, bool) {
	s := strings.ToLower(msg)
	switch {
	// Checked before "stream idle" because the grace-period force-complete
	// surfaces as "stream idle: turn forced complete after grace period ...";
	// matching the more specific phrase first keeps the reason distinct (#270).
	case strings.Contains(s, "turn forced complete after grace period"):
		return ReasonGraceForced, true
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
		strings.Contains(s, "socket connection was closed"),
		// "API Error: Connection closed mid-response. The response above may be
		// incomplete." — seen in jiradozer cron failures. "connection closed"
		// matches it; we don't also match a bare "mid-response" because that
		// substring appears in unrelated, non-retryable messages.
		strings.Contains(s, "connection closed"),
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
