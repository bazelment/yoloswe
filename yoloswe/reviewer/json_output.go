package reviewer

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
)

// JSONSchemaVersion is the envelope schema version. Bump on breaking changes.
//
// v2 adds optional code-mode fields for class-level findings (Issue.Invariant,
// Issue.Sites) and reviewer self-assessment (ReviewBody.Sufficiency). Older
// readers that don't know v2 ignore the new fields and continue to parse
// v2 envelopes — the additions are strictly additive on the consumer side.
const JSONSchemaVersion = 2

// ReviewIssue mirrors the per-issue shape requested by BuildJSONPrompt.
//
// Confidence is *float64 so JSON omission and an explicit value are
// distinguishable. The prompt contract is "omit when unassessed; otherwise
// emit a value in (0.0, 1.0]" — a plain float64 with omitempty would treat
// the boundary value 0 as missing and leave the validator unable to reject
// out-of-range values. Consumers should treat nil as "no signal" rather
// than synthesizing a default.
//
// The struct carries fields for both review modes; per-mode validation
// (validateReviewBody, dispatched by ReviewMode) decides which fields are
// required vs forbidden. omitempty on every mode-specific field keeps the
// wire format clean: a code-mode issue serializes without section/dimension
// and a design-doc-mode issue serializes without file/line.
// Field order is dictated by govet/fieldalignment: pointer + slice
// (large-alignment) fields first, then strings, then int. The JSON wire
// shape is unaffected — json.Marshal honors the struct fields, not the
// source order.
type ReviewIssue struct {
	Confidence *float64    `json:"confidence,omitempty"`
	Severity   string      `json:"severity"`
	Message    string      `json:"message"`
	Suggestion string      `json:"suggestion,omitempty"`
	File       string      `json:"file,omitempty"`
	Section    string      `json:"section,omitempty"`
	Dimension  string      `json:"dimension,omitempty"`
	Invariant  string      `json:"invariant,omitempty"`
	Sites      []IssueSite `json:"sites,omitempty"`
	Line       int         `json:"line,omitempty"`
}

// IssueSite is one location of a class-level finding's Sites array. Note
// is optional per-site context the reviewer may add when the same rule
// manifests differently at different sites (e.g. one site reads stale env,
// another writes through it). Field order: strings before int per
// fieldalignment.
type IssueSite struct {
	File string `json:"file"`
	Note string `json:"note,omitempty"`
	Line int    `json:"line"`
}

// UnmarshalJSON accepts the canonical object form and a legacy/model-emitted
// shorthand "path/to/file.go:123" or "path/to/file.go:123-130".
func (s *IssueSite) UnmarshalJSON(data []byte) error {
	type issueSite IssueSite
	var obj issueSite
	if err := json.Unmarshal(data, &obj); err == nil {
		*s = IssueSite(obj)
		return nil
	}

	var shorthand string
	if err := json.Unmarshal(data, &shorthand); err != nil {
		return err
	}
	file, line, ok := strings.Cut(shorthand, ":")
	if !ok || strings.TrimSpace(file) == "" {
		return fmt.Errorf("invalid issue site shorthand %q", shorthand)
	}
	lineText := strings.TrimSpace(line)
	if before, _, ok := strings.Cut(lineText, "-"); ok {
		lineText = before
	}
	lineNo, err := strconv.Atoi(lineText)
	if err != nil || lineNo < 1 {
		return fmt.Errorf("invalid issue site line in shorthand %q", shorthand)
	}
	*s = IssueSite{File: strings.TrimSpace(file), Line: lineNo}
	return nil
}

