// Package store defines the persistence interfaces, domain records, in-memory
// backend, and shared contract test suite for the swarm orchestrator harness.
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/recipe"
)

// Sentinel errors returned by Store implementations.
var (
	ErrNotFound        = errors.New("orca/store: not found")
	ErrAlreadyExists   = errors.New("orca/store: already exists")
	ErrLeaseHeld       = errors.New("orca/store: lease held by another holder")
	ErrLeaseNotHeld    = errors.New("orca/store: lease not held by caller")
	ErrInvalidArgument = errors.New("orca/store: invalid argument")
	ErrConflict        = errors.New("orca/store: conflict")
)

type (
	Lane       = lane.Lane
	LaneStatus = lane.Status
	Recipe     = recipe.Recipe
)

// MemoryEntry is durable guidance for orchestration roles.
type MemoryEntry struct {
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Type        string    `json:"type"`  // rule, preference, fact, reference
	Scope       string    `json:"scope"` // owner, repo, run
	RunID       string    `json:"run_id,omitempty"`
	Source      string    `json:"source"`
	Body        string    `json:"body"`
	ProposedBy  string    `json:"proposed_by"` // owner or model
}

func (m MemoryEntry) Clone() MemoryEntry { return m }

func (m MemoryEntry) validate() error {
	if strings.TrimSpace(m.ID) == "" || strings.TrimSpace(m.Name) == "" || strings.TrimSpace(m.Body) == "" {
		return fmt.Errorf("%w: memory id, name and body required", ErrInvalidArgument)
	}
	if !slices.Contains([]string{"rule", "preference", "fact", "reference"}, m.Type) ||
		!slices.Contains([]string{"owner", "repo", "run"}, m.Scope) ||
		!slices.Contains([]string{"owner", "model"}, m.ProposedBy) {
		return fmt.Errorf("%w: invalid memory type, scope or proposed_by", ErrInvalidArgument)
	}
	if m.Scope != "run" && m.RunID != "" {
		return fmt.Errorf("%w: only run memories may have run_id", ErrInvalidArgument)
	}
	if m.Scope == "run" && strings.TrimSpace(m.RunID) == "" {
		return fmt.Errorf("%w: run memory requires run_id", ErrInvalidArgument)
	}
	return nil
}

// RunNote is an append-only note attached to a run or task.
type RunNote struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
	RunID     string    `json:"run_id"`
	TaskID    string    `json:"task_id,omitempty"`
	Phase     string    `json:"phase,omitempty"`
	Text      string    `json:"text"`
}

func (n RunNote) Clone() RunNote { return n }

// StarterMemories returns the default operating rules.
func StarterMemories() []MemoryEntry {
	return []MemoryEntry{
		{ID: "starter-inner-loop", Name: "fast inner loop", Description: "Validate the smallest case first", Type: "rule", Scope: "owner", Source: "orca starter set", Body: "Why: feedback is fastest on the smallest case. How to apply: run the narrowest deterministic check before broad work.", ProposedBy: "owner"},
		{ID: "starter-reversible", Name: "reversible decisions", Description: "Decide anything reversible", Type: "rule", Scope: "owner", Source: "orca starter set", Body: "Why: reversible choices do not need delay. How to apply: choose and proceed when undo is cheap.", ProposedBy: "owner"},
		{ID: "starter-one-way", Name: "one-way doors", Description: "Ask only on irreversible choices", Type: "rule", Scope: "owner", Source: "orca starter set", Body: "Why: owner input matters at irreversible boundaries. How to apply: ask only before a one-way door.", ProposedBy: "owner"},
		{ID: "starter-verifier", Name: "verifier independence", Description: "Verifier is never the author", Type: "rule", Scope: "owner", Source: "orca starter set", Body: "Why: independent verification catches author bias. How to apply: assign verification to a separate role.", ProposedBy: "owner"},
		{ID: "starter-language", Name: "plain language", Description: "Use plain language with the owner", Type: "preference", Scope: "owner", Source: "orca starter set", Body: "Why: the owner should understand decisions quickly. How to apply: explain results in direct, plain language.", ProposedBy: "owner"},
	}
}

// MemoryIndex renders the compact index shared with orchestration roles.
func MemoryIndex(entries []MemoryEntry) string {
	var b strings.Builder
	for i := range entries {
		m := &entries[i]
		fmt.Fprintf(&b, "- [%s] %s (%s/%s): %s\n", m.ID, m.Name, m.Type, m.Scope, strings.TrimSpace(m.Description))
	}
	return b.String()
}

// Lease represents exclusive controller ownership of a Run with a TTL heartbeat.
type Lease struct {
	AcquiredAt  time.Time `json:"acquired_at"`
	HeartbeatAt time.Time `json:"heartbeat_at"`
	ExpiresAt   time.Time `json:"expires_at"`
	RunID       string    `json:"run_id"`
	HolderID    string    `json:"holder_id"`
}

