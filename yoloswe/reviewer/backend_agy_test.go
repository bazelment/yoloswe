package reviewer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// fakeAgyCLI writes an executable shell script standing in for the agy
// binary, driven purely by exit code and response text — the same contract
// processManager.Start reads (see agent-cli-wrapper/agy/process.go). This
// exercises RunPrompt end to end (argv construction, event loop, resume
// bookkeeping) without a live agy CLI, deterministically.
//
// On success (exitCode 0), it prints a realistic --output-format json
// envelope (see resultPayload in agent-cli-wrapper/agy/session.go) with
// response set to responseText, so the real parser in that package — which
// every RunPrompt call goes through unconditionally — has something valid to
// parse. On failure (nonzero exitCode) it prints nothing, matching a real
// agy process that dies before it can write its result blob; the wrapper's
// process-error path takes over and never reaches the JSON parser.
//
// It also asserts the invocation actually requested --output-format json,
// so a future drift in BuildCLIArgs (agent-cli-wrapper/agy/process.go) that
// silently dropped it would fail here instead of surfacing only as a
// downstream parse error.
func fakeAgyCLI(t *testing.T, responseText string, exitCode int) string {
	t.Helper()
	return fakeAgyCLIWithConversation(t, responseText, exitCode, "conv-fake")
}

// fakeAgyCLIWithConversation is fakeAgyCLI with an explicit conversation_id,
// for tests that need control over whether the echoed id matches (or does
// not match) a requested resume.
func fakeAgyCLIWithConversation(t *testing.T, responseText string, exitCode int, conversationID string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agy")

	var body string
	if exitCode == 0 {
		envelope := fmt.Sprintf(
			`{"conversation_id":%s,"status":"SUCCESS","response":%s,`+
				`"duration_seconds":0.5,"num_turns":1,`+
				`"usage":{"input_tokens":100,"output_tokens":2,"thinking_tokens":0,`+
				`"cache_read_tokens":0,"total_tokens":102}}`,
			mustJSONString(t, conversationID),
			mustJSONString(t, responseText+"\n"),
		)
		body = "printf %s " + shellQuote(envelope) + "\n"
	}

	script := "#!/bin/sh\n" +
		"case \" $* \" in\n" +
		"  *' --output-format json '*) ;;\n" +
		"  *) echo 'fakeAgyCLI: missing --output-format json' >&2; exit 99 ;;\n" +
		"esac\n" +
		body +
		"exit " + strconv.Itoa(exitCode) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake agy CLI: %v", err)
	}
	return path
}

// mustJSONString marshals s as a JSON string literal for embedding in the
// fake CLI's hand-built envelope above.
func mustJSONString(t *testing.T, s string) string {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("marshal response text: %v", err)
	}
	return string(b)
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func TestNewAgyBackend_StopBeforeStartIsNoop(t *testing.T) {
	b := newAgyBackend(Config{BackendType: BackendAgy, Model: "gemini-3.8-flash-medium"})
	if b == nil {
		t.Fatal("expected non-nil backend")
	}
	if err := b.Stop(); err != nil {
		t.Errorf("Stop before Start should be no-op, got error: %v", err)
	}
}

func TestAgyBackend_RunPrompt_Success(t *testing.T) {
	b := &agyBackend{
		config:  Config{Model: "gemini-3.8-flash-medium"},
		cliPath: fakeAgyCLI(t, "AGYOK", 0),
	}

	handler := &recordingHandler{}
	result, err := b.RunPrompt(context.Background(), "review this", handler)
	if err != nil {
		t.Fatalf("RunPrompt failed: %v", err)
	}
	if !result.Success {
		t.Errorf("expected success=true, got %+v", result)
	}
	if result.ResponseText != "AGYOK" {
		t.Errorf("ResponseText = %q, want %q", result.ResponseText, "AGYOK")
	}
	if len(handler.texts) == 0 || handler.texts[0] != "AGYOK" {
		t.Errorf("handler.texts = %v, want [AGYOK]", handler.texts)
	}
	if len(handler.turns) != 1 || !handler.turns[0] {
		t.Errorf("handler.turns = %v, want [true]", handler.turns)
	}
}

