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

// NewPaneWriter wraps a Controller so a session.Courier can type into panes.
func NewPaneWriter(ctl Controller) session.PaneWriter { return &paneWriter{ctl: ctl} }

func (w *paneWriter) Paste(ctx context.Context, target, text string) error {
	// Leave copy mode first. A pane someone scrolled back in swallows the
	// Enter that would submit this text, so the message lands in the composer
	// and simply sits there — delivered by every measure bramble can see, and
	// never actually read by the agent.
	if err := w.ctl.ExitCopyMode(ctx, target); err != nil {
		return err
	}
	return w.ctl.Paste(ctx, target, text)
}

// SendEnter submits what was pasted. The courier's interface names the intent
// rather than taking a SpecialKey, so session does not need tmuxctl's key
// vocabulary to describe a delivery.
func (w *paneWriter) SendEnter(ctx context.Context, target string) error {
	return w.ctl.SendSpecial(ctx, target, KeyEnter)
}
