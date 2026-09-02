package session

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/acp"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/codex"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/llmendpoint"
	"github.com/bazelment/yoloswe/bramble/selfexec"
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
	// CLISessionID returns the CLI session ID (from system{init}) after Start().
	// Returns "" if the runner doesn't support resume (e.g. tmux, provider).
	CLISessionID() string
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
func (r *plannerRunner) CLISessionID() string            { return r.pw.CLISessionID() }

func (r *plannerRunner) RunTurn(ctx context.Context, message string) (*claude.TurnUsage, error) {
	if !r.firstRun {
		r.firstRun = true
		err := r.pw.Run(ctx, message)
		r.PlanFilePath = r.pw.PlanFilePath()
		// Run() doesn't return usage directly, but the planner accumulates
		// stats internally via TurnComplete events. Extract them so the
		// caller can update Progress (fixes $0.0000 cost display).
		stats := r.pw.TotalStats()
		usage := &claude.TurnUsage{
			InputTokens:     stats.InputTokens,
			OutputTokens:    stats.OutputTokens,
			CacheReadTokens: stats.CacheReadTokens,
			CostUSD:         stats.CostUSD,
		}
		return usage, err
	}
	return r.pw.RunTurn(ctx, message)
}

// providerRunner adapts agent.Provider to the sessionRunner interface.
// This allows plugging in any provider backend (Claude, Codex, Gemini, agy)
// via the ManagerConfig.Provider field.
type providerRunner struct { //nolint:govet // fieldalignment: keep related lifecycle fields grouped
	provider        agent.Provider
	eventHandler    *sessionEventHandler
	eventBridgeDone chan struct{}
	model           string // model ID for provider (e.g. "gpt-5.5")
	permissionMode  string // execution permissions (e.g. "bypass", "plan")
	workDir         string // working directory for provider
	llmEndpoint     llmendpoint.Endpoint
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
				// TurnEnd is emitted synchronously by the manager after
				// RunTurn returns, so we skip OnTurnComplete here to
				// avoid duplicates. See the addOutput call after RunTurn.
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
	if !r.llmEndpoint.IsZero() {
		opts = append(opts, agent.WithProviderLLMEndpoint(r.llmEndpoint))
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

func (r *providerRunner) CLISessionID() string { return "" }

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
	// RecordingDir enables JSONL session recording for all sessions.
	// If empty, recording is disabled.
	RecordingDir string
	// ResearchDir is where subagent result files are written. If empty it
	// defaults to ~/.bramble/research. Injectable so a test can point it at
	// its own directory rather than writing into the real one.
	ResearchDir string
	// IPCSockPath is the path to the bramble IPC Unix domain socket.
	// Used to propagate BRAMBLE_SOCK to tmux windows so hook commands
	// can call back to the TUI. Set by main after startIPCServer.
	IPCSockPath string
	// ControlSockPath is the path to the bramble control Unix domain socket.
	// Propagated to tmux windows as BRAMBLE_CONTROL_SOCK so a session can
	// drive its peers (send-input, send-key). Set by main after
	// startControlServer.
	ControlSockPath string
	// Registry is the shared session registry for cross-repo IPC lookups.
	// Propagated through sharedManagerConfig so that openRepo can register
	// new managers automatically.
	Registry *SessionRegistry
	// ChildModel overrides the default model for child sessions spawned by
	// the delegator. If empty, children default to the delegator's own model.
	ChildModel string
}

// Manager handles multiple concurrent sessions.
type Manager struct { //nolint:govet // fieldalignment: readability over packing
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
	// stateSubscribers receive copies of SessionStateChangeEvent, without
	// consuming the primary events channel. The delegator's child watcher and
	// the subagent notifier are both built on it.
	stateSubscribers   []*stateSink
	stateSubscribersMu sync.Mutex
	worktreeDirtyMu    sync.RWMutex
	onWorktreeDirty    func(repoName, worktreePath string)
}

// RepoName returns the repo name this manager is configured for.
func (m *Manager) RepoName() string {
	return m.config.RepoName
}

// SetWorktreeDirtyCallback registers a callback for mutating tool completion.
func (m *Manager) SetWorktreeDirtyCallback(callback func(repoName, worktreePath string)) {
	m.worktreeDirtyMu.Lock()
	defer m.worktreeDirtyMu.Unlock()
	m.onWorktreeDirty = callback
}

func (m *Manager) notifyWorktreeDirty(sessionID SessionID) {
	m.worktreeDirtyMu.RLock()
	callback := m.onWorktreeDirty
	m.worktreeDirtyMu.RUnlock()
	if callback == nil {
		return
	}
	info, ok := m.GetSessionInfo(sessionID)
	if !ok || info.WorktreePath == "" {
		return
	}
	repoName := info.RepoName
	if repoName == "" {
		repoName = m.config.RepoName
	}
	callback(repoName, info.WorktreePath)
}

// NewManager creates a new session manager.
func NewManager() *Manager {
	return NewManagerWithConfig(ManagerConfig{})
}

// ResolveSessionMode turns a configured mode into the one a manager will
// actually run in: auto (and unset) means tmux exactly when there is a tmux to
// be inside, and an explicit mode is taken at its word.
//
// Exported because callers outside a manager have to make the same call — the
// startup probe decides which repos to open on it, and deciding differently
// from the manager that then has to reconcile them is how repos get opened for
// sessions nothing will ever touch.
func ResolveSessionMode(mode SessionMode) SessionMode {
	if mode != SessionModeAuto && mode != "" {
		return mode
	}
	if IsInsideTmux() && IsTmuxAvailable() {
		return SessionModeTmux
	}
	return SessionModeTUI
}

// NewManagerWithConfig creates a new session manager with the given config.
func NewManagerWithConfig(config ManagerConfig) *Manager {
	ctx, cancel := context.WithCancel(context.Background())

	config.SessionMode = ResolveSessionMode(config.SessionMode)

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

// IPCSockPath returns the IPC socket path, if configured.
func (m *Manager) IPCSockPath() string {
	return m.config.IPCSockPath
}

// SetIPCSockPath updates the IPC socket path after the server has started.
// This is called from main after startIPCServer since the manager is created
// before the IPC server.
func (m *Manager) SetIPCSockPath(path string) {
	m.config.IPCSockPath = path
}

// ControlSockPath returns the control socket path, if configured.
func (m *Manager) ControlSockPath() string {
	return m.config.ControlSockPath
}

// SetControlSockPath updates the control socket path after the server has
// started. Like SetIPCSockPath, this is called from main because the manager
// is created before the control server.
func (m *Manager) SetControlSockPath(path string) {
	m.config.ControlSockPath = path
}

// DisableTmuxExitOnQuit clears the kill-windows-on-close behaviour that Close()
// applies, which an in-place restart must not trigger: those windows are what
// the new process image re-adopts.
//
// Like the socket-path setters above, this is unsynchronized because it runs on
// the main goroutine after the TUI has exited.
func (m *Manager) DisableTmuxExitOnQuit() {
	m.config.TmuxExitOnQuit = false
}

// stateSink is one registered listener. A struct rather than the bare function
// so unsubscribing can find it again: functions are not comparable.
type stateSink struct {
	fn func(SessionStateChangeEvent)
}

// SubscribeStateChanges registers a sink for every SessionStateChangeEvent and
// returns an unsubscribe function.
//
// A function called on the emitting goroutine, not a channel. A channel here
// has to be bounded, and a bounded channel leaves only two behaviours, both
// wrong: drop, which loses the completion a subagent's report rides and which
// the emitter cannot even detect — a tight burst fills the buffer before the
// reading goroutine is scheduled once — or block, which stalls the status
// transition that produced the event, behind arbitrary listener work.
//
// So the sink must not block. One that has slow work to do queues internally,
// where the queue can grow; watchStateChanges is that, and every listener in
// this package goes through it.
func (m *Manager) SubscribeStateChanges(fn func(SessionStateChangeEvent)) func() {
	sink := &stateSink{fn: fn}
	m.stateSubscribersMu.Lock()
	m.stateSubscribers = append(m.stateSubscribers, sink)
	m.stateSubscribersMu.Unlock()

	return func() {
		m.stateSubscribersMu.Lock()
		defer m.stateSubscribersMu.Unlock()
		for i, sub := range m.stateSubscribers {
			if sub == sink {
				m.stateSubscribers = slices.Delete(m.stateSubscribers, i, i+1)
				break
			}
		}
	}
}

// tmuxWindowAlive reports whether the tmux window identified by windowID and/or
// windowName is still alive. It prefers ID-based lookup (stable); falls back to
// name-based lookup when no ID is available. Returns false if neither identifier
// is provided.
func tmuxWindowAlive(windowID, windowName string) bool {
	if windowID != "" {
		return TmuxWindowExistsByID(windowID)
	}
	if windowName != "" {
		// Also check for the "!"-prefixed name used by NotifyTmuxWindow,
		// so the liveness check still succeeds after a notification rename.
		return TmuxWindowExists(windowName) || TmuxWindowExists(TmuxNotifyPrefix+windowName)
	}
	return false
}

// dirExists reports whether path is an existing directory. Any Stat failure
// counts as "not there", so callers must only use it where a false negative is
// harmless — see the ID-less scoping of the reconcile worktree gate below.
//
// app.worktreePathExists is the same check for the app layer, kept separate
// because that one is a package var for stubbing. Keep the two in step.
func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// ReconcileTmuxSessions checks stored sessions for previously-running tmux sessions
// and re-adopts any whose tmux windows are still alive. Sessions whose windows
// have disappeared are marked as completed.
func (m *Manager) ReconcileTmuxSessions() error {
	if m.config.Store == nil || m.config.RepoName == "" {
		return nil
	}
	if m.config.SessionMode != SessionModeTmux {
		return nil
	}
	// Only reconcile when actually inside tmux; outside tmux all window-alive checks
	// return false, which would incorrectly mark live sessions as completed.
	if !IsInsideTmux() || !IsTmuxAvailable() {
		return nil
	}
	return m.reconcileTmuxSessions(tmuxWindowAlive)
}

// reconcileTmuxSessions is the reconcile pass behind ReconcileTmuxSessions,
// split out so it can be tested without a tmux server: the caller's guards and
// windowAlive are the only parts that need one.
func (m *Manager) reconcileTmuxSessions(windowAlive func(windowID, windowName string) bool) error {
	// List all worktrees for this repo
	worktrees, err := m.config.Store.ListWorktrees(m.config.RepoName)
	if err != nil {
		return fmt.Errorf("failed to list worktrees: %w", err)
	}

	for _, wtName := range worktrees {
		sessions, err := m.config.Store.ListSessions(m.config.RepoName, wtName)
		if err != nil {
			continue
		}

		for _, meta := range sessions {
			if !isTmuxRunner(meta.RunnerType) {
				continue
			}
			if meta.Status.IsTerminal() {
				continue
			}

			// Check if already tracked by this manager
			m.mu.RLock()
			_, alreadyTracked := m.sessions[meta.ID]
			m.mu.RUnlock()
			if alreadyTracked {
				continue
			}

			// Load full session data
			stored, err := m.config.Store.LoadSession(m.config.RepoName, wtName, meta.ID)
			if err != nil {
				continue
			}

			// Is the session still there? For a record carrying a tmux window
			// ID the window answers on its own: tmuxWindowAlive matches the ID
			// exactly, and tmux never recycles a window ID within a server
			// lifetime, so a live window under that ID is this session's window.
			//
			// A record carrying only a name cannot tell. Names are recycled —
			// GenerateTmuxWindowName hands out the lowest free "repo/worktree:N"
			// — so once a session's window is killed the next session on that
			// worktree takes the same name back, and the record looks alive
			// because a *different* session is answering for it. Adopted, it
			// lands in m.sessions with a monitor goroutine free to emit fresh
			// idle transitions that reach the parent as subagent reports for a
			// session list-sessions no longer knows about: issue #331.
			//
			// For that ID-less case only, the worktree is the independent
			// signal. Reaping a session kills its window and removes its
			// worktree, so a worktree path that no longer exists says the
			// session is gone whoever holds its old name. An empty path is not
			// evidence of anything and defers to the window.
			//
			// Deliberately not applied to ID-carrying records. It could only
			// ever reap them — the ID match is already exact — and a failed
			// Stat is not a finished session. Reaping is unrecoverable: the
			// branch below writes StatusCompleted to the store without killing
			// the window, so every later pass skips the record as terminal and
			// a live agent keeps working in a session bramble has disowned.
			gone := !windowAlive(stored.TmuxWindowID, stored.TmuxWindowName) ||
				(stored.TmuxWindowID == "" && stored.WorktreePath != "" && !dirExists(stored.WorktreePath))
			if gone {
				// The session is gone — mark as completed.
				//
				// Only when it is not terminal already: this runs on every
				// start, and re-announcing a session that was completed three
				// restarts ago would report it to its parent each time.
				if stored.Status.IsTerminal() {
					continue
				}
				previous := stored.Status
				now := time.Now()
				stored.Status = StatusCompleted
				stored.CompletedAt = &now
				_ = m.config.Store.SaveSession(stored)

				// Announce it. A subagent that finished while bramble was down
				// still owes its parent a report, and this is the only place
				// that transition happens — nothing else will emit it, because
				// the session is never adopted into m.sessions.
				m.emitSessionStateChange(SessionStateChangeEvent{
					Info:      StoredToSessionInfo(stored),
					SessionID: stored.ID,
					OldStatus: previous,
					NewStatus: StatusCompleted,
				})
				continue
			}

			// Re-adopt: create in-memory session and start monitoring
			ctx, cancel := context.WithCancel(m.ctx)
			session := storedToSession(stored)
			session.TmuxWindowName = stored.TmuxWindowName
			session.TmuxWindowID = stored.TmuxWindowID
			session.RunnerType = stored.RunnerType
			// The manager's name, not the record's: this loop only ever loads
			// sessions filed under m.config.RepoName, and the in-memory session
			// is keyed by it.
			session.RepoName = m.config.RepoName
			session.ctx = ctx
			session.cancel = cancel

			m.mu.Lock()
			m.sessions[session.ID] = session
			m.models[session.ID] = sessionmodel.NewSessionModel(1000)
			m.mu.Unlock()

			m.outputsMu.Lock()
			m.outputs[session.ID] = make([]OutputLine, 0, 16)
			m.outputsMu.Unlock()

			// Emit rather than calling updateSessionStatus, so this re-adoption
			// path has no side-effects on StartedAt or the other fields that
			// a real transition would touch.
			//
			// Through emitSessionStateChange, so state subscribers see it too:
			// a re-adopted session that is already idle makes no further
			// transition, and a subscriber would otherwise never learn its
			// status.
			m.emitSessionStateChange(SessionStateChangeEvent{
				Info:      session.ToInfo(),
				SessionID: session.ID,
				OldStatus: stored.Status,
				NewStatus: stored.Status,
			})

			// Monitor the window lifecycle
			if IsInsideTmux() && IsTmuxAvailable() {
				m.wg.Add(1)
				go m.monitorTrackedTmuxWindow(session)
			}
		}
	}

	return nil
}

// ReposNeedingTmuxReconcile returns repo names (other than activeRepo) holding
// a tmux session that has not reached a terminal state. The caller auto-opens
// them so a Manager re-adopts their sessions.
//
// Deliberately not "repos with a *live* window". This probe has no manager, and
// so nothing subscribed to its transitions, which is why it mutates nothing:
// marking a dead session completed here would spend that transition with
// nothing listening. That leaves ReconcileTmuxSessions to make the
// transition and emit it — and ReconcileTmuxSessions only runs for a repo that
// gets opened. So a repo whose only subagent died while bramble was down has to
// be returned too, or the parent is never told, which is the whole point of
// leaving the session alone.
func ReposNeedingTmuxReconcile(store *Store, activeRepo string, mode SessionMode) []string {
	if store == nil {
		return nil
	}
	// Both of ReconcileTmuxSessions' environmental guards, because naming a repo
	// it will decline to reconcile is worse than saying nothing: the repo is
	// opened — a manager and its goroutines, and a sidebar entry — on every
	// startup, and since nothing there will ever mark those sessions terminal,
	// it happens again forever. In TUI mode a tmux session record is simply not
	// this process's to settle.
	if ResolveSessionMode(mode) != SessionModeTmux {
		return nil
	}
	if !IsInsideTmux() || !IsTmuxAvailable() {
		return nil
	}
	return reposWithUnreconciledTmuxSessions(store, activeRepo)
}

// reposWithUnreconciledTmuxSessions is the store scan behind
// ReposNeedingTmuxReconcile, split out so it can be tested without a tmux
// server: the caller's guard is the only part that needs one.
func reposWithUnreconciledTmuxSessions(store *Store, activeRepo string) []string {
	repos, err := store.ListRepos()
	if err != nil {
		return nil
	}

	var needing []string
	for _, repo := range repos {
		if repo == activeRepo {
			continue // handled by the active manager's ReconcileTmuxSessions
		}
		if repoHasUnreconciledTmuxSession(store, repo) {
			needing = append(needing, repo)
		}
	}

	return needing
}

// repoHasUnreconciledTmuxSession reports whether a repo holds a tmux session
// still owed a terminal transition. Decided from the session metadata alone —
// a dead window and a live one both need the repo opened, so there is nothing
// left for a window-alive check to decide.
func repoHasUnreconciledTmuxSession(store *Store, repo string) bool {
	worktrees, err := store.ListWorktrees(repo)
	if err != nil {
		return false
	}
	for _, wtName := range worktrees {
		sessions, err := store.ListSessions(repo, wtName)
		if err != nil {
			continue
		}
		for _, meta := range sessions {
			if isTmuxRunner(meta.RunnerType) && !meta.Status.IsTerminal() {
				return true
			}
		}
	}
	return false
}

// Close shuts down the manager and all sessions.
func (m *Manager) Close() {
	// Persist all active tmux sessions BEFORE canceling so that goroutines
	// (monitorTrackedTmuxWindow, runSession) have not yet transitioned the
	// status to StatusStopped — ReconcileTmuxSessions skips terminal sessions.
	if m.config.SessionMode == SessionModeTmux && m.config.Store != nil {
		m.mu.RLock()
		sessions := make([]*Session, 0, len(m.sessions))
		for _, s := range m.sessions {
			sessions = append(sessions, s)
		}
		m.mu.RUnlock()
		for _, s := range sessions {
			m.persistSession(s)
		}
	}

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
// model is the AgentModel ID (e.g. "opus", "gpt-5.5"). If empty,
// defaults to "opus" for planners and "sonnet" for builders.
func (m *Manager) StartSession(sessionType SessionType, worktreePath, prompt, model string) (SessionID, error) {
	return m.StartSessionWithOpts(sessionType, worktreePath, prompt, model, SpawnOpts{})
}

// SpawnOpts carries the optional attributes of a new session that only some
// callers set. The zero value means a plain, top-level session, so callers with
// nothing to say can keep using StartSession.
type SpawnOpts struct { //nolint:govet // fieldalignment: keep endpoint next to its backend.
	// ParentSessionID makes the new session a subagent of that parent: its
	// completion is reported back there. Leave empty for a top-level session.
	//
	// The delegator deliberately does NOT set this. It runs its own child
	// watcher (watchChildSessionChanges) and would otherwise be told about
	// every child transition twice.
	ParentSessionID SessionID
	// Backend selects the CLI independently from the model ID. This matters for
	// gateways where one model is reachable through more than one CLI/wire API.
	Backend     string
	LLMEndpoint llmendpoint.Endpoint
}

// StartSessionWithOpts is StartSession with the optional attributes in SpawnOpts.
func (m *Manager) StartSessionWithOpts(sessionType SessionType, worktreePath, prompt, model string, opts SpawnOpts) (SessionID, error) {
	worktreeName := filepath.Base(worktreePath)
	sessionID := generateSessionID(worktreeName, sessionType)
	return m.startSessionWithID(sessionID, sessionType, worktreePath, worktreeName, prompt, model, opts)
}

// startSessionWithID starts a session using a caller-supplied session ID.
// This allows callers (e.g. DelegatorToolHandler) to pre-register the ID
// before spawning the child goroutine, closing the window where a very fast
// state transition could be missed by watchChildSessionChanges.
func (m *Manager) startSessionWithID(sessionID SessionID, sessionType SessionType, worktreePath, worktreeName, prompt, model string, opts SpawnOpts) (SessionID, error) {
	ctx, cancel := context.WithCancel(m.ctx)

	backend := opts.Backend
	if err := validateBackend(backend); err != nil {
		cancel()
		return "", err
	}
	// Resolve the backend before defaulting the model. An explicit backend
	// means the model ID is the backend's own (an OpenRouter slug, say), so
	// there is no sensible default to fall back to; substituting "sonnet"
	// here would launch `codex -m sonnet` against the endpoint and surface a
	// remote 400 instead of naming the missing flag. Leaving the model empty
	// lets resolveAgentModel's guard fire with that name.
	if model == "" && backend == "" {
		switch sessionType {
		case SessionTypePlanner, SessionTypeCodeTalk:
			model = "opus"
		default:
			model = "sonnet"
		}
	}
	endpoint := opts.LLMEndpoint
	if err := endpoint.Validate(); err != nil {
		cancel()
		return "", err
	}
	// validateEndpointBackend is a no-op on an empty backend, but runSession
	// re-runs it against the *resolved* provider -- so without this, an
	// endpoint on a gemini/cursor/agy model id with no --backend passed every
	// check here, printed a session ID, and only failed in the background.
	// That is the exact split the duplicate checks below exist to close, and
	// startSessionWithID already holds the registry needed to close it.
	endpointBackend := backend
	if endpointBackend == "" && !endpoint.IsZero() {
		agentModel, err := resolveAgentModel(model, "", m.config.ModelRegistry)
		if err != nil {
			cancel()
			return "", err
		}
		endpointBackend = agentModel.Provider
	}
	if err := validateEndpointBackend(endpoint, endpointBackend); err != nil {
		cancel()
		return "", err
	}
	// Fail here as well as in runSession so `bramble new-session` gets the
	// error on stderr instead of a session ID followed by a background
	// failure. runSession carries the same check for the resume path.
	if err := validateEndpointCredential(endpoint); err != nil {
		cancel()
		return "", err
	}
	if err := validatePersistableEndpoint(endpoint, m.config.Store != nil); err != nil {
		cancel()
		return "", err
	}
	// Reported here rather than left to resolveAgentModel inside runSession, so
	// the operator sees it at the command line.
	if err := validateBackendModel(backend, model); err != nil {
		cancel()
		return "", err
	}

	session := &Session{
		ID:              sessionID,
		Type:            sessionType,
		Status:          StatusPending,
		WorktreePath:    worktreePath,
		WorktreeName:    worktreeName,
		Prompt:          prompt,
		Title:           generateTitle(prompt, 20),
		Model:           model,
		Backend:         backend,
		LLMEndpoint:     endpoint.Clone(),
		RepoName:        m.config.RepoName,
		ParentSessionID: opts.ParentSessionID,
		Progress:        &SessionProgress{LastActivity: time.Now()},
		CreatedAt:       time.Now(),
		ctx:             ctx,
		cancel:          cancel,
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

// ResumeSession resumes a stopped/completed/failed session using --resume.
// It reuses the same bramble session ID and passes the CLI session ID to
// the runner so the Claude conversation continues where it left off.
// The prompt is sent as the first message in the resumed conversation.
// If the session isn't in memory (e.g. a historical session), it is
// re-hydrated from the stored data.
func (m *Manager) ResumeSession(id SessionID, prompt string) error {
	m.mu.Lock()
	session, ok := m.sessions[id]
	m.mu.Unlock()

	if !ok {
		// Try to re-hydrate from stored history
		session, ok = m.rehydrateSession(id)
		if !ok {
			return fmt.Errorf("session %s not found", id)
		}
	}

	// Combine status read, validation, and state update under a single lock to
	// prevent TOCTOU races where two concurrent callers both read a terminal
	// status and both proceed to start a new runner.
	ctx, cancel := context.WithCancel(m.ctx)
	var (
		cliSessionID string
		resumeErr    error
	)
	func() {
		session.mu.Lock()
		defer session.mu.Unlock()

		cliSessionID = session.CLISessionID
		if cliSessionID == "" {
			cancel()
			resumeErr = fmt.Errorf("session has no CLI session ID — cannot resume")
			return
		}

		switch session.Status {
		case StatusCompleted, StatusFailed, StatusStopped:
			// OK to resume — reset state while still holding the lock.
			session.Status = StatusPending
			session.Error = nil
			session.CompletedAt = nil
			session.ctx = ctx
			session.cancel = cancel
		default:
			cancel()
			resumeErr = fmt.Errorf("session is %s — can only resume completed/failed/stopped sessions", session.Status)
		}
	}()

	if resumeErr != nil {
		return resumeErr
	}

	// Re-initialize the session model and output buffer
	m.mu.Lock()
	m.models[id] = sessionmodel.NewSessionModel(1000)
	m.mu.Unlock()

	m.outputsMu.Lock()
	m.outputs[id] = make([]OutputLine, 0, 1000)
	m.outputsMu.Unlock()

	// Truncate to 12 chars for display only; avoid slicing short IDs.
	displayID := cliSessionID
	if len(displayID) > 12 {
		displayID = displayID[:12]
	}
	m.addOutput(id, OutputLine{
		Timestamp: time.Now(),
		Type:      OutputTypeStatus,
		Content:   fmt.Sprintf("Resuming session (CLI session: %s)...", displayID),
	})

	m.wg.Add(1)
	go m.runSession(session, prompt)

	return nil
}

// rehydrateSession loads a session from the store and adds it back to the
// in-memory sessions map. Returns the session and true if found.
func (m *Manager) rehydrateSession(id SessionID) (*Session, bool) {
	if m.config.Store == nil || m.config.RepoName == "" {
		return nil, false
	}

	// We need the worktree name to load from the store.
	// Try all worktrees to find the session.
	worktrees, err := m.config.Store.ListWorktrees(m.config.RepoName)
	if err != nil {
		return nil, false
	}

	for _, wt := range worktrees {
		stored, err := m.config.Store.LoadSession(m.config.RepoName, wt, id)
		if err != nil {
			continue
		}

		// Re-create the live session from stored data.
		// Do not allocate a context here — the session is in a terminal state
		// (completed/failed/stopped) and ResumeSession will set ctx/cancel
		// before running. Allocating one here would leak it immediately.
		session := storedToSession(stored)

		m.mu.Lock()
		m.sessions[id] = session
		m.mu.Unlock()

		return session, true
	}

	return nil, false
}

// storedToSession rebuilds the in-memory Session a persisted record describes.
//
// One function rather than a literal at each restore site. Both callers —
// ReconcileTmuxSessions re-adopting a live tmux window, and rehydrateSession
// loading a terminal session for resume — restore the same fields, and the two
// copies had already drifted once. What makes a shared helper worth it here is
// that a field dropped on restore is silent: Backend is fail-loud (an
// uncurated model id fails to resolve), but a dropped LLMEndpoint is
// indistinguishable from a session that never had one, because every
// validateEndpoint* guard short-circuits on IsZero. The session simply runs
// against the default provider with the user's own credentials.
//
// Callers add what is theirs: the tmux window identity and a context for a
// re-adopted session, nothing for one awaiting ResumeSession.
func storedToSession(stored *StoredSession) *Session {
	session := &Session{
		ID:              stored.ID,
		Type:            stored.Type,
		Status:          stored.Status,
		WorktreePath:    stored.WorktreePath,
		WorktreeName:    stored.WorktreeName,
		Prompt:          stored.Prompt,
		Title:           stored.Title,
		Model:           stored.Model,
		Backend:         stored.Backend,
		RepoName:        stored.RepoName,
		CLISessionID:    stored.CLISessionID,
		ParentSessionID: stored.ParentSessionID,
		CreatedAt:       stored.CreatedAt,
		StartedAt:       stored.StartedAt,
		CompletedAt:     stored.CompletedAt,
		Progress:        &SessionProgress{LastActivity: time.Now()},
	}
	if stored.LLMEndpoint != nil {
		session.LLMEndpoint = stored.LLMEndpoint.Clone()
	}
	return session
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
		RunnerType:     RunnerTypeTmuxTracked,
		RepoName:       m.config.RepoName,
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

	captureTicker := time.NewTicker(15 * time.Second)
	defer captureTicker.Stop()

	// captureRecentOutput grabs the last lines from the tmux pane and updates
	// the session's RecentOutput and status bar fields so the command center
	// can display rich information about the session.
	var captureState trackedTmuxCaptureState
	captureRecentOutput := func() {
		session.mu.RLock()
		wID := session.TmuxWindowID
		wName := session.TmuxWindowName
		statusAtCapture := session.Status
		session.mu.RUnlock()

		if statusAtCapture != StatusRunning && statusAtCapture != StatusIdle {
			return
		}
		target := SessionInfo{TmuxWindowID: wID, TmuxWindowName: wName}.TmuxTarget()
		if target == "" {
			return
		}
		// Use cursor-based capture for precise status bar location.
		// Falls back to legacy capture+scan if cursor detection fails.
		lines, cursorY, err := CaptureTmuxPaneFull(target)
		var paneStatus *PaneStatus
		if err == nil && len(lines) > 0 {
			paneStatus = ParseClaudeStatusBarWithCursor(lines, cursorY)
		}
		if paneStatus == nil {
			// Fallback: use legacy bottom-N capture + separator scanning.
			legacyLines, legacyErr := CaptureTmuxPane(target, 30)
			if legacyErr != nil {
				return
			}
			lines = legacyLines
			paneStatus = ParseClaudeStatusBar(lines)
		}

		// Extract meaningful content lines, stripping TUI chrome.
		contentLines := ContentLines(lines, paneStatus)
		contentChanged := false
		if paneStatus != nil {
			contentChanged = captureState.observeContentLines(contentLines)
		}

		if len(contentLines) > sessionmodel.RecentOutputDisplayLines {
			contentLines = contentLines[len(contentLines)-sessionmodel.RecentOutputDisplayLines:]
		}

		session.Progress.Update(func(p *SessionProgress) {
			p.RecentOutput = contentLines
			if contentChanged {
				p.LastActivity = time.Now()
			}
			if paneStatus != nil {
				p.StatusLine = paneStatus.StatusLine
				if paneStatus.Model != "" {
					p.CurrentPhase = paneStatus.Model + " ctx:" + paneStatus.ContextPct
				}
				if paneStatus.IsWorking && paneStatus.StatusLine != "" {
					p.CurrentTool = paneStatus.StatusLine
				} else {
					p.CurrentTool = ""
				}
			}
		})

		// If pane shows working state but session is idle, transition back to running.
		if shouldReviveIdleTmuxSession(statusAtCapture, paneStatus, contentChanged) {
			m.tryUpdateSessionStatus(session, StatusIdle, StatusRunning)
		}

		// Update session model if parsed status indicates idle
		if paneStatus != nil && paneStatus.Model != "" {
			session.mu.Lock()
			if session.Model == "" {
				session.Model = paneStatus.Model
			}
			session.mu.Unlock()
		}
	}

	// A re-adopted session reaches this loop instead of runSession's, so its
	// hookless backend needs the same pane-idle polling here or a restart
	// permanently strands it. The provider comes from the stored model: there is
	// no live agentModel on the re-adopt path.
	session.mu.RLock()
	storedModel := session.Model
	storedBackend := session.Backend
	session.mu.RUnlock()
	idleTracker := m.newPaneIdleTrackerForModel(storedModel, storedBackend)

	// Do an initial capture so the command center has data immediately.
	captureRecentOutput()

	for {
		select {
		case <-session.ctx.Done():
			// Window cleanup is handled in Close() if TmuxExitOnQuit is enabled.
			// This ensures cleanup only happens on app quit, not on individual session stops.
			m.updateSessionStatus(session, StatusStopped)
			// If the manager is still alive (not a Close), this was an explicit
			// StopSession call. Persist the stopped status so it won't be
			// re-adopted on restart.
			if m.ctx.Err() == nil {
				m.persistSession(session)
				m.mu.Lock()
				delete(m.sessions, session.ID)
				delete(m.models, session.ID)
				m.mu.Unlock()
				m.outputsMu.Lock()
				delete(m.outputs, session.ID)
				m.outputsMu.Unlock()
			}
			return
		case <-captureTicker.C:
			captureRecentOutput()
		case <-ticker.C:
			session.mu.RLock()
			windowID := session.TmuxWindowID
			windowName := session.TmuxWindowName
			sessionID := session.ID
			runnerType := session.RunnerType
			session.mu.RUnlock()

			if windowID == "" && windowName == "" {
				// No identification available — assume still alive to avoid false completions
				continue
			}

			windowAlive := tmuxWindowAlive(windowID, windowName)

			// For RunnerTypeTmux sessions (not tmux-tracked), the window persists
			// after process exit due to remain-on-exit. Detect pane-dead so that
			// re-adopted RunnerTypeTmux sessions complete properly.
			if windowAlive && runnerType == RunnerTypeTmux {
				windowTarget := windowName
				if windowID != "" {
					windowTarget = windowID
				} else if TmuxWindowExists(TmuxNotifyPrefix + windowName) {
					// Window was renamed by NotifyTmuxWindow; use the current
					// name so pane-dead/exit-status checks find the window.
					windowTarget = TmuxNotifyPrefix + windowName
				}

				// Clear notification prefix when user is viewing this window.
				// This must be inside the RunnerTypeTmux branch because only
				// tmux-mode sessions receive the Notification hook.
				session.mu.RLock()
				status := session.Status
				turnEpoch := session.turnEpoch
				session.mu.RUnlock()

				m.pollPaneIdle(idleTracker, sessionID, status, turnEpoch, windowTarget)

				if status == StatusIdle {
					if IsSessionWindowActive(windowID, windowName) {
						// Use windowID as target when available; for re-adopted
						// sessions without an ID, target the renamed "!name" only
						// while it exists (after first clear the window is just windowName).
						clearTarget := windowTarget
						if windowID == "" {
							if TmuxWindowExists(TmuxNotifyPrefix + windowName) {
								clearTarget = TmuxNotifyPrefix + windowName
							} else {
								clearTarget = windowName
							}
						}
						ClearTmuxWindowNotification(clearTarget, windowName)
					}
				}

				if TmuxWindowPaneDead(windowTarget) {
					exitCode, gotStatus := TmuxWindowPaneExitStatus(windowTarget)
					if gotStatus && exitCode == 0 {
						// Clean exit — mark completed
						m.updateSessionStatus(session, StatusCompleted)
					} else {
						// Non-zero exit or couldn't read status — mark failed
						var err error
						if gotStatus {
							err = fmt.Errorf("claude process exited with code %d (window %q still open — check it for error details)", exitCode, windowName)
						} else {
							err = fmt.Errorf("claude process exited with unknown status (window %q still open — check it for error details)", windowName)
						}
						m.failSession(session, err)
						m.addOutput(sessionID, OutputLine{
							Timestamp: time.Now(),
							Type:      OutputTypeError,
							Content:   fmt.Sprintf("Session failed: claude exited in tmux window %q. Switch to that window to see the error.", windowName),
						})
					}

					m.persistSession(session)

					m.mu.Lock()
					delete(m.sessions, sessionID)
					delete(m.models, sessionID)
					m.mu.Unlock()

					m.outputsMu.Lock()
					delete(m.outputs, sessionID)
					m.outputsMu.Unlock()
					return
				}
				continue
			}

			// Prefer stable window ID; fall back to name for re-adopted sessions
			// that may not have a window ID.
			if windowAlive {
				continue
			}

			m.updateSessionStatus(session, StatusCompleted)

			// Persist before removing from memory so the completed status is written to disk.
			m.persistSession(session)

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

func resolveAgentModel(modelID, backend string, registry *agent.ModelRegistry) (agent.AgentModel, error) {
	if backend != "" {
		if err := validateBackend(backend); err != nil {
			return agent.AgentModel{}, err
		}
		if err := validateBackendModel(backend, modelID); err != nil {
			return agent.AgentModel{}, err
		}
		return agent.AgentModel{ID: modelID, Provider: backend, Label: modelID}, nil
	}
	if registry != nil {
		if m, ok := registry.ModelByID(modelID); ok {
			return m, nil
		}
	}
	if m, ok := agent.ResolveModel(modelID); ok {
		return m, nil
	}
	return agent.AgentModel{}, fmt.Errorf("unknown model %q: no curated entry and no recognized prefix (%s)", modelID, agent.KnownModelPrefixes())
}

// newPaneIdleTrackerForModel returns the pane-idle tracker a session needs,
// resolving its provider from the model it was created with.
//
// Both tmux monitor loops go through this — the one runSession runs for a
// session it started, and monitorTrackedTmuxWindow for one re-adopted after a
// restart. That is the point: a backend with no completion hook (cursor) is
// only ever seen to finish because one of these loops reads its pane, and a
// loop that skips it leaves such a session running forever, with its queued
// mail undelivered and its parent never told it is done.
//
// nil for a provider that reports its own turn ends, and nil for a model that
// no longer resolves — an unrecognized model is not grounds for guessing at a
// pane's chrome.
//
// backend is the session's explicit --backend, empty when it has none. It has
// to come along: with one set, the model ID is that backend's own (a
// third-party slug the curated registry has never heard of), so resolving on
// the model alone would fail and hand back a nil tracker — silently turning a
// hookless backend into one that is never seen to finish.
func (m *Manager) newPaneIdleTrackerForModel(model, backend string) *paneIdleTracker {
	agentModel, err := resolveAgentModel(model, backend, m.config.ModelRegistry)
	if err != nil {
		return nil
	}
	return newPaneIdleTracker(agentModel.Provider)
}

func validateBackend(backend string) error {
	if backend != "" && !slices.Contains(agent.AllProviders, backend) {
		return fmt.Errorf("unknown backend %q (expected one of %s)", backend, strings.Join(agent.AllProviders, ", "))
	}
	return nil
}

// validateBackendModel rejects a (backend, model) pair that cannot launch.
//
// Two ways it cannot. An explicit backend means the model ID is that backend's
// own — an OpenRouter slug, say — so an empty model has no default to fall back
// to; bramble's own defaults are Claude aliases. And a model ID the curated
// registry assigns to a *different* provider is a user error, not a request for
// that CLI's default: agent.CLIModelArg drops the flag entirely in that case
// (model_registry.go:47-49), so `--backend codex --model opus` would launch
// codex with no -m at all, running codex's default while bramble recorded and
// displayed "opus". The same split would let endpointEnv pin ANTHROPIC_MODEL
// from the raw ID while --model carried the filtered one.
//
// Checked against the global registry rather than the manager's filtered one
// because that is the registry CLIModelArg consults; validating against a
// different set than the consumer uses is how the two drift apart. An ID absent
// from the registry is fine — carrying third-party IDs is what --backend is for.
//
// Called from startSessionWithID (so the CLI gets a synchronous error) and from
// resolveAgentModel (so every other caller is covered too). One function rather
// than a check in each: this is one rule about one pair.
func validateBackendModel(backend, model string) error {
	if backend == "" {
		return nil
	}
	if model == "" {
		return fmt.Errorf("model must not be empty when backend %q is selected", backend)
	}
	if m, ok := agent.ModelByID(model); ok && m.Provider != backend {
		return fmt.Errorf("model %q belongs to backend %q, not the requested backend %q: pass a model id that backend serves, or drop --backend", model, m.Provider, backend)
	}
	return nil
}

// validatePersistableEndpoint rejects an endpoint this manager cannot faithfully
// reconstruct after a restart.
//
// SessionToStored persists via Endpoint.Redacted(), whose contract is
// log-safety, not persistence: it clears APIKey *and* drops Headers entirely.
// The credential survives that because APIKeyEnv re-resolves and
// validateEndpointCredential names the failure when it cannot. Headers have
// neither — a rehydrated endpoint simply has none, so a gateway that needs a
// routing or tenant header would work on first launch and fail opaquely on
// every resume, with the store's redaction test (which asserts only on APIKey)
// blind to it.
//
// Refusing up front is the cheaper half of the trade: persisting the header
// values instead would put arbitrary secrets in ~/.bramble/sessions/*.json,
// which is the posture this whole path exists to avoid. Headers are not
// reachable from `bramble new-session` — there is no --llm-header flag — so
// this can only fire for a direct IPC caller that sets them, and it tells that
// caller the constraint at the point it can still act on it.
func validatePersistableEndpoint(endpoint llmendpoint.Endpoint, persists bool) error {
	if !persists || endpoint.IsZero() {
		return nil
	}
	// Two things Redacted() removes, so two ways an endpoint can fail to
	// survive its own session. The credential normally survives because
	// APIKeyEnv re-resolves; an inline-only key has nothing to re-resolve
	// from, so the session launches and then cannot be resumed.
	if endpoint.APIKey != "" && endpoint.APIKeyEnv == "" {
		return errors.New("llm endpoint carries an inline api key with no APIKeyEnv, which is not persisted (Redacted drops it) and cannot be re-resolved on resume: set APIKeyEnv so the key can be recovered from the environment")
	}
	if len(endpoint.Headers) > 0 {
		return fmt.Errorf("llm endpoint sets %d http header(s), which are not persisted (Redacted drops them) and would silently vanish on resume: drop the headers, or run this endpoint through the in-process path", len(endpoint.Headers))
	}
	return nil
}

// validateEndpointCredential rejects an endpoint whose key resolves to nothing.
//
// Only the env var *name* is guaranteed to cross the IPC boundary from
// `bramble new-session`; this process is the one that reads it, so a TUI
// started before the key was exported resolves nothing. Resume is the same
// hole reached by a different route: SessionToStored persists the endpoint via
// Redacted(), so a rehydrated session has APIKey=="" and re-resolves through
// APIKeyEnv. Either way the wrappers would omit the auth headers while still
// setting the base URL, and the window would talk to the endpoint with the
// user's own credentials and fail on an opaque remote 401.
//
// Called from startSessionWithID (so the CLI gets a synchronous error) and
// from runSession (so ResumeSession, the other caller, is covered too). One
// function rather than two copies: this is one rule about one invariant.
func validateEndpointCredential(endpoint llmendpoint.Endpoint) error {
	if endpoint.IsZero() || endpoint.ResolvedKey() != "" {
		return nil
	}
	return fmt.Errorf("llm endpoint api key is unset: %s holds no value in the bramble server's environment (export it before starting bramble, or start a new session with --llm-api-key-env, which resolves the key in the client)", endpointKeySource(endpoint))
}

// endpointKeySource names where the endpoint's credential was meant to come
// from, so an unresolved key reports the env var the operator actually typed
// rather than a generic "no key". An inline APIKey can never be the unresolved
// case (ResolvedKey returns it verbatim), so the fallback covers only an
// endpoint that named neither — which Validate already rejects.
func endpointKeySource(endpoint llmendpoint.Endpoint) string {
	if endpoint.APIKeyEnv != "" {
		return "$" + endpoint.APIKeyEnv
	}
	return "the endpoint's api key"
}

func validateEndpointBackend(endpoint llmendpoint.Endpoint, backend string) error {
	if endpoint.IsZero() || backend == "" || backend == ProviderClaude {
		return nil
	}
	if backend != ProviderCodex {
		return fmt.Errorf("LLM endpoints support only %q and %q tmux backends, got %q", ProviderClaude, ProviderCodex, backend)
	}
	if endpoint.WireAPI() != llmendpoint.WireAPIResponses {
		return fmt.Errorf("codex requires llm-wire-api=%q; %q is no longer supported", llmendpoint.WireAPIResponses, endpoint.WireAPI())
	}
	return nil
}

// plannerConfigFor, builderConfigFor and codeTalkConfigFor build the wrapper
// configs for the default (non-provider) TUI branch of runSession.
//
// Split out of the switch so the session -> config carry can be asserted
// without launching a real CLI. Each is the only place a session's model and
// endpoint reach its wrapper, and the two travel together for the reason the
// provider branch states: claude.WithLLMEndpoint skips the ANTHROPIC_MODEL and
// ANTHROPIC_DEFAULT_* side-call pins when Model is empty. A dropped endpoint is
// also indistinguishable from no endpoint -- the wrapper just runs against the
// default provider with the user's own credentials -- so while these were
// inline literals, deleting any LLMEndpoint line left the whole suite green.
// TestManager_TUIWrapperConfigsCarrySessionEndpoint now fails if one goes.
func (m *Manager) plannerConfigFor(session *Session, eventHandler *sessionEventHandler) planner.Config {
	return planner.Config{
		Model:           session.Model,
		LLMEndpoint:     session.LLMEndpoint.Clone(),
		WorkDir:         session.WorktreePath,
		Simple:          true,
		BuildMode:       planner.BuildModeReturn,
		Output:          io.Discard,
		EventHandler:    eventHandler,
		ResumeSessionID: session.CLISessionID,
		RecordingDir:    m.config.RecordingDir,
	}
}

func (m *Manager) builderConfigFor(session *Session) yoloswe.BuilderConfig {
	return yoloswe.BuilderConfig{
		Model:           session.Model,
		LLMEndpoint:     session.LLMEndpoint.Clone(),
		WorkDir:         session.WorktreePath,
		ResumeSessionID: session.CLISessionID,
		RecordingDir:    m.config.RecordingDir,
	}
}

func (m *Manager) codeTalkConfigFor(session *Session) yoloswe.CodeTalkConfig {
	return yoloswe.CodeTalkConfig{
		Model:           session.Model,
		LLMEndpoint:     session.LLMEndpoint.Clone(),
		WorkDir:         session.WorktreePath,
		ResumeSessionID: session.CLISessionID,
		RecordingDir:    m.config.RecordingDir,
	}
}

// newTmuxRunner builds the runner for a tmux-mode session, copying the
// manager's configured sockets into the runner that exports them to the
// window. It is a method rather than inline construction so the
// config → runner → env plumbing can be asserted without a live tmux server:
// dropping a socket here is what silently strands a session, and envArgs()
// alone cannot catch it.
func (m *Manager) newTmuxRunner(session *Session, prompt, tmuxName string, agentModel agent.AgentModel) *tmuxRunner {
	permissionMode := ""
	if session.Type == SessionTypePlanner || session.Type == SessionTypeCodeTalk {
		permissionMode = "plan"
	}

	// selfexec.Path() rather than os.Executable(): this is baked into the
	// session's Claude Stop-hook argv for the window's whole lifetime, and a
	// lazy os.Executable() returns "<path> (deleted)" once someone rebuilds the
	// binary underneath a running bramble.
	brambleBin := selfexec.Path()
	if brambleBin == "" {
		brambleBin = "bramble" // fallback to PATH lookup
	}
	return &tmuxRunner{
		windowName:      tmuxName,
		workDir:         session.WorktreePath,
		prompt:          prompt,
		model:           agentModel.ID,
		provider:        agentModel.Provider,
		permissionMode:  permissionMode,
		resumeSessionID: session.CLISessionID,
		sessionID:       string(session.ID),
		brambleBin:      brambleBin,
		brambleSock:     m.config.IPCSockPath,
		controlSock:     m.config.ControlSockPath,
		llmEndpoint:     session.LLMEndpoint.Clone(),
		yoloMode:        m.config.YoloMode,
		killOnStop:      false, // Never kill on Stop(); cleanup happens in Close() if TmuxExitOnQuit is set
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
	agentModel, resolveErr := resolveAgentModel(session.Model, session.Backend, m.config.ModelRegistry)
	if resolveErr != nil {
		m.failSession(session, resolveErr)
		m.addOutput(session.ID, OutputLine{
			Timestamp: time.Now(),
			Type:      OutputTypeError,
			Content:   resolveErr.Error(),
		})
		m.persistSession(session)
		return
	}
	if err := validateEndpointBackend(session.LLMEndpoint, agentModel.Provider); err != nil {
		m.failSession(session, err)
		m.addOutput(session.ID, OutputLine{Timestamp: time.Now(), Type: OutputTypeError, Content: err.Error()})
		m.persistSession(session)
		return
	}
	// Both callers of runSession pass through here; ResumeSession does not go
	// through startSessionWithID, so this is the only place a resumed endpoint
	// session's credential is checked.
	if err := validateEndpointCredential(session.LLMEndpoint); err != nil {
		m.failSession(session, err)
		m.addOutput(session.ID, OutputLine{Timestamp: time.Now(), Type: OutputTypeError, Content: err.Error()})
		m.persistSession(session)
		return
	}

	// If a registry is configured and the model's provider is not available,
	// fail early with a clear message.
	if m.config.ModelRegistry != nil && !m.config.ModelRegistry.HasProvider(agentModel.Provider) {
		err := fmt.Errorf("provider %q is not available (not installed or disabled in settings)", agentModel.Provider)
		m.failSession(session, err)
		m.addOutput(session.ID, OutputLine{
			Timestamp: time.Now(),
			Type:      OutputTypeError,
			// Name the binary in the advice, not in the %q slot: HasProvider is
			// installed AND enabled, so this also fires for a CLI the user has
			// but switched off — telling them to install `cursor` would be
			// wrong, while naming `agent` is right either way.
			Content: fmt.Sprintf("Provider %q is not available. Install %s or enable it in settings.",
				agentModel.Provider, agent.BinaryForProvider(agentModel.Provider)),
		})
		m.persistSession(session)
		return
	}

	// Delegator sessions require the default Claude SDK path.  They are not
	// supported in tmux mode, with a pluggable provider, or with a non-Claude
	// model provider.  Fail fast so the problem is obvious instead of silently
	// falling through to a providerRunner that has no delegator tools.
	if session.Type == SessionTypeDelegator {
		unsupported := ""
		switch {
		case m.config.SessionMode == SessionModeTmux:
			unsupported = "tmux session mode"
		case m.config.Provider != nil:
			unsupported = "pluggable provider backend"
		case agentModel.Provider != ProviderClaude:
			unsupported = fmt.Sprintf("provider %q (only Claude is supported for delegator sessions)", agentModel.Provider)
		case !session.LLMEndpoint.IsZero():
			// The fourth arm of the switch below, and the only one that does
			// not carry the endpoint into its runner: delegatorRunner builds
			// its claude.Session from DelegatorBaseSessionOpts, which has no
			// endpoint seam, and its spawned children get their own sessions
			// with no endpoint either. Dropping it silently would run the
			// session against the default provider with the user's own
			// credentials -- a zero endpoint and a discarded one are
			// indistinguishable downstream, since every validate* guard
			// short-circuits on IsZero. Refuse instead, so adding endpoint
			// support to the delegator is a deliberate act rather than the
			// repair of a silent fallback.
			unsupported = "a per-session LLM endpoint"
		}
		if unsupported != "" {
			err := fmt.Errorf("delegator sessions are not supported with %s", unsupported)
			m.failSession(session, err)
			m.addOutput(session.ID, OutputLine{
				Timestamp: time.Now(),
				Type:      OutputTypeError,
				Content:   fmt.Sprintf("Delegator session requires the Claude SDK backend; %s is not supported.", unsupported),
			})
			m.persistSession(session)
			return
		}
	}

	if m.config.SessionMode == SessionModeTmux {
		// Tmux mode: create tmux window running the agent CLI
		tmuxName := GenerateTmuxWindowName(m.config.RepoName, session.WorktreeName)
		session.mu.Lock()
		session.TmuxWindowName = tmuxName
		session.RunnerType = RunnerTypeTmux
		session.mu.Unlock()

		runner = m.newTmuxRunner(session, prompt, tmuxName, agentModel)
		// No event handler for tmux mode - all output is in the tmux window
	} else {
		// TUI mode: create in-process runner
		session.mu.Lock()
		session.RunnerType = RunnerTypeTUI
		session.mu.Unlock()

		eventHandler = newSessionEventHandler(m, session.ID)

		if m.config.Provider != nil {
			// Use the pluggable provider backend. model and workDir are set
			// here for the same reason the four branches below set them: a
			// third-party endpoint only works when the model travels with it —
			// claude.WithLLMEndpoint skips the ANTHROPIC_MODEL and
			// ANTHROPIC_DEFAULT_* side-call pins when Model is empty, and codex
			// gets no -m. This branch is test-only today (nothing outside tests
			// sets ManagerConfig.Provider), which is precisely why it mattered:
			// it is the branch the manager-level endpoint tests run through, so
			// an omission here made those tests unable to observe the pairing.
			runner = &providerRunner{
				provider:     m.config.Provider,
				eventHandler: eventHandler,
				model:        session.Model,
				workDir:      session.WorktreePath,
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
					if session.Type == SessionTypePlanner || session.Type == SessionTypeCodeTalk {
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
			if session.Type == SessionTypePlanner || session.Type == SessionTypeCodeTalk {
				// Planner/codetalk sessions should only be able to read, not write
				clientOpts = append(clientOpts, acp.WithPermissionHandler(&acp.PlanOnlyPermissionHandler{}))
			}
			// Builder sessions use the default BypassPermissionHandler (auto-approve all)

			runner = &providerRunner{
				provider:     agent.NewGeminiLongRunningProvider(clientOpts, acp.WithSessionCWD(session.WorktreePath)),
				eventHandler: eventHandler,
				model:        session.Model,
				workDir:      session.WorktreePath,
			}
		} else if agentModel.Provider == ProviderCursor {
			// Cursor provider backend
			runner = &providerRunner{
				provider:     agent.NewCursorProvider(),
				eventHandler: eventHandler,
				model:        session.Model,
				workDir:      session.WorktreePath,
			}
		} else if agentModel.Provider == ProviderAgy {
			// Antigravity provider backend
			runner = &providerRunner{
				provider:     agent.NewAgyProvider(),
				eventHandler: eventHandler,
				model:        session.Model,
				permissionMode: func() string {
					if session.Type == SessionTypePlanner || session.Type == SessionTypeCodeTalk {
						return "plan"
					}
					return "bypass"
				}(),
				workDir: session.WorktreePath,
			}
		} else {
			// Default: use hardcoded planner/builder runners with model from session
			switch session.Type {
			case SessionTypePlanner:
				runner = &plannerRunner{pw: planner.NewPlannerWrapper(m.plannerConfigFor(session, eventHandler))}
			case SessionTypeBuilder:
				builderHandler := newSessionEventHandlerNoTurnEnd(m, session.ID)
				runner = yoloswe.NewBuilderSessionWithEvents(m.builderConfigFor(session), nil, builderHandler)
			case SessionTypeDelegator:
				childModel := session.Model
				if m.config.ChildModel != "" {
					childModel = m.config.ChildModel
				}
				toolHandler := NewDelegatorToolHandler(m, session.WorktreePath, childModel, m.config.ModelRegistry)
				runner = &delegatorRunner{
					toolHandler:  toolHandler,
					eventHandler: eventHandler,
					worktreePath: session.WorktreePath,
					model:        session.Model,
					recordingDir: m.config.RecordingDir,
				}
			case SessionTypeCodeTalk:
				codetalkHandler := newSessionEventHandlerNoTurnEnd(m, session.ID)
				runner = yoloswe.NewCodeTalkSessionWithEvents(m.codeTalkConfigFor(session), nil, codetalkHandler)
			default:
				err := fmt.Errorf("unknown session type: %s", session.Type)
				m.failSession(session, err)
				m.addOutput(session.ID, OutputLine{
					Timestamp: time.Now(),
					Type:      OutputTypeError,
					Content:   err.Error(),
					IsError:   true,
				})
				return
			}
		}
	}

	// Attach the endpoint once, after the branch, rather than in each
	// providerRunner literal. Five branches build one, and this PR wired only
	// two of them — the other three are unreachable with a non-zero endpoint
	// today (validateEndpointBackend above rejects anything but claude and
	// codex), but nothing at those literals said so, and a later backend
	// joining the allowed set would have had to remember. Setting it here
	// makes forgetting impossible instead of merely unlikely. The non-provider
	// runners (planner/builder/codetalk wrappers) take the endpoint through
	// their own config structs in the default branch above.
	if pr, ok := runner.(*providerRunner); ok {
		pr.llmEndpoint = session.LLMEndpoint.Clone()
	}

	if err := runner.Start(session.ctx); err != nil {
		m.failSession(session, err)
		m.addOutput(session.ID, OutputLine{
			Timestamp: time.Now(),
			Type:      OutputTypeError,
			Content:   fmt.Sprintf("Failed to start session: %v", err),
		})
		m.persistSession(session)
		return
	}

	// Capture the CLI session ID right after Start() — the system{init}
	// message has already been processed so the ID is available immediately.
	if cliID := runner.CLISessionID(); cliID != "" {
		session.mu.Lock()
		session.CLISessionID = cliID
		session.mu.Unlock()
	}

	// For manager-started tmux sessions, capture the stable window ID from the
	// runner. tmuxRunner.Start() captures it atomically via "new-window -P -F
	// #{window_id}", so no post-hoc name lookup (and no TOCTOU race) is needed.
	// Fall back to TmuxWindowIDByName only if the runner didn't supply an ID
	// (e.g. non-tmux runners or sessions started before this field was added).
	if m.config.SessionMode == SessionModeTmux {
		session.mu.RLock()
		windowName := session.TmuxWindowName
		hasID := session.TmuxWindowID != ""
		session.mu.RUnlock()
		if !hasID {
			if tr, ok := runner.(*tmuxRunner); ok && tr.WindowID() != "" {
				session.mu.Lock()
				session.TmuxWindowID = tr.WindowID()
				session.mu.Unlock()
			} else if windowName != "" {
				if id, ok := TmuxWindowIDByName(windowName); ok {
					session.mu.Lock()
					session.TmuxWindowID = id
					session.mu.Unlock()
				}
			}
		}
	}

	// naturallyPersisted is set to true by the tmux natural-completion paths
	// (pane-dead clean exit, window-gone normal) that call persistSession and
	// then delete m.outputs[sessionID]. The defer below must not call
	// persistSession a second time in those cases, because m.outputs is already
	// gone and the second persist would overwrite the on-disk record with an
	// empty output slice.
	naturallyPersisted := false

	defer func() {
		runner.Stop()
		if m.config.SessionMode != SessionModeTmux {
			m.followUpChansMu.Lock()
			delete(m.followUpChans, session.ID)
			m.followUpChansMu.Unlock()
		}
		// In tmux mode, Close() persists sessions as StatusRunning before
		// canceling the context. Don't overwrite that with the StatusStopped
		// set by the ctx.Done handler above.
		// Only skip if the manager context is also canceled (meaning Close()
		// was called). If the manager is still alive, this is an explicit
		// StopSession call — persist the stopped status.
		if m.config.SessionMode == SessionModeTmux && m.ctx.Err() != nil {
			return
		}
		// Natural tmux completion paths already called persistSession before
		// removing m.outputs. Don't call it again with the now-empty slice.
		if naturallyPersisted {
			return
		}
		m.persistSession(session)
	}()

	// Tmux mode: just wait for the window to be stopped manually
	// The tmux window handles all interaction, so we don't run turns
	if m.config.SessionMode == SessionModeTmux {
		// Compare-and-set, not a plain write. The window has been up since
		// runner.Start(), which lingers ~100ms before returning, and the agent
		// inside can finish a turn and fire its notify hook in that gap. An
		// unconditional write here would overwrite that idle with Running and
		// leave the session stuck: SetSessionIdle only advances a Running
		// session, so the *next* notify — which never comes, because the agent
		// is waiting for input — is the only thing that could fix it.
		//
		// Status is already Running from the top of runSession, so this is a
		// no-op in the normal case and simply declines to move backwards.
		m.tryUpdateSessionStatus(session, StatusPending, StatusRunning)
		startTime := time.Now()

		// Pane probes cover cursor's hookless finish, codex's premature-idle
		// correction, and claude's fallback when Stop hooks cannot reach
		// bramble. Stored model/backend data keeps re-adopted sessions on the
		// same tracker path.
		idleTracker := m.newPaneIdleTrackerForModel(session.Model, session.Backend)

		// Periodically check if tmux window still exists
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		for {
			select {
			case <-session.ctx.Done():
				m.updateSessionStatus(session, StatusStopped)
				return
			case <-ticker.C:
				// Check if tmux window still exists. Prefer stable window ID
				// so renames don't cause false "window gone" completions.
				session.mu.RLock()
				tmuxName := session.TmuxWindowName
				tmuxID := session.TmuxWindowID
				sessionID := session.ID
				status := session.Status
				turnEpoch := session.turnEpoch
				session.mu.RUnlock()

				if tmuxName == "" && tmuxID == "" {
					continue
				}

				// windowTarget is the most stable identifier available for
				// pane-dead / exit-status queries (list-panes -t <target>).
				windowTarget := tmuxName
				if tmuxID != "" {
					windowTarget = tmuxID
				}

				m.pollPaneIdle(idleTracker, sessionID, status, turnEpoch, windowTarget)

				windowExists := tmuxWindowAlive(tmuxID, tmuxName)
				paneDead := windowExists && TmuxWindowPaneDead(windowTarget)

				if paneDead {
					// The process in the tmux window has exited but the window
					// remains (due to remain-on-exit). Check exit code to
					// determine whether it completed successfully or failed.
					exitCode, gotStatus := TmuxWindowPaneExitStatus(windowTarget)

					if gotStatus && exitCode == 0 {
						// Clean exit — session completed successfully
						m.updateSessionStatus(session, StatusCompleted)

						// Persist before removing from memory so output history is written to disk.
						m.persistSession(session)
						naturallyPersisted = true

						m.mu.Lock()
						delete(m.sessions, sessionID)
						delete(m.models, sessionID)
						m.mu.Unlock()

						m.outputsMu.Lock()
						delete(m.outputs, sessionID)
						m.outputsMu.Unlock()

						return
					}

					// Non-zero exit or couldn't read status — failure
					var err error
					if gotStatus {
						err = fmt.Errorf("claude process exited with code %d (window %q still open — check it for error details)", exitCode, tmuxName)
					} else {
						err = fmt.Errorf("claude process exited unexpectedly (window %q still open with remain-on-exit — check it for error details)", tmuxName)
					}
					m.failSession(session, err)
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
						err := fmt.Errorf("tmux window %q disappeared shortly after creation — claude may have failed to start", tmuxName)
						m.failSession(session, err)
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

					// Persist before removing from memory so output history is written to disk.
					m.persistSession(session)
					naturallyPersisted = true

					// Remove from active sessions map
					m.mu.Lock()
					delete(m.sessions, sessionID)
					delete(m.models, sessionID)
					m.mu.Unlock()

					// Remove outputs
					m.outputsMu.Lock()
					delete(m.outputs, sessionID)
					m.outputsMu.Unlock()

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

	// For delegator sessions, watch for child session state changes so the
	// delegator auto-resumes when a child goes idle, completes, or fails.
	var childNotifyChan <-chan SessionStateChangeEvent
	if dr, ok := runner.(*delegatorRunner); ok {
		ch := make(chan SessionStateChangeEvent, 10)
		childNotifyChan = ch
		unsub := watchChildSessionChanges(session.ctx, m, dr.toolHandler, ch)
		defer unsub()
	}

	currentPrompt := prompt
	for {
		turnStart := time.Now()
		usage, err := runner.RunTurn(session.ctx, currentPrompt)
		turnDurationMs := time.Since(turnStart).Milliseconds()
		if err != nil {
			if session.ctx.Err() != nil {
				m.updateSessionStatus(session, StatusStopped)
			} else {
				m.failSession(session, err)
				m.addOutput(session.ID, OutputLine{
					Timestamp: time.Now(),
					Type:      OutputTypeError,
					Content:   fmt.Sprintf("Session error: %v", err),
				})
			}
			return
		}

		if usage != nil {
			var turnCount int
			session.Progress.Update(func(p *SessionProgress) {
				p.TurnCount++
				turnCount = p.TurnCount
				p.TotalCostUSD += usage.CostUSD
				p.InputTokens += usage.InputTokens
				p.OutputTokens += usage.OutputTokens
				if usage.ContextWindow > 0 {
					p.ContextWindow = usage.ContextWindow
				}
				// Store last turn's total input for context utilization.
				// TotalInputTokens() = InputTokens + CacheCreationTokens + CacheReadTokens,
				// representing the full prompt size sent for this turn.
				if total := usage.TotalInputTokens(); total > 0 {
					p.LastTurnInputTotal = total
				}
			})

			// Emit TurnEnd synchronously so it's guaranteed to be in the
			// output buffer before StatusIdle. The async forwardEvents
			// goroutine may still be processing events from the SDK's
			// channel when RunTurn returns, causing a race where StatusIdle
			// arrives at consumers before TurnEnd.
			m.addOutput(session.ID, OutputLine{
				Timestamp:  time.Now(),
				Type:       OutputTypeTurnEnd,
				Content:    fmt.Sprintf("Turn %d complete", turnCount),
				TurnNumber: turnCount,
				CostUSD:    usage.CostUSD,
				DurationMs: turnDurationMs,
				IsError:    false,
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

		// After a codetalk turn: write research output to a file for delegator
		// consumption. Subagents get the same treatment whatever their type —
		// their parent is told they finished and handed this path, and a
		// backend like codex cannot be asked to produce a result file itself.
		// Without this a parent would have to scrape the child's pane.
		if session.Type == SessionTypeCodeTalk || session.parentSessionID() != "" {
			if researchPath, err := m.writeResearchFile(session); err == nil {
				session.mu.Lock()
				session.ResearchFilePath = researchPath
				session.mu.Unlock()
			}
		}

		m.updateSessionStatus(session, StatusIdle)

		// Prioritize child notifications over user follow-ups. When rapid
		// follow-ups arrive (e.g. multi-turn eval), Go's select picks
		// randomly between ready channels, so child completion events can
		// pile up in the buffer and never get processed. Drain all pending
		// child notifications first as a single combined prompt.
		var childNotifs []SessionStateChangeEvent
		if childNotifyChan != nil {
		drainChildNotifs:
			for {
				select {
				case notif := <-childNotifyChan:
					childNotifs = append(childNotifs, notif)
				default:
					break drainChildNotifs
				}
			}
		}
		if len(childNotifs) > 0 {
			var parts []string
			// Indexed, not ranged by value: the event carries a session
			// snapshot, so copying each one costs half a kilobyte per notif.
			for i := range childNotifs {
				parts = append(parts, fmt.Sprintf(
					"Child session %s status changed to %s.",
					childNotifs[i].SessionID, childNotifs[i].NewStatus))
			}
			currentPrompt = strings.Join(parts, "\n") + "\nUse get_session_progress to check details and decide next steps."
			m.updateSessionStatus(session, StatusRunning)
			continue
		}

		// No pending child notifications — block until a follow-up,
		// child notification, or context cancellation arrives.
		select {
		case <-session.ctx.Done():
			m.updateSessionStatus(session, StatusStopped)
			return
		case followUp, ok := <-followUpChan:
			if !ok {
				m.updateSessionStatus(session, StatusCompleted)
				return
			}
			// Update session prompt so command center shows the latest input.
			session.mu.Lock()
			session.Prompt = followUp
			session.mu.Unlock()
			m.updateSessionStatus(session, StatusRunning)
			now := time.Now()
			m.addOutput(session.ID, OutputLine{
				Timestamp: now,
				Type:      OutputTypeStatus,
				Content:   "Follow-up prompt:",
			})
			m.addOutput(session.ID, OutputLine{
				Timestamp:    now,
				Type:         OutputTypeText,
				Content:      followUp,
				IsUserPrompt: true,
			})
			currentPrompt = followUp
		case notif := <-childNotifyChan:
			currentPrompt = fmt.Sprintf(
				"Child session %s status changed to %s. Use get_session_progress to check details and decide next steps.",
				notif.SessionID, notif.NewStatus)
			m.updateSessionStatus(session, StatusRunning)
		}
	}
}

type trackedTmuxCaptureState struct {
	prevContentLines   []string
	haveContentCapture bool
}

func (s *trackedTmuxCaptureState) observeContentLines(contentLines []string) bool {
	if !s.haveContentCapture {
		s.prevContentLines = slices.Clone(contentLines)
		s.haveContentCapture = true
		return false
	}
	if slices.Equal(contentLines, s.prevContentLines) {
		return false
	}
	s.prevContentLines = slices.Clone(contentLines)
	return true
}

func shouldReviveIdleTmuxSession(statusAtCapture SessionStatus, paneStatus *PaneStatus, contentChanged bool) bool {
	if statusAtCapture != StatusIdle || paneStatus == nil {
		return false
	}
	return contentChanged || paneStatus.IsWorking
}

// updateSessionStatus updates session status and emits event.
func (m *Manager) updateSessionStatus(session *Session, newStatus SessionStatus) {
	session.mu.Lock()
	oldStatus := session.Status
	if !mayLeaveStatus(oldStatus, newStatus) {
		session.mu.Unlock()
		slog.Debug("suppressed status change for a session already terminal",
			"session", session.ID, "old_status", oldStatus, "new_status", newStatus)
		return
	}
	applySessionStatusLocked(session, oldStatus, newStatus)
	session.mu.Unlock()

	m.emitSessionStateChange(SessionStateChangeEvent{
		Info:      session.ToInfo(),
		SessionID: session.ID,
		OldStatus: oldStatus,
		NewStatus: newStatus,
	})
}

// mayLeaveStatus reports whether a session sitting at oldStatus may move to
// newStatus.
//
// Terminal is final. A session reaches completed/failed/stopped exactly when its
// window is gone or it was stopped, and every one of those paths then drops it
// from m.sessions — the map list-sessions reports. Any later move back to running
// or idle re-animates a session the orchestrator can no longer see, and
// Notifier.Watch hints to the parent about a session that no longer exists.
// That is issue #331.
//
// The race is real: a tmux monitor's 15-second capture ticker and its own
// 2-second liveness ticker hold the same *Session, so a stale poll lands after
// the reap. Guarding at the writers rather than at each caller means no future
// emitter can slip past. failSession needs no guard — StatusFailed is terminal,
// so this would pass it through anyway.
//
// Terminal→terminal is allowed through so the settling paths keep working: a
// stop racing a natural completion should still record the stop.
//
// This makes updateSessionStatus/tryUpdateSessionStatus unusable for reviving a
// terminal session: a resume must write session.Status directly under
// session.mu, as ResumeSession does, or it is silently dropped with only a
// slog.Debug for a trace.
func mayLeaveStatus(oldStatus, newStatus SessionStatus) bool {
	return !oldStatus.IsTerminal() || newStatus.IsTerminal()
}

func (m *Manager) failSession(session *Session, err error) {
	session.mu.Lock()
	oldStatus := session.Status
	session.Error = err
	applySessionStatusLocked(session, oldStatus, StatusFailed)
	session.mu.Unlock()

	m.emitSessionStateChange(SessionStateChangeEvent{
		Info:      session.ToInfo(),
		SessionID: session.ID,
		OldStatus: oldStatus,
		NewStatus: StatusFailed,
	})
}

func (m *Manager) tryUpdateSessionStatus(session *Session, fromStatus, toStatus SessionStatus) bool {
	session.mu.Lock()
	oldStatus := session.Status
	if oldStatus != fromStatus {
		session.mu.Unlock()
		return false
	}
	if !mayLeaveStatus(oldStatus, toStatus) {
		session.mu.Unlock()
		slog.Debug("suppressed status change for a session already terminal",
			"session", session.ID, "old_status", oldStatus, "new_status", toStatus)
		return false
	}
	applySessionStatusLocked(session, oldStatus, toStatus)
	session.mu.Unlock()

	m.emitSessionStateChange(SessionStateChangeEvent{
		Info:      session.ToInfo(),
		SessionID: session.ID,
		OldStatus: oldStatus,
		NewStatus: toStatus,
	})
	return true
}

func applySessionStatusLocked(session *Session, oldStatus, newStatus SessionStatus) {
	session.Status = newStatus
	now := time.Now()
	switch newStatus {
	case StatusRunning:
		// A new turn. Bumped here rather than at each caller because this is the
		// only place a session's status is written, so nothing can start a turn
		// without the pane-idle probe noticing — see paneIdleTracker.forTurn.
		session.turnEpoch++
		// Set StartedAt only when first starting (Pending→Running) or when missing;
		// preserve the original start time when resuming from Idle.
		if oldStatus == StatusPending || session.StartedAt == nil {
			session.StartedAt = &now
		}
	case StatusCompleted, StatusFailed, StatusStopped:
		session.CompletedAt = &now
	}
}

func (m *Manager) emitSessionStateChange(evt SessionStateChangeEvent) {
	// Emit state change event
	select {
	case m.events <- evt:
	default:
		slog.Warn("events channel full, dropping state change event",
			"session", evt.SessionID, "old_status", evt.OldStatus, "new_status", evt.NewStatus)
	}

	// Notify state subscribers. Called outside the lock so a sink is never
	// running while subscribe/unsubscribe waits on it, and never dropped: see
	// SubscribeStateChanges for why a bounded channel here is not an option.
	m.stateSubscribersMu.Lock()
	sinks := slices.Clone(m.stateSubscribers)
	m.stateSubscribersMu.Unlock()
	for _, sink := range sinks {
		sink.fn(evt)
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
		slog.Warn("events channel full, dropping output event", "session", sessionID)
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
		slog.Warn("events channel full, dropping append event", "line_type", lineType, "session", sessionID)
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
				slog.Warn("events channel full, dropping tool update event", "session", sessionID)
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

// ActiveWorktreePaths returns worktree paths with non-terminal sessions.
func (m *Manager) ActiveWorktreePaths() map[string]struct{} {
	m.mu.RLock()
	defer m.mu.RUnlock()

	paths := make(map[string]struct{})
	for _, s := range m.sessions {
		if s.WorktreePath != "" && !s.Status.IsTerminal() {
			paths[s.WorktreePath] = struct{}{}
		}
	}
	return paths
}

// SetSessionRunning moves an idle session back to running.
//
// Needed because a tmux session's status is driven entirely from outside: the
// agent's notify hook reports idleness, and nothing reports the opposite. When
// bramble itself types a prompt into such a session it is the only party that
// knows a turn just started. Without this the session stays "idle" through the
// whole turn, and the next notify hits the StatusRunning guard in
// SetSessionIdle and is dropped — so a second turn never produces a state
// change, and anything waiting on one (queued delivery, subagent reports)
// goes quiet after the first exchange.
//
// TUI sessions do not need this: their turn loop sets StatusRunning itself.
//
// The transition is compare-and-set rather than check-then-set because this is
// racing the agent's notify hook by construction: bramble submits a prompt at
// the same moment the previous turn's notify may be landing, and a separate
// read and write would let the two interleave into a lost update.
// It reports whether it moved the session, which is what lets a failed submit
// undo only the turn it actually started: the transition is a no-op on a
// session that was already running, and reverting that would end somebody
// else's live turn.
func (m *Manager) SetSessionRunning(id SessionID) bool {
	return m.trySetStatus(id, StatusIdle, StatusRunning)
}

// pollPaneIdle reads idleness off a backend's pane, for one tick of the
// monitor loop. tracker is nil for a provider with no probe, in which case
// this does nothing.
//
// A running session may be marked idle after consecutive idle observations.
// turnEpoch is what makes re-arming across turns reliable — a delivery and the
// transition it causes both happen between two polls, so status alone cannot
// show that boundary.
//
// An *idle* session is a candidate only for a provider whose idle status can
// become stale: reading its pane is the one thing that can put it back to
// running, either after a premature completion hook or work the provider
// starts without Bramble observing the running edge. Terminal sessions are
// never resurrected.
func (m *Manager) pollPaneIdle(tracker *paneIdleTracker, id SessionID, status SessionStatus, turnEpoch uint64, windowTarget string) {
	// A provider whose idle status can become stale needs a pane read even once
	// marked idle — that read is the only thing that can put it back to running.
	// Everything else is probed only while running, when the pane can establish
	// the idle transition. Which providers those are is the probe table's
	// business, not this loop's.
	if !shouldPollPaneIdle(tracker, status) {
		if tracker != nil {
			tracker.reset()
		}
		return
	}
	tracker.forTurn(turnEpoch)
	// CaptureTmuxPane rather than CapturePaneText: the caller resolved this
	// same target from the same session a few lines ago, and going back through
	// the session map would retake two locks per tick to rederive it.
	lines, err := CaptureTmuxPane(windowTarget, paneIdleCaptureLines)
	if err != nil {
		return
	}
	switch decidePaneIdlePoll(tracker, status, lines) {
	case paneIdleActionMarkIdle:
		m.SetSessionIdle(id)
	case paneIdleActionMarkRunning:
		m.SetSessionRunning(id)
	}
}

// SetSessionIdle transitions a session to StatusIdle (waiting for user input).
// It only transitions from StatusRunning to avoid reverting terminal states
// (completed, failed, stopped) that may have been set by the monitor loop.
func (m *Manager) SetSessionIdle(id SessionID) bool {
	return m.trySetStatus(id, StatusRunning, StatusIdle)
}

// trySetStatus looks a session up and moves it from one status to another,
// doing nothing if it is not there or has since moved on.
// trySetStatus reports whether it performed the transition, so a caller that
// needs to undo its own write can tell that write apart from a no-op.
func (m *Manager) trySetStatus(id SessionID, from, to SessionStatus) bool {
	m.mu.RLock()
	s, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return false
	}
	return m.tryUpdateSessionStatus(s, from, to)
}

// GetAllSessions returns all sessions sorted by creation time (newest first).
// RecentOutput is populated from the live output buffer so it's always fresh.
func (m *Manager) GetAllSessions() []SessionInfo {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]SessionInfo, 0, len(m.sessions))
	for _, s := range m.sessions {
		info := s.ToInfo()
		// For TUI sessions, populate RecentOutput from the live output buffer
		// so the command center shows the latest agent text, not a stale snapshot.
		// For tmux sessions, RecentOutput is already set by captureRecentOutput()
		// in the session's Progress struct, so we keep it from ToInfo().
		if !isTmuxRunner(info.RunnerType) {
			info.Progress.RecentOutput = m.RecentOutputLines(s.ID, sessionmodel.RecentOutputDisplayLines)
		}
		result = append(result, info)
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

// RecentOutputLines returns the last n lines of non-user assistant text for a session.
func (m *Manager) RecentOutputLines(id SessionID, n int) []string {
	m.outputsMu.RLock()
	defer m.outputsMu.RUnlock()

	lines, ok := m.outputs[id]
	if !ok {
		return nil
	}
	return sessionmodel.RecentAssistantTextFromSlice(lines, n)
}

// writeResearchFile writes a codetalk session's text output to a markdown file
// under the manager's result dir (~/.bramble/research by default).
func (m *Manager) writeResearchFile(session *Session) (string, error) {
	researchPath, err := ResultFilePath(m.config.ResearchDir, session.ID)
	if err != nil {
		return "", err
	}
	lines := m.GetSessionOutput(session.ID)

	var body strings.Builder
	for i := range lines {
		if lines[i].Type == OutputTypeText && !lines[i].IsUserPrompt {
			body.WriteString(lines[i].Content)
			body.WriteByte('\n')
		}
	}

	// Same treatment as the pane capture in subagent_report.go: a transcript is
	// the session's whole output and so is 0600 even inside a private ~/.bramble,
	// and create-and-rename replaces a symlink at this predictable path rather
	// than writing through it.
	if err := writeFileAtomic(researchPath, []byte(body.String()), 0o600); err != nil {
		return "", fmt.Errorf("write research file: %w", err)
	}

	return researchPath, nil
}

// CapturePaneText captures the last n lines of text from a tmux session's pane.
// Returns an error if the session is not a tmux session or capture fails.
func (m *Manager) CapturePaneText(id SessionID, n int) ([]string, error) {
	m.mu.RLock()
	session, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("session %q not found", id)
	}

	session.mu.RLock()
	windowID := session.TmuxWindowID
	windowName := session.TmuxWindowName
	runnerType := session.RunnerType
	session.mu.RUnlock()

	if !isTmuxRunner(runnerType) {
		return nil, fmt.Errorf("session %q is not a tmux session (runner type: %s)", id, runnerType)
	}

	target := SessionInfo{TmuxWindowID: windowID, TmuxWindowName: windowName}.TmuxTarget()
	if target == "" {
		return nil, fmt.Errorf("session %q has no tmux window target", id)
	}

	if n <= 0 {
		n = 10
	}
	return CaptureTmuxPane(target, n)
}

// ResolveTmuxTarget returns the tmux target (window ID, falling back to window
// name) for a tmux-backed session, applying the same runner-type guard as
// CapturePaneText. It is the single resolution point control-plane write ops
// (send-input, send-key, select) use so a caller addresses a session, never a
// raw tmux target.
func (m *Manager) ResolveTmuxTarget(id SessionID) (string, error) {
	m.mu.RLock()
	session, ok := m.sessions[id]
	m.mu.RUnlock()
	if !ok {
		return "", fmt.Errorf("session %q not found", id)
	}

	session.mu.RLock()
	windowID := session.TmuxWindowID
	windowName := session.TmuxWindowName
	runnerType := session.RunnerType
	session.mu.RUnlock()

	if !isTmuxRunner(runnerType) {
		return "", fmt.Errorf("session %q is not a tmux session (runner type: %s)", id, runnerType)
	}

	target := SessionInfo{TmuxWindowID: windowID, TmuxWindowName: windowName}.TmuxTarget()
	if target == "" {
		return "", fmt.Errorf("session %q has no tmux window target", id)
	}
	return target, nil
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
//
// NOTE: During the current transition period, live session data flows through
// the legacy addOutput/outputs path. The SessionModel returned here receives
// data only from InitOutputBuffer (test helper) or future callers that write
// directly to the model. A subsequent PR will wire addOutput into the
// SessionModel to complete the migration.
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
		slog.Warn("failed to create protocol log dir", "dir", logDir, "error", err)
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
			slog.Warn("failed to write log", "path", path, "error", err)
		}
	}
}