func TestAgyBackend_RunPrompt_ProcessFailure(t *testing.T) {
	b := &agyBackend{
		config:  Config{Model: "gemini-3.8-flash-medium"},
		cliPath: fakeAgyCLI(t, "", 1),
	}

	handler := &recordingHandler{}
	result, err := b.RunPrompt(context.Background(), "review this", handler)
	if err == nil {
		t.Fatal("expected error from failing agy process")
	}
	if result == nil {
		t.Fatal("expected non-nil result carrying the error")
	}
	if result.Success {
		t.Errorf("expected success=false, got %+v", result)
	}
	if len(handler.errors) == 0 {
		t.Errorf("expected handler.OnError to be called, got %v", handler.errors)
	}
	if len(handler.turns) != 1 || handler.turns[0] {
		t.Errorf("handler.turns = %v, want [false]", handler.turns)
	}
}

func TestAgyBackend_RunPrompt_ResumeStatusOKOnSuccess(t *testing.T) {
	b := &agyBackend{
		config:  Config{Model: "gemini-3.8-flash-medium", ResumeSessionID: "conv-123"},
		cliPath: fakeAgyCLIWithConversation(t, "AGYOK", 0, "conv-123"),
	}

	result, err := b.RunPrompt(context.Background(), "continue review", &recordingHandler{})
	if err != nil {
		t.Fatalf("RunPrompt failed: %v", err)
	}
	if result.ResumeStatus != ResumeStatusOK {
		t.Errorf("ResumeStatus = %q, want %q", result.ResumeStatus, ResumeStatusOK)
	}
}

// TestAgyBackend_RunPrompt_ResumeStatusUnverifiedOnMismatch pins the other
// half of real verification: agy completing successfully is not enough by
// itself, the echoed conversation_id must actually match what was requested.
func TestAgyBackend_RunPrompt_ResumeStatusUnverifiedOnMismatch(t *testing.T) {
	b := &agyBackend{
		config:  Config{Model: "gemini-3.8-flash-medium", ResumeSessionID: "conv-123"},
		cliPath: fakeAgyCLIWithConversation(t, "AGYOK", 0, "conv-999"),
	}

	result, err := b.RunPrompt(context.Background(), "continue review", &recordingHandler{})
	if err != nil {
		t.Fatalf("RunPrompt failed: %v", err)
	}
	if result.ResumeStatus != ResumeStatusUnverified {
		t.Errorf("ResumeStatus = %q, want %q (echoed conversation_id did not match request)", result.ResumeStatus, ResumeStatusUnverified)
	}
}

func TestAgyBackend_RunPrompt_ResumeStatusUnverifiedOnFailure(t *testing.T) {
	b := &agyBackend{
		config:  Config{Model: "gemini-3.8-flash-medium", ResumeSessionID: "conv-123"},
		cliPath: fakeAgyCLI(t, "", 1),
	}

	result, err := b.RunPrompt(context.Background(), "continue review", &recordingHandler{})
	if err == nil {
		t.Fatal("expected error from failing agy process")
	}
	if result.ResumeStatus != ResumeStatusUnverified {
		t.Errorf("ResumeStatus = %q, want %q", result.ResumeStatus, ResumeStatusUnverified)
	}
}

func TestAgyBackend_RunPrompt_NoResumeLeavesStatusEmpty(t *testing.T) {
	b := &agyBackend{
		config:  Config{Model: "gemini-3.8-flash-medium"},
		cliPath: fakeAgyCLI(t, "AGYOK", 0),
	}

	result, err := b.RunPrompt(context.Background(), "review this", &recordingHandler{})
	if err != nil {
		t.Fatalf("RunPrompt failed: %v", err)
	}
	if result.ResumeStatus != "" {
		t.Errorf("ResumeStatus = %q, want empty (no resume requested)", result.ResumeStatus)
	}
}

