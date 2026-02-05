//go:build integration
// +build integration

package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/multiagent/checkpoint"
	"github.com/bazelment/yoloswe/multiagent/planner"
	"github.com/bazelment/yoloswe/multiagent/progress"
	"github.com/bazelment/yoloswe/multiagent/protocol"
)

// mockReporter captures progress events for testing.
type mockReporter struct {
	mu     sync.Mutex
	events []progress.Event
}

func (m *mockReporter) Event(e progress.Event) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.events = append(m.events, e)
}

func (m *mockReporter) Close() {}

func (m *mockReporter) getEvents() []progress.Event {
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]progress.Event, len(m.events))
	copy(result, m.events)
	return result
}

func (m *mockReporter) hasEventType(t progress.EventType) bool {
	for _, e := range m.getEvents() {
		if e.Type() == t {
			return true
		}
	}
	return false
}

func (m *mockReporter) countEventType(t progress.EventType) int {
	count := 0
	for _, e := range m.getEvents() {
		if e.Type() == t {
			count++
		}
	}
	return count
}

// TestCallDesigner_WithProgress tests that CallDesigner emits correct progress events.
func TestCallDesigner_WithProgress(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	reporter := &mockReporter{}
	cfg := planner.Config{
		PlannerConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    t.TempDir(),
			SessionDir: t.TempDir(),
		},
		DesignerConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    t.TempDir(),
			SessionDir: t.TempDir(),
		},
		BuilderConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    t.TempDir(),
			SessionDir: t.TempDir(),
		},
		ReviewerConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    t.TempDir(),
			SessionDir: t.TempDir(),
		},
		Progress:      reporter,
		MaxIterations: 10,
	}

	p := planner.New(cfg, "test-session")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Call Designer
	req := &protocol.DesignRequest{
		Task:    "Create a simple function that returns 'Hello, World!'",
		Context: "We need a simple Go function for a test",
	}

	_, err := p.CallDesigner(ctx, req)
	if err != nil {
		t.Fatalf("CallDesigner failed: %v", err)
	}

	// Verify progress events were emitted
	events := reporter.getEvents()
	t.Logf("Captured %d progress events", len(events))
	for i, e := range events {
		t.Logf("  Event %d: %T", i, e)
	}

	// Check that we got the expected events
	if !reporter.hasEventType(progress.EventPhaseChange) {
		t.Error("Expected PhaseChangeEvent but none found")
	}

	if !reporter.hasEventType(progress.EventAgentStart) {
		t.Error("Expected AgentStartEvent but none found")
	}

	if !reporter.hasEventType(progress.EventAgentComplete) {
		t.Error("Expected AgentCompleteEvent but none found")
	}

	// Verify iteration count increased
	if p.IterationCount() != 1 {
		t.Errorf("Expected iteration count 1, got %d", p.IterationCount())
	}
}

// TestCallBuilder_WithProgress tests that CallBuilder emits correct progress events.
func TestCallBuilder_WithProgress(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	reporter := &mockReporter{}
	cfg := planner.Config{
		PlannerConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    t.TempDir(),
			SessionDir: t.TempDir(),
		},
		DesignerConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    t.TempDir(),
			SessionDir: t.TempDir(),
		},
		BuilderConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    t.TempDir(),
			SessionDir: t.TempDir(),
		},
		ReviewerConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    t.TempDir(),
			SessionDir: t.TempDir(),
		},
		Progress:      reporter,
		MaxIterations: 10,
	}

	p := planner.New(cfg, "test-session")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	workDir := t.TempDir()

	// Call Builder
	req := &protocol.BuildRequest{
		Task:    "Create a file called hello.go with a simple Hello World function",
		WorkDir: workDir,
	}

	_, err := p.CallBuilder(ctx, req)
	if err != nil {
		t.Fatalf("CallBuilder failed: %v", err)
	}

	// Verify progress events
	events := reporter.getEvents()
	t.Logf("Captured %d progress events", len(events))
	for i, e := range events {
		t.Logf("  Event %d: %T", i, e)
	}

	if !reporter.hasEventType(progress.EventPhaseChange) {
		t.Error("Expected PhaseChangeEvent but none found")
	}

	if !reporter.hasEventType(progress.EventAgentStart) {
		t.Error("Expected AgentStartEvent but none found")
	}

	if !reporter.hasEventType(progress.EventAgentComplete) {
		t.Error("Expected AgentCompleteEvent but none found")
	}

	// Check for phase transition to building
	for _, e := range events {
		if pc, ok := e.(progress.PhaseChangeEvent); ok {
			if pc.To != checkpoint.PhaseBuilding {
				t.Errorf("Expected phase transition to PhaseBuilding, got %v", pc.To)
			}
		}
	}
}