// ReviewBody is the parsed reviewer-level JSON. When the reviewer's response
// could not be parsed, Verdict is empty and RawText holds the original text.
//
// Confidence is the design-doc verdict-level confidence (0.0..1.0). It is
// *float64 so the field can be distinguished between "not provided" and
// "explicitly zero", same as ReviewIssue.Confidence. Code-mode envelopes
// don't emit it; design-doc-mode envelopes require it.
// Field order: pointers + slices first, then strings, per fieldalignment.
type ReviewBody struct {
	Confidence  *float64      `json:"confidence,omitempty"`
	Sufficiency *Sufficiency  `json:"sufficiency,omitempty"`
	Verdict     string        `json:"verdict,omitempty"`
	Summary     string        `json:"summary,omitempty"`
	RawText     string        `json:"raw_text,omitempty"`
	Issues      []ReviewIssue `json:"issues,omitempty"`
}

// Sufficiency lets the reviewer claim "I think I've found all sites of
// every invariant I named this turn." Evidence is a 1-2 sentence
// rationale; the orchestrator displays it but does not parse it. Field
// order: string before bool per fieldalignment.
type Sufficiency struct {
	Evidence            string `json:"evidence,omitempty"`
	IsConfidentComplete bool   `json:"is_confident_complete"`
}

// validVerdicts enumerates per-mode verdict strings the reviewer prompt
// requires. Anything outside the matching set for a given mode is treated as
// a schema violation in validateReviewBody.
//
// Verdicts are deliberately single words — multi-word verdicts force a
// hyphen-vs-underscore choice that LLMs reliably get wrong (snake_case
// vs kebab-case is a coin flip in their training corpus). Pick imperative
// single-word verbs for design-doc mode that say what the author should
// do with the doc, not what state it's in.
var validVerdicts = map[ReviewMode]map[string]struct{}{
	ReviewModeCode: {
		"accepted": {},
		"rejected": {},
	},
	ReviewModeDesignDoc: {
		"ready":   {}, // ship as-is; no high/critical issues
		"revise":  {}, // address issues, doc shape is right
		"rethink": {}, // premise needs reconsideration
	},
}

// verdictAliases maps common reviewer-emitted variants to the canonical
// verdict token for the corresponding mode. The reviewer prompt asks for
// the canonical token explicitly, but cursor (in particular) occasionally
// emits PR-review-style verdicts like "approve_with_notes" or
// "request_changes". Rather than failing validation and forcing the skill's
// recover-envelope shim to re-map these, normalize at the validator
// boundary so the wrapper itself converges on the canonical set.
//
// Keys are lowercased; lookup is case-insensitive (the validator lowercases
// the input). Hyphen-vs-underscore variants are both keyed because models
// pick one or the other at random.
var verdictAliases = map[ReviewMode]map[string]string{
	ReviewModeCode: {
		"approve":            "accepted",
		"approve_with_notes": "accepted",
		"approve-with-notes": "accepted",
		"approved":           "accepted",
		"lgtm":               "accepted",
		"ship":               "accepted",
		"request_changes":    "rejected",
		"request-changes":    "rejected",
		"needs_changes":      "rejected",
		"needs-changes":      "rejected",
		"changes_requested":  "rejected",
		"changes-requested":  "rejected",
		"reject":             "rejected",
		"block":              "rejected",
	},
	ReviewModeDesignDoc: {
		// Design-doc verdicts (ready/revise/rethink) rarely confuse the
		// model — the rubric framing keeps them top of mind. Leaving this
		// empty makes adding aliases trivial later without changing the
		// validator shape.
	},
}

// normalizeVerdict canonicalizes verdict aliases at the validator
// boundary. Returns the original input unchanged when it's already
// canonical or has no alias mapping; the validator's existing
// validVerdicts check still catches genuinely unknown verdicts.
func normalizeVerdict(verdict string, mode ReviewMode) string {
	if verdict == "" {
		return verdict
	}
	aliases, ok := verdictAliases[mode]
	if !ok {
		return verdict
	}
	if canonical, hit := aliases[strings.ToLower(verdict)]; hit {
		return canonical
	}
	return verdict
}

// validSeverities enumerates the severity labels the reviewer prompt requires.
// Anything else is treated as a schema violation; keeping this aligned with the
// rank table in cmd/codereview/codereview.go's maxSeverity prevents drift
// between the envelope contract and downstream consumers.
var validSeverities = map[string]struct{}{
	"low":      {},
	"medium":   {},
	"high":     {},
	"critical": {},
}

