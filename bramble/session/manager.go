package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/acp"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
	"github.com/bazelment/yoloswe/bramble/sessionmodel"
	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/yoloswe"
	"github.com/bazelment/yoloswe/yoloswe/planner"
)

// sessionRunner abstracts the differences between planner and builder execution.
type sessionRunner interface {
	Start(ctx context.Context) error
	RunTurn(ctx context.Context, message string) (*claude.TurnUsage, error)
	Stop() error
}

// plannerRunner adapts PlannerWrapper to the sessionRunner interface.
// The first turn uses Run() to handle planning until ExitPlanMode.
// Subsequent turns use RunTurn() for plan iteration.
type plannerRunner struct {
	pw           *planner.PlannerWrapper
	PlanFilePath string // Set after first Run() completes
	firstRun     bool
}

func (r *plannerRunner) Start(ctx context.Context) error { return r.pw.Start(ctx) }
func (r *plannerRunner) Stop() error                     { return r.pw.Stop() }

func (r *plannerRunner) RunTurn(ctx context.Context, message string) (*claude.TurnUsage, error) {
	if !r.firstRun {
		r.firstRun = true
		err := r.pw.Run(ctx, message)
		r.PlanFilePath = r.pw.PlanFilePath()
		return nil, err
	}
	return r.pw.RunTurn(ctx, message)
}

// builderRunner adapts BuilderSession to the sessionRunner interface.
type builderRunner struct {
	builder *yoloswe.BuilderSession
}

func (r *builderRunner) Start(ctx context.Context) error { return r.builder.Start(ctx) }
func (r *builderRunner) Stop() error                     { return r.builder.Stop() }

func (r *builderRunner) RunTurn(ctx context.Context, message string) (*claude.TurnUsage, error) {
	return r.builder.RunTurn(ctx, message)
}

// providerRunner adapts agent.Provider to the sessionRunner interface.
// This allows plugging in any provider backend (Claude, Codex, Gemini)
// via the ManagerConfig.Provider field.
type providerRunner struct { //nolint:govet // fieldalignment: keep related lifecycle fields grouped
	provider        agent.Provider
	eventHandler    *sessionEventHandler
	eventBridgeDone chan struct{}
	model           string // model ID for provider (e.g. "gpt-5.3-codex")
	permissionMode  string // execution permissions (e.g. "bypass", "plan")
	workDir         string // working directory for provider
	eventBridgeWg   sync.WaitGroup
	turnObsMu       sync.Mutex
	turnObsSeq      uint64
	sawText         bool
	sawThinking     bool
	turnDone        bool
	turnDoneCh      chan struct{}
}

// trackingEventHandler wraps provider callbacks to record observed event types
// for the current turn before forwarding to the session event handler.
type trackingEventHandler struct {
	runner     *providerRunner
	next       agent.EventHandler
	turnObsSeq uint64
}

func (h *trackingEventHandler) OnText(text string) {
	if !h.runner.observeText(h.turnObsSeq, text) {
		return
	}
	h.next.OnText(text)
}

func (h *trackingEventHandler) OnThinking(thinking string) {
	if !h.runner.observeThinking(h.turnObsSeq, thinking) {
		return
	}
	h.next.OnThinking(thinking)
}

func (h *trackingEventHandler) OnToolStart(name, id string, input map[string]interface{}) {
	if !h.runner.acceptTurnEvent(h.turnObsSeq) {
		return
	}
	h.next.OnToolStart(name, id, input)
}

func (h *trackingEventHandler) OnToolComplete(name, id string, input map[string]interface{}, result interface{}, isError bool) {
	if !h.runner.acceptTurnEvent(h.turnObsSeq) {
		return
	}
	h.next.OnToolComplete(name, id, input, result, isError)
}

func (h *trackingEventHandler) OnTurnComplete(turnNumber int, success bool, durationMs int64, costUSD float64) {
	if !h.runner.markTurnDone(h.turnObsSeq) {
		return
	}
	h.next.OnTurnComplete(turnNumber, success, durationMs, costUSD)
}

func (h *trackingEventHandler) OnError(err error, context string) {
	if !h.runner.acceptTurnEvent(h.turnObsSeq) {
		return
	}
	h.next.OnError(err, context)
}

func (r *providerRunner) Start(ctx context.Context) error {
	if lrp, ok := r.provider.(agent.LongRunningProvider); ok {
		if err := lrp.Start(ctx); err != nil {
			return err
		}

		// Start event bridge to forward provider events to session event handler
		if r.eventHandler != nil {
			r.eventBridgeDone = make(chan struct{})
			r.eventBridgeWg.Add(1)
			go r.bridgeProviderEvents()
		}

		return nil
	}
	return nil
}

// bridgeProviderEvents forwards events from the provider to the session event handler.
func (r *providerRunner) bridgeProviderEvents() {
	defer r.eventBridgeWg.Done()

	events := r.provider.Events()
	if events == nil {
		return
	}

	for {
		select {
		case <-r.eventBridgeDone:
			return
		case ev, ok := <-events:
			if !ok {
				return
			}

			// Forward event to session handler. Call observation tracking
			// for side effects (turn-done detection, text/thinking seen flags)
			// and filter whitespace-only text/thinking deltas.
			turnObsSeq := r.currentTurnObservationSeq()
			switch e := ev.(type) {
			case agent.TextAgentEvent:
				if r.observeText(turnObsSeq, e.Text) || strings.TrimSpace(e.Text) != "" {
					r.eventHandler.OnText(e.Text)
				}
			case agent.ThinkingAgentEvent:
				if r.observeThinking(turnObsSeq, e.Thinking) || strings.TrimSpace(e.Thinking) != "" {
					r.eventHandler.OnThinking(e.Thinking)
				}
			case agent.ToolStartAgentEvent:
				r.eventHandler.OnToolStart(e.Name, e.ID, e.Input)
			case agent.ToolCompleteAgentEvent:
				r.eventHandler.OnToolComplete(e.Name, e.ID, e.Input, e.Result, e.IsError)
			case agent.TurnCompleteAgentEvent:
				r.markTurnDone(turnObsSeq)
				r.eventHandler.OnTurnComplete(e.TurnNumber, e.Success, e.DurationMs, e.CostUSD)
			case agent.ErrorAgentEvent:
				r.eventHandler.OnError(e.Err, e.Context)
			}
		}
	}
}

