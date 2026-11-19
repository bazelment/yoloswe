package model

import (
	"bufio"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/llmendpoint"
)

// AgentBackend selects how Dispatcher and Planner turns are executed.
type AgentBackend string

const (
	// AgentBackendLoop runs the hand-rolled ModelClient.RunToolLoop per turn.
	AgentBackendLoop AgentBackend = "loop"
	// AgentBackendCLI runs a long-lived claude or codex CLI agent session per held run.
	AgentBackendCLI AgentBackend = "cli"

	// DefaultMaxSessionTokens is the cumulative token threshold at which a long-lived
	// CLI agent session rotates to a fresh session seeded from the Store snapshot.
	DefaultMaxSessionTokens = 120000
	// DefaultMaxSessionTurns is the turn count threshold at which a long-lived
	// CLI agent session rotates to a fresh session.
	DefaultMaxSessionTurns = 32
)

// Valid reports whether b is a supported AgentBackend value.
func (b AgentBackend) Valid() bool {
	return b == AgentBackendLoop || b == AgentBackendCLI
}

// ParseAgentBackend normalizes and validates an AgentBackend string.
func ParseAgentBackend(raw string) (AgentBackend, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", string(AgentBackendLoop):
		return AgentBackendLoop, nil
	case string(AgentBackendCLI):
		return AgentBackendCLI, nil
	default:
		return "", fmt.Errorf("orca/model: unsupported agent backend %q (want %q or %q)", raw, AgentBackendLoop, AgentBackendCLI)
	}
}

// AgentSessionConfig configures a long-lived CLI agent session for a run role.
// Claude roles launch like the host orchestrator: built-in tools on and
// permissions bypassed.
type AgentSessionConfig struct {
	Env              map[string]string
	Provider         string
	Model            string
	Effort           string
	SystemPrompt     string
	WorkDir          string
	ResumeID         string
	LLMGatewayURL    string
	ClaudeBin        string
	CodexBin         string
	MaxSessionTokens int
	MaxSessionTurns  int
	// AllowBuiltins marks the adopted dispatcher, which acts through the orca
	// shell toolbox and gets no typed MCP tools.
	AllowBuiltins bool
}

// DeterministicSessionWorkDir returns a stable, isolated working directory path
// for a (runID, role) CLI agent session so `claude --resume <id>` resolves the
// same project conversation directory across process restarts and re-adoptions.
func DeterministicSessionWorkDir(baseDir, runID, role string) string {
	runSeg := sanitizePathSegment(cmp.Or(strings.TrimSpace(runID), "default"))
	roleSeg := sanitizePathSegment(cmp.Or(strings.TrimSpace(role), "agent"))
	if trimmedBase := strings.TrimSpace(baseDir); trimmedBase != "" {
		if abs, err := filepath.Abs(trimmedBase); err == nil {
			trimmedBase = abs
		}
		return filepath.Join(filepath.Clean(trimmedBase), ".orca-sessions", runSeg, roleSeg)
	}
	if cacheDir, err := os.UserCacheDir(); err == nil && strings.TrimSpace(cacheDir) != "" {
		return filepath.Join(filepath.Clean(cacheDir), "orca", "agent-sessions", runSeg, roleSeg)
	}
	if homeDir, err := os.UserHomeDir(); err == nil && strings.TrimSpace(homeDir) != "" {
		return filepath.Join(filepath.Clean(homeDir), ".orca", "agent-sessions", runSeg, roleSeg)
	}
	return filepath.Join(os.TempDir(), fmt.Sprintf("orca-agent-sessions-%d", os.Getuid()), runSeg, roleSeg)
}