// blockingSeverities are the severities that contradict an "accepted"
// verdict in code mode (or "ready" verdict in design-doc mode). Anything at
// high or critical means the reviewer surfaced something that blocks merge
// (or shipping), so the verdict must move out of the no-blockers slot for
// the envelope to be ok.
var blockingSeverities = map[string]struct{}{
	"high":     {},
	"critical": {},
}

// noBlockerVerdicts names the "no blockers" verdict for each mode — the
// verdict the reviewer must not pick when at least one high/critical issue
// is present. Code mode: "accepted". Design-doc mode: "ready".
var noBlockerVerdicts = map[ReviewMode]string{
	ReviewModeCode:      "accepted",
	ReviewModeDesignDoc: "ready",
}

// EnvelopeStatus values distinguish bramble-level outcomes from reviewer
// verdicts. A successful review with verdict="rejected" is still Status="ok".
type EnvelopeStatus string

const (
	// StatusOK means bramble ran the review to completion and parsed the
	// reviewer response. Consumers should trust review.verdict.
	StatusOK EnvelopeStatus = "ok"
	// StatusError means bramble or the reviewer backend failed. review may
	// still carry raw_text if any response was produced.
	StatusError EnvelopeStatus = "error"
)

// ResultEnvelope is the structured result written on every exit path. It goes
// to --envelope-file when set, otherwise to stdout. The schema_version field
// lets consumers reject incompatible payloads cleanly.
//
// ReviewMode is emitted at the top level so downstream consumers (e.g. the
// /pr-polish or /design-doc-polish triage layer) can pick the right
// consensus key without re-reading the original CLI flags. Empty
// ("review_mode" omitted) is treated as ReviewModeCode by consumers — the
// pre-mode envelopes shipped without this field, and we want their
// behaviour preserved.
// Field order: large nested struct (Review) first so its inner pointer
// fields align cleanly, then strings, then ints. Wire format unchanged.
type ResultEnvelope struct {
	Status        EnvelopeStatus `json:"status"`
	Backend       string         `json:"backend"`
	Model         string         `json:"model"`
	SessionID     string         `json:"session_id,omitempty"`
	ResumeStatus  ResumeStatus   `json:"resume_status,omitempty"`
	ReviewMode    ReviewMode     `json:"review_mode,omitempty"`
	Error         string         `json:"error,omitempty"`
	Review        ReviewBody     `json:"review"`
	SchemaVersion int            `json:"schema_version"`
	DurationMs    int64          `json:"duration_ms"`
	InputTokens   int64          `json:"input_tokens"`
	OutputTokens  int64          `json:"output_tokens"`
}

// BuildEnvelope assembles a stable envelope from a review result. It extracts
// the JSON body from result.ResponseText when possible, falling back to
// RawText on parse failure. mode dispatches schema validation; the empty
// string is treated as ReviewModeCode for backward-compat with callers that
// pre-date the mode field.
func BuildEnvelope(result *ReviewResult, backend BackendType, model, sessionID string, mode ReviewMode) ResultEnvelope {
	if mode == "" {
		mode = ReviewModeCode
	}
	env := ResultEnvelope{
		SchemaVersion: JSONSchemaVersion,
		Backend:       string(backend),
		Model:         model,
		SessionID:     sessionID,
		ReviewMode:    mode,
	}
	if result == nil {
		env.Status = StatusError
		env.Error = "nil review result"
		return env
	}
	env.DurationMs = result.DurationMs
	env.InputTokens = result.InputTokens
	env.OutputTokens = result.OutputTokens
	env.ResumeStatus = result.ResumeStatus
	if result.ErrorMessage != "" {
		env.Error = result.ErrorMessage
	}

	body, parseErr := extractReviewBody(result.ResponseText)
	env.Review = body

	// validateReviewBody normalizes the verdict in place so downstream
	// envelope readers see the canonical token — the alias map turns
	// "approve_with_notes" into "accepted" before the validVerdicts gate
	// fires, which means the skill's recover-envelope shim becomes a
	// no-op on v2 envelopes. Pass &env.Review (not the local body)
	// because the normalization must land on the envelope we emit.
	schemaErr := validateReviewBody(&env.Review, mode)

	switch {
	case result.ErrorMessage != "":
		env.Status = StatusError
	case !result.Success:
		env.Status = StatusError
		if env.Error == "" {
			env.Error = "reviewer reported non-success"
		}
	case parseErr != nil:
		env.Status = StatusError
		if env.Error == "" {
			env.Error = fmt.Sprintf("parse reviewer JSON: %v", parseErr)
		}
	case schemaErr != nil:
		env.Status = StatusError
		if env.Error == "" {
			env.Error = fmt.Sprintf("invalid reviewer JSON: %v", schemaErr)
		}
	default:
		env.Status = StatusOK
	}
	return env
}

