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
