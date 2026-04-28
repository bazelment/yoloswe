package reviewer

import (
	"encoding/json"
	"fmt"
	"os"
)

// ScopeHintsSchemaVersion is the wire-format version of the scope-hints
// JSON file consumed by `bramble code-review --scope-hints-file`. A version
// bump only happens on a breaking shape change; new optional fields can be
// added without a bump.
const ScopeHintsSchemaVersion = 1

// ScopeHints is the JSON contract between bramble and any caller (today: the
// /pr-polish skill's scope_gate.py) that wants to widen the review scope.
//
// The shape is small on purpose: bramble owns the prompt structure and the
// types here, callers compute the lists. See plan
// plans/issue-175-widen-review-scope.md for the design rationale.
type ScopeHints struct {
	// TestPaths lists co-located test files the reviewer should read in
	// addition to anything in the diff. When non-empty, the test-quality
	// clause is appended to the prompt.
	TestPaths []string `json:"test_paths"`

	// CrossServicePackages names the top-level packages the PR touches.
	// When it has at least two entries, the cross-service contract-sweep
	// clause is appended.
	CrossServicePackages []string `json:"cross_service_packages"`

	// SchemaVersion must equal ScopeHintsSchemaVersion. LoadScopeHints
	// rejects any other value to make incompatible upgrades fail loudly
	// instead of silently producing a degenerate prompt.
	SchemaVersion int `json:"schema_version"`
}

// LoadScopeHints reads and validates a ScopeHints JSON file.
//
// The function is strict on schema version (rejects mismatches) and on JSON
// well-formedness (rejects malformed payloads), but permissive on missing
// fields: an empty TestPaths or CrossServicePackages is a valid
// "no clause" signal, not an error.
//
// The CLI layer is expected to log-and-fall-back on errors here rather than
// abort the review — a malformed scope-hints file should never block a
// caller from getting today's narrow review.
func LoadScopeHints(path string) (*ScopeHints, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read scope-hints file %s: %w", path, err)
	}
	var h ScopeHints
	if err := json.Unmarshal(data, &h); err != nil {
		return nil, fmt.Errorf("parse scope-hints file %s: %w", path, err)
	}
	if h.SchemaVersion != ScopeHintsSchemaVersion {
		return nil, fmt.Errorf(
			"scope-hints file %s: schema_version=%d, want %d",
			path, h.SchemaVersion, ScopeHintsSchemaVersion,
		)
	}
	return &h, nil
}

// ToPromptOptions converts hints into PromptOptions, preserving the caller's
// SkipTestExecution setting which is orthogonal to scope. Pass through nil-
// safe: a nil receiver returns a zero PromptOptions.
func (h *ScopeHints) ToPromptOptions(skipTestExecution bool) PromptOptions {
	opts := PromptOptions{SkipTestExecution: skipTestExecution}
	if h == nil {
		return opts
	}
	opts.TestScopeHints = h.TestPaths
	opts.CrossServicePackages = h.CrossServicePackages
	return opts
}
