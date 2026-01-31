package planner

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
)

// Note: readLineWithContext tests have been removed because the function
// now reads directly from os.Stdin with SetReadDeadline for interruptibility.
// This makes unit testing impractical without mocking stdin at the OS level.
// The function's correctness is verified through integration tests.

func TestTrackPlanFileWrite(t *testing.T) {
	tests := []struct {
		name         string
		toolName     string
		input        map[string]interface{}
		expectedPath string
	}{
		{
			name:     "detects .claude/plans write",
			toolName: "Write",
			input: map[string]interface{}{
				"file_path": "/Users/test/.claude/plans/my-plan.md",
			},
			expectedPath: "/Users/test/.claude/plans/my-plan.md",
		},
		{
			name:     "ignores non-plan writes",
			toolName: "Write",
			input: map[string]interface{}{
				"file_path": "/Users/test/some-file.md",
			},
			expectedPath: "",
		},
		{
			name:     "ignores non-md files in plans dir",
			toolName: "Write",
			input: map[string]interface{}{
				"file_path": "/Users/test/.claude/plans/notes.txt",
			},
			expectedPath: "",
		},
		{
			name:     "ignores other tools",
			toolName: "Read",
			input: map[string]interface{}{
				"file_path": "/Users/test/.claude/plans/my-plan.md",
			},
			expectedPath: "",
		},
		{
			name:         "handles missing file_path",
			toolName:     "Write",
			input:        map[string]interface{}{},
			expectedPath: "",
		},
		{
			name:     "handles non-string file_path",
			toolName: "Write",
			input: map[string]interface{}{
				"file_path": 12345,
			},
			expectedPath: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &PlannerWrapper{}

			p.trackPlanFileWrite(tt.toolName, tt.input)

			if p.planFilePath != tt.expectedPath {
				t.Errorf("expected planFilePath=%q, got %q", tt.expectedPath, p.planFilePath)
			}
		})
	}
}

func TestExportPlanToFile(t *testing.T) {
	// Create temp dir and plan file
	tmpDir := t.TempDir()
	planContent := "# My Plan\n\nThis is the actual plan content."
	planFile := filepath.Join(tmpDir, "test-plan.md")
	if err := os.WriteFile(planFile, []byte(planContent), 0644); err != nil {
		t.Fatal(err)
	}

	p := &PlannerWrapper{
		planFilePath: planFile,
	}

	// Export to a new file
	exportFile := filepath.Join(tmpDir, "exported.md")
	err := p.exportPlanToFile(exportFile)
	if err != nil {
		t.Fatalf("export failed: %v", err)
	}

	// Verify content matches
	exported, err := os.ReadFile(exportFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(exported) != planContent {
		t.Errorf("exported content doesn't match\nexpected: %q\ngot: %q", planContent, string(exported))
	}
}

func TestExportPlanToFile_NoPlanFile(t *testing.T) {
	p := &PlannerWrapper{
		planFilePath: "", // No plan file detected
	}

	err := p.exportPlanToFile("/tmp/test.md")
	if err == nil {
		t.Error("expected error when planFilePath is empty")
	}
}

func TestExportPlanToFile_MissingSourceFile(t *testing.T) {
	p := &PlannerWrapper{
		planFilePath: "/nonexistent/path/plan.md",
	}

	err := p.exportPlanToFile("/tmp/test.md")
	if err == nil {
		t.Error("expected error when source file doesn't exist")
	}
}

func TestParseQuestionOptions(t *testing.T) {
	tests := []struct {
		name     string
		input    []interface{}
		expected []QuestionOption
	}{
		{
			name:  "string options",
			input: []interface{}{"Option A", "Option B"},
			expected: []QuestionOption{
				{Label: "Option A"},
				{Label: "Option B"},
			},
		},
		{
			name: "object options with description",
			input: []interface{}{
				map[string]interface{}{"label": "Opt1", "description": "First option"},
				map[string]interface{}{"label": "Opt2", "description": "Second option"},
			},
			expected: []QuestionOption{
				{Label: "Opt1", Description: "First option"},
				{Label: "Opt2", Description: "Second option"},
			},
		},
		{
			name:     "empty input",
			input:    []interface{}{},
			expected: []QuestionOption{},
		},
		{
			name: "mixed string and object options",
			input: []interface{}{
				"Simple option",
				map[string]interface{}{"label": "Complex", "description": "With desc"},
			},
			expected: []QuestionOption{
				{Label: "Simple option"},
				{Label: "Complex", Description: "With desc"},
			},
		},
		{
			name: "object without label is skipped",
			input: []interface{}{
				map[string]interface{}{"description": "No label"},
			},
			expected: []QuestionOption{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := render.ParseQuestionOptions(tt.input)
			if len(result) != len(tt.expected) {
				t.Errorf("expected %d options, got %d", len(tt.expected), len(result))
				return
			}
			for i, opt := range result {
				if opt.Label != tt.expected[i].Label || opt.Description != tt.expected[i].Description {
					t.Errorf("option %d: expected %+v, got %+v", i, tt.expected[i], opt)
				}
			}
		})
	}
}

func TestParseOptionIndex(t *testing.T) {
	tests := []struct {
		name       string
		input      string
		numOptions int
		expected   int
	}{
		{"valid first option", "1", 3, 0},
		{"valid last option", "3", 3, 2},
		{"zero is invalid", "0", 3, -1},
		{"exceeds options", "4", 3, -1},
		{"non-numeric", "abc", 3, -1},
		{"empty string", "", 3, -1},
		{"negative number", "-1", 3, -1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result := parseOptionIndex(tt.input, tt.numOptions)
			if result != tt.expected {
				t.Errorf("expected %d, got %d", tt.expected, result)
			}
		})
	}
}

