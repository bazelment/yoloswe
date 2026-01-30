package protocol

import "testing"

func TestReviewResponseHasCriticalIssues(t *testing.T) {
	tests := []struct {
		response *ReviewResponse
		name     string
		want     bool
	}{
		{
			name: "empty issues",
			response: &ReviewResponse{
				Issues: []Issue{},
			},
			want: false,
		},
		{
			name: "nil issues",
			response: &ReviewResponse{
				Issues: nil,
			},
			want: false,
		},
		{
			name: "only minor issues",
			response: &ReviewResponse{
				Issues: []Issue{
					{Severity: "minor", Message: "Style"},
					{Severity: "nitpick", Message: "Naming"},
				},
			},
			want: false,
		},
		{
			name: "one critical issue",
			response: &ReviewResponse{
				Issues: []Issue{
					{Severity: "minor", Message: "Style"},
					{Severity: "critical", Message: "Security bug"},
				},
			},
			want: true,
		},
		{
			name: "multiple critical issues",
			response: &ReviewResponse{
				Issues: []Issue{
					{Severity: "critical", Message: "Bug 1"},
					{Severity: "critical", Message: "Bug 2"},
				},
			},
			want: true,
		},
		{
			name: "only critical issues",
			response: &ReviewResponse{
				Issues: []Issue{
					{Severity: "critical", Message: "Only critical"},
				},
			},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.response.HasCriticalIssues()
			if got != tt.want {
				t.Errorf("HasCriticalIssues() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDesignRequest(t *testing.T) {
	req := &DesignRequest{
		Task:        "Create a CLI",
		Context:     "Go project",
		Constraints: []string{"Use cobra", "Support flags"},
	}

	if req.Task != "Create a CLI" {
		t.Errorf("Task = %v, want 'Create a CLI'", req.Task)
	}

	if req.Context != "Go project" {
		t.Errorf("Context = %v, want 'Go project'", req.Context)
	}

	if len(req.Constraints) != 2 {
		t.Errorf("len(Constraints) = %v, want 2", len(req.Constraints))
	}
}

func TestDesignResponse(t *testing.T) {
	resp := &DesignResponse{
		Architecture: "Layered architecture",
		Files: []FileSpec{
			{Path: "main.go", Purpose: "Entry point", Action: "create"},
			{Path: "cli.go", Purpose: "CLI logic", Action: "create"},
		},
		Interfaces:          "type CLI interface { Run() }",
		ImplementationNotes: []string{"Start with main", "Add tests"},
		Dependencies:        []string{"cobra"},
		Risks:               []string{"Complexity"},
	}

	if resp.Architecture != "Layered architecture" {
		t.Errorf("Architecture = %v, want 'Layered architecture'", resp.Architecture)
	}

	if len(resp.Files) != 2 {
		t.Errorf("len(Files) = %v, want 2", len(resp.Files))
	}

	if resp.Files[0].Action != "create" {
		t.Errorf("Files[0].Action = %v, want 'create'", resp.Files[0].Action)
	}
}

func TestBuildRequest(t *testing.T) {
	design := &DesignResponse{
		Architecture: "Simple",
	}

	feedback := &ReviewResponse{
		Issues: []Issue{{Severity: "minor", Message: "Fix style"}},
	}

	req := &BuildRequest{
		Task:     "Implement CLI",
		Design:   design,
		WorkDir:  "/tmp/project",
		Feedback: feedback,
	}

	if req.Task != "Implement CLI" {
		t.Errorf("Task = %v, want 'Implement CLI'", req.Task)
	}

	if req.Design == nil {
		t.Error("Design should not be nil")
	}

	if req.Feedback == nil {
		t.Error("Feedback should not be nil")
	}
}

func TestBuildResponse(t *testing.T) {
	resp := &BuildResponse{
		FilesCreated:  []string{"main.go", "cli.go"},
		FilesModified: []string{"go.mod"},
		TestsRun:      true,
		TestsPassed:   true,
		TestOutput:    "PASS",
		BuildOutput:   "Build successful",
		Notes:         "All done",
	}

	if len(resp.FilesCreated) != 2 {
		t.Errorf("len(FilesCreated) = %v, want 2", len(resp.FilesCreated))
	}

	if !resp.TestsPassed {
		t.Error("TestsPassed should be true")
	}
}

func TestReviewRequest(t *testing.T) {
	design := &DesignResponse{
		Architecture: "Original design",
	}

	req := &ReviewRequest{
		Task:           "Review CLI",
		FilesChanged:   []string{"main.go", "cli.go"},
		OriginalDesign: design,
	}

	if len(req.FilesChanged) != 2 {
		t.Errorf("len(FilesChanged) = %v, want 2", len(req.FilesChanged))
	}

	if req.OriginalDesign == nil {
		t.Error("OriginalDesign should not be nil")
	}
}

func TestIssue(t *testing.T) {
	issue := Issue{
		Severity:   "critical",
		File:       "main.go",
		Line:       42,
		Message:    "Null pointer dereference",
		Suggestion: "Add nil check",
	}

	if issue.Severity != "critical" {
		t.Errorf("Severity = %v, want 'critical'", issue.Severity)
	}

	if issue.Line != 42 {
		t.Errorf("Line = %v, want 42", issue.Line)
	}
}

func TestPlannerResult(t *testing.T) {
	result := &PlannerResult{
		Success:           true,
		Summary:           "Completed successfully",
		FilesCreated:      []string{"main.go"},
		FilesModified:     []string{"go.mod"},
		RemainingConcerns: []string{"Need more tests"},
		TotalCost:         0.05,
	}

	if !result.Success {
		t.Error("Success should be true")
	}

	if result.TotalCost != 0.05 {
		t.Errorf("TotalCost = %v, want 0.05", result.TotalCost)
	}
}