func sanitizePathSegment(raw string) string {
	var b strings.Builder
	for _, r := range raw {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	out := strings.Trim(b.String(), "_")
	if out == "" {
		return "default"
	}
	return out
}

func (c AgentSessionConfig) withDefaults() AgentSessionConfig {
	switch strings.ToLower(strings.TrimSpace(c.Provider)) {
	case "", "claude", "claude-cli", "anthropic":
		c.Provider = "claude"
	case "codex", "codex-cli", "openai":
		c.Provider = "codex"
	default:
		c.Provider = strings.ToLower(strings.TrimSpace(c.Provider))
	}
	if c.MaxSessionTokens <= 0 {
		c.MaxSessionTokens = DefaultMaxSessionTokens
	}
	if c.MaxSessionTurns <= 0 {
		c.MaxSessionTurns = DefaultMaxSessionTurns
	}
	return c
}

func cliModelFlag(provider, modelName string) string {
	m := strings.TrimSpace(modelName)
	if provider == "claude" {
		switch strings.ToLower(m) {
		case "claude-sonnet-5-5":
			return "sonnet"
		case "claude-opus-5-5":
			return "opus"
		case "claude-haiku-5-5":
			return "haiku"
		}
	}
	return m
}

// AgentTurnRequest holds the parameters for a single user turn on an AgentSession.
type AgentTurnRequest struct {
	Registry     *ToolRegistry
	Observer     ToolCallObserver
	Prompt       string
	Model        string
	SystemPrompt string
	MaxToolCalls int
}

// AgentTurnResult holds the result of a single user turn on an AgentSession.
type AgentTurnResult struct {
	Text      string
	SessionID string
	Usage     Usage
	// ContextTokens is the session's context size after the turn; zero when
	// the CLI did not report it. Usage sums every API call in the turn, so it
	// is not a context size.
	ContextTokens int
	TurnCount     int
	Rotated       bool
}

// AgentSession manages a long-lived Claude or Codex CLI session for one held run
// and role (Dispatcher, Planner, or Concierge), exposing Orca's ToolRegistry as MCP tools.
type AgentSession struct {
	bridge       *MCPToolBridge
	claudeSess   *claude.Session
	codexClient  *codex.Client
	codexThread  *codex.Thread
	mcpServer    *http.Server
	mcpURL       string
	tempWorkDir  string
	sessionID    string
	currentModel string
	toolNames    []string
	cfg          AgentSessionConfig
	turnCount    int
	totalTokens  int
	mu           sync.Mutex
	started      bool
}

// NewAgentSession creates a new long-lived CLI AgentSession.
func NewAgentSession(cfg AgentSessionConfig) *AgentSession {
	cfg = cfg.withDefaults()
	return &AgentSession{
		cfg:       cfg,
		bridge:    NewMCPToolBridge(nil),
		sessionID: strings.TrimSpace(cfg.ResumeID),
	}
}

// SessionID returns the active or persisted CLI session/thread ID.
func (s *AgentSession) SessionID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionID
}

// WorkDir returns the configured or temporary working directory for this session.
func (s *AgentSession) WorkDir() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if wd := strings.TrimSpace(s.cfg.WorkDir); wd != "" {
		return wd
	}
	return s.tempWorkDir
}

// SetResumeID sets the session ID to resume when the session has not yet started
// or after a simulated restart.
func (s *AgentSession) SetResumeID(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	trimmed := strings.TrimSpace(id)
	if trimmed == "" {
		return
	}
	if !s.started {
		s.sessionID = trimmed
	}
}

// TurnCount returns the number of turns executed in the current session generation.
func (s *AgentSession) TurnCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.turnCount
}

// Turn delivers req.Prompt as a user turn to the long-lived CLI agent session,
// executing any MCP tool calls against req.Registry.
func (s *AgentSession) Turn(ctx context.Context, req AgentTurnRequest) (*AgentTurnResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(req.Prompt) == "" {
		return nil, errors.New("orca/model: agent session turn prompt is required")
	}

	sysPromptChanged := false
	if sys := strings.TrimSpace(req.SystemPrompt); sys != "" {
		if strings.TrimSpace(s.cfg.SystemPrompt) != "" && strings.TrimSpace(s.cfg.SystemPrompt) != sys {
			sysPromptChanged = true
		}
		s.cfg.SystemPrompt = sys
	}
	s.bridge.SetRegistry(req.Registry)
	s.bridge.SetMaxCalls(req.MaxToolCalls)
	s.bridge.SetObserver(req.Observer)
	defer func() {
		s.bridge.SetObserver(nil)
		s.bridge.SetMaxCalls(0)
		s.bridge.SetRegistry(nil)
	}()

	newTools := toolListNames(req.Registry)
	var rotated bool

	// Compact/rotate when context or turn count exceeds the session budget.
	if s.started && (s.totalTokens >= s.cfg.MaxSessionTokens || s.turnCount >= s.cfg.MaxSessionTurns) {
		_ = s.stopLocked()
		s.sessionID = ""
		s.turnCount = 0
		s.totalTokens = 0
		rotated = true
	}

	// If the tool set or system prompt changed across turns, restart the CLI
	// process with --resume so it reloads tools/list and system prompt while
	// keeping conversation context.
	if s.started && (!slices.Equal(s.toolNames, newTools) || sysPromptChanged) {
		prevID := s.sessionID
		_ = s.stopLocked()
		s.sessionID = prevID
	}

	resumingFromPrior := !s.started && s.sessionID != ""
	priorResumeID := s.sessionID
	effectiveModel := cmp.Or(strings.TrimSpace(req.Model), s.cfg.Model)
	res, err := s.runTurnLocked(ctx, req.Prompt, effectiveModel, newTools)
	if err != nil && resumingFromPrior && s.bridge.CallCount() == 0 && ctx.Err() == nil {
		// If resuming a caller-supplied prior session ID failed before any tool
		// call executed, fall back to a fresh session so a stale resume ID never
		// wedges the run.
		slog.Warn("agent session resume failed; rotating to fresh session",
			"session_id", priorResumeID,
			"error", err,
		)
		_ = s.stopLocked()
		s.sessionID = ""
		rotated = true
		res, err = s.runTurnLocked(ctx, req.Prompt, effectiveModel, newTools)
	}
	if err != nil {
		prevID := s.sessionID
		_ = s.stopLocked()
		if !resumingFromPrior {
			s.sessionID = prevID
		}
		return nil, err
	}

	res.Rotated = rotated
	s.turnCount++
	s.totalTokens = res.ContextTokens
	return res, nil
}