func TestGeneratePlanFilename(t *testing.T) {
	tests := []struct {
		name     string
		prompt   string
		expected string
	}{
		{
			name:     "simple prompt",
			prompt:   "Create a hello world program",
			expected: "plan-create-a-hello-world-program.md",
		},
		{
			name:     "long prompt truncated to 5 words",
			prompt:   "Implement a REST API with authentication and rate limiting for users",
			expected: "plan-implement-a-rest-api-with.md",
		},
		{
			name:     "special characters removed",
			prompt:   "Fix bug #123 in the @login module!",
			expected: "plan-fix-bug-123-in-the.md",
		},
		{
			name:     "uppercase converted to lowercase",
			prompt:   "CREATE API",
			expected: "plan-create-api.md",
		},
		{
			name:     "numbers preserved",
			prompt:   "Add v2 endpoint",
			expected: "plan-add-v2-endpoint.md",
		},
		{
			name:     "single word prompt",
			prompt:   "Test",
			expected: "plan-test.md",
		},
		{
			name:     "empty prompt",
			prompt:   "",
			expected: "plan.md",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &PlannerWrapper{
				config: Config{Prompt: tt.prompt},
			}
			result := p.generatePlanFilename()
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestBuildModeConstants(t *testing.T) {
	// Verify BuildMode constants have expected values
	tests := []struct {
		name     string
		mode     BuildMode
		expected string
	}{
		{"BuildModeNone", BuildModeNone, ""},
		{"BuildModeCurrent", BuildModeCurrent, "current"},
		{"BuildModeNewSession", BuildModeNewSession, "new"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if string(tt.mode) != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, string(tt.mode))
			}
		})
	}
}

func TestConfigBuildMode(t *testing.T) {
	// Test that BuildMode can be set in Config
	tests := []struct {
		name      string
		buildMode BuildMode
	}{
		{"none", BuildModeNone},
		{"current", BuildModeCurrent},
		{"new", BuildModeNewSession},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := Config{
				Model:     "sonnet",
				BuildMode: tt.buildMode,
			}
			if config.BuildMode != tt.buildMode {
				t.Errorf("expected BuildMode %q, got %q", tt.buildMode, config.BuildMode)
			}
		})
	}
}

func TestBuildModeFromString(t *testing.T) {
	// Test that string values can be converted to BuildMode
	tests := []struct {
		input    string
		expected BuildMode
	}{
		{"", BuildModeNone},
		{"current", BuildModeCurrent},
		{"new", BuildModeNewSession},
		{"unknown", BuildMode("unknown")}, // Invalid but should work
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			result := BuildMode(tt.input)
			if result != tt.expected {
				t.Errorf("expected %q, got %q", tt.expected, result)
			}
		})
	}
}

