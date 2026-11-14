package concierge

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/orca/store"
)

// MemorySubmitParams describes an owner correction; the host supplies its source quote.
type MemorySubmitParams struct {
	Action      string `json:"action" jsonschema:"required,enum=create,enum=update,enum=delete"`
	ID          string `json:"id,omitempty"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Type        string `json:"type,omitempty" jsonschema:"enum=rule,enum=preference,enum=fact,enum=reference"`
	Scope       string `json:"scope,omitempty" jsonschema:"enum=owner,enum=repo,enum=run"`
	Body        string `json:"body,omitempty"`
}

type MemoryUndoParams struct{}

// MemoryEditor keeps the last reversible memory change for one conversation.
//
//nolint:govet
type MemoryEditor struct {
	mu        sync.Mutex
	last      *store.MemoryEntry
	previous  *store.MemoryEntry
	directive *store.Directive
}

func (e *MemoryEditor) Submit(ctx context.Context, st store.Store, runID, id, quote string, p MemorySubmitParams) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	action := strings.ToLower(strings.TrimSpace(p.Action))
	if action != "create" && action != "update" && action != "delete" {
		return "", fmt.Errorf("%w: memory action must be create, update or delete", store.ErrInvalidArgument)
	}
	if strings.TrimSpace(quote) == "" {
		return "", fmt.Errorf("%w: owner source quote required", store.ErrInvalidArgument)
	}
	if strings.TrimSpace(p.ID) != "" {
		id = strings.TrimSpace(p.ID)
	}
	previous, err := st.Memories().Get(ctx, "", id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	if previous != nil && previous.RunID != "" && previous.RunID != runID {
		return "", store.ErrNotFound
	}
	if action == "create" && previous != nil {
		return "", fmt.Errorf("%w: memory already exists", store.ErrInvalidArgument)
	}
	if action != "create" && previous == nil {
		return "", store.ErrNotFound
	}
	directive, err := st.Directives().Get(ctx, runID, id)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	if action == "delete" {
		if err := st.Memories().Delete(ctx, previous.Scope, id); err != nil {
			return "", err
		}
		if directive != nil {
			if err := st.Directives().UpdateStatus(ctx, runID, id, store.DirectiveRevoked); err != nil {
				return "", err
			}
		}
		e.last, e.previous, e.directive = previous, previous, directive
		return "Forgot memory " + id + "; undo to restore.", nil
	}
	m := store.MemoryEntry{ID: id, Scope: "run", RunID: runID, Type: "rule", ProposedBy: "owner"}
	if previous != nil {
		m = *previous
	}
	if p.Name != "" {
		m.Name = p.Name
	}
	if p.Description != "" {
		m.Description = p.Description
	}
	if p.Body != "" {
		m.Body = p.Body
	}
	if p.Type != "" {
		m.Type = p.Type
	}
	if p.Scope != "" {
		m.Scope = p.Scope
	}
	if m.Scope != "run" {
		m.RunID = ""
	} else {
		m.RunID = runID
	}
	m.Source, m.ProposedBy = quote, "owner"
	if m.Description == "" {
		m.Description = m.Body
	}
	if m.Type == "rule" {
		if err := ValidateStandingRule(m.Name+"\n"+m.Description+"\n"+m.Body, quote); err != nil {
			return "", err
		}
	}
	saved, err := st.Memories().Put(ctx, m)
	if err != nil {
		return "", err
	}
	e.last, e.previous, e.directive = &saved, previous, directive
	return "Saved memory " + id + ": " + strings.Join(strings.Fields(m.Name), " ") + "; undo or forget to revert.", nil
}

// StarterSetMemorySource is the Source marker on pre-seeded starter memories.
const StarterSetMemorySource = "orca starter set"

var (
	plainOwnerNoNewWorkPhrases = []string{
		"no new lanes",
		"start no new lanes",
		"do not start new lanes",
		"don't start new lanes",
		"do not spawn",
		"don't spawn",
		"stop spawning",
		"pause spawning",
		"no additional lanes",
		"no more lanes",
		"hold new lanes",
		"hold off on new lanes",
		"block new lanes",
		"pause new lanes",
		"no new work",
		"block all new work",
	}
	noNewWorkRulePhrases = append(slices.Clone(plainOwnerNoNewWorkPhrases),
		"standing owner hold",
		"standing hold",
	)
	negatedNoNewWorkPrefixes = []string{
		"not ",
		"no ",
		"never ",
		"did not ",
		"didn't ",
		"do not want",
		"don't want",
		"without ",
		"remove ",
		"forget ",
		"undo ",
		"drop ",
		"revoke ",
		"clear ",
		"isn't ",
		"is not ",
		"wasn't ",
		"was not ",
	}
)

// stripQuotedSpans removes single- and double-quoted spans so quoted mentions
// (such as "I do not want a 'no new lanes' rule") are not treated as direct
// owner instructions.
func stripQuotedSpans(s string) string {
	var b strings.Builder
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '"' {
			quote = c
			b.WriteByte(' ')
			continue
		}
		if c == '\'' {
			prevAlpha := i > 0 && ((s[i-1] >= 'a' && s[i-1] <= 'z') || (s[i-1] >= 'A' && s[i-1] <= 'Z'))
			nextAlpha := i+1 < len(s) && ((s[i+1] >= 'a' && s[i+1] <= 'z') || (s[i+1] >= 'A' && s[i+1] <= 'Z'))
			if !prevAlpha || !nextAlpha {
				quote = c
				b.WriteByte(' ')
				continue
			}
		}
		b.WriteByte(c)
	}
	return b.String()
}

// ClaimsNoNewWork reports whether text states a standing hold or no-new-lanes
// restriction.
func ClaimsNoNewWork(text string) bool {
	lower := strings.ToLower(strings.TrimSpace(text))
	for _, p := range noNewWorkRulePhrases {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// OwnerPlainlyStatedNoNewWork reports whether quote contains an affirmative,
// unquoted owner instruction to block or pause new lanes/work.
func OwnerPlainlyStatedNoNewWork(quote string) bool {
	unquoted := strings.ToLower(stripQuotedSpans(quote))
	for _, p := range plainOwnerNoNewWorkPhrases {
		idx := strings.Index(unquoted, p)
		for idx >= 0 {
			prefix := unquoted[:idx]
			if sep := strings.LastIndexAny(prefix, ".;\n!?"); sep >= 0 {
				prefix = prefix[sep+1:]
			}
			prefixTrim := strings.TrimSpace(prefix)
			negated := false
			for _, neg := range negatedNoNewWorkPrefixes {
				if strings.Contains(prefix, neg) || strings.HasSuffix(prefixTrim, strings.TrimSpace(neg)) {
					negated = true
					break
				}
			}
			if !negated {
				return true
			}
			rest := unquoted[idx+len(p):]
			next := strings.Index(rest, p)
			if next < 0 {
				break
			}
			idx += len(p) + next
		}
	}
	return false
}

func isWordChar(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9')
}

func containsWholeWord(s, word string) bool {
	for start := 0; start < len(s); {
		idx := strings.Index(s[start:], word)
		if idx < 0 {
			return false
		}
		pos := start + idx
		end := pos + len(word)
		leftOK := pos == 0 || !isWordChar(s[pos-1])
		rightOK := end == len(s) || !isWordChar(s[end])
		if leftOK && rightOK {
			return true
		}
		start = pos + 1
	}
	return false
}

// ValidateStandingRule ensures a standing rule or directive never invents a
// no-new-lanes / work hold unless the owner plainly stated it, and never treats
// "keep the hold" (which means orca's lease on an adopted run) as a standing
// work hold.
func ValidateStandingRule(ruleText, ownerQuote string) error {
	ruleLower := strings.ToLower(strings.TrimSpace(ruleText))
	quoteLower := strings.ToLower(strings.TrimSpace(ownerQuote))
	claimsNoNewWork := ClaimsNoNewWork(ruleText)
	ruleMentionsHold := claimsNoNewWork || (containsWholeWord(ruleLower, "hold") && !strings.Contains(ruleLower, "lease"))
	quoteMentionsLeaseHold := (strings.Contains(quoteLower, "keep the hold") ||
		strings.Contains(quoteLower, "keep hold") ||
		strings.Contains(quoteLower, "hold on the run") ||
		strings.Contains(quoteLower, "hold on run"))
	ownerPlainlyStated := OwnerPlainlyStatedNoNewWork(ownerQuote)
	if claimsNoNewWork && !ownerPlainlyStated {
		return fmt.Errorf("%w: standing rule cannot block new lanes or work unless the owner states it plainly (in an adopted run, 'hold' means orca's lease on the run)", store.ErrInvalidArgument)
	}
	if quoteMentionsLeaseHold && ruleMentionsHold && !ownerPlainlyStated {
		return fmt.Errorf("%w: 'keep the hold' on an adopted run means keep orca's lease on the run, not a standing rule blocking work", store.ErrInvalidArgument)
	}
	return nil
}

// ValidateDirectiveText checks a submitted directive (standing rule or instruction)
// against the owner's quote so "keep the hold" never becomes a standing rule or
// an invented no-new-lanes block.
func ValidateDirectiveText(kind store.DirectiveKind, text, ownerQuote string) error {
	if kind == store.DirectiveStanding || ClaimsNoNewWork(text) {
		return ValidateStandingRule(text, ownerQuote)
	}
	return nil
}

// StandingRule is a deduplicated active standing rule from directives or memories.
type StandingRule struct {
	ID    string
	Scope string
	Text  string
	RunID string
}

// FormatLine formats one standing rule with its undo/forget hint.
func (r StandingRule) FormatLine(includeScope bool) string {
	if includeScope {
		return fmt.Sprintf("%s [scope=%s]: %s (say \"undo\" or \"forget %s\" to remove)", r.ID, cmp.Or(r.Scope, "run"), r.Text, r.ID)
	}
	return fmt.Sprintf("%s: %s (say \"undo\" or \"forget %s\" to remove)", r.ID, r.Text, r.ID)
}

// WriteStandingRulesSection appends the model-facing "Standing Rules (N):"
// section used by status tools.
func WriteStandingRulesSection(b *strings.Builder, rules []StandingRule) {
	fmt.Fprintf(b, "\nStanding Rules (%d):\n", len(rules))
	if len(rules) == 0 {
		b.WriteString("(none)\n")
		return
	}
	for _, r := range rules {
		fmt.Fprintf(b, "- %s\n", r.FormatLine(true))
	}
}

// ListStandingRules returns active standing rules for runID from both the
// directive store and rule-type memories (excluding starter-set entries),
// deduplicated by ID.
func ListStandingRules(ctx context.Context, st store.Store, runID string, now time.Time) []StandingRule {
	if st == nil {
		return nil
	}
	seen := make(map[string]bool)
	var out []StandingRule
	if st.Directives() != nil {
		if dirs, err := st.Directives().List(ctx, store.DirectiveFilter{
			RunID:      runID,
			Kinds:      []store.DirectiveKind{store.DirectiveStanding},
			ActiveOnly: true,
		}); err == nil {
			for i := range dirs {
				d := &dirs[i]
				if d.Kind != store.DirectiveStanding || d.Status != store.DirectiveActive {
					continue
				}
				if !now.IsZero() && d.ExpiresAt != nil && !now.Before(*d.ExpiresAt) {
					continue
				}
				seen[d.ID] = true
				out = append(out, StandingRule{
					ID:    d.ID,
					Scope: cmp.Or(d.Scope, "run"),
					Text:  d.Text,
					RunID: d.RunID,
				})
			}
		}
	}
	if st.Memories() != nil {
		if mems, err := st.Memories().List(ctx, store.MemoryFilter{RunID: runID, Type: "rule"}); err == nil {
			for i := range mems {
				m := &mems[i]
				if m.Source == StarterSetMemorySource || seen[m.ID] {
					continue
				}
				seen[m.ID] = true
				out = append(out, StandingRule{
					ID:    m.ID,
					Scope: cmp.Or(m.Scope, "run"),
					Text:  cmp.Or(strings.TrimSpace(m.Body), strings.TrimSpace(m.Description), strings.TrimSpace(m.Name)),
					RunID: m.RunID,
				})
			}
		}
	}
	return out
}

func (e *MemoryEditor) Undo(ctx context.Context, st store.Store) (string, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.last == nil {
		return "No memory change to undo.", nil
	}
	var err error
	if e.previous != nil {
		_, err = st.Memories().Put(ctx, *e.previous)
	} else {
		err = st.Memories().Delete(ctx, e.last.Scope, e.last.ID)
	}
	if err != nil {
		return "", err
	}
	if e.directive != nil {
		status := e.directive.Status
		if e.previous == nil {
			status = store.DirectiveRevoked
		}
		if err := st.Directives().UpdateStatus(ctx, e.directive.RunID, e.directive.ID, status); err != nil {
			return "", err
		}
	}
	id := e.last.ID
	e.last, e.previous, e.directive = nil, nil, nil
	return "Undid memory " + id + ".", nil
}
