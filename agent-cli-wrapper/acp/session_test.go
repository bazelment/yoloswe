package acp

import (
	"fmt"
	"testing"
)

func TestIsRecoverablePromptError(t *testing.T) {
	tests := []struct {
		err             error
		name            string
		hasText         bool
		hasThink        bool
		hasToolActivity bool
		wantRecover     bool
	}{
		{
			name:        "RPCError 500 with empty response text and accumulated thinking",
			err:         &RPCError{Code: 500, Message: "Model stream ended with empty response text."},
			hasThink:    true,
			wantRecover: true,
		},
		{
			name:        "RPCError 500 with empty response text and accumulated text",
			err:         &RPCError{Code: 500, Message: "Model stream ended with empty response text."},
			hasText:     true,
			wantRecover: true,
		},
		{
			name:            "RPCError 500 with empty response text and tool activity only",
			err:             &RPCError{Code: 500, Message: "Model stream ended with empty response text."},
			hasToolActivity: true,
			wantRecover:     true,
		},
		{
			name:        "RPCError 500 with empty response text but no activity",
			err:         &RPCError{Code: 500, Message: "Model stream ended with empty response text."},
			wantRecover: false,
		},
		{
			name:        "RPCError 500 with different message",
			err:         &RPCError{Code: 500, Message: "Internal server error"},
			hasText:     true,
			wantRecover: false,
		},
		{
			name:        "RPCError non-500 with matching message",
			err:         &RPCError{Code: 400, Message: "Model stream ended with empty response text."},
			hasText:     true,
			wantRecover: false,
		},
		{
			name:        "non-RPCError",
			err:         fmt.Errorf("some other error"),
			hasText:     true,
			wantRecover: false,
		},
		{
			name:        "wrapped RPCError 500 with empty response text",
			err:         fmt.Errorf("wrapped: %w", &RPCError{Code: 500, Message: "Model stream ended with empty response text."}),
			hasThink:    true,
			wantRecover: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Session{
				state: newSessionStateManager(),
			}
			if tt.hasText {
				s.text.WriteString("some text")
			}
			if tt.hasThink {
				s.thinking.WriteString("some thinking")
			}
			if tt.hasToolActivity {
				s.sawToolActivity = true
			}

			got := s.isRecoverablePromptError(tt.err)
			if got != tt.wantRecover {
				t.Errorf("isRecoverablePromptError() = %v, want %v", got, tt.wantRecover)
			}
		})
	}
}