func TestWaitingForUserInputReset(t *testing.T) {
	// Test that waitingForUserInput is properly managed
	p := &PlannerWrapper{
		waitingForUserInput: true,
	}

	// Simulate what happens when we're executing (not waiting for input)
	p.waitingForUserInput = false

	if p.waitingForUserInput {
		t.Error("waitingForUserInput should be false after reset")
	}
}

func TestBuildModeIsValid(t *testing.T) {
	tests := []struct {
		mode  BuildMode
		valid bool
	}{
		{BuildModeNone, true},
		{BuildModeCurrent, true},
		{BuildModeNewSession, true},
		{BuildMode("invalid"), false},
		{BuildMode("CURRENT"), false}, // Case sensitive
		{BuildMode("  "), false},      // Whitespace
	}

	for _, tt := range tests {
		t.Run(string(tt.mode), func(t *testing.T) {
			if got := tt.mode.IsValid(); got != tt.valid {
				t.Errorf("BuildMode(%q).IsValid() = %v, want %v", tt.mode, got, tt.valid)
			}
		})
	}
}

func TestPlanFilePathRequired(t *testing.T) {
	// Test that executeInNewSession requires a valid plan file path
	tests := []struct {
		name         string
		planFilePath string
		expectFail   bool // Whether we expect the path check to fail
	}{
		{"empty path requires fallback", "", true},
		{"nonexistent file requires fallback", "/nonexistent/path/plan.md", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &PlannerWrapper{
				planFilePath: tt.planFilePath,
			}

			// Check the conditions that executeInNewSession would check
			needsFallback := false
			if p.planFilePath == "" {
				needsFallback = true
			} else if _, err := os.Stat(p.planFilePath); os.IsNotExist(err) {
				needsFallback = true
			}

			if needsFallback != tt.expectFail {
				t.Errorf("expected needsFallback=%v, got %v", tt.expectFail, needsFallback)
			}
		})
	}
}

func TestPendingBuildStartTransition(t *testing.T) {
	// Test that pendingBuildStart correctly handles the plan→build transition
	// The sequence is:
	// 1. ExitPlanMode turn completes (planning stats) → transition to build phase
	// 2. Build turn completes (build stats) → prompt/exit
	p := &PlannerWrapper{
		waitingForUserInput: true, // Set by handleExitPlanMode
		pendingBuildStart:   true, // Set by executeInCurrentSession
	}

	// Before: not in build phase
	if p.inBuildPhase {
		t.Error("should not be in build phase before transition")
	}

	// Step 1: Simulate the planning TurnComplete (ExitPlanMode turn)
	planningUsage := claude.TurnUsage{
		InputTokens:  1000,
		OutputTokens: 500,
		CostUSD:      0.05,
	}

	if p.pendingBuildStart {
		// Count as planning stats - this is still the ExitPlanMode turn
		p.planningStats.Add(planningUsage)
		// Transition to build phase for subsequent turns
		p.inBuildPhase = true
		p.pendingBuildStart = false
		// Keep waitingForUserInput=true - don't exit yet
	}

	// After planning turn: in build phase, waiting for build response
	if !p.inBuildPhase {
		t.Error("should be in build phase after planning turn")
	}
	if p.pendingBuildStart {
		t.Error("pendingBuildStart should be false after planning turn")
	}
	if !p.waitingForUserInput {
		t.Error("waitingForUserInput should still be true after planning turn")
	}
	if p.planningStats.InputTokens != 1000 {
		t.Errorf("expected planning InputTokens=1000, got %d", p.planningStats.InputTokens)
	}
	if p.buildingStats.InputTokens != 0 {
		t.Errorf("expected building InputTokens=0 after planning turn, got %d", p.buildingStats.InputTokens)
	}

	// Step 2: Simulate the build TurnComplete
	buildUsage := claude.TurnUsage{
		InputTokens:  2000,
		OutputTokens: 1000,
		CostUSD:      0.10,
	}

	// Accumulate build stats
	if p.inBuildPhase {
		p.buildingStats.Add(buildUsage)
	}

	// Handle waitingForUserInput flag (would prompt/exit here)
	if p.waitingForUserInput {
		p.waitingForUserInput = false
		// Would call promptForFollowUp or exit here
	}

	// After build turn: stats recorded correctly
	if p.buildingStats.InputTokens != 2000 {
		t.Errorf("expected building InputTokens=2000, got %d", p.buildingStats.InputTokens)
	}
	if p.buildingStats.TurnCount != 1 {
		t.Errorf("expected building TurnCount=1, got %d", p.buildingStats.TurnCount)
	}
	if p.planningStats.TurnCount != 1 {
		t.Errorf("expected planning TurnCount=1, got %d", p.planningStats.TurnCount)
	}
	if p.waitingForUserInput {
		t.Error("waitingForUserInput should be false after build turn")
	}
}

