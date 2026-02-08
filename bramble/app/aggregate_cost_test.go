package app

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/bramble/session"
)

func TestAggregateCost_Empty(t *testing.T) {
	m := NewModel(context.Background(), "", "", "", session.NewManager(), nil, 80, 24)
	cost := m.aggregateCost()
	if cost != 0.0 {
		t.Errorf("expected 0.0, got %.4f", cost)
	}
}

func TestAggregateCost_MultipleSessions(t *testing.T) {
	m := NewModel(context.Background(), "", "", "", session.NewManager(), nil, 80, 24)
	m.sessions = []session.SessionInfo{
		{Progress: session.SessionProgressSnapshot{TotalCostUSD: 0.0100}},
		{Progress: session.SessionProgressSnapshot{TotalCostUSD: 0.0250}},
		{Progress: session.SessionProgressSnapshot{TotalCostUSD: 0.0000}},
	}
	cost := m.aggregateCost()
	expected := 0.0350
	if math.Abs(cost-expected) > 1e-9 {
		t.Errorf("expected %.4f, got %.4f", expected, cost)
	}
}

func TestRenderStatusBar_CostOmittedWhenZero(t *testing.T) {
	m := NewModel(context.Background(), "", "", "", session.NewManager(), nil, 80, 24)
	m.sessions = []session.SessionInfo{
		{Progress: session.SessionProgressSnapshot{TotalCostUSD: 0.0}},
	}
	output := m.renderStatusBar()
	if contains(output, "Cost:") {
		t.Errorf("expected no cost display when total is zero, got: %s", output)
	}
}

func TestRenderStatusBar_CostShownWhenNonZero(t *testing.T) {
	m := NewModel(context.Background(), "", "", "", session.NewManager(), nil, 80, 24)
	m.sessions = []session.SessionInfo{
		{Progress: session.SessionProgressSnapshot{TotalCostUSD: 0.0100}},
		{Progress: session.SessionProgressSnapshot{TotalCostUSD: 0.0250}},
	}
	output := m.renderStatusBar()
	if !contains(output, "Cost:") {
		t.Errorf("expected cost display when total > 0, got: %s", output)
	}
	if !contains(output, "$0.0350") {
		t.Errorf("expected cost $0.0350 in output, got: %s", output)
	}
}

func contains(s, substr string) bool {
	return strings.Contains(s, substr)
}
