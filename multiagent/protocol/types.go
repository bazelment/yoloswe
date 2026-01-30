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
	// Task describes what to build.
	Task string `json:"task"`

	// Design is the technical design from the Designer.
	Design *DesignResponse `json:"design"`

	// WorkDir is the working directory for file operations.
	WorkDir string `json:"work_dir"`

	// Feedback contains any feedback from a previous review to address.
	Feedback *ReviewResponse `json:"feedback,omitempty"`
}

// BuildResponse is the output from the Builder agent.
type BuildResponse struct {
	// FilesCreated lists newly created files.
	FilesCreated []string `json:"files_created"`

	// FilesModified lists modified files.
	FilesModified []string `json:"files_modified"`

	// TestsRun indicates whether tests were executed.
	TestsRun bool `json:"tests_run"`

	// TestsPassed indicates whether tests passed.
	TestsPassed bool `json:"tests_passed"`

	// TestOutput contains the test command output.
	TestOutput string `json:"test_output,omitempty"`

	// BuildOutput contains the build command output.
	BuildOutput string `json:"build_output,omitempty"`

	// Notes contains any issues encountered or decisions made.
	Notes string `json:"notes,omitempty"`
}

// ReviewRequest is the input for the Reviewer agent.
type ReviewRequest struct {
	// Task describes what was supposed to be built.
	Task string `json:"task"`

	// FilesChanged lists files that were created or modified.
	FilesChanged []string `json:"files_changed"`

	// OriginalDesign is the design the Builder was following.
	OriginalDesign *DesignResponse `json:"original_design"`
}

// Issue represents a problem found during review.
type Issue struct {
	// Severity is "critical", "minor", or "nitpick".
	Severity string `json:"severity"`

	// File is the path to the file with the issue.
	File string `json:"file"`

	// Line is the line number (optional).
	Line int `json:"line,omitempty"`

	// Message describes the issue.
	Message string `json:"message"`

	// Suggestion describes how to fix it.
	Suggestion string `json:"suggestion,omitempty"`
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
	// Success indicates whether the mission was completed successfully.
	Success bool `json:"success"`

	// Summary describes what was accomplished.
	Summary string `json:"summary"`

	// FilesCreated lists all newly created files.
	FilesCreated []string `json:"files_created"`

	// FilesModified lists all modified files.
	FilesModified []string `json:"files_modified"`

	// RemainingConcerns lists any issues that weren't fully resolved.
	RemainingConcerns []string `json:"remaining_concerns,omitempty"`

	// TotalCost is the accumulated cost across all sub-agents.
	TotalCost float64 `json:"total_cost"`
}