// Expired reports whether the lease has expired at now.
func (l Lease) Expired(now time.Time) bool {
	return !now.Before(l.ExpiresAt)
}

func (l Lease) Clone() Lease {
	return l
}

// Run holds the top-level run objective, plan reference, and controller lease.
type Run struct {
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
	Lease         *Lease    `json:"lease,omitempty"`
	ID            string    `json:"id"`
	Objective     string    `json:"objective"`
	PlanRef       string    `json:"plan_ref,omitempty"`
	DefaultRecipe string    `json:"default_recipe,omitempty"`
}

func (r Run) Clone() Run {
	out := r
	if r.Lease != nil {
		cp := r.Lease.Clone()
		out.Lease = &cp
	}
	return out
}

// Priority orders BacklogItem dispatch precedence.
type Priority string

const (
	PriorityP0 Priority = "p0"
	PriorityP1 Priority = "p1"
	PriorityP2 Priority = "p2"
	PriorityP3 Priority = "p3"
)

// Rank returns a numeric sort key where lower numbers sort higher.
func (p Priority) Rank() int {
	switch p {
	case PriorityP0:
		return 0
	case PriorityP1:
		return 1
	case PriorityP2, "":
		return 2
	case PriorityP3:
		return 3
	default:
		return 4
	}
}

func (p Priority) Valid() bool {
	switch p {
	case PriorityP0, PriorityP1, PriorityP2, PriorityP3:
		return true
	default:
		return false
	}
}

// BacklogStatus represents the lifecycle state of a BacklogItem.
type BacklogStatus string

const (
	BacklogOpen       BacklogStatus = "open"
	BacklogInProgress BacklogStatus = "in_progress"
	BacklogBlocked    BacklogStatus = "blocked"
	BacklogDone       BacklogStatus = "done"
	BacklogDropped    BacklogStatus = "dropped"
)

// Terminal reports whether s is a closed backlog status (done or dropped).
func (s BacklogStatus) Terminal() bool {
	return s == BacklogDone || s == BacklogDropped
}

func (s BacklogStatus) Valid() bool {
	switch s {
	case BacklogOpen, BacklogInProgress, BacklogBlocked, BacklogDone, BacklogDropped:
		return true
	default:
		return false
	}
}

// BacklogHistoryEntry records a single mutation to a BacklogItem.
type BacklogHistoryEntry struct {
	At     time.Time `json:"at"`
	Actor  string    `json:"actor"`
	Change string    `json:"change"`
}

// BacklogItem is a work item in a run's backlog.
type BacklogItem struct {
	CreatedAt   time.Time             `json:"created_at"`
	UpdatedAt   time.Time             `json:"updated_at"`
	ID          string                `json:"id"`
	RunID       string                `json:"run_id"`
	Title       string                `json:"title"`
	Description string                `json:"description,omitempty"`
	Acceptance  string                `json:"acceptance,omitempty"`
	Priority    Priority              `json:"priority"`
	Status      BacklogStatus         `json:"status"`
	LaneID      string                `json:"lane_id,omitempty"`
	RecipeName  string                `json:"recipe_name,omitempty"`
	DependsOn   []string              `json:"depends_on,omitempty"`
	Tags        []string              `json:"tags,omitempty"`
	History     []BacklogHistoryEntry `json:"history,omitempty"`
	Order       int                   `json:"order"`
}

func (b BacklogItem) Clone() BacklogItem {
	out := b
	out.DependsOn = slices.Clone(b.DependsOn)
	out.Tags = slices.Clone(b.Tags)
	out.History = slices.Clone(b.History)
	return out
}

// BacklogFilter constrains BacklogStore.List queries.
type BacklogFilter struct {
	RunID    string
	LaneID   string
	Statuses []BacklogStatus
	Limit    int
}

// LaneFilter constrains LaneStore.List queries.
type LaneFilter struct {
	RunID    string
	Statuses []LaneStatus
}

// DirectiveKind classifies a Directive.
type DirectiveKind string

const (
	DirectiveStanding    DirectiveKind = "standing"
	DirectiveInstruction DirectiveKind = "instruction"
	DirectiveRecipeEdit  DirectiveKind = "recipe_edit"
	DirectivePlanImport  DirectiveKind = "plan_import"
	DirectiveNote        DirectiveKind = "note"
)

// DirectiveSource identifies who authored a Directive.
type DirectiveSource string

const (
	DirectiveSourceOwner      DirectiveSource = "owner"
	DirectiveSourceDispatcher DirectiveSource = "dispatcher"
	DirectiveSourceConcierge  DirectiveSource = "concierge"
	DirectiveSourcePlanner    DirectiveSource = "planner"
)

