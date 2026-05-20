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
		ResumeStatus: "ok",
	}
	env := BuildEnvelope(result, BackendCodex, "gpt-x", "sess-1", "")
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
	if env.ResumeStatus != "ok" {
		t.Errorf("resume_status = %q, want ok", env.ResumeStatus)
	}
}

func TestBuildEnvelope_BackendError(t *testing.T) {
	result := &ReviewResult{
		ErrorMessage: "backend crashed",
		Success:      false,
		ResumeStatus: ResumeStatusFallback,
	}
	env := BuildEnvelope(result, BackendCodex, "m", "", "")
	if env.Status != StatusError {
		t.Errorf("status = %s, want error", env.Status)
	}
	if env.Error != "backend crashed" {
		t.Errorf("error = %q", env.Error)
	}
	if env.ResumeStatus != ResumeStatusFallback {
		t.Errorf("resume_status = %q, want %q", env.ResumeStatus, ResumeStatusFallback)
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
	env := BuildEnvelope(result, BackendCodex, "m", "", "")
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
	env := BuildEnvelope(result, BackendCodex, "m", "", "")
	if env.Status != StatusError {
		t.Errorf("status = %s, want error for unknown verdict", env.Status)
	}
}

func TestBuildEnvelope_IssueMissingFields(t *testing.T) {
	result := &ReviewResult{
		ResponseText: `{"verdict":"rejected","issues":[{"file":"a.go"}]}`,
		Success:      true,
	}
	env := BuildEnvelope(result, BackendCodex, "m", "", "")
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
	env := BuildEnvelope(result, BackendCodex, "m", "", "")
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
	env := BuildEnvelope(result, BackendCodex, "m", "", "")
	if env.Status != StatusError {
		t.Errorf("status = %s, want error for issue missing line", env.Status)
	}
}

func TestBuildEnvelope_UnknownSeverity(t *testing.T) {
	result := &ReviewResult{
		ResponseText: `{"verdict":"rejected","issues":[{"severity":"blocker","file":"a.go","line":1,"message":"bug"}]}`,
		Success:      true,
	}
	env := BuildEnvelope(result, BackendCodex, "m", "", "")
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
	env := BuildEnvelope(result, BackendCodex, "m", "", "")
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
	env := BuildEnvelope(result, BackendCodex, "m", "", "")
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
	env := BuildEnvelope(result, BackendCodex, "m", "", "")
	if env.Status != StatusOK {
		t.Errorf("status = %s, want ok for accepted with only low issues", env.Status)
	}
}

func TestBuildEnvelope_ConfidenceValid(t *testing.T) {
	// A valid confidence in (0.0, 1.0] passes validation and round-trips
	// through to the envelope.
	result := &ReviewResult{
		ResponseText: `{"verdict":"accepted","issues":[{"severity":"low","file":"a.go","line":1,"message":"nit","confidence":0.7}]}`,
		Success:      true,
	}
	env := BuildEnvelope(result, BackendCodex, "m", "", "")
	if env.Status != StatusOK {
		t.Errorf("status = %s, want ok for valid confidence", env.Status)
	}
	if len(env.Review.Issues) != 1 || env.Review.Issues[0].Confidence == nil {
		t.Fatalf("confidence not preserved: %+v", env.Review.Issues)
	}
	if got := *env.Review.Issues[0].Confidence; got != 0.7 {
		t.Errorf("confidence = %v, want 0.7", got)
	}
}

func TestBuildEnvelope_ConfidenceOmitted(t *testing.T) {
	// An issue without a confidence field is valid — the prompt makes the
	// field optional ("omit when you cannot assess").
	result := &ReviewResult{
		ResponseText: `{"verdict":"accepted","issues":[{"severity":"low","file":"a.go","line":1,"message":"nit"}]}`,
		Success:      true,
	}
	env := BuildEnvelope(result, BackendCodex, "m", "", "")
	if env.Status != StatusOK {
		t.Errorf("status = %s, want ok when confidence is omitted", env.Status)
	}
	if len(env.Review.Issues) != 1 {
		t.Fatalf("issue count = %d, want 1", len(env.Review.Issues))
	}
	if env.Review.Issues[0].Confidence != nil {
		t.Errorf("confidence = %v, want nil for omitted field", *env.Review.Issues[0].Confidence)
	}
}

func TestBuildEnvelope_ConfidenceOutOfRange(t *testing.T) {
	// Confidence must be in (0.0, 1.0]. The boundary 0.0 is excluded so a
	// reviewer cannot use it to mean "speculative" — that intent should be
	// expressed as a small positive value or by omitting the field.
	cases := []struct {
		name string
		json string
	}{
		{"zero", `{"verdict":"accepted","issues":[{"severity":"low","file":"a.go","line":1,"message":"nit","confidence":0.0}]}`},
		{"negative", `{"verdict":"accepted","issues":[{"severity":"low","file":"a.go","line":1,"message":"nit","confidence":-0.1}]}`},
		{"above_one", `{"verdict":"accepted","issues":[{"severity":"low","file":"a.go","line":1,"message":"nit","confidence":1.5}]}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := BuildEnvelope(&ReviewResult{ResponseText: tc.json, Success: true}, BackendCodex, "m", "", "")
			if env.Status != StatusError {
				t.Errorf("status = %s, want error for confidence=%s", env.Status, tc.name)
			}
		})
	}
}

func TestValidateReviewJSON(t *testing.T) {
	// The exported helper lets callers outside this package (e.g.
	// yoloswe/swe.go) reuse the envelope's schema check without going
	// through BuildEnvelope. Cover the boundary cases of the contract.
	cases := []struct {
		name      string
		json      string
		expectErr bool
	}{
		{"valid_accepted_with_confidence", `{"verdict":"accepted","issues":[{"severity":"low","file":"a.go","line":1,"message":"nit","confidence":0.7}]}`, false},
		{"valid_accepted_no_issues", `{"verdict":"accepted","issues":[]}`, false},
		{"unknown_verdict", `{"verdict":"maybe","issues":[]}`, true},
		{"confidence_out_of_range", `{"verdict":"accepted","issues":[{"severity":"low","file":"a.go","line":1,"message":"nit","confidence":2.0}]}`, true},
		{"missing_line", `{"verdict":"rejected","issues":[{"severity":"high","file":"a.go","message":"bug"}]}`, true},
		{"malformed_json", `not json at all`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateReviewJSON([]byte(tc.json), ReviewModeCode)
			if tc.expectErr && err == nil {
				t.Errorf("expected error, got nil")
			}
			if !tc.expectErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
		})
	}
}

func TestBuildEnvelope_UnparseableText(t *testing.T) {
	result := &ReviewResult{
		ResponseText: "the reviewer refused to produce JSON",
		Success:      true,
	}
	env := BuildEnvelope(result, BackendCursor, "composer-2", "", "")
	if env.Status != StatusError {
		t.Errorf("status = %s, want error on parse failure", env.Status)
	}
	if env.Review.RawText == "" {
		t.Error("raw_text should be preserved when JSON parsing fails")
	}
}

func TestBuildEnvelope_NilResult(t *testing.T) {
	env := BuildEnvelope(nil, BackendCodex, "m", "", "")
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
	env := BuildEnvelope(result, BackendGemini, "gemini-3.1-flash-lite-preview", "sess-gemini-1", "")
	if env.Status != StatusOK {
		t.Errorf("status = %s, want ok", env.Status)
	}
	if env.Backend != "gemini" {
		t.Errorf("backend = %q, want gemini", env.Backend)
	}
	if env.Model != "gemini-3.1-flash-lite-preview" {
		t.Errorf("model = %q, want gemini-3.1-flash-lite-preview", env.Model)
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
	env := BuildEnvelope(result, BackendGemini, "gemini-3.1-flash-lite-preview", "", "")
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

// TestValidateReviewJSONDesignDoc covers the design-doc validator. It
// asserts both the positive shape (section + dimension + ready/revise/rethink
// + top-level confidence) and the cross-mode rejection rules: code-mode
// fields (file/line) must NOT appear in design-doc envelopes, and code-mode
// verdict values must be rejected for design-doc mode.
func TestValidateReviewJSONDesignDoc(t *testing.T) {
	cases := []struct {
		name      string
		json      string
		errSub    string
		expectErr bool
	}{
		{
			name: "valid design-doc body",
			json: `{
				"verdict": "revise",
				"summary": "milestone risk",
				"confidence": 0.8,
				"issues": [{
					"severity": "high",
					"section": "Milestone 2",
					"dimension": "q4",
					"message": "doesn't frontload risk"
				}]
			}`,
			expectErr: false,
		},
		{
			name: "verdict ready with no issues is allowed",
			json: `{
				"verdict": "ready",
				"summary": "looks good",
				"confidence": 0.95,
				"issues": []
			}`,
			expectErr: false,
		},
		{
			name: "code-mode verdict rejected in design-doc mode",
			json: `{
				"verdict": "accepted",
				"summary": "x",
				"confidence": 0.5,
				"issues": []
			}`,
			expectErr: true,
			errSub:    `verdict "accepted"`,
		},
		{
			name: "missing top-level confidence rejected",
			json: `{
				"verdict": "ready",
				"summary": "x",
				"issues": []
			}`,
			expectErr: true,
			errSub:    "top-level confidence",
		},
		{
			name: "out-of-range confidence rejected",
			json: `{
				"verdict": "ready",
				"summary": "x",
				"confidence": 1.5,
				"issues": []
			}`,
			expectErr: true,
			errSub:    "[0.0, 1.0]",
		},
		{
			name: "issue with file/line rejected",
			json: `{
				"verdict": "revise",
				"summary": "x",
				"confidence": 0.7,
				"issues": [{
					"severity": "medium",
					"section": "Intro",
					"dimension": "q1",
					"file": "intro.md",
					"line": 4,
					"message": "x"
				}]
			}`,
			expectErr: true,
			errSub:    "must not carry file/line",
		},
		{
			name: "issue missing section rejected",
			json: `{
				"verdict": "revise",
				"summary": "x",
				"confidence": 0.7,
				"issues": [{
					"severity": "medium",
					"dimension": "q1",
					"message": "x"
				}]
			}`,
			expectErr: true,
			errSub:    "missing section",
		},
		{
			name: "issue missing dimension rejected",
			json: `{
				"verdict": "revise",
				"summary": "x",
				"confidence": 0.7,
				"issues": [{
					"severity": "medium",
					"section": "Intro",
					"message": "x"
				}]
			}`,
			expectErr: true,
			errSub:    "missing dimension",
		},
		{
			name: "ready verdict with high-severity issue rejected",
			json: `{
				"verdict": "ready",
				"summary": "x",
				"confidence": 0.5,
				"issues": [{
					"severity": "high",
					"section": "Intro",
					"dimension": "q1",
					"message": "x"
				}]
			}`,
			expectErr: true,
			errSub:    `verdict "ready" inconsistent`,
		},
		{
			name: "revise verdict with no issues rejected",
			json: `{
				"verdict": "revise",
				"summary": "x",
				"confidence": 0.6,
				"issues": []
			}`,
			expectErr: true,
			errSub:    "requires at least one issue",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateReviewJSON([]byte(tc.json), ReviewModeDesignDoc)
			if tc.expectErr && err == nil {
				t.Errorf("expected error containing %q, got nil", tc.errSub)
			}
			if !tc.expectErr && err != nil {
				t.Errorf("unexpected error: %v", err)
			}
			if err != nil && tc.errSub != "" && !strings.Contains(err.Error(), tc.errSub) {
				t.Errorf("error %q missing substring %q", err.Error(), tc.errSub)
			}
		})
	}
}

// TestValidateReviewJSONCodeRejectsDesignDocFields ensures code-mode
// validation rejects bodies that look like design-doc output. The
// existing TestValidateReviewJSON covers the happy path; this guards
// against a misrouted envelope (e.g. the orchestrator passing the wrong
// mode to ValidateReviewJSON) silently passing.
func TestValidateReviewJSONCodeRejectsDesignDocFields(t *testing.T) {
	body := `{
		"verdict": "ready",
		"summary": "x",
		"confidence": 0.5,
		"issues": []
	}`
	err := ValidateReviewJSON([]byte(body), ReviewModeCode)
	if err == nil {
		t.Fatal("expected error for design-doc verdict in code mode")
	}
	if !strings.Contains(err.Error(), `verdict "ready"`) {
		t.Errorf("error %q should mention rejected verdict", err.Error())
	}
}

// TestBuildEnvelopeReviewModePropagated asserts the new top-level
// review_mode field rides on every emitted envelope. Triage layers depend
// on this to dispatch consensus key construction without having to read
// the original CLI flags.
func TestBuildEnvelopeReviewModePropagated(t *testing.T) {
	cases := []struct {
		input ReviewMode
		want  ReviewMode
	}{
		{ReviewModeCode, ReviewModeCode},
		{ReviewModeDesignDoc, ReviewModeDesignDoc},
		{"", ReviewModeCode}, // empty defaults to code
	}
	for _, tc := range cases {
		t.Run(string(tc.input), func(t *testing.T) {
			env := BuildEnvelope(&ReviewResult{ErrorMessage: "x"}, BackendCodex, "m", "", tc.input)
			if env.ReviewMode != tc.want {
				t.Errorf("ReviewMode = %q, want %q", env.ReviewMode, tc.want)
			}
		})
	}
}

// TestValidateVerdictAliases pins the v2 alias-normalization contract: a
// reviewer emitting a PR-review-style verdict (approve_with_notes, etc.)
// gets canonicalized to accepted/rejected at the envelope boundary, the
// validator accepts it, and the emitted envelope shows the canonical
// token so downstream consumers don't see the alias.
func TestValidateVerdictAliases(t *testing.T) {
	cases := []struct {
		raw           string
		expectVerdict string
		needsIssue    bool // "rejected" requires at least one issue
	}{
		{"approve_with_notes", "accepted", false},
		{"approve-with-notes", "accepted", false},
		{"approve", "accepted", false},
		{"approved", "accepted", false},
		{"LGTM", "accepted", false}, // case-insensitive
		{"ship", "accepted", false},
		{"request_changes", "rejected", true},
		{"request-changes", "rejected", true},
		{"needs_changes", "rejected", true},
		{"needs-changes", "rejected", true},
		{"changes_requested", "rejected", true},
		{"reject", "rejected", true},
		{"block", "rejected", true},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			body := `{"verdict":"` + tc.raw + `","summary":"x","issues":[]}`
			if tc.needsIssue {
				body = `{"verdict":"` + tc.raw + `","summary":"x","issues":[{"severity":"medium","file":"a.go","line":1,"message":"m"}]}`
			}
			env := BuildEnvelope(&ReviewResult{ResponseText: body, Success: true}, BackendCodex, "m", "", "")
			if env.Status != StatusOK {
				t.Fatalf("status = %s (err=%q), want ok after alias normalization", env.Status, env.Error)
			}
			if env.Review.Verdict != tc.expectVerdict {
				t.Errorf("normalized verdict = %q, want %q", env.Review.Verdict, tc.expectVerdict)
			}
		})
	}
}

// TestValidateVerdict_UnknownStillFails guards against the alias map
// accidentally swallowing genuinely-unknown verdicts. "maybe" must still
// produce status=error rather than silently passing.
func TestValidateVerdict_UnknownStillFails(t *testing.T) {
	env := BuildEnvelope(&ReviewResult{ResponseText: `{"verdict":"maybe","issues":[]}`, Success: true}, BackendCodex, "m", "", "")
	if env.Status != StatusError {
		t.Errorf("status = %s, want error for unknown verdict", env.Status)
	}
}

// TestValidateInvariantSites_SingleSiteAccepted: a single-site finding
// with no invariant is the legacy shape and continues to validate.
func TestValidateInvariantSites_SingleSiteAccepted(t *testing.T) {
	body := `{"verdict":"rejected","issues":[{"severity":"high","file":"a.go","line":1,"message":"bug"}]}`
	env := BuildEnvelope(&ReviewResult{ResponseText: body, Success: true}, BackendCodex, "m", "", "")
	if env.Status != StatusOK {
		t.Errorf("status = %s (err=%q), want ok for single-site issue", env.Status, env.Error)
	}
}

// TestValidateInvariantSites_ClassLevelAccepted: an invariant with N
// sites, where file/line points at one of them, validates.
func TestValidateInvariantSites_ClassLevelAccepted(t *testing.T) {
	body := `{"verdict":"rejected","issues":[{
		"severity":"high","file":"a.go","line":10,
		"message":"sibling sites of one rule",
		"invariant":"ambient env vars shadow explicit proxy keys",
		"sites":[{"file":"a.go","line":10},{"file":"b.go","line":20}]
	}]}`
	env := BuildEnvelope(&ReviewResult{ResponseText: body, Success: true}, BackendCodex, "m", "", "")
	if env.Status != StatusOK {
		t.Fatalf("status = %s (err=%q), want ok for invariant+sites", env.Status, env.Error)
	}
	if len(env.Review.Issues) != 1 || env.Review.Issues[0].Invariant == "" || len(env.Review.Issues[0].Sites) != 2 {
		t.Errorf("invariant/sites not parsed: %+v", env.Review.Issues)
	}
}

// TestValidateInvariantSites_FileLineMustMatchOneSite enforces back-compat
// with single-site triage: when sites is non-empty, file/line at the top
// must match one entry so a consumer that ignores sites still sees a
// representative address.
func TestValidateInvariantSites_FileLineMustMatchOneSite(t *testing.T) {
	body := `{"verdict":"rejected","issues":[{
		"severity":"high","file":"z.go","line":99,
		"message":"mismatched representative site",
		"invariant":"rule x",
		"sites":[{"file":"a.go","line":10},{"file":"b.go","line":20}]
	}]}`
	env := BuildEnvelope(&ReviewResult{ResponseText: body, Success: true}, BackendCodex, "m", "", "")
	if env.Status != StatusError {
		t.Errorf("status = %s, want error when file/line doesn't match any site", env.Status)
	}
}

// TestValidateInvariantSites_InvariantWithoutSitesRejected: naming an
// invariant without listing sites is meaningless — there's nothing for
// the orchestrator to act on.
func TestValidateInvariantSites_InvariantWithoutSitesRejected(t *testing.T) {
	body := `{"verdict":"rejected","issues":[{
		"severity":"high","file":"a.go","line":1,
		"message":"named invariant with no sites",
		"invariant":"some rule"
	}]}`
	env := BuildEnvelope(&ReviewResult{ResponseText: body, Success: true}, BackendCodex, "m", "", "")
	if env.Status != StatusError {
		t.Errorf("status = %s, want error for invariant without sites", env.Status)
	}
}

// TestValidateInvariantSites_SiteMissingFile guards the per-site shape.
func TestValidateInvariantSites_SiteMissingFile(t *testing.T) {
	body := `{"verdict":"rejected","issues":[{
		"severity":"high","file":"a.go","line":10,
		"message":"sites entry missing file",
		"invariant":"r",
		"sites":[{"file":"a.go","line":10},{"line":20}]
	}]}`
	env := BuildEnvelope(&ReviewResult{ResponseText: body, Success: true}, BackendCodex, "m", "", "")
	if env.Status != StatusError {
		t.Errorf("status = %s, want error for site without file", env.Status)
	}
}

// TestValidateInvariantSites_DesignDocRejectsClassFields: invariant/sites
// are code-mode-only; design-doc envelopes that carry them are malformed.
func TestValidateInvariantSites_DesignDocRejectsClassFields(t *testing.T) {
	body := `{"verdict":"revise","confidence":0.8,"issues":[{
		"severity":"medium","section":"M1","dimension":"q1","message":"x",
		"invariant":"r","sites":[]
	}]}`
	env := BuildEnvelope(&ReviewResult{ResponseText: body, Success: true}, BackendCodex, "m", "", ReviewModeDesignDoc)
	if env.Status != StatusError {
		t.Errorf("status = %s, want error for invariant in design-doc mode", env.Status)
	}
}

// TestSufficiency_AcceptedAndPropagated: a top-level sufficiency object
// parses into the envelope. Surfaces in v2 envelopes only; the field is
// optional, so omitting it remains valid.
func TestSufficiency_AcceptedAndPropagated(t *testing.T) {
	body := `{"verdict":"accepted","summary":"clean","issues":[],
		"sufficiency":{"is_confident_complete":true,"evidence":"all named invariants addressed"}}`
	env := BuildEnvelope(&ReviewResult{ResponseText: body, Success: true}, BackendCodex, "m", "", "")
	if env.Status != StatusOK {
		t.Fatalf("status = %s (err=%q), want ok", env.Status, env.Error)
	}
	if env.Review.Sufficiency == nil {
		t.Fatal("Sufficiency should be parsed")
	}
	if !env.Review.Sufficiency.IsConfidentComplete {
		t.Errorf("IsConfidentComplete = false, want true")
	}
	if env.Review.Sufficiency.Evidence == "" {
		t.Errorf("Evidence should be parsed")
	}
}

// TestSufficiency_OmittedRemainsNil: absence means no signal — the field
// stays nil. Don't synthesize a default.
func TestSufficiency_OmittedRemainsNil(t *testing.T) {
	body := `{"verdict":"accepted","summary":"clean","issues":[]}`
	env := BuildEnvelope(&ReviewResult{ResponseText: body, Success: true}, BackendCodex, "m", "", "")
	if env.Status != StatusOK {
		t.Fatalf("status = %s, want ok", env.Status)
	}
	if env.Review.Sufficiency != nil {
		t.Errorf("Sufficiency = %+v, want nil for omitted field", env.Review.Sufficiency)
	}
}

// TestSchemaVersion_IsV2 pins the bump. Anyone bumping JSONSchemaVersion
// should also update the docstring on the constant and notify downstream
// readers; this test catches accidental reverts.
func TestSchemaVersion_IsV2(t *testing.T) {
	if JSONSchemaVersion != 2 {
		t.Errorf("JSONSchemaVersion = %d, want 2 (Issue.Invariant/Sites + ReviewBody.Sufficiency)", JSONSchemaVersion)
	}
}