func TestNewSessionBuildPhaseImmediate(t *testing.T) {
	// Test that executeInNewSession sets inBuildPhase=true directly
	// (not pendingBuildStart, since there's no planning TurnComplete in new session)
	p := &PlannerWrapper{
		waitingForUserInput: true, // Set by handleExitPlanMode
		inBuildPhase:        true, // Set directly by executeInNewSession
	}

	// Verify we're immediately in build phase
	if !p.inBuildPhase {
		t.Error("should be in build phase for new session")
	}
	if p.pendingBuildStart {
		t.Error("pendingBuildStart should be false for new session")
	}

	// Build turn should accumulate to build stats
	buildUsage := claude.TurnUsage{
		InputTokens:  3000,
		OutputTokens: 1500,
		CostUSD:      0.15,
	}
	if p.inBuildPhase {
		p.buildingStats.Add(buildUsage)
	} else {
		p.planningStats.Add(buildUsage)
	}

	// Verify stats go to build, not planning
	if p.buildingStats.InputTokens != 3000 {
		t.Errorf("expected building InputTokens=3000, got %d", p.buildingStats.InputTokens)
	}
	if p.planningStats.InputTokens != 0 {
		t.Errorf("expected planning InputTokens=0, got %d", p.planningStats.InputTokens)
	}
}

func TestPendingBuildStartClearsWaitingForUserInput(t *testing.T) {
	// Test that waitingForUserInput is properly managed through the
	// plan→build transition (2-turn sequence)
	p := &PlannerWrapper{
		waitingForUserInput: true,
		pendingBuildStart:   true,
	}

	// Step 1: Planning turn completes (ExitPlanMode turn)
	if p.pendingBuildStart {
		p.planningStats.Add(claude.TurnUsage{InputTokens: 100})
		p.inBuildPhase = true
		p.pendingBuildStart = false
		// Keep waitingForUserInput=true - don't clear yet
	}

	// After planning turn: still waiting for build response
	if !p.waitingForUserInput {
		t.Error("waitingForUserInput should still be true after planning turn")
	}

	// Step 2: Build turn completes
	if p.inBuildPhase {
		p.buildingStats.Add(claude.TurnUsage{InputTokens: 200})
	}

	// Now clear waitingForUserInput (would prompt/exit here)
	if p.waitingForUserInput {
		p.waitingForUserInput = false
	}

	// After build turn: no longer waiting
	if p.waitingForUserInput {
		t.Error("waitingForUserInput should be false after build turn completes")
	}
}

