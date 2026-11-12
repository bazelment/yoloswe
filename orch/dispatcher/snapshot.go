package dispatcher

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bazelment/yoloswe/orch/clock"
	"github.com/bazelment/yoloswe/orch/lane"
	"github.com/bazelment/yoloswe/orch/recipe"
	"github.com/bazelment/yoloswe/orch/store"
)

// Default snapshot sizing limits. Target is strictly < 30k tokens.
const (
	DefaultMaxSnapshotTokens    = 24000
	MaxSnapshotTokensCeiling    = 29500
	DefaultBacklogTopN          = 25
	DefaultMaxObjectiveBytes    = 4096
	DefaultMaxDirectiveBytes    = 512
	DefaultMaxItemExcerptBytes  = 512
	DefaultMaxEventExcerptBytes = 1024
	DefaultMaxSummaryBytes      = 512
	bytesPerTokenEstimate       = 4
)

// SnapshotConfig controls how a compact Store snapshot is built for a Dispatcher turn.
type SnapshotConfig struct {
	Steers               []string
	BacklogTopN          int
	MaxSnapshotTokens    int
	MaxObjectiveBytes    int
	MaxDirectiveBytes    int
	MaxItemExcerptBytes  int
	MaxEventExcerptBytes int
}

func (c SnapshotConfig) withDefaults() SnapshotConfig {
	if c.BacklogTopN <= 0 {
		c.BacklogTopN = DefaultBacklogTopN
	}
	if c.MaxSnapshotTokens <= 0 {
		c.MaxSnapshotTokens = DefaultMaxSnapshotTokens
	}
	if c.MaxSnapshotTokens > MaxSnapshotTokensCeiling {
		c.MaxSnapshotTokens = MaxSnapshotTokensCeiling
	}
	if c.MaxObjectiveBytes <= 0 {
		c.MaxObjectiveBytes = DefaultMaxObjectiveBytes
	}
	if c.MaxDirectiveBytes <= 0 {
		c.MaxDirectiveBytes = DefaultMaxDirectiveBytes
	}
	if c.MaxItemExcerptBytes <= 0 {
		c.MaxItemExcerptBytes = DefaultMaxItemExcerptBytes
	}
	if c.MaxEventExcerptBytes <= 0 {
		c.MaxEventExcerptBytes = DefaultMaxEventExcerptBytes
	}
	return c
}

// Snapshot is the rendered, token-bounded view of a Run's Store state and
// incoming judgment event batch.
type Snapshot struct {
	RunID           string
	Text            string
	EstimatedTokens int
	DirectiveCount  int
	BacklogTotal    int
	BacklogShown    int
	LaneCount       int
	EventCount      int
	Truncated       bool
}

// EstimateTokens estimates the token count of s using the standard ~4 UTF-8
// bytes per token heuristic.
func EstimateTokens(s string) int {
	if s == "" {
		return 0
	}
	return (len(s) + bytesPerTokenEstimate - 1) / bytesPerTokenEstimate
}

