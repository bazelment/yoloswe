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
		s, err := d.ctl.ListSessions(ctx)
		return s, err
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
func (d *Dispatcher) sendInput(ctx context.Context, req *Msg, sessionScoped bool) (OKResult, error) {
	var r SendInputReq
	if err := req.decode(&r); err != nil {
		return OKResult{}, err
	}
	target, err := d.targetFor(r.SessionID, r.Target, sessionScoped)
	if err != nil {
		return OKResult{}, err
	}
	if err := d.ctl.Paste(ctx, target, r.Text); err != nil {
		return OKResult{}, err
	}
	if r.Submit {
		if err := d.ctl.SendSpecial(ctx, target, tmuxctl.KeyEnter); err != nil {
			return OKResult{}, err
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
