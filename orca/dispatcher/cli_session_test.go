package dispatcher_test

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/orca/dispatcher"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/store"
)

// TestFakeClaudeCLIHelper is invoked as a subprocess by the fake claude wrapper
// script created in newFakeClaudeCLI. When GO_WANT_FAKE_CLAUDE_CLI is not set,
// it is a no-op test.
func TestFakeClaudeCLIHelper(t *testing.T) {
	if os.Getenv("GO_WANT_FAKE_CLAUDE_CLI") != "1" {
		return
	}

	cliArgs := os.Args
	if idx := slices.Index(cliArgs, "--"); idx >= 0 {
		cliArgs = cliArgs[idx+1:]
	}
	if argsFile := os.Getenv("FAKE_CLAUDE_ARGS_FILE"); argsFile != "" {
		raw, _ := json.Marshal(cliArgs)
		f, err := os.OpenFile(argsFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err == nil {
			_, _ = f.Write(append(raw, '\n'))
			_ = f.Close()
		}
	}

	streamFile := os.Getenv("FAKE_CLAUDE_STREAM_FILE")
	streamData, err := os.ReadFile(streamFile)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read stream file %q: %v\n", streamFile, err)
		os.Exit(1)
	}
	var allLines []string
	for _, line := range strings.Split(string(streamData), "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed != "" {
			allLines = append(allLines, trimmed)
		}
	}

	// Group stream lines into turns ending with a "result" message.
	var turns [][]string
	var cur []string
	for _, line := range allLines {
		cur = append(cur, line)
		var env struct {
			Type string `json:"type"`
		}
		if json.Unmarshal([]byte(line), &env) == nil && env.Type == "result" {
			turns = append(turns, cur)
			cur = nil
		}
	}
	if len(cur) > 0 {
		turns = append(turns, cur)
	}

	// Track which turn index across process invocations via a counter file.
	turnIdx := 0
	if counterFile := os.Getenv("FAKE_CLAUDE_TURN_COUNTER_FILE"); counterFile != "" {
		if b, err := os.ReadFile(counterFile); err == nil {
			_, _ = fmt.Sscanf(strings.TrimSpace(string(b)), "%d", &turnIdx)
		}
	}

	mcpRespFile := os.Getenv("FAKE_CLAUDE_MCP_RESPONSES_FILE")
	recordMCPResp := func(line string) {
		if mcpRespFile == "" {
			return
		}
		f, err := os.OpenFile(mcpRespFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err == nil {
			_, _ = f.WriteString(line + "\n")
			_ = f.Close()
		}
	}

	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	for scanner.Scan() {
		inLine := strings.TrimSpace(scanner.Text())
		if inLine == "" {
			continue
		}
		var msg struct {
			Type      string `json:"type"`
			RequestID string `json:"request_id"`
			Request   struct {
				Subtype string `json:"subtype"`
			} `json:"request"`
		}
		if err := json.Unmarshal([]byte(inLine), &msg); err != nil {
			continue
		}

		switch msg.Type {
		case "control_request":
			if msg.Request.Subtype == "initialize" {
				resp := fmt.Sprintf(`{"type":"control_response","response":{"subtype":"success","request_id":%q,"response":{}}}`, msg.RequestID)
				fmt.Fprintln(os.Stdout, resp)
			}
		case "user":
			if turnIdx >= len(turns) {
				turnIdx = len(turns) - 1
			}
			turnLines := turns[turnIdx]
			turnIdx++
			if counterFile := os.Getenv("FAKE_CLAUDE_TURN_COUNTER_FILE"); counterFile != "" {
				_ = os.WriteFile(counterFile, []byte(fmt.Sprintf("%d", turnIdx)), 0o600)
			}

			for _, outLine := range turnLines {
				fmt.Fprintln(os.Stdout, outLine)
				var outEnv struct {
					Type      string `json:"type"`
					RequestID string `json:"request_id"`
				}
				if json.Unmarshal([]byte(outLine), &outEnv) == nil && outEnv.Type == "control_request" {
					// Wait for the SDK's control_response before proceeding.
					for scanner.Scan() {
						respLine := strings.TrimSpace(scanner.Text())
						if respLine == "" {
							continue
						}
						recordMCPResp(respLine)
						var ctrlResp struct {
							Type string `json:"type"`
						}
						if json.Unmarshal([]byte(respLine), &ctrlResp) == nil && ctrlResp.Type == "control_response" {
							break
						}
					}
				}
			}
		}
	}
	os.Exit(0)
}

