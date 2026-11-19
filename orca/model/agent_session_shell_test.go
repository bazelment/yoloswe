package model_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/orca/model"
)

// The fake CLI runs Bash the way the real CLI does once permissions are
// bypassed: without a permission request. It refuses any other launch mode.
func TestShellClaudeHelper(t *testing.T) {
	if os.Getenv("ORCA_FAKE_SHELL_CLAUDE") != "1" {
		return
	}
	bypass := slices.Contains(os.Args, "bypassPermissions") && !slices.Contains(os.Args, "--permission-prompt-tool")
	for scan := bufio.NewScanner(os.Stdin); scan.Scan(); {
		var m struct {
			Type      string
			RequestID string `json:"request_id"`
		}
		_ = json.Unmarshal(scan.Bytes(), &m)
		switch {
		case m.Type == "control_request":
			fmt.Printf("{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"request_id\":%q,\"response\":{}}}\n", m.RequestID)
		case m.Type == "user" && bypass && exec.Command("bash", "-c", "printf shell-works > bash-proof").Run() == nil:
			fmt.Println(`{"type":"system","subtype":"init","session_id":"synthetic-shell","model":"sonnet"}`)
			fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"Bash worked","session_id":"synthetic-shell","usage":{"input_tokens":1,"output_tokens":1}}`)
		case m.Type == "user":
			os.Exit(3)
		}
	}
	os.Exit(0)
}

func TestAdoptedSessionBashWorks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\nexport ORCA_FAKE_SHELL_CLAUDE=1\nexec '" + strings.ReplaceAll(exe, "'", "'\\''") + "' -test.run=^TestShellClaudeHelper$ -- \"$@\"\n"
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	sess := model.NewAgentSession(model.AgentSessionConfig{Provider: "claude", Model: "sonnet", ClaudeBin: bin, WorkDir: dir, AllowBuiltins: true})
	defer sess.Close()
	if _, err := sess.Turn(t.Context(), model.AgentTurnRequest{Prompt: "Use Bash to write the synthetic proof"}); err != nil {
		t.Fatal(err)
	}
	if proof, err := os.ReadFile(filepath.Join(dir, "bash-proof")); err != nil || string(proof) != "shell-works" {
		t.Fatalf("proof=%q err=%v", proof, err)
	}
}