// BuildSnapshot constructs a compact, deterministic Markdown snapshot from st
// for runID and the given judgment event batch.
func BuildSnapshot(
	ctx context.Context,
	st store.Store,
	clk clock.Clock,
	runID string,
	events []store.Event,
	cfg SnapshotConfig,
) (Snapshot, error) {
	if clk == nil {
		clk = clock.NewRealClock()
	}
	cfg = cfg.withDefaults()
	now := clk.Now().UTC()

	run, err := st.Runs().Get(ctx, runID)
	if err != nil {
		return Snapshot{}, err
	}

	directives, err := st.Directives().List(ctx, store.DirectiveFilter{
		RunID:      runID,
		ActiveOnly: true,
	})
	if err != nil {
		return Snapshot{}, err
	}

	allBacklog, err := st.Backlog().List(ctx, store.BacklogFilter{
		RunID: runID,
	})
	if err != nil {
		return Snapshot{}, err
	}

	lanes, err := st.Lanes().List(ctx, store.LaneFilter{
		RunID: runID,
	})
	if err != nil {
		return Snapshot{}, err
	}

	maxTotalBytes := cfg.MaxSnapshotTokens * bytesPerTokenEstimate
	// Allocate per-section byte budgets so no single large table starves the
	// event batch at the bottom of the snapshot.
	dirBudget := maxTotalBytes / 6
	backlogBudget := maxTotalBytes / 4
	laneBudget := maxTotalBytes / 4
	eventBudget := maxTotalBytes / 3

	var b strings.Builder
	truncated := false

	fmt.Fprintf(&b, "# Run Snapshot: `%s`\n", run.ID)
	fmt.Fprintf(&b, "Now: %s\n", now.Format(time.RFC3339))
	fmt.Fprintf(&b, "Default Recipe: %s\n", cmp.Or(run.DefaultRecipe, recipe.DefaultName))
	if run.PlanRef != "" {
		fmt.Fprintf(&b, "Plan Ref: %s\n", truncateText(run.PlanRef, 256, &truncated))
	}
	b.WriteString("\n## Objective\n")
	if strings.TrimSpace(run.Objective) == "" {
		b.WriteString("(none)\n")
	} else {
		b.WriteString(truncateText(strings.TrimSpace(run.Objective), cfg.MaxObjectiveBytes, &truncated))
		b.WriteString("\n")
	}

	if len(cfg.Steers) > 0 {
		b.WriteString("\n## Queued Steers\n")
		for i, stMsg := range cfg.Steers {
			fmt.Fprintf(&b, "%d. %s\n", i+1, truncateText(strings.TrimSpace(stMsg), cfg.MaxDirectiveBytes, &truncated))
		}
	}

	fmt.Fprintf(&b, "\n## Active Directives (%d)\n", len(directives))
	if len(directives) == 0 {
		b.WriteString("(none)\n")
	} else {
		secStart := b.Len()
		for i := range directives {
			if b.Len()-secStart >= dirBudget {
				truncated = true
				fmt.Fprintf(&b, "... (%d more directives omitted for token budget)\n", len(directives)-i)
				break
			}
			d := directives[i]
			exp := "never"
			if d.ExpiresAt != nil {
				exp = d.ExpiresAt.UTC().Format(time.RFC3339)
			}
			scope := cmp.Or(d.Scope, "run")
			text := singleLine(truncateText(d.Text, cfg.MaxDirectiveBytes, &truncated))
			if d.Ref != "" {
				fmt.Fprintf(&b, "- [%s] kind=%s source=%s scope=%s expires=%s ref=%s: %s\n",
					d.ID, d.Kind, d.Source, scope, exp, d.Ref, text)
			} else {
				fmt.Fprintf(&b, "- [%s] kind=%s source=%s scope=%s expires=%s: %s\n",
					d.ID, d.Kind, d.Source, scope, exp, text)
			}
		}
	}

	// Prioritize non-terminal backlog items ahead of terminal items within BacklogTopN.
	counts := make(map[store.BacklogStatus]int)
	var actionableItems []store.BacklogItem
	var terminalItems []store.BacklogItem
	for i := range allBacklog {
		it := allBacklog[i]
		counts[it.Status]++
		if it.Status.Terminal() {
			terminalItems = append(terminalItems, it)
		} else {
			actionableItems = append(actionableItems, it)
		}
	}
	orderedBacklog := slices.Concat(actionableItems, terminalItems)
	topItems := orderedBacklog
	if len(topItems) > cfg.BacklogTopN {
		topItems = topItems[:cfg.BacklogTopN]
	}

	fmt.Fprintf(&b, "\n## Backlog (showing %d/%d; open=%d in_progress=%d blocked=%d done=%d dropped=%d)\n",
		len(topItems), len(allBacklog),
		counts[store.BacklogOpen],
		counts[store.BacklogInProgress],
		counts[store.BacklogBlocked],
		counts[store.BacklogDone],
		counts[store.BacklogDropped],
	)
	shownBacklog := 0
	if len(topItems) == 0 {
		b.WriteString("(empty)\n")
	} else {
		secStart := b.Len()
		for i := range topItems {
			if b.Len()-secStart >= backlogBudget {
				truncated = true
				fmt.Fprintf(&b, "... (%d more backlog items omitted for token budget)\n", len(topItems)-i)
				break
			}
			it := topItems[i]
			shownBacklog++
			fmt.Fprintf(&b, "- **%s** [%s #%d %s]", it.ID, it.Priority, it.Order, it.Status)
			if it.LaneID != "" {
				fmt.Fprintf(&b, " lane=%s", it.LaneID)
			}
			if it.RecipeName != "" {
				fmt.Fprintf(&b, " recipe=%s", it.RecipeName)
			}
			if len(it.DependsOn) > 0 {
				fmt.Fprintf(&b, " deps=[%s]", strings.Join(it.DependsOn, ","))
			}
			fmt.Fprintf(&b, ": %s", singleLine(truncateText(it.Title, 200, &truncated)))
			if it.Acceptance != "" {
				fmt.Fprintf(&b, " | acceptance: %s", singleLine(truncateText(it.Acceptance, cfg.MaxItemExcerptBytes/2, &truncated)))
			}
			if it.Description != "" {
				fmt.Fprintf(&b, " | desc: %s", singleLine(truncateText(it.Description, cfg.MaxItemExcerptBytes/2, &truncated)))
			}
			b.WriteString("\n")
		}
	}

	fmt.Fprintf(&b, "\n## Lanes (%d)\n", len(lanes))
	if len(lanes) == 0 {
		b.WriteString("(none)\n")
	} else {
		b.WriteString("| Lane | Item | Status | Recipe | Phase | Round | MaxRound | Attempt | Age | PR | Checks | Approval | LastEvent |\n")
		b.WriteString("|---|---|---|---|---|---|---|---|---|---|---|---|---|\n")
		secStart := b.Len()
		for i := range lanes {
			if b.Len()-secStart >= laneBudget {
				truncated = true
				fmt.Fprintf(&b, "| ... | (%d more lanes omitted) | | | | | | | | | | | |\n", len(lanes)-i)
				break
			}
			ln := lanes[i]
			recLabel := cmp.Or(ln.RecipeName, recipe.DefaultName)
			if ln.RecipeVersion > 0 {
				recLabel = fmt.Sprintf("%s@v%d", recLabel, ln.RecipeVersion)
			}
			phaseName := cmp.Or(ln.Phase, "-")
			maxRound := 0
			if ln.Phase != "" {
				maxRound = ln.MaxRound(ln.Phase)
			}
			age := formatLaneAge(now, ln)
			prStr := "-"
			if ln.PR > 0 {
				prStr = fmt.Sprintf("#%d", ln.PR)
				if ln.PRHead != "" {
					prStr += "@" + shortSHA(ln.PRHead)
				}
			}
			approvalStr := "-"
			if ln.PR > 0 || ln.ApprovalSHA != "" || ln.PRHead != "" {
				if ln.ApprovalStale() {
					approvalStr = "STALE"
				} else {
					approvalStr = "OK(" + shortSHA(ln.ApprovalSHA) + ")"
				}
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %d | %d | %d | %s | %s | %s | %s | %s |\n",
				ln.ID,
				cmp.Or(ln.ItemID, "-"),
				ln.Status,
				recLabel,
				phaseName,
				ln.Round,
				maxRound,
				ln.Attempt,
				age,
				prStr,
				cmp.Or(ln.Checks, "-"),
				approvalStr,
				cmp.Or(ln.LastEventID, "-"),
			)
		}
	}

	fmt.Fprintf(&b, "\n## Judgment Event Batch (%d)\n", len(events))
	if len(events) == 0 {
		b.WriteString("(none)\n")
	} else {
		secStart := b.Len()
		for i := range events {
			if b.Len()-secStart >= eventBudget {
				truncated = true
				fmt.Fprintf(&b, "... (%d more events omitted for token budget)\n", len(events)-i)
				break
			}
			ev := events[i]
			urgentTag := ""
			if ev.Urgent {
				urgentTag = " [URGENT]"
			}
			fmt.Fprintf(&b, "- **%s** (seq=%d kind=%s lane=%s source=%s)%s",
				cmp.Or(ev.ID, fmt.Sprintf("seq-%d", ev.Seq)),
				ev.Seq,
				ev.Kind,
				cmp.Or(ev.LaneID, "-"),
				cmp.Or(ev.Source, "-"),
				urgentTag,
			)
			if ev.Summary != "" {
				fmt.Fprintf(&b, ": %s", singleLine(truncateText(ev.Summary, DefaultMaxSummaryBytes, &truncated)))
			}
			b.WriteString("\n")
			if len(ev.Payload) > 0 {
				excerpt := truncateText(strings.TrimSpace(string(ev.Payload)), cfg.MaxEventExcerptBytes, &truncated)
				fmt.Fprintf(&b, "  payload: `%s`\n", singleLine(excerpt))
			}
		}
	}

	text := truncateWithSuffix(b.String(), maxTotalBytes, "\n...[snapshot truncated to fit <30k token budget]\n", &truncated)

	return Snapshot{
		RunID:           runID,
		Text:            text,
		EstimatedTokens: EstimateTokens(text),
		DirectiveCount:  len(directives),
		BacklogTotal:    len(allBacklog),
		BacklogShown:    shownBacklog,
		LaneCount:       len(lanes),
		EventCount:      len(events),
		Truncated:       truncated,
	}, nil
}