type fakeClaudeHarness struct {
	binPath     string
	argsFile    string
	mcpRespFile string
	counterFile string
}

func newFakeClaudeCLI(t *testing.T, streamFixturePath string) *fakeClaudeHarness {
	t.Helper()
	exe, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	absStream, err := filepath.Abs(streamFixturePath)
	if err != nil {
		t.Fatalf("Abs(%q): %v", streamFixturePath, err)
	}

	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args.jsonl")
	mcpRespFile := filepath.Join(dir, "mcp_responses.jsonl")
	counterFile := filepath.Join(dir, "turn_counter.txt")
	binPath := filepath.Join(dir, "fake-claude")

	script := fmt.Sprintf(`#!/bin/sh
export GO_WANT_FAKE_CLAUDE_CLI=1
export FAKE_CLAUDE_STREAM_FILE=%q
export FAKE_CLAUDE_ARGS_FILE=%q
export FAKE_CLAUDE_MCP_RESPONSES_FILE=%q
export FAKE_CLAUDE_TURN_COUNTER_FILE=%q
exec %q -test.run=^TestFakeClaudeCLIHelper$ -- "$@"
`, absStream, argsFile, mcpRespFile, counterFile, exe)

	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake-claude script: %v", err)
	}
	return &fakeClaudeHarness{
		binPath:     binPath,
		argsFile:    argsFile,
		mcpRespFile: mcpRespFile,
		counterFile: counterFile,
	}
}

func (h *fakeClaudeHarness) readInvocations(t *testing.T) [][]string {
	t.Helper()
	data, err := os.ReadFile(h.argsFile)
	if err != nil {
		t.Fatalf("read args file: %v", err)
	}
	var out [][]string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var args []string
		if err := json.Unmarshal([]byte(line), &args); err != nil {
			t.Fatalf("unmarshal args line %q: %v", line, err)
		}
		out = append(out, args)
	}
	return out
}

func (h *fakeClaudeHarness) readMCPResponses(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(h.mcpRespFile)
	if err != nil {
		t.Fatalf("read mcp responses: %v", err)
	}
	var out []string
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) != "" {
			out = append(out, line)
		}
	}
	return out
}

