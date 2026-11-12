package model_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orch/model"
)

type spawnParams struct {
	ItemID string `json:"item_id" jsonschema:"required,description=Backlog item ID"`
	Brief  string `json:"brief" jsonschema:"required,description=Brief for the lane"`
	Round  int    `json:"round,omitempty"`
}

func TestHTTPClient_SingleTurn(t *testing.T) {
	t.Parallel()

	var (
		gotMethod     string
		gotPath       string
		gotAPIKey     string
		gotAuthHeader string
		gotCustomHdr  string
		gotVersion    string
		gotBody       []byte
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAPIKey = r.Header.Get("x-api-key")
		gotAuthHeader = r.Header.Get("Authorization")
		gotCustomHdr = r.Header.Get("X-Gateway-Tenant")
		gotVersion = r.Header.Get("anthropic-version")
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id": "msg_single_1",
			"type": "message",
			"role": "assistant",
			"model": "claude-sonnet-4-5-20250929",
			"stop_reason": "end_turn",
			"content": [
				{"type": "thinking", "thinking": "Preparing greeting.", "signature": "sig_1"},
				{"type": "text", "text": "Hello from Anthropic Messages API."}
			],
			"usage": {
				"input_tokens": 1000,
				"output_tokens": 200,
				"cache_creation_input_tokens": 100,
				"cache_read_input_tokens": 500
			}
		}`)
	}))
	defer srv.Close()

	// Point BaseURL with a gateway subpath + /v1 to verify URL normalization.
	client := model.NewHTTPClient(
		model.WithBaseURL(srv.URL+"/anthropic/v1/"),
		model.WithAPIKey("sk-ant-test"),
		model.WithAuthToken("bearer-tok"),
		model.WithHeader("X-Gateway-Tenant", "nebula"),
	)

	resp, err := client.CreateMessage(t.Context(), model.Request{
		Model:     "claude-sonnet-4-5-20250929",
		System:    "You are a concise assistant.",
		Messages:  []model.Message{model.NewUserMessage("Say hello")},
		MaxTokens: 512,
	})
	if err != nil {
		t.Fatalf("CreateMessage error: %v", err)
	}

	if gotMethod != http.MethodPost || gotPath != "/anthropic/v1/messages" {
		t.Fatalf("request = %s %s, want POST /anthropic/v1/messages", gotMethod, gotPath)
	}
	if gotAPIKey != "sk-ant-test" {
		t.Fatalf("x-api-key = %q, want sk-ant-test", gotAPIKey)
	}
	if gotAuthHeader != "Bearer bearer-tok" {
		t.Fatalf("Authorization = %q, want Bearer bearer-tok", gotAuthHeader)
	}
	if gotCustomHdr != "nebula" {
		t.Fatalf("X-Gateway-Tenant = %q, want nebula", gotCustomHdr)
	}
	if gotVersion != model.DefaultAnthropicVersion {
		t.Fatalf("anthropic-version = %q, want %q", gotVersion, model.DefaultAnthropicVersion)
	}
	if !strings.Contains(string(gotBody), `"system":"You are a concise assistant."`) {
		t.Fatalf("request body missing system prompt: %s", string(gotBody))
	}
	if resp.ID != "msg_single_1" || resp.StopReason != model.StopReasonEndTurn {
		t.Fatalf("unexpected response metadata: %+v", resp)
	}
	if resp.Text() != "Hello from Anthropic Messages API." {
		t.Fatalf("resp.Text() = %q", resp.Text())
	}
	if resp.Message.Thinking() != "Preparing greeting." {
		t.Fatalf("resp.Message.Thinking() = %q", resp.Message.Thinking())
	}
	if resp.Usage.TotalTokens() != 1800 {
		t.Fatalf("TotalTokens() = %d, want 1800", resp.Usage.TotalTokens())
	}
	// Sonnet pricing: 1000*3/1M + 200*15/1M + 100*3.75/1M + 500*0.3/1M = 0.006525
	const wantCost = 0.006525
	if diff := resp.Usage.CostUSD - wantCost; diff < -1e-9 || diff > 1e-9 {
		t.Fatalf("CostUSD = %v, want %v", resp.Usage.CostUSD, wantCost)
	}
	if total := client.TotalUsage(); total.TotalTokens() != 1800 || total.CostUSD != resp.Usage.CostUSD {
		t.Fatalf("client.TotalUsage() = %+v", total)
	}
}

func TestHTTPClient_ToolLoop(t *testing.T) {
	t.Parallel()

	type calcParams struct {
		Op string `json:"op" jsonschema:"required,enum=add,enum=mul,description=Operation"`
		A  int    `json:"a" jsonschema:"required,description=First operand"`
		B  int    `json:"b" jsonschema:"required,description=Second operand"`
	}
	type calcResult struct {
		Answer int `json:"answer"`
	}

	reg := model.NewToolRegistry()
	model.AddToolJSON(reg, "calc", "Perform arithmetic", func(_ context.Context, p calcParams) (calcResult, error) {
		switch p.Op {
		case "add":
			return calcResult{Answer: p.A + p.B}, nil
		case "mul":
			return calcResult{Answer: p.A * p.B}, nil
		default:
			return calcResult{}, fmt.Errorf("unsupported op %q", p.Op)
		}
	})

	var turnCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn := int(turnCount.Add(1))
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		var req model.Request
		if err := json.Unmarshal(body, &req); err != nil {
			t.Errorf("decode request: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		switch turn {
		case 1:
			if len(req.Tools) != 1 || req.Tools[0].Name != "calc" {
				t.Errorf("turn 1 tools = %+v", req.Tools)
			}
			_, _ = io.WriteString(w, `{
				"id": "msg_loop_1",
				"type": "message",
				"role": "assistant",
				"model": "claude-opus-4-6",
				"stop_reason": "tool_use",
				"content": [
					{"type": "text", "text": "Computing 6 + 7 first."},
					{"type": "tool_use", "id": "call_add", "name": "calc", "input": {"op": "add", "a": 6, "b": 7}}
				],
				"usage": {"input_tokens": 200, "output_tokens": 50}
			}`)
		case 2:
			if len(req.Messages) != 3 {
				t.Errorf("turn 2 len(messages) = %d, want 3", len(req.Messages))
			} else {
				toolRes := req.Messages[2].Content
				if len(toolRes) != 1 || toolRes[0].Type != model.BlockTypeToolResult || toolRes[0].ToolUseID != "call_add" {
					t.Errorf("turn 2 tool_result block = %+v", toolRes)
				}
				if toolRes[0].Content != `{"answer":13}` || toolRes[0].IsError {
					t.Errorf("turn 2 tool_result content = %q (is_error=%v)", toolRes[0].Content, toolRes[0].IsError)
				}
			}
			_, _ = io.WriteString(w, `{
				"id": "msg_loop_2",
				"type": "message",
				"role": "assistant",
				"model": "claude-opus-4-6",
				"stop_reason": "tool_use",
				"content": [
					{"type": "text", "text": "Now multiplying 13 by 3."},
					{"type": "tool_use", "id": "call_mul", "name": "calc", "input": {"op": "mul", "a": 13, "b": 3}}
				],
				"usage": {"input_tokens": 300, "output_tokens": 60}
			}`)
		case 3:
			if len(req.Messages) != 5 {
				t.Errorf("turn 3 len(messages) = %d, want 5", len(req.Messages))
			} else {
				toolRes := req.Messages[4].Content
				if len(toolRes) != 1 || toolRes[0].Content != `{"answer":39}` {
					t.Errorf("turn 3 tool_result = %+v", toolRes)
				}
			}
			_, _ = io.WriteString(w, `{
				"id": "msg_loop_3",
				"type": "message",
				"role": "assistant",
				"model": "claude-opus-4-6",
				"stop_reason": "end_turn",
				"content": [
					{"type": "text", "text": "The result is 39."}
				],
				"usage": {"input_tokens": 400, "output_tokens": 40}
			}`)
		default:
			t.Errorf("unexpected turn %d", turn)
		}
	}))
	defer srv.Close()

	client := model.NewHTTPClient(
		model.WithBaseURL(srv.URL),
		model.WithAPIKey("test-key"),
	)

	var seenToolCalls []string
	res, err := client.RunToolLoop(
		t.Context(),
		model.Request{
			Model:     "claude-opus-4-6",
			System:    "Use calc for math.",
			Messages:  []model.Message{model.NewUserMessage("Compute (6 + 7) * 3")},
			MaxTokens: 1024,
		},
		reg,
		model.WithMaxTurns(5),
		model.WithOnToolCall(func(_ int, call model.ToolCall, result model.ContentBlock) {
			seenToolCalls = append(seenToolCalls, fmt.Sprintf("%s->%s", call.ID, result.Content))
		}),
	)
	if err != nil {
		t.Fatalf("RunToolLoop error: %v", err)
	}

	if res.TurnCount != 3 || len(res.Turns) != 3 {
		t.Fatalf("TurnCount = %d (len(Turns)=%d), want 3", res.TurnCount, len(res.Turns))
	}
	if res.Response.Text() != "The result is 39." {
		t.Fatalf("Response.Text() = %q, want 'The result is 39.'", res.Response.Text())
	}
	if len(seenToolCalls) != 2 {
		t.Fatalf("seenToolCalls = %v, want 2 calls", seenToolCalls)
	}
	if res.Usage.InputTokens != 900 || res.Usage.OutputTokens != 150 {
		t.Fatalf("accumulated Usage = %+v, want input=900 output=150", res.Usage)
	}
	// Opus pricing: 900*15/1M + 150*75/1M = 0.0135 + 0.01125 = 0.02475
	const wantCost = 0.02475
	if diff := res.Usage.CostUSD - wantCost; diff < -1e-9 || diff > 1e-9 {
		t.Fatalf("CostUSD = %v, want %v", res.Usage.CostUSD, wantCost)
	}
}

func TestHTTPClient_ToolLoopMaxTurnsStop(t *testing.T) {
	t.Parallel()

	reg := model.NewToolRegistry()
	model.AddTool(reg, "ping", "Ping service", func(_ context.Context, _ struct{}) (string, error) {
		return "pong", nil
	})

	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{
			"id": "msg_inf_%d",
			"type": "message",
			"role": "assistant",
			"model": "claude-sonnet-4-5-20250929",
			"stop_reason": "tool_use",
			"content": [
				{"type": "tool_use", "id": "call_%d", "name": "ping", "input": {}}
			],
			"usage": {"input_tokens": 100, "output_tokens": 20}
		}`, n, n)
	}))
	defer srv.Close()

	client := model.NewHTTPClient(
		model.WithBaseURL(srv.URL),
		model.WithAPIKey("test-key"),
	)

	res, err := client.RunToolLoop(
		t.Context(),
		model.Request{
			Model:     "claude-sonnet-4-5-20250929",
			Messages:  []model.Message{model.NewUserMessage("Keep pinging")},
			MaxTokens: 256,
		},
		reg,
		model.WithMaxTurns(2),
	)
	if !errors.Is(err, model.ErrMaxTurnsExceeded) {
		t.Fatalf("err = %v, want ErrMaxTurnsExceeded", err)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("server call count = %d, want 2", got)
	}
	if res == nil || res.TurnCount != 2 || res.Usage.InputTokens != 200 {
		t.Fatalf("partial result = %+v, want TurnCount=2 InputTokens=200", res)
	}
}

