package bramble

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeBrambleBin writes a stub `bramble` whose capture-pane output is whatever
// is in paneFile at the time of the call, so a test can decide whether a send
// changes the pane.
func fakeBrambleBin(t *testing.T, paneFile string) string {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "bramble")
	script := "#!/bin/bash\n" +
		"case \"$1\" in\n" +
		"  capture-pane) cat " + paneFile + " ;;\n" +
		"  send-key) exit 0 ;;\n" +
		"esac\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin
}

// The live failure: three sends exited 0 against a pane that never changed,
// holding a typed-but-unsubmitted line, while the process was alive and the
// session's status read `running`. An exit code describes the request, not its
// effect -- so a send that reports success against an unchanged pane must be an
// error, or a wedged session is indistinguishable from a working one.
func TestSendKeyConfirmedReportsAnUnchangedPaneAsUndelivered(t *testing.T) {
	t.Parallel()
	pane := filepath.Join(t.TempDir(), "pane")
	if err := os.WriteFile(pane, []byte("frozen\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Client{Bin: fakeBrambleBin(t, pane), Sock: "unused"}

	err := c.SendKeyConfirmed(context.Background(), "sess", "Enter", 150*time.Millisecond)
	if !errors.Is(err, ErrNotDelivered) {
		t.Fatalf("an unchanged pane must report ErrNotDelivered, got %v", err)
	}
}

// The other direction: a pane that does move is a delivered key, and must not
// be reported as wedged.
func TestSendKeyConfirmedAcceptsAPaneThatMoves(t *testing.T) {
	t.Parallel()
	pane := filepath.Join(t.TempDir(), "pane")
	if err := os.WriteFile(pane, []byte("before\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Client{Bin: fakeBrambleBin(t, pane), Sock: "unused"}

	// The pane repaints shortly after the send, as a real one does.
	go func() {
		time.Sleep(30 * time.Millisecond)
		_ = os.WriteFile(pane, []byte("after\n"), 0o600)
	}()

	if err := c.SendKeyConfirmed(context.Background(), "sess", "Enter", 2*time.Second); err != nil {
		t.Fatalf("a pane that changed means the key landed, got %v", err)
	}
}