func TestCLISession_DispatcherLaneSpawnVetterAndCost(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()

	if err := st.Backlog().Add(ctx, store.BacklogItem{
		ID:         "item-1",
		RunID:      "run-1",
		Title:      "Implement feature 1",
		Acceptance: "tests pass",
		Priority:   store.PriorityP0,
		Status:     store.BacklogOpen,
	}); err != nil {
		t.Fatalf("Add backlog item: %v", err)
	}

	harness := newFakeClaudeCLI(t, "testdata/cli_stream_lane_spawn.jsonl")
	d := dispatcher.New(dispatcher.Config{
		AgentBackend: model.AgentBackendCLI,
		Provider:     "claude-cli",
		Model:        "claude-sonnet-5-5",
		ClaudeBin:    harness.binPath,
	}, st, drv, clk, nil)
	t.Cleanup(func() { _ = d.Close() })

	ev, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventOwnerMessage,
		Class:   store.EventClassJudgment,
		Summary: "Spawn item-1 now",
	})
	if err != nil {
		t.Fatalf("Append event: %v", err)
	}

	res, err := d.Dispatch(ctx, "run-1", []store.Event{ev})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	// Verify both decisions were recorded: first rejected by vetter (item-missing),
	// second accepted by vetter (item-1 -> lane-1).
	if len(res.Decisions) != 2 {
		t.Fatalf("Decisions count = %d, want 2: %+v", len(res.Decisions), res.Decisions)
	}
	if res.Decisions[0].Vetted {
		t.Fatalf("Decision[0] (item-missing) was vetted, want rejected: %+v", res.Decisions[0])
	}
	if !strings.Contains(res.Decisions[0].Result, "item-missing") {
		t.Fatalf("Decision[0] result = %q, want mention of item-missing", res.Decisions[0].Result)
	}
	if !res.Decisions[1].Vetted {
		t.Fatalf("Decision[1] (item-1) was rejected: %s", res.Decisions[1].Result)
	}

	// Verify lane-1 was spawned in the driver and store.
	if len(drv.Spawns) != 1 || drv.Spawns[0].LaneID != "lane-1" {
		t.Fatalf("drv.Spawns = %+v, want [lane-1]", drv.Spawns)
	}
	ln, err := st.Lanes().Get(ctx, "run-1", "lane-1")
	if err != nil {
		t.Fatalf("Get lane-1: %v", err)
	}
	if ln.Status != lane.StatusRunning {
		t.Fatalf("lane-1 status = %q, want %q", ln.Status, lane.StatusRunning)
	}

	// Verify cost/usage from CLI result event fed BatchResult, Thread, and Decision.
	if res.Usage.InputTokens != 120 || res.Usage.OutputTokens != 45 || res.Usage.CacheCreationInputTokens != 10 || res.Usage.CacheReadInputTokens != 30 {
		t.Fatalf("BatchResult.Usage = %+v, want 120/45/10/30", res.Usage)
	}
	if res.Usage.CostUSD != 0.0125 {
		t.Fatalf("BatchResult.Usage.CostUSD = %v, want 0.0125", res.Usage.CostUSD)
	}
	turnRec, err := st.Threads().GetTurn(ctx, res.ThreadID, res.TurnID)
	if err != nil {
		t.Fatalf("GetTurn: %v", err)
	}
	if turnRec.CostUSD != 0.0125 || turnRec.InputTokens != 120 {
		t.Fatalf("Turn cost/tokens = %v / %d, want 0.0125 / 120", turnRec.CostUSD, turnRec.InputTokens)
	}
	th, err := st.Threads().GetThread(ctx, res.ThreadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if th.InputTokens != 120 || th.OutputTokens != 45 || th.CostUSD != 0.0125 {
		t.Fatalf("Thread totals = in:%d out:%d cost:%v, want 120/45/0.0125", th.InputTokens, th.OutputTokens, th.CostUSD)
	}
	if res.Decisions[0].InputTokens != 120 || res.Decisions[0].OutputTokens != 45 || res.Decisions[0].CostUSD != 0.0125 {
		t.Fatalf("BatchResult.Decisions[0] usage = %+v, want 120/45/0.0125", res.Decisions[0])
	}
	storedDecs, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: "run-1"})
	if err != nil || len(storedDecs) != 2 {
		t.Fatalf("stored Decisions = %+v, err=%v", storedDecs, err)
	}
	if storedDecs[0].InputTokens != 120 || storedDecs[0].OutputTokens != 45 || storedDecs[0].CostUSD != 0.0125 {
		t.Fatalf("stored Decisions[0] usage = %+v, want 120/45/0.0125", storedDecs[0])
	}
}

