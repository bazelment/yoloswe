package claude

import (
	"context"
	"testing"
)

func TestParseQuestionsFromInput(t *testing.T) {
	tests := []struct {
		name        string
		input       map[string]interface{}
		expected    []Question
		expectError bool
	}{
		{
			name:        "missing questions field",
			input:       map[string]interface{}{},
			expectError: true,
		},
		{
			name: "questions not an array",
			input: map[string]interface{}{
				"questions": "not an array",
			},
			expectError: true,
		},
		{
			name: "empty questions array",
			input: map[string]interface{}{
				"questions": []interface{}{},
			},
			expected: []Question{},
		},
		{
			name: "question missing text",
			input: map[string]interface{}{
				"questions": []interface{}{
					map[string]interface{}{
						"options": []interface{}{"a", "b"},
					},
				},
			},
			expectError: true,
		},
		{
			name: "single question with string options",
			input: map[string]interface{}{
				"questions": []interface{}{
					map[string]interface{}{
						"question": "Pick one?",
						"options":  []interface{}{"option1", "option2"},
					},
				},
			},
			expected: []Question{
				{
					Text: "Pick one?",
					Options: []QuestionOption{
						{Label: "option1"},
						{Label: "option2"},
					},
				},
			},
		},
		{
			name: "single question with label options",
			input: map[string]interface{}{
				"questions": []interface{}{
					map[string]interface{}{
						"question": "Select library?",
						"options": []interface{}{
							map[string]interface{}{"label": "React"},
							map[string]interface{}{"label": "Vue"},
						},
					},
				},
			},
			expected: []Question{
				{
					Text: "Select library?",
					Options: []QuestionOption{
						{Label: "React"},
						{Label: "Vue"},
					},
				},
			},
		},
		{
			name: "question with multiSelect",
			input: map[string]interface{}{
				"questions": []interface{}{
					map[string]interface{}{
						"question":    "Select features?",
						"multiSelect": true,
						"options":     []interface{}{"A", "B", "C"},
					},
				},
			},
			expected: []Question{
				{
					Text:        "Select features?",
					MultiSelect: true,
					Options: []QuestionOption{
						{Label: "A"},
						{Label: "B"},
						{Label: "C"},
					},
				},
			},
		},
		{
			name: "multiple questions",
			input: map[string]interface{}{
				"questions": []interface{}{
					map[string]interface{}{
						"question": "Q1?",
						"options":  []interface{}{"yes", "no"},
					},
					map[string]interface{}{
						"question": "Q2?",
						"options":  []interface{}{},
					},
				},
			},
			expected: []Question{
				{
					Text:    "Q1?",
					Options: []QuestionOption{{Label: "yes"}, {Label: "no"}},
				},
				{
					Text:    "Q2?",
					Options: []QuestionOption{},
				},
			},
		},
		{
			name: "question not an object",
			input: map[string]interface{}{
				"questions": []interface{}{
					"not an object",
				},
			},
			expectError: true,
		},
		{
			name: "options not an array",
			input: map[string]interface{}{
				"questions": []interface{}{
					map[string]interface{}{
						"question": "Test?",
						"options":  "not an array",
					},
				},
			},
			expectError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			questions, err := ParseQuestionsFromInput(tt.input)

			if tt.expectError {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(questions) != len(tt.expected) {
				t.Fatalf("expected %d questions, got %d", len(tt.expected), len(questions))
			}

			for i, q := range questions {
				exp := tt.expected[i]
				if q.Text != exp.Text {
					t.Errorf("question %d: expected text %q, got %q", i, exp.Text, q.Text)
				}
				if q.MultiSelect != exp.MultiSelect {
					t.Errorf("question %d: expected multiSelect %v, got %v", i, exp.MultiSelect, q.MultiSelect)
				}
				if len(q.Options) != len(exp.Options) {
					t.Errorf("question %d: expected %d options, got %d", i, len(exp.Options), len(q.Options))
					continue
				}
				for j, opt := range q.Options {
					if opt.Label != exp.Options[j].Label {
						t.Errorf("question %d option %d: expected label %q, got %q", i, j, exp.Options[j].Label, opt.Label)
					}
				}
			}
		})
	}
}

