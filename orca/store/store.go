package store

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/orca/model"
)

// Store is the persistence boundary for the swarm orchestrator harness.
type Store interface {
	Runs() RunStore
	Backlog() BacklogStore
	Lanes() LaneStore
	Directives() DirectiveStore
	Decisions() DecisionStore
	Events() EventStore
	Threads() ThreadStore
	Recipes() RecipeStore
	Memories() MemoryEntryStore
	Notes() NoteStore
	Close() error
}

type MemoryFilter struct{ RunID, Scope, Type string }
type MemoryEntryStore interface {
	Put(context.Context, MemoryEntry) (MemoryEntry, error)
	Get(context.Context, string, string) (*MemoryEntry, error)
	List(context.Context, MemoryFilter) ([]MemoryEntry, error)
	Delete(context.Context, string, string) error
}
type NoteFilter struct{ RunID, TaskID string }
type NoteStore interface {
	Append(context.Context, RunNote) (RunNote, error)
	List(context.Context, NoteFilter) ([]RunNote, error)
}

// RunStore manages run objectives, plan references, and controller leases.
type RunStore interface {
	Create(ctx context.Context, run Run) error
	Get(ctx context.Context, runID string) (*Run, error)
	// List returns every run in the store, ordered by ID.
	List(ctx context.Context) ([]Run, error)
	UpdateObjective(ctx context.Context, runID, objective, planRef string) error
	GetSettings(ctx context.Context, runID string) (RunSettings, error)
	UpdateSettings(ctx context.Context, runID string, settings RunSettings) error
	AcquireLease(ctx context.Context, runID, holderID string, ttl time.Duration) (*Lease, error)
	HeartbeatLease(ctx context.Context, runID, holderID string, ttl time.Duration) (*Lease, error)
	ReleaseLease(ctx context.Context, runID, holderID string) error
}

// BacklogStore manages a run's prioritized work items and mutation history.
type BacklogStore interface {
	Add(ctx context.Context, item BacklogItem) error
	Get(ctx context.Context, runID, itemID string) (*BacklogItem, error)
	Update(ctx context.Context, item BacklogItem, actor, reason string) error
	Reorder(ctx context.Context, runID string, itemIDs []string, actor string) error
	Close(ctx context.Context, runID, itemID string, status BacklogStatus, actor, reason string) error
	List(ctx context.Context, filter BacklogFilter) ([]BacklogItem, error)
}

// LaneStore manages lane state, phase/round progress, and active session/terminal IDs.
type LaneStore interface {
	Upsert(ctx context.Context, l Lane) error
	Get(ctx context.Context, runID, laneID string) (*Lane, error)
	List(ctx context.Context, filter LaneFilter) ([]Lane, error)
	Delete(ctx context.Context, runID, laneID string) error
}

// DirectiveStore manages standing owner rules, instructions, plan imports, and recipe edits.
type DirectiveStore interface {
	Add(ctx context.Context, d Directive) error
	Get(ctx context.Context, runID, directiveID string) (*Directive, error)
	UpdateStatus(ctx context.Context, runID, directiveID string, status DirectiveStatus) error
	List(ctx context.Context, filter DirectiveFilter) ([]Directive, error)
}

// DecisionStore is an append-only audit log of deterministic and LLM decisions.
type DecisionStore interface {
	Append(ctx context.Context, d Decision) (Decision, error)
	List(ctx context.Context, filter DecisionFilter) ([]Decision, error)
	SetUsage(ctx context.Context, runID string, seq int64, usage model.Usage) error
}

