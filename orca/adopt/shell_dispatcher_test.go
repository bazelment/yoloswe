package adopt

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/store"
)

// The fake CLI logs each turn and holds its first turn open until the test
// writes the release file, so the test can act while a turn is running.
func TestFakeDispatcherClaude(t *testing.T) {
	dir := os.Getenv("ORCA_FAKE_DISPATCHER_DIR")
	if dir == "" {
		return
	}
	turns := 0
	for scan := bufio.NewScanner(os.Stdin); scan.Scan(); {
		var m struct {
			Type      string
			RequestID string `json:"request_id"`
		}
		_ = json.Unmarshal(scan.Bytes(), &m)
		switch m.Type {
		case "control_request":
			fmt.Printf("{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"request_id\":%q,\"response\":{}}}\n", m.RequestID)
		case "user":
			turns++
			f, err := os.OpenFile(filepath.Join(dir, "turns"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
			if err != nil {
				os.Exit(4)
			}
			_, _ = f.WriteString("turn\n")
			_ = f.Close()
			for deadline := time.Now().Add(20 * time.Second); turns == 1 && time.Now().Before(deadline); time.Sleep(20 * time.Millisecond) {
				if _, err := os.Stat(filepath.Join(dir, "release")); err == nil {
					break
				}
			}
			fmt.Println(`{"type":"system","subtype":"init","session_id":"synthetic-dispatcher","model":"sonnet"}`)
			fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"waiting","session_id":"synthetic-dispatcher","usage":{"input_tokens":1,"output_tokens":1}}`)
		}
	}
	os.Exit(0)
}

// An owner message relayed while a turn runs wakes the next turn; it must not
// become part of the post-turn wait's baseline.
func TestShellDispatcherWakesForOwnerMessageDuringTurn(t *testing.T) {
	t.Parallel()
	live, copyDir, intent, fake := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'" }
	bin := filepath.Join(fake, "claude")
	script := "#!/bin/sh\nexport ORCA_FAKE_DISPATCHER_DIR=" + quote(fake) + "\nexec " + quote(exe) + " -test.run=^TestFakeDispatcherClaude$ -- \"$@\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	st := store.NewMemoryStore(clock.RealClock{})
	d := &ShellDispatcher{
		Store: st, Run: Run{ID: "run-wake", Dir: live}, Shadow: true, IntentDir: intent,
		Config: model.AgentSessionConfig{Model: "sonnet", ClaudeBin: bin, WorkDir: copyDir},
	}
	if err := d.Prepare(); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { defer close(done); d.Supervise(ctx) }()
	defer func() { cancel(); <-done }()
	turns := func() int {
		raw, _ := os.ReadFile(filepath.Join(fake, "turns"))
		return strings.Count(string(raw), "\n")
	}
	waitFor(t, func() bool { return turns() == 1 })
	// The owner writes during the first turn; the watcher relays it.
	if _, err := st.Events().Append(ctx, store.Event{RunID: "run-wake", Kind: store.EventOwnerMessage, Source: "owner", Summary: "answer lane L1"}); err != nil {
		t.Fatal(err)
	}
	if err := d.syncEventFiles(ctx); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(fake, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	waitFor(t, func() bool { return turns() >= 2 })
}
