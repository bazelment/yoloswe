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
			if err := validateNativeReadPath(cfg.allowedRoots(), "", *req.BlockedPath); err != nil {
				return denyClaudePermission(err.Error()), nil
			}
		}
		tool := strings.TrimSpace(req.ToolName)
		roots := cfg.allowedRoots()
		if len(roots) == 0 {
			return denyClaudePermission("native reads are disabled without configured run/repository roots"), nil
		}
		switch tool {
		case "Read":
			path := stringInput(req.Input, "file_path", "path")
			if path == "" {
				return denyClaudePermission("Read requires an explicit confined path"), nil
			}
			if err := validateNativeReadPath(roots, "", path); err != nil {
				return denyClaudePermission(err.Error()), nil
			}
		case "Grep", "Glob":
			path := stringInput(req.Input, "path", "file_path")
			if path == "" {
				return denyClaudePermission(tool + " requires an explicit confined path"), nil
			}
			if err := validateNativeReadPath(roots, "", path); err != nil {
				return denyClaudePermission(err.Error()), nil
			}
			if tool == "Grep" {
				if pattern := stringInput(req.Input, "glob"); strings.HasPrefix(pattern, "/") || strings.HasPrefix(pattern, "~") || strings.Contains(pattern, "..") {
					if err := validateNativeReadPath(roots, "", pattern); err != nil {
						return denyClaudePermission(err.Error()), nil
					}
				}
			}
			if tool == "Glob" {
				if pattern := stringInput(req.Input, "pattern"); pattern != "" {
					if err := validateNativeReadPath(roots, "", pattern); err != nil {
						return denyClaudePermission(err.Error()), nil
					}
				}
			}
		case "Bash":
			command := stringInput(req.Input, "command")
			if hasShellControlSyntax(command) {
				return denyClaudePermission("shell operators and substitutions are not permitted; use one read-only command"), nil
			}
			if err := validateNativeBashCommand(roots, command); err != nil {
				return denyClaudePermission(err.Error()), nil
			}
		default:
			return denyClaudePermission(fmt.Sprintf("native tool %q is not permitted; use Orca typed tools for writes", tool)), nil
		}
		return &claude.PermissionResponse{Behavior: claude.PermissionAllow}, nil
	})
}

// parseNativeShellWords handles the quoting Claude emits for a single Bash
// command. It deliberately does not expand variables, command substitutions,
// or operators; those are rejected before this parser is called.
func parseNativeShellWords(input string) ([]string, error) {
	var words []string
	var word strings.Builder
	inWord := false
	quote := byte(0)
	for i := 0; i < len(input); i++ {
		ch := input[i]
		if quote != 0 {
			if ch == quote {
				quote = 0
				continue
			}
			if quote == '"' && ch == '\\' {
				if i+1 >= len(input) {
					return nil, fmt.Errorf("unterminated shell escape")
				}
				i++
				word.WriteByte(input[i])
				inWord = true
				continue
			}
			word.WriteByte(ch)
			inWord = true
			continue
		}
		switch ch {
		case '\'', '"':
			quote, inWord = ch, true
		case '\\':
			if i+1 >= len(input) {
				return nil, fmt.Errorf("unterminated shell escape")
			}
			i++
			word.WriteByte(input[i])
			inWord = true
		case ' ', '\t', '\n', '\r':
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		default:
			word.WriteByte(ch)
			inWord = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated shell quote")
	}
	if inWord {
		words = append(words, word.String())
	}
	return words, nil
}

func validateNativeBashCommand(roots []string, command string) error {
	parts, err := parseNativeShellWords(command)
	if err != nil {
		return fmt.Errorf("bash command: %w", err)
	}
	if len(parts) == 0 {
		return fmt.Errorf("empty Bash command")
	}
	prog := filepath.Base(parts[0])
	if prog != parts[0] {
		return fmt.Errorf("bash command %q must use an allowlisted program name", parts[0])
	}
	switch prog {
	case "cat", "ls", "rg", "grep", "head", "tail", "stat", "wc", "sha256sum", "shasum":
	default:
		return fmt.Errorf("bash command %q is not a permitted read-only command", parts[0])
	}
	if prog == "env" || prog == "printenv" || prog == "set" {
		return fmt.Errorf("bash command %q is forbidden", prog)
	}
	var paths []string
	seenPattern := false
	for _, arg := range parts[1:] {
		if strings.HasPrefix(arg, "-") && !strings.HasPrefix(arg, "--") {
			continue
		}
		if (prog == "rg" || prog == "grep") && !seenPattern {
			seenPattern = true
			continue
		}
		paths = append(paths, arg)
	}
	if len(paths) == 0 && prog != "ls" && prog != "rg" && prog != "grep" {
		return fmt.Errorf("bash command %q requires a path", prog)
	}
	for _, path := range paths {
		if err := validateNativeReadPath(roots, "", path); err != nil {
			return err
		}
	}
	return nil
}

func validateNativeReadPath(roots []string, cwd, raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fmt.Errorf("native read path is empty")
	}
	if strings.Contains(raw, "$HOME") || strings.HasPrefix(raw, "~") {
		return fmt.Errorf("native read path %q is outside allowed roots", raw)
	}
	if isSensitiveInvestigationPath(raw) {
		return fmt.Errorf("native read path %q is restricted", raw)
	}
	base := raw
	if cwd != "" && !filepath.IsAbs(base) {
		base = filepath.Join(cwd, base)
	}
	if i := strings.IndexAny(base, "*?["); i >= 0 {
		prefix := base[:i]
		prefix = strings.TrimRight(prefix, string(filepath.Separator))
		if prefix == "" {
			prefix = string(filepath.Separator)
		}
		if _, err := resolveSafePath(roots, prefix); err != nil {
			return err
		}
		matches, _ := filepath.Glob(base)
		for _, match := range matches {
			if isSensitiveInvestigationPath(match) {
				return fmt.Errorf("native read path %q is restricted", raw)
			}
			if _, err := resolveSafePath(roots, match); err != nil {
				return err
			}
		}
		return nil
	}
	if _, err := resolveSafePath(roots, base); err != nil {
		return err
	}
	return nil
}

// NativeReadRoots reports the canonical roots used by both CLI providers.
// It is intentionally read-only and exists for diagnostics/tests.
func NativeReadRoots(cfg InvestigateConfig) []string { return cfg.allowedRoots() }

// ValidateNativeReadOnlyCommand applies the same root and sensitive-path
// checks used by Claude's native Bash permission handler. It is also useful to
// audit a provider launch configuration without starting a model process.
func ValidateNativeReadOnlyCommand(cfg InvestigateConfig, command string) error {
	if hasShellControlSyntax(command) {
		return fmt.Errorf("shell operators and substitutions are not permitted")
	}
	return validateNativeBashCommand(cfg.allowedRoots(), command)
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