// AttributeTurnUsage attributes CLI turn usage to the first unattributed decision
// row in decisions and persists it via DecisionStore.SetUsage.
func AttributeTurnUsage(
	ctx context.Context,
	decisions DecisionStore,
	runID string,
	rows []Decision,
	usage model.Usage,
) {
	if len(rows) == 0 || (usage.TotalTokens() <= 0 && usage.CostUSD <= 0) {
		return
	}
	for i := range rows {
		if rows[i].InputTokens == 0 && rows[i].OutputTokens == 0 && rows[i].CostUSD == 0 {
			rows[i].InputTokens = usage.InputTokens
			rows[i].OutputTokens = usage.OutputTokens
			rows[i].CostUSD = usage.CostUSD
			if decisions != nil && rows[i].Seq > 0 {
				_ = decisions.SetUsage(ctx, runID, rows[i].Seq, usage)
			}
			return
		}
	}
}

// EventStore is an append-only event inbox with per-consumer monotonic cursors.
type EventStore interface {
	Append(ctx context.Context, ev Event) (Event, error)
	List(ctx context.Context, filter EventFilter) ([]Event, error)
	GetCursor(ctx context.Context, runID, consumer string) (EventCursor, error)
	SetCursor(ctx context.Context, runID, consumer string, seq int64) error
}

// ThreadStore persists thread, turn, and item records for dispatcher, concierge, and planner roles.
type ThreadStore interface {
	StartThread(ctx context.Context, t Thread) (Thread, error)
	GetThread(ctx context.Context, threadID string) (*Thread, error)
	ListThreads(ctx context.Context, filter ThreadFilter) ([]Thread, error)
	UpdateThreadStatus(ctx context.Context, threadID string, status ThreadStatus) error
	SetThreadGoal(ctx context.Context, threadID, goal string) error
	SetThreadResumeID(ctx context.Context, threadID, resumeID string) error
	ForkThread(ctx context.Context, parentThreadID, newThreadID, upToTurnID string) (Thread, error)
	StartTurn(ctx context.Context, turn Turn) (Turn, error)
	SteerTurn(ctx context.Context, threadID, turnID, message string) error
	CompleteTurn(ctx context.Context, threadID, turnID string, status TurnStatus, usage model.Usage, errMsg string) error
	GetTurn(ctx context.Context, threadID, turnID string) (*Turn, error)
	ListTurns(ctx context.Context, threadID string) ([]Turn, error)
	AppendItem(ctx context.Context, item Item) (Item, error)
	ListItems(ctx context.Context, threadID, turnID string, limit, offset int) ([]Item, error)
}

// AppendMCPToolCallItems records a tool_call item and its matching tool_result
// item on threads for an observed MCP tool invocation.
func AppendMCPToolCallItems(
	ctx context.Context,
	threads ThreadStore,
	threadID, turnID, callID, name string,
	args json.RawMessage,
	output string,
	isError bool,
) {
	if threads == nil || threadID == "" {
		return
	}
	_, _ = threads.AppendItem(ctx, Item{
		ThreadID: threadID,
		TurnID:   turnID,
		Kind:     ItemKindToolCall,
		Name:     name,
		CallID:   callID,
		Payload:  args,
	})
	_, _ = threads.AppendItem(ctx, Item{
		ThreadID: threadID,
		TurnID:   turnID,
		Kind:     ItemKindToolResult,
		Name:     name,
		CallID:   callID,
		Content:  output,
		IsError:  isError,
	})
}