func TestAgyBackend_RunPrompt_NoToolEvents(t *testing.T) {
	// agy's print-mode wire format has no tool-call events at all (see the
	// agyBackend doc comment) — RunPrompt must never call
	// OnToolStart/OnToolComplete, unlike the gemini/ACP backend it replaced.
	b := &agyBackend{
		config:  Config{Model: "gemini-3.8-flash-medium"},
		cliPath: fakeAgyCLI(t, "AGYOK", 0),
	}

	handler := &recordingHandler{}
	if _, err := b.RunPrompt(context.Background(), "review this", handler); err != nil {
		t.Fatalf("RunPrompt failed: %v", err)
	}
	if len(handler.toolStarts) != 0 || len(handler.toolEnds) != 0 {
		t.Errorf("expected no tool events, got starts=%v ends=%v", handler.toolStarts, handler.toolEnds)
	}
}

// TestAgyBackend_RunPrompt_EffortDoesNotConflictWithPinnedModel covers the
// documented `--backend agy --effort high` path end to end.
//
// DefaultAgyModel pins medium, and reviewer.Config advertises agy effort as
// (low, medium, high), so a caller asking for high used to produce
// `--model gemini-3.8-flash-medium --effort high` — a hard agy rejection:
//
//	Error: invalid model selection (--model "gemini-3.8-flash-medium"
//	--effort "high"): --model gemini-3.8-flash-medium conflicts with --effort=high
//
// yoloswe/reviewer does not depend on //multiagent/agent, so it cannot reach
// that package's reconcileAgyEffort; the wrapper's own BuildCLIArgs is what
// makes this safe for every caller.
func TestAgyBackend_RunPrompt_EffortDoesNotConflictWithPinnedModel(t *testing.T) {
	resultJSON := `{"conversation_id":"conv-effort","status":"SUCCESS","response":"AGYOK",` +
		`"duration_seconds":0.2,"num_turns":1,` +
		`"usage":{"input_tokens":10,"output_tokens":2,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":12}}`
	// The fake rejects the conflicting pair exactly as the real CLI does, so
	// this fails loudly if reconciliation ever stops happening.
	cliPath := fakeAgyCLIRejectingEffortConflict(t, resultJSON)

	b := &agyBackend{cliPath: cliPath, config: Config{
		Model:  DefaultAgyModel,
		Effort: "high",
	}}

	result, err := b.RunPrompt(context.Background(), "review this", nil)
	if err != nil {
		t.Fatalf("RunPrompt with a pinned model plus an explicit effort failed: %v", err)
	}
	if result == nil {
		t.Fatal("RunPrompt returned a nil result")
	}
	if result.ResponseText != "AGYOK" {
		t.Errorf("ResponseText = %q, want %q", result.ResponseText, "AGYOK")
	}
	if !result.Success {
		t.Errorf("expected success=true, got %+v", result)
	}
}

