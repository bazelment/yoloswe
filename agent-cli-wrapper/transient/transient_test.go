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