// RunRoleAgentTurn executes one user turn on sessions for (runID, threadID, actor),
// loading and persisting the thread's resume_id and recording observed MCP tool
// calls as thread items.
func RunRoleAgentTurn(
	ctx context.Context,
	threads ThreadStore,
	sessions *model.AgentSessionMap,
	runID, threadID, turnID, actor string,
	sessCfg model.AgentSessionConfig,
	req model.AgentTurnRequest,
	logger *slog.Logger,
) (*model.AgentTurnResult, error) {
	if strings.TrimSpace(sessCfg.ResumeID) == "" && threads != nil && threadID != "" {
		if th, err := threads.GetThread(ctx, threadID); err == nil && th != nil {
			sessCfg.ResumeID = strings.TrimSpace(th.ResumeID)
		}
	}
	if strings.TrimSpace(sessCfg.WorkDir) == "" {
		sessCfg.WorkDir = model.DeterministicSessionWorkDir("", runID, cmp.Or(actor, "agent"))
	}
	sess := sessions.GetOrCreate(runID, sessCfg)

	cleanupCtx := context.WithoutCancel(ctx)
	prevObs := req.Observer
	var callSeq int
	req.Observer = func(name string, args json.RawMessage, output string, isError bool) {
		callSeq++
		callID := fmt.Sprintf("mcp-%d", callSeq)
		if logger != nil && actor != "" {
			logger.Info("tool call",
				"actor", actor,
				"tool", name,
				"is_error", isError,
			)
		}
		AppendMCPToolCallItems(cleanupCtx, threads, threadID, turnID, callID, name, args, output, isError)
		if prevObs != nil {
			prevObs(name, args, output, isError)
		}
	}

	turnRes, err := sess.Turn(ctx, req)
	if threads != nil && threadID != "" {
		if sessID := sess.SessionID(); sessID != "" {
			_ = threads.SetThreadResumeID(cleanupCtx, threadID, sessID)
		}
	}
	return turnRes, err
}

// RecipeStore persists versioned natural-language recipes and their compiled forms.
type RecipeStore interface {
	Save(ctx context.Context, r Recipe) (Recipe, error)
	Get(ctx context.Context, runID, name string, version int) (*Recipe, error)
	ListVersions(ctx context.Context, runID, name string) ([]Recipe, error)
	ListLatest(ctx context.Context, runID string) ([]Recipe, error)
}

// EstimateUsageCostUSD estimates model usage cost from a model name or actor role.
func EstimateUsageCostUSD(modelOrRole string, inputTokens, outputTokens int64) float64 {
	if inputTokens <= 0 && outputTokens <= 0 {
		return 0
	}
	lower := strings.ToLower(strings.TrimSpace(modelOrRole))
	modelName := lower
	switch lower {
	case "", "dispatcher", "concierge", "controller":
		modelName = "sonnet"
	case "planner":
		modelName = "opus"
	case "router":
		modelName = "haiku"
	}
	return model.CalculateCost(modelName, model.Usage{InputTokens: inputTokens, OutputTokens: outputTokens}, nil)
}

// IsDoneOrEmptyNext reports whether a Next: value indicates there is no remaining work.
func IsDoneOrEmptyNext(s string) bool {
	trimmed := strings.TrimSpace(s)
	if trimmed == "" {
		return true
	}
	lower := strings.ToLower(trimmed)
	exact := strings.TrimRight(lower, ".! \t")
	if isDoneToken(exact) {
		return true
	}
	for _, prefix := range []string{
		"run done ", "swarm done ", "nothing left ", "no open work ", "no remaining work ",
	} {
		if strings.HasPrefix(lower, prefix) {
			return true
		}
	}
	cutIdx := -1
	cutSep := ""
	for _, sep := range []string{".", ";", ":", "—", " - ", " (", ",", "\n"} {
		if idx := strings.Index(lower, sep); idx > 0 && (cutIdx < 0 || idx < cutIdx) {
			cutIdx = idx
			cutSep = sep
		}
	}
	if cutIdx > 0 {
		lead := strings.TrimSpace(lower[:cutIdx])
		rest := strings.TrimSpace(lower[cutIdx+len(cutSep):])
		if cutSep == ";" && (strings.Contains(rest, "pending") || strings.Contains(rest, "todo") || strings.Contains(rest, "open") || strings.Contains(rest, "start ") || strings.Contains(rest, "staff ")) {
			return false
		}
		if isDoneToken(lead) {
			return true
		}
	}
	return false
}