// fakeAgyCLIRejectingEffortConflict writes a fake agy that mirrors the real
// CLI's refusal to accept a level pinned by --model and repeated in --effort.
func fakeAgyCLIRejectingEffortConflict(t *testing.T, resultJSON string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agy")
	script := `#!/bin/sh
model=""; effort=""
while [ $# -gt 0 ]; do
  case "$1" in
    --model) model="$2"; shift 2 ;;
    --effort) effort="$2"; shift 2 ;;
    *) shift ;;
  esac
done
if [ -n "$effort" ]; then
  case "$model" in
    *-low|*-medium|*-high)
      echo "Error: invalid model selection (--model \"$model\" --effort \"$effort\"): --model $model conflicts with --effort=$effort" >&2
      exit 1 ;;
  esac
fi
cat <<'EOF'
` + resultJSON + `
EOF
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake agy CLI: %v", err)
	}
	return path
}

// TestAgyBackend_RunPrompt_ReportsConversationIDAndUsage pins the outputs a
// caller needs from a completed agy turn.
//
// Reviewer.lastSessionID is set only from OnSessionInfo, and BuildEnvelope
// publishes it as session_id — so reporting an empty id makes the conversation
// id agy assigned unobtainable, and Config.ResumeSessionID unreachable for any
// caller that did not already have the id from somewhere else. Token counts are
// reported by backend_codex.go and backend_claude.go from their own turn usage;
// agy carries the same numbers in its JSON result.
func TestAgyBackend_RunPrompt_ReportsConversationIDAndUsage(t *testing.T) {
	resultJSON := `{"conversation_id":"conv-report-1","status":"SUCCESS","response":"AGYOK",` +
		`"duration_seconds":0.3,"num_turns":1,` +
		`"usage":{"input_tokens":321,"output_tokens":12,"thinking_tokens":4,"cache_read_tokens":40,"total_tokens":377}}`
	cliPath := fakeAgyCLIRejectingEffortConflict(t, resultJSON)

	handler := &recordingHandler{}
	b := &agyBackend{cliPath: cliPath, config: Config{Model: DefaultAgyModel}}

	result, err := b.RunPrompt(context.Background(), "review this", handler)
	if err != nil {
		t.Fatalf("RunPrompt failed: %v", err)
	}
	if got := handler.lastSessionID(); got != "conv-report-1" {
		t.Errorf("handler observed session id %q, want %q — a caller cannot resume without it",
			got, "conv-report-1")
	}
	if result.InputTokens != 321 {
		t.Errorf("InputTokens = %d, want 321", result.InputTokens)
	}
	if result.OutputTokens != 12 {
		t.Errorf("OutputTokens = %d, want 12", result.OutputTokens)
	}
}

// TestAgyEffortLevel maps the reviewer's shared --effort flag onto agy's
// vocabulary, mirroring claudeEffortLevel's contract next door.
func TestAgyEffortLevel(t *testing.T) {
	tests := []struct {
		in     string
		want   string
		wantOK bool
	}{
		{"", "", false},
		{"auto", "", false},
		{"low", "low", true},
		{"medium", "medium", true},
		{"med", "medium", true},
		{"high", "high", true},
		{"MAX", "high", true}, // clamps: high is the most agy offers
		{"hgih", "", false},   // typo warns and drops rather than losing the run
		{"turbo", "", false},
	}
	for _, tt := range tests {
		got, ok := agyEffortLevel(tt.in)
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("agyEffortLevel(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.wantOK)
		}
	}
}

// TestAgyBackend_RunPrompt_UnservableEffortDoesNotForgeAModelID guards the
// interaction between the two: an effort value agy cannot serve must cost the
// LEVEL, never corrupt the model id into one that does not exist.
func TestAgyBackend_RunPrompt_UnservableEffortDoesNotForgeAModelID(t *testing.T) {
	resultJSON := `{"conversation_id":"conv-max","status":"SUCCESS","response":"AGYOK",` +
		`"duration_seconds":0.1,"num_turns":1,` +
		`"usage":{"input_tokens":5,"output_tokens":1,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":6}}`
	cliPath := fakeAgyCLIRejectingUnknownModel(t, resultJSON)

	handler := &recordingHandler{}
	b := &agyBackend{cliPath: cliPath, config: Config{Model: DefaultAgyModel, Effort: "max"}}

	result, err := b.RunPrompt(context.Background(), "review this", handler)
	if err != nil {
		t.Fatalf("RunPrompt with --effort max failed: %v", err)
	}
	if !result.Success {
		t.Errorf("expected success=true, got %+v", result)
	}
	// max clamps to high, so the run retargets onto a real catalog id.
	if got := handler.lastModel(); got != "gemini-3.8-flash-high" {
		t.Errorf("reported model = %q, want %q", got, "gemini-3.8-flash-high")
	}
}

// TestAgyBackend_RunPrompt_ReportsTheRetargetedModel pins that the id reported
// upward is the one agy ran, not the one configured.
func TestAgyBackend_RunPrompt_ReportsTheRetargetedModel(t *testing.T) {
	resultJSON := `{"conversation_id":"conv-retarget","status":"SUCCESS","response":"AGYOK",` +
		`"duration_seconds":0.1,"num_turns":1,` +
		`"usage":{"input_tokens":5,"output_tokens":1,"thinking_tokens":0,"cache_read_tokens":0,"total_tokens":6}}`
	cliPath := fakeAgyCLIRejectingEffortConflict(t, resultJSON)

	handler := &recordingHandler{}
	b := &agyBackend{cliPath: cliPath, config: Config{Model: DefaultAgyModel, Effort: "high"}}

	if _, err := b.RunPrompt(context.Background(), "review this", handler); err != nil {
		t.Fatalf("RunPrompt failed: %v", err)
	}
	if got := handler.lastModel(); got != "gemini-3.8-flash-high" {
		t.Errorf("reported model = %q, want %q — EffectiveModel promises the model actually used",
			got, "gemini-3.8-flash-high")
	}
}

// fakeAgyCLIRejectingUnknownModel mirrors agy's refusal of a model id outside
// its catalog, which is what a forged "-max" suffix would produce.
func fakeAgyCLIRejectingUnknownModel(t *testing.T, resultJSON string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agy")
	script := `#!/bin/sh
model=""
while [ $# -gt 0 ]; do
  case "$1" in
    --model) model="$2"; shift 2 ;;
    *) shift ;;
  esac
done
case "$model" in
  gemini-3.8-flash-low|gemini-3.8-flash-medium|gemini-3.8-flash-high|"") ;;
  *) echo "Error: invalid model selection (--model \"$model\" --effort \"\"): model $model is not recognized as a known model or custom model in settings" >&2
     exit 1 ;;
esac
cat <<'EOF'
` + resultJSON + `
EOF
`
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake agy CLI: %v", err)
	}
	return path
}

// hangingAgyCLI writes a fake agy that never exits and never prints a result,
// standing in for a wedged subprocess. It also ignores --print-timeout, so it
// exercises the backend's own wall-clock backstop rather than agy's flag.
func hangingAgyCLI(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "agy")
	script := "#!/bin/sh\nwhile :; do sleep 30; done\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write hanging agy CLI: %v", err)
	}
	return path
}