func TestCLISession_ResumeIDSurvivesSimulatedRestart(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()

	streamPath := filepath.Join(t.TempDir(), "resume_stream.jsonl")
	streamContent := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"sess-resume-42","model":"claude-sonnet-5-5","cwd":"/tmp"}`,
		`{"type":"control_request","request_id":"mcp-1","request":{"subtype":"mcp_message","server_name":"orca","message":{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"directive_note","arguments":{"text":"waiting on owner"}}}}}`,
		`{"type":"assistant","message":{"model":"claude-sonnet-5-5","role":"assistant","content":[{"type":"text","text":"Turn 1 complete."}]},"session_id":"sess-resume-42"}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"sess-resume-42","result":"Turn 1 complete.","num_turns":1,"duration_ms":20,"duration_api_ms":15,"total_cost_usd":0.005,"usage":{"input_tokens":50,"output_tokens":20}}`,
		`{"type":"system","subtype":"init","session_id":"sess-resume-42","model":"claude-sonnet-5-5","cwd":"/tmp"}`,
		`{"type":"control_request","request_id":"mcp-2","request":{"subtype":"mcp_message","server_name":"orca","message":{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"directive_note","arguments":{"text":"still waiting"}}}}}`,
		`{"type":"assistant","message":{"model":"claude-sonnet-5-5","role":"assistant","content":[{"type":"text","text":"Turn 2 complete."}]},"session_id":"sess-resume-42"}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"sess-resume-42","result":"Turn 2 complete.","num_turns":1,"duration_ms":20,"duration_api_ms":15,"total_cost_usd":0.006,"usage":{"input_tokens":60,"output_tokens":25}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(streamPath, []byte(streamContent), 0o600); err != nil {
		t.Fatalf("write stream file: %v", err)
	}

	harness := newFakeClaudeCLI(t, streamPath)
	cfg := dispatcher.Config{
		AgentBackend: model.AgentBackendCLI,
		Provider:     "claude-cli",
		Model:        "claude-sonnet-5-5",
		ClaudeBin:    harness.binPath,
	}

	// First Dispatcher instance runs Turn 1 and persists ResumeID to store.
	d1 := dispatcher.New(cfg, st, drv, clk, nil)
	ev1, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventTimer,
		Class:   store.EventClassJudgment,
		Summary: "check progress 1",
	})
	if err != nil {
		t.Fatal(err)
	}
	res1, err := d1.Dispatch(ctx, "run-1", []store.Event{ev1})
	if err != nil {
		t.Fatalf("d1.Dispatch: %v", err)
	}
	workDir1 := d1.Session("run-1").WorkDir()
	_ = d1.Close()

	th, err := st.Threads().GetThread(ctx, res1.ThreadID)
	if err != nil {
		t.Fatalf("GetThread: %v", err)
	}
	if th.ResumeID != "sess-resume-42" {
		t.Fatalf("stored Thread.ResumeID = %q, want %q", th.ResumeID, "sess-resume-42")
	}

	// Simulate /restart or re-adopt: create a brand-new Dispatcher on the same store.
	d2 := dispatcher.New(cfg, st, drv, clk, nil)
	t.Cleanup(func() { _ = d2.Close() })

	ev2, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventTimer,
		Class:   store.EventClassJudgment,
		Summary: "check progress 2",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d2.Dispatch(ctx, "run-1", []store.Event{ev2}); err != nil {
		t.Fatalf("d2.Dispatch: %v", err)
	}
	workDir2 := d2.Session("run-1").WorkDir()
	if workDir1 == "" || workDir1 != workDir2 {
		t.Fatalf("Session WorkDir changed across restart: %q vs %q", workDir1, workDir2)
	}

	invocations := harness.readInvocations(t)
	if len(invocations) != 2 {
		t.Fatalf("CLI invocation count = %d, want 2: %+v", len(invocations), invocations)
	}
	if slices.Contains(invocations[0], "--resume") {
		t.Fatalf("first invocation unexpectedly passed --resume: %v", invocations[0])
	}
	resumeIdx := slices.Index(invocations[1], "--resume")
	if resumeIdx < 0 || resumeIdx+1 >= len(invocations[1]) || invocations[1][resumeIdx+1] != "sess-resume-42" {
		t.Fatalf("second invocation missing --resume sess-resume-42: %v", invocations[1])
	}

	// Dispatching a 3rd turn on the SAME Dispatcher with a newly added memory and a lane event
	// must reuse the live CLI subprocess (invocation count stays 2) rather than restarting it.
	if _, err := st.Memories().Put(ctx, store.MemoryEntry{
		ID:          "mem-1",
		Name:        "Rule 1",
		Description: "Keep sessions warm",
		Body:        "Do not restart CLI sessions on memory changes.",
		Scope:       "run",
		RunID:       "run-1",
		Type:        "rule",
		ProposedBy:  "owner",
	}); err != nil {
		t.Fatal(err)
	}
	ev3, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		LaneID:  "lane-1",
		Kind:    store.EventLaneReport,
		Class:   store.EventClassJudgment,
		Summary: "lane reported",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d2.Dispatch(ctx, "run-1", []store.Event{ev3}); err != nil {
		t.Fatalf("d2.Dispatch turn 3: %v", err)
	}
	if got := len(harness.readInvocations(t)); got != 2 {
		t.Fatalf("CLI invocation count after warm turn with memory + lane batch = %d, want 2 (no subprocess restart)", got)
	}
}

