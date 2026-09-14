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
//
// afterFile, when non-empty, is installed as the pane BY THE send-key call
// itself, via an atomic rename. That ordering is the point: the previous fixture
// rewrote the pane from a goroutine 30ms after it started and raced the poll
// loop, which forks this script per capture. If the first capture lost that race
// the "before" snapshot already read the new text, the pane never appeared to
// change, and a delivered key was reported as wedged -- observed as a flake. A
// non-atomic os.WriteFile also truncates first, so a capture landing mid-write
// could read an empty pane and count a half-written file as the change. Making
// the send install the new pane removes the timing question entirely.
func fakeBrambleBin(t *testing.T, paneFile, afterFile string) string {
	t.Helper()
	dir := filepath.Dir(paneFile)
	bin := filepath.Join(t.TempDir(), "bramble")
	script := "#!/bin/bash\n" +
		"case \"$1\" in\n" +
		"  capture-pane) cat " + paneFile + " ;;\n" +
		"  send-key)\n"
	if afterFile != "" {
		// Write then rename: a reader sees the old pane or the new one, never a
		// truncated one.
		script += "    printf '%s' \"$(cat " + afterFile + ")\" > " + dir + "/.pane.tmp\n" +
			"    mv " + dir + "/.pane.tmp " + paneFile + "\n"
	}
	script += "    exit 0 ;;\n" +
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
	// No afterFile: the send changes nothing, which is the wedged case.
	c := &Client{Bin: fakeBrambleBin(t, pane, ""), Sock: "unused"}

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
	// The pane repaints as a CONSEQUENCE of the send, which is both what a real
	// one does and the only ordering a test can rely on.
	after := filepath.Join(t.TempDir(), "after")
	if err := os.WriteFile(after, []byte("after\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	c := &Client{Bin: fakeBrambleBin(t, pane, after), Sock: "unused"}

	if err := c.SendKeyConfirmed(context.Background(), "sess", "Enter", 2*time.Second); err != nil {
		t.Fatalf("a pane that changed means the key landed, got %v", err)
	}
}