// A wedged agy must not hang the review forever. The bound is
// Config.TurnTimeout (a total wall-clock cap), NOT Config.IdleTimeout — agy
// streams nothing until the turn ends, so there is no activity to reset an
// inactivity deadline. Without the backstop in RunPrompt this hangs until the
// Go test timeout.
func TestAgyBackend_RunPrompt_TurnTimeoutBoundsAWedgedProcess(t *testing.T) {
	b := &agyBackend{
		config: Config{
			Model:       "gemini-3.8-flash-medium",
			TurnTimeout: 150 * time.Millisecond,
		},
		cliPath: hangingAgyCLI(t),
	}

	start := time.Now()
	done := make(chan struct{})
	var err error
	go func() {
		defer close(done)
		_, err = b.RunPrompt(context.Background(), "review this", &recordingHandler{})
	}()

	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("RunPrompt did not return: Config.TurnTimeout is not enforced")
	}

	if err == nil {
		t.Fatal("expected an error when the agy process never produces a result")
	}
	if !strings.Contains(err.Error(), "turn timeout") {
		t.Fatalf("error should name the turn timeout, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("took %s; the timeout should bound it near 150ms", elapsed)
	}
}

// Zero must disable the local bound, leaving agy's own --print-timeout default
// in force. Guards against a backstop that fires immediately on a nil/zero timer.
func TestAgyBackend_RunPrompt_TurnTimeoutZeroDisablesTheBound(t *testing.T) {
	b := &agyBackend{
		config:  Config{Model: "gemini-3.8-flash-medium", TurnTimeout: 0},
		cliPath: fakeAgyCLI(t, "AGYOK", 0),
	}

	result, err := b.RunPrompt(context.Background(), "review this", &recordingHandler{})
	if err != nil {
		t.Fatalf("RunPrompt failed with TurnTimeout=0: %v", err)
	}
	if !result.Success {
		t.Fatal("expected success with the turn bound disabled")
	}
}

