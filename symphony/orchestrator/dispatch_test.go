package orchestrator

import (
	"testing"
	"time"

	"github.com/bazelment/yoloswe/symphony/config"
	"github.com/bazelment/yoloswe/symphony/model"
)

func TestSortForDispatch(t *testing.T) {
	t.Parallel()

	p1, p2, p4 := 1, 2, 4
	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)

	issues := []model.Issue{
		{Identifier: "D", Priority: nil, CreatedAt: &t1},
		{Identifier: "A", Priority: &p2, CreatedAt: &t1},
		{Identifier: "B", Priority: &p1, CreatedAt: &t2},
		{Identifier: "C", Priority: &p1, CreatedAt: &t1},
		{Identifier: "E", Priority: &p4, CreatedAt: &t1},
	}

	sortForDispatch(issues)

	expected := []string{"C", "B", "A", "E", "D"} // p1 oldest first, p2, p4, nil last
	for i, want := range expected {
		if issues[i].Identifier != want {
			t.Errorf("position %d: got %s, want %s", i, issues[i].Identifier, want)
		}
	}
}

func TestSortForDispatch_NullPriorityLast(t *testing.T) {
	t.Parallel()

	p1 := 1
	issues := []model.Issue{
		{Identifier: "B", Priority: nil},
		{Identifier: "A", Priority: &p1},
	}

	sortForDispatch(issues)

	if issues[0].Identifier != "A" {
		t.Errorf("null priority should sort last, got %s first", issues[0].Identifier)
	}
}

func testConfig() *config.ServiceConfig {
	return config.NewServiceConfig(&model.WorkflowDefinition{
		Config: map[string]any{
			"tracker": map[string]any{
				"kind":         "linear",
				"api_key":      "test",
				"project_slug": "TEST",
			},
		},
	})
}

func TestShouldDispatch_BasicEligibility(t *testing.T) {
	t.Parallel()

	o := New(testConfig, nil, RealClock{}, nil)
	cfg := testConfig()

	issue := model.Issue{
		ID:         "id-1",
		Identifier: "TEST-1",
		Title:      "Test issue",
		State:      "Todo",
	}

	if !o.shouldDispatch(issue, cfg) {
		t.Error("expected eligible issue to be dispatchable")
	}
}

func TestShouldDispatch_MissingFields(t *testing.T) {
	t.Parallel()

	o := New(testConfig, nil, RealClock{}, nil)
	cfg := testConfig()

	tests := []struct {
		name  string
		issue model.Issue
	}{
		{"no ID", model.Issue{Identifier: "T-1", Title: "t", State: "Todo"}},
		{"no identifier", model.Issue{ID: "1", Title: "t", State: "Todo"}},
		{"no title", model.Issue{ID: "1", Identifier: "T-1", State: "Todo"}},
		{"no state", model.Issue{ID: "1", Identifier: "T-1", Title: "t"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if o.shouldDispatch(tt.issue, cfg) {
				t.Error("expected not dispatchable with missing field")
			}
		})
	}
}

func TestShouldDispatch_AlreadyRunning(t *testing.T) {
	t.Parallel()

	o := New(testConfig, nil, RealClock{}, nil)
	cfg := testConfig()

	o.running["id-1"] = &model.RunningEntry{}

	issue := model.Issue{ID: "id-1", Identifier: "TEST-1", Title: "t", State: "Todo"}
	if o.shouldDispatch(issue, cfg) {
		t.Error("should not dispatch already-running issue")
	}
}

func TestShouldDispatch_AlreadyClaimed(t *testing.T) {
	t.Parallel()

	o := New(testConfig, nil, RealClock{}, nil)
	cfg := testConfig()

	o.claimed["id-1"] = struct{}{}

	issue := model.Issue{ID: "id-1", Identifier: "TEST-1", Title: "t", State: "Todo"}
	if o.shouldDispatch(issue, cfg) {
		t.Error("should not dispatch already-claimed issue")
	}
}

func TestShouldDispatch_TerminalState(t *testing.T) {
	t.Parallel()

	o := New(testConfig, nil, RealClock{}, nil)
	cfg := testConfig()

	issue := model.Issue{ID: "id-1", Identifier: "TEST-1", Title: "t", State: "Done"}
	if o.shouldDispatch(issue, cfg) {
		t.Error("should not dispatch terminal-state issue")
	}
}

func TestShouldDispatch_InactiveState(t *testing.T) {
	t.Parallel()

	o := New(testConfig, nil, RealClock{}, nil)
	cfg := testConfig()

	issue := model.Issue{ID: "id-1", Identifier: "TEST-1", Title: "t", State: "Human Review"}
	if o.shouldDispatch(issue, cfg) {
		t.Error("should not dispatch non-active state issue")
	}
}

func TestShouldDispatch_GlobalConcurrencyFull(t *testing.T) {
	t.Parallel()

	cfgFn := func() *config.ServiceConfig {
		cfg := testConfig()
		cfg.MaxConcurrentAgents = 1
		return cfg
	}
	o := New(cfgFn, nil, RealClock{}, nil)
	cfg := cfgFn()

	o.running["other-id"] = &model.RunningEntry{}

	issue := model.Issue{ID: "id-1", Identifier: "TEST-1", Title: "t", State: "Todo"}
	if o.shouldDispatch(issue, cfg) {
		t.Error("should not dispatch when global slots full")
	}
}

func TestShouldDispatch_PerStateConcurrencyFull(t *testing.T) {
	t.Parallel()

	cfgFn := func() *config.ServiceConfig {
		cfg := testConfig()
		cfg.MaxConcurrentByState = map[string]int{"todo": 1}
		return cfg
	}
	o := New(cfgFn, nil, RealClock{}, nil)
	cfg := cfgFn()

	o.running["other-id"] = &model.RunningEntry{
		Issue: model.Issue{State: "Todo"},
	}

	issue := model.Issue{ID: "id-1", Identifier: "TEST-1", Title: "t", State: "Todo"}
	if o.shouldDispatch(issue, cfg) {
		t.Error("should not dispatch when per-state slots full")
	}
}

func TestShouldDispatch_TodoBlockerRule(t *testing.T) {
	t.Parallel()

	o := New(testConfig, nil, RealClock{}, nil)
	cfg := testConfig()

	activeState := "In Progress"
	terminalState := "Done"

	// Non-terminal blocker → not eligible.
	issue := model.Issue{
		ID: "id-1", Identifier: "TEST-1", Title: "t", State: "Todo",
		BlockedBy: []model.BlockerRef{{State: &activeState}},
	}
	if o.shouldDispatch(issue, cfg) {
		t.Error("Todo with non-terminal blocker should not dispatch")
	}

	// Terminal blocker → eligible.
	issue2 := model.Issue{
		ID: "id-2", Identifier: "TEST-2", Title: "t", State: "Todo",
		BlockedBy: []model.BlockerRef{{State: &terminalState}},
	}
	if !o.shouldDispatch(issue2, cfg) {
		t.Error("Todo with terminal blocker should dispatch")
	}
}
