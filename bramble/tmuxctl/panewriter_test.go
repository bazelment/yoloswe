package tmuxctl

import (
	"context"
	"errors"
	"testing"
)

// A pane someone scrolled back in is in tmux copy mode, where the pager eats
// every key — including the Enter that submits a delivered prompt. The message
// then lands in the composer and sits there: delivered by every measure bramble
// can see, and never read by the agent. Leaving copy mode first is what makes
// delivery to a watched session reliable.
func TestPaneWriterExitsCopyModeBeforePasting(t *testing.T) {
	t.Parallel()
	ctl := NewFake()
	w := NewPaneWriter(ctl)

	if err := w.Paste(context.Background(), "@7", "hello"); err != nil {
		t.Fatalf("Paste() error = %v", err)
	}

	if len(ctl.Calls) < 2 {
		t.Fatalf("expected an ExitCopyMode then a Paste, got %v", ctl.Calls)
	}
	if ctl.Calls[0].Method != "ExitCopyMode" {
		t.Errorf("first call = %q, want ExitCopyMode", ctl.Calls[0].Method)
	}
	if ctl.Calls[1].Method != "Paste" {
		t.Errorf("second call = %q, want Paste", ctl.Calls[1].Method)
	}
}

// If the pane cannot be taken out of copy mode, pasting into it would produce
// a message that is never submitted. Fail instead, so the caller can keep the
// delivery queued.
func TestPaneWriterPasteFailsWhenCopyModeCannotBeCleared(t *testing.T) {
	t.Parallel()
	ctl := NewFake()
	ctl.Err = errors.New("no such pane")
	w := NewPaneWriter(ctl)

	if err := w.Paste(context.Background(), "@7", "hello"); err == nil {
		t.Fatal("Paste() succeeded despite a failing ExitCopyMode")
	}
	for _, c := range ctl.Calls {
		if c.Method == "Paste" {
			t.Error("pasted anyway after ExitCopyMode failed")
		}
	}
}

// A submit can be reached without a preceding Paste: a caller that recognizes
// text it staged on an earlier attempt presses Enter without re-pasting, so
// Paste's own ExitCopyMode never runs. In copy mode that Enter is eaten by the
// pager and the staged text sits in the composer looking delivered while no
// turn ever starts — the same failure Paste exits copy mode to avoid.
func TestPaneWriterExitsCopyModeBeforeSubmitting(t *testing.T) {
	t.Parallel()
	ctl := NewFake()
	w := NewPaneWriter(ctl)

	if err := w.SendEnter(context.Background(), "@7"); err != nil {
		t.Fatalf("SendEnter() error = %v", err)
	}

	if len(ctl.Calls) < 2 {
		t.Fatalf("expected an ExitCopyMode then a SendSpecial, got %v", ctl.Calls)
	}
	if ctl.Calls[0].Method != "ExitCopyMode" {
		t.Errorf("first call = %q, want ExitCopyMode", ctl.Calls[0].Method)
	}
	if ctl.Calls[1].Method != "SendSpecial" {
		t.Errorf("second call = %q, want SendSpecial", ctl.Calls[1].Method)
	}
}

// If the pane cannot be taken out of copy mode, the Enter would be swallowed
// and the caller would believe it submitted. Fail instead, so the caller can
// leave the parent's status alone and try again.
func TestPaneWriterSendEnterFailsWhenCopyModeCannotBeCleared(t *testing.T) {
	t.Parallel()
	ctl := NewFake()
	ctl.Err = errors.New("no such pane")
	w := NewPaneWriter(ctl)

	if err := w.SendEnter(context.Background(), "@7"); err == nil {
		t.Fatal("SendEnter() succeeded despite a failing ExitCopyMode")
	}
	for _, c := range ctl.Calls {
		if c.Method == "SendSpecial" {
			t.Error("sent Enter anyway after ExitCopyMode failed")
		}
	}
}

// SendEnter is the submit step; it must map to the Enter key.
func TestPaneWriterSendEnterSendsEnter(t *testing.T) {
	t.Parallel()
	ctl := NewFake()
	w := NewPaneWriter(ctl)

	if err := w.SendEnter(context.Background(), "@7"); err != nil {
		t.Fatalf("SendEnter() error = %v", err)
	}
	calls := ctl.CallsFor("SendSpecial")
	if len(calls) != 1 || calls[0].Special != KeyEnter {
		t.Errorf("SendSpecial calls = %v, want one Enter", calls)
	}
}