// The bound must also reach agy itself as --print-timeout, so the CLI enforces
// it in the common case instead of relying on the local backstop alone.
func TestAgyBackend_RunPrompt_TurnTimeoutReachesAgyAsPrintTimeout(t *testing.T) {
	dir := t.TempDir()
	argvPath := filepath.Join(dir, "argv.txt")
	cliPath := filepath.Join(dir, "agy")
	envelope := `{"conversation_id":"conv-fake","status":"SUCCESS","response":"AGYOK\n",` +
		`"duration_seconds":0.5,"num_turns":1,` +
		`"usage":{"input_tokens":1,"output_tokens":1,"thinking_tokens":0,` +
		`"cache_read_tokens":0,"total_tokens":2}}`
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + shellQuote(argvPath) + "\n" +
		"printf %s " + shellQuote(envelope) + "\nexit 0\n"
	if err := os.WriteFile(cliPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write argv-recording agy CLI: %v", err)
	}

	b := &agyBackend{
		config:  Config{Model: "gemini-3.8-flash-medium", TurnTimeout: 90 * time.Second},
		cliPath: cliPath,
	}
	if _, err := b.RunPrompt(context.Background(), "review this", &recordingHandler{}); err != nil {
		t.Fatalf("RunPrompt failed: %v", err)
	}

	argv, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}
	if !strings.Contains(string(argv), "--print-timeout 90s") {
		t.Fatalf("argv should carry --print-timeout 90s, got %q", string(argv))
	}
}

// Reviewer.FollowUp sends a purely context-dependent prompt ("the code has been
// updated based on your previous feedback"), so the second turn must continue
// the conversation the first one established. agy spawns a fresh process per
// turn, so only the backend can thread the id — Config is fixed at New().
func TestAgyBackend_RunPrompt_ThreadsConversationAcrossTurns(t *testing.T) {
	dir := t.TempDir()
	argvPath := filepath.Join(dir, "argv.txt")
	cliPath := filepath.Join(dir, "agy")
	envelope := `{"conversation_id":"conv-server-1","status":"SUCCESS","response":"AGYOK\n",` +
		`"duration_seconds":0.5,"num_turns":1,` +
		`"usage":{"input_tokens":1,"output_tokens":1,"thinking_tokens":0,` +
		`"cache_read_tokens":0,"total_tokens":2}}`
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + shellQuote(argvPath) + "\n" +
		"printf %s " + shellQuote(envelope) + "\nexit 0\n"
	if err := os.WriteFile(cliPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write argv-recording agy CLI: %v", err)
	}

	// No ResumeSessionID: the id must come from the first turn's own result.
	b := &agyBackend{config: Config{Model: "gemini-3.8-flash-medium"}, cliPath: cliPath}

	if _, err := b.RunPrompt(context.Background(), "first", &recordingHandler{}); err != nil {
		t.Fatalf("first RunPrompt failed: %v", err)
	}
	if _, err := b.RunPrompt(context.Background(), "follow up", &recordingHandler{}); err != nil {
		t.Fatalf("second RunPrompt failed: %v", err)
	}

	raw, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 recorded invocations, got %d: %q", len(lines), lines)
	}
	if strings.Contains(lines[0], "--conversation") {
		t.Fatalf("first turn must not request a resume, got %q", lines[0])
	}
	if !strings.Contains(lines[1], "--conversation conv-server-1") {
		t.Fatalf("second turn must continue conv-server-1, got %q", lines[1])
	}
}

// A new turn must not erase the id a prior turn published:
// rendererEventHandler.OnSessionInfo assigns lastSessionID unconditionally, so
// reporting "" at turn start would blank it whenever a follow-up then fails.
func TestAgyBackend_RunPrompt_TurnStartDoesNotBlankAKnownConversationID(t *testing.T) {
	b := &agyBackend{
		config:  Config{Model: "gemini-3.8-flash-medium", ResumeSessionID: "conv-123"},
		cliPath: fakeAgyCLIWithConversation(t, "", 1, "conv-123"),
	}

	handler := &recordingHandler{}
	if _, err := b.RunPrompt(context.Background(), "follow up", handler); err == nil {
		t.Fatal("expected the failing agy process to error")
	}

	if len(handler.sessionIDs) == 0 {
		t.Fatal("expected OnSessionInfo to be called at turn start")
	}
	if got := handler.sessionIDs[0]; got != "conv-123" {
		t.Fatalf("turn start reported %q; reporting \"\" erases Reviewer.lastSessionID", got)
	}
}

