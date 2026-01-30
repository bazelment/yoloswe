// Package protocol defines the request/response types for inter-agent communication.
package protocol

// DesignRequest is the input for the Designer agent.
type DesignRequest struct {
	// Task describes what needs to be designed.
	Task string `json:"task"`

	// Context provides relevant codebase information and existing patterns.
	Context string `json:"context"`

	// Constraints are any limitations or requirements.
	Constraints []string `json:"constraints,omitempty"`
}

// FileSpec describes a file to be created or modified.
type FileSpec struct {
	// Path is the file path relative to the working directory.
	Path string `json:"path"`

	// Purpose describes what this file does.
	Purpose string `json:"purpose"`

	// Action is "create" or "modify".
	Action string `json:"action"`
}

// DesignResponse is the output from the Designer agent.
type DesignResponse struct {
	// Architecture is a high-level description of the approach.
	Architecture string `json:"architecture"`

	// Files lists the files to be created or modified.
	Files []FileSpec `json:"files"`

	// Interfaces contains type definitions and function signatures.
	Interfaces string `json:"interfaces,omitempty"`

	// ImplementationNotes provides step-by-step guidance for the Builder.
	ImplementationNotes []string `json:"implementation_notes"`

	// Dependencies lists any new dependencies needed.
	Dependencies []string `json:"dependencies,omitempty"`

	// Risks lists potential issues or concerns.
	Risks []string `json:"risks,omitempty"`
}

// BuildRequest is the input for the Builder agent.
type BuildRequest struct {
	Design   *DesignResponse `json:"design"`
	Feedback *ReviewResponse `json:"feedback,omitempty"`
	Task     string          `json:"task"`
	WorkDir  string          `json:"work_dir"`
}

// BuildResponse is the output from the Builder agent.
type BuildResponse struct {
	TestOutput    string   `json:"test_output,omitempty"`
	BuildOutput   string   `json:"build_output,omitempty"`
	Notes         string   `json:"notes,omitempty"`
	FilesCreated  []string `json:"files_created"`
	FilesModified []string `json:"files_modified"`
	TestsRun      bool     `json:"tests_run"`
	TestsPassed   bool     `json:"tests_passed"`
}

// ReviewRequest is the input for the Reviewer agent.
type ReviewRequest struct {
	OriginalDesign *DesignResponse `json:"original_design"`
	Task           string          `json:"task"`
	FilesChanged   []string        `json:"files_changed"`
}

// Issue represents a problem found during review.
type Issue struct {
	Severity   string `json:"severity"`
	File       string `json:"file"`
	Message    string `json:"message"`
	Suggestion string `json:"suggestion,omitempty"`
	Line       int    `json:"line,omitempty"`
}

// ReviewResponse is the output from the Reviewer agent.
type ReviewResponse struct {
	// Summary is a brief overall assessment.
	Summary string `json:"summary"`

	// Issues lists problems found.
	Issues []Issue `json:"issues"`

	// Positives lists things done well.
	Positives []string `json:"positives,omitempty"`

	// Suggestions are optional improvements, not blocking.
	Suggestions []string `json:"suggestions,omitempty"`
}

// HasCriticalIssues returns true if there are any critical severity issues.
func (r *ReviewResponse) HasCriticalIssues() bool {
	for _, issue := range r.Issues {
		if issue.Severity == "critical" {
			return true
		}
	}
	return false
}

// PlannerResult is the output from the Planner agent back to Orchestrator.
type PlannerResult struct {
	Summary           string   `json:"summary"`
	FilesCreated      []string `json:"files_created"`
	FilesModified     []string `json:"files_modified"`
	RemainingConcerns []string `json:"remaining_concerns,omitempty"`
	TotalCost         float64  `json:"total_cost"`
	Success           bool     `json:"success"`
}
