package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadWorkflow_FullFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")

	content := `---
tracker:
  kind: linear
  api_key: $LINEAR_API_KEY
  project_slug: MY-PROJECT
---
You are working on {{ issue.title }}.
`
	os.WriteFile(path, []byte(content), 0644)

	wf, err := LoadWorkflow(path)
	if err != nil {
		t.Fatal(err)
	}

	if wf.Config["tracker"] == nil {
		t.Fatal("expected tracker config")
	}
	tracker := wf.Config["tracker"].(map[string]any)
	if tracker["kind"] != "linear" {
		t.Errorf("tracker.kind = %v, want linear", tracker["kind"])
	}
	if wf.PromptTemplate != "You are working on {{ issue.title }}." {
		t.Errorf("prompt = %q", wf.PromptTemplate)
	}
}

func TestLoadWorkflow_NoFrontMatter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")

	content := "Just a prompt, no config.\n"
	os.WriteFile(path, []byte(content), 0644)

	wf, err := LoadWorkflow(path)
	if err != nil {
		t.Fatal(err)
	}

	if len(wf.Config) != 0 {
		t.Errorf("expected empty config, got %v", wf.Config)
	}
	if wf.PromptTemplate != "Just a prompt, no config." {
		t.Errorf("prompt = %q", wf.PromptTemplate)
	}
}

func TestLoadWorkflow_EmptyFrontMatter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")

	content := "---\n---\nPrompt body.\n"
	os.WriteFile(path, []byte(content), 0644)

	wf, err := LoadWorkflow(path)
	if err != nil {
		t.Fatal(err)
	}

	if len(wf.Config) != 0 {
		t.Errorf("expected empty config, got %v", wf.Config)
	}
	if wf.PromptTemplate != "Prompt body." {
		t.Errorf("prompt = %q", wf.PromptTemplate)
	}
}

func TestLoadWorkflow_MissingFile(t *testing.T) {
	_, err := LoadWorkflow("/nonexistent/WORKFLOW.md")
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	wfErr, ok := err.(*WorkflowError)
	if !ok {
		t.Fatalf("expected WorkflowError, got %T", err)
	}
	if wfErr.Code != "missing_workflow_file" {
		t.Errorf("code = %q, want missing_workflow_file", wfErr.Code)
	}
}

func TestLoadWorkflow_NonMapFrontMatter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")

	content := "---\n- item1\n- item2\n---\nPrompt.\n"
	os.WriteFile(path, []byte(content), 0644)

	_, err := LoadWorkflow(path)
	if err == nil {
		t.Fatal("expected error for non-map front matter")
	}
	wfErr, ok := err.(*WorkflowError)
	if !ok {
		t.Fatalf("expected WorkflowError, got %T", err)
	}
	if wfErr.Code != "workflow_front_matter_not_a_map" {
		t.Errorf("code = %q, want workflow_front_matter_not_a_map", wfErr.Code)
	}
}

func TestLoadWorkflow_InvalidYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")

	content := "---\n: invalid: yaml: {{{\n---\nPrompt.\n"
	os.WriteFile(path, []byte(content), 0644)

	_, err := LoadWorkflow(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML")
	}
	wfErr, ok := err.(*WorkflowError)
	if !ok {
		t.Fatalf("expected WorkflowError, got %T", err)
	}
	if wfErr.Code != "workflow_parse_error" {
		t.Errorf("code = %q, want workflow_parse_error", wfErr.Code)
	}
}

func TestLoadWorkflow_UnterminatedFrontMatter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "WORKFLOW.md")

	content := "---\ntracker:\n  kind: linear\n"
	os.WriteFile(path, []byte(content), 0644)

	_, err := LoadWorkflow(path)
	if err == nil {
		t.Fatal("expected error for unterminated front matter")
	}
	wfErr := err.(*WorkflowError)
	if wfErr.Code != "workflow_parse_error" {
		t.Errorf("code = %q, want workflow_parse_error", wfErr.Code)
	}
}