func TestParsePlanInfoFromInput(t *testing.T) {
	tests := []struct {
		name        string
		input       map[string]interface{}
		expected    PlanInfo
		expectError bool
	}{
		{
			name:     "empty input",
			input:    map[string]interface{}{},
			expected: PlanInfo{},
		},
		{
			name: "allowedPrompts not an array",
			input: map[string]interface{}{
				"allowedPrompts": "not an array",
			},
			expectError: true,
		},
		{
			name: "with allowedPrompts",
			input: map[string]interface{}{
				"allowedPrompts": []interface{}{
					map[string]interface{}{
						"tool":   "Bash",
						"prompt": "run tests",
					},
					map[string]interface{}{
						"tool":   "Write",
						"prompt": "create files",
					},
				},
			},
			expected: PlanInfo{
				AllowedPrompts: []AllowedPrompt{
					{Tool: "Bash", Prompt: "run tests"},
					{Tool: "Write", Prompt: "create files"},
				},
			},
		},
		{
			name: "skips non-object prompts",
			input: map[string]interface{}{
				"allowedPrompts": []interface{}{
					"not an object",
					map[string]interface{}{
						"tool":   "Bash",
						"prompt": "run tests",
					},
				},
			},
			expected: PlanInfo{
				AllowedPrompts: []AllowedPrompt{
					{Tool: "Bash", Prompt: "run tests"},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info, err := ParsePlanInfoFromInput(tt.input)

			if tt.expectError {
				if err == nil {
					t.Errorf("expected error, got nil")
				}
				return
			}

			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(info.AllowedPrompts) != len(tt.expected.AllowedPrompts) {
				t.Fatalf("expected %d allowedPrompts, got %d", len(tt.expected.AllowedPrompts), len(info.AllowedPrompts))
			}

			for i, p := range info.AllowedPrompts {
				exp := tt.expected.AllowedPrompts[i]
				if p.Tool != exp.Tool {
					t.Errorf("prompt %d: expected tool %q, got %q", i, exp.Tool, p.Tool)
				}
				if p.Prompt != exp.Prompt {
					t.Errorf("prompt %d: expected prompt %q, got %q", i, exp.Prompt, p.Prompt)
				}
			}
		})
	}
}

func TestBuildAllowResponseWithAnswers(t *testing.T) {
	requestID := "test-req-123"
	originalInput := map[string]interface{}{
		"questions": []interface{}{
			map[string]interface{}{"question": "Q1?"},
		},
	}
	answers := map[string]string{
		"Q1?": "Answer 1",
	}

	resp := buildAllowResponseWithAnswers(requestID, originalInput, answers)

	if resp == nil {
		t.Fatal("response should not be nil")
	}

	// Check response structure
	if resp.Response.RequestID != requestID {
		t.Errorf("expected request ID %q, got %q", requestID, resp.Response.RequestID)
	}

	if resp.Response.Subtype != "success" {
		t.Errorf("expected subtype 'success', got %q", resp.Response.Subtype)
	}
}

func TestBuildAllowResponseWithFeedback(t *testing.T) {
	requestID := "test-req-456"
	originalInput := map[string]interface{}{
		"plan": "test plan",
	}
	feedback := "Approved. Please proceed."

	resp := buildAllowResponseWithFeedback(requestID, originalInput, feedback)

	if resp == nil {
		t.Fatal("response should not be nil")
	}

	// Check response structure
	if resp.Response.RequestID != requestID {
		t.Errorf("expected request ID %q, got %q", requestID, resp.Response.RequestID)
	}
}

func TestBuildDenyResponse(t *testing.T) {
	tests := []struct {
		name      string
		requestID string
		message   string
		interrupt bool
	}{
		{
			name:      "deny without interrupt",
			requestID: "req-1",
			message:   "Permission denied",
			interrupt: false,
		},
		{
			name:      "deny with interrupt",
			requestID: "req-2",
			message:   "User cancelled",
			interrupt: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp := buildDenyResponse(tt.requestID, tt.message, tt.interrupt)

			if resp == nil {
				t.Fatal("response should not be nil")
			}

			if resp.Response.RequestID != tt.requestID {
				t.Errorf("expected request ID %q, got %q", tt.requestID, resp.Response.RequestID)
			}

			if resp.Response.Subtype != "success" {
				t.Errorf("expected subtype 'success', got %q", resp.Response.Subtype)
			}
		})
	}
}

// mockInteractiveHandler is a mock implementation for testing
type mockInteractiveHandler struct {
	askUserQuestionFunc func(ctx context.Context, questions []Question) (map[string]string, error)
	exitPlanModeFunc    func(ctx context.Context, plan PlanInfo) (string, error)
}

func (m *mockInteractiveHandler) HandleAskUserQuestion(ctx context.Context, questions []Question) (map[string]string, error) {
	if m.askUserQuestionFunc != nil {
		return m.askUserQuestionFunc(ctx, questions)
	}
	return map[string]string{}, nil
}

func (m *mockInteractiveHandler) HandleExitPlanMode(ctx context.Context, plan PlanInfo) (string, error) {
	if m.exitPlanModeFunc != nil {
		return m.exitPlanModeFunc(ctx, plan)
	}
	return "Approved", nil
}

func TestMockInteractiveHandler(t *testing.T) {
	// Test that mock handler satisfies the interface
	var handler InteractiveToolHandler = &mockInteractiveHandler{
		askUserQuestionFunc: func(ctx context.Context, questions []Question) (map[string]string, error) {
			answers := make(map[string]string)
			for _, q := range questions {
				answers[q.Text] = "mock answer"
			}
			return answers, nil
		},
		exitPlanModeFunc: func(ctx context.Context, plan PlanInfo) (string, error) {
			return "mock feedback", nil
		},
	}

	// Test HandleAskUserQuestion
	questions := []Question{{Text: "Test question?"}}
	answers, err := handler.HandleAskUserQuestion(context.Background(), questions)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if answers["Test question?"] != "mock answer" {
		t.Errorf("expected 'mock answer', got %q", answers["Test question?"])
	}

	// Test HandleExitPlanMode
	feedback, err := handler.HandleExitPlanMode(context.Background(), PlanInfo{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if feedback != "mock feedback" {
		t.Errorf("expected 'mock feedback', got %q", feedback)
	}
}
