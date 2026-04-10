package protocol

import (
	"testing"
)

func TestParseMessage_KeepAlive(t *testing.T) {
	msg, err := ParseMessage([]byte(`{"type":"keep_alive"}`))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	ka, ok := msg.(KeepAliveMessage)
	if !ok {
		t.Fatalf("expected KeepAliveMessage, got %T", msg)
	}
	if ka.MsgType() != MessageTypeKeepAlive {
		t.Errorf("bad MsgType: %q", ka.MsgType())
	}
}

func TestParseMessage_ToolProgress(t *testing.T) {
	raw := `{"type":"tool_progress","tool_use_id":"toolu_1","tool_name":"Bash","elapsed_time_seconds":3.5,"session_id":"s1","uuid":"u1","parent_tool_use_id":null}`
	msg, err := ParseMessage([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	tp, ok := msg.(ToolProgressMessage)
	if !ok {
		t.Fatalf("expected ToolProgressMessage, got %T", msg)
	}
	if tp.ToolUseID != "toolu_1" {
		t.Errorf("tool_use_id: %q", tp.ToolUseID)
	}
	if tp.ToolName != "Bash" {
		t.Errorf("tool_name: %q", tp.ToolName)
	}
	if tp.ElapsedTimeSeconds != 3.5 {
		t.Errorf("elapsed: %v", tp.ElapsedTimeSeconds)
	}
	if tp.SessionID != "s1" {
		t.Errorf("session_id: %q", tp.SessionID)
	}
}

func TestParseMessage_ToolUseSummary(t *testing.T) {
	raw := `{"type":"tool_use_summary","preceding_tool_use_ids":["t1","t2"],"summary":"Ran 2 tools","session_id":"s1","uuid":"u1"}`
	msg, err := ParseMessage([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s, ok := msg.(ToolUseSummaryMessage)
	if !ok {
		t.Fatalf("expected ToolUseSummaryMessage, got %T", msg)
	}
	if len(s.PrecedingToolUseIDs) != 2 || s.PrecedingToolUseIDs[0] != "t1" {
		t.Errorf("preceding: %v", s.PrecedingToolUseIDs)
	}
	if s.Summary != "Ran 2 tools" {
		t.Errorf("summary: %q", s.Summary)
	}
}

func TestParseMessage_AuthStatus(t *testing.T) {
	raw := `{"type":"auth_status","output":["step 1","step 2"],"isAuthenticating":true,"session_id":"s1","uuid":"u1"}`
	msg, err := ParseMessage([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	a, ok := msg.(AuthStatusMessage)
	if !ok {
		t.Fatalf("expected AuthStatusMessage, got %T", msg)
	}
	if !a.IsAuthenticating {
		t.Error("expected isAuthenticating true")
	}
	if len(a.Output) != 2 {
		t.Errorf("output: %v", a.Output)
	}
}

func TestParseMessage_RateLimitEvent(t *testing.T) {
	raw := `{"type":"rate_limit_event","session_id":"s1","uuid":"u1","rate_limit_info":{"status":"allowed_warning","rateLimitType":"primary","isUsingOverage":false}}`
	msg, err := ParseMessage([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	rl, ok := msg.(RateLimitEventMessage)
	if !ok {
		t.Fatalf("expected RateLimitEventMessage, got %T", msg)
	}
	if rl.RateLimitInfo.Status != "allowed_warning" {
		t.Errorf("status: %q", rl.RateLimitInfo.Status)
	}
	if rl.RateLimitInfo.RateLimitType != "primary" {
		t.Errorf("rateLimitType: %q", rl.RateLimitInfo.RateLimitType)
	}
}

func TestParseMessage_PromptSuggestion(t *testing.T) {
	raw := `{"type":"prompt_suggestion","suggestion":"maybe try this","session_id":"s1","uuid":"u1"}`
	msg, err := ParseMessage([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	p, ok := msg.(PromptSuggestionMessage)
	if !ok {
		t.Fatalf("expected PromptSuggestionMessage, got %T", msg)
	}
	if p.Suggestion != "maybe try this" {
		t.Errorf("suggestion: %q", p.Suggestion)
	}
}

func TestParseMessage_StreamlinedText(t *testing.T) {
	raw := `{"type":"streamlined_text","text":"hello world","session_id":"s1","uuid":"u1"}`
	msg, err := ParseMessage([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s, ok := msg.(StreamlinedTextMessage)
	if !ok {
		t.Fatalf("expected StreamlinedTextMessage, got %T", msg)
	}
	if s.Text != "hello world" {
		t.Errorf("text: %q", s.Text)
	}
}

func TestParseMessage_StreamlinedToolUseSummary(t *testing.T) {
	raw := `{"type":"streamlined_tool_use_summary","tool_summary":"3 bash, 1 read","session_id":"s1","uuid":"u1"}`
	msg, err := ParseMessage([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s, ok := msg.(StreamlinedToolUseSummaryMessage)
	if !ok {
		t.Fatalf("expected StreamlinedToolUseSummaryMessage, got %T", msg)
	}
	if s.ToolSummary != "3 bash, 1 read" {
		t.Errorf("tool_summary: %q", s.ToolSummary)
	}
}

func TestParseMessage_ControlCancelRequest(t *testing.T) {
	raw := `{"type":"control_cancel_request","request_id":"req_42"}`
	msg, err := ParseMessage([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	c, ok := msg.(ControlCancelRequest)
	if !ok {
		t.Fatalf("expected ControlCancelRequest, got %T", msg)
	}
	if c.RequestID != "req_42" {
		t.Errorf("request_id: %q", c.RequestID)
	}
}

func TestParseMessage_UpdateEnvironmentVariables(t *testing.T) {
	raw := `{"type":"update_environment_variables","variables":{"FOO":"bar","BAZ":"qux"}}`
	msg, err := ParseMessage([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	u, ok := msg.(UpdateEnvironmentVariablesMessage)
	if !ok {
		t.Fatalf("expected UpdateEnvironmentVariablesMessage, got %T", msg)
	}
	if u.Variables["FOO"] != "bar" || u.Variables["BAZ"] != "qux" {
		t.Errorf("variables: %v", u.Variables)
	}
}

func TestResultMessage_Outcome_Success(t *testing.T) {
	m := ResultMessage{Subtype: "success", Result: "hello"}
	out := m.Outcome()
	s, ok := out.(ResultSuccess)
	if !ok {
		t.Fatalf("expected ResultSuccess, got %T", out)
	}
	if s.Text != "hello" {
		t.Errorf("text: %q", s.Text)
	}
}

func TestResultMessage_Outcome_Errors(t *testing.T) {
	tests := []struct {
		name    string
		subtype ResultSubtype
	}{
		{"max_turns", ResultSubtypeErrorMaxTurns},
		{"during_execution", ResultSubtypeErrorDuringExecution},
		{"max_budget", ResultSubtypeErrorMaxBudgetUSD},
		{"max_structured_retries", ResultSubtypeErrorMaxStructuredOutputRetries},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := ResultMessage{Subtype: string(tt.subtype), Errors: []string{"boom"}}
			out := m.Outcome()
			e, ok := out.(ResultError)
			if !ok {
				t.Fatalf("expected ResultError, got %T", out)
			}
			if e.Subtype != tt.subtype {
				t.Errorf("subtype: got %q want %q", e.Subtype, tt.subtype)
			}
			if len(e.Errors) != 1 || e.Errors[0] != "boom" {
				t.Errorf("errors: %v", e.Errors)
			}
		})
	}
}