func toolListNames(reg *ToolRegistry) []string {
	if reg == nil {
		return nil
	}
	defs := reg.Definitions()
	out := make([]string, 0, len(defs))
	for _, d := range defs {
		out = append(out, encodeToolName(d.Name))
	}
	slices.Sort(out)
	return out
}

func (s *AgentSession) runTurnLocked(
	ctx context.Context,
	prompt string,
	effectiveModel string,
	newTools []string,
) (*AgentTurnResult, error) {
	switch s.cfg.Provider {
	case "", "claude":
		return s.runClaudeTurnLocked(ctx, prompt, effectiveModel, newTools)
	case "codex":
		return s.runCodexTurnLocked(ctx, prompt, effectiveModel, newTools)
	default:
		return nil, fmt.Errorf("orca/model: unsupported agent session provider %q", s.cfg.Provider)
	}
}

func (s *AgentSession) resolvedGatewayURL() string {
	return cmp.Or(strings.TrimSpace(s.cfg.LLMGatewayURL), strings.TrimSpace(os.Getenv("LLM_GATEWAY_URL")))
}

func (s *AgentSession) runClaudeTurnLocked(
	ctx context.Context,
	prompt string,
	effectiveModel string,
	newTools []string,
) (*AgentTurnResult, error) {
	if !s.started || s.claudeSess == nil {
		wd, err := s.ensureWorkDirLocked()
		if err != nil {
			return nil, err
		}
		bin := cmp.Or(strings.TrimSpace(s.cfg.ClaudeBin), strings.TrimSpace(os.Getenv("ORCA_CLAUDE_CLI")), strings.TrimSpace(os.Getenv("CLAUDE_BIN")), "claude")
		cliModel := cliModelFlag("claude", effectiveModel)

		var allowed []string
		for _, t := range s.bridge.Tools() {
			allowed = append(allowed, "mcp__orca__"+t.Name)
		}

		opts := []claude.SessionOption{
			claude.WithCLIPath(bin),
			claude.WithDisablePlugins(),
			claude.WithEnv(s.cfg.Env),
			// Every role works like the host orchestrator, in live and shadow
			// alike; shadow safety is the synced copy and the external proxy.
			claude.WithPermissionMode(claude.PermissionModeBypass),
		}
		if !s.cfg.AllowBuiltins {
			opts = append(opts, claude.WithSDKTools("orca", s.bridge))
		}
		opts = append(opts, claude.WithAllowedTools(allowed...))
		if cliModel != "" {
			opts = append(opts, claude.WithModel(cliModel))
		}
		if gw := s.resolvedGatewayURL(); gw != "" {
			opts = append(opts, claude.WithLLMEndpoint(llmendpoint.Endpoint{
				BaseURL:   gw,
				APIKeyEnv: "ANTHROPIC_API_KEY",
				APIKey:    cmp.Or(os.Getenv("ANTHROPIC_API_KEY"), os.Getenv("ANTHROPIC_AUTH_TOKEN"), os.Getenv("NEBULA_LLM_KEY")),
			}))
		}
		if effort := strings.TrimSpace(s.cfg.Effort); effort != "" {
			opts = append(opts, claude.WithEffort(claude.EffortLevel(effort)))
		}
		if sys := strings.TrimSpace(s.cfg.SystemPrompt); sys != "" {
			opts = append(opts, claude.WithSystemPrompt(sys))
		}
		if wd != "" {
			opts = append(opts, claude.WithWorkDir(wd))
		}
		if s.sessionID != "" {
			opts = append(opts, claude.WithResume(s.sessionID))
		}

		sess := claude.NewSession(opts...)
		if err := sess.Start(context.WithoutCancel(ctx)); err != nil {
			return nil, fmt.Errorf("orca/model: start claude session: %w", err)
		}
		s.claudeSess = sess
		s.started = true
		s.currentModel = effectiveModel
		s.toolNames = slices.Clone(newTools)
	} else if effectiveModel != "" && cliModelFlag("claude", effectiveModel) != cliModelFlag("claude", s.currentModel) {
		_ = s.claudeSess.SetModel(ctx, cliModelFlag("claude", effectiveModel))
		s.currentModel = effectiveModel
	}

	askRes, err := s.claudeSess.Ask(ctx, prompt)
	if info := s.claudeSess.Info(); info != nil && strings.TrimSpace(info.SessionID) != "" {
		s.sessionID = strings.TrimSpace(info.SessionID)
	}
	if err != nil {
		return nil, fmt.Errorf("orca/model: claude session turn: %w", err)
	}

	usage := Usage{
		InputTokens:              int64(askRes.Usage.InputTokens),
		OutputTokens:             int64(askRes.Usage.OutputTokens),
		CacheCreationInputTokens: int64(askRes.Usage.CacheCreationTokens),
		CacheReadInputTokens:     int64(askRes.Usage.CacheReadTokens),
		CostUSD:                  askRes.Usage.CostUSD,
	}
	if usage.CostUSD <= 0 {
		usage.CostUSD = CalculateCost(cmp.Or(effectiveModel, s.cfg.Model), usage, nil)
	}

	// The result's usage sums every API call of the turn; a multi-tool turn
	// would read as over the limit and rotate the session on every wake. Ask
	// the CLI for its actual context size instead.
	var contextTokens int
	if cu, err := s.claudeSess.ContextUsage(ctx); err == nil {
		contextTokens = cu.TotalTokens
	} else {
		slog.Warn("agent session context usage unavailable", "session_id", s.sessionID, "error", err)
	}

	return &AgentTurnResult{
		Text:          strings.TrimSpace(askRes.Text),
		SessionID:     s.sessionID,
		Usage:         usage,
		ContextTokens: contextTokens,
		TurnCount:     max(1, s.bridge.CallCount()),
	}, nil
}

func randomMCPToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("%d", time.Now().UnixNano())
}

func (s *AgentSession) ensureMCPServerLocked() (string, error) {
	if s.mcpServer != nil && s.mcpURL != "" {
		return s.mcpURL, nil
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", fmt.Errorf("orca/model: listen local MCP server: %w", err)
	}
	routePath := "/mcp/" + randomMCPToken()
	mux := http.NewServeMux()
	mux.Handle(routePath, s.bridge)
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		_ = srv.Serve(ln)
	}()
	s.mcpServer = srv
	s.mcpURL = fmt.Sprintf("http://%s%s", ln.Addr().String(), routePath)
	return s.mcpURL, nil
}

func (s *AgentSession) ensureWorkDirLocked() (string, error) {
	if wd := strings.TrimSpace(s.cfg.WorkDir); wd != "" {
		clean := filepath.Clean(wd)
		if err := ensurePrivateSessionDir(clean); err != nil {
			return "", err
		}
		return clean, nil
	}
	if s.tempWorkDir != "" {
		return s.tempWorkDir, nil
	}
	dir, err := os.MkdirTemp("", "orca-agent-session-*")
	if err != nil {
		return "", fmt.Errorf("orca/model: create temp session workdir: %w", err)
	}
	if err := ensurePrivateSessionDir(dir); err != nil {
		_ = os.RemoveAll(dir)
		return "", err
	}
	s.tempWorkDir = dir
	return dir, nil
}