// The semantics pin: IdleTimeout must NOT bound an agy turn. agy streams
// nothing until the turn ends, so treating the inactivity value as a wall-clock
// cap kills healthy long reviews — /pr-polish passes --idle-timeout 8m under a
// 40m backstop precisely because "a review making progress runs as long as it
// needs", and a real review on a large diff runs 18min+.
//
// This turn takes materially longer than IdleTimeout and must still succeed.
// It is the test the round-5 fix lacked: a wedged-process test passes under
// either semantics, because a process that never emits is killed either way.
func TestAgyBackend_RunPrompt_IdleTimeoutDoesNotTruncateAProgressingTurn(t *testing.T) {
	dir := t.TempDir()
	cliPath := filepath.Join(dir, "agy")
	envelope := `{"conversation_id":"conv-slow","status":"SUCCESS","response":"AGYOK\n",` +
		`"duration_seconds":0.5,"num_turns":1,` +
		`"usage":{"input_tokens":1,"output_tokens":1,"thinking_tokens":0,` +
		`"cache_read_tokens":0,"total_tokens":2}}`
	// Emits nothing for 400ms — 8x the idle value — then completes normally.
	script := "#!/bin/sh\nsleep 0.4\nprintf %s " + shellQuote(envelope) + "\nexit 0\n"
	if err := os.WriteFile(cliPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write slow agy CLI: %v", err)
	}

	b := &agyBackend{
		config: Config{
			Model:       "gemini-3.8-flash-medium",
			IdleTimeout: 50 * time.Millisecond, // must NOT bound the turn
			TurnTimeout: 10 * time.Second,      // the real cap, comfortably clear
		},
		cliPath: cliPath,
	}

	result, err := b.RunPrompt(context.Background(), "review this", &recordingHandler{})
	if err != nil {
		t.Fatalf("a turn that outlived IdleTimeout was killed: %v", err)
	}
	if !result.Success {
		t.Fatal("expected the slow-but-healthy turn to succeed")
	}
}

// IdleTimeout must not leak into agy's argv either: --print-timeout is a total
// wall-clock bound, so driving it from the inactivity value would cap the turn
// at the wrong quantity inside the CLI itself.
func TestAgyBackend_RunPrompt_IdleTimeoutIsNotSentAsPrintTimeout(t *testing.T) {
	dir := t.TempDir()
	argvPath := filepath.Join(dir, "argv.txt")
	cliPath := filepath.Join(dir, "agy")
	envelope := `{"conversation_id":"conv-fake","status":"SUCCESS","response":"AGYOK\n",` +
		`"duration_seconds":0.5,"num_turns":1,` +
		`"usage":{"input_tokens":1,"output_tokens":1,"thinking_tokens":0,` +
		`"cache_read_tokens":0,"total_tokens":2}}`
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" > " + shellQuote(argvPath) + "\n" +
		"printf %s " + shellQuote(envelope) + "\nexit 0\n"
	if err := os.WriteFile(cliPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write argv-recording agy CLI: %v", err)
	}

	b := &agyBackend{
		config:  Config{Model: "gemini-3.8-flash-medium", IdleTimeout: 8 * time.Minute},
		cliPath: cliPath,
	}
	if _, err := b.RunPrompt(context.Background(), "review this", &recordingHandler{}); err != nil {
		t.Fatalf("RunPrompt failed: %v", err)
	}

	argv, err := os.ReadFile(argvPath)
	if err != nil {
		t.Fatalf("read recorded argv: %v", err)
	}
	if strings.Contains(string(argv), "--print-timeout") {
		t.Fatalf("IdleTimeout leaked into argv as --print-timeout: %q", string(argv))
	}
}
