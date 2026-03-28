package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/bazelment/yoloswe/symphony/model"
)

// ServiceConfig is an immutable snapshot of typed runtime config derived from
// WorkflowDefinition.config. Workers capture a snapshot at dispatch time.
// Spec Section 4.1.3, 6.4.
type ServiceConfig struct {
	Workflow               *model.WorkflowDefinition
	MaxConcurrentByState   map[string]int
	ServerPort             *int
	HookAfterCreate        string
	HookAfterRun           string
	CodexCommand           string
	TrackerKind            string
	WorkspaceRoot          string
	CodexApprovalPolicy    string
	HookBeforeRun          string
	TrackerEndpoint        string
	HookBeforeRemove       string
	TrackerProjectSlug     string
	TrackerAPIKey          string
	CodexTurnSandboxPolicy string
	CodexThreadSandbox     string
	ActiveStates           []string
	TerminalStates         []string
	PollIntervalMs         int
	MaxRetryBackoffMs      int
	MaxTurns               int
	CodexTurnTimeoutMs     int
	CodexReadTimeoutMs     int
	CodexStallTimeoutMs    int
	MaxConcurrentAgents    int
	HookTimeoutMs          int
}

// NewServiceConfig creates an immutable ServiceConfig from a WorkflowDefinition.
// Applies defaults, $VAR env resolution, and ~ path expansion.
func NewServiceConfig(wf *model.WorkflowDefinition) *ServiceConfig {
	c := wf.Config

	cfg := &ServiceConfig{
		Workflow: wf,

		// Tracker defaults (Spec Section 5.3.1)
		TrackerKind:        getString(c, "tracker", "kind", ""),
		TrackerEndpoint:    resolveEnv(getString(c, "tracker", "endpoint", "")),
		TrackerAPIKey:      resolveEnv(getString(c, "tracker", "api_key", "")),
		TrackerProjectSlug: getString(c, "tracker", "project_slug", ""),
		ActiveStates:       getStringList(c, "tracker", "active_states", []string{"Todo", "In Progress"}),
		TerminalStates:     getStringList(c, "tracker", "terminal_states", []string{"Closed", "Cancelled", "Canceled", "Duplicate", "Done"}),

		// Polling defaults (Spec Section 5.3.2)
		PollIntervalMs: getInt(c, "polling", "interval_ms", 30000),

		// Workspace defaults (Spec Section 5.3.3)
		WorkspaceRoot: expandPath(resolveEnv(getString(c, "workspace", "root", ""))),

		// Hook defaults (Spec Section 5.3.4)
		HookAfterCreate:  getString(c, "hooks", "after_create", ""),
		HookBeforeRun:    getString(c, "hooks", "before_run", ""),
		HookAfterRun:     getString(c, "hooks", "after_run", ""),
		HookBeforeRemove: getString(c, "hooks", "before_remove", ""),
		HookTimeoutMs:    getInt(c, "hooks", "timeout_ms", 60000),

		// Agent defaults (Spec Section 5.3.5)
		MaxConcurrentAgents:  getInt(c, "agent", "max_concurrent_agents", 10),
		MaxTurns:             getInt(c, "agent", "max_turns", 20),
		MaxRetryBackoffMs:    getInt(c, "agent", "max_retry_backoff_ms", 300000),
		MaxConcurrentByState: getIntMap(c, "agent", "max_concurrent_agents_by_state"),

		// Codex defaults (Spec Section 5.3.6)
		CodexCommand:           getString(c, "codex", "command", "codex app-server"),
		CodexApprovalPolicy:    getString(c, "codex", "approval_policy", ""),
		CodexThreadSandbox:     getString(c, "codex", "thread_sandbox", ""),
		CodexTurnSandboxPolicy: getString(c, "codex", "turn_sandbox_policy", ""),
		CodexTurnTimeoutMs:     getInt(c, "codex", "turn_timeout_ms", 3600000),
		CodexReadTimeoutMs:     getInt(c, "codex", "read_timeout_ms", 5000),
		CodexStallTimeoutMs:    getInt(c, "codex", "stall_timeout_ms", 300000),
	}

	// Non-positive hook timeout falls back to default (Spec Section 5.3.4).
	if cfg.HookTimeoutMs <= 0 {
		cfg.HookTimeoutMs = 60000
	}

	// Default tracker endpoint for linear (Spec Section 5.3.1).
	if cfg.TrackerKind == "linear" && cfg.TrackerEndpoint == "" {
		cfg.TrackerEndpoint = "https://api.linear.app/graphql"
	}

	// Default workspace root (Spec Section 5.3.3).
	if cfg.WorkspaceRoot == "" {
		cfg.WorkspaceRoot = filepath.Join(os.TempDir(), "symphony_workspaces")
	}

	// Server port (extension).
	if sp := getOptionalInt(c, "server", "port"); sp != nil {
		cfg.ServerPort = sp
	}

	return cfg
}