// ValidateReviewJSON parses an extracted reviewer JSON object and runs the
// same schema validation that BuildEnvelope applies to ResultEnvelope.Review.
// Returns nil on a well-formed, schema-compliant body.
//
// mode dispatches the per-mode schema. Empty defaults to ReviewModeCode so
// pre-mode callers (e.g. yoloswe/swe.go) keep working unchanged.
//
// Input contract: raw must be the bare JSON object the reviewer emitted,
// without any narration prefix or fenced code block — same shape that
// extractReviewBody returns. Feeding it raw model response text (which may
// be wrapped in ``` fences or follow narration) will fail to unmarshal.
// Callers wanting a one-shot "extract + validate" should go through
// BuildEnvelope, which composes both steps.
func ValidateReviewJSON(raw []byte, mode ReviewMode) error {
	var body ReviewBody
	if err := json.Unmarshal(raw, &body); err != nil {
		return fmt.Errorf("unmarshal reviewer JSON: %w", err)
	}
	return validateReviewBody(&body, mode)
}

// validateReviewBody checks the parsed body against the required reviewer
// schema for the given mode. A non-nil error means the envelope must report
// status="error" so downstream automation never acts on malformed reviewer
// output.
//
// Code mode (ReviewModeCode, the default) requires:
//   - verdict ∈ {accepted, rejected}
//   - each issue has severity ∈ {low, medium, high, critical}, message, file, line ≥ 1
//   - "rejected" carries at least one issue (otherwise nothing was rejected)
//   - "accepted" carries no high/critical issues (those block merge by definition)
//   - confidence, when present, is in (0.0, 1.0] and finite (no NaN/Inf)
//
// Design-doc mode (ReviewModeDesignDoc) requires:
//   - verdict ∈ {ready, revise, rethink} (single-word imperative verbs;
//     see validVerdicts for the rationale on avoiding hyphens)
//   - each issue has severity, message, section (heading or "(whole document)"),
//     dimension (rubric question id, e.g. "q1"); file/line MUST NOT be present
//   - "revise" or "rethink" carries at least one issue
//   - "ready" carries no high/critical issues
//   - top-level confidence ∈ [0.0, 1.0]; per-issue confidence ∈ (0.0, 1.0] when present
//
// validateInvariantSites enforces the code-mode contract for class-level
// findings:
//
//   - Issue.Invariant non-empty implies len(Sites) ≥ 1 (an invariant must
//     point at evidence).
//   - When Sites has ≥2 entries, Issue.File/Line must match one of them —
//     the representative site stays in sync with the array so single-site
//     triage and consensus matching see a coherent address.
//   - Each Site has a non-empty File and Line ≥ 1.
//
// A single-site issue with no invariant has Sites empty; that's the legacy
// shape and stays valid. A single-site issue MAY carry Invariant with one
// Site (e.g. the reviewer found one sibling but expects more on the next
// turn) — we accept that without forcing it.
//
// Takes *ReviewIssue (not a copy) because ReviewIssue is ~152 bytes once
// Sites + Invariant landed; the validator runs per-issue in a tight loop.
func validateInvariantSites(idx int, issue *ReviewIssue) error {
	if issue.Invariant != "" && len(issue.Sites) == 0 {
		return fmt.Errorf("issue[%d] invariant requires at least one site", idx)
	}
	if len(issue.Sites) == 0 {
		return nil
	}
	matched := false
	for j := range issue.Sites {
		site := &issue.Sites[j]
		if site.File == "" {
			return fmt.Errorf("issue[%d].sites[%d] missing file", idx, j)
		}
		if site.Line < 1 {
			return fmt.Errorf("issue[%d].sites[%d] missing line", idx, j)
		}
		if site.File == issue.File && site.Line == issue.Line {
			matched = true
		}
	}
	if !matched {
		return fmt.Errorf("issue[%d] file/line must match one of sites[]", idx)
	}
	return nil
}

