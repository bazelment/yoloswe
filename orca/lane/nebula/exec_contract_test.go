package nebula

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/orca/lane"
)

func TestLaneProbeExecWireContract(t *testing.T) {
	if !strings.Contains(healthProbeScript, "ps -eo comm=") || strings.Contains(healthProbeScript, "ps -eo args=") {
		t.Fatalf("health probe must inspect process names without command lines: %s", healthProbeScript)
	}
	var wireCommand string
	var wireTimeout int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Command   json.RawMessage `json:"command"`
			TimeoutMs int             `json:"timeoutMs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode exec body: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := json.Unmarshal(body.Command, &wireCommand); err != nil {
			t.Errorf("command must be a string: %v", err)
			http.Error(w, "command must be a string", http.StatusBadRequest)
			return
		}
		wireTimeout = body.TimeoutMs
		_, _ = w.Write([]byte(`{"stdout":"COMMITS:0\nDISK:20\n","exitCode":0}`))
	}))
	defer srv.Close()

	d, err := New(Config{APIURL: srv.URL, Token: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	ln := lane.Lane{RunID: "run", ID: "lane", SessionID: "session", BaseBranch: "main", Branch: "origin/feature with spaces'and-symbols"}
	if _, err := d.LaneGitState(t.Context(), ln); err != nil {
		t.Fatal(err)
	}

	argv := []string{"sh", "-c", laneProbeScript, "--", "main", "feature with spaces'and-symbols"}
	want := "'sh' '-c' '" + strings.ReplaceAll(laneProbeScript, "'", "'\\''") + "' '--' 'main' 'feature with spaces'\\''and-symbols'"
	if wireCommand != want {
		t.Fatalf("wire command=%q, want %q", wireCommand, want)
	}
	decoded, err := parseContractShellWords(wireCommand)
	if err != nil || len(decoded) != len(argv) {
		t.Fatalf("POSIX shell words round trip=%q/%v, want %q", decoded, err, argv)
	}
	for i := range argv {
		if decoded[i] != argv[i] {
			t.Fatalf("POSIX shell words round trip=%q/%v, want %q", decoded, err, argv)
		}
	}
	if wireTimeout != execTimeoutMs || wireTimeout > 30000 {
		t.Fatalf("timeoutMs=%d, want %d and <=30000", wireTimeout, execTimeoutMs)
	}
}

func parseContractShellWords(input string) ([]string, error) {
	var words []string
	var word strings.Builder
	inWord := false
	state := byte(0)
	for i := 0; i < len(input); i++ {
		ch := input[i]
		switch state {
		case '\'':
			if ch == '\'' {
				state = 0
			} else {
				word.WriteByte(ch)
			}
			inWord = true
		case '"':
			if ch == '"' {
				state = 0
			} else if ch == '\\' {
				if i+1 >= len(input) {
					return nil, fmt.Errorf("unterminated escape")
				}
				i++
				word.WriteByte(input[i])
			} else {
				word.WriteByte(ch)
			}
			inWord = true
		default:
			switch ch {
			case ' ', '\t', '\n', '\r':
				if inWord {
					words = append(words, word.String())
					word.Reset()
					inWord = false
				}
			case '\'', '"':
				state = ch
				inWord = true
			case '\\':
				if i+1 >= len(input) {
					return nil, fmt.Errorf("unterminated escape")
				}
				i++
				word.WriteByte(input[i])
				inWord = true
			default:
				word.WriteByte(ch)
				inWord = true
			}
		}
	}
	if state != 0 {
		return nil, fmt.Errorf("unterminated quote")
	}
	if inWord {
		words = append(words, word.String())
	}
	return words, nil
}

func TestHealthProbeSanitizesInterpreterProcesses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, processes string
		wantBusy        int
	}{
		{"interpreter scripts", "python3 /tmp/review_cli.py --token=synthetic-secret\nbash /tmp/prepush.sh --token=synthetic-secret\n", 2},
		{"unrelated arguments", "python3 worker.py --note=review_cli\nsh -c echo review_cli\n", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			fixture := filepath.Join(dir, "processes")
			if err := os.WriteFile(fixture, []byte(tc.processes), 0600); err != nil {
				t.Fatal(err)
			}
			for name, script := range map[string]string{
				"ps":    "#!/bin/sh\nprintf 'python3\\nbash\\n'\n",
				"pgrep": "#!/bin/sh\ngrep -E -- \"$2\" \"$PROBE_PROCESS_FIXTURE\" >/dev/null\n",
			} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
			}
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "sh", "-c", healthProbeScript)
			cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "PROBE_PROCESS_FIXTURE="+fixture)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("health probe: %v: %s", err, out)
			}
			if strings.Contains(string(out), "synthetic-secret") || strings.Contains(string(out), "python3") || strings.Contains(string(out), "worker.py") {
				t.Fatalf("health probe leaked arguments: %s", out)
			}
			health := parseSessionHealthOutput(string(out))
			if len(health.BusyProcesses) != tc.wantBusy {
				t.Fatalf("busy=%v, want %d; output=%s", health.BusyProcesses, tc.wantBusy, out)
			}
		})
	}
}
