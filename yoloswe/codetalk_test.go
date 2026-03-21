package yoloswe

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

func TestNewCodeTalkSession(t *testing.T) {
	t.Parallel()

	homeDir, _ := os.UserHomeDir()
	wantDefaultRecordingDir := filepath.Join(homeDir, ".yoloswe")

	tests := []struct {
		name           string
		expectedModel  string
		expectedRecDir string
		config         CodeTalkConfig
	}{
		{
			name:           "default values",
			config:         CodeTalkConfig{},
			expectedModel:  "opus",
			expectedRecDir: wantDefaultRecordingDir,
		},
		{
			name: "custom model and recording dir",
			config: CodeTalkConfig{
				Model:        "sonnet",
				RecordingDir: "/tmp/custom",
			},
			expectedModel:  "sonnet",
			expectedRecDir: "/tmp/custom",
		},
		{
			name: "empty model uses default",
			config: CodeTalkConfig{
				Model:        "",
				RecordingDir: "/custom",
			},
			expectedModel:  "opus",
			expectedRecDir: "/custom",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			ct := NewCodeTalkSession(tt.config, &buf)

			if ct.config.Model != tt.expectedModel {
				t.Errorf("expected model %q, got %q", tt.expectedModel, ct.config.Model)
			}
			if ct.config.RecordingDir != tt.expectedRecDir {
				t.Errorf("expected recording dir %q, got %q", tt.expectedRecDir, ct.config.RecordingDir)
			}
			if ct.output != &buf {
				t.Error("output writer not set correctly")
			}
			if ct.renderer == nil {
				t.Error("renderer not initialized")
			}
		})
	}
}

func TestNewCodeTalkSessionWithEvents_NilOutput(t *testing.T) {
	t.Parallel()

	ct := NewCodeTalkSessionWithEvents(CodeTalkConfig{}, nil, nil)
	if ct == nil {
		t.Fatal("NewCodeTalkSessionWithEvents returned nil")
	}
	if ct.renderer == nil {
		t.Error("renderer not initialized")
	}
}

func TestCodeTalkHandleAskUserQuestion(t *testing.T) {
	t.Parallel()

	tests := []struct {
		expectedAnswers map[string]string
		name            string
		questions       []claude.Question
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
		{
			name: "multiple questions",
			questions: []claude.Question{
				{
					Text:    "Question 1?",
					Options: []claude.QuestionOption{{Label: "a"}, {Label: "b"}},
				},
				{
					Text:    "Question 2?",
					Options: []claude.QuestionOption{},
				},
			},
			expectedAnswers: map[string]string{
				"Question 1?": "a",
				"Question 2?": "yes",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			ct := NewCodeTalkSession(CodeTalkConfig{}, &buf)
			handler := &codetalkInteractiveHandler{ct: ct}

			answers, err := handler.HandleAskUserQuestion(context.Background(), tt.questions)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(answers) != len(tt.expectedAnswers) {
				t.Errorf("expected %d answers, got %d", len(tt.expectedAnswers), len(answers))
			}
			for q, want := range tt.expectedAnswers {
				if got := answers[q]; got != want {
					t.Errorf("for question %q: expected %q, got %q", q, want, got)
				}
			}
		})
	}
}

func TestCodeTalkHandleExitPlanMode_Denies(t *testing.T) {
	t.Parallel()

	var buf bytes.Buffer
	ct := NewCodeTalkSession(CodeTalkConfig{}, &buf)
	handler := &codetalkInteractiveHandler{ct: ct}

	feedback, err := handler.HandleExitPlanMode(context.Background(), claude.PlanInfo{
		Plan: "Test plan",
	})
	if err == nil {
		t.Error("HandleExitPlanMode should return an error to deny plan exit in a read-only session")
	}
	if feedback != "" {
		t.Errorf("HandleExitPlanMode should return empty feedback on denial, got %q", feedback)
	}
}

func TestCodeTalkStop(t *testing.T) {
	t.Parallel()

	t.Run("stop with nil session", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		ct := NewCodeTalkSession(CodeTalkConfig{}, &buf)
		// session is nil before Start
		if err := ct.Stop(); err != nil {
			t.Errorf("Stop() with nil session should not error, got %v", err)
		}
	})
}

func TestCodeTalkRecordingPath(t *testing.T) {
	t.Parallel()

	t.Run("recording path with nil session", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		ct := NewCodeTalkSession(CodeTalkConfig{}, &buf)
		// session is nil before Start
		path := ct.RecordingPath()
		if path != "" {
			t.Errorf("RecordingPath() with nil session should return empty string, got %q", path)
		}
	})
}

func TestCodeTalkCLISessionID(t *testing.T) {
	t.Parallel()

	t.Run("CLISessionID with nil session", func(t *testing.T) {
		t.Parallel()
		var buf bytes.Buffer
		ct := NewCodeTalkSession(CodeTalkConfig{}, &buf)
		// session is nil before Start
		id := ct.CLISessionID()
		if id != "" {
			t.Errorf("CLISessionID() with nil session should return empty string, got %q", id)
		}
	})
}

func TestCodeTalkNilOutput(t *testing.T) {
	t.Parallel()

	// Should handle nil output gracefully
	ct := NewCodeTalkSession(CodeTalkConfig{}, nil)
	if ct == nil {
		t.Error("NewCodeTalkSession should not return nil even with nil output")
	}
}
