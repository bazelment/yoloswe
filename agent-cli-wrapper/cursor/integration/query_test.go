//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/cursor"
)

func TestQuery_Simple(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	result, err := cursor.Query(ctx, "What is 2+2? Reply with just the number.")
	if err != nil {
		t.Fatalf("Query failed: %v", err)
	}

	if result.Text == "" {
		t.Fatal("expected non-empty response text")
	}

	t.Logf("Response: %s", result.Text)
	t.Logf("Duration: %dms", result.DurationMs)
}

func TestQueryStream_Simple(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	events, err := cursor.QueryStream(ctx, "What is 2+2? Reply with just the number.")
	if err != nil {
		t.Fatalf("QueryStream failed: %v", err)
	}

	var gotText, gotTurnComplete bool
	for event := range events {
		switch e := event.(type) {
		case cursor.TextEvent:
			gotText = true
			t.Logf("Text: %q", e.Text)
		case cursor.TurnCompleteEvent:
			gotTurnComplete = true
			t.Logf("Done: success=%v duration=%dms", e.Success, e.DurationMs)
		case cursor.ErrorEvent:
			t.Fatalf("Error: %v (context: %s)", e.Error, e.Context)
		}
	}

	if !gotText {
		t.Error("expected at least one text event")
	}
	if !gotTurnComplete {
		t.Error("expected turn complete event")
	}
}