func (r *providerRunner) RunTurn(ctx context.Context, message string) (*claude.TurnUsage, error) {
	turnObsSeq := r.beginTurnObservation()

	var opts []agent.ExecuteOption
	if r.eventHandler != nil {
		opts = append(opts, agent.WithProviderEventHandler(&trackingEventHandler{
			runner:     r,
			next:       r.eventHandler,
			turnObsSeq: turnObsSeq,
		}))
	}
	if r.model != "" {
		opts = append(opts, agent.WithProviderModel(r.model))
	}
	if r.permissionMode != "" {
		opts = append(opts, agent.WithProviderPermissionMode(r.permissionMode))
	}
	if r.workDir != "" {
		opts = append(opts, agent.WithProviderWorkDir(r.workDir))
	}

	var result *agent.AgentResult

	// Long-running providers maintain state across turns
	if lrp, ok := r.provider.(agent.LongRunningProvider); ok {
		var err error
		result, err = lrp.SendMessage(ctx, message)
		if err != nil {
			return nil, err
		}
		// Give bridged events a brief window to flush before fallback synthesis.
		r.waitForTurnDone(turnObsSeq, 150*time.Millisecond)
	} else {
		// Ephemeral providers create a fresh session each turn
		var err error
		result, err = r.provider.Execute(ctx, message, nil, opts...)
		if err != nil {
			return nil, err
		}
	}

	r.emitFallbackFromResult(turnObsSeq, result)
	return agentUsageToTurnUsage(result.Usage), nil
}

func (r *providerRunner) beginTurnObservation() uint64 {
	r.turnObsMu.Lock()
	defer r.turnObsMu.Unlock()
	r.turnObsSeq++
	r.sawText = false
	r.sawThinking = false
	r.turnDone = false
	r.turnDoneCh = make(chan struct{})
	return r.turnObsSeq
}

func (r *providerRunner) currentTurnObservationSeq() uint64 {
	r.turnObsMu.Lock()
	defer r.turnObsMu.Unlock()
	return r.turnObsSeq
}

func (r *providerRunner) acceptTurnEvent(turnObsSeq uint64) bool {
	r.turnObsMu.Lock()
	defer r.turnObsMu.Unlock()
	return turnObsSeq == r.turnObsSeq && !r.turnDone && r.turnDoneCh != nil
}

func (r *providerRunner) observeText(turnObsSeq uint64, text string) bool {
	if strings.TrimSpace(text) == "" {
		return false
	}
	r.turnObsMu.Lock()
	defer r.turnObsMu.Unlock()
	if turnObsSeq != r.turnObsSeq || r.turnDone || r.turnDoneCh == nil {
		return false
	}
	r.sawText = true
	return true
}

func (r *providerRunner) observeThinking(turnObsSeq uint64, thinking string) bool {
	if strings.TrimSpace(thinking) == "" {
		return false
	}
	r.turnObsMu.Lock()
	defer r.turnObsMu.Unlock()
	if turnObsSeq != r.turnObsSeq || r.turnDone || r.turnDoneCh == nil {
		return false
	}
	r.sawThinking = true
	return true
}

func (r *providerRunner) markTurnDone(turnObsSeq uint64) bool {
	r.turnObsMu.Lock()
	defer r.turnObsMu.Unlock()
	if turnObsSeq != r.turnObsSeq || r.turnDone || r.turnDoneCh == nil {
		return false
	}
	r.turnDone = true
	close(r.turnDoneCh)
	return true
}

func (r *providerRunner) waitForTurnDone(turnObsSeq uint64, timeout time.Duration) {
	r.turnObsMu.Lock()
	if turnObsSeq != r.turnObsSeq {
		r.turnObsMu.Unlock()
		return
	}
	alreadyDone := r.turnDone
	doneCh := r.turnDoneCh
	r.turnObsMu.Unlock()

	if alreadyDone || doneCh == nil {
		return
	}

	select {
	case <-doneCh:
	case <-time.After(timeout):
	}
}

func (r *providerRunner) emitFallbackFromResult(turnObsSeq uint64, result *agent.AgentResult) {
	if r.eventHandler == nil || result == nil {
		return
	}

	r.turnObsMu.Lock()
	if turnObsSeq != r.turnObsSeq {
		r.turnObsMu.Unlock()
		return
	}
	sawText := r.sawText
	sawThinking := r.sawThinking
	r.turnObsMu.Unlock()

	thinking := strings.TrimSpace(result.Thinking)
	if !sawThinking && thinking != "" {
		r.eventHandler.OnThinking(thinking)
	}

	text := strings.TrimSpace(result.Text)
	if !sawText && text != "" {
		r.eventHandler.OnText(result.Text)
	}
}

func (r *providerRunner) Stop() error {
	// Stop event bridge
	if r.eventBridgeDone != nil {
		close(r.eventBridgeDone)
		// Wait for bridge goroutine to exit before proceeding
		r.eventBridgeWg.Wait()
		r.eventBridgeDone = nil
	}

	// Stop provider
	if lrp, ok := r.provider.(agent.LongRunningProvider); ok {
		return lrp.Stop()
	}
	return r.provider.Close()
}

// agentUsageToTurnUsage converts agent.AgentUsage to claude.TurnUsage.
func agentUsageToTurnUsage(u agent.AgentUsage) *claude.TurnUsage {
	return &claude.TurnUsage{
		CostUSD:         u.CostUSD,
		InputTokens:     u.InputTokens,
		OutputTokens:    u.OutputTokens,
		CacheReadTokens: u.CacheReadTokens,
	}
}

// SessionMode controls how sessions are executed.
type SessionMode string

const (
	// SessionModeAuto auto-detects based on environment ($TMUX)
	SessionModeAuto SessionMode = "auto"
	// SessionModeTUI uses in-process SDK with TUI display (default)
	SessionModeTUI SessionMode = "tui"
	// SessionModeTmux creates tmux sessions running claude CLI
	SessionModeTmux SessionMode = "tmux"
)

// ManagerConfig holds configuration for the session manager.
type ManagerConfig struct { //nolint:govet // fieldalignment: readability over packing
	Store         *Store
	Provider      agent.Provider       // Optional pluggable agent backend; nil uses default runners
	ModelRegistry *agent.ModelRegistry // Optional filtered model registry; nil uses full list
	RepoName      string
	SessionMode   SessionMode
	YoloMode      bool // Skip all permission prompts
	// TmuxExitOnQuit controls whether Bramble should kill tmux windows it started
	// when a session is stopped (including app quit). Default is false.
	TmuxExitOnQuit bool
	// ProtocolLogDir captures provider protocol/session logs for debugging.
	// If empty, protocol logging is disabled.
	ProtocolLogDir string
}