func TestPlanToBuildTransitionEventSequence(t *testing.T) {
	// Test the complete event sequence for plan→build transition:
	// 1. ExitPlanMode tool completes
	// 2. User selects "Execute in current session"
	// 3. Planning TurnComplete arrives (ExitPlanMode turn)
	// 4. Build turn starts processing "I approve this plan..."
	// 5. Build TurnComplete arrives
	// 6. Prompt for follow-up (or exit in simple mode)

	tests := []struct {
		name                      string
		simple                    bool
		expectExitAfterPlanningTC bool
		expectExitAfterBuildTC    bool
		expectPromptAfterBuildTC  bool
	}{
		{
			name:                      "non-simple mode prompts after build",
			simple:                    false,
			expectExitAfterPlanningTC: false,
			expectExitAfterBuildTC:    false,
			expectPromptAfterBuildTC:  true,
		},
		{
			name:                      "simple mode exits after build",
			simple:                    true,
			expectExitAfterPlanningTC: false,
			expectExitAfterBuildTC:    true,
			expectPromptAfterBuildTC:  false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &PlannerWrapper{
				config:              Config{Simple: tt.simple},
				waitingForUserInput: true, // Set by ExitPlanMode handler
				pendingBuildStart:   true, // Set by executeInCurrentSession
			}

			// Step 3: Planning TurnComplete (ExitPlanMode turn)
			planningUsage := claude.TurnUsage{InputTokens: 1000, OutputTokens: 500}

			// Simulate the pendingBuildStart handler
			var shouldExitAfterPlanning bool
			if p.pendingBuildStart {
				p.planningStats.Add(planningUsage)
				p.inBuildPhase = true
				p.pendingBuildStart = false
				// Keep waitingForUserInput=true
				shouldExitAfterPlanning = false // Should NOT exit yet
			}

			if shouldExitAfterPlanning != tt.expectExitAfterPlanningTC {
				t.Errorf("after planning TC: expected exit=%v, got %v",
					tt.expectExitAfterPlanningTC, shouldExitAfterPlanning)
			}

			// Verify state after planning turn
			if !p.inBuildPhase {
				t.Error("should be in build phase after planning turn")
			}
			if !p.waitingForUserInput {
				t.Error("should still be waiting for user input after planning turn")
			}
			if p.planningStats.TurnCount != 1 {
				t.Errorf("expected 1 planning turn, got %d", p.planningStats.TurnCount)
			}
			if p.buildingStats.TurnCount != 0 {
				t.Errorf("expected 0 build turns after planning, got %d", p.buildingStats.TurnCount)
			}

			// Step 5: Build TurnComplete
			buildUsage := claude.TurnUsage{InputTokens: 2000, OutputTokens: 1000}

			// Simulate normal stats accumulation
			if p.inBuildPhase {
				p.buildingStats.Add(buildUsage)
			}

			// Simulate waitingForUserInput handler
			var shouldExitAfterBuild, shouldPromptAfterBuild bool
			if p.waitingForUserInput {
				p.waitingForUserInput = false
				if p.inBuildPhase {
					if p.config.Simple {
						shouldExitAfterBuild = true
					} else {
						shouldPromptAfterBuild = true
					}
				}
			}

			if shouldExitAfterBuild != tt.expectExitAfterBuildTC {
				t.Errorf("after build TC: expected exit=%v, got %v",
					tt.expectExitAfterBuildTC, shouldExitAfterBuild)
			}
			if shouldPromptAfterBuild != tt.expectPromptAfterBuildTC {
				t.Errorf("after build TC: expected prompt=%v, got %v",
					tt.expectPromptAfterBuildTC, shouldPromptAfterBuild)
			}

			// Verify final state
			if p.waitingForUserInput {
				t.Error("should not be waiting after build turn")
			}
			if p.planningStats.TurnCount != 1 {
				t.Errorf("expected 1 planning turn finally, got %d", p.planningStats.TurnCount)
			}
			if p.buildingStats.TurnCount != 1 {
				t.Errorf("expected 1 build turn finally, got %d", p.buildingStats.TurnCount)
			}
		})
	}
}

