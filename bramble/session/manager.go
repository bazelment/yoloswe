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

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
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
type providerRunner struct {
	provider     agent.Provider
	eventHandler *sessionEventHandler
	model        string // model ID for provider (e.g. "gpt-5.3-codex")
	workDir      string // working directory for provider
}

func (r *providerRunner) Start(ctx context.Context) error {
	if lrp, ok := r.provider.(agent.LongRunningProvider); ok {
		return lrp.Start(ctx)
	}
	return nil
}

func (r *providerRunner) RunTurn(ctx context.Context, message string) (*claude.TurnUsage, error) {
	var opts []agent.ExecuteOption
	if r.eventHandler != nil {
		opts = append(opts, agent.WithProviderEventHandler(r.eventHandler))
	}
	if r.model != "" {
		opts = append(opts, agent.WithProviderModel(r.model))
	}
	if r.workDir != "" {
		opts = append(opts, agent.WithProviderWorkDir(r.workDir))
	}

	// Long-running providers maintain state across turns
	if lrp, ok := r.provider.(agent.LongRunningProvider); ok {
		result, err := lrp.SendMessage(ctx, message)
		if err != nil {
			return nil, err
		}
		return agentUsageToTurnUsage(result.Usage), nil
	}

	// Ephemeral providers create a fresh session each turn
	result, err := r.provider.Execute(ctx, message, nil, opts...)
	if err != nil {
		return nil, err
	}
	return agentUsageToTurnUsage(result.Usage), nil
}

func (r *providerRunner) Stop() error {
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
type ManagerConfig struct {
	Store       *Store
	Provider    agent.Provider // Optional pluggable agent backend; nil uses default runners
	RepoName    string
	SessionMode SessionMode
	YoloMode    bool // Skip all permission prompts
}

// Manager handles multiple concurrent sessions.
type Manager struct {
	ctx           context.Context
	sessions      map[SessionID]*Session
	events        chan interface{}
	outputs       map[SessionID][]OutputLine
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

// runSession runs a session in a goroutine, handling both planner and builder types.
// Both types follow the same lifecycle: start → run turns → idle → follow-up → ...
func (m *Manager) runSession(session *Session, prompt string) {
	defer m.wg.Done()
	m.updateSessionStatus(session, StatusRunning)

	// Create the appropriate runner based on session mode and type
	var runner sessionRunner
	var eventHandler *sessionEventHandler

	// Resolve model provider for runner routing
	agentModel, modelFound := ModelByID(session.Model)
	if !modelFound {
		// Unknown model — default to claude provider
		agentModel = AgentModel{ID: session.Model, Provider: ProviderClaude}
	}

	if m.config.SessionMode == SessionModeTmux {
		// Tmux mode: create tmux window running the agent CLI
		tmuxName := generateTmuxWindowName()
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
			runner = &providerRunner{
				provider:     agent.NewCodexProvider(),
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
					// remains (due to remain-on-exit). This means claude failed
					// to start or crashed.
					m.updateSessionStatus(session, StatusFailed)
					session.mu.Lock()
					session.Error = fmt.Errorf("claude process exited unexpectedly (window %q still open with remain-on-exit — check it for error details)", tmuxName)
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
			m.addOutput(session.ID, OutputLine{
				Timestamp: time.Now(),
				Type:      OutputTypeStatus,
				Content:   fmt.Sprintf("Follow-up: %s", followUp),
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

// appendOrAddText appends text to the last output line if it's a text line,
// otherwise adds a new text line. This allows streaming text to accumulate
// into a single OutputLine for proper markdown rendering.
func (m *Manager) appendOrAddText(sessionID SessionID, text string) {
	m.outputsMu.Lock()
	lines, ok := m.outputs[sessionID]
	if ok && len(lines) > 0 && lines[len(lines)-1].Type == OutputTypeText {
		// Append to existing text line
		lines[len(lines)-1].Content += text
		m.outputsMu.Unlock()
	} else {
		m.outputsMu.Unlock()
		m.addOutput(sessionID, OutputLine{
			Timestamp: time.Now(),
			Type:      OutputTypeText,
			Content:   text,
		})
		return
	}

	// Emit event so the TUI re-renders
	select {
	case m.events <- SessionOutputEvent{SessionID: sessionID}:
	default:
		log.Printf("WARNING: events channel full, dropping text append event for session %s", sessionID)
	}
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
		result[i] = deepCopyOutputLine(lines[i])
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
	m.outputsMu.Lock()
	m.outputs[sessionID] = make([]OutputLine, 0)
	m.outputsMu.Unlock()
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
