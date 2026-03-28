// Package config handles WORKFLOW.md loading, typed config, validation, and dynamic reload.
package config

import (
	"fmt"
	"os"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/bazelment/yoloswe/symphony/model"
)

// LoadWorkflow reads and parses a WORKFLOW.md file into a WorkflowDefinition.
// Spec Section 5.1, 5.2.
func LoadWorkflow(path string) (*model.WorkflowDefinition, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, &WorkflowError{Code: "missing_workflow_file", Message: fmt.Sprintf("workflow file not found: %s", path)}
		}
		return nil, &WorkflowError{Code: "missing_workflow_file", Message: fmt.Sprintf("cannot read workflow file: %s: %v", path, err)}
	}

	content := string(data)
	config, promptBody, err := parseFrontMatter(content)
	if err != nil {
		return nil, err
	}

	return &model.WorkflowDefinition{
		Config:         config,
		PromptTemplate: strings.TrimSpace(promptBody),
	}, nil
}

// parseFrontMatter splits YAML front matter from the prompt body.
// If file starts with "---", parse lines until the next "---" as YAML.
// Remaining lines become the prompt body.
func parseFrontMatter(content string) (map[string]any, string, error) {
	if !strings.HasPrefix(content, "---") {
		// No front matter: entire file is prompt body, empty config.
		return map[string]any{}, content, nil
	}

	// Find end delimiter by splitting into lines.
	lines := strings.SplitAfter(content, "\n")
	// First line is "---\n" (or "---\r\n"), skip it.
	endIdx := -1
	for i := 1; i < len(lines); i++ {
		trimmed := strings.TrimRight(lines[i], "\r\n")
		if trimmed == "---" {
			endIdx = i
			break
		}
	}
	if endIdx < 0 {
		return nil, "", &WorkflowError{Code: "workflow_parse_error", Message: "unterminated YAML front matter: no closing ---"}
	}

	yamlStr := strings.Join(lines[1:endIdx], "")
	body := strings.Join(lines[endIdx+1:], "")

	var parsed any
	if err := yaml.Unmarshal([]byte(yamlStr), &parsed); err != nil {
		return nil, "", &WorkflowError{Code: "workflow_parse_error", Message: fmt.Sprintf("invalid YAML front matter: %v", err)}
	}

	if parsed == nil {
		// Empty front matter is allowed, treated as empty map.
		return map[string]any{}, body, nil
	}

	configMap, ok := parsed.(map[string]any)
	if !ok {
		return nil, "", &WorkflowError{Code: "workflow_front_matter_not_a_map", Message: "YAML front matter must be a map/object"}
	}

	return configMap, body, nil
}

// WorkflowError represents a workflow loading or parsing error.
type WorkflowError struct {
	Code    string
	Message string
}

func (e *WorkflowError) Error() string {
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}