// Manager handles multiple concurrent sessions.
type Manager struct {
	ctx           context.Context
	sessions      map[SessionID]*Session
	events        chan interface{}
	outputs       map[SessionID][]OutputLine
	models        map[SessionID]*sessionmodel.SessionModel
	followUpChans map[SessionID]chan string
	cancel        context.CancelFunc
	config        ManagerConfig
	wg            sync.WaitGroup
	// Lock ordering: mu > outputsMu > followUpChansMu. Never acquire in reverse order.
	mu              sync.RWMutex
	outputsMu       sync.RWMutex
	followUpChansMu sync.RWMutex
}

// NewManager creates a new session manager.
func NewManager() *Manager {
	return NewManagerWithConfig(ManagerConfig{})
}

// NewManagerWithConfig creates a new session manager with the given config.
func NewManagerWithConfig(config ManagerConfig) *Manager {
	ctx, cancel := context.WithCancel(context.Background())

	// Resolve auto mode
	if config.SessionMode == SessionModeAuto || config.SessionMode == "" {
		if IsInsideTmux() && IsTmuxAvailable() {
			config.SessionMode = SessionModeTmux
		} else {
			config.SessionMode = SessionModeTUI
		}
	}

	return &Manager{
		config:        config,
		sessions:      make(map[SessionID]*Session),
		events:        make(chan interface{}, 10000),
		outputs:       make(map[SessionID][]OutputLine),
		models:        make(map[SessionID]*sessionmodel.SessionModel),
		followUpChans: make(map[SessionID]chan string),
		ctx:           ctx,
		cancel:        cancel,
	}
}

// Events returns the channel for session events.
func (m *Manager) Events() <-chan interface{} {
	return m.events
}

// IsInTmuxMode returns true if the manager is configured to use tmux mode.
func (m *Manager) IsInTmuxMode() bool {
	return m.config.SessionMode == SessionModeTmux
}

// Close shuts down the manager and all sessions.
func (m *Manager) Close() {
	m.cancel()

	// If TmuxExitOnQuit is enabled, kill tmux windows before canceling sessions
	if m.config.TmuxExitOnQuit && m.config.SessionMode == SessionModeTmux {
		m.mu.Lock()
		for _, s := range m.sessions {
			s.mu.RLock()
			windowID := s.TmuxWindowID
			windowName := s.TmuxWindowName
			s.mu.RUnlock()
			// Prefer window ID (stable), fall back to window name
			if windowID != "" {
				_ = KillTmuxWindowByID(windowID)
			} else if windowName != "" {
				// For sessions created before window ID tracking
				_ = (&tmuxRunner{windowName: windowName, killOnStop: true}).Stop()
			}
		}
		m.mu.Unlock()
	}

	m.mu.Lock()
	for _, s := range m.sessions {
		if s.cancel != nil {
			s.cancel()
		}
	}
	m.mu.Unlock()

	// Wait for all runSession goroutines to finish (with timeout).
	done := make(chan struct{})
	go func() { m.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
	}
}

// generateSessionID creates a unique session ID.
func generateSessionID(worktreeName string, sessionType SessionType) SessionID {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return SessionID(fmt.Sprintf("%s-%s-%s", worktreeName, sessionType, hex.EncodeToString(b)))
}

// generateTitle creates a short title from the first words of a prompt.
func generateTitle(prompt string, maxLen int) string {
	words := strings.Fields(prompt)
	var b strings.Builder
	for _, w := range words {
		if b.Len()+len(w)+1 > maxLen {
			break
		}
		if b.Len() > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(w)
	}
	if b.Len() == 0 && prompt != "" {
		if len(prompt) > maxLen-3 {
			return prompt[:maxLen-3] + "..."
		}
		return prompt
	}
	return b.String()
}

// StartSession creates and starts a new session of the given type.
// model is the AgentModel ID (e.g. "opus", "gpt-5.3-codex"). If empty,
// defaults to "opus" for planners and "sonnet" for builders.
func (m *Manager) StartSession(sessionType SessionType, worktreePath, prompt, model string) (SessionID, error) {
	worktreeName := filepath.Base(worktreePath)
	sessionID := generateSessionID(worktreeName, sessionType)

	ctx, cancel := context.WithCancel(m.ctx)

	if model == "" {
		model = "sonnet"
		if sessionType == SessionTypePlanner {
			model = "opus"
		}
	}

	session := &Session{
		ID:           sessionID,
		Type:         sessionType,
		Status:       StatusPending,
		WorktreePath: worktreePath,
		WorktreeName: worktreeName,
		Prompt:       prompt,
		Title:        generateTitle(prompt, 20),
		Model:        model,
		Progress:     &SessionProgress{LastActivity: time.Now()},
		CreatedAt:    time.Now(),
		ctx:          ctx,
		cancel:       cancel,
	}

	m.mu.Lock()
	m.sessions[sessionID] = session
	m.models[sessionID] = sessionmodel.NewSessionModel(1000)
	m.mu.Unlock()

	m.outputsMu.Lock()
	m.outputs[sessionID] = make([]OutputLine, 0, 1000)
	m.outputsMu.Unlock()

	m.wg.Add(1)
	go m.runSession(session, prompt)

	return sessionID, nil
}

// StartPlannerSession creates and starts a new planner session.
func (m *Manager) StartPlannerSession(worktreePath, prompt, model string) (SessionID, error) {
	return m.StartSession(SessionTypePlanner, worktreePath, prompt, model)
}

// StartBuilderSession creates and starts a new builder session.
func (m *Manager) StartBuilderSession(worktreePath, prompt, model string) (SessionID, error) {
	return m.StartSession(SessionTypeBuilder, worktreePath, prompt, model)
}