// DirectiveStatus tracks whether a Directive is active or resolved.
type DirectiveStatus string

const (
	DirectiveActive  DirectiveStatus = "active"
	DirectiveHandled DirectiveStatus = "handled"
	DirectiveExpired DirectiveStatus = "expired"
	DirectiveRevoked DirectiveStatus = "revoked"
)

// Directive is a structured rule, instruction, plan import, or note with source and expiry.
type Directive struct {
	CreatedAt time.Time       `json:"created_at"`
	ExpiresAt *time.Time      `json:"expires_at,omitempty"`
	ID        string          `json:"id"`
	RunID     string          `json:"run_id"`
	Kind      DirectiveKind   `json:"kind"`
	Source    DirectiveSource `json:"source"`
	Scope     string          `json:"scope,omitempty"`
	Text      string          `json:"text"`
	Ref       string          `json:"ref,omitempty"`
	Status    DirectiveStatus `json:"status"`
}

// IsActive reports whether d is currently active at now.
func (d Directive) IsActive(now time.Time) bool {
	if d.Status != DirectiveActive {
		return false
	}
	if d.ExpiresAt != nil && !now.Before(*d.ExpiresAt) {
		return false
	}
	return true
}

func (d Directive) Clone() Directive {
	out := d
	if d.ExpiresAt != nil {
		t := *d.ExpiresAt
		out.ExpiresAt = &t
	}
	return out
}

// DirectiveFilter constrains DirectiveStore.List queries.
type DirectiveFilter struct {
	RunID      string
	Scope      string
	Kinds      []DirectiveKind
	ActiveOnly bool
}

// DecisionCategory distinguishes deterministic controller actions from LLM judgment actions.
type DecisionCategory string

const (
	DecisionAuto     DecisionCategory = "auto"
	DecisionJudgment DecisionCategory = "judgment"
)

// Decision is an append-only audit record of a controller or LLM tool action.
type Decision struct {
	CreatedAt    time.Time        `json:"created_at"`
	ID           string           `json:"id"`
	RunID        string           `json:"run_id"`
	Category     DecisionCategory `json:"category"`
	Actor        string           `json:"actor"`
	ThreadID     string           `json:"thread_id,omitempty"`
	TurnID       string           `json:"turn_id,omitempty"`
	LaneID       string           `json:"lane_id,omitempty"`
	Tool         string           `json:"tool"`
	Reason       string           `json:"reason,omitempty"`
	Result       string           `json:"result,omitempty"`
	Args         json.RawMessage  `json:"args,omitempty"`
	Seq          int64            `json:"seq"`
	InputTokens  int64            `json:"input_tokens,omitempty"`
	OutputTokens int64            `json:"output_tokens,omitempty"`
	CostUSD      float64          `json:"cost_usd,omitempty"`
	LatencyMs    int64            `json:"latency_ms,omitempty"`
	Vetted       bool             `json:"vetted"`
}

func (d Decision) Clone() Decision {
	out := d
	if d.Args != nil {
		out.Args = slices.Clone(d.Args)
	}
	return out
}

// DecisionFilter constrains DecisionStore.List queries.
type DecisionFilter struct {
	SinceTime *time.Time
	RunID     string
	LaneID    string
	Actor     string
	Category  DecisionCategory
	SinceSeq  int64
	Limit     int
}

// EventKind identifies the source type of an incoming orchestration event.
type EventKind string

const (
	EventLaneReport      EventKind = "lane_report"
	EventPhaseExit       EventKind = "phase_exit"
	EventCI              EventKind = "ci"
	EventMail            EventKind = "mail"
	EventTimer           EventKind = "timer"
	EventOwnerMessage    EventKind = "owner_msg"
	EventOwnerAskExpired EventKind = "owner_ask_expired"
	EventStall           EventKind = "stall"
)

// EventClass records whether an event was handled deterministically or requires LLM judgment.
type EventClass string

const (
	EventClassUnclassified EventClass = ""
	EventClassAuto         EventClass = "auto"
	EventClassJudgment     EventClass = "judgment"
)

