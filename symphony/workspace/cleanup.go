package workspace

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/bazelment/yoloswe/symphony/config"
	"github.com/bazelment/yoloswe/symphony/model"
)

// CleanupWorkspace removes the workspace directory for the given issue identifier.
// If the directory exists and cfg.HookBeforeRemove is set, the hook runs best-effort
// before removal. Spec Sections 8.6, 9.4.
func CleanupWorkspace(cfg *config.ServiceConfig, identifier string, logger *slog.Logger) error {
	workspaceKey := model.SanitizeIdentifier(identifier)

	if err := ValidateWorkspaceKey(workspaceKey); err != nil {
		return fmt.Errorf("invalid workspace key for %q: %w", identifier, err)
	}

	wsPath := filepath.Join(cfg.WorkspaceRoot, workspaceKey)

	if err := ValidatePathContainment(wsPath, cfg.WorkspaceRoot); err != nil {
		return fmt.Errorf("path containment violation for %q: %w", identifier, err)
	}

	// Check if the directory exists.
	info, err := os.Stat(wsPath)
	if err != nil {
		if os.IsNotExist(err) {
			logger.Debug("workspace directory does not exist, nothing to clean",
				"identifier", identifier,
				"path", wsPath,
			)
			return nil
		}
		return fmt.Errorf("stat workspace %q: %w", wsPath, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("workspace path %q exists but is not a directory", wsPath)
	}

	// Run before_remove hook best-effort.
	if cfg.HookBeforeRemove != "" {
		// Use a fresh background context: before_remove is best-effort cleanup that
		// should run regardless of whether the caller's context has been cancelled.
		RunHookBestEffort(context.Background(), cfg.HookBeforeRemove, wsPath, cfg.HookTimeoutMs, logger)
	}

	// Remove the workspace directory.
	if err := os.RemoveAll(wsPath); err != nil {
		return fmt.Errorf("remove workspace %q: %w", wsPath, err)
	}

	logger.Info("cleaned up workspace",
		"identifier", identifier,
		"path", wsPath,
	)
	return nil
}