// TrackTmuxWindow registers an externally created tmux window so it appears in
// the session list for its worktree.
func (m *Manager) TrackTmuxWindow(worktreePath, windowName, windowID string) (SessionID, error) {
	if m.config.SessionMode != SessionModeTmux {
		return "", fmt.Errorf("track tmux window is only available in tmux mode")
	}
	if strings.TrimSpace(worktreePath) == "" {
		return "", fmt.Errorf("worktree path is empty")
	}
	if strings.TrimSpace(windowName) == "" {
		return "", fmt.Errorf("tmux window name is empty")
	}
	if strings.TrimSpace(windowID) == "" {
		return "", fmt.Errorf("tmux window ID is empty")
	}

	worktreeName := filepath.Base(worktreePath)
	sessionID := generateSessionID(worktreeName, SessionTypeBuilder)
	ctx, cancel := context.WithCancel(m.ctx)

	session := &Session{
		ID:             sessionID,
		Type:           SessionTypeBuilder,
		Status:         StatusPending,
		WorktreePath:   worktreePath,
		WorktreeName:   worktreeName,
		Prompt:         "Manual tmux window",
		Title:          windowName,
		TmuxWindowName: windowName,
		TmuxWindowID:   windowID,
		RunnerType:     "tmux-tracked",
		Progress:       &SessionProgress{LastActivity: time.Now()},
		CreatedAt:      time.Now(),
		ctx:            ctx,
		cancel:         cancel,
	}

	m.mu.Lock()
	m.sessions[sessionID] = session
	m.models[sessionID] = sessionmodel.NewSessionModel(1000)
	m.mu.Unlock()

	m.outputsMu.Lock()
	m.outputs[sessionID] = make([]OutputLine, 0, 16)
	m.outputsMu.Unlock()

	m.updateSessionStatus(session, StatusRunning)

	// Monitor window lifecycle only in real tmux environments.
	if IsInsideTmux() && IsTmuxAvailable() {
		m.wg.Add(1)
		go m.monitorTrackedTmuxWindow(session)
	}

	return sessionID, nil
}

func (m *Manager) monitorTrackedTmuxWindow(session *Session) {
	defer m.wg.Done()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-session.ctx.Done():
			// Window cleanup is handled in Close() if TmuxExitOnQuit is enabled
			// This ensures cleanup only happens on app quit, not on individual session stops
			m.updateSessionStatus(session, StatusStopped)
			return
		case <-ticker.C:
			session.mu.RLock()
			windowID := session.TmuxWindowID
			sessionID := session.ID
			session.mu.RUnlock()

			// Use window ID for stable identification
			if windowID == "" || TmuxWindowExistsByID(windowID) {
				continue
			}

			m.updateSessionStatus(session, StatusCompleted)

			m.mu.Lock()
			delete(m.sessions, sessionID)
			delete(m.models, sessionID)
			m.mu.Unlock()

			m.outputsMu.Lock()
			delete(m.outputs, sessionID)
			m.outputsMu.Unlock()
			return
		}
	}
}

