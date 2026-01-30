package yoloswe

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

func TestNewBuilderSession(t *testing.T) {
	// Get expected default recording dir
	homeDir, _ := os.UserHomeDir()
	defaultRecordingDir := filepath.Join(homeDir, ".yoloswe")

	tests := []struct {
		name           string
		config         BuilderConfig
		expectedModel  string
		expectedRecDir string
	}{
		{
			name:           "default values",
			config:         BuilderConfig{},
			expectedModel:  "sonnet",
			expectedRecDir: defaultRecordingDir,
		},
		{
			name: "custom values",
			config: BuilderConfig{
				Model:        "opus",
				RecordingDir: "/tmp/custom",
			},
			expectedModel:  "opus",
			expectedRecDir: "/tmp/custom",
		},
		{
			name: "empty model uses default",
			config: BuilderConfig{
				Model:        "",
				RecordingDir: "/custom",
			},
			expectedModel:  "sonnet",
			expectedRecDir: "/custom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			builder := NewBuilderSession(tt.config, &buf)

			if builder.config.Model != tt.expectedModel {
				t.Errorf("expected model %q, got %q", tt.expectedModel, builder.config.Model)
			}
			if builder.config.RecordingDir != tt.expectedRecDir {
				t.Errorf("expected recording dir %q, got %q", tt.expectedRecDir, builder.config.RecordingDir)
			}
			if builder.output != &buf {
				t.Error("output writer not set correctly")
			}
			if builder.renderer == nil {
				t.Error("renderer not initialized")
			}
		})
	}
}

func TestHandleAskUserQuestion(t *testing.T) {
	tests := []struct {
		name            string
		questions       []claude.Question
		expectedAnswers map[string]string
	}{
		{
			name:            "empty questions",
			questions:       []claude.Question{},
			expectedAnswers: map[string]string{},
		},
		{
			name: "single question with options",
			questions: []claude.Question{
				{
					Text:    "Choose an option?",
					Options: []claude.QuestionOption{{Label: "option1"}, {Label: "option2"}},
				},
			},
			expectedAnswers: map[string]string{
				"Choose an option?": "option1",
			},
		},
		{
			name: "multiple questions",
			questions: []claude.Question{
				{
					Text:    "Question 1?",
					Options: []claude.QuestionOption{{Label: "a"}, {Label: "b"}},
				},
				{
					Text:    "Question 2?",
					Options: []claude.QuestionOption{{Label: "yes"}},
				},
			},
			expectedAnswers: map[string]string{
				"Question 1?": "a",
				"Question 2?": "yes",
			},
		},
		{
			name: "question with no options",
			questions: []claude.Question{
				{
					Text:    "Continue?",
					Options: []claude.QuestionOption{},
				},
			},
			expectedAnswers: map[string]string{
				"Continue?": "yes",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			builder := NewBuilderSession(BuilderConfig{}, &buf)
			handler := &builderInteractiveHandler{b: builder}

			answers, err := handler.HandleAskUserQuestion(context.Background(), tt.questions)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(answers) != len(tt.expectedAnswers) {
				t.Errorf("expected %d answers, got %d", len(tt.expectedAnswers), len(answers))
			}

			for q, expectedA := range tt.expectedAnswers {
				if answers[q] != expectedA {
					t.Errorf("for question %q: expected %q, got %q", q, expectedA, answers[q])
				}
			}
		})
	}
}