// resolveEnv replaces $VAR_NAME with the environment variable value.
// If the value doesn't start with $, it's returned as-is.
// If $VAR resolves to empty string, return empty (treated as missing by callers).
func resolveEnv(val string) string {
	if !strings.HasPrefix(val, "$") {
		return val
	}
	envName := val[1:]
	return os.Getenv(envName)
}

// expandPath handles ~ home expansion and path normalization.
func expandPath(path string) string {
	if path == "" {
		return path
	}
	if strings.HasPrefix(path, "~/") || path == "~" {
		home, err := os.UserHomeDir()
		if err == nil {
			if path == "~" {
				path = home
			} else {
				path = filepath.Join(home, path[2:]) // skip "~/"
			}
		}
	}
	return path
}

// getString navigates a nested config map: config[section][key].
func getString(config map[string]any, section, key, defaultVal string) string {
	sec, ok := config[section]
	if !ok {
		return defaultVal
	}
	secMap, ok := sec.(map[string]any)
	if !ok {
		return defaultVal
	}
	val, ok := secMap[key]
	if !ok {
		return defaultVal
	}
	switch v := val.(type) {
	case string:
		return v
	default:
		return fmt.Sprintf("%v", v)
	}
}

// getInt navigates a nested config map and returns an integer.
// Handles both integer and string-encoded integers.
func getInt(config map[string]any, section, key string, defaultVal int) int {
	sec, ok := config[section]
	if !ok {
		return defaultVal
	}
	secMap, ok := sec.(map[string]any)
	if !ok {
		return defaultVal
	}
	val, ok := secMap[key]
	if !ok {
		return defaultVal
	}
	switch v := val.(type) {
	case int:
		return v
	case float64:
		return int(v)
	case string:
		i, err := strconv.Atoi(v)
		if err != nil {
			return defaultVal
		}
		return i
	default:
		return defaultVal
	}
}

// getOptionalInt returns nil if the key is absent or not a valid integer.
func getOptionalInt(config map[string]any, section, key string) *int {
	sec, ok := config[section]
	if !ok {
		return nil
	}
	secMap, ok := sec.(map[string]any)
	if !ok {
		return nil
	}
	val, ok := secMap[key]
	if !ok {
		return nil
	}
	switch v := val.(type) {
	case int:
		return &v
	case float64:
		i := int(v)
		return &i
	case string:
		i, err := strconv.Atoi(v)
		if err != nil {
			return nil
		}
		return &i
	default:
		return nil
	}
}

// getStringList navigates a nested config map and returns a string list.
func getStringList(config map[string]any, section, key string, defaultVal []string) []string {
	sec, ok := config[section]
	if !ok {
		return defaultVal
	}
	secMap, ok := sec.(map[string]any)
	if !ok {
		return defaultVal
	}
	val, ok := secMap[key]
	if !ok {
		return defaultVal
	}
	list, ok := val.([]any)
	if !ok {
		return defaultVal
	}
	result := make([]string, 0, len(list))
	for _, item := range list {
		if s, ok := item.(string); ok {
			result = append(result, s)
		}
	}
	if len(result) == 0 {
		return defaultVal
	}
	return result
}

// getIntMap navigates a nested config map and returns a map[string]int.
// Invalid entries (non-positive, non-numeric) are ignored.
// State keys are normalized to lowercase. Spec Section 5.3.5.
func getIntMap(config map[string]any, section, key string) map[string]int {
	sec, ok := config[section]
	if !ok {
		return nil
	}
	secMap, ok := sec.(map[string]any)
	if !ok {
		return nil
	}
	val, ok := secMap[key]
	if !ok {
		return nil
	}
	raw, ok := val.(map[string]any)
	if !ok {
		return nil
	}
	result := make(map[string]int)
	for k, v := range raw {
		var intVal int
		switch vv := v.(type) {
		case int:
			intVal = vv
		case float64:
			intVal = int(vv)
		case string:
			i, err := strconv.Atoi(vv)
			if err != nil {
				continue
			}
			intVal = i
		default:
			continue
		}
		if intVal <= 0 {
			continue
		}
		result[strings.ToLower(k)] = intVal
	}
	if len(result) == 0 {
		return nil
	}
	return result
}