func ensurePrivateSessionDir(clean string) error {
	if err := os.MkdirAll(clean, 0o700); err != nil {
		return fmt.Errorf("orca/model: create session workdir %q: %w", clean, err)
	}
	fi, err := os.Lstat(clean)
	if err != nil {
		return fmt.Errorf("orca/model: stat session workdir %q: %w", clean, err)
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("orca/model: session workdir %q must be a real directory, not a symlink or file", clean)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		uid := os.Getuid()
		if uid >= 0 && st.Uid != uint32(uid) {
			return fmt.Errorf("orca/model: session workdir %q is owned by uid %d, want %d", clean, st.Uid, uid)
		}
	}
	if fi.Mode().Perm()&0o077 != 0 {
		if err := os.Chmod(clean, 0o700); err != nil {
			return fmt.Errorf("orca/model: chmod session workdir %q to 0700: %w", clean, err)
		}
		fi, err = os.Lstat(clean)
		if err != nil {
			return fmt.Errorf("orca/model: restat session workdir %q: %w", clean, err)
		}
		if fi.Mode().Perm()&0o077 != 0 {
			return fmt.Errorf("orca/model: session workdir %q has unsafe permissions %#o", clean, fi.Mode().Perm())
		}
	}
	return nil
}

func (s *AgentSession) runCodexTurnLocked(
	ctx context.Context,
	prompt string,
	effectiveModel string,
	newTools []string,
) (*AgentTurnResult, error) {
	if !s.started || s.codexClient == nil || s.codexThread == nil {
		wd, err := s.ensureWorkDirLocked()
		if err != nil {
			return nil, err
		}
		mcpURL, err := s.ensureMCPServerLocked()
		if err != nil {
			return nil, err
		}
		bin := cmp.Or(strings.TrimSpace(s.cfg.CodexBin), strings.TrimSpace(os.Getenv("ORCA_CODEX_CLI")), strings.TrimSpace(os.Getenv("CODEX_BIN")), "codex")
		cliModel := cliModelFlag("codex", effectiveModel)

		enabledToolsJSON, _ := json.Marshal(newTools)
		appArgs := []string{
			"-c", fmt.Sprintf("mcp_servers.orca.url=%q", mcpURL),
			"-c", fmt.Sprintf("mcp_servers.orca.enabled_tools=%s", string(enabledToolsJSON)),
		}
		if effort := strings.TrimSpace(s.cfg.Effort); effort != "" {
			appArgs = append(appArgs, "-c", fmt.Sprintf("model_reasoning_effort=%q", effort))
		}

		clientOpts := []codex.ClientOption{
			codex.WithCodexPath(bin),
		}
		if gw := s.resolvedGatewayURL(); gw != "" {
			clientOpts = append(clientOpts, codex.WithLLMEndpoint(llmendpoint.Endpoint{
				BaseURL:   gw,
				APIKeyEnv: "OPENAI_API_KEY",
				APIKey:    cmp.Or(os.Getenv("OPENAI_API_KEY"), os.Getenv("NEBULA_LLM_KEY")),
				Wire:      llmendpoint.WireAPIResponses,
			}))
		}
		clientOpts = append(clientOpts, codex.WithAppServerArgs(appArgs...))

		client := codex.NewClient(clientOpts...)
		// Start the long-lived app-server process with a non-cancelling context so
		// a request-scoped first turn does not kill the subprocess on return.
		if err := client.Start(context.WithoutCancel(ctx)); err != nil {
			return nil, fmt.Errorf("orca/model: start codex app-server: %w", err)
		}

		devInstructions := strings.TrimSpace(s.cfg.SystemPrompt)
		threadCfg := map[string]interface{}{
			"mcp_servers": map[string]interface{}{
				"orca": map[string]interface{}{
					"url":           mcpURL,
					"enabled_tools": slices.Clone(newTools),
				},
			},
			"developer_instructions": strings.TrimSpace(devInstructions),
		}
		if effort := strings.TrimSpace(s.cfg.Effort); effort != "" {
			threadCfg["model_reasoning_effort"] = effort
		}

		threadOpts := []codex.ThreadOption{
			codex.WithSandbox("read-only"),
			codex.WithApprovalPolicy(codex.ApprovalPolicyNever),
			codex.WithThreadConfig(threadCfg),
		}
		if cliModel != "" {
			threadOpts = append(threadOpts, codex.WithModel(cliModel))
		}
		if wd != "" {
			threadOpts = append(threadOpts, codex.WithWorkDir(wd))
		}

		var thread *codex.Thread
		if s.sessionID != "" {
			thread, err = client.ResumeThread(ctx, s.sessionID, threadOpts...)
			if err != nil {
				thread, err = client.CreateThread(ctx, threadOpts...)
			}
		} else {
			thread, err = client.CreateThread(ctx, threadOpts...)
		}
		if err != nil {
			_ = client.Stop()
			return nil, fmt.Errorf("orca/model: start codex thread: %w", err)
		}
		if err := thread.WaitReady(ctx); err != nil {
			_ = client.Stop()
			return nil, fmt.Errorf("orca/model: codex thread ready: %w", err)
		}

		s.codexClient = client
		s.codexThread = thread
		s.sessionID = strings.TrimSpace(thread.ID())
		s.started = true
		s.currentModel = effectiveModel
		s.toolNames = slices.Clone(newTools)
	}

	var turnOpts []codex.TurnOption
	if effort := strings.TrimSpace(s.cfg.Effort); effort != "" {
		turnOpts = append(turnOpts, codex.WithEffort(effort))
	}
	turnRes, err := s.codexThread.Ask(ctx, prompt, turnOpts...)
	if id := strings.TrimSpace(s.codexThread.ID()); id != "" {
		s.sessionID = id
	}
	if err != nil {
		return nil, fmt.Errorf("orca/model: codex session turn: %w", err)
	}

	usage := Usage{
		InputTokens:          int64(turnRes.Usage.InputTokens),
		OutputTokens:         int64(turnRes.Usage.OutputTokens),
		CacheReadInputTokens: int64(turnRes.Usage.CachedInputTokens),
	}
	usage.CostUSD = CalculateCost(cmp.Or(effectiveModel, s.cfg.Model), usage, nil)

	return &AgentTurnResult{
		Text:          strings.TrimSpace(turnRes.FullText),
		SessionID:     s.sessionID,
		Usage:         usage,
		ContextTokens: int(usage.InputTokens + usage.OutputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens),
		TurnCount:     max(1, s.bridge.CallCount()),
	}, nil
}

