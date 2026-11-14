package store

import (
	"context"
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
	ForkThread(ctx context.Context, parentThreadID, newThreadID, upToTurnID string) (Thread, error)
	StartTurn(ctx context.Context, turn Turn) (Turn, error)
	SteerTurn(ctx context.Context, threadID, turnID, message string) error
	CompleteTurn(ctx context.Context, threadID, turnID string, status TurnStatus, usage model.Usage, errMsg string) error
	GetTurn(ctx context.Context, threadID, turnID string) (*Turn, error)
	ListTurns(ctx context.Context, threadID string) ([]Turn, error)
	AppendItem(ctx context.Context, item Item) (Item, error)
	ListItems(ctx context.Context, threadID, turnID string, limit, offset int) ([]Item, error)
}

// RecipeStore persists versioned natural-language recipes and their compiled forms.
type RecipeStore interface {
	Save(ctx context.Context, r Recipe) (Recipe, error)
	Get(ctx context.Context, runID, name string, version int) (*Recipe, error)
	ListVersions(ctx context.Context, runID, name string) ([]Recipe, error)
	ListLatest(ctx context.Context, runID string) ([]Recipe, error)
}