// runSession runs a session in a goroutine, handling both planner and builder types.
// Both types follow the same lifecycle: start → run turns → idle → follow-up → ...
func (m *Manager) runSession(session *Session, prompt string) {
	defer m.wg.Done()
	m.updateSessionStatus(session, StatusRunning)

	// Create the appropriate runner based on session mode and type
	var runner sessionRunner
	var eventHandler *sessionEventHandler

	// Resolve model provider for runner routing.
	// Prefer the filtered registry if available, fall back to the full list.
	var agentModel AgentModel
	var modelFound bool
	if m.config.ModelRegistry != nil {
		agentModel, modelFound = m.config.ModelRegistry.ModelByID(session.Model)
	}
	if !modelFound {
		agentModel, modelFound = ModelByID(session.Model)
	}
	if !modelFound {
		// Unknown model — default to claude provider
		agentModel = AgentModel{ID: session.Model, Provider: ProviderClaude}
	}

	// If a registry is configured and the model's provider is not available,
	// fail early with a clear message.
	if m.config.ModelRegistry != nil && !m.config.ModelRegistry.HasProvider(agentModel.Provider) {
		m.updateSessionStatus(session, StatusFailed)
		session.mu.Lock()
		session.Error = fmt.Errorf("provider %q is not available (not installed or disabled in settings)", agentModel.Provider)
		session.mu.Unlock()
		m.addOutput(session.ID, OutputLine{
			Timestamp: time.Now(),
			Type:      OutputTypeError,
			Content:   fmt.Sprintf("Provider %q is not available. Install the CLI or enable it in settings.", agentModel.Provider),
		})
		m.persistSession(session)
		return
	}

	if m.config.SessionMode == SessionModeTmux {
		// Tmux mode: create tmux window running the agent CLI
		tmuxName := GenerateTmuxWindowName()
		session.mu.Lock()
		session.TmuxWindowName = tmuxName
		session.RunnerType = "tmux"
		session.mu.Unlock()

		permissionMode := ""
		if session.Type == SessionTypePlanner {
			permissionMode = "plan"
		}

		runner = &tmuxRunner{
			windowName:     tmuxName,
			workDir:        session.WorktreePath,
			prompt:         prompt,
			model:          agentModel.ID,
			provider:       agentModel.Provider,
			permissionMode: permissionMode,
			yoloMode:       m.config.YoloMode,
			killOnStop:     false, // Never kill on Stop(); cleanup happens in Close() if TmuxExitOnQuit is set
		}
		// No event handler for tmux mode - all output is in the tmux window
	} else {
		// TUI mode: create in-process runner
		session.mu.Lock()
		session.RunnerType = "tui"
		session.mu.Unlock()

		eventHandler = newSessionEventHandler(m, session.ID)

		if m.config.Provider != nil {
			// Use the pluggable provider backend
			runner = &providerRunner{
				provider:     m.config.Provider,
				eventHandler: eventHandler,
			}
		} else if agentModel.Provider == ProviderCodex {
			// Codex provider backend
			codexOpts, codexLogHint, codexStderrHint := m.codexProviderOptions(session.ID)
			if codexLogHint != "" {
				m.addOutput(session.ID, OutputLine{
					Timestamp: time.Now(),
					Type:      OutputTypeStatus,
					Content:   codexLogHint,
				})
			}
			if codexStderrHint != "" {
				m.addOutput(session.ID, OutputLine{
					Timestamp: time.Now(),
					Type:      OutputTypeStatus,
					Content:   codexStderrHint,
				})
			}
			runner = &providerRunner{
				provider:     agent.NewCodexProvider(codexOpts...),
				eventHandler: eventHandler,
				model:        session.Model,
				permissionMode: func() string {
					if session.Type == SessionTypePlanner {
						return "plan"
					}
					return "bypass"
				}(),
				workDir: session.WorktreePath,
			}
		} else if agentModel.Provider == ProviderGemini {
			// Gemini provider backend
			clientOpts := []acp.ClientOption{
				acp.WithBinaryArgs("--experimental-acp", "--model", session.Model),
			}

			geminiOpts, geminiLogHint, geminiStderrHint := m.geminiProviderOptions(session.ID)
			clientOpts = append(clientOpts, geminiOpts...)
			if geminiLogHint != "" {
				m.addOutput(session.ID, OutputLine{
					Timestamp: time.Now(),
					Type:      OutputTypeStatus,
					Content:   geminiLogHint,
				})
			}
			if geminiStderrHint != "" {
				m.addOutput(session.ID, OutputLine{
					Timestamp: time.Now(),
					Type:      OutputTypeStatus,
					Content:   geminiStderrHint,
				})
			}

			// Configure permission handler based on session type
			if session.Type == SessionTypePlanner {
				// Planner sessions should only be able to read, not write
				clientOpts = append(clientOpts, acp.WithPermissionHandler(&acp.PlanOnlyPermissionHandler{}))
			}
			// Builder sessions use the default BypassPermissionHandler (auto-approve all)

			runner = &providerRunner{
				provider:     agent.NewGeminiLongRunningProvider(clientOpts, acp.WithSessionCWD(session.WorktreePath)),
				eventHandler: eventHandler,
				model:        session.Model,
				workDir:      session.WorktreePath,
			}
		} else {
			// Default: use hardcoded planner/builder runners with model from session
			switch session.Type {
			case SessionTypePlanner:
				pw := planner.NewPlannerWrapper(planner.Config{
					Model:        session.Model,
					WorkDir:      session.WorktreePath,
					Simple:       true,
					BuildMode:    planner.BuildModeReturn,
					Output:       io.Discard,
					EventHandler: eventHandler,
				})
				runner = &plannerRunner{pw: pw}
			case SessionTypeBuilder:
				builder := yoloswe.NewBuilderSessionWithEvents(yoloswe.BuilderConfig{
					Model:   session.Model,
					WorkDir: session.WorktreePath,
				}, nil, eventHandler)
				runner = &builderRunner{builder: builder}
			}
		}
	}

	if err := runner.Start(session.ctx); err != nil {
		m.updateSessionStatus(session, StatusFailed)
		session.mu.Lock()
		session.Error = err
		session.mu.Unlock()
		m.addOutput(session.ID, OutputLine{
			Timestamp: time.Now(),
			Type:      OutputTypeError,
			Content:   fmt.Sprintf("Failed to start session: %v", err),
		})
		m.persistSession(session)
		return
	}

	defer func() {
		runner.Stop()
		if m.config.SessionMode != SessionModeTmux {
			m.followUpChansMu.Lock()
			delete(m.followUpChans, session.ID)
			m.followUpChansMu.Unlock()
		}
		m.persistSession(session)
	}()

	// Tmux mode: just wait for the window to be stopped manually
	// The tmux window handles all interaction, so we don't run turns
	if m.config.SessionMode == SessionModeTmux {
		m.updateSessionStatus(session, StatusRunning)
		startTime := time.Now()

		// Periodically check if tmux window still exists
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-session.ctx.Done():
				m.updateSessionStatus(session, StatusStopped)
				return
			case <-ticker.C:
				// Check if tmux window still exists
				session.mu.RLock()
				tmuxName := session.TmuxWindowName
				sessionID := session.ID
				session.mu.RUnlock()

				if tmuxName == "" {
					continue
				}

				windowExists := TmuxWindowExists(tmuxName)
				paneDead := windowExists && TmuxWindowPaneDead(tmuxName)

				if paneDead {
					// The process in the tmux window has exited but the window
					// remains (due to remain-on-exit). Check exit code to
					// determine whether it completed successfully or failed.
					exitCode, gotStatus := TmuxWindowPaneExitStatus(tmuxName)

					if gotStatus && exitCode == 0 {
						// Clean exit — session completed successfully
						m.updateSessionStatus(session, StatusCompleted)

						m.mu.Lock()
						delete(m.sessions, sessionID)
						delete(m.models, sessionID)
						m.mu.Unlock()

						m.outputsMu.Lock()
						delete(m.outputs, sessionID)
						m.outputsMu.Unlock()

						m.persistSession(session)
						return
					}

					// Non-zero exit or couldn't read status — failure
					m.updateSessionStatus(session, StatusFailed)
					session.mu.Lock()
					if gotStatus {
						session.Error = fmt.Errorf("claude process exited with code %d (window %q still open — check it for error details)", exitCode, tmuxName)
					} else {
						session.Error = fmt.Errorf("claude process exited unexpectedly (window %q still open with remain-on-exit — check it for error details)", tmuxName)
					}
					session.mu.Unlock()
					m.addOutput(session.ID, OutputLine{
						Timestamp: time.Now(),
						Type:      OutputTypeError,
						Content:   fmt.Sprintf("Session failed: claude exited in tmux window %q. Switch to that window to see the error.", tmuxName),
					})
					m.persistSession(session)
					return
				}

				if !windowExists {
					if time.Since(startTime) < 10*time.Second {
						// Window disappeared very quickly — likely a startup failure.
						// With remain-on-exit this shouldn't happen, but handle it
						// defensively in case the option wasn't set.
						m.updateSessionStatus(session, StatusFailed)
						session.mu.Lock()
						session.Error = fmt.Errorf("tmux window %q disappeared shortly after creation — claude may have failed to start", tmuxName)
						session.mu.Unlock()
						m.addOutput(session.ID, OutputLine{
							Timestamp: time.Now(),
							Type:      OutputTypeError,
							Content:   fmt.Sprintf("Session failed: tmux window %q vanished immediately. Claude may have failed to start.", tmuxName),
						})
						m.persistSession(session)
						return
					}

					// Window disappeared after running for a while — normal completion
					m.updateSessionStatus(session, StatusCompleted)

					// Remove from active sessions map
					m.mu.Lock()
					delete(m.sessions, sessionID)
					delete(m.models, sessionID)
					m.mu.Unlock()

					// Remove outputs
					m.outputsMu.Lock()
					delete(m.outputs, sessionID)
					m.outputsMu.Unlock()

					// Persist before removal (for history)
					m.persistSession(session)

					return
				}
			}
		}
	}

	// TUI mode: run turn-based interaction loop
	followUpChan := make(chan string, 1)
	m.followUpChansMu.Lock()
	m.followUpChans[session.ID] = followUpChan
	m.followUpChansMu.Unlock()

	currentPrompt := prompt
	for {
		usage, err := runner.RunTurn(session.ctx, currentPrompt)
		if err != nil {
			if session.ctx.Err() != nil {
				m.updateSessionStatus(session, StatusStopped)
			} else {
				m.updateSessionStatus(session, StatusFailed)
				session.mu.Lock()
				session.Error = err
				session.mu.Unlock()
				m.addOutput(session.ID, OutputLine{
					Timestamp: time.Now(),
					Type:      OutputTypeError,
					Content:   fmt.Sprintf("Session error: %v", err),
				})
			}
			return
		}

		if usage != nil {
			session.Progress.Update(func(p *SessionProgress) {
				p.TurnCount++
				p.TotalCostUSD += usage.CostUSD
				p.InputTokens += usage.InputTokens
				p.OutputTokens += usage.OutputTokens
			})
		}

		// After planner's first turn: read plan file and add to output
		if pr, ok := runner.(*plannerRunner); ok && pr.PlanFilePath != "" {
			session.mu.Lock()
			session.PlanFilePath = pr.PlanFilePath
			session.mu.Unlock()

			if planContent, readErr := os.ReadFile(pr.PlanFilePath); readErr == nil {
				m.addOutput(session.ID, OutputLine{
					Timestamp: time.Now(),
					Type:      OutputTypePlanReady,
					Content:   string(planContent),
				})
			}
		}

		m.updateSessionStatus(session, StatusIdle)

		select {
		case <-session.ctx.Done():
			m.updateSessionStatus(session, StatusStopped)
			return
		case followUp, ok := <-followUpChan:
			if !ok {
				m.updateSessionStatus(session, StatusCompleted)
				return
			}
			m.updateSessionStatus(session, StatusRunning)
			now := time.Now()
			m.addOutput(session.ID, OutputLine{
				Timestamp: now,
				Type:      OutputTypeStatus,
				Content:   "Follow-up prompt:",
			})
			m.addOutput(session.ID, OutputLine{
				Timestamp: now,
				Type:      OutputTypeText,
				Content:   followUp,
			})
			currentPrompt = followUp
		}
	}
}

