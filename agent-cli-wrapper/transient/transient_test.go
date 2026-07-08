package transient

import "testing"

func TestClassifyText(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		msg        string
		wantReason string
		wantOK     bool
	}{
		{
			name:       "stream idle",
			msg:        "stream idle timeout - partial response received",
			wantReason: ReasonStreamIdle,
			wantOK:     true,
		},
		{
			name:       "codex incomplete",
			msg:        "turn never reached turn/completed",
			wantReason: ReasonCodexIncomplete,
			wantOK:     true,
		},
		{
			name:       "http 429",
			msg:        "request failed with HTTP 429 Too Many Requests",
			wantReason: ReasonHTTP429,
			wantOK:     true,
		},
		{
			name:       "rate limit text",
			msg:        "provider rate limited the request",
			wantReason: ReasonHTTP429,
			wantOK:     true,
		},
		{
			name:       "http 503",
			msg:        "upstream returned status code 503",
			wantReason: ReasonHTTP5xx,
			wantOK:     true,
		},
		{
			// 529 (Anthropic overload) must classify as a retryable 5xx (#273).
			name:       "http 529 overloaded",
			msg:        "API Error: 529 {\"type\":\"overloaded_error\"}",
			wantReason: ReasonHTTP5xx,
			wantOK:     true,
		},
		{
			// Verbatim from jiradozer cron failures (#272).
			name:       "connection closed mid-response",
			msg:        "API Error: Connection closed mid-response. The response above may be incomplete.",
			wantReason: ReasonConnectionReset,
			wantOK:     true,
		},
		{
			// Verbatim from jiradozer cron failure (build step, 2026-07-07).
			// A worded 5xx with no status digit — httpStatusPattern misses it,
			// so it must be caught by the "server error" text case.
			name:       "server error mid-response",
			msg:        "API Error: Server error mid-response. The response above may be incomplete.",
			wantReason: ReasonHTTP5xx,
			wantOK:     true,
		},
		{
			// Grace-period force-complete must classify distinctly (#270) while
			// staying retryable.
			name:       "turn forced complete after grace period",
			msg:        "stream idle: turn forced complete after grace period gated on background tool_use",
			wantReason: ReasonGraceForced,
			wantOK:     true,
		},
		{
			name:       "connection reset",
			msg:        "read tcp: connection reset by peer",
			wantReason: ReasonConnectionReset,
			wantOK:     true,
		},
		{
			name:       "websocket closed",
			msg:        "websocket: close 1006 abnormal closure",
			wantReason: ReasonConnectionReset,
			wantOK:     true,
		},
		{
			// Verbatim from jiradozer cron failures (2026-06-04/05 plan step).
			name:       "socket connection closed unexpectedly",
			msg:        "API Error: The socket connection was closed unexpectedly. For more information, pass `verbose: true` in the second argument to fetch()",
			wantReason: ReasonConnectionReset,
			wantOK:     true,
		},
		{
			name:       "operation timed out",
			msg:        "context deadline exceeded: operation timed out",
			wantReason: ReasonTimeout,
			wantOK:     true,
		},
		{
			name:   "empty",
			msg:    "",
			wantOK: false,
		},
		{
			name:   "non transient syntax error",
			msg:    "syntax error near unexpected token",
			wantOK: false,
		},
		{
			name:   "embedded status-like digits",
			msg:    "processed 1500 records on port :5004",
			wantOK: false,
		},
		{
			// Accepted-breadth guard: the "server error" match is standalone
			// (2026-07-07), so any message carrying that phrase is retried as a
			// 5xx — a deliberate, tested choice. No wrapper currently emits a
			// non-retryable "server error" string; if one ever does, tighten
			// the classifier to require "mid-response" too.
			name:       "worded server error without status digit",
			msg:        "upstream returned an internal server error",
			wantReason: ReasonHTTP5xx,
			wantOK:     true,
		},
		{
			// Near-miss: no "server error" phrase and no status digit — must
			// stay non-transient so the standalone match doesn't over-broaden
			// to any message merely mentioning a "server".
			name:   "server mentioned but not an error",
			msg:    "connecting to build server at host:port",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotReason, gotOK := ClassifyText(tt.msg)
			if gotOK != tt.wantOK || gotReason != tt.wantReason {
				t.Fatalf("ClassifyText(%q) = (%q, %v), want (%q, %v)",
					tt.msg, gotReason, gotOK, tt.wantReason, tt.wantOK)
			}
		})
	}
}
