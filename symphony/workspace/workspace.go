package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/bazelment/yoloswe/symphony/config"
	"github.com/bazelment/yoloswe/symphony/model"
)

// CreateForIssue creates or reuses a workspace directory for the given issue identifier.
// Returns a Workspace with CreatedNow=true only if the directory was newly created.
// If newly created and cfg.HookAfterCreate is set, the after_create hook runs;
// hook failure is fatal and the partially created directory is removed.
// Spec Section 9.2.
func CreateForIssue(ctx context.Context, cfg *config.ServiceConfig, identifier string) (*model.Workspace, error) {
	workspaceKey := model.SanitizeIdentifier(identifier)

	if err := ValidateWorkspaceKey(workspaceKey); err != nil {
		return nil, fmt.Errorf("invalid workspace key for %q: %w", identifier, err)
	}

	wsPath := filepath.Join(cfg.WorkspaceRoot, workspaceKey)

	if err := ValidatePathContainment(wsPath, cfg.WorkspaceRoot); err != nil {
		return nil, fmt.Errorf("path containment violation for %q: %w", identifier, err)
	}

	// Check whether the directory already exists.
	createdNow := false
	info, err := os.Stat(wsPath)
	if err != nil {
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("stat workspace %q: %w", wsPath, err)
		}
		// Directory does not exist — create it.
		if mkErr := os.MkdirAll(wsPath, 0o755); mkErr != nil {
			return nil, fmt.Errorf("create workspace directory %q: %w", wsPath, mkErr)
		}
		createdNow = true
	} else if !info.IsDir() {
		return nil, fmt.Errorf("workspace path %q exists but is not a directory", wsPath)
	}

	// Run after_create hook if the directory was just created.
	if createdNow && cfg.HookAfterCreate != "" {
		if hookErr := RunHook(ctx, cfg.HookAfterCreate, wsPath, cfg.HookTimeoutMs); hookErr != nil {
			// Remove partially prepared directory on hook failure.
			_ = os.RemoveAll(wsPath)
			return nil, fmt.Errorf("after_create hook failed for %q: %w", identifier, hookErr)
		}
	}

	return &model.Workspace{
		Path:         wsPath,
		WorkspaceKey: workspaceKey,
		CreatedNow:   createdNow,
	}, nil
}