func TestCLISession_InvestigateToolReadsObjectiveAndRejectsMutation(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()

	runDir := t.TempDir()
	repoDir := t.TempDir()
	objectiveText := "# Objective\nRun dispatcher and planner as real CLI agent sessions.\n"
	if err := os.WriteFile(filepath.Join(runDir, "OBJECTIVE.md"), []byte(objectiveText), 0o600); err != nil {
		t.Fatal(err)
	}

	streamPath := filepath.Join(t.TempDir(), "investigate_stream.jsonl")
	streamContent := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"sess-inv-1","model":"claude-sonnet-5-5","cwd":"/tmp"}`,
		`{"type":"control_request","request_id":"mcp-read-obj","request":{"subtype":"mcp_message","server_name":"orca","message":{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_file","arguments":{"scope":"run","path":"OBJECTIVE.md"}}}}}`,
		`{"type":"control_request","request_id":"mcp-bad-cmd","request":{"subtype":"mcp_message","server_name":"orca","message":{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"read_cmd","arguments":{"program":"gh","args":["pr","merge","123"]}}}}}`,
		`{"type":"control_request","request_id":"mcp-note","request":{"subtype":"mcp_message","server_name":"orca","message":{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"directive_note","arguments":{"text":"investigated objective"}}}}}`,
		`{"type":"assistant","message":{"model":"claude-sonnet-5-5","role":"assistant","content":[{"type":"text","text":"Read OBJECTIVE.md and recorded note."}]},"session_id":"sess-inv-1"}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"sess-inv-1","result":"Read OBJECTIVE.md and recorded note.","num_turns":1,"duration_ms":25,"duration_api_ms":18,"total_cost_usd":0.004,"usage":{"input_tokens":80,"output_tokens":30}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(streamPath, []byte(streamContent), 0o600); err != nil {
		t.Fatal(err)
	}

	harness := newFakeClaudeCLI(t, streamPath)
	d := dispatcher.New(dispatcher.Config{
		AgentBackend: model.AgentBackendCLI,
		Provider:     "claude-cli",
		Model:        "claude-sonnet-5-5",
		ClaudeBin:    harness.binPath,
		Investigate: model.InvestigateConfig{
			RunDir:  runDir,
			RepoDir: repoDir,
		},
	}, st, drv, clk, nil)
	t.Cleanup(func() { _ = d.Close() })

	ev, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventOwnerMessage,
		Class:   store.EventClassJudgment,
		Summary: "check OBJECTIVE.md before acting",
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.Dispatch(ctx, "run-1", []store.Event{ev})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(res.Decisions) != 1 || res.Decisions[0].Tool != dispatcher.ToolDirectiveNote {
		t.Fatalf("Decisions = %+v, want 1 directive.note decision (investigation calls are read-only)", res.Decisions)
	}

	mcpResps := harness.readMCPResponses(t)
	if len(mcpResps) != 3 {
		t.Fatalf("MCP responses count = %d, want 3: %v", len(mcpResps), mcpResps)
	}
	if !strings.Contains(mcpResps[0], "Run dispatcher and planner as real CLI agent sessions.") {
		t.Fatalf("MCP response[0] missing OBJECTIVE.md content: %s", mcpResps[0])
	}
	if !strings.Contains(mcpResps[1], `"isError":true`) {
		t.Fatalf("MCP response[1] expected isError:true for mutating gh pr merge: %s", mcpResps[1])
	}
}

func TestLoopBackendDoesNotAdvertiseInvestigationTools(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()

	fakeClient := model.NewFakeClient(
		model.Response{
			ID:         "msg-loop-tools",
			Model:      dispatcher.DefaultModel,
			StopReason: model.StopReasonEndTurn,
			Message:    model.NewAssistantMessage(model.NewTextBlock("No action.")),
		},
	)
	d := dispatcher.New(dispatcher.Config{
		AgentBackend: model.AgentBackendLoop,
		Investigate: model.InvestigateConfig{
			RunDir:  t.TempDir(),
			RepoDir: t.TempDir(),
		},
	}, st, drv, clk, fakeClient)

	ev, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		Kind:    store.EventOwnerMessage,
		Class:   store.EventClassJudgment,
		Summary: "check status",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.Dispatch(ctx, "run-1", []store.Event{ev}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(fakeClient.Requests) != 1 {
		t.Fatalf("Requests = %d, want 1", len(fakeClient.Requests))
	}
	for _, tool := range fakeClient.Requests[0].Tools {
		if tool.Name == model.ToolReadFile || tool.Name == model.ToolReadCmd {
			t.Fatalf("loop backend unexpectedly advertised investigation tool %q", tool.Name)
		}
	}
}