func TestHandleExitPlanMode(t *testing.T) {
	var buf bytes.Buffer
	builder := NewBuilderSession(BuilderConfig{}, &buf)
	handler := &builderInteractiveHandler{b: builder}

	feedback, err := handler.HandleExitPlanMode(context.Background(), claude.PlanInfo{
		Plan: "Test plan",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expected := "Approved. Please proceed with implementation."
	if feedback != expected {
		t.Errorf("expected %q, got %q", expected, feedback)
	}
}

func TestBuilderStop(t *testing.T) {
	t.Run("stop with nil session", func(t *testing.T) {
		var buf bytes.Buffer
		builder := NewBuilderSession(BuilderConfig{}, &buf)
		// session is nil before Start
		if err := builder.Stop(); err != nil {
			t.Errorf("Stop() with nil session should not error, got %v", err)
		}
	})
}

func TestBuilderRecordingPath(t *testing.T) {
	t.Run("recording path with nil session", func(t *testing.T) {
		var buf bytes.Buffer
		builder := NewBuilderSession(BuilderConfig{}, &buf)
		// session is nil before Start
		path := builder.RecordingPath()
		if path != "" {
			t.Errorf("RecordingPath() with nil session should return empty string, got %q", path)
		}
	})
}

func TestBuilderConfigValidation(t *testing.T) {
	tests := []struct {
		name        string
		config      BuilderConfig
		shouldPanic bool
	}{
		{
			name: "valid config",
			config: BuilderConfig{
				Model:        "sonnet",
				WorkDir:      "/tmp",
				RecordingDir: "/tmp/recordings",
			},
			shouldPanic: false,
		},
		{
			name: "minimal config",
			config: BuilderConfig{
				Model: "haiku",
			},
			shouldPanic: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer func() {
				if r := recover(); r != nil && !tt.shouldPanic {
					t.Errorf("unexpected panic: %v", r)
				}
			}()

			var buf bytes.Buffer
			builder := NewBuilderSession(tt.config, &buf)
			if builder == nil {
				t.Error("NewBuilderSession returned nil")
			}
		})
	}
}

// TestBuilderRunTurnCancellation tests that RunTurn handles context cancellation correctly
func TestBuilderRunTurnCancellation(t *testing.T) {
	t.Skip("Requires integration test with real session")
}

// TestBuilderStartWithInvalidWorkDir tests that Start handles invalid work directory
func TestBuilderStartWithInvalidWorkDir(t *testing.T) {
	t.Skip("Requires integration test to verify error handling")
}

func TestHandleAskUserQuestionEdgeCases(t *testing.T) {
	tests := []struct {
		name            string
		questions       []claude.Question
		expectedAnswers map[string]string
	}{
		{
			name: "multiSelect flag with multiple options - selects first",
			questions: []claude.Question{
				{
					Text:        "Select features?",
					MultiSelect: true,
					Options: []claude.QuestionOption{
						{Label: "Feature A"},
						{Label: "Feature B"},
						{Label: "Feature C"},
					},
				},
			},
			expectedAnswers: map[string]string{
				"Select features?": "Feature A",
			},
		},
		{
			name: "question with no options uses yes",
			questions: []claude.Question{
				{
					Text:    "Which approach should we use?",
					Options: []claude.QuestionOption{},
				},
			},
			expectedAnswers: map[string]string{
				"Which approach should we use?": "yes",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			builder := NewBuilderSession(BuilderConfig{}, &buf)
			handler := &builderInteractiveHandler{b: builder}

			answers, err := handler.HandleAskUserQuestion(context.Background(), tt.questions)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			for q, expectedA := range tt.expectedAnswers {
				if answers[q] != expectedA {
					t.Errorf("for question %q: expected %q, got %q", q, expectedA, answers[q])
				}
			}
		})
	}
}

func TestBuilderNilOutput(t *testing.T) {
	// Should handle nil output gracefully
	builder := NewBuilderSession(BuilderConfig{}, nil)
	if builder == nil {
		t.Error("NewBuilderSession should not return nil even with nil output")
	}
}

func TestHandleAskUserQuestionPerformance(t *testing.T) {
	// Test with many questions to ensure no performance issues
	// Each question needs a unique Text to get separate answer entries
	questions := make([]claude.Question, 100)
	for i := 0; i < 100; i++ {
		questions[i] = claude.Question{
			Text:    "Question " + string(rune('A'+i/26)) + string(rune('a'+i%26)) + "?",
			Options: []claude.QuestionOption{{Label: "a"}, {Label: "b"}, {Label: "c"}},
		}
	}

	var buf bytes.Buffer
	builder := NewBuilderSession(BuilderConfig{}, &buf)
	handler := &builderInteractiveHandler{b: builder}

	answers, err := handler.HandleAskUserQuestion(context.Background(), questions)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should handle all questions
	if len(answers) != 100 {
		t.Errorf("expected 100 answers, got %d", len(answers))
	}
}