// TestCallReviewer_WithProgress tests that CallReviewer emits correct progress events.
func TestCallReviewer_WithProgress(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	reporter := &mockReporter{}
	cfg := planner.Config{
		PlannerConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    t.TempDir(),
			SessionDir: t.TempDir(),
		},
		DesignerConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    t.TempDir(),
			SessionDir: t.TempDir(),
		},
		BuilderConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    t.TempDir(),
			SessionDir: t.TempDir(),
		},
		ReviewerConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    t.TempDir(),
			SessionDir: t.TempDir(),
		},
		Progress:      reporter,
		MaxIterations: 10,
	}

	p := planner.New(cfg, "test-session")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	// Call Reviewer
	req := &protocol.ReviewRequest{
		Task:         "Review the code quality",
		FilesChanged: []string{"main.go"},
	}

	_, err := p.CallReviewer(ctx, req)
	if err != nil {
		t.Fatalf("CallReviewer failed: %v", err)
	}

	// Verify progress events
	events := reporter.getEvents()
	t.Logf("Captured %d progress events", len(events))
	for i, e := range events {
		t.Logf("  Event %d: %T", i, e)
	}

	if !reporter.hasEventType(progress.EventPhaseChange) {
		t.Error("Expected PhaseChangeEvent but none found")
	}

	if !reporter.hasEventType(progress.EventAgentStart) {
		t.Error("Expected AgentStartEvent but none found")
	}

	if !reporter.hasEventType(progress.EventAgentComplete) {
		t.Error("Expected AgentCompleteEvent but none found")
	}

	// Check for phase transition to reviewing
	for _, e := range events {
		if pc, ok := e.(progress.PhaseChangeEvent); ok {
			if pc.To != checkpoint.PhaseReviewing {
				t.Errorf("Expected phase transition to PhaseReviewing, got %v", pc.To)
			}
		}
	}
}

// TestFullWorkflow_DesignBuildReview tests the full workflow with all three sub-agents.
func TestFullWorkflow_DesignBuildReview(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}

	reporter := &mockReporter{}
	workDir := t.TempDir()

	cfg := planner.Config{
		PlannerConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    workDir,
			SessionDir: t.TempDir(),
		},
		DesignerConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    workDir,
			SessionDir: t.TempDir(),
		},
		BuilderConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    workDir,
			SessionDir: t.TempDir(),
		},
		ReviewerConfig: agent.AgentConfig{
			Model:      "haiku",
			WorkDir:    workDir,
			SessionDir: t.TempDir(),
		},
		Progress:      reporter,
		MaxIterations: 10,
	}

	p := planner.New(cfg, "test-workflow-session")
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second) // 3 minutes for all three
	defer cancel()

	// Step 1: Design
	t.Log("Step 1: Calling Designer")
	designReq := &protocol.DesignRequest{
		Task:    "Create a simple greeting function in Go",
		Context: "Part of a larger greeting library",
	}
	designResp, err := p.CallDesigner(ctx, designReq)
	if err != nil {
		t.Fatalf("CallDesigner failed: %v", err)
	}
	t.Logf("Design completed, response: %+v", designResp)

	// Step 2: Build
	t.Log("Step 2: Calling Builder")
	buildReq := &protocol.BuildRequest{
		Task:    "Create a greeting.go file with a Greet(name string) function",
		WorkDir: workDir,
		Design:  designResp,
	}
	buildResp, err := p.CallBuilder(ctx, buildReq)
	if err != nil {
		t.Fatalf("CallBuilder failed: %v", err)
	}
	t.Logf("Build completed, response: %+v", buildResp)

	// Step 3: Review
	t.Log("Step 3: Calling Reviewer")
	reviewReq := &protocol.ReviewRequest{
		Task:           "Review the greeting function implementation",
		FilesChanged:   []string{"greeting.go"},
		OriginalDesign: designResp,
	}
	reviewResp, err := p.CallReviewer(ctx, reviewReq)
	if err != nil {
		t.Fatalf("CallReviewer failed: %v", err)
	}
	t.Logf("Review completed, response: %+v", reviewResp)

	// Verify we captured events for all three phases
	events := reporter.getEvents()
	t.Logf("Total captured events: %d", len(events))

	// Count phase change events
	phaseCount := reporter.countEventType(progress.EventPhaseChange)
	t.Logf("Phase change events: %d", phaseCount)
	if phaseCount < 3 {
		t.Errorf("Expected at least 3 PhaseChangeEvents (design, build, review), got %d", phaseCount)
	}

	// Count agent start events
	startCount := reporter.countEventType(progress.EventAgentStart)
	t.Logf("Agent start events: %d", startCount)
	if startCount < 3 {
		t.Errorf("Expected at least 3 AgentStartEvents, got %d", startCount)
	}

	// Count agent complete events
	completeCount := reporter.countEventType(progress.EventAgentComplete)
	t.Logf("Agent complete events: %d", completeCount)
	if completeCount < 3 {
		t.Errorf("Expected at least 3 AgentCompleteEvents, got %d", completeCount)
	}

	// Verify iteration count
	if p.IterationCount() != 3 {
		t.Errorf("Expected iteration count 3, got %d", p.IterationCount())
	}

	// Verify total cost is tracked
	cost := p.TotalCost()
	t.Logf("Total cost: $%.6f", cost)
	if cost <= 0 {
		t.Error("Expected non-zero total cost")
	}
}