func (s *AgentSession) stopLocked() error {
	var errs []error
	if s.claudeSess != nil {
		errs = append(errs, s.claudeSess.Stop())
		s.claudeSess = nil
	}
	if s.codexClient != nil {
		errs = append(errs, s.codexClient.Stop())
		s.codexClient = nil
		s.codexThread = nil
	}
	if s.mcpServer != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		errs = append(errs, s.mcpServer.Shutdown(ctx))
		cancel()
		s.mcpServer = nil
		s.mcpURL = ""
	}
	s.started = false
	return errors.Join(errs...)
}

// Close stops the underlying CLI process and cleans up any local MCP server.
func (s *AgentSession) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	err := s.stopLocked()
	if s.tempWorkDir != "" {
		_ = os.RemoveAll(s.tempWorkDir)
		s.tempWorkDir = ""
	}
	return err
}

// AgentSessionMap manages one long-lived AgentSession per runID for a role.
type AgentSessionMap struct {
	sessions map[string]*AgentSession
	mu       sync.Mutex
}

// GetOrCreate returns the active AgentSession for runID, creating one from cfg
// when none exists and syncing cfg.ResumeID when the session has not yet started.
func (m *AgentSessionMap) GetOrCreate(runID string, cfg AgentSessionConfig) *AgentSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions == nil {
		m.sessions = make(map[string]*AgentSession)
	}
	sess, ok := m.sessions[runID]
	if !ok || sess == nil {
		sess = NewAgentSession(cfg)
		m.sessions[runID] = sess
	} else if cfg.ResumeID != "" {
		sess.SetResumeID(cfg.ResumeID)
	}
	return sess
}

// Session returns the active AgentSession for runID, if any.
func (m *AgentSessionMap) Session(runID string) *AgentSession {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.sessions == nil {
		return nil
	}
	return m.sessions[runID]
}

// CloseRun stops and removes the AgentSession for runID, if any.
func (m *AgentSessionMap) CloseRun(runID string) error {
	m.mu.Lock()
	var sess *AgentSession
	if m.sessions != nil {
		sess = m.sessions[runID]
		delete(m.sessions, runID)
	}
	m.mu.Unlock()
	if sess == nil {
		return nil
	}
	return sess.Close()
}