func isDoneToken(tok string) bool {
	switch tok {
	case "", "none", "n/a", "na", "nil", "nothing", "-", "done", "complete", "completed", "finished",
		"all done", "all complete", "run done", "run complete", "swarm done",
		"no open work", "no remaining work", "nothing left", "nothing left to do", "no open work remaining", "no loop armed":
		return true
	}
	return false
}

// HasExplicitNextSection reports whether any source contains a Next: line or # Next heading.
func HasExplicitNextSection(sources ...string) bool {
	for _, src := range sources {
		for _, line := range strings.Split(src, "\n") {
			trimmed := strings.TrimSpace(line)
			clean := strings.TrimLeft(trimmed, "-* \t")
			lower := strings.ToLower(clean)
			if strings.HasPrefix(lower, "next:") {
				return true
			}
			if strings.HasPrefix(trimmed, "#") {
				heading := strings.ToLower(strings.TrimSpace(strings.TrimLeft(trimmed, "#")))
				if heading == "next" || strings.HasPrefix(heading, "next ") || strings.HasPrefix(heading, "next:") {
					return true
				}
			}
		}
	}
	return false
}

// ExtractNextSection returns the latest non-empty Next: line or # Next section from sources.
func ExtractNextSection(sources ...string) string {
	for _, src := range sources {
		lines := strings.Split(src, "\n")
		var found string
		for i := 0; i < len(lines); i++ {
			trimmed := strings.TrimSpace(lines[i])
			clean := strings.TrimLeft(trimmed, "-* \t")
			lower := strings.ToLower(clean)
			if strings.HasPrefix(lower, "next:") {
				rest := strings.TrimSpace(clean[len("next:"):])
				var parts []string
				if rest != "" {
					parts = append(parts, rest)
				}
				for j := i + 1; j < len(lines); j++ {
					nextLine := lines[j]
					nextTrim := strings.TrimSpace(nextLine)
					if nextTrim == "" || strings.HasPrefix(nextTrim, "#") {
						break
					}
					nxtClean := strings.ToLower(strings.TrimLeft(nextTrim, "-* \t"))
					if strings.HasPrefix(nxtClean, "next:") {
						break
					}
					if strings.HasPrefix(nextLine, " ") || strings.HasPrefix(nextLine, "\t") || strings.HasPrefix(nextTrim, "- ") || strings.HasPrefix(nextTrim, "* ") {
						parts = append(parts, nextTrim)
						continue
					}
					break
				}
				if len(parts) > 0 {
					found = strings.Join(parts, "\n")
				}
			}
			if strings.HasPrefix(trimmed, "#") {
				heading := strings.ToLower(strings.TrimSpace(strings.TrimLeft(trimmed, "#")))
				if heading == "next" || strings.HasPrefix(heading, "next ") || strings.HasPrefix(heading, "next:") {
					var parts []string
					for j := i + 1; j < len(lines); j++ {
						nextTrim := strings.TrimSpace(lines[j])
						if strings.HasPrefix(nextTrim, "#") {
							break
						}
						if nextTrim != "" {
							parts = append(parts, nextTrim)
						}
					}
					if len(parts) > 0 {
						found = strings.Join(parts, "\n")
					}
				}
			}
		}
		if found != "" {
			return found
		}
	}
	return ""
}

// ObjectiveIsDone reports whether the latest Next: or status lines in md
// declare the run done/complete with no remaining work.
func ObjectiveIsDone(md string) bool {
	if strings.TrimSpace(md) == "" {
		return false
	}
	if HasExplicitNextSection(md) {
		next := strings.TrimSpace(ExtractNextSection(md))
		return IsDoneOrEmptyNext(next)
	}
	lines := strings.Split(md, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		clean := strings.ToLower(strings.TrimLeft(strings.TrimSpace(lines[i]), "-*# \t"))
		if clean == "" {
			continue
		}
		if strings.Contains(clean, "run done") ||
			strings.HasPrefix(clean, "status: done") ||
			strings.HasPrefix(clean, "status: complete") ||
			strings.HasPrefix(clean, "state: done") ||
			strings.HasPrefix(clean, "state: complete") {
			return true
		}
	}
	return false
}

