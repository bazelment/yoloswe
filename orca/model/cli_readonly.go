package model

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
)

// readOnlyClaudePermissionHandler is the last safety fence for Claude's
// native tools. Tool visibility is useful for parity, but permission checks
// must still reject an attempted write or an inspection outside the adopted
// run/repository roots.
func readOnlyClaudePermissionHandler(cfg InvestigateConfig) claude.PermissionHandler {
	return claude.PermissionHandlerFunc(func(_ context.Context, req *claude.PermissionRequest) (*claude.PermissionResponse, error) {
		if req == nil {
			return denyClaudePermission("missing permission request"), nil
		}
		if req.BlockedPath != nil {
			if _, err := resolveSafePath(cfg.allowedRoots(), *req.BlockedPath); err != nil {
				return denyClaudePermission(err.Error()), nil
			}
		}
		tool := strings.TrimSpace(req.ToolName)
		switch tool {
		case "Read":
			if path := stringInput(req.Input, "file_path", "path"); path != "" {
				if _, err := resolveSafePath(cfg.allowedRoots(), path); err != nil {
					return denyClaudePermission(err.Error()), nil
				}
			}
		case "Grep", "Glob":
			if path := stringInput(req.Input, "path", "file_path"); path != "" {
				if _, err := resolveSafePath(cfg.allowedRoots(), filepath.Clean(path)); err != nil {
					return denyClaudePermission(err.Error()), nil
				}
			}
			if tool == "Glob" {
				if pattern := stringInput(req.Input, "pattern"); pattern != "" {
					if _, err := resolveSafePath(cfg.allowedRoots(), filepath.Clean(pattern)); err != nil {
						return denyClaudePermission(err.Error()), nil
					}
				}
			}
		case "Bash":
			command := stringInput(req.Input, "command")
			if hasShellControlSyntax(command) {
				return denyClaudePermission("shell operators and substitutions are not permitted; use one read-only command"), nil
			}
			parts := strings.Fields(command)
			if len(parts) == 0 {
				return denyClaudePermission("empty Bash command"), nil
			}
			if err := ValidateReadOnlyCommand(parts[0], parts[1:]); err != nil {
				return denyClaudePermission(err.Error()), nil
			}
		default:
			return denyClaudePermission(fmt.Sprintf("native tool %q is not permitted; use Orca typed tools for writes", tool)), nil
		}
		return &claude.PermissionResponse{Behavior: claude.PermissionAllow}, nil
	})
}

func hasShellControlSyntax(command string) bool {
	return strings.ContainsAny(command, ";|&<>`\\\n\r") || strings.Contains(command, "$")
}

func stringInput(input map[string]interface{}, names ...string) string {
	for _, name := range names {
		if value, ok := input[name].(string); ok {
			return strings.TrimSpace(value)
		}
	}
	return ""
}

func denyClaudePermission(reason string) *claude.PermissionResponse {
	return &claude.PermissionResponse{Behavior: claude.PermissionDeny, Message: reason}
}