// Close stops and removes all active AgentSessions in the map.
func (m *AgentSessionMap) Close() error {
	m.mu.Lock()
	sessions := m.sessions
	m.sessions = nil
	m.mu.Unlock()
	var errs []error
	for _, s := range sessions {
		if s != nil {
			errs = append(errs, s.Close())
		}
	}
	return errors.Join(errs...)
}

// RunFakeClaudeStreamCLI replays a recorded Claude NDJSON stream fixture over
// stdin/stdout for subprocess-based unit tests of AgentSession.
func RunFakeClaudeStreamCLI(streamFile, argsFile, mcpRespFile, counterFile string) {
	cliArgs := os.Args
	if idx := slices.Index(cliArgs, "--"); idx >= 0 {
		cliArgs = cliArgs[idx+1:]
	}
	if argsFile != "" {
		raw, _ := json.Marshal(cliArgs)
		if f, err := os.OpenFile(argsFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			_, _ = f.Write(append(raw, '\n'))
			_ = f.Close()
		}
	}

	streamData, err := os.ReadFile(streamFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read stream file %q: %v\n", streamFile, err)
		os.Exit(1)
	}
	var allLines []string
	for _, line := range strings.Split(string(streamData), "\n") {
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			allLines = append(allLines, trimmed)
		}
	}

	var turns [][]string
	var cur []string
	for _, line := range allLines {
		cur = append(cur, line)
		var env struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(line), &env) == nil && env.Type == "result" {
			turns = append(turns, cur)
			cur = nil
		}
	}
	if len(cur) > 0 {
		turns = append(turns, cur)
	}

	turnIdx := 0
	if counterFile != "" {
		if b, err := os.ReadFile(counterFile); err == nil {
			_, _ = fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &turnIdx)
		}
	}

	recordMCPResp := func(line string) {
		if mcpRespFile == "" {
			return
		}
		if f, err := os.OpenFile(mcpRespFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600); err == nil {
			_, _ = f.WriteString(line + "\n")
			_ = f.Close()
		}
	}

	// contextTokens is the last API call's context, as the CLI reports it.
	var contextTokens int
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		inLine := strings.TrimSpace(scanner.Text())
		if inLine == "" {
			continue
		}
		var msg struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype string `json:"subtype"`
			} `json:"request"`
		}
		if json.Unmarshal([]byte(inLine), &msg) != nil {
			continue
		}
		switch msg.Type {
		case "control_request":
			switch msg.Request.Subtype {
			case "initialize":
				fmt.Fprintf(os.Stdout, `{"type":"control_response","response":{"subtype":"success","request_id":%q,"response":{}}}`+"\n", msg.RequestID)
			case "get_context_usage":
				fmt.Fprintf(os.Stdout, `{"type":"control_response","response":{"subtype":"success","request_id":%q,"response":{"totalTokens":%d}}}`+"\n", msg.RequestID, contextTokens)
			}
		case "user":
			if turnIdx >= len(turns) {
				turnIdx = len(turns) - 1
			}
			turnLines := turns[turnIdx]
			turnIdx++
			if counterFile != "" {
				_ = os.WriteFile(counterFile, []byte(fmt.Sprintf("%d", turnIdx)), 0o600)
			}
			for _, outLine := range turnLines {
				fmt.Fprintln(os.Stdout, outLine)
				var outEnv struct {
					Type    string `json:"type"`
					Message struct {
						Usage struct {
							InputTokens              int `json:"input_tokens"`
							OutputTokens             int `json:"output_tokens"`
							CacheReadInputTokens     int `json:"cache_read_input_tokens"`
							CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
						} `json:"usage"`
					} `json:"message"`
				}
				_ = json.Unmarshal([]byte(outLine), &outEnv)
				if u := outEnv.Message.Usage; outEnv.Type == "assistant" {
					contextTokens = u.InputTokens + u.OutputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
				}
				if outEnv.Type == "control_request" {
					for scanner.Scan() {
						respLine := strings.TrimSpace(scanner.Text())
						if respLine == "" {
							continue
						}
						recordMCPResp(respLine)
						var ctrlResp struct {
							Type string `json:"type"`
						}
						if json.Unmarshal([]byte(respLine), &ctrlResp) == nil && ctrlResp.Type == "control_response" {
							break
						}
					}
				}
			}
		}
	}
	os.Exit(0)
}
