package protocol

import (
	"testing"
)

func parseSystem(t *testing.T, raw string) SystemMessage {
	t.Helper()
	msg, err := ParseMessage([]byte(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	s, ok := msg.(SystemMessage)
	if !ok {
		t.Fatalf("expected SystemMessage, got %T", msg)
	}
	return s
}

func TestSystemSubtype_Init(t *testing.T) {
	raw := `{"type":"system","subtype":"init","session_id":"s1","uuid":"u1","cwd":"/tmp","model":"claude-opus-4-6","tools":["Bash","Read"],"mcp_servers":[],"slash_commands":[],"skills":[],"plugins":[],"permissionMode":"default","apiKeySource":"none","claude_code_version":"2.1.12","output_style":"default"}`
	m := parseSystem(t, raw)
	p, ok := m.AsInit()
	if !ok {
		t.Fatalf("AsInit failed")
	}
	if p.Model != "claude-opus-4-6" {
		t.Errorf("model: %q", p.Model)
	}
	if len(p.Tools) != 2 || p.Tools[0] != "Bash" {
		t.Errorf("tools: %v", p.Tools)
	}
	if p.PermissionMode != "default" {
		t.Errorf("permissionMode: %q", p.PermissionMode)
	}
	if p.ClaudeCodeVersion != "2.1.12" {
		t.Errorf("version: %q", p.ClaudeCodeVersion)
	}
}

func TestSystemSubtype_Status(t *testing.T) {
	raw := `{"type":"system","subtype":"status","session_id":"s1","uuid":"u1","status":"compacting","permissionMode":"default"}`
	m := parseSystem(t, raw)
	p, ok := m.AsStatus()
	if !ok {
		t.Fatalf("AsStatus failed")
	}
	if p.Status == nil || *p.Status != "compacting" {
		t.Errorf("status: %v", p.Status)
	}
}

func TestSystemSubtype_CompactBoundary(t *testing.T) {
	raw := `{"type":"system","subtype":"compact_boundary","session_id":"s1","uuid":"u1","compact_metadata":{"trigger":"auto","pre_tokens":120000,"preserved_segment":{"head_uuid":"h","anchor_uuid":"a","tail_uuid":"t"}}}`
	m := parseSystem(t, raw)
	p, ok := m.AsCompactBoundary()
	if !ok {
		t.Fatalf("AsCompactBoundary failed")
	}
	if p.CompactMetadata.Trigger != "auto" {
		t.Errorf("trigger: %q", p.CompactMetadata.Trigger)
	}
	if p.CompactMetadata.PreTokens != 120000 {
		t.Errorf("pre_tokens: %d", p.CompactMetadata.PreTokens)
	}
	if p.CompactMetadata.PreservedSegment == nil || p.CompactMetadata.PreservedSegment.HeadUUID != "h" {
		t.Errorf("preserved_segment: %+v", p.CompactMetadata.PreservedSegment)
	}
}

func TestSystemSubtype_PostTurnSummary(t *testing.T) {
	raw := `{"type":"system","subtype":"post_turn_summary","session_id":"s1","uuid":"u1","artifact_urls":[],"summarizes_uuid":"x","status_category":"ok","status_detail":"d","title":"T","description":"D","recent_action":"ra","needs_action":"none","is_noteworthy":true}`
	m := parseSystem(t, raw)
	p, ok := m.AsPostTurnSummary()
	if !ok {
		t.Fatalf("AsPostTurnSummary failed")
	}
	if p.Title != "T" || p.Description != "D" {
		t.Errorf("title/desc: %q/%q", p.Title, p.Description)
	}
	if p.StatusCategory != "ok" {
		t.Errorf("status_category: %q", p.StatusCategory)
	}
	if !p.IsNoteworthy {
		t.Error("expected noteworthy")
	}
}

func TestSystemSubtype_APIRetry(t *testing.T) {
	raw := `{"type":"system","subtype":"api_retry","session_id":"s1","uuid":"u1","attempt":2,"max_retries":5,"retry_delay_ms":1000,"error":"overloaded","error_status":529}`
	m := parseSystem(t, raw)
	p, ok := m.AsAPIRetry()
	if !ok {
		t.Fatalf("AsAPIRetry failed")
	}
	if p.Attempt != 2 || p.MaxRetries != 5 || p.RetryDelayMs != 1000 {
		t.Errorf("retry fields: %+v", p)
	}
	if p.Error != "overloaded" {
		t.Errorf("error: %q", p.Error)
	}
	if p.ErrorStatus == nil || *p.ErrorStatus != 529 {
		t.Errorf("error_status: %v", p.ErrorStatus)
	}
}

func TestSystemSubtype_LocalCommandOutput(t *testing.T) {
	raw := `{"type":"system","subtype":"local_command_output","session_id":"s1","uuid":"u1","content":"cost: $0.01"}`
	m := parseSystem(t, raw)
	p, ok := m.AsLocalCommandOutput()
	if !ok {
		t.Fatalf("AsLocalCommandOutput failed")
	}
	if p.Content != "cost: $0.01" {
		t.Errorf("content: %q", p.Content)
	}
}

func TestSystemSubtype_HookStarted(t *testing.T) {
	raw := `{"type":"system","subtype":"hook_started","session_id":"s1","uuid":"u1","hook_id":"h1","hook_name":"PreToolUse","hook_event":"PreToolUse"}`
	m := parseSystem(t, raw)
	p, ok := m.AsHookStarted()
	if !ok {
		t.Fatalf("AsHookStarted failed")
	}
	if p.HookID != "h1" || p.HookName != "PreToolUse" || p.HookEvent != "PreToolUse" {
		t.Errorf("hook fields: %+v", p)
	}
}

func TestSystemSubtype_HookProgress(t *testing.T) {
	raw := `{"type":"system","subtype":"hook_progress","session_id":"s1","uuid":"u1","hook_id":"h1","hook_name":"H","hook_event":"E","stdout":"out","stderr":"err","output":"o"}`
	m := parseSystem(t, raw)
	p, ok := m.AsHookProgress()
	if !ok {
		t.Fatalf("AsHookProgress failed")
	}
	if p.Stdout != "out" || p.Stderr != "err" {
		t.Errorf("stdio: %+v", p)
	}
}

func TestSystemSubtype_HookResponse(t *testing.T) {
	raw := `{"type":"system","subtype":"hook_response","session_id":"s1","uuid":"u1","hook_id":"h1","hook_name":"H","hook_event":"E","output":"done","stdout":"","stderr":"","outcome":"success","exit_code":0}`
	m := parseSystem(t, raw)
	p, ok := m.AsHookResponse()
	if !ok {
		t.Fatalf("AsHookResponse failed")
	}
	if p.Outcome != "success" {
		t.Errorf("outcome: %q", p.Outcome)
	}
	if p.ExitCode == nil || *p.ExitCode != 0 {
		t.Errorf("exit_code: %v", p.ExitCode)
	}
}

func TestSystemSubtype_TaskNotification(t *testing.T) {
	raw := `{"type":"system","subtype":"task_notification","session_id":"s1","uuid":"u1","task_id":"t1","status":"completed","output_file":"/tmp/out","summary":"done","usage":{"total_tokens":100,"tool_uses":2,"duration_ms":500}}`
	m := parseSystem(t, raw)
	p, ok := m.AsTaskNotification()
	if !ok {
		t.Fatalf("AsTaskNotification failed")
	}
	if p.TaskID != "t1" || p.Status != "completed" {
		t.Errorf("task fields: %+v", p)
	}
	if p.Usage.TotalTokens != 100 {
		t.Errorf("usage.total_tokens: %d", p.Usage.TotalTokens)
	}
}

func TestSystemSubtype_TaskStarted(t *testing.T) {
	raw := `{"type":"system","subtype":"task_started","session_id":"s1","uuid":"u1","task_id":"t1","description":"search","task_type":"agent","prompt":"go"}`
	m := parseSystem(t, raw)
	p, ok := m.AsTaskStarted()
	if !ok {
		t.Fatalf("AsTaskStarted failed")
	}
	if p.TaskID != "t1" || p.Description != "search" || p.TaskType != "agent" || p.Prompt != "go" {
		t.Errorf("task fields: %+v", p)
	}
}

func TestSystemSubtype_TaskProgress(t *testing.T) {
	raw := `{"type":"system","subtype":"task_progress","session_id":"s1","uuid":"u1","task_id":"t1","description":"searching","last_tool_name":"Bash","usage":{"total_tokens":42,"tool_uses":1,"duration_ms":100}}`
	m := parseSystem(t, raw)
	p, ok := m.AsTaskProgress()
	if !ok {
		t.Fatalf("AsTaskProgress failed")
	}
	if p.LastToolName != "Bash" {
		t.Errorf("last_tool_name: %q", p.LastToolName)
	}
	if p.Usage.TotalTokens != 42 {
		t.Errorf("usage: %+v", p.Usage)
	}
}

func TestSystemSubtype_SessionStateChanged(t *testing.T) {
	raw := `{"type":"system","subtype":"session_state_changed","session_id":"s1","uuid":"u1","state":"running"}`
	m := parseSystem(t, raw)
	p, ok := m.AsSessionStateChanged()
	if !ok {
		t.Fatalf("AsSessionStateChanged failed")
	}
	if p.State != "running" {
		t.Errorf("state: %q", p.State)
	}
}

func TestSystemSubtype_FilesPersisted(t *testing.T) {
	raw := `{"type":"system","subtype":"files_persisted","session_id":"s1","uuid":"u1","files":[{"filename":"a.txt","file_id":"f1"}],"failed":[{"filename":"b.txt","error":"nope"}],"processed_at":"2026-01-01T00:00:00Z"}`
	m := parseSystem(t, raw)
	p, ok := m.AsFilesPersisted()
	if !ok {
		t.Fatalf("AsFilesPersisted failed")
	}
	if len(p.Files) != 1 || p.Files[0].Filename != "a.txt" || p.Files[0].FileID != "f1" {
		t.Errorf("files: %+v", p.Files)
	}
	if len(p.Failed) != 1 || p.Failed[0].Error != "nope" {
		t.Errorf("failed: %+v", p.Failed)
	}
	if p.ProcessedAt != "2026-01-01T00:00:00Z" {
		t.Errorf("processed_at: %q", p.ProcessedAt)
	}
}

func TestSystemSubtype_ElicitationComplete(t *testing.T) {
	raw := `{"type":"system","subtype":"elicitation_complete","session_id":"s1","uuid":"u1","mcp_server_name":"foo","elicitation_id":"el_1"}`
	m := parseSystem(t, raw)
	p, ok := m.AsElicitationComplete()
	if !ok {
		t.Fatalf("AsElicitationComplete failed")
	}
	if p.MCPServerName != "foo" || p.ElicitationID != "el_1" {
		t.Errorf("fields: %+v", p)
	}
}

func TestSystemDecodePayload_DispatchesAllSubtypes(t *testing.T) {
	cases := []struct {
		check   func(t *testing.T, v any)
		subtype string
		raw     string
	}{
		{subtype: "init", raw: `{"type":"system","subtype":"init","session_id":"s","uuid":"u","tools":["X"],"mcp_servers":[],"slash_commands":[],"skills":[],"plugins":[],"apiKeySource":"none","claude_code_version":"1","cwd":"/","model":"m","permissionMode":"default","output_style":"default"}`, check: func(t *testing.T, v any) {
			if _, ok := v.(*SystemInitPayload); !ok {
				t.Errorf("expected *SystemInitPayload, got %T", v)
			}
		}},
		{subtype: "status", raw: `{"type":"system","subtype":"status","session_id":"s","uuid":"u","status":"compacting"}`, check: func(t *testing.T, v any) {
			if _, ok := v.(*SystemStatusPayload); !ok {
				t.Errorf("wrong type: %T", v)
			}
		}},
		{subtype: "compact_boundary", raw: `{"type":"system","subtype":"compact_boundary","session_id":"s","uuid":"u","compact_metadata":{"trigger":"auto","pre_tokens":1}}`, check: func(t *testing.T, v any) {
			if _, ok := v.(*CompactBoundaryPayload); !ok {
				t.Errorf("wrong type: %T", v)
			}
		}},
		{subtype: "post_turn_summary", raw: `{"type":"system","subtype":"post_turn_summary","session_id":"s","uuid":"u","artifact_urls":[],"summarizes_uuid":"x","status_category":"ok","status_detail":"","title":"t","description":"d","recent_action":"","needs_action":"","is_noteworthy":false}`, check: func(t *testing.T, v any) {
			if _, ok := v.(*PostTurnSummaryPayload); !ok {
				t.Errorf("wrong type: %T", v)
			}
		}},
		{subtype: "api_retry", raw: `{"type":"system","subtype":"api_retry","session_id":"s","uuid":"u","attempt":1,"max_retries":3,"retry_delay_ms":100,"error":"e","error_status":null}`, check: func(t *testing.T, v any) {
			if _, ok := v.(*APIRetryPayload); !ok {
				t.Errorf("wrong type: %T", v)
			}
		}},
		{subtype: "local_command_output", raw: `{"type":"system","subtype":"local_command_output","session_id":"s","uuid":"u","content":"x"}`, check: func(t *testing.T, v any) {
			if _, ok := v.(*LocalCommandOutputPayload); !ok {
				t.Errorf("wrong type: %T", v)
			}
		}},
		{subtype: "hook_started", raw: `{"type":"system","subtype":"hook_started","session_id":"s","uuid":"u","hook_id":"h","hook_name":"n","hook_event":"e"}`, check: func(t *testing.T, v any) {
			if _, ok := v.(*HookStartedPayload); !ok {
				t.Errorf("wrong type: %T", v)
			}
		}},
		{subtype: "hook_progress", raw: `{"type":"system","subtype":"hook_progress","session_id":"s","uuid":"u","hook_id":"h","hook_name":"n","hook_event":"e","stdout":"","stderr":"","output":""}`, check: func(t *testing.T, v any) {
			if _, ok := v.(*HookProgressPayload); !ok {
				t.Errorf("wrong type: %T", v)
			}
		}},
		{subtype: "hook_response", raw: `{"type":"system","subtype":"hook_response","session_id":"s","uuid":"u","hook_id":"h","hook_name":"n","hook_event":"e","output":"","stdout":"","stderr":"","outcome":"success"}`, check: func(t *testing.T, v any) {
			if _, ok := v.(*HookResponsePayload); !ok {
				t.Errorf("wrong type: %T", v)
			}
		}},
		{subtype: "task_notification", raw: `{"type":"system","subtype":"task_notification","session_id":"s","uuid":"u","task_id":"t","status":"completed","output_file":"","summary":""}`, check: func(t *testing.T, v any) {
			if _, ok := v.(*TaskNotificationPayload); !ok {
				t.Errorf("wrong type: %T", v)
			}
		}},
		{subtype: "task_started", raw: `{"type":"system","subtype":"task_started","session_id":"s","uuid":"u","task_id":"t","description":"d"}`, check: func(t *testing.T, v any) {
			if _, ok := v.(*TaskStartedPayload); !ok {
				t.Errorf("wrong type: %T", v)
			}
		}},
		{subtype: "task_progress", raw: `{"type":"system","subtype":"task_progress","session_id":"s","uuid":"u","task_id":"t","description":"d","usage":{"total_tokens":0,"tool_uses":0,"duration_ms":0}}`, check: func(t *testing.T, v any) {
			if _, ok := v.(*TaskProgressPayload); !ok {
				t.Errorf("wrong type: %T", v)
			}
		}},
		{subtype: "session_state_changed", raw: `{"type":"system","subtype":"session_state_changed","session_id":"s","uuid":"u","state":"idle"}`, check: func(t *testing.T, v any) {
			if _, ok := v.(*SessionStateChangedPayload); !ok {
				t.Errorf("wrong type: %T", v)
			}
		}},
		{subtype: "files_persisted", raw: `{"type":"system","subtype":"files_persisted","session_id":"s","uuid":"u","files":[],"failed":[],"processed_at":""}`, check: func(t *testing.T, v any) {
			if _, ok := v.(*FilesPersistedPayload); !ok {
				t.Errorf("wrong type: %T", v)
			}
		}},
		{subtype: "elicitation_complete", raw: `{"type":"system","subtype":"elicitation_complete","session_id":"s","uuid":"u","mcp_server_name":"m","elicitation_id":"e"}`, check: func(t *testing.T, v any) {
			if _, ok := v.(*ElicitationCompletePayload); !ok {
				t.Errorf("wrong type: %T", v)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.subtype, func(t *testing.T) {
			m := parseSystem(t, tc.raw)
			v, err := m.DecodePayload()
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if v == nil {
				t.Fatal("expected non-nil payload")
			}
			tc.check(t, v)
		})
	}
}

func TestSystemDecodePayload_UnknownReturnsNil(t *testing.T) {
	m := parseSystem(t, `{"type":"system","subtype":"brand_new","session_id":"s","uuid":"u"}`)
	v, err := m.DecodePayload()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v != nil {
		t.Errorf("expected nil payload for unknown subtype, got %T", v)
	}
}