func TestHTTPClient_RetriesOn429And529(t *testing.T) {
	t.Parallel()

	t.Run("recovers after 429 and 529", func(t *testing.T) {
		t.Parallel()

		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			n := attempts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			switch n {
			case 1:
				w.Header().Set("retry-after-ms", "1")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"rate limited"}}`)
			case 2:
				w.WriteHeader(529)
				_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`)
			default:
				_, _ = io.WriteString(w, `{
					"id": "msg_ok",
					"type": "message",
					"role": "assistant",
					"model": "claude-haiku-4-5-20251001",
					"stop_reason": "end_turn",
					"content": [{"type": "text", "text": "recovered"}],
					"usage": {"input_tokens": 50, "output_tokens": 10}
				}`)
			}
		}))
		defer srv.Close()

		client := model.NewHTTPClient(
			model.WithBaseURL(srv.URL),
			model.WithAPIKey("test-key"),
			model.WithMaxRetries(3),
			model.WithRetryBackoff(time.Millisecond, 5*time.Millisecond),
		)

		resp, err := client.CreateMessage(t.Context(), model.Request{
			Model:    "claude-haiku-4-5-20251001",
			Messages: []model.Message{model.NewUserMessage("hi")},
		})
		if err != nil {
			t.Fatalf("CreateMessage failed after retries: %v", err)
		}
		if attempts.Load() != 3 {
			t.Fatalf("attempts = %d, want 3", attempts.Load())
		}
		if resp.Text() != "recovered" {
			t.Fatalf("resp.Text() = %q, want recovered", resp.Text())
		}
	})

	t.Run("exhausts retries on persistent 429", func(t *testing.T) {
		t.Parallel()

		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			attempts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("request-id", "req_429")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error","message":"too many requests"}}`)
		}))
		defer srv.Close()

		client := model.NewHTTPClient(
			model.WithBaseURL(srv.URL),
			model.WithAPIKey("test-key"),
			model.WithMaxRetries(2),
			model.WithRetryBackoff(time.Millisecond, 2*time.Millisecond),
		)

		_, err := client.CreateMessage(t.Context(), model.Request{
			Model:    "claude-sonnet-4-5-20250929",
			Messages: []model.Message{model.NewUserMessage("hi")},
		})
		if !model.IsRateLimit(err) || !model.IsRetryable(err) {
			t.Fatalf("expected rate-limit retryable error, got: %v", err)
		}
		var apiErr *model.APIError
		if !errors.As(err, &apiErr) || apiErr.Attempts != 3 || apiErr.RequestID != "req_429" {
			t.Fatalf("unexpected APIError: %+v", apiErr)
		}
		if attempts.Load() != 3 {
			t.Fatalf("attempts = %d, want 3", attempts.Load())
		}
	})

	t.Run("exhausts retries on persistent 529", func(t *testing.T) {
		t.Parallel()

		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			attempts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(529)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"overloaded_error","message":"server overloaded"}}`)
		}))
		defer srv.Close()

		client := model.NewHTTPClient(
			model.WithBaseURL(srv.URL),
			model.WithAPIKey("test-key"),
			model.WithMaxRetries(1),
			model.WithRetryBackoff(time.Millisecond, 2*time.Millisecond),
		)

		_, err := client.CreateMessage(t.Context(), model.Request{
			Model:    "claude-sonnet-4-5-20250929",
			Messages: []model.Message{model.NewUserMessage("hi")},
		})
		if !model.IsOverloaded(err) || !model.IsRetryable(err) {
			t.Fatalf("expected overloaded retryable error, got: %v", err)
		}
		if attempts.Load() != 2 {
			t.Fatalf("attempts = %d, want 2", attempts.Load())
		}
	})

	t.Run("does not retry non-retryable 400", func(t *testing.T) {
		t.Parallel()

		var attempts atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			attempts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error","message":"max_tokens is too large"}}`)
		}))
		defer srv.Close()

		client := model.NewHTTPClient(
			model.WithBaseURL(srv.URL),
			model.WithAPIKey("test-key"),
			model.WithMaxRetries(3),
			model.WithRetryBackoff(time.Millisecond, 2*time.Millisecond),
		)

		_, err := client.CreateMessage(t.Context(), model.Request{
			Model:    "claude-sonnet-4-5-20250929",
			Messages: []model.Message{model.NewUserMessage("hi")},
		})
		if err == nil || model.IsRetryable(err) {
			t.Fatalf("expected non-retryable error, got: %v", err)
		}
		if attempts.Load() != 1 {
			t.Fatalf("attempts = %d, want 1 (no retries on 400)", attempts.Load())
		}
	})
}

