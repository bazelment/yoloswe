package dispatcher_test

import (
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
	"github.com/bazelment/yoloswe/orca/recipe"
	"github.com/bazelment/yoloswe/orca/store"
)

// TestFakeClaudeCLIHelper is invoked as a subprocess by the fake claude wrapper
// script created in newFakeClaudeCLI. When GO_WANT_FAKE_CLAUDE_CLI is not set,
// it is a no-op test.
func TestFakeClaudeCLIHelper(t *testing.T) {
	if os.Getenv("GO_WANT_FAKE_CLAUDE_CLI") != "1" {
		return
	}
	model.RunFakeClaudeStreamCLI(
		os.Getenv("FAKE_CLAUDE_STREAM_FILE"),
		os.Getenv("FAKE_CLAUDE_ARGS_FILE"),
		os.Getenv("FAKE_CLAUDE_MCP_RESPONSES_FILE"),
		os.Getenv("FAKE_CLAUDE_TURN_COUNTER_FILE"),
	)
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

func TestCLISession_RawEvidenceInspectionAndForward(t *testing.T) {
	t.Parallel()
	clk, st, drv := setupTestEnv(t)
	ctx := t.Context()

	runDir := t.TempDir()
	laneDir := filepath.Join(runDir, "lanes", "lazyblk-perf")
	if err := os.MkdirAll(laneDir, 0o755); err != nil {
		t.Fatal(err)
	}
	rawSummary := `{"claim":"66x fewer bytes read","seed_bytes":32,"duplicated_blocks":2048,"note":"test files were one 32-byte seed repeated"}` + "\n"
	if err := os.WriteFile(filepath.Join(laneDir, "summary.json"), []byte(rawSummary), 0o600); err != nil {
		t.Fatal(err)
	}

	ln := lane.Lane{
		ID:         "lazyblk-perf",
		RunID:      "run-1",
		ItemID:     "lazyblk-perf",
		Title:      "Benchmark lazy block device",
		Status:     lane.StatusBlocked,
		Phase:      recipe.PhaseReview,
		Round:      1,
		RecipeName: recipe.DefaultName,
		Worktree:   laneDir,
	}
	ln.RecordAttempt(recipe.PhaseSWE, 1, "sess-lazy-1", "term-lazy-swe-1")
	ln.RecordAttempt(recipe.PhaseReview, 1, "sess-lazy-1", "term-lazy-rev-1")
	if err := st.Lanes().Upsert(ctx, ln); err != nil {
		t.Fatal(err)
	}
	peerLn := lane.Lane{
		ID:         "transport",
		RunID:      "run-1",
		ItemID:     "transport",
		Title:      "Transport layer",
		Status:     lane.StatusRunning,
		Phase:      recipe.PhaseSWE,
		Round:      1,
		RecipeName: recipe.DefaultName,
		SessionID:  "sess-transport-1",
		TerminalID: "term-transport-1",
	}
	peerLn.RecordAttempt(recipe.PhaseSWE, 1, "sess-transport-1", "term-transport-1")
	if err := st.Lanes().Upsert(ctx, peerLn); err != nil {
		t.Fatal(err)
	}

	streamPath := filepath.Join(t.TempDir(), "cli_inspect_forward.jsonl")
	streamContent := strings.Join([]string{
		`{"type":"system","subtype":"init","session_id":"sess-cli-judg-1","model":"claude-sonnet-5-5","cwd":"/tmp"}`,
		`{"type":"control_request","request_id":"mcp-inspect","request":{"subtype":"mcp_message","server_name":"orca","message":{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"read_lane","arguments":{"lane_id":"lazyblk-perf","path":"summary.json","reason":"verify 66x claim from raw summary.json"}}}}}`,
		`{"type":"control_request","request_id":"mcp-fwd","request":{"subtype":"mcp_message","server_name":"orca","message":{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"lane_forward","arguments":{"from_lane_id":"lazyblk-perf","to_lane_id":"transport","excerpt":"32-byte repeated seed inflated compression ratio 66x","reason":"share benchmark pitfall with transport"}}}}}`,
		`{"type":"control_request","request_id":"mcp-retry","request":{"subtype":"mcp_message","server_name":"orca","message":{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"lane_retry","arguments":{"lane_id":"lazyblk-perf","phase":"swe","round":2,"model":"claude-opus-4-6","note":"Voided reported win: summary.json shows one 32-byte seed repeated.","reason":"raw evidence check failed"}}}}}`,
		`{"type":"assistant","message":{"model":"claude-sonnet-5-5","role":"assistant","content":[{"type":"text","text":"Inspected summary.json, forwarded finding to transport, and sent lazyblk-perf back for round 2."}]},"session_id":"sess-cli-judg-1"}`,
		`{"type":"result","subtype":"success","is_error":false,"session_id":"sess-cli-judg-1","result":"Done.","num_turns":1,"duration_ms":30,"duration_api_ms":20,"total_cost_usd":0.008,"usage":{"input_tokens":100,"output_tokens":40}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(streamPath, []byte(streamContent), 0o600); err != nil {
		t.Fatal(err)
	}

	harness := newFakeClaudeCLI(t, streamPath)
	d := dispatcher.New(dispatcher.Config{
		AgentBackend:  model.AgentBackendCLI,
		Provider:      "claude-cli",
		Model:         "claude-sonnet-5-5",
		ClaudeBin:     harness.binPath,
		AllowedModels: []string{"claude-opus-4-6", "claude-sonnet-4-6"},
		Investigate:   model.InvestigateConfig{RunDir: runDir},
	}, st, drv, clk, nil)
	t.Cleanup(func() { _ = d.Close() })

	ev, err := st.Events().Append(ctx, store.Event{
		RunID:   "run-1",
		LaneID:  "lazyblk-perf",
		Kind:    store.EventLaneReport,
		Class:   store.EventClassJudgment,
		Urgent:  true,
		Summary: "lazyblk-perf review reported done: 66x fewer bytes read",
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := d.Dispatch(ctx, "run-1", []store.Event{ev})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}
	if len(res.Rejected) != 0 || len(res.Decisions) != 3 {
		t.Fatalf("Decisions=%d Rejected=%v, want 3 decisions and 0 rejections", len(res.Decisions), res.Rejected)
	}
	mcpResps := harness.readMCPResponses(t)
	if len(mcpResps) != 3 || !strings.Contains(mcpResps[0], "one 32-byte seed repeated") {
		t.Fatalf("MCP responses = %v, want read_lane output with raw summary.json content", mcpResps)
	}
	if len(drv.Steers) != 1 || drv.Steers[0].SessionID != "sess-transport-1" || !strings.Contains(drv.Steers[0].Message, "32-byte repeated seed") {
		t.Fatalf("Steers = %+v, want forwarded excerpt on sess-transport-1", drv.Steers)
	}
	if len(drv.PhaseStarts) != 1 || drv.PhaseStarts[0].Model != "claude-opus-4-6" || drv.PhaseStarts[0].Round != 2 {
		t.Fatalf("PhaseStarts = %+v, want round 2 retry with claude-opus-4-6", drv.PhaseStarts)
	}
}
