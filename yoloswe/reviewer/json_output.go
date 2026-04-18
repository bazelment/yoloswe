package reviewer

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// JSONSchemaVersion is the envelope schema version. Bump on breaking changes.
const JSONSchemaVersion = 1

// ReviewIssue mirrors the per-issue shape requested by BuildJSONPrompt.
type ReviewIssue struct {
	Severity   string `json:"severity"`
	File       string `json:"file"`
	Message    string `json:"message"`
	Suggestion string `json:"suggestion,omitempty"`
	Line       int    `json:"line,omitempty"`
}

// ReviewBody is the parsed reviewer-level JSON. When the reviewer's response
// could not be parsed, Verdict is empty and RawText holds the original text.
type ReviewBody struct {
	Verdict string        `json:"verdict,omitempty"`
	Summary string        `json:"summary,omitempty"`
	RawText string        `json:"raw_text,omitempty"`
	Issues  []ReviewIssue `json:"issues,omitempty"`
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

// ResultEnvelope is the stable stdout format emitted by --json mode. The
// schema_version field lets consumers reject incompatible payloads cleanly.
type ResultEnvelope struct {
	Status        EnvelopeStatus `json:"status"`
	Backend       string         `json:"backend"`
	Model         string         `json:"model"`
	SessionID     string         `json:"session_id,omitempty"`
	Error         string         `json:"error,omitempty"`
	Review        ReviewBody     `json:"review"`
	SchemaVersion int            `json:"schema_version"`
	DurationMs    int64          `json:"duration_ms"`
	InputTokens   int64          `json:"input_tokens"`
	OutputTokens  int64          `json:"output_tokens"`
}

// BuildEnvelope assembles a stable envelope from a review result. It extracts
// the JSON body from result.ResponseText when possible, falling back to
// RawText on parse failure.
func BuildEnvelope(result *ReviewResult, backend BackendType, model, sessionID string) ResultEnvelope {
	env := ResultEnvelope{
		SchemaVersion: JSONSchemaVersion,
		Backend:       string(backend),
		Model:         model,
		SessionID:     sessionID,
	}
	if result == nil {
		env.Status = StatusError
		env.Error = "nil review result"
		return env
	}
	env.DurationMs = result.DurationMs
	env.InputTokens = result.InputTokens
	env.OutputTokens = result.OutputTokens
	if result.ErrorMessage != "" {
		env.Error = result.ErrorMessage
	}

	body, parseErr := extractReviewBody(result.ResponseText)
	env.Review = body

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
	default:
		env.Status = StatusOK
	}
	return env
}

// PrintJSONResult serializes the envelope to w as a single-line JSON object
// followed by a trailing newline. Intended for stdout.
func PrintJSONResult(w io.Writer, env ResultEnvelope) error {
	encoded, err := json.Marshal(env)
	if err != nil {
		return fmt.Errorf("marshal envelope: %w", err)
	}
	if _, err := w.Write(encoded); err != nil {
		return err
	}
	_, err = w.Write([]byte("\n"))
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
