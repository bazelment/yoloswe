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
}

// Courier is the narrow slice of *session.Courier the dispatcher needs to hold
// a message back until its recipient is idle. Consumer-side, like Registry, so
// the queue branch can be tested without a real delivery directory.
type Courier interface {
	Send(ctx context.Context, from, to session.SessionID, text string, submit bool) (bool, error)
}

// Dispatcher handles control protocol requests against a registry (session
// -centric ops) and a tmuxctl.Controller (raw-pane ops). It is transport
// -agnostic: the local CLI and the remote hub client both call Handle.
type Dispatcher struct {
	reg     Registry
	ctl     tmuxctl.Controller
	courier Courier
}

// NewDispatcher constructs a Dispatcher. Queued delivery is unavailable until
// SetCourier is called; a send_input asking for it gets a clear error rather
// than silently interrupting the recipient.
func NewDispatcher(reg Registry, ctl tmuxctl.Controller) *Dispatcher {
	return &Dispatcher{reg: reg, ctl: ctl}
}

// SetCourier enables queued delivery.
func (d *Dispatcher) SetCourier(c Courier) { d.courier = c }

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
		target := s.TmuxWindowID
		if target == "" {
			target = s.TmuxWindowName
		}
		out.Sessions = append(out.Sessions, SessionSummary{
			ID:           string(s.ID),
			Type:         string(s.Type),
			Status:       string(s.Status),
			WorktreeName: s.WorktreeName,
			Model:        s.Model,
			RunnerType:   s.RunnerType,
			TmuxTarget:   target,
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
func (d *Dispatcher) sendInput(ctx context.Context, req *Msg, sessionScoped bool) (any, error) {
	var r SendInputReq
	if err := req.decode(&r); err != nil {
		return nil, err
	}

	// The queued path goes through the courier, which knows how to reach a
	// session whatever its runner. The unqueued path below is unchanged: it
	// still types straight into a pane, which stays the right behaviour for a
	// deliberate interrupt and for raw pane targets.
	if r.Queue {
		if r.SessionID == "" {
			return nil, fmt.Errorf("queue requires session_id: a raw pane target has no status to wait on")
		}
		if d.courier == nil {
			return nil, fmt.Errorf("queued delivery is not available on this bramble")
		}
		queued, err := d.courier.Send(ctx, session.SessionID(r.From), session.SessionID(r.SessionID), r.Text, r.Submit)
		if err != nil {
			return nil, err
		}
		return SendInputResult{OK: true, Queued: queued}, nil
	}

	target, err := d.targetFor(r.SessionID, r.Target, sessionScoped)
	if err != nil {
		return nil, err
	}
	if err := d.ctl.Paste(ctx, target, r.Text); err != nil {
		return nil, err
	}
	if r.Submit {
		if err := d.ctl.SendSpecial(ctx, target, tmuxctl.KeyEnter); err != nil {
			return nil, err
		}
	}
	return OKResult{OK: true}, nil
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
	if err := d.ctl.SendSpecial(ctx, target, r.Key); err != nil {
		return OKResult{}, err
	}
	return OKResult{OK: true}, nil
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