func TestRecordAndReplayFake(t *testing.T) {
	t.Parallel()

	reg := model.NewToolRegistry()
	model.AddTool(reg, "lane_spawn", "Spawn a lane for a backlog item", func(_ context.Context, p spawnParams) (string, error) {
		if p.ItemID == "" {
			return "", fmt.Errorf("item_id is required")
		}
		return fmt.Sprintf("spawned %s", p.ItemID), nil
	})

	var srvCalls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		n := srvCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if n == 1 {
			_, _ = io.WriteString(w, `{
				"id": "msg_r_1",
				"type": "message",
				"role": "assistant",
				"model": "claude-sonnet-4-5-20250929",
				"stop_reason": "tool_use",
				"content": [
					{"type": "text", "text": "Spawning item-9."},
					{"type": "tool_use", "id": "call_r_1", "name": "lane_spawn", "input": {"item_id": "item-9", "brief": "fix bug"}}
				],
				"usage": {"input_tokens": 150, "output_tokens": 40}
			}`)
			return
		}
		_, _ = io.WriteString(w, `{
			"id": "msg_r_2",
			"type": "message",
			"role": "assistant",
			"model": "claude-sonnet-4-5-20250929",
			"stop_reason": "end_turn",
			"content": [
				{"type": "text", "text": "Spawned item-9."}
			],
			"usage": {"input_tokens": 210, "output_tokens": 20}
		}`)
	}))

	httpClient := model.NewHTTPClient(
		model.WithBaseURL(srv.URL),
		model.WithAPIKey("test-key"),
	)

	recordFile := filepath.Join(t.TempDir(), "recorded_loop.json")
	recorder, err := model.NewRecordOrReplayClient(recordFile, true, httpClient)
	if err != nil {
		t.Fatalf("NewRecordOrReplayClient(record=true): %v", err)
	}

	req := model.Request{
		Model:     "claude-sonnet-4-5-20250929",
		System:    "You are the dispatcher.",
		Messages:  []model.Message{model.NewUserMessage("Dispatch item-9")},
		MaxTokens: 512,
	}

	recRes, err := model.RunToolLoop(t.Context(), recorder, req, reg)
	if err != nil {
		t.Fatalf("RunToolLoop(recorder) error: %v", err)
	}
	srv.Close()

	if len(recorder.Exchanges()) != 2 {
		t.Fatalf("len(recorder.Exchanges()) = %d, want 2", len(recorder.Exchanges()))
	}

	// Now replay deterministically from the saved JSON file with the server closed.
	replayer, err := model.NewRecordOrReplayClient(recordFile, false, nil)
	if err != nil {
		t.Fatalf("NewRecordOrReplayClient(record=false): %v", err)
	}
	replayer.SetStrictMatch(true)

	replayRes, err := model.RunToolLoop(t.Context(), replayer, req, reg)
	if err != nil {
		t.Fatalf("RunToolLoop(replayer) error: %v", err)
	}

	if replayRes.Response.Text() != recRes.Response.Text() {
		t.Fatalf("replay text = %q, want %q", replayRes.Response.Text(), recRes.Response.Text())
	}
	if replayRes.TurnCount != 2 || replayRes.Usage.TotalTokens() != recRes.Usage.TotalTokens() {
		t.Fatalf("replay stats mismatch: got turns=%d tokens=%d, want turns=%d tokens=%d",
			replayRes.TurnCount, replayRes.Usage.TotalTokens(), recRes.TurnCount, recRes.Usage.TotalTokens())
	}
}

