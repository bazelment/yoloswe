package tmuxctl

import (
	"context"

	"github.com/bazelment/yoloswe/bramble/session"
)

// paneWriter adapts a Controller to session.PaneWriter.
//
// The adapter lives here rather than in session because the dependency only
// runs one way: tmuxctl already imports session for PaneStatus, so session
// cannot import tmuxctl back. session.PaneWriter is declared consumer-side for
// that reason, and this is the single place that satisfies it.
type paneWriter struct{ ctl Controller }

// NewPaneWriter wraps a Controller so a session.Notifier can type into panes.
func NewPaneWriter(ctl Controller) session.PaneWriter { return &paneWriter{ctl: ctl} }

func (w *paneWriter) Paste(ctx context.Context, target, text string) error {
	return Paste(ctx, w.ctl, target, text)
}

// Paste writes text into a pane, leaving copy mode first.
//
// The copy-mode step is why this is shared rather than two call sites doing
// ctl.Paste: a pane someone scrolled back in swallows the Enter that would
// submit the text, so the message lands in the composer and simply sits there —
// delivered by every measure bramble can see, and never actually read by the
// agent. Every writer needs that, so a third one cannot forget it.
func Paste(ctx context.Context, ctl Controller, target, text string) error {
	if err := ctl.ExitCopyMode(ctx, target); err != nil {
		return err
	}
	return ctl.Paste(ctx, target, text)
}

// SendEnter submits what is staged in the composer. The interface names the
// intent rather than taking a SpecialKey, so session does not need tmuxctl's
// key vocabulary to describe a write.
//
// It leaves copy mode for the same reason Paste does, and not merely because
// Paste happens to run first: a submit can now be reached without one, when a
// caller recognizes text it staged on an earlier attempt and presses Enter
// without re-pasting. In copy mode that Enter is consumed by the pager, so the
// text stays in the composer looking delivered while no turn ever starts.
func (w *paneWriter) SendEnter(ctx context.Context, target string) error {
	if err := w.ctl.ExitCopyMode(ctx, target); err != nil {
		return err
	}
	return w.ctl.SendSpecial(ctx, target, KeyEnter)
}
