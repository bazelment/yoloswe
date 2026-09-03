package control

import (
	"context"
	"fmt"

	"github.com/bazelment/yoloswe/bramble/session"
	"github.com/bazelment/yoloswe/bramble/tmuxctl"
)

// Registry is the narrow slice of *session.SessionRegistry the dispatcher needs.
// Defined here (consumer side) so the dispatcher can be tested with a fake and
// does not pull in the full Manager machinery.
type Registry interface {
	GetAllSessions() []session.SessionInfo
	ResolveTmuxTarget(id session.SessionID) (string, error)
	CapturePaneText(id session.SessionID, n int) ([]string, error)
	StopSession(id session.SessionID) error
	// SetSessionRunning records that a write started a turn. A tmux session's
	// status is driven from outside — the agent's hook reports idle and nothing
	// reports the opposite — so the party that typed the prompt is the only one
	// that knows. It reports whether it moved the session: a no-op on an
	// already-running session must not be undone, or the undo ends a turn this
	// write did not start.
	SetSessionRunning(id session.SessionID) bool
	// SetSessionIdle rolls that back when the submit itself failed.
	SetSessionIdle(id session.SessionID) bool
}

// Dispatcher handles control protocol requests against a registry (session
// -centric ops) and a tmuxctl.Controller (raw-pane ops). It is transport
// -agnostic: the local CLI and the remote hub client both call Handle.
type Dispatcher struct {
	reg Registry
	ctl tmuxctl.Controller
}

// NewDispatcher constructs a Dispatcher.
func NewDispatcher(reg Registry, ctl tmuxctl.Controller) *Dispatcher {
	return &Dispatcher{reg: reg, ctl: ctl}
}

// Handle processes one request Msg and returns a response Msg. It never returns
// a nil Msg for a known request: failures are encoded as a TypeResponse with an
// error string so the caller always has something to send back.
func (d *Dispatcher) Handle(ctx context.Context, req *Msg) *Msg {
	result, err := d.dispatch(ctx, req)
	if err != nil {
		return errResponse(req.ID, err)
	}
	return okResponse(req.ID, result)
}

// dispatch routes a request to its handler and returns the typed result.
func (d *Dispatcher) dispatch(ctx context.Context, req *Msg) (any, error) {
	switch req.Type {
	case TypeSessionList:
		return d.sessionList(), nil
	case TypeSessionCapture:
		return d.sessionCapture(ctx, req)
	case TypeSessionStatus:
		return d.sessionStatus(ctx, req)
	case TypeSessionSendInput:
		return d.sendInput(ctx, req, true)
	case TypeSessionSendKey:
		return d.sendKey(ctx, req, true)
	case TypeSessionSelect:
		return d.sessionSelect(ctx, req)
	case TypeSessionStop:
		return d.sessionStop(req)

	case TypeTmuxListSessions:
		return d.ctl.ListSessions(ctx)
	case TypeTmuxListWindows:
		var r TargetRef
		if err := req.decode(&r); err != nil {
			return nil, err
		}
		return d.ctl.ListWindows(ctx, r.Target)
	case TypeTmuxListPanes:
		var r TargetRef
		if err := req.decode(&r); err != nil {
			return nil, err
		}
		return d.ctl.ListPanes(ctx, r.Target)
	case TypePaneCapture:
		return d.sessionCapture(ctx, req) // handles both session_id and target
	case TypePaneSendInput:
		return d.sendInput(ctx, req, false)
	case TypePaneSendKey:
		return d.sendKey(ctx, req, false)
	case TypePaneNewWindow:
		var r NewWindowReq
		if err := req.decode(&r); err != nil {
			return nil, err
		}
		id, err := d.ctl.NewWindow(ctx, r.Name, r.CWD, r.Cmd)
		if err != nil {
			return nil, err
		}
		return NewWindowResult{WindowID: id}, nil
	case TypePaneKill:
		var r TargetRef
		if err := req.decode(&r); err != nil {
			return nil, err
		}
		if err := d.ctl.Kill(ctx, r.Target); err != nil {
			return nil, err
		}
		return OKResult{OK: true}, nil

	default:
		return nil, fmt.Errorf("control: unsupported request type %q", req.Type)
	}
}