// Note: the design-doc prompt (designDocJSONOutputRules) describes
// finer verdict-vs-severity calibration bands (e.g. "revise is for
// low/medium issues only; rethink for high/critical clusters"). Those
// are guidance to the model, NOT structural schema rules — the
// validator only enforces the "ready ⇔ no high/critical" symmetry, so
// the model can pick "rethink" with only medium issues if it thinks
// the doc's premise needs reconsideration. Don't tighten this without
// also updating the prompt; otherwise valid model output will trip the
// validator.
//
// The body is mutated in place: verdict aliases (e.g. "approve_with_notes")
// are normalized to the canonical token before the validVerdicts gate,
// and the canonical verdict is the one downstream consumers see on the
// emitted envelope.
func validateReviewBody(body *ReviewBody, mode ReviewMode) error {
	if mode == "" {
		mode = ReviewModeCode
	}
	verdicts, ok := validVerdicts[mode]
	if !ok {
		return fmt.Errorf("unknown review mode %q", mode)
	}
	body.Verdict = normalizeVerdict(body.Verdict, mode)
	if _, ok := verdicts[body.Verdict]; !ok {
		return fmt.Errorf("verdict %q not valid for mode %q", body.Verdict, mode)
	}
	if mode == ReviewModeDesignDoc {
		if body.Confidence == nil {
			return fmt.Errorf("design-doc verdict requires top-level confidence")
		}
		c := *body.Confidence
		if math.IsNaN(c) || math.IsInf(c, 0) || c < 0 || c > 1 {
			return fmt.Errorf("top-level confidence %v not in [0.0, 1.0]", c)
		}
	}
	hasBlocking := false
	// Index loop avoids the per-iteration value-copy lint (each
	// ReviewIssue is ~152 bytes once Sites/Invariant landed); see
	// gocritic.rangeValCopy.
	for i := range body.Issues {
		issue := &body.Issues[i]
		if issue.Severity == "" {
			return fmt.Errorf("issue[%d] missing severity", i)
		}
		if _, ok := validSeverities[issue.Severity]; !ok {
			return fmt.Errorf("issue[%d] severity %q not in {low,medium,high,critical}", i, issue.Severity)
		}
		if issue.Message == "" {
			return fmt.Errorf("issue[%d] missing message", i)
		}
		if mode == ReviewModeCode {
			if len(issue.Sites) > 0 {
				if issue.File == "" {
					issue.File = issue.Sites[0].File
				}
				if issue.Line < 1 {
					issue.Line = issue.Sites[0].Line
				}
			}
			if issue.File == "" {
				return fmt.Errorf("issue[%d] missing file", i)
			}
			if issue.Line < 1 {
				return fmt.Errorf("issue[%d] missing line", i)
			}
			if err := validateInvariantSites(i, issue); err != nil {
				return err
			}
		} else { // ReviewModeDesignDoc
			if issue.Section == "" {
				return fmt.Errorf("issue[%d] missing section", i)
			}
			if issue.Dimension == "" {
				return fmt.Errorf("issue[%d] missing dimension", i)
			}
			if issue.File != "" || issue.Line != 0 {
				return fmt.Errorf("issue[%d] design-doc mode must not carry file/line", i)
			}
			if issue.Invariant != "" || len(issue.Sites) > 0 {
				return fmt.Errorf("issue[%d] design-doc mode must not carry invariant/sites", i)
			}
		}
		if issue.Confidence != nil {
			c := *issue.Confidence
			if math.IsNaN(c) || math.IsInf(c, 0) {
				return fmt.Errorf("issue[%d] confidence not finite: %v", i, c)
			}
			if c <= 0 || c > 1 {
				return fmt.Errorf("issue[%d] confidence %v not in (0.0, 1.0]", i, c)
			}
		}
		if _, blocks := blockingSeverities[issue.Severity]; blocks {
			hasBlocking = true
		}
	}
	noBlocker := noBlockerVerdicts[mode]
	if body.Verdict != noBlocker && len(body.Issues) == 0 {
		return fmt.Errorf("verdict %q requires at least one issue", body.Verdict)
	}
	if body.Verdict == noBlocker && hasBlocking {
		return fmt.Errorf("verdict %q inconsistent with high/critical issues", body.Verdict)
	}
	return nil
}

