//go:build integration
// +build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude"
	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/store"
)

const liveSentinel = "ORCH-MODEL-LIVE-42"

type lookupSecretParams struct {
	Key string `json:"key" jsonschema:"required,description=Key name to look up"`
}

func TestHTTPClient_LiveMessagesAPI(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" &&
		os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" &&
		os.Getenv("ANTHROPIC_BASE_URL") == "" {
		t.Skip("skipping live model test: none of ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, or ANTHROPIC_BASE_URL is set")
	}

	modelName := strings.TrimSpace(os.Getenv("ANTHROPIC_MODEL"))
	if modelName == "" {
		modelName = "claude-haiku-4-5-20251001"
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	client := model.NewHTTPClient()
	reg := model.NewToolRegistry()
	var toolInvoked bool
	model.AddTool(reg, "lookup_secret", "Look up the secret token for a given key", func(_ context.Context, p lookupSecretParams) (string, error) {
		toolInvoked = true
		return liveSentinel + ":" + p.Key, nil
	})

	res, err := client.RunToolLoop(
		ctx,
		model.Request{
			Model:  modelName,
			System: "Always call the lookup_secret tool when asked for the secret token, then reply with the exact token returned.",
			Messages: []model.Message{
				model.NewUserMessage("Call lookup_secret with key=\"test\" and reply with the returned token."),
			},
			MaxTokens: 256,
		},
		reg,
		model.WithMaxTurns(4),
	)
	if err != nil {
		t.Fatalf("RunToolLoop live call failed: %v", err)
	}
	if !toolInvoked {
		t.Fatalf("expected lookup_secret tool to be invoked during live tool loop; response=%q", res.Response.Text())
	}
	if !strings.Contains(res.Response.Text(), liveSentinel) {
		t.Fatalf("live response %q did not contain sentinel %q", res.Response.Text(), liveSentinel)
	}
	if res.Usage.InputTokens <= 0 || res.Usage.OutputTokens <= 0 {
		t.Fatalf("expected positive token counts, got %+v", res.Usage)
	}
	if res.Usage.CostUSD <= 0 {
		t.Fatalf("expected positive CostUSD, got %+v", res.Usage)
	}
	t.Logf("live tool loop completed in %d turns: tokens=%d cost=$%.6f text=%q",
		res.TurnCount, res.Usage.TotalTokens(), res.Usage.CostUSD, strings.TrimSpace(res.Response.Text()))
}

type backlogAddInnerParams struct {
	ID       string `json:"id" jsonschema:"required,description=Unique backlog item ID"`
	Title    string `json:"title" jsonschema:"required,description=Short title of the work item"`
	Priority string `json:"priority,omitempty" jsonschema:"enum=p0,enum=p1,enum=p2,enum=p3,description=Item priority"`
}

func TestClaudeSession_SDKMCPBacklogAdd(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" &&
		os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" &&
		os.Getenv("ANTHROPIC_BASE_URL") == "" {
		t.Skip("skipping live claude session test: none of ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, or ANTHROPIC_BASE_URL is set")
	}

	ctx, cancel := context.WithTimeout(t.Context(), 60*time.Second)
	defer cancel()

	st := store.NewMemoryStore(nil)
	const runID = "run-inner-1"
	if err := st.Runs().Create(ctx, store.Run{ID: runID, Objective: "Inner loop smoke"}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	reg := model.NewToolRegistry()
	model.AddTool(reg, "backlog.add", "Add a new item to the run backlog", func(callCtx context.Context, p backlogAddInnerParams) (string, error) {
		prio := store.Priority(strings.ToLower(strings.TrimSpace(p.Priority)))
		if prio == "" {
			prio = store.PriorityP2
		}
		if err := st.Backlog().Add(callCtx, store.BacklogItem{
			ID:       p.ID,
			RunID:    runID,
			Title:    p.Title,
			Priority: prio,
			Status:   store.BacklogOpen,
		}); err != nil {
			return "", err
		}
		return "added backlog item " + p.ID, nil
	})

	bridge := model.NewMCPToolBridge(reg)
	sess := claude.NewSession(
		claude.WithModel("haiku"),
		claude.WithPermissionMode(claude.PermissionModeBypass),
		claude.WithDisablePlugins(),
		claude.WithSystemPrompt("You are the Dispatcher. Call the backlog_add MCP tool when asked to add a backlog item, then reply concisely."),
		claude.WithSDKTools("orca", bridge),
		claude.WithAllowedTools("mcp__orca__backlog_add"),
	)
	if err := sess.Start(ctx); err != nil {
		t.Fatalf("sess.Start: %v", err)
	}
	defer func() { _ = sess.Stop() }()

	res, err := sess.Ask(ctx, `Call backlog_add to add a p1 item with id "item-inner-1" and title "Verify SDK MCP tool call".`)
	if err != nil {
		t.Fatalf("sess.Ask: %v", err)
	}

	got, err := st.Backlog().Get(ctx, runID, "item-inner-1")
	if err != nil {
		t.Fatalf("expected backlog item item-inner-1 in scratch store: %v (response=%q)", err, res.Text)
	}
	if got.Title != "Verify SDK MCP tool call" || got.Priority != store.PriorityP1 {
		t.Fatalf("unexpected backlog item: %+v", got)
	}
	var sessID string
	if info := sess.Info(); info != nil {
		sessID = info.SessionID
	}
	t.Logf("inner loop succeeded: session_id=%s item=%s (%s) tokens=%d+%d cost=$%.6f text=%q",
		sessID, got.ID, got.Priority, res.Usage.InputTokens, res.Usage.OutputTokens, res.Usage.CostUSD, strings.TrimSpace(res.Text))
}

func TestBareOrcaCLIBackend_AdoptAndOwnerInstruction(t *testing.T) {
	if os.Getenv("ANTHROPIC_API_KEY") == "" &&
		os.Getenv("ANTHROPIC_AUTH_TOKEN") == "" &&
		os.Getenv("ANTHROPIC_BASE_URL") == "" {
		t.Skip("skipping live bare orca smoke test: none of ANTHROPIC_API_KEY, ANTHROPIC_AUTH_TOKEN, or ANTHROPIC_BASE_URL is set")
	}

	orcaBin, err := filepath.Abs(filepath.Join("..", "..", "cmd", "orca", "orca_", "orca"))
	if err != nil {
		t.Fatalf("resolve orca binary path: %v", err)
	}
	if _, statErr := os.Stat(orcaBin); statErr != nil {
		t.Fatalf("orca binary not found at %s: %v", orcaBin, statErr)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 120*time.Second)
	defer cancel()

	root := t.TempDir()
	const runID = "20261114-trial-m1"
	const sentinel = "VERIFY-RESUME-ID-20261114"
	dir := filepath.Join(root, ".swarm", runID)
	if err := os.MkdirAll(filepath.Join(dir, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixtureFile := func(path, content string) {
		if wErr := os.WriteFile(path, []byte(content), 0o644); wErr != nil {
			t.Fatal(wErr)
		}
	}
	writeFixtureFile(filepath.Join(dir, "state.json"), `{"config":{"goal":"trial m1: replace one-shot CLI loop with agent sessions","base":"main","target":"main","repo":"bazelment/yoloswe","merge":"none",
"phases":[{"name":"swe"},{"name":"clean"},{"name":"review"}]},
"tasks":[{"id":"m2-default","title":"default prompts","branch":"swarm-lane/`+runID+`/m2-default","priority":"p1","status":"running","phase":"review","round":1,"session_id":"sess-m2","terminals":{"review":{"1":"term-m2-review"}},"pr":5705,"pr_head":"bbb"}]}`)
	writeFixtureFile(filepath.Join(dir, "OBJECTIVE.md"), "# "+runID+"\nGoal: trial m1: replace one-shot CLI loop with agent sessions.\nReview checklist sentinel: "+sentinel+"\n")
	writeFixtureFile(filepath.Join(dir, "PROGRESS.md"), "Next: m2-default review (#5705).\n")
	writeFixtureFile(filepath.Join(dir, "EVENTS"), "2026-11-14T06:35:00Z\tm2-default\tstarted\treview term-m2-review\n")

	var mu sync.Mutex
	label := "claude"
	var steeredMessages []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/code-runs/"+runID:
			mu.Lock()
			defer mu.Unlock()
			if r.Method == http.MethodPut {
				b, _ := io.ReadAll(r.Body)
				var body struct {
					Label *string `json:"externalOrchestratorLabel"`
				}
				if json.Unmarshal(b, &body) == nil && body.Label != nil {
					label = *body.Label
				}
				_, _ = io.WriteString(w, `{}`)
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]string{"id": runID, "externalOrchestratorLabel": label})
		case r.URL.Path == "/api/code-sessions":
			_, _ = io.WriteString(w, `{"sessions":[]}`)
		case r.URL.Path == "/api/code-sessions/sess-m2":
			_, _ = io.WriteString(w, `{"id":"sess-m2","status":"active","repositoryFullName":"bazelment/yoloswe","terminals":[{"id":"term-m2-review","status":"running"}]}`)
		case r.URL.Path == "/api/code-terminals/term-m2-review":
			_, _ = io.WriteString(w, `{"id":"term-m2-review","codeSessionId":"sess-m2","status":"running","agentState":"working"}`)
		case r.URL.Path == "/api/code-terminals/term-m2-review/input" && r.Method == http.MethodPost:
			b, _ := io.ReadAll(r.Body)
			var body struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(b, &body)
			mu.Lock()
			steeredMessages = append(steeredMessages, body.Text)
			mu.Unlock()
			_, _ = io.WriteString(w, `{}`)
		case r.URL.Path == "/api/code-terminals/term-m2-review/phase-report":
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	cfgPath := filepath.Join(root, "orca.json")
	cfgBytes, _ := json.Marshal(map[string]string{
		"nebula_api_url": srv.URL,
		"nebula_token":   "test-token",
	})
	writeFixtureFile(cfgPath, string(cfgBytes))
	storeDir := filepath.Join(root, "store")

	inR, inW := io.Pipe()
	defer inR.Close()
	defer inW.Close()

	var stdout, stderr bytes.Buffer
	adopted := make(chan struct{})
	var adoptOnce sync.Once
	outWriter := writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		n, wErr := stdout.Write(p)
		cur := stdout.String()
		mu.Unlock()
		if strings.Contains(cur, "adopted "+runID) {
			adoptOnce.Do(func() { close(adopted) })
		}
		return n, wErr
	})

	cmd := exec.CommandContext(ctx, orcaBin,
		"--agent-backend", "cli",
		"--model-provider", "claude-cli",
		"--config", cfgPath,
		"--store-dir", storeDir,
		"--repo", "bazelment/yoloswe",
		"--dispatcher-model", "sonnet",
		"--concierge-model", "haiku",
		"--router-model", "haiku",
	)
	cmd.Env = append(os.Environ(),
		"ORCA_SWARM_ROOTS="+filepath.Join(root, ".swarm"),
		"ORCA_NEBULA_SWARM_SH="+filepath.Join(root, "unused_swarm_nebula.sh"),
	)
	cmd.Stdin = inR
	cmd.Stdout = outWriter
	cmd.Stderr = &stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start bare orca: %v", err)
	}

	if _, err := io.WriteString(inW, "/adopt "+runID+"\n"); err != nil {
		t.Fatalf("write /adopt: %v", err)
	}
	select {
	case <-adopted:
	case <-ctx.Done():
		t.Fatalf("timed out waiting for adoption; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	dbPath, err := store.RunDBPath(storeDir, "front-desk")
	if err != nil {
		t.Fatal(err)
	}

	instruction := "Steer lane m2-default on 20261114-trial-m1: read OBJECTIVE.md in the run dir and include its exact review checklist sentinel in the steer message"
	if _, err := io.WriteString(inW, instruction+"\n"); err != nil {
		t.Fatalf("write instruction: %v", err)
	}

	// Poll the SQLite store until the Dispatcher's CLI agent session executes a tool call on 20261114-trial-m1.
	var dispDecisions []store.Decision
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
waitLoop:
	for {
		select {
		case <-ctx.Done():
			mu.Lock()
			outStr := stdout.String()
			mu.Unlock()
			t.Fatalf("timed out waiting for dispatcher decision on %s;\nstdout:\n%s\nstderr:\n%s", runID, outStr, stderr.String())
		case <-ticker.C:
			st, openErr := store.OpenSQLite(dbPath, clock.NewRealClock())
			if openErr != nil {
				continue
			}
			decs, listErr := st.Decisions().List(ctx, store.DecisionFilter{RunID: runID})
			_ = st.Close()
			if listErr != nil {
				continue
			}
			var matched []store.Decision
			for _, d := range decs {
				if d.Actor == "dispatcher" && (d.InputTokens+d.OutputTokens > 0 || d.CostUSD > 0) {
					matched = append(matched, d)
				}
			}
			if len(matched) > 0 {
				dispDecisions = matched
				break waitLoop
			}
		}
	}

	_, _ = io.WriteString(inW, "/exit\n")
	_ = inW.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("bare orca exited with error: %v\nstderr:\n%s", err, stderr.String())
	}

	st, err := store.OpenSQLite(dbPath, clock.NewRealClock())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	for _, rID := range []string{"front-desk", runID} {
		decs, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: rID})
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range decs {
			t.Logf("DECISION row: seq=%d run_id=%s category=%s actor=%s tool=%s lane_id=%s vetted=%t tokens=%d+%d cost=$%.6f reason=%q result=%q args=%s",
				d.Seq, d.RunID, d.Category, d.Actor, d.Tool, d.LaneID, d.Vetted,
				d.InputTokens, d.OutputTokens, d.CostUSD, d.Reason, d.Result, string(d.Args))
		}
	}

	threads, err := st.Threads().ListThreads(ctx, store.ThreadFilter{RunID: runID, Role: store.ThreadRoleDispatcher})
	if err != nil {
		t.Fatal(err)
	}
	if len(threads) == 0 || threads[0].ResumeID == "" {
		t.Fatalf("expected dispatcher thread with persisted ResumeID, got %+v", threads)
	}
	t.Logf("DISPATCHER THREAD: id=%s run_id=%s role=%s status=%s resume_id=%s tokens=%d+%d cost=$%.6f",
		threads[0].ID, threads[0].RunID, threads[0].Role, threads[0].Status, threads[0].ResumeID,
		threads[0].InputTokens, threads[0].OutputTokens, threads[0].CostUSD)

	foundSteer := false
	for _, d := range dispDecisions {
		if d.Tool == "lane.steer" && d.LaneID == "m2-default" && d.Vetted && strings.Contains(string(d.Args), sentinel) {
			foundSteer = true
		}
	}
	if !foundSteer {
		t.Fatalf("expected vetted lane.steer decision on m2-default containing sentinel %q; got %+v", sentinel, dispDecisions)
	}
	mu.Lock()
	gotSteer := append([]string(nil), steeredMessages...)
	mu.Unlock()
	if len(gotSteer) == 0 || !strings.Contains(gotSteer[0], sentinel) {
		t.Fatalf("expected Nebula terminal steer containing %q, got %v", sentinel, gotSteer)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

var (
	_ model.ModelClient = (*model.HTTPClient)(nil)
	_ model.ModelClient = (*model.FakeClient)(nil)
)
