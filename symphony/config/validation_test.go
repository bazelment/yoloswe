package config

import (
	"testing"

	"github.com/bazelment/yoloswe/symphony/model"
)

func TestValidateForDispatch_Valid(t *testing.T) {
	cfg := NewServiceConfig(&model.WorkflowDefinition{
		Config: map[string]any{
			"tracker": map[string]any{
				"kind":         "linear",
				"api_key":      "test-key",
				"project_slug": "MY-PROJ",
			},
		},
	})

	if err := ValidateForDispatch(cfg); err != nil {
		t.Errorf("expected no error, got %v", err)
	}
}

func TestValidateForDispatch_MissingTrackerKind(t *testing.T) {
	cfg := NewServiceConfig(&model.WorkflowDefinition{
		Config: map[string]any{},
	})

	err := ValidateForDispatch(cfg)
	if err == nil {
		t.Fatal("expected error")
	}
	vErr := err.(*ValidationError)
	if len(vErr.Checks) == 0 {
		t.Fatal("expected validation checks")
	}
}

func TestValidateForDispatch_UnsupportedTracker(t *testing.T) {
	cfg := NewServiceConfig(&model.WorkflowDefinition{
		Config: map[string]any{
			"tracker": map[string]any{
				"kind":    "jira",
				"api_key": "test-key",
			},
		},
	})

	err := ValidateForDispatch(cfg)
	if err == nil {
		t.Fatal("expected error")
	}
	vErr := err.(*ValidationError)
	found := false
	for _, c := range vErr.Checks {
		if c == `unsupported tracker.kind: "jira"` {
			found = true
		}
	}
	if !found {
		t.Errorf("expected unsupported tracker kind check, got %v", vErr.Checks)
	}
}

func TestValidateForDispatch_MissingAPIKey(t *testing.T) {
	cfg := NewServiceConfig(&model.WorkflowDefinition{
		Config: map[string]any{
			"tracker": map[string]any{
				"kind":         "linear",
				"project_slug": "MY-PROJ",
			},
		},
	})

	err := ValidateForDispatch(cfg)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestValidateForDispatch_MissingProjectSlug(t *testing.T) {
	cfg := NewServiceConfig(&model.WorkflowDefinition{
		Config: map[string]any{
			"tracker": map[string]any{
				"kind":    "linear",
				"api_key": "test-key",
			},
		},
	})

	err := ValidateForDispatch(cfg)
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestValidateForDispatch_UnsupportedAgentType(t *testing.T) {
	cfg := NewServiceConfig(&model.WorkflowDefinition{
		Config: map[string]any{
			"tracker": map[string]any{
				"kind":         "linear",
				"api_key":      "test-key",
				"project_slug": "MY-PROJ",
			},
			"agent_session": map[string]any{
				"type": "codxe",
			},
		},
	})

	err := ValidateForDispatch(cfg)
	if err == nil {
		t.Fatal("expected error for unsupported agent type")
	}
	vErr := err.(*ValidationError)
	found := false
	for _, c := range vErr.Checks {
		if c == `unsupported agent_session.type: "codxe" (supported: "codex")` {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("expected unsupported agent type check, got %v", vErr.Checks)
	}
}

func TestValidateForDispatch_EmptyAgentCommand(t *testing.T) {
	cfg := NewServiceConfig(&model.WorkflowDefinition{
		Config: map[string]any{
			"tracker": map[string]any{
				"kind":         "linear",
				"api_key":      "test-key",
				"project_slug": "MY-PROJ",
			},
			"codex": map[string]any{
				"command": "",
			},
		},
	})

	err := ValidateForDispatch(cfg)
	if err == nil {
		t.Fatal("expected error for empty agent command")
	}
}
