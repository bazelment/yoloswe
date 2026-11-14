package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/model"
)

func TestConciergeStreamsBeforeResponseCompletes(t *testing.T) {
	for _, provider := range []string{"anthropic", "openai"} {
		t.Run(provider, func(t *testing.T) {
			first := make(chan string)
			release := make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			defer unblock()
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
					return
				}
				if body["stream"] != true {
					t.Error("streaming not requested")
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if provider == "anthropic" {
					fmt.Fprintln(w, `data: {"type":"message_start","message":{"id":"m1","type":"message","role":"assistant","model":"test","usage":{"input_tokens":3}}}`)
					fmt.Fprintln(w, `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`)
					fmt.Fprintln(w, `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hello"}}`)
				} else {
					fmt.Fprintln(w, `data: {"type":"response.output_text.delta","delta":"hello"}`)
				}
				w.(http.Flusher).Flush()
				<-release
				if provider == "anthropic" {
					fmt.Fprintln(w, `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}`)
					fmt.Fprintln(w, `data: {"type":"message_stop"}`)
				} else {
					fmt.Fprintln(w, `data: {"type":"response.completed","response":{"id":"r1","model":"test","output":[{"type":"message","text":"hello"}],"usage":{"input_tokens":3,"output_tokens":1}}}`)
				}
			}))
			defer server.Close()
			hc := &http.Client{Transport: chatTransport{base: http.DefaultTransport}, Timeout: 5 * time.Second}
			var client model.ModelClient
			if provider == "anthropic" {
				client = model.NewHTTPClient(model.WithBaseURL(server.URL), model.WithHTTPClient(hc), model.WithMaxRetries(0))
			} else {
				client = model.NewOpenAIClient(model.WithOpenAIBaseURL(server.URL), model.WithOpenAIHTTPClient(hc), model.WithOpenAIMaxRetries(0))
			}
			ctx, stream := withChatStream(context.Background(), func(s string) { first <- s })
			done := make(chan error, 1)
			go func() {
				resp, err := client.CreateMessage(ctx, model.Request{Model: "test", Messages: []model.Message{model.NewUserMessage("hello")}, MaxTokens: 32})
				if err == nil {
					stream.complete(resp.Text())
				}
				done <- err
			}()
			select {
			case s := <-first:
				if s != "hello" {
					t.Fatal(s)
				}
			case <-time.After(5 * time.Second):
				unblock()
				t.Fatal("no text before response completion")
			}
			unblock()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("stream did not complete or emitted duplicate text")
			}
		})
	}
}

func TestChatStreamEmitsFinalAnswerAfterWhitespaceToolTurn(t *testing.T) {
	t.Parallel()
	var emitted []string
	_, stream := withChatStream(context.Background(), func(s string) {
		emitted = append(emitted, s)
	})
	// Turn 1 streams only whitespace before a tool call, so OnTurn does not call complete.
	stream.text("\n  \n")
	// Turn 2 completes with a final answer without delta chunks (e.g. non-SSE or budget answer).
	stream.complete("final 2206-token concierge reply")
	if len(emitted) != 1 || emitted[0] != "final 2206-token concierge reply" {
		t.Fatalf("emitted = %q, want [\"final 2206-token concierge reply\"]", emitted)
	}
}
