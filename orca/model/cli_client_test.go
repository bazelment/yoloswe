package model_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
