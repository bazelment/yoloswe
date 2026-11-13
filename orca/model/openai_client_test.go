package model_test

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/model"
)

func TestOpenAIResponsesToolLoop(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		requests = append(requests, body)
		w.Header().Set("Content-Type", "application/json")
		if len(requests) == 1 {
			_, _ = w.Write([]byte(`{"id":"r1","model":"gpt-6.1-sol","output":[{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{\"key\":\"x\"}"}],"usage":{"input_tokens":7,"output_tokens":3}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"r2","model":"gpt-6.1-sol","output":[{"type":"message","text":"done"}],"usage":{"input_tokens":9,"output_tokens":2}}`))
	}))
	defer server.Close()
	reg := model.NewToolRegistry()
	model.AddTool(reg, "lookup", "look up a value", func(_ context.Context, in struct {
		Key string `json:"key"`
	}) (string, error) {
		return "value:" + in.Key, nil
	})
	client := model.NewOpenAIClient(
		model.WithOpenAIBaseURL(server.URL), model.WithOpenAIAPIKey("test"), model.WithOpenAIMaxRetries(0),
		model.WithOpenAIPricing(map[string]model.ModelPricing{"gpt-6.1-sol": {InputPerMTok: 1, OutputPerMTok: 2}}),
	)
	res, err := client.RunToolLoop(context.Background(), model.Request{Model: "gpt-6.1-sol", Messages: []model.Message{model.NewUserMessage("find")}, MaxTurns: 3}, reg)
	if err != nil {
		t.Fatal(err)
	}
	if res.Response.Text() != "done" || res.Usage.InputTokens != 16 || res.Usage.OutputTokens != 5 {
		t.Fatalf("response=%q usage=%+v", res.Response.Text(), res.Usage)
	}
	if got, want := res.Usage.CostUSD, 0.000026; math.Abs(got-want) > 1e-12 {
		t.Fatalf("cost=%v, want %v", got, want)
	}
	if got := len(requests); got != 2 {
		t.Fatalf("requests=%d, want 2", got)
	}
	if requests[0]["tools"] == nil {
		t.Fatalf("first request has no function tools: %#v", requests[0])
	}
	items, ok := requests[1]["input"].([]any)
	if !ok || len(items) < 3 {
		t.Fatalf("second input=%#v", requests[1]["input"])
	}
	last := items[len(items)-1].(map[string]any)
	if last["type"] != "function_call_output" || last["call_id"] != "call-1" {
		t.Fatalf("tool output=%#v", last)
	}
}

func TestOpenAIStreamFailureRetries(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{name: "truncated", body: "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\"}\n\n"},
		{name: "no-completed", body: "event: response.created\ndata: {\"type\":\"response.created\"}\n\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = w.Write([]byte(tc.body))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}))
			defer server.Close()
			client := model.NewOpenAIClient(
				model.WithOpenAIBaseURL(server.URL), model.WithOpenAIAPIKey("test"),
				model.WithOpenAIMaxRetries(1), model.WithOpenAIRetryBackoff(0, 0),
				model.WithOpenAIRequestTimeout(50*time.Millisecond), model.WithOpenAIStreamIdleTimeout(5*time.Millisecond),
			)
			_, err := client.CreateMessage(context.Background(), model.Request{Model: "gpt-test", Messages: []model.Message{model.NewUserMessage("hi")}})
			if err == nil {
				t.Fatal("CreateMessage succeeded, want stream failure")
			}
			if calls != 2 {
				t.Fatalf("requests=%d, want 2 retries", calls)
			}
		})
	}
}

func TestPlannerEffortOnProviderWire(t *testing.T) {
	t.Parallel()
	for _, provider := range []string{"openai", "anthropic"} {
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				field := "reasoning"
				if provider == "anthropic" {
					field = "output_config"
				}
				var effort struct {
					Effort string `json:"effort"`
				}
				if err := json.Unmarshal(body[field], &effort); err != nil {
					t.Error(err)
				}
				if effort.Effort != "high" {
					t.Errorf("effort=%q", effort.Effort)
				}
				var metadata map[string]string
				if len(body["metadata"]) > 0 {
					if err := json.Unmarshal(body["metadata"], &metadata); err != nil {
						t.Error(err)
					}
				}
				if metadata["provider"] != "" || metadata["role"] != "" {
					t.Errorf("internal metadata leaked: %v", metadata)
				}
				w.Header().Set("Content-Type", "application/json")
				if provider == "openai" {
					_, _ = w.Write([]byte(`{"id":"r","output":[{"type":"message","text":"ok"}]}`))
				} else {
					_, _ = w.Write([]byte(`{"id":"r","role":"assistant","content":[{"type":"text","text":"ok"}]}`))
				}
			}))
			defer server.Close()
			var client model.ModelClient
			if provider == "openai" {
				client = model.NewOpenAIClient(model.WithOpenAIBaseURL(server.URL), model.WithOpenAIAPIKey("test"), model.WithOpenAIMaxRetries(0))
			} else {
				client = model.NewHTTPClient(model.WithBaseURL(server.URL), model.WithAPIKey("test"), model.WithMaxRetries(0))
			}
			_, err := client.CreateMessage(t.Context(), model.Request{Model: "planner", ReasoningEffort: "high", Metadata: map[string]string{"role": "planner", "provider": provider}, Messages: []model.Message{model.NewUserMessage("Plan")}})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOpenAIClient_RetryAfter48Seconds(t *testing.T) {
	t.Parallel()
	testRetryWaits(t, "openai", "48", []time.Duration{48 * time.Second}, false, false)
}