func TestReplayFromTestdataFixture(t *testing.T) {
	t.Parallel()

	reg := model.NewToolRegistry()
	var spawnedItems []string
	model.AddTool(reg, "lane_spawn", "Spawn a lane for a backlog item", func(_ context.Context, p spawnParams) (string, error) {
		spawnedItems = append(spawnedItems, p.ItemID)
		return fmt.Sprintf("spawned %s", p.ItemID), nil
	})

	replayClient, err := model.LoadFakeClient("testdata/tool_loop_replay.json")
	if err != nil {
		t.Fatalf("LoadFakeClient: %v", err)
	}
	replayClient.SetStrictMatch(true)

	resp, err := replayClient.CreateMessage(t.Context(), model.Request{
		Model:        "claude-sonnet-4-5-20250929",
		System:       "You are the dispatcher.",
		Messages:     []model.Message{model.NewUserMessage("Spawn item-42 on lane alpha.")},
		MaxTokens:    1024,
		ToolRegistry: reg,
	})
	if err != nil {
		t.Fatalf("CreateMessage replay error: %v", err)
	}
	if resp.Text() != "Lane spawned for item-42." {
		t.Fatalf("resp.Text() = %q", resp.Text())
	}
	if resp.Turns != 2 {
		t.Fatalf("resp.Turns = %d, want 2", resp.Turns)
	}
	if len(spawnedItems) != 1 || spawnedItems[0] != "item-42" {
		t.Fatalf("spawnedItems = %v, want [item-42]", spawnedItems)
	}
	if resp.Usage.InputTokens != 1100 || resp.Usage.OutputTokens != 140 {
		t.Fatalf("resp.Usage = %+v, want input=1100 output=140", resp.Usage)
	}
}

