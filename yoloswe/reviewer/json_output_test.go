package reviewer

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestExtractReviewBody_WellFormed(t *testing.T) {
	input := `{"verdict":"accepted","summary":"looks good","issues":[]}`
	body, err := extractReviewBody(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body.Verdict != "accepted" {
		t.Errorf("verdict = %q, want accepted", body.Verdict)
	}
	if body.RawText != "" {
		t.Errorf("raw_text should be empty on success, got %q", body.RawText)
	}
}

func TestExtractReviewBody_FencedJSON(t *testing.T) {
	input := "```json\n{\"verdict\":\"rejected\",\"issues\":[{\"severity\":\"high\",\"file\":\"a.go\",\"line\":1,\"message\":\"bug\"}]}\n```"
	body, err := extractReviewBody(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body.Verdict != "rejected" {
		t.Errorf("verdict = %q, want rejected", body.Verdict)
	}
	if len(body.Issues) != 1 || body.Issues[0].Severity != "high" {
		t.Errorf("issues not parsed: %+v", body.Issues)
	}
}

func TestExtractReviewBody_NarrationThenJSON(t *testing.T) {
	// Reproduces the codex behavior: intra-step narration precedes the final
	// JSON object. The last balanced {...} should win.
	input := `I'm inspecting the branch diff. No findings.

{"verdict":"accepted","summary":"correct","issues":[]}`
	body, err := extractReviewBody(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body.Verdict != "accepted" {
		t.Errorf("verdict = %q, want accepted", body.Verdict)
	}
}

func TestExtractReviewBody_BracesInsideStrings(t *testing.T) {
	// Brace scanner must respect string quoting.
	input := `{"verdict":"accepted","summary":"template {value}","issues":[]}`
	body, err := extractReviewBody(input)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if body.Summary != "template {value}" {
		t.Errorf("summary = %q", body.Summary)
	}
}

func TestExtractReviewBody_Malformed(t *testing.T) {
	input := "not json at all, just prose"
	body, err := extractReviewBody(input)
	if err == nil {
		t.Fatal("expected error for non-JSON input")
	}
	if body.RawText != input {
		t.Errorf("raw_text = %q, want the original input", body.RawText)
	}
}

func TestExtractReviewBody_Empty(t *testing.T) {
	_, err := extractReviewBody("")
	if err == nil {
		t.Fatal("expected error for empty input")
	}
}

func TestBuildEnvelope_SuccessPath(t *testing.T) {
	result := &ReviewResult{
		ResponseText: `{"verdict":"accepted","summary":"ok","issues":[]}`,
		Success:      true,
		DurationMs:   1234,
		InputTokens:  100,
		OutputTokens: 200,
	}
	env := BuildEnvelope(result, BackendCodex, "gpt-x", "sess-1")
	if env.Status != StatusOK {
		t.Errorf("status = %s, want ok", env.Status)
	}
	if env.Review.Verdict != "accepted" {
		t.Errorf("verdict = %q", env.Review.Verdict)
	}
	if env.SchemaVersion != JSONSchemaVersion {
		t.Errorf("schema_version = %d", env.SchemaVersion)
	}
	if env.SessionID != "sess-1" || env.Backend != "codex" || env.Model != "gpt-x" {
		t.Errorf("metadata wrong: %+v", env)
	}
	if env.DurationMs != 1234 || env.InputTokens != 100 || env.OutputTokens != 200 {
		t.Errorf("counters wrong: %+v", env)
	}
}

func TestBuildEnvelope_BackendError(t *testing.T) {
	result := &ReviewResult{
		ErrorMessage: "backend crashed",
		Success:      false,
	}
	env := BuildEnvelope(result, BackendCodex, "m", "")
	if env.Status != StatusError {
		t.Errorf("status = %s, want error", env.Status)
	}
	if env.Error != "backend crashed" {
		t.Errorf("error = %q", env.Error)
	}
}

func TestBuildEnvelope_MissingVerdict(t *testing.T) {
	// Syntactically valid JSON without a required verdict must not be reported
	// as ok. Regression guard for an earlier contract gap where any parseable
	// object was treated as a success.
	result := &ReviewResult{
		ResponseText: `{"summary":"looks fine","issues":[]}`,
		Success:      true,
	}
	env := BuildEnvelope(result, BackendCodex, "m", "")
	if env.Status != StatusError {
		t.Errorf("status = %s, want error for missing verdict", env.Status)
	}
	if !strings.Contains(env.Error, "verdict") {
		t.Errorf("error = %q, want mention of verdict", env.Error)
	}
}

func TestBuildEnvelope_UnknownVerdict(t *testing.T) {
	result := &ReviewResult{
		ResponseText: `{"verdict":"maybe","summary":"unclear"}`,
		Success:      true,
	}
	env := BuildEnvelope(result, BackendCodex, "m", "")
	if env.Status != StatusError {
		t.Errorf("status = %s, want error for unknown verdict", env.Status)
	}
}

func TestBuildEnvelope_IssueMissingFields(t *testing.T) {
	result := &ReviewResult{
		ResponseText: `{"verdict":"rejected","issues":[{"file":"a.go"}]}`,
		Success:      true,
	}
	env := BuildEnvelope(result, BackendCodex, "m", "")
	if env.Status != StatusError {
		t.Errorf("status = %s, want error for issue missing severity/message", env.Status)
	}
}

func TestBuildEnvelope_IssueMissingFile(t *testing.T) {
	// Downstream automation places PR comments using file/line. An issue
	// without a file pin can't be acted on, so the envelope must reject it.
	result := &ReviewResult{
		ResponseText: `{"verdict":"rejected","issues":[{"severity":"high","message":"bug","line":1}]}`,
		Success:      true,
	}
	env := BuildEnvelope(result, BackendCodex, "m", "")
	if env.Status != StatusError {
		t.Errorf("status = %s, want error for issue missing file", env.Status)
	}
}

func TestBuildEnvelope_IssueMissingLine(t *testing.T) {
	// The prompt instructs reviewers to cite file and line range; downstream
	// automation needs both to anchor PR comments precisely.
	result := &ReviewResult{
		ResponseText: `{"verdict":"rejected","issues":[{"severity":"high","file":"a.go","message":"bug"}]}`,
		Success:      true,
	}
	env := BuildEnvelope(result, BackendCodex, "m", "")
	if env.Status != StatusError {
		t.Errorf("status = %s, want error for issue missing line", env.Status)
	}
}

func TestBuildEnvelope_UnknownSeverity(t *testing.T) {
	result := &ReviewResult{
		ResponseText: `{"verdict":"rejected","issues":[{"severity":"blocker","file":"a.go","line":1,"message":"bug"}]}`,
		Success:      true,
	}
	env := BuildEnvelope(result, BackendCodex, "m", "")
	if env.Status != StatusError {
		t.Errorf("status = %s, want error for unknown severity", env.Status)
	}
}

func TestBuildEnvelope_RejectedWithoutIssues(t *testing.T) {
	// A "rejected" verdict with no issues makes no sense — the reviewer
	// rejected the change without naming anything to fix.
	result := &ReviewResult{
		ResponseText: `{"verdict":"rejected","summary":"bad","issues":[]}`,
		Success:      true,
	}
	env := BuildEnvelope(result, BackendCodex, "m", "")
	if env.Status != StatusError {
		t.Errorf("status = %s, want error for rejected with no issues", env.Status)
	}
}

func TestBuildEnvelope_AcceptedWithBlockingIssue(t *testing.T) {
	// "accepted" with a high/critical issue is a contradiction: those
	// severities block merge by definition.
	result := &ReviewResult{
		ResponseText: `{"verdict":"accepted","issues":[{"severity":"high","file":"a.go","line":1,"message":"bug"}]}`,
		Success:      true,
	}
	env := BuildEnvelope(result, BackendCodex, "m", "")
	if env.Status != StatusError {
		t.Errorf("status = %s, want error for accepted with blocking issue", env.Status)
	}
}

func TestBuildEnvelope_AcceptedWithLowIssue(t *testing.T) {
	// Low/medium issues are fine alongside accepted — they're suggestions,
	// not blockers. Regression guard against over-tightening.
	result := &ReviewResult{
		ResponseText: `{"verdict":"accepted","issues":[{"severity":"low","file":"a.go","line":3,"message":"nit"}]}`,
		Success:      true,
	}
	env := BuildEnvelope(result, BackendCodex, "m", "")
	if env.Status != StatusOK {
		t.Errorf("status = %s, want ok for accepted with only low issues", env.Status)
	}
}

func TestBuildEnvelope_UnparseableText(t *testing.T) {
	result := &ReviewResult{
		ResponseText: "the reviewer refused to produce JSON",
		Success:      true,
	}
	env := BuildEnvelope(result, BackendCursor, "composer-2", "")
	if env.Status != StatusError {
		t.Errorf("status = %s, want error on parse failure", env.Status)
	}
	if env.Review.RawText == "" {
		t.Error("raw_text should be preserved when JSON parsing fails")
	}
}

func TestBuildEnvelope_NilResult(t *testing.T) {
	env := BuildEnvelope(nil, BackendCodex, "m", "")
	if env.Status != StatusError {
		t.Errorf("status = %s, want error", env.Status)
	}
	if env.Error == "" {
		t.Error("error should be populated for nil result")
	}
}

func TestBuildEnvelope_GeminiBackend(t *testing.T) {
	result := &ReviewResult{
		ResponseText: `{"verdict":"accepted","summary":"lgtm","issues":[]}`,
		Success:      true,
		DurationMs:   5000,
	}
	env := BuildEnvelope(result, BackendGemini, "gemini-2.5-pro", "sess-gemini-1")
	if env.Status != StatusOK {
		t.Errorf("status = %s, want ok", env.Status)
	}
	if env.Backend != "gemini" {
		t.Errorf("backend = %q, want gemini", env.Backend)
	}
	if env.Model != "gemini-2.5-pro" {
		t.Errorf("model = %q, want gemini-2.5-pro", env.Model)
	}
	if env.SessionID != "sess-gemini-1" {
		t.Errorf("session_id = %q, want sess-gemini-1", env.SessionID)
	}
	if env.Review.Verdict != "accepted" {
		t.Errorf("verdict = %q, want accepted", env.Review.Verdict)
	}
	if env.SchemaVersion != JSONSchemaVersion {
		t.Errorf("schema_version = %d, want %d", env.SchemaVersion, JSONSchemaVersion)
	}
}

func TestBuildEnvelope_GeminiBackendError(t *testing.T) {
	result := &ReviewResult{
		ErrorMessage: "gemini: ACP client failed to start",
		Success:      false,
	}
	env := BuildEnvelope(result, BackendGemini, "gemini-2.5-pro", "")
	if env.Status != StatusError {
		t.Errorf("status = %s, want error", env.Status)
	}
	if env.Backend != "gemini" {
		t.Errorf("backend = %q, want gemini", env.Backend)
	}
	if env.Error != "gemini: ACP client failed to start" {
		t.Errorf("error = %q, want gemini error", env.Error)
	}
}

func TestPrintJSONResult_RoundTrip(t *testing.T) {
	env := ResultEnvelope{
		SchemaVersion: JSONSchemaVersion,
		Status:        StatusOK,
		Backend:       "codex",
		Model:         "m",
		Review:        ReviewBody{Verdict: "accepted"},
	}
	var buf bytes.Buffer
	if err := PrintJSONResult(&buf, env); err != nil {
		t.Fatalf("print: %v", err)
	}
	if !strings.HasSuffix(buf.String(), "\n") {
		t.Error("expected trailing newline")
	}
	var decoded ResultEnvelope
	if err := json.Unmarshal(buf.Bytes(), &decoded); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Review.Verdict != "accepted" {
		t.Errorf("decoded verdict = %q", decoded.Review.Verdict)
	}
}

func TestLastBalancedObject_PicksLast(t *testing.T) {
	input := `prose {"a":1} more prose {"b":2} end`
	got, ok := lastBalancedObject(input)
	if !ok {
		t.Fatal("expected match")
	}
	if got != `{"b":2}` {
		t.Errorf("got %q", got)
	}
}

func TestLastBalancedObject_NestedObjects(t *testing.T) {
	input := `{"outer":{"inner":"v"}}`
	got, ok := lastBalancedObject(input)
	if !ok {
		t.Fatal("expected match")
	}
	if got != input {
		t.Errorf("got %q", got)
	}
}

func TestStripJSONFence_Variants(t *testing.T) {
	cases := map[string]string{
		"```json\n{\"k\":1}\n```": `{"k":1}`,
		"```\n{\"k\":1}\n```":     `{"k":1}`,
		`{"k":1}`:                 `{"k":1}`,
		"```json\n{\"k\":1}":      "```json\n{\"k\":1}", // unterminated → unchanged
	}
	for input, want := range cases {
		got := stripJSONFence(input)
		if got != want {
			t.Errorf("stripJSONFence(%q) = %q, want %q", input, got, want)
		}
	}
}