// Event is an append-only inbox record with a monotonic per-run sequence number.
type Event struct {
	CreatedAt time.Time       `json:"created_at"`
	ID        string          `json:"id"`
	RunID     string          `json:"run_id"`
	Kind      EventKind       `json:"kind"`
	Class     EventClass      `json:"class,omitempty"`
	LaneID    string          `json:"lane_id,omitempty"`
	Source    string          `json:"source,omitempty"`
	Summary   string          `json:"summary,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	Seq       int64           `json:"seq"`
	Urgent    bool            `json:"urgent,omitempty"`
}

func (e Event) Clone() Event {
	out := e
	if e.Payload != nil {
		out.Payload = slices.Clone(e.Payload)
	}
	return out
}

// EventCursor tracks the highest processed Event.Seq for a given consumer on a run.
type EventCursor struct {
	UpdatedAt time.Time `json:"updated_at"`
	RunID     string    `json:"run_id"`
	Consumer  string    `json:"consumer"`
	Seq       int64     `json:"seq"`
}

// EventFilter constrains EventStore.List queries.
type EventFilter struct {
	RunID    string
	LaneID   string
	Kinds    []EventKind
	AfterSeq int64
	Limit    int
}

// ThreadRole identifies the orchestrator role hosted on a Thread.
type ThreadRole string

const (
	ThreadRoleDispatcher ThreadRole = "dispatcher"
	ThreadRoleConcierge  ThreadRole = "concierge"
	ThreadRolePlanner    ThreadRole = "planner"
)

// ThreadStatus represents the execution state of a Thread.
type ThreadStatus string

const (
	ThreadStatusIdle     ThreadStatus = "idle"
	ThreadStatusRunning  ThreadStatus = "running"
	ThreadStatusArchived ThreadStatus = "archived"
)

// TurnStatus represents the execution state of a Turn within a Thread.
type TurnStatus string

const (
	TurnStatusRunning     TurnStatus = "running"
	TurnStatusCompleted   TurnStatus = "completed"
	TurnStatusInterrupted TurnStatus = "interrupted"
	TurnStatusFailed      TurnStatus = "failed"
)

// ItemKind classifies an Item recorded inside a Turn.
type ItemKind string

const (
	ItemKindUserMessage      ItemKind = "user_message"
	ItemKindAssistantMessage ItemKind = "assistant_message"
	ItemKindToolCall         ItemKind = "tool_call"
	ItemKindToolResult       ItemKind = "tool_result"
	ItemKindSteer            ItemKind = "steer"
	ItemKindSnapshot         ItemKind = "snapshot"
)

// Thread is a persistent agent conversation/execution context (dispatcher, concierge, or planner).
type Thread struct {
	CreatedAt      time.Time    `json:"created_at"`
	UpdatedAt      time.Time    `json:"updated_at"`
	ID             string       `json:"id"`
	RunID          string       `json:"run_id"`
	Role           ThreadRole   `json:"role"`
	Goal           string       `json:"goal,omitempty"`
	ParentThreadID string       `json:"parent_thread_id,omitempty"`
	ForkedAtTurnID string       `json:"forked_at_turn_id,omitempty"`
	Status         ThreadStatus `json:"status"`
	QueuedSteers   []string     `json:"queued_steers,omitempty"`
	InputTokens    int64        `json:"input_tokens"`
	OutputTokens   int64        `json:"output_tokens"`
	CostUSD        float64      `json:"cost_usd"`
}

func (t Thread) Clone() Thread {
	out := t
	out.QueuedSteers = slices.Clone(t.QueuedSteers)
	return out
}

// Turn is one bounded execution turn on a Thread.
type Turn struct {
	StartedAt    time.Time  `json:"started_at"`
	CompletedAt  *time.Time `json:"completed_at,omitempty"`
	ID           string     `json:"id"`
	ThreadID     string     `json:"thread_id"`
	RunID        string     `json:"run_id"`
	Status       TurnStatus `json:"status"`
	Input        string     `json:"input,omitempty"`
	Error        string     `json:"error,omitempty"`
	Steers       []string   `json:"steers,omitempty"`
	Seq          int        `json:"seq"`
	InputTokens  int64      `json:"input_tokens"`
	OutputTokens int64      `json:"output_tokens"`
	CostUSD      float64    `json:"cost_usd"`
}

func (t Turn) Clone() Turn {
	out := t
	if t.CompletedAt != nil {
		cp := *t.CompletedAt
		out.CompletedAt = &cp
	}
	out.Steers = slices.Clone(t.Steers)
	return out
}

// Item is an individual message, tool call, tool result, steer, or snapshot within a Turn.
type Item struct {
	CreatedAt time.Time       `json:"created_at"`
	ID        string          `json:"id"`
	ThreadID  string          `json:"thread_id"`
	TurnID    string          `json:"turn_id"`
	Kind      ItemKind        `json:"kind"`
	Name      string          `json:"name,omitempty"`
	CallID    string          `json:"call_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
	Seq       int             `json:"seq"`
	IsError   bool            `json:"is_error,omitempty"`
}

func (i Item) Clone() Item {
	out := i
	if i.Payload != nil {
		out.Payload = slices.Clone(i.Payload)
	}
	return out
}

// ThreadFilter constrains ThreadStore.ListThreads queries.
type ThreadFilter struct {
	RunID    string
	Role     ThreadRole
	Statuses []ThreadStatus
}
