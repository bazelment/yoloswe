package testutil

import (
	"time"

	"github.com/bazelment/yoloswe/multiagent/protocol"
	"github.com/bazelment/yoloswe/multiagent/subagent"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// Sample design response JSON for testing
const SampleDesignResponseJSON = `{
	"architecture": "A simple CLI application with main.go entry point",
	"files": [
		{"path": "main.go", "purpose": "CLI entry point", "action": "create"},
		{"path": "hello/hello.go", "purpose": "Core greeting logic", "action": "create"}
	],
	"interfaces": "type Greeter interface { Greet(name string) string }",
	"implementation_notes": ["Keep it simple", "Add unit tests"],
	"dependencies": [],
	"risks": ["None identified"]
}`

// Sample build response JSON for testing
const SampleBuildResponseJSON = `{
	"files_created": ["main.go", "hello/hello.go"],
	"files_modified": [],
	"tests_run": true,
	"tests_passed": true,
	"test_output": "PASS\nok  \thello\t0.001s",
	"build_output": "go build: success",
	"notes": "Implementation complete"
}`

// Sample review response JSON for testing (no critical issues)
const SampleReviewResponsePassJSON = `{
	"summary": "Code looks good overall",
	"issues": [
		{"severity": "minor", "file": "main.go", "line": 10, "message": "Consider adding error handling", "suggestion": "Add err check"}
	],
	"positives": ["Clean code structure", "Good naming conventions"],
	"suggestions": ["Add README"]
}`

// Sample review response JSON with critical issues
const SampleReviewResponseFailJSON = `{
	"summary": "Critical issues found",
	"issues": [
		{"severity": "critical", "file": "main.go", "line": 5, "message": "Null pointer dereference", "suggestion": "Add nil check"},
		{"severity": "minor", "file": "hello/hello.go", "line": 12, "message": "Unused variable", "suggestion": "Remove or use"}
	],
	"positives": [],
	"suggestions": []
}`

// NewSampleDesignResponse creates a sample DesignResponse for testing.
func NewSampleDesignResponse() *protocol.DesignResponse {
	return &protocol.DesignResponse{
		Architecture: "A simple CLI application with main.go entry point",
		Files: []protocol.FileSpec{
			{Path: "main.go", Purpose: "CLI entry point", Action: "create"},
			{Path: "hello/hello.go", Purpose: "Core greeting logic", Action: "create"},
		},
		Interfaces:          "type Greeter interface { Greet(name string) string }",
		ImplementationNotes: []string{"Keep it simple", "Add unit tests"},
		Dependencies:        []string{},
		Risks:               []string{"None identified"},
	}
}

// NewSampleBuildResponse creates a sample BuildResponse for testing.
func NewSampleBuildResponse() *protocol.BuildResponse {
	return &protocol.BuildResponse{
		FilesCreated:  []string{"main.go", "hello/hello.go"},
		FilesModified: []string{},
		TestsRun:      true,
		TestsPassed:   true,
		TestOutput:    "PASS\nok  \thello\t0.001s",
		BuildOutput:   "go build: success",
		Notes:         "Implementation complete",
	}
}

// NewSampleReviewResponsePass creates a sample ReviewResponse with no critical issues.
func NewSampleReviewResponsePass() *protocol.ReviewResponse {
	return &protocol.ReviewResponse{
		Summary: "Code looks good overall",
		Issues: []protocol.Issue{
			{Severity: "minor", File: "main.go", Line: 10, Message: "Consider adding error handling", Suggestion: "Add err check"},
		},
		Positives:   []string{"Clean code structure", "Good naming conventions"},
		Suggestions: []string{"Add README"},
	}
}

// NewSampleReviewResponseFail creates a sample ReviewResponse with critical issues.
func NewSampleReviewResponseFail() *protocol.ReviewResponse {
	return &protocol.ReviewResponse{
		Summary: "Critical issues found",
		Issues: []protocol.Issue{
			{Severity: "critical", File: "main.go", Line: 5, Message: "Null pointer dereference", Suggestion: "Add nil check"},
			{Severity: "minor", File: "hello/hello.go", Line: 12, Message: "Unused variable", Suggestion: "Remove or use"},
		},
		Positives:   []string{},
		Suggestions: []string{},
	}
}

// StandardMockResponses returns a set of mock responses for a typical successful workflow.
func StandardMockResponses() []MockResponse {
	return []MockResponse{
		{Text: SampleDesignResponseJSON, Cost: 0.01, Success: true},
		{Text: SampleBuildResponseJSON, Cost: 0.02, Success: true},
		{Text: SampleReviewResponsePassJSON, Cost: 0.005, Success: true},
	}
}

// FailureIterationMockResponses returns mock responses that trigger a fix iteration.
func FailureIterationMockResponses() []MockResponse {
	return []MockResponse{
		{Text: SampleDesignResponseJSON, Cost: 0.01, Success: true},
		{Text: SampleBuildResponseJSON, Cost: 0.02, Success: true},
		{Text: SampleReviewResponseFailJSON, Cost: 0.005, Success: true}, // Critical issues
		{Text: SampleBuildResponseJSON, Cost: 0.02, Success: true},       // Re-build
		{Text: SampleReviewResponsePassJSON, Cost: 0.005, Success: true}, // Pass
	}
}

// NewSampleProgressEvents returns sample Progress events for testing.
func NewSampleProgressEvents() []subagent.Progress {
	now := time.Now()
	return []subagent.Progress{
		{
			Type:      subagent.MessageTypeProgress,
			RequestID: "req-1",
			Agent:     subagent.AgentTypeDesigner,
			Timestamp: now,
			Phase:     subagent.PhaseThinking,
			Message:   "Analyzing requirements",
		},
		{
			Type:      subagent.MessageTypeProgress,
			RequestID: "req-1",
			Agent:     subagent.AgentTypeDesigner,
			Timestamp: now.Add(100 * time.Millisecond),
			Phase:     subagent.PhaseToolCall,
			ToolName:  "Read",
			ToolID:    "tool-1",
			ToolStarted: true,
		},
		{
			Type:      subagent.MessageTypeProgress,
			RequestID: "req-1",
			Agent:     subagent.AgentTypeDesigner,
			Timestamp: now.Add(200 * time.Millisecond),
			Phase:     subagent.PhaseStreaming,
			TextDelta: "Here is the design...",
			FullText:  "Here is the design...",
		},
	}
}

// NewSampleFileEvents returns sample FileEvent events for testing.
func NewSampleFileEvents() []subagent.FileEvent {
	now := time.Now()
	return []subagent.FileEvent{
		{
			Type:      subagent.MessageTypeFileEvent,
			RequestID: "req-1",
			Agent:     subagent.AgentTypeBuilder,
			Timestamp: now,
			Path:      "/tmp/main.go",
			Action:    subagent.FileActionCreate,
			ToolName:  "Write",
		},
		{
			Type:      subagent.MessageTypeFileEvent,
			RequestID: "req-1",
			Agent:     subagent.AgentTypeBuilder,
			Timestamp: now.Add(100 * time.Millisecond),
			Path:      "/tmp/hello/hello.go",
			Action:    subagent.FileActionCreate,
			ToolName:  "Write",
		},
		{
			Type:      subagent.MessageTypeFileEvent,
			RequestID: "req-1",
			Agent:     subagent.AgentTypeBuilder,
			Timestamp: now.Add(200 * time.Millisecond),
			Path:      "/tmp/main.go",
			Action:    subagent.FileActionModify,
			ToolName:  "Edit",
		},
	}
}

// NewSampleClaudeEvents returns sample Claude SDK events for testing.
func NewSampleClaudeEvents() []claude.Event {
	now := time.Now()
	return []claude.Event{
		claude.ThinkingEvent{
			TurnNumber:   1,
			Thinking:     "Let me analyze this...",
			FullThinking: "Let me analyze this...",
		},
		claude.ToolStartEvent{
			TurnNumber: 1,
			ID:         "tool-1",
			Name:       "Read",
			Timestamp:  now,
		},
		claude.ToolCompleteEvent{
			TurnNumber: 1,
			ID:         "tool-1",
			Name:       "Read",
			Input:      map[string]interface{}{"file_path": "/tmp/test.go"},
			Timestamp:  now.Add(50 * time.Millisecond),
		},
		claude.TextEvent{
			TurnNumber: 1,
			Text:       "Based on my analysis...",
			FullText:   "Based on my analysis...",
		},
		claude.TurnCompleteEvent{
			TurnNumber: 1,
			Success:    true,
			DurationMs: 1500,
			Usage: claude.TurnUsage{
				InputTokens:  100,
				OutputTokens: 200,
				CostUSD:      0.01,
			},
		},
	}
}

// NewSampleDesignResult returns a sample Result for a designer sub-agent.
func NewSampleDesignResult() *subagent.Result {
	return &subagent.Result{
		Type:          subagent.MessageTypeResult,
		RequestID:     "req-1",
		Agent:         subagent.AgentTypeDesigner,
		Timestamp:     time.Now(),
		Success:       true,
		Text:          SampleDesignResponseJSON,
		Design:        NewSampleDesignResponse(),
		FilesCreated:  []string{},
		FilesModified: []string{},
		TotalCostUSD:  0.01,
		DurationMs:    1500,
	}
}

// NewSampleBuildResult returns a sample Result for a builder sub-agent.
func NewSampleBuildResult() *subagent.Result {
	return &subagent.Result{
		Type:          subagent.MessageTypeResult,
		RequestID:     "req-2",
		Agent:         subagent.AgentTypeBuilder,
		Timestamp:     time.Now(),
		Success:       true,
		Text:          SampleBuildResponseJSON,
		Build:         NewSampleBuildResponse(),
		FilesCreated:  []string{"main.go", "hello/hello.go"},
		FilesModified: []string{},
		TotalCostUSD:  0.02,
		DurationMs:    3000,
	}
}

// NewSampleReviewResult returns a sample Result for a reviewer sub-agent.
func NewSampleReviewResult() *subagent.Result {
	return &subagent.Result{
		Type:          subagent.MessageTypeResult,
		RequestID:     "req-3",
		Agent:         subagent.AgentTypeReviewer,
		Timestamp:     time.Now(),
		Success:       true,
		Text:          SampleReviewResponsePassJSON,
		Review:        NewSampleReviewResponsePass(),
		FilesCreated:  []string{},
		FilesModified: []string{},
		TotalCostUSD:  0.005,
		DurationMs:    1000,
	}
}

// NewSampleCostUpdate returns a sample CostUpdate event.
func NewSampleCostUpdate() *subagent.CostUpdate {
	return subagent.NewCostUpdate("req-1", subagent.AgentTypeDesigner, 0.01, 0.01, 100, 200)
}