func formatLaneAge(now time.Time, ln lane.Lane) string {
	ref := ln.LastEventAt
	if ref.IsZero() {
		ref = ln.UpdatedAt
	}
	if ref.IsZero() {
		ref = ln.CreatedAt
	}
	if ref.IsZero() || now.Before(ref) {
		return "0s"
	}
	d := now.Sub(ref).Round(time.Second)
	return d.String()
}

func shortSHA(sha string) string {
	sha = strings.TrimSpace(sha)
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func singleLine(s string) string {
	s = strings.ReplaceAll(s, "\r\n", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	return strings.Join(strings.Fields(s), " ")
}

func truncateText(s string, maxBytes int, truncated *bool) string {
	return truncateWithSuffix(s, maxBytes, "...[truncated]", truncated)
}

func truncateWithSuffix(s string, maxBytes int, suffix string, truncated *bool) string {
	if maxBytes <= 0 || len(s) <= maxBytes {
		return s
	}
	if truncated != nil {
		*truncated = true
	}
	if maxBytes <= len(suffix) {
		return validUTF8Prefix(s, maxBytes)
	}
	return validUTF8Prefix(s, maxBytes-len(suffix)) + suffix
}

func validUTF8Prefix(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	s = s[:maxBytes]
	for s != "" && !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}
