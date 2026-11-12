package model_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/orch/model"
)

type spawnParams struct {
	ItemID string `json:"item_id" jsonschema:"required,description=Backlog item ID"`
	Brief  string `json:"brief" jsonschema:"required,description=Brief for the lane"`
	Round  int    `json:"round,omitempty"`
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