// updateSessionStatus updates session status and emits event.
func (m *Manager) updateSessionStatus(session *Session, newStatus SessionStatus) {
	session.mu.Lock()
	oldStatus := session.Status
	session.Status = newStatus

	now := time.Now()
	switch newStatus {
	case StatusRunning:
		session.StartedAt = &now
	case StatusCompleted, StatusFailed, StatusStopped:
		session.CompletedAt = &now
	}
	session.mu.Unlock()

	// Emit state change event
	select {
	case m.events <- SessionStateChangeEvent{
		SessionID: session.ID,
		OldStatus: oldStatus,
		NewStatus: newStatus,
	}:
	default:
		log.Printf("WARNING: events channel full, dropping state change event for session %s (%s -> %s)", session.ID, oldStatus, newStatus)
	}
}

// addOutput adds an output line and emits event.
func (m *Manager) addOutput(sessionID SessionID, line OutputLine) {
	m.outputsMu.Lock()
	if lines, ok := m.outputs[sessionID]; ok {
		// Keep last 1000 lines
		if len(lines) >= 1000 {
			m.outputs[sessionID] = append(lines[1:], line)
		} else {
			m.outputs[sessionID] = append(lines, line)
		}
	}
	m.outputsMu.Unlock()

	// Emit output event
	select {
	case m.events <- SessionOutputEvent{
		SessionID: sessionID,
		Line:      line,
	}:
	default:
		log.Printf("WARNING: events channel full, dropping output event for session %s", sessionID)
	}
}

// appendOrAddOutput appends a streaming delta to the last output line if its
// type matches, otherwise adds a new line. This allows streaming text and
// thinking deltas to accumulate into a single OutputLine instead of creating
// one line per delta. Plain concatenation is used because live streaming
// deltas are non-overlapping token chunks. (For replay of protocol logs where
// deltas may overlap, use AppendStreamingDelta instead.)
func (m *Manager) appendOrAddOutput(sessionID SessionID, lineType OutputLineType, delta string) {
	m.outputsMu.Lock()
	lines, ok := m.outputs[sessionID]
	if ok && len(lines) > 0 && lines[len(lines)-1].Type == lineType {
		lines[len(lines)-1].Content += delta
		m.outputsMu.Unlock()
	} else {
		m.outputsMu.Unlock()
		m.addOutput(sessionID, OutputLine{
			Timestamp: time.Now(),
			Type:      lineType,
			Content:   delta,
		})
		return
	}

	// Emit event so the TUI re-renders
	select {
	case m.events <- SessionOutputEvent{SessionID: sessionID}:
	default:
		log.Printf("WARNING: events channel full, dropping %s append event for session %s", lineType, sessionID)
	}
}

// appendOrAddText appends text to the last text output line, or adds a new one.
func (m *Manager) appendOrAddText(sessionID SessionID, text string) {
	m.appendOrAddOutput(sessionID, OutputTypeText, text)
}

// appendOrAddThinking appends thinking to the last thinking output line, or adds a new one.
func (m *Manager) appendOrAddThinking(sessionID SessionID, thinking string) {
	if strings.TrimSpace(thinking) == "" {
		return
	}
	m.appendOrAddOutput(sessionID, OutputTypeThinking, thinking)
}

// updateToolOutput updates an existing tool output line by ToolID.
// This is used to update tool state from running to complete in-place.
func (m *Manager) updateToolOutput(sessionID SessionID, toolID string, fn func(*OutputLine)) {
	m.outputsMu.Lock()
	defer m.outputsMu.Unlock()

	lines, ok := m.outputs[sessionID]
	if !ok {
		return
	}

	// Find the tool line by ID (search from end since recent tools are likely at the end)
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i].ToolID == toolID && lines[i].Type == OutputTypeToolStart {
			// Copy-on-write: copy line, mutate copy, assign back to avoid races
			// with concurrent readers that may hold references to the old line.
			lineCopy := lines[i]
			// Deep-copy mutable map fields before mutation
			if lineCopy.ToolInput != nil {
				newInput := make(map[string]interface{}, len(lineCopy.ToolInput))
				for k, v := range lineCopy.ToolInput {
					newInput[k] = v
				}
				lineCopy.ToolInput = newInput
			}
			fn(&lineCopy)
			lines[i] = lineCopy
			// Emit update event
			select {
			case m.events <- SessionOutputEvent{
				SessionID: sessionID,
				Line:      lineCopy,
			}:
			default:
				log.Printf("WARNING: events channel full, dropping tool update event for session %s", sessionID)
			}
			return
		}
	}
}