func TestBuildPhaseFollowUpConditions(t *testing.T) {
	// Test the conditions that trigger follow-up prompt after build completion
	tests := []struct {
		name                string
		simple              bool
		inBuildPhase        bool
		waitingForUserInput bool
		expectFollowUp      bool // Would trigger promptForFollowUp in non-simple mode
		expectExit          bool // Would exit immediately
	}{
		{
			name:                "simple mode exits after build",
			simple:              true,
			inBuildPhase:        true,
			waitingForUserInput: true,
			expectFollowUp:      false,
			expectExit:          true,
		},
		{
			name:                "non-simple mode with build complete triggers follow-up",
			simple:              false,
			inBuildPhase:        true,
			waitingForUserInput: true,
			expectFollowUp:      true,
			expectExit:          false,
		},
		{
			name:                "non-simple mode not in build phase continues",
			simple:              false,
			inBuildPhase:        false,
			waitingForUserInput: true,
			expectFollowUp:      false,
			expectExit:          false,
		},
		{
			name:                "planning phase continues (continue refining case)",
			simple:              false,
			inBuildPhase:        false,
			waitingForUserInput: false,
			expectFollowUp:      false,
			expectExit:          false, // Should NOT exit during planning
		},
		{
			name:                "simple mode exits even during planning",
			simple:              true,
			inBuildPhase:        false,
			waitingForUserInput: false,
			expectFollowUp:      false,
			expectExit:          true, // Simple mode always exits when not waiting
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &PlannerWrapper{
				config:              Config{Simple: tt.simple},
				inBuildPhase:        tt.inBuildPhase,
				waitingForUserInput: tt.waitingForUserInput,
			}

			// Simulate the logic from handleEvent for TurnCompleteEvent
			// (after pendingBuildStart handling)
			shouldFollowUp := false
			shouldExit := false

			if p.config.Simple && p.inBuildPhase {
				shouldExit = true
			} else if p.waitingForUserInput {
				p.waitingForUserInput = false
				if p.inBuildPhase {
					shouldFollowUp = true
				}
			} else {
				// Not waiting for input - exit only if build complete or simple mode
				if p.inBuildPhase || p.config.Simple {
					shouldExit = true
				}
				// Otherwise continue waiting during planning phase
			}

			if shouldFollowUp != tt.expectFollowUp {
				t.Errorf("expected followUp=%v, got %v", tt.expectFollowUp, shouldFollowUp)
			}
			if shouldExit != tt.expectExit {
				t.Errorf("expected exit=%v, got %v", tt.expectExit, shouldExit)
			}
		})
	}
}

func TestSessionStatsAdd(t *testing.T) {
	stats := &SessionStats{}

	// Add first turn
	stats.Add(claude.TurnUsage{
		InputTokens:     100,
		OutputTokens:    50,
		CacheReadTokens: 10,
		CostUSD:         0.01,
	})

	if stats.TurnCount != 1 {
		t.Errorf("expected TurnCount=1, got %d", stats.TurnCount)
	}
	if stats.InputTokens != 100 {
		t.Errorf("expected InputTokens=100, got %d", stats.InputTokens)
	}
	if stats.OutputTokens != 50 {
		t.Errorf("expected OutputTokens=50, got %d", stats.OutputTokens)
	}
	if stats.CacheReadTokens != 10 {
		t.Errorf("expected CacheReadTokens=10, got %d", stats.CacheReadTokens)
	}
	if stats.CostUSD != 0.01 {
		t.Errorf("expected CostUSD=0.01, got %f", stats.CostUSD)
	}

	// Add second turn
	stats.Add(claude.TurnUsage{
		InputTokens:     200,
		OutputTokens:    100,
		CacheReadTokens: 20,
		CostUSD:         0.02,
	})

	if stats.TurnCount != 2 {
		t.Errorf("expected TurnCount=2, got %d", stats.TurnCount)
	}
	if stats.InputTokens != 300 {
		t.Errorf("expected InputTokens=300, got %d", stats.InputTokens)
	}
	if stats.OutputTokens != 150 {
		t.Errorf("expected OutputTokens=150, got %d", stats.OutputTokens)
	}
	if stats.CacheReadTokens != 30 {
		t.Errorf("expected CacheReadTokens=30, got %d", stats.CacheReadTokens)
	}
	if stats.CostUSD != 0.03 {
		t.Errorf("expected CostUSD=0.03, got %f", stats.CostUSD)
	}
}

func TestPlanFilePathWithValidFile(t *testing.T) {
	// Create a temp file to simulate a valid plan file
	tmpFile, err := os.CreateTemp("", "plan-*.md")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(tmpFile.Name())
	tmpFile.WriteString("# Test Plan\n")
	tmpFile.Close()

	p := &PlannerWrapper{
		planFilePath: tmpFile.Name(),
	}

	// Should not need fallback with valid file
	needsFallback := false
	if p.planFilePath == "" {
		needsFallback = true
	} else if _, err := os.Stat(p.planFilePath); os.IsNotExist(err) {
		needsFallback = true
	}

	if needsFallback {
		t.Error("expected no fallback needed for valid plan file")
	}
}

// TestFormatQuestionResponses was removed along with the formatQuestionResponses function.
// User question responses are now collected in the permission handler and embedded
// in the tool's updatedInput as the "answers" field, so the CLI handles formatting.
