// Package prompt renders task prompts for coding-agent sessions.
// It implements the template rendering rules from Spec Section 12.
package prompt

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/symphony/model"
)

const fallbackPrompt = "You are working on an issue from Linear."

// knownVariables is the set of template variables that are allowed.
// Any variable not in this set causes a strict-mode render error.
var knownVariables = map[string]bool{
	"issue.id":          true,
	"issue.identifier":  true,
	"issue.title":       true,
	"issue.description": true,
	"issue.priority":    true,
	"issue.state":       true,
	"issue.branch_name": true,
	"issue.url":         true,
	"issue.labels":      true,
	"issue.blocked_by":  true,
	"issue.created_at":  true,
	"issue.updated_at":  true,
	"attempt":           true,
}

// RenderInitialPrompt renders the workflow prompt template with the given issue
// and attempt metadata. It uses strict variable checking per Spec Section 12.2:
// unknown variables or filters cause an immediate error.
//
// If template is empty, a fallback prompt is returned.
func RenderInitialPrompt(template string, issue model.Issue, attempt *int) (string, error) {
	if template == "" {
		return fallbackPrompt, nil
	}

	vars := buildVarMap(issue, attempt)
	return render(template, vars)
}

// RenderContinuationPrompt produces a continuation message for an existing
// coding-agent thread. Per Spec Section 7.1, continuation turns do not resend
// the original task prompt.
func RenderContinuationPrompt(issue model.Issue, turnNumber int) string {
	return BuildContinuationGuidance(issue, turnNumber, 0)
}

// buildVarMap converts the issue and attempt into the flat string map used by
// the template renderer.
func buildVarMap(issue model.Issue, attempt *int) map[string]string {
	m := make(map[string]string)

	m["issue.id"] = issue.ID
	m["issue.identifier"] = issue.Identifier
	m["issue.title"] = issue.Title
	m["issue.description"] = ptrStringVal(issue.Description)
	m["issue.priority"] = ptrIntVal(issue.Priority)
	m["issue.state"] = issue.State
	m["issue.branch_name"] = ptrStringVal(issue.BranchName)
	m["issue.url"] = ptrStringVal(issue.URL)
	m["issue.labels"] = strings.Join(issue.Labels, ", ")
	m["issue.blocked_by"] = formatBlockers(issue.BlockedBy)
	m["issue.created_at"] = ptrTimeVal(issue.CreatedAt)
	m["issue.updated_at"] = ptrTimeVal(issue.UpdatedAt)

	if attempt != nil {
		m["attempt"] = strconv.Itoa(*attempt)
	}
	// When attempt is nil, the key is absent from the map, which makes
	// {% if attempt %} blocks evaluate to false and {{ attempt }} render
	// as empty string.

	return m
}

func ptrStringVal(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func ptrIntVal(p *int) string {
	if p == nil {
		return ""
	}
	return strconv.Itoa(*p)
}

func ptrTimeVal(p *time.Time) string {
	if p == nil {
		return ""
	}
	return p.UTC().Format(time.RFC3339)
}

func formatBlockers(refs []model.BlockerRef) string {
	if len(refs) == 0 {
		return ""
	}
	parts := make([]string, 0, len(refs))
	for _, r := range refs {
		switch {
		case r.Identifier != nil:
			parts = append(parts, *r.Identifier)
		case r.ID != nil:
			parts = append(parts, *r.ID)
		}
	}
	return strings.Join(parts, ", ")
}

// render is a minimal strict template renderer. It processes two constructs:
//
//   - {{ variable }} — interpolate a known variable
//   - {% if variable %}...{% endif %} — conditional block
//
// Unknown variables and any filter syntax ({{ x | filter }}) cause an error.
// This is intentionally NOT regex-based; it uses a simple scanner.
func render(tmpl string, vars map[string]string) (string, error) {
	var out strings.Builder
	i := 0
	for i < len(tmpl) {
		// Look for the next tag opener.
		nextVar := strings.Index(tmpl[i:], "{{")
		nextBlock := strings.Index(tmpl[i:], "{%")

		// No more tags — copy the rest.
		if nextVar == -1 && nextBlock == -1 {
			out.WriteString(tmpl[i:])
			break
		}

		// Determine which tag comes first.
		var tagPos int
		var isBlock bool
		switch {
		case nextVar == -1:
			tagPos = nextBlock
			isBlock = true
		case nextBlock == -1:
			tagPos = nextVar
			isBlock = false
		case nextBlock < nextVar:
			tagPos = nextBlock
			isBlock = true
		default:
			tagPos = nextVar
			isBlock = false
		}

		// Write literal text before the tag.
		out.WriteString(tmpl[i : i+tagPos])
		pos := i + tagPos

		if isBlock {
			newPos, err := processBlock(tmpl, pos, vars, &out)
			if err != nil {
				return "", err
			}
			i = newPos
		} else {
			newPos, err := processVar(tmpl, pos, vars, &out)
			if err != nil {
				return "", err
			}
			i = newPos
		}
	}
	return out.String(), nil
}

// processVar handles a {{ variable }} tag starting at pos.
func processVar(tmpl string, pos int, vars map[string]string, out *strings.Builder) (int, error) {
	// Find the closing }}.
	end := strings.Index(tmpl[pos:], "}}")
	if end == -1 {
		return 0, fmt.Errorf("template_render_error: unclosed variable tag at position %d", pos)
	}
	// The inner content is between {{ and }}.
	inner := tmpl[pos+2 : pos+end]
	inner = strings.TrimSpace(inner)

	// Check for filters (pipe character).
	if strings.Contains(inner, "|") {
		return 0, fmt.Errorf("template_render_error: unknown filter in expression %q", inner)
	}

	if !knownVariables[inner] {
		return 0, fmt.Errorf("template_render_error: unknown variable %q", inner)
	}

	if val, ok := vars[inner]; ok {
		out.WriteString(val)
	}
	// If key is absent (e.g. nil attempt), output nothing.
	return pos + end + 2, nil
}

// processBlock handles {% if variable %}...{% endif %} blocks starting at pos.
func processBlock(tmpl string, pos int, vars map[string]string, out *strings.Builder) (int, error) {
	// Find closing %}.
	end := strings.Index(tmpl[pos:], "%}")
	if end == -1 {
		return 0, fmt.Errorf("template_render_error: unclosed block tag at position %d", pos)
	}
	inner := tmpl[pos+2 : pos+end]
	inner = strings.TrimSpace(inner)
	tagEnd := pos + end + 2

	if strings.HasPrefix(inner, "if ") {
		varName := strings.TrimSpace(strings.TrimPrefix(inner, "if"))

		if !knownVariables[varName] {
			return 0, fmt.Errorf("template_render_error: unknown variable %q in conditional", varName)
		}

		// Find matching {% endif %}.
		endifTag := "{% endif %}"
		endifPos := strings.Index(tmpl[tagEnd:], endifTag)
		if endifPos == -1 {
			return 0, fmt.Errorf("template_render_error: missing {%% endif %%} for conditional at position %d", pos)
		}
		body := tmpl[tagEnd : tagEnd+endifPos]
		afterEndif := tagEnd + endifPos + len(endifTag)

		// Evaluate: variable is truthy if present in map and non-empty.
		val, ok := vars[varName]
		if ok && val != "" {
			rendered, err := render(body, vars)
			if err != nil {
				return 0, err
			}
			out.WriteString(rendered)
		}
		return afterEndif, nil
	}

	return 0, fmt.Errorf("template_render_error: unknown block tag %q", inner)
}