// updateSessionProgress updates session progress safely.
// This is called by event handlers to update real-time progress.
func (m *Manager) updateSessionProgress(sessionID SessionID, fn func(*SessionProgress)) {
	m.mu.RLock()
	session, ok := m.sessions[sessionID]
	m.mu.RUnlock()

	if !ok || session.Progress == nil {
		return
	}

	session.Progress.Update(fn)
}

// StopSession stops a running session.
func (m *Manager) StopSession(id SessionID) error {
	m.mu.RLock()
	session, ok := m.sessions[id]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("session not found: %s", id)
	}

	session.mu.RLock()
	status := session.Status
	session.mu.RUnlock()

	if status != StatusRunning && status != StatusPending && status != StatusIdle {
		return fmt.Errorf("session not active: %s", id)
	}

	if session.cancel != nil {
		session.cancel()
	}

	return nil
}

// GetSession returns a session by ID.
func (m *Manager) GetSession(id SessionID) (*Session, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	session, ok := m.sessions[id]
	return session, ok
}

// GetSessionInfo returns session info for display.
func (m *Manager) GetSessionInfo(id SessionID) (SessionInfo, bool) {
	m.mu.RLock()
	session, ok := m.sessions[id]
	m.mu.RUnlock()

	if !ok {
		return SessionInfo{}, false
	}

	return session.ToInfo(), true
}

// GetSessionsForWorktree returns all sessions for a worktree.
func (m *Manager) GetSessionsForWorktree(worktreePath string) []SessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []SessionInfo
	for _, s := range m.sessions {
		if s.WorktreePath == worktreePath {
			result = append(result, s.ToInfo())
		}
	}
	sortSessionsByTime(result)
	return result
}

// GetAllSessions returns all sessions sorted by creation time (newest first).
func (m *Manager) GetAllSessions() []SessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]SessionInfo, 0, len(m.sessions))
	for _, s := range m.sessions {
		result = append(result, s.ToInfo())
	}
	sortSessionsByTime(result)
	return result
}

// sortSessionsByTime sorts sessions newest-first, breaking ties by ID
// for deterministic ordering when timestamps are equal.
func sortSessionsByTime(sessions []SessionInfo) {
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].CreatedAt.Equal(sessions[j].CreatedAt) {
			return sessions[i].ID < sessions[j].ID
		}
		return sessions[i].CreatedAt.After(sessions[j].CreatedAt)
	})
}

// GetSessionOutput returns the output lines for a session.
func (m *Manager) GetSessionOutput(id SessionID) []OutputLine {
	m.outputsMu.RLock()
	defer m.outputsMu.RUnlock()

	lines, ok := m.outputs[id]
	if !ok {
		return nil
	}

	// Deep copy to avoid shared references to mutable fields.
	result := make([]OutputLine, len(lines))
	for i := range lines {
		result[i] = DeepCopyOutputLine(lines[i])
	}
	return result
}

// CountByStatus returns counts of sessions by status.
func (m *Manager) CountByStatus() map[SessionStatus]int {
	m.mu.RLock()
	defer m.mu.RUnlock()

	counts := make(map[SessionStatus]int)
	for _, s := range m.sessions {
		s.mu.RLock()
		counts[s.Status]++
		s.mu.RUnlock()
	}
	return counts
}

// SendFollowUp sends a follow-up message to an idle session.
func (m *Manager) SendFollowUp(id SessionID, message string) error {
	m.mu.RLock()
	session, ok := m.sessions[id]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("session not found: %s", id)
	}

	session.mu.RLock()
	status := session.Status
	session.mu.RUnlock()

	if status != StatusIdle {
		return fmt.Errorf("session is not idle (status: %s)", status)
	}

	m.followUpChansMu.RLock()
	ch, ok := m.followUpChans[id]
	m.followUpChansMu.RUnlock()

	if !ok {
		return fmt.Errorf("no follow-up channel for session: %s", id)
	}

	select {
	case ch <- message:
		return nil
	default:
		return fmt.Errorf("follow-up channel full")
	}
}

// CompleteSession marks an idle session as completed.
// This is used when the user is done with follow-ups.
func (m *Manager) CompleteSession(id SessionID) error {
	m.mu.RLock()
	session, ok := m.sessions[id]
	m.mu.RUnlock()

	if !ok {
		return fmt.Errorf("session not found: %s", id)
	}

	session.mu.RLock()
	status := session.Status
	session.mu.RUnlock()

	if status != StatusIdle {
		return fmt.Errorf("session is not idle (status: %s)", status)
	}

	// Close the follow-up channel to signal completion
	m.followUpChansMu.Lock()
	if ch, ok := m.followUpChans[id]; ok {
		close(ch)
		delete(m.followUpChans, id)
	}
	m.followUpChansMu.Unlock()

	return nil
}

// DeleteSession removes a session from the manager.
// The session must be in a terminal state.
func (m *Manager) DeleteSession(id SessionID) error {
	m.mu.Lock()
	session, ok := m.sessions[id]
	if !ok {
		m.mu.Unlock()
		return fmt.Errorf("session not found: %s", id)
	}

	session.mu.RLock()
	status := session.Status
	session.mu.RUnlock()

	if !status.IsTerminal() && status != StatusIdle {
		m.mu.Unlock()
		return fmt.Errorf("cannot delete session in status: %s", status)
	}

	delete(m.sessions, id)
	delete(m.models, id)
	m.mu.Unlock()

	m.outputsMu.Lock()
	delete(m.outputs, id)
	m.outputsMu.Unlock()

	// Also delete from store if configured
	if m.config.Store != nil && m.config.RepoName != "" {
		_ = m.config.Store.DeleteSession(m.config.RepoName, session.WorktreeName, id)
	}

	return nil
}