// HourlyRunCostUSD computes the spend in USD for runID over the trailing hour
// ending at now, deduplicating costs recorded on both Decisions and Turns.
func HourlyRunCostUSD(ctx context.Context, st Store, runID string, now time.Time) (float64, error) {
	if st == nil || strings.TrimSpace(runID) == "" {
		return 0, nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	windowStart := now.Add(-time.Hour)

	decs, err := st.Decisions().List(ctx, DecisionFilter{RunID: runID, SinceTime: &windowStart})
	if err != nil {
		return 0, err
	}
	byTurn := map[string]float64{}
	var unlinkedDecCost float64
	for i := range decs {
		d := &decs[i]
		if !d.CreatedAt.IsZero() && d.CreatedAt.Before(windowStart) {
			continue
		}
		if d.Tool == "metrics.snapshot" || d.Tool == "cost.guard" || d.Tool == "mail.adopt_summary" {
			continue
		}
		c := d.CostUSD
		if c <= 0 {
			c = EstimateUsageCostUSD(cmp.Or(d.Model, d.Actor), d.InputTokens, d.OutputTokens)
		}
		if c <= 0 {
			continue
		}
		if d.TurnID != "" {
			byTurn[d.TurnID] += c
		} else {
			unlinkedDecCost += c
		}
	}

	threads, err := st.Threads().ListThreads(ctx, ThreadFilter{RunID: runID})
	if err != nil {
		return 0, err
	}
	var linkedTurnExtra, unlinkedTurnCost float64
	for i := range threads {
		if !threads[i].UpdatedAt.IsZero() && threads[i].UpdatedAt.Before(windowStart) {
			continue
		}
		turns, err := st.Threads().ListTurns(ctx, threads[i].ID)
		if err != nil {
			return 0, err
		}
		for j := range turns {
			t := &turns[j]
			ts := t.StartedAt
			if ts.IsZero() && t.CompletedAt != nil {
				ts = *t.CompletedAt
			}
			if !ts.IsZero() && ts.Before(windowStart) {
				continue
			}
			tc := t.CostUSD
			if tc <= 0 {
				tc = EstimateUsageCostUSD(string(threads[i].Role), t.InputTokens, t.OutputTokens)
			}
			if tc <= 0 {
				continue
			}
			if decCost, ok := byTurn[t.ID]; ok {
				if tc > decCost {
					linkedTurnExtra += tc - decCost
				}
			} else {
				unlinkedTurnCost += tc
			}
		}
	}

	var total float64
	for _, c := range byTurn {
		total += c
	}
	total += linkedTurnExtra + max(unlinkedDecCost, unlinkedTurnCost)
	return total, nil
}

// EnforceCostGuard checks whether runID has reached or exceeded its configured
// hourly spend limit (from RunSettings.MaxCostPerHourUSD or fallbackLimit).
// When reached, it pauses the run (RunSettings.Paused = true), records an
// owner notification directive and vetted cost.guard decision once per breach,
// and returns (true, spent, limit, message, nil).
func EnforceCostGuard(ctx context.Context, st Store, runID string, now time.Time, fallbackLimit float64) (bool, float64, float64, string, error) {
	if st == nil || strings.TrimSpace(runID) == "" {
		return false, 0, 0, "", nil
	}
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	settings, err := st.Runs().GetSettings(ctx, runID)
	if err != nil {
		return false, 0, 0, "", err
	}
	if settings.CostGuardDisabled {
		return false, 0, 0, "", nil
	}
	limit := settings.MaxCostPerHourUSD
	if limit <= 0 {
		limit = fallbackLimit
	}
	if limit <= 0 {
		return false, 0, 0, "", nil
	}
	spent, err := HourlyRunCostUSD(ctx, st, runID, now)
	if err != nil {
		return false, 0, limit, "", err
	}
	if spent < limit {
		return false, spent, limit, "", nil
	}

	wasPaused := settings.Paused
	msg := fmt.Sprintf("Cost guard paused turns for run %s: spent $%.2f in the last hour (limit $%.2f/hr).", runID, spent, limit)
	if !wasPaused {
		settings.Paused = true
		if uErr := st.Runs().UpdateSettings(ctx, runID, settings); uErr != nil {
			return true, spent, limit, msg, uErr
		}
	}

	askID := "cost-guard-" + runID
	dirID := "ask-" + askID
	if existing, dErr := st.Directives().Get(ctx, runID, dirID); dErr != nil || existing == nil {
		askPayload, _ := json.Marshal(struct {
			ID       string `json:"id"`
			RunID    string `json:"run_id"`
			Question string `json:"question"`
		}{
			ID:       askID,
			RunID:    runID,
			Question: msg,
		})
		_ = st.Directives().Add(ctx, Directive{
			ID:        dirID,
			RunID:     runID,
			Kind:      DirectiveInstruction,
			Scope:     "run",
			Ref:       "owner_ask:" + askID,
			Text:      string(askPayload),
			Status:    DirectiveActive,
			CreatedAt: now,
		})
	} else if existing.Status != DirectiveActive {
		_ = st.Directives().UpdateStatus(ctx, runID, dirID, DirectiveActive)
	}

	windowStart := now.Add(-time.Hour)
	existingDecs, _ := st.Decisions().List(ctx, DecisionFilter{RunID: runID, SinceTime: &windowStart})
	alreadyRecorded := false
	for i := range existingDecs {
		if existingDecs[i].Tool != "cost.guard" {
			continue
		}
		if existingDecs[i].CreatedAt.IsZero() || !existingDecs[i].CreatedAt.Before(windowStart) {
			alreadyRecorded = true
			break
		}
	}
	if !wasPaused || !alreadyRecorded {
		_, _ = st.Decisions().Append(ctx, Decision{
			RunID:     runID,
			Actor:     "controller",
			Tool:      "cost.guard",
			Reason:    fmt.Sprintf("hourly spend $%.2f reached limit $%.2f/hr", spent, limit),
			Result:    msg,
			Vetted:    true,
			CreatedAt: now,
		})
		payload, _ := json.Marshal(struct {
			RunID   string  `json:"run_id"`
			Message string  `json:"message"`
			Spent   float64 `json:"spent_usd"`
			Limit   float64 `json:"limit_usd"`
		}{
			RunID:   runID,
			Message: msg,
			Spent:   spent,
			Limit:   limit,
		})
		_, _ = st.Events().Append(ctx, Event{
			ID:        "cost-guard-" + runID,
			RunID:     runID,
			Kind:      EventMail,
			Class:     EventClassAuto,
			Source:    "cost.guard",
			Summary:   msg,
			Payload:   payload,
			CreatedAt: now,
		})
		return true, spent, limit, msg, nil
	}
	return true, spent, limit, "", nil
}

// NormalizeDirectiveID trims whitespace and optional surrounding brackets
// (as rendered in snapshot lines like "- [note-7] ...") from a directive ID.
func NormalizeDirectiveID(id string) string {
	s := strings.TrimSpace(id)
	for strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") && len(s) >= 2 {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	return s
}

// IsInternalDirective reports whether d is an internal machine record (such as
// an open owner_ask card, an expected lane wait deadline, a planner request,
// or the chat pause control flag) rather than a user-facing active directive.
func IsInternalDirective(d Directive) bool {
	if d.Kind == DirectivePlanImport || d.Kind == DirectiveRecipeEdit {
		return true
	}
	ref := strings.TrimSpace(d.Ref)
	return strings.HasPrefix(ref, "owner_ask:") ||
		strings.HasPrefix(ref, "named-wait:") ||
		strings.HasPrefix(ref, "planner_request:") ||
		ref == "chat:control"
}

// ListActiveDirectives returns the canonical active directives for runID across
// the directive store and run-scoped rule memories (excluding starter-set and
// any memory entry whose directive row is revoked, expired, or handled).
func ListActiveDirectives(ctx context.Context, st Store, runID string, now time.Time) ([]Directive, error) {
	if st == nil || st.Directives() == nil {
		return nil, nil
	}
	allDirs, err := st.Directives().List(ctx, DirectiveFilter{RunID: runID, ActiveOnly: false})
	if err != nil {
		return nil, err
	}
	inactiveIDs := make(map[string]bool, len(allDirs))
	seenActive := make(map[string]bool, len(allDirs))
	out := make([]Directive, 0, len(allDirs))
	for i := range allDirs {
		d := allDirs[i]
		if d.Status != DirectiveActive || (!now.IsZero() && d.ExpiresAt != nil && !now.Before(*d.ExpiresAt)) {
			inactiveIDs[d.ID] = true
			continue
		}
		seenActive[d.ID] = true
		out = append(out, d)
	}
	if st.Memories() != nil {
		if mems, mErr := st.Memories().List(ctx, MemoryFilter{RunID: runID, Type: "rule"}); mErr == nil {
			for i := range mems {
				m := &mems[i]
				if m.Source == "orca starter set" || inactiveIDs[m.ID] || seenActive[m.ID] {
					continue
				}
				if m.RunID != runID || m.RunID == "" {
					continue
				}
				seenActive[m.ID] = true
				out = append(out, Directive{
					ID:        m.ID,
					RunID:     runID,
					Kind:      DirectiveStanding,
					Source:    DirectiveSourceOwner,
					Scope:     cmp.Or(m.Scope, "run"),
					Text:      cmp.Or(strings.TrimSpace(m.Body), strings.TrimSpace(m.Description), strings.TrimSpace(m.Name)),
					Status:    DirectiveActive,
					CreatedAt: m.UpdatedAt,
				})
			}
		}
	}
	return out, nil
}

// FilterUserFacingDirectives filters out internal machine records from dirs.
func FilterUserFacingDirectives(dirs []Directive) []Directive {
	out := make([]Directive, 0, len(dirs))
	for i := range dirs {
		if !IsInternalDirective(dirs[i]) {
			out = append(out, dirs[i])
		}
	}
	return out
}

// ListUserFacingActiveDirectives returns active user-facing directives for runID.
func ListUserFacingActiveDirectives(ctx context.Context, st Store, runID string, now time.Time) ([]Directive, error) {
	dirs, err := ListActiveDirectives(ctx, st, runID, now)
	if err != nil {
		return nil, err
	}
	return FilterUserFacingDirectives(dirs), nil
}

// FilterActiveMemories removes any memory entry whose ID corresponds to an
// inactive (revoked, expired, or handled) directive on runID.
func FilterActiveMemories(ctx context.Context, st Store, runID string, memories []MemoryEntry, now time.Time) []MemoryEntry {
	if st == nil || st.Directives() == nil || len(memories) == 0 {
		return memories
	}
	allDirs, err := st.Directives().List(ctx, DirectiveFilter{RunID: runID, ActiveOnly: false})
	if err != nil || len(allDirs) == 0 {
		return memories
	}
	inactiveIDs := make(map[string]bool, len(allDirs))
	for i := range allDirs {
		d := &allDirs[i]
		if d.Status != DirectiveActive || (!now.IsZero() && d.ExpiresAt != nil && !now.Before(*d.ExpiresAt)) {
			inactiveIDs[d.ID] = true
		}
	}
	if len(inactiveIDs) == 0 {
		return memories
	}
	out := make([]MemoryEntry, 0, len(memories))
	for i := range memories {
		if !inactiveIDs[memories[i].ID] {
			out = append(out, memories[i])
		}
	}
	return out
}