func (d *Dispatcher) sessionList() SessionListResult {
	infos := d.reg.GetAllSessions()
	out := SessionListResult{Sessions: make([]SessionSummary, 0, len(infos))}
	for i := range infos {
		s := &infos[i]
		out.Sessions = append(out.Sessions, SessionSummary{
			ID:           string(s.ID),
			Type:         string(s.Type),
			Status:       string(s.Status),
			WorktreeName: s.WorktreeName,
			Model:        s.Model,
			Backend:      s.Backend,
			RunnerType:   s.RunnerType,
			TmuxTarget:   s.TmuxTarget(),
		})
	}
	return out
}

func (d *Dispatcher) sessionCapture(ctx context.Context, req *Msg) (CaptureResult, error) {
	var r CaptureReq
	if err := req.decode(&r); err != nil {
		return CaptureResult{}, err
	}
	if r.SessionID != "" {
		lines, err := d.reg.CapturePaneText(session.SessionID(r.SessionID), r.Lines)
		if err != nil {
			return CaptureResult{}, err
		}
		return CaptureResult{Lines: lines}, nil
	}
	if r.Target == "" {
		return CaptureResult{}, fmt.Errorf("control: capture requires session_id or target")
	}
	lines, err := d.ctl.Capture(ctx, r.Target, r.Lines)
	if err != nil {
		return CaptureResult{}, err
	}
	return CaptureResult{Lines: lines}, nil
}

func (d *Dispatcher) sessionStatus(ctx context.Context, req *Msg) (*PaneStatusJSON, error) {
	target, err := d.resolveTarget(req)
	if err != nil {
		return nil, err
	}
	ps, err := d.ctl.Status(ctx, target)
	if err != nil {
		return nil, err
	}
	return toStatusJSON(ps), nil
}

// sendInput delivers text to a session (sessionScoped=true) or a raw target.
func (d *Dispatcher) sendInput(ctx context.Context, req *Msg, sessionScoped bool) (SendInputResult, error) {
	var r SendInputReq
	if err := req.decode(&r); err != nil {
		return SendInputResult{}, err
	}

	// Refused rather than silently downgraded to an immediate paste: a caller
	// that asked to wait for an idle recipient must not get a mid-turn
	// interrupt instead.
	if r.Queue {
		return SendInputResult{}, fmt.Errorf(
			"queued delivery has been removed: send without --queue for a deliberate interrupt, " +
				"or let the orchestrator read completion from the run directory")
	}

	target, err := d.targetFor(r.SessionID, r.Target, sessionScoped)
	if err != nil {
		return SendInputResult{}, err
	}
	if err := tmuxctl.Paste(ctx, d.ctl, target, r.Text); err != nil {
		return SendInputResult{}, err
	}
	if r.Submit {
		// Before the Enter, not after. See noteTurnStarted.
		started := d.noteTurnStarted(sessionScoped, r.SessionID)
		if err := d.ctl.SendSpecial(ctx, target, tmuxctl.KeyEnter); err != nil {
			started.undo()
			return SendInputResult{}, err
		}
	}
	return SendInputResult{OK: true}, nil
}

