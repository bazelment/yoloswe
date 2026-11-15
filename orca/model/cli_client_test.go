package model_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/bazelment/yoloswe/orca/model"
)

func cliScript(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cli")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestCodexRecordedToolLoop(t *testing.T) {
	t.Parallel()
	call, err := filepath.Abs("testdata/codex_exec_call.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	final, err := filepath.Abs("testdata/codex_exec_final.jsonl")
	if err != nil {
		t.Fatal(err)
	}
	promptPath := filepath.Join(t.TempDir(), "prompt")
	script := cliScript(t, "cat > '"+promptPath+"'\ncase $(cat '"+promptPath+"') in\n*tool_result*) cat '"+final+"';;\n*) cat '"+call+"';;\nesac\n")
	client := model.NewCLIClient("codex-cli", model.WithCLIPath(script))
	reg := model.NewToolRegistry()
	called := 0
	reg.Register(model.ToolDefinition{Name: "calc", Description: "Add two integers", InputSchema: json.RawMessage(`{"type":"object","properties":{"a":{"type":"integer"},"b":{"type":"integer"}}}`)}, func(_ context.Context, input json.RawMessage) (string, error) {
		if string(input) != `{"a":2,"b":3}` {
			t.Errorf("input=%s", input)
		}
		called++
		return "5", nil
	})
	resp, err := client.CreateMessage(t.Context(), model.Request{Model: "test-model", ReasoningEffort: "high", Messages: []model.Message{model.NewUserMessage("Add 2 and 3")}, ToolRegistry: reg})
	if err != nil {
		t.Fatal(err)
	}
	if called != 1 || resp.Turns != 2 || resp.Text() != "The result is 5." {
		t.Fatalf("calls=%d response=%+v", called, resp)
	}
	if len(resp.Transcript) != 4 || resp.Transcript[1].ToolCalls()[0].ID != "call-1" || resp.Transcript[2].Content[0].Content != "5" {
		t.Fatalf("transcript=%+v", resp.Transcript)
	}
	if got := resp.Usage; got.InputTokens != 9805 || got.CacheReadInputTokens != 18176 || got.OutputTokens != 54 {
		t.Fatalf("usage=%+v", got)
	}
	if client.TotalUsage() != resp.Usage {
		t.Fatalf("total=%+v response=%+v", client.TotalUsage(), resp.Usage)
	}
	prompt, err := os.ReadFile(promptPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{`"type":"tool_use"`, `"id":"call-1"`, `"tool_use_id":"call-1"`, `"content":"5"`, `Add two integers`, `"input_schema"`, `"a":{"type":"integer"}`} {
		if !strings.Contains(string(prompt), fragment) {
			t.Errorf("prompt missing %s", fragment)
		}
	}
	client.ResetUsage()
	if client.TotalUsage() != (model.Usage{}) {
		t.Fatal("ResetUsage failed")
	}
}

func TestCLIResponses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, provider, output, want string
		wantError                    bool
	}{
		{"claude text", "claude-cli", `{"result":"{\"text\":\"hello\"}","usage":{"input_tokens":3,"output_tokens":2}}`, "hello", false},
		{"codex plain text", "codex-cli", "{\"type\":\"item.completed\",\"item\":{\"type\":\"agent_message\",\"text\":\"hello\"}}\n{\"type\":\"turn.completed\"}", "hello", false},
		{"failed turn", "codex-cli", `{"type":"turn.failed","error":{"message":"private"}}`, "", true},
		{"error event", "codex-cli", `{"type":"error","message":"private"}`, "", true},
		{"malformed stream", "codex-cli", `not-json`, "", true},
		{"truncated stream", "codex-cli", `{"type":"turn.started"}`, "", true},
		{"empty stream", "codex-cli", "", "", true},
		{"empty completed", "codex-cli", `{"type":"turn.completed"}`, "", true},
		{"claude error", "claude-cli", `{"is_error":true,"result":"private"}`, "", true},
		{"claude malformed", "claude-cli", `not-json`, "", true},
		{"invalid tool input", "claude-cli", `{"result":"{\"tool\":\"calc\",\"input\":[]}"}`, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			fixture := filepath.Join(t.TempDir(), "output")
			if err := os.WriteFile(fixture, []byte(tc.output), 0600); err != nil {
				t.Fatal(err)
			}
			client := model.NewCLIClient(tc.provider, model.WithCLIPath(cliScript(t, "cat '"+fixture+"'\n")))
			resp, err := client.CreateMessage(t.Context(), model.Request{})
			if tc.wantError {
				if err == nil {
					t.Fatalf("expected error, got %+v", resp)
				}
				if strings.Contains(err.Error(), "private") {
					t.Fatalf("diagnostic leaked: %s", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if resp.Text() != tc.want {
				t.Fatalf("text=%q", resp.Text())
			}
		})
	}
}

func TestCLIFailureAndCancellation(t *testing.T) {
	t.Parallel()
	client := model.NewCLIClient("codex-cli", model.WithCLIPath(cliScript(t, "echo private >&2\nexit 1\n")))
	_, err := client.CreateMessage(t.Context(), model.Request{})
	if err == nil || model.IsRetryable(err) || strings.Contains(err.Error(), "private") {
		t.Fatalf("failure=%v", err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = client.CreateMessage(ctx, model.Request{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel=%v", err)
	}
}

func TestInvestigateTools_ReadFileConfinementAndSymlinkEscape(t *testing.T) {
	t.Parallel()
	runDir := t.TempDir()
	repoDir := t.TempDir()
	outsideDir := t.TempDir()

	if err := os.WriteFile(filepath.Join(runDir, "OBJECTIVE.md"), []byte("line1\nline2\nline3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	secretFile := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(secretFile, []byte("outside-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlinkPath := filepath.Join(runDir, "escape.md")
	if err := os.Symlink(secretFile, symlinkPath); err != nil {
		t.Fatal(err)
	}

	reg := model.NewToolRegistry()
	model.RegisterInvestigationTools(reg, model.InvestigateConfig{
		RunDir:  runDir,
		RepoDir: repoDir,
	})

	// 1. Valid bounded read inside runDir.
	res := reg.Execute(t.Context(), model.ToolCall{
		ID:    "c1",
		Name:  model.ToolReadFile,
		Input: json.RawMessage(`{"path":"OBJECTIVE.md","max_bytes":5}`),
	})
	if res.IsError || !strings.Contains(res.Content, "line1") || !strings.Contains(res.Content, "...[truncated]") {
		t.Fatalf("valid read_file = %+v", res)
	}

	// 2. Relative path traversal rejected.
	if res := reg.Execute(t.Context(), model.ToolCall{
		ID:    "c2",
		Name:  model.ToolReadFile,
		Input: json.RawMessage(`{"path":"../secret.txt"}`),
	}); !res.IsError {
		t.Fatalf("expected path traversal rejection, got %+v", res)
	}

	// 3. Relative symlink escaping runDir rejected.
	if res := reg.Execute(t.Context(), model.ToolCall{
		ID:    "c3",
		Name:  model.ToolReadFile,
		Input: json.RawMessage(`{"path":"escape.md"}`),
	}); !res.IsError {
		t.Fatalf("expected relative symlink escape rejection, got %+v", res)
	}

	// 4. Absolute path to symlink inside runDir pointing outside runDir rejected.
	absReq, _ := json.Marshal(model.ReadFileParams{
		Path: symlinkPath,
	})
	if res := reg.Execute(t.Context(), model.ToolCall{
		ID:    "c4",
		Name:  model.ToolReadFile,
		Input: absReq,
	}); !res.IsError {
		t.Fatalf("expected absolute symlink escape rejection, got %+v", res)
	}
}

func TestValidateReadOnlyCommand(t *testing.T) {
	t.Parallel()
	allowed := [][]string{
		{"gh", "pr", "view", "417"},
		{"gh", "pr", "checks", "417"},
		{"gh", "run", "view", "12345"},
		{"nebula", "post", "get", "286c9d4f-9da3-4787-aa66-bd97aa21f947"},
		{"nebula", "mail", "list"},
		{"nebula", "session", "list"},
	}
	for _, cmd := range allowed {
		if err := model.ValidateReadOnlyCommand(cmd[0], cmd[1:]); err != nil {
			t.Errorf("ValidateReadOnlyCommand(%v) unexpected error: %v", cmd, err)
		}
	}

	denied := [][]string{
		{"gh", "api", "repos/bazelment/yoloswe/pulls/417/merge"},
		{"gh", "pr", "merge", "417"},
		{"gh", "pr", "view", "417", "--web"},
		{"gh", "pr", "view", "417", "-w"},
		{"nebula", "mail", "send", "--body", "hi"},
		{"nebula", "phase", "report", "--outcome", "done"},
		{"bash", "-c", "echo hi"},
	}
	for _, cmd := range denied {
		if err := model.ValidateReadOnlyCommand(cmd[0], cmd[1:]); err == nil {
			t.Errorf("ValidateReadOnlyCommand(%v) expected error, got nil", cmd)
		}
	}
}

func TestMCPToolBridge_SerializationMaxCallsAndHTTPGuards(t *testing.T) {
	t.Parallel()
	reg := model.NewToolRegistry()

	var inFlight atomic.Int32
	var maxConcurrent atomic.Int32
	entered := make(chan struct{}, 8)
	release := make(chan struct{})

	reg.Register(model.ToolDefinition{
		Name:        "lane.spawn",
		Description: "Spawn a lane",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(_ context.Context, _ json.RawMessage) (string, error) {
		cur := inFlight.Add(1)
		for {
			prev := maxConcurrent.Load()
			if cur <= prev || maxConcurrent.CompareAndSwap(prev, cur) {
				break
			}
		}
		entered <- struct{}{}
		<-release
		inFlight.Add(-1)
		return "ok", nil
	})

	bridge := model.NewMCPToolBridge(reg)
	bridge.SetMaxCalls(2)

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := bridge.HandleToolCall(t.Context(), "lane_spawn", json.RawMessage(`{}`)); err != nil {
				t.Errorf("HandleToolCall error: %v", err)
			}
		}()
	}

	// First call enters handler; second call must wait on bridge.execMu.
	<-entered
	close(release)
	<-entered
	wg.Wait()

	if got := maxConcurrent.Load(); got != 1 {
		t.Fatalf("max concurrent tool handler executions = %d, want 1 (serialized)", got)
	}

	// Third call exceeds MaxCalls(2) and returns an error result.
	res, err := bridge.HandleToolCall(t.Context(), "lane_spawn", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("HandleToolCall 3 error: %v", err)
	}
	if res == nil || !res.IsError {
		t.Fatalf("expected 3rd call to be rejected by MaxCalls, got %+v", res)
	}

	// Clearing registry rejects late calls after turn ends.
	bridge.SetRegistry(nil)
	lateRes, err := bridge.HandleToolCall(t.Context(), "lane_spawn", json.RawMessage(`{}`))
	if err != nil {
		t.Fatalf("late HandleToolCall error: %v", err)
	}
	if lateRes == nil || !lateRes.IsError {
		t.Fatalf("expected late call after SetRegistry(nil) to fail, got %+v", lateRes)
	}

	// ServeHTTP guards: reject GET, Origin header, and non-JSON Content-Type.
	getRec := httptest.NewRecorder()
	bridge.ServeHTTP(getRec, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if getRec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /mcp status = %d, want %d", getRec.Code, http.StatusMethodNotAllowed)
	}

	originReq := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(`{}`)))
	originReq.Header.Set("Origin", "https://evil.example")
	originReq.Header.Set("Content-Type", "application/json")
	originRec := httptest.NewRecorder()
	bridge.ServeHTTP(originRec, originReq)
	if originRec.Code != http.StatusForbidden {
		t.Fatalf("POST /mcp with Origin status = %d, want %d", originRec.Code, http.StatusForbidden)
	}

	ctReq := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(`{}`)))
	ctReq.Header.Set("Content-Type", "text/plain")
	ctRec := httptest.NewRecorder()
	bridge.ServeHTTP(ctRec, ctReq)
	if ctRec.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("POST /mcp with text/plain status = %d, want %d", ctRec.Code, http.StatusUnsupportedMediaType)
	}
}

func TestMCPToolBridge_WireNameMapping(t *testing.T) {
	t.Parallel()
	reg := model.NewToolRegistry()
	var observedName string
	reg.Register(model.ToolDefinition{
		Name:        "read.lane_report",
		Description: "Read lane report",
		InputSchema: json.RawMessage(`{"type":"object"}`),
	}, func(_ context.Context, _ json.RawMessage) (string, error) {
		return "report-body", nil
	})
	bridge := model.NewMCPToolBridge(reg)
	bridge.SetObserver(func(name string, _ json.RawMessage, _ string, _ bool) {
		observedName = name
	})
	tools := bridge.Tools()
	if len(tools) != 1 || tools[0].Name != "read_lane_report" {
		t.Fatalf("Tools = %+v, want [read_lane_report]", tools)
	}
	res, err := bridge.HandleToolCall(t.Context(), "mcp__orca__read_lane_report", json.RawMessage(`{}`))
	if err != nil || res == nil || res.IsError {
		t.Fatalf("HandleToolCall(mcp__orca__read_lane_report) = %+v, %v", res, err)
	}
	if observedName != "read.lane_report" {
		t.Fatalf("observed canonical tool name = %q, want read.lane_report", observedName)
	}
}

func TestCLIMultiCallArrayAndCostFallbacks(t *testing.T) {
	t.Parallel()
	t.Run("array reply deduplicates colliding ids and uses total_cost_usd", func(t *testing.T) {
		t.Parallel()
		fixture := filepath.Join(t.TempDir(), "output")
		out := `{"result":"[{\"id\":\"dup\",\"tool\":\"directive.revoke\",\"input\":{\"id\":\"h1\"}},{\"id\":\"dup\",\"name\":\"lane.spawn\",\"input\":{\"item_id\":\"m4\"}}]","total_cost_usd":0.042,"usage":{"input_tokens":100,"output_tokens":20}}`
		if err := os.WriteFile(fixture, []byte(out), 0600); err != nil {
			t.Fatal(err)
		}
		client := model.NewCLIClient("claude-cli", model.WithCLIPath(cliScript(t, "cat '"+fixture+"'\n")))
		resp, err := client.CreateMessage(t.Context(), model.Request{Model: "sonnet"})
		if err != nil {
			t.Fatal(err)
		}
		calls := resp.ToolCalls()
		if len(calls) != 2 {
			t.Fatalf("ToolCalls = %+v, want 2", calls)
		}
		if calls[0].ID == calls[1].ID || calls[0].ID != "dup" || calls[1].ID != "cli-tool-2" {
			t.Fatalf("ToolCalls IDs = %q, %q; want unique IDs (dup, cli-tool-2)", calls[0].ID, calls[1].ID)
		}
		if resp.Usage.CostUSD != 0.042 {
			t.Fatalf("CostUSD = %v, want 0.042", resp.Usage.CostUSD)
		}
	})

	t.Run("array reply mixed unnamed entry returns error", func(t *testing.T) {
		t.Parallel()
		fixture := filepath.Join(t.TempDir(), "output")
		out := `{"result":"[{\"tool\":\"directive.revoke\",\"input\":{\"id\":\"h1\"}},{\"input\":{\"item_id\":\"m4\"}}]","usage":{"input_tokens":10,"output_tokens":5}}`
		if err := os.WriteFile(fixture, []byte(out), 0600); err != nil {
			t.Fatal(err)
		}
		client := model.NewCLIClient("claude-cli", model.WithCLIPath(cliScript(t, "cat '"+fixture+"'\n")))
		if _, err := client.CreateMessage(t.Context(), model.Request{Model: "sonnet"}); err == nil {
			t.Fatal("expected error for unnamed entry in tool call array")
		}
	})

	t.Run("pricing family fallback calculates cost without false o3 substring match", func(t *testing.T) {
		t.Parallel()
		if got := model.CalculateCost("claude-sonnet-4-6-custom", model.Usage{InputTokens: 1_000_000}, nil); got != 3.0 {
			t.Fatalf("CalculateCost(claude-sonnet-4-6-custom) = %v, want 3.0", got)
		}
		if got := model.CalculateCost("custom-foo3-bar", model.Usage{InputTokens: 1_000_000}, nil); got != 0 {
			t.Fatalf("CalculateCost(custom-foo3-bar) = %v, want 0 for unknown model", got)
		}
	})
}