// persistSession saves a session to the store.
func (m *Manager) persistSession(session *Session) {
	if m.config.Store == nil || m.config.RepoName == "" {
		return
	}

	m.outputsMu.RLock()
	output := m.outputs[session.ID]
	outputCopy := make([]OutputLine, len(output))
	copy(outputCopy, output)
	m.outputsMu.RUnlock()

	stored := SessionToStored(session, m.config.RepoName, outputCopy)
	if err := m.config.Store.SaveSession(stored); err != nil {
		// Log error but don't fail
		m.addOutput(session.ID, OutputLine{
			Timestamp: time.Now(),
			Type:      OutputTypeError,
			Content:   fmt.Sprintf("Failed to persist session: %v", err),
		})
	}
}

// LoadHistorySessions loads past sessions from the store for a worktree.
func (m *Manager) LoadHistorySessions(worktreeName string) ([]*SessionMeta, error) {
	if m.config.Store == nil || m.config.RepoName == "" {
		return nil, nil
	}
	return m.config.Store.ListSessions(m.config.RepoName, worktreeName)
}

// LoadSessionFromHistory loads a full session from the store.
func (m *Manager) LoadSessionFromHistory(worktreeName string, id SessionID) (*StoredSession, error) {
	if m.config.Store == nil || m.config.RepoName == "" {
		return nil, fmt.Errorf("store not configured")
	}
	return m.config.Store.LoadSession(m.config.RepoName, worktreeName, id)
}

// AddSession adds a session to the manager (for testing).
func (m *Manager) AddSession(session *Session) {
	m.mu.Lock()
	m.sessions[session.ID] = session
	m.mu.Unlock()
}

// AddOutputLine adds an output line to a session (for testing).
func (m *Manager) AddOutputLine(sessionID SessionID, line OutputLine) {
	m.addOutput(sessionID, line)
}

// InitOutputBuffer initializes the output buffer for a session (for testing).
func (m *Manager) InitOutputBuffer(sessionID SessionID) {
	// Acquire mu before outputsMu to respect the documented lock ordering:
	// mu > outputsMu > followUpChansMu.
	m.mu.Lock()
	if _, ok := m.models[sessionID]; !ok {
		m.models[sessionID] = sessionmodel.NewSessionModel(1000)
	}
	m.mu.Unlock()

	m.outputsMu.Lock()
	m.outputs[sessionID] = make([]OutputLine, 0)
	m.outputsMu.Unlock()
}

// GetSessionModel returns the SessionModel for a session.
func (m *Manager) GetSessionModel(id SessionID) *sessionmodel.SessionModel {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.models[id]
}

// PersistSession saves a session to the store (exported for testing).
func (m *Manager) PersistSession(session *Session) {
	m.persistSession(session)
}

// UpdateSessionStatus updates session status (exported for testing).
func (m *Manager) UpdateSessionStatus(session *Session, newStatus SessionStatus) {
	m.updateSessionStatus(session, newStatus)
}

// SetFollowUpChan sets the follow-up channel for a session (for testing).
func (m *Manager) SetFollowUpChan(sessionID SessionID, ch chan string) {
	m.followUpChansMu.Lock()
	m.followUpChans[sessionID] = ch
	m.followUpChansMu.Unlock()
}

// HasFollowUpChan checks if a follow-up channel exists for a session (for testing).
func (m *Manager) HasFollowUpChan(sessionID SessionID) bool {
	m.followUpChansMu.RLock()
	defer m.followUpChansMu.RUnlock()
	_, exists := m.followUpChans[sessionID]
	return exists
}

func (m *Manager) protocolLogPath(sessionID SessionID, suffix string) (string, bool) {
	logDir := strings.TrimSpace(m.config.ProtocolLogDir)
	if logDir == "" {
		return "", false
	}
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		log.Printf("WARNING: failed to create protocol log dir %q: %v", logDir, err)
		return "", false
	}
	return filepath.Join(logDir, fmt.Sprintf("%s-%s", sessionID, suffix)), true
}

func (m *Manager) codexProviderOptions(sessionID SessionID) ([]codex.ClientOption, string, string) {
	sessionLogPath, ok := m.protocolLogPath(sessionID, "codex.protocol.jsonl")
	if !ok {
		return nil, "", ""
	}

	stderrLogPath, _ := m.protocolLogPath(sessionID, "codex.stderr.log")

	opts := []codex.ClientOption{
		codex.WithSessionLogPath(sessionLogPath),
	}

	if stderrLogPath != "" {
		opts = append(opts, codex.WithStderrHandler(newFileAppendHandler(stderrLogPath)))
	}

	return opts,
		fmt.Sprintf("Codex protocol log: %s", sessionLogPath),
		fmt.Sprintf("Codex stderr log: %s", stderrLogPath)
}

func (m *Manager) geminiProviderOptions(sessionID SessionID) ([]acp.ClientOption, string, string) {
	stderrLogPath, ok := m.protocolLogPath(sessionID, "gemini.stderr.log")
	if !ok {
		return nil, "", ""
	}

	protocolLogPath, _ := m.protocolLogPath(sessionID, "gemini.protocol.jsonl")

	opts := []acp.ClientOption{
		acp.WithStderrHandler(newFileAppendHandler(stderrLogPath)),
	}

	var protocolLogHint string
	if protocolLogPath != "" {
		opts = append(opts, acp.WithProtocolLogger(newFileAppendWriter(protocolLogPath)))
		protocolLogHint = fmt.Sprintf("Gemini protocol log: %s", protocolLogPath)
	}

	return opts,
		protocolLogHint,
		fmt.Sprintf("Gemini stderr log: %s", stderrLogPath)
}

// fileAppendWriter implements io.Writer by appending to a file.
type fileAppendWriter struct {
	path string
	mu   sync.Mutex
}

func newFileAppendWriter(path string) *fileAppendWriter {
	return &fileAppendWriter{path: path}
}

func (w *fileAppendWriter) Write(p []byte) (int, error) {
	if len(p) == 0 || strings.TrimSpace(w.path) == "" {
		return len(p), nil
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	f, err := os.OpenFile(w.path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return 0, err
	}
	n, writeErr := f.Write(p)
	if closeErr := f.Close(); writeErr == nil {
		writeErr = closeErr
	}
	return n, writeErr
}

func newFileAppendHandler(path string) func([]byte) {
	w := newFileAppendWriter(path)
	return func(data []byte) {
		if _, err := w.Write(data); err != nil {
			log.Printf("WARNING: failed to write log %q: %v", path, err)
		}
	}
}