func (d *Dispatcher) sendKey(ctx context.Context, req *Msg, sessionScoped bool) (OKResult, error) {
	var r SendKeyReq
	if err := req.decode(&r); err != nil {
		return OKResult{}, err
	}
	target, err := d.targetFor(r.SessionID, r.Target, sessionScoped)
	if err != nil {
		return OKResult{}, err
	}
	// Enter submits whatever is in the composer, which is a turn starting just
	// as much as a submitted send-input is — and the two-step "stage, then
	// Enter" is a documented workflow, so this is the completion of the write
	// the unsubmitted send deliberately did not finish. Only Enter: C-c,
	// Escape and the arrows do not start a turn.
	//
	// Before the key, not after. See noteTurnStarted.
	var started turnStart
	if r.Key == tmuxctl.KeyEnter {
		// Nothing pasted before this Enter, so nothing has taken the pane out
		// of copy mode for it the way tmuxctl.Paste does for a submitted
		// send-input. In copy mode the pager eats the key, and the staged text
		// stays in the composer looking delivered while no turn ever starts.
		// Only for Enter: C-c, Escape and the arrows are legitimate copy-mode
		// input and must reach the pager unchanged.
		if err := d.ctl.ExitCopyMode(ctx, target); err != nil {
			return OKResult{}, err
		}
		started = d.noteTurnStarted(sessionScoped, r.SessionID)
	}
	if err := d.ctl.SendSpecial(ctx, target, r.Key); err != nil {
		started.undo()
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// turnStart undoes a turn-start marking whose submit then failed.
type turnStart struct {
	reg Registry
	id  session.SessionID
}

// undo returns the session to idle after a failed submit.
//
// Only when this write is what made it running. The two halves are
// compare-and-set with opposite preconditions — SetSessionRunning moves
// Idle→Running and no-ops on an already-running session, while SetSessionIdle
// moves Running→Idle and succeeds on exactly that session — so an unconditional
// undo ends a turn somebody else started. That is the ordinary case here, not a
// narrow race: interrupting a mid-turn session is what this endpoint is for, and
// a false idle is the harmful direction, telling the orchestrator a working lane
// has finished.
func (t turnStart) undo() {
	if t.reg != nil && t.id != "" {
		t.reg.SetSessionIdle(t.id)
	}
}

// noteTurnStarted records that a write is starting a turn in a session.
//
// One helper rather than the same lines at each write path, because the cost of
// forgetting it is invisible locally and severe downstream: a tmux session's
// status only ever moves one way from outside — the agent's hook reports idle
// and nothing reports the opposite — so a turn that goes unrecorded leaves the
// session reading idle for its whole duration. list-sessions, which this
// transport relies on for delivery, then calls a working lane finished; and
// SetSessionIdle is a compare-and-set from StatusRunning, so the turn's real
// completion notify is dropped too and the next turn produces no state change
// either.
//
// **Called before the Enter, not after.** Marking afterwards looks harmless and
// is not: a fast agent can answer and fire its completion notify in the gap. That
// notify hits SetSessionIdle's compare-and-set, sees StatusIdle rather than
// StatusRunning, and is dropped — and then this marks the session running with
// nothing left alive to end it, so it reads busy forever. Marking first means
// the worst case is a session briefly running when the submit failed, which
// undo corrects.
//
// Session-scoped writes only: a raw --target names a pane, which has no session
// status to move.
func (d *Dispatcher) noteTurnStarted(sessionScoped bool, sessionID string) turnStart {
	if !sessionScoped || sessionID == "" {
		return turnStart{}
	}
	id := session.SessionID(sessionID)
	if !d.reg.SetSessionRunning(id) {
		// Already running: this write did not start that turn, so it has
		// nothing to undo.
		return turnStart{}
	}
	return turnStart{reg: d.reg, id: id}
}

func (d *Dispatcher) sessionSelect(ctx context.Context, req *Msg) (OKResult, error) {
	target, err := d.resolveTarget(req)
	if err != nil {
		return OKResult{}, err
	}
	if err := d.ctl.Select(ctx, target); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

func (d *Dispatcher) sessionStop(req *Msg) (OKResult, error) {
	var r SessionRef
	if err := req.decode(&r); err != nil {
		return OKResult{}, err
	}
	if err := d.reg.StopSession(session.SessionID(r.SessionID)); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
}

// resolveTarget extracts a SessionRef from the request and resolves it.
func (d *Dispatcher) resolveTarget(req *Msg) (string, error) {
	var r SessionRef
	if err := req.decode(&r); err != nil {
		return "", err
	}
	return d.reg.ResolveTmuxTarget(session.SessionID(r.SessionID))
}

// targetFor returns the tmux target: resolve via the registry guard when
// session-scoped, otherwise use the raw target. This is the single place the
// session-vs-raw decision is made for the write ops.
func (d *Dispatcher) targetFor(sessionID, rawTarget string, sessionScoped bool) (string, error) {
	if sessionScoped {
		if sessionID == "" {
			return "", fmt.Errorf("control: session_id required")
		}
		return d.reg.ResolveTmuxTarget(session.SessionID(sessionID))
	}
	if rawTarget == "" {
		return "", fmt.Errorf("control: target required")
	}
	return rawTarget, nil
}

// toStatusJSON projects a session.PaneStatus into the wire type (nil-safe).
func toStatusJSON(ps *session.PaneStatus) *PaneStatusJSON {
	if ps == nil {
		return nil
	}
	return &PaneStatusJSON{
		Model:       ps.Model,
		ContextPct:  ps.ContextPct,
		TokenCount:  ps.TokenCount,
		Branch:      ps.Branch,
		StatusLine:  ps.StatusLine,
		Permissions: ps.Permissions,
		IsIdle:      ps.IsIdle,
		IsWorking:   ps.IsWorking,
	}
}