// PrintJSONResult serializes the envelope to w as a single-line JSON object
// followed by a trailing newline. Intended for stdout.
func PrintJSONResult(w io.Writer, env ResultEnvelope) error {
	encoded, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	_, err = w.Write(append(encoded, '\n'))
	return err
}

// extractReviewBody pulls the reviewer-level JSON out of free-form text. The
// reviewer is instructed to emit JSON only, but models routinely wrap it in a
// fenced ```json block or prepend narration. Strategy: strip common fences,
// then find the last balanced top-level {...} block. On any failure, return a
// ReviewBody with RawText populated and a descriptive error.
func extractReviewBody(text string) (ReviewBody, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ReviewBody{RawText: text}, fmt.Errorf("empty response")
	}

	candidate := stripJSONFence(trimmed)
	jsonBlob, ok := lastBalancedObject(candidate)
	if !ok {
		return ReviewBody{RawText: text}, fmt.Errorf("no JSON object found in response")
	}

	var body ReviewBody
	if err := json.Unmarshal([]byte(jsonBlob), &body); err != nil {
		return ReviewBody{RawText: text}, fmt.Errorf("unmarshal: %w", err)
	}
	return body, nil
}

// stripJSONFence removes a surrounding ```json ... ``` or ``` ... ``` fence,
// if present. Returns the input unchanged when no full fence wraps it.
func stripJSONFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	rest := strings.TrimPrefix(s, "```")
	rest = strings.TrimPrefix(rest, "json")
	rest = strings.TrimPrefix(rest, "JSON")
	rest = strings.TrimLeft(rest, " \t\r\n")
	end := strings.LastIndex(rest, "```")
	if end < 0 {
		return s
	}
	return strings.TrimSpace(rest[:end])
}

// lastBalancedObject returns the last top-level balanced {...} substring in s.
// Naive brace counting is sufficient here: reviewer JSON rarely contains raw
// unescaped braces in strings, and strings.Unmarshal will reject a bad slice.
// It tracks string state so braces inside quoted strings are ignored.
func lastBalancedObject(s string) (string, bool) {
	var (
		start     = -1
		depth     = 0
		inString  = false
		escaped   = false
		lastStart = -1
		lastEnd   = -1
	)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' && inString {
			escaped = true
			continue
		}
		if c == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		switch c {
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth > 0 {
				depth--
				if depth == 0 && start >= 0 {
					lastStart = start
					lastEnd = i + 1
					start = -1
				}
			}
		}
	}
	if lastStart < 0 || lastEnd <= lastStart {
		return "", false
	}
	return s[lastStart:lastEnd], true
}