func TestToolRegistryAndFakeClient(t *testing.T) {
	t.Parallel()

	reg := model.NewToolRegistry()
	model.AddTool(reg, "lane.spawn", "Spawn a lane for a backlog item", func(_ context.Context, p spawnParams) (string, error) {
		if p.ItemID == "" {
			return "", fmt.Errorf("item_id is required")
		}
		return fmt.Sprintf("spawned %s", p.ItemID), nil
	})

	defs := reg.Definitions()
	if len(defs) != 1 || defs[0].Name != "lane.spawn" {
		t.Fatalf("Definitions() = %+v", defs)
	}
	if !strings.Contains(string(defs[0].InputSchema), `"item_id"`) {
		t.Fatalf("InputSchema missing item_id: %s", string(defs[0].InputSchema))
	}

	callInput := json.RawMessage(`{"item_id":"item-1","brief":"do work"}`)
	client := model.NewFakeClient(model.Response{
		ID:         "msg-1",
		Model:      "claude-sonnet-4-5",
		StopReason: model.StopReasonToolUse,
		Message: model.NewAssistantMessage(
			model.NewTextBlock("Spawning lane."),
			model.NewToolUseBlock("call-1", "lane.spawn", callInput),
		),
		Usage: model.Usage{
			InputTokens:  120,
			OutputTokens: 30,
			CostUSD:      0.001,
		},
	})

	resp, err := client.CreateMessage(t.Context(), model.Request{
		Model:     "claude-sonnet-4-5",
		System:    "You are the dispatcher.",
		Messages:  []model.Message{model.NewUserMessage("Snapshot: 1 open item")},
		Tools:     defs,
		MaxTokens: 1024,
	})
	if err != nil {
		t.Fatalf("CreateMessage error: %v", err)
	}
	if resp.Text() != "Spawning lane." {
		t.Fatalf("resp.Text() = %q", resp.Text())
	}
	calls := resp.ToolCalls()
	if len(calls) != 1 || calls[0].Name != "lane.spawn" {
		t.Fatalf("ToolCalls() = %+v", calls)
	}

	resBlock := reg.Dispatch(t.Context(), calls[0])
	if resBlock.IsError || resBlock.Content != "spawned item-1" {
		t.Fatalf("Dispatch result = %+v", resBlock)
	}

	totalUsage := resp.Usage.Add(model.Usage{InputTokens: 80, OutputTokens: 20, CostUSD: 0.001})
	if totalUsage.TotalTokens() != 250 {
		t.Fatalf("TotalTokens() = %d, want 250", totalUsage.TotalTokens())
	}
}
