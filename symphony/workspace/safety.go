// Package workspace manages per-issue workspace directories and lifecycle hooks.
// Spec Sections 9.1–9.5.
package workspace

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"
)

// validKeyPattern matches workspace keys containing only allowed characters.
var validKeyPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

// ValidateWorkspaceKey ensures a workspace key contains only [A-Za-z0-9._-].
// Returns an error if the key is empty or contains disallowed characters.
// Spec Section 9.5, Invariant 3.
func ValidateWorkspaceKey(key string) error {
	if key == "" {
		return fmt.Errorf("workspace key is empty")
	}
	if !validKeyPattern.MatchString(key) {
		return fmt.Errorf("workspace key %q contains invalid characters (allowed: A-Za-z0-9._-)", key)
	}
	// Reject ".." which would escape containment.
	if key == ".." {
		return fmt.Errorf("workspace key %q is a reserved directory name", key)
	}
	return nil
}

// ValidatePathContainment ensures workspacePath is strictly inside workspaceRoot.
// Both paths are normalized to absolute before comparison.
// Spec Section 9.5, Invariant 2.
func ValidatePathContainment(workspacePath, workspaceRoot string) error {
	absRoot, err := filepath.Abs(workspaceRoot)
	if err != nil {
		return fmt.Errorf("cannot resolve workspace root: %w", err)
	}
	absPath, err := filepath.Abs(workspacePath)
	if err != nil {
		return fmt.Errorf("cannot resolve workspace path: %w", err)
	}

	// Clean both paths to remove trailing slashes and redundant separators.
	absRoot = filepath.Clean(absRoot)
	absPath = filepath.Clean(absPath)

	// The workspace path must not equal the root — it must be a child.
	if absPath == absRoot {
		return fmt.Errorf("workspace path %q must be inside workspace root %q, not equal to it", absPath, absRoot)
	}

	// Require that the workspace path starts with root + separator.
	prefix := absRoot + string(filepath.Separator)
	if !strings.HasPrefix(absPath, prefix) {
		return fmt.Errorf("workspace path %q is not inside workspace root %q", absPath, absRoot)
	}

	return nil
}
