package model_test

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/model"
)

// The fake CLI asks the SDK to permit Bash before executing the command. This
// exercises the same stdio permission handshake that failed in G65 run 5.
func TestShellClaudeHelper(t *testing.T) {
	if os.Getenv("ORCA_FAKE_SHELL_CLAUDE") != "1" {
		return
	}
	scan := bufio.NewScanner(os.Stdin)
	for scan.Scan() {
		var m struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
		}
		if err := json.Unmarshal(scan.Bytes(), &m); err != nil {
			os.Exit(2)
		}
		switch m.Type {
		case "control_request":
			fmt.Printf("{\"type\":\"control_response\",\"response\":{\"subtype\":\"success\",\"request_id\":%q,\"response\":{}}}\n", m.RequestID)
		case "user":
			fmt.Println(`{"type":"system","subtype":"init","session_id":"synthetic-shell","model":"sonnet"}`)
			if os.Getenv("ORCA_FAKE_READ_FIRST") == "1" {
				fmt.Println(`{"type":"control_request","request_id":"read-first","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"cat EVENTS"}}}`)
				if !scan.Scan() {
					os.Exit(6)
				}
				var readReply struct {
					Response struct {
						Response struct {
							Behavior string `json:"behavior"`
						} `json:"response"`
					} `json:"response"`
				}
				if json.Unmarshal(scan.Bytes(), &readReply) != nil || readReply.Response.Response.Behavior != "allow" {
					os.Exit(7)
				}
				if err := os.WriteFile("read-first-order", []byte("read\n"), 0600); err != nil {
					os.Exit(8)
				}
				fmt.Println(`{"type":"control_request","request_id":"write-after-read","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"printf write > touched"}}}`)
				if !scan.Scan() {
					os.Exit(9)
				}
				var writeReply struct {
					Response struct {
						Response struct {
							Behavior string `json:"behavior"`
						} `json:"response"`
					} `json:"response"`
				}
				if json.Unmarshal(scan.Bytes(), &writeReply) != nil || writeReply.Response.Response.Behavior != "allow" {
					os.Exit(10)
				}
				if err := os.WriteFile("read-first-order", []byte("read\nwrite\n"), 0600); err != nil {
					os.Exit(11)
				}
				fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"read before write","session_id":"synthetic-shell","usage":{"input_tokens":1,"output_tokens":1}}`)
				continue
			}
			fmt.Println(`{"type":"control_request","request_id":"bash-proof","request":{"subtype":"can_use_tool","tool_name":"Bash","input":{"command":"printf shell-works > bash-proof"}}}`)
			if !scan.Scan() {
				os.Exit(3)
			}
			var reply struct {
				Response struct {
					Response struct {
						Behavior string `json:"behavior"`
					} `json:"response"`
				} `json:"response"`
			}
			if err := json.Unmarshal(scan.Bytes(), &reply); err != nil || reply.Response.Response.Behavior != "allow" {
				os.Exit(4)
			}
			cmd := exec.Command("bash", "-c", "printf shell-works > bash-proof")
			if err := cmd.Run(); err != nil {
				os.Exit(5)
			}
			fmt.Println(`{"type":"result","subtype":"success","is_error":false,"result":"Bash worked","session_id":"synthetic-shell","usage":{"input_tokens":1,"output_tokens":1}}`)
		}
	}
	os.Exit(0)
}

func TestAdoptedSessionReadsEvidenceBeforeWrite(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "EVENTS"), []byte("run-5 host handled stall\n"), 0600); err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\nexport ORCA_FAKE_SHELL_CLAUDE=1 ORCA_FAKE_READ_FIRST=1\nexec '" + strings.ReplaceAll(exe, "'", "'\\''") + "' -test.run=^TestShellClaudeHelper$ -- \"$@\"\n"
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	sess := model.NewAgentSession(model.AgentSessionConfig{Provider: "claude", ClaudeBin: bin, WorkDir: dir, AllowBuiltins: true, Env: map[string]string{"ORCA_TOOLBOX_SHADOW": "1"}})
	defer sess.Close()
	if _, err := sess.Turn(t.Context(), model.AgentTurnRequest{Prompt: "Read EVENTS and status before any write"}); err != nil {
		t.Fatal(err)
	}
	order, err := os.ReadFile(filepath.Join(dir, "read-first-order"))
	if err != nil {
		t.Fatal(err)
	}
	if string(order) != "read\nwrite\n" {
		t.Fatalf("tool order=%q", order)
	}
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
	if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	sess := model.NewAgentSession(model.AgentSessionConfig{Provider: "claude", Model: "sonnet", ClaudeBin: bin, WorkDir: dir, AllowBuiltins: true, Env: map[string]string{"ORCA_TOOLBOX_SHADOW": "1"}})
	defer sess.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err := sess.Turn(ctx, model.AgentTurnRequest{Prompt: "Use Bash to write the synthetic proof"}); err != nil {
		t.Fatal(err)
	}
	proof, err := os.ReadFile(filepath.Join(dir, "bash-proof"))
	if err != nil {
		t.Fatal(err)
	}
	if string(proof) != "shell-works" {
		t.Fatalf("proof=%q", proof)
	}
}
