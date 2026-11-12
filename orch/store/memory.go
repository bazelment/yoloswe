package store

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/orch/clock"
	"github.com/bazelment/yoloswe/orch/model"
)

// MemoryStore is a thread-safe, in-memory implementation of Store.
type MemoryStore struct {
	clock        clock.Clock
	runs         map[string]*Run
	backlog      map[string]map[string]*BacklogItem
	lanes        map[string]map[string]*Lane
	directives   map[string]map[string]*Directive
	decisions    map[string][]Decision
	decisionSeqs map[string]int64
	events       map[string][]Event
	eventSeqs    map[string]int64
	cursors      map[string]map[string]EventCursor
	threads      map[string]*Thread
	turns        map[string][]Turn
	items        map[string][]Item
	recipes      map[string]map[string][]Recipe
	mu           sync.RWMutex
}

// NewMemoryStore creates a new in-memory Store using clk for timestamps and
// expiry checks. If clk is nil, clock.RealClock{} is used.
func NewMemoryStore(clk clock.Clock) *MemoryStore {
	if clk == nil {
		clk = clock.RealClock{}
	}
	return &MemoryStore{
		clock:        clk,
		runs:         make(map[string]*Run),
		backlog:      make(map[string]map[string]*BacklogItem),
		lanes:        make(map[string]map[string]*Lane),
		directives:   make(map[string]map[string]*Directive),
		decisions:    make(map[string][]Decision),
		decisionSeqs: make(map[string]int64),
		events:       make(map[string][]Event),
		eventSeqs:    make(map[string]int64),
		cursors:      make(map[string]map[string]EventCursor),
		threads:      make(map[string]*Thread),
		turns:        make(map[string][]Turn),
		items:        make(map[string][]Item),
		recipes:      make(map[string]map[string][]Recipe),
	}
}

func (s *MemoryStore) now() time.Time {
	return s.clock.Now().UTC()
}

// Runs returns the RunStore view.
func (s *MemoryStore) Runs() RunStore { return (*memRunStore)(s) }

// Backlog returns the BacklogStore view.
func (s *MemoryStore) Backlog() BacklogStore { return (*memBacklogStore)(s) }

// Lanes returns the LaneStore view.
func (s *MemoryStore) Lanes() LaneStore { return (*memLaneStore)(s) }

// Directives returns the DirectiveStore view.
func (s *MemoryStore) Directives() DirectiveStore { return (*memDirectiveStore)(s) }

// Decisions returns the DecisionStore view.
func (s *MemoryStore) Decisions() DecisionStore { return (*memDecisionStore)(s) }

// Events returns the EventStore view.
func (s *MemoryStore) Events() EventStore { return (*memEventStore)(s) }

// Threads returns the ThreadStore view.
func (s *MemoryStore) Threads() ThreadStore { return (*memThreadStore)(s) }

// Recipes returns the RecipeStore view.
func (s *MemoryStore) Recipes() RecipeStore { return (*memRecipeStore)(s) }

// Close implements Store.
func (s *MemoryStore) Close() error { return nil }

var _ Store = (*MemoryStore)(nil)

// --- RunStore ---

type memRunStore MemoryStore

func (r *memRunStore) s() *MemoryStore { return (*MemoryStore)(r) }

func (r *memRunStore) Create(_ context.Context, run Run) error {
	s := r.s()
	if strings.TrimSpace(run.ID) == "" {
		return fmt.Errorf("%w: run id is required", ErrInvalidArgument)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.runs[run.ID]; exists {
		return fmt.Errorf("%w: run %q", ErrAlreadyExists, run.ID)
	}
	now := s.now()
	cp := run.Clone()
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = now
	}
	if cp.UpdatedAt.IsZero() {
		cp.UpdatedAt = cp.CreatedAt
	}
	if cp.DefaultRecipe == "" {
		cp.DefaultRecipe = "default"
	}
	s.runs[cp.ID] = &cp
	return nil
}

func (r *memRunStore) Get(_ context.Context, runID string) (*Run, error) {
	s := r.s()
	s.mu.RLock()
	defer s.mu.RUnlock()
	run, ok := s.runs[runID]
	if !ok {
		return nil, fmt.Errorf("%w: run %q", ErrNotFound, runID)
	}
	cp := run.Clone()
	return &cp, nil
}

func (r *memRunStore) UpdateObjective(_ context.Context, runID, objective, planRef string) error {
	s := r.s()
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[runID]
	if !ok {
		return fmt.Errorf("%w: run %q", ErrNotFound, runID)
	}
	run.Objective = objective
	run.PlanRef = planRef
	run.UpdatedAt = s.now()
	return nil
}

func (r *memRunStore) AcquireLease(_ context.Context, runID, holderID string, ttl time.Duration) (*Lease, error) {
	s := r.s()
	if strings.TrimSpace(holderID) == "" || ttl <= 0 {
		return nil, fmt.Errorf("%w: holderID and positive ttl are required", ErrInvalidArgument)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[runID]
	if !ok {
		return nil, fmt.Errorf("%w: run %q", ErrNotFound, runID)
	}
	now := s.now()
	if run.Lease != nil && !run.Lease.Expired(now) && run.Lease.HolderID != holderID {
		return nil, fmt.Errorf("%w: run %q held by %q until %s", ErrLeaseHeld, runID, run.Lease.HolderID, run.Lease.ExpiresAt.Format(time.RFC3339))
	}
	acquiredAt := now
	if run.Lease != nil && run.Lease.HolderID == holderID && !run.Lease.Expired(now) {
		acquiredAt = run.Lease.AcquiredAt
	}
	lease := Lease{
		RunID:       runID,
		HolderID:    holderID,
		AcquiredAt:  acquiredAt,
		HeartbeatAt: now,
		ExpiresAt:   now.Add(ttl),
	}
	run.Lease = &lease
	run.UpdatedAt = now
	cp := lease.Clone()
	return &cp, nil
}

func (r *memRunStore) HeartbeatLease(_ context.Context, runID, holderID string, ttl time.Duration) (*Lease, error) {
	s := r.s()
	if strings.TrimSpace(holderID) == "" || ttl <= 0 {
		return nil, fmt.Errorf("%w: holderID and positive ttl are required", ErrInvalidArgument)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[runID]
	if !ok {
		return nil, fmt.Errorf("%w: run %q", ErrNotFound, runID)
	}
	now := s.now()
	if run.Lease == nil || run.Lease.HolderID != holderID || run.Lease.Expired(now) {
		return nil, fmt.Errorf("%w: run %q by holder %q", ErrLeaseNotHeld, runID, holderID)
	}
	run.Lease.HeartbeatAt = now
	run.Lease.ExpiresAt = now.Add(ttl)
	run.UpdatedAt = now
	cp := run.Lease.Clone()
	return &cp, nil
}

func (r *memRunStore) ReleaseLease(_ context.Context, runID, holderID string) error {
	s := r.s()
	s.mu.Lock()
	defer s.mu.Unlock()
	run, ok := s.runs[runID]
	if !ok {
		return fmt.Errorf("%w: run %q", ErrNotFound, runID)
	}
	if run.Lease == nil || run.Lease.HolderID != holderID {
		return fmt.Errorf("%w: run %q by holder %q", ErrLeaseNotHeld, runID, holderID)
	}
	run.Lease = nil
	run.UpdatedAt = s.now()
	return nil
}

// --- BacklogStore ---

type memBacklogStore MemoryStore

func (b *memBacklogStore) s() *MemoryStore { return (*MemoryStore)(b) }

func (b *memBacklogStore) Add(_ context.Context, item BacklogItem) error {
	s := b.s()
	if strings.TrimSpace(item.RunID) == "" || strings.TrimSpace(item.ID) == "" {
		return fmt.Errorf("%w: backlog item requires run_id and id", ErrInvalidArgument)
	}
	if strings.TrimSpace(item.Title) == "" {
		return fmt.Errorf("%w: backlog item %q requires title", ErrInvalidArgument, item.ID)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	byRun := s.backlog[item.RunID]
	if byRun == nil {
		byRun = make(map[string]*BacklogItem)
		s.backlog[item.RunID] = byRun
	}
	if _, exists := byRun[item.ID]; exists {
		return fmt.Errorf("%w: backlog item %q in run %q", ErrAlreadyExists, item.ID, item.RunID)
	}
	now := s.now()
	cp := item.Clone()
	if cp.Priority == "" {
		cp.Priority = PriorityP2
	}
	if !cp.Priority.Valid() {
		return fmt.Errorf("%w: invalid priority %q", ErrInvalidArgument, cp.Priority)
	}
	if cp.Status == "" {
		cp.Status = BacklogOpen
	}
	if !cp.Status.Valid() {
		return fmt.Errorf("%w: invalid status %q", ErrInvalidArgument, cp.Status)
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = now
	}
	if cp.UpdatedAt.IsZero() {
		cp.UpdatedAt = cp.CreatedAt
	}
	if len(cp.History) == 0 {
		cp.History = []BacklogHistoryEntry{
			{At: cp.CreatedAt, Actor: "system", Change: "created"},
		}
	}
	byRun[cp.ID] = &cp
	return nil
}

func (b *memBacklogStore) Get(_ context.Context, runID, itemID string) (*BacklogItem, error) {
	s := b.s()
	s.mu.RLock()
	defer s.mu.RUnlock()
	item, ok := s.backlog[runID][itemID]
	if !ok {
		return nil, fmt.Errorf("%w: backlog item %q in run %q", ErrNotFound, itemID, runID)
	}
	cp := item.Clone()
	return &cp, nil
}

func (b *memBacklogStore) Update(_ context.Context, item BacklogItem, actor, reason string) error {
	s := b.s()
	if !item.Priority.Valid() || !item.Status.Valid() {
		return fmt.Errorf("%w: invalid priority or status", ErrInvalidArgument)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	byRun := s.backlog[item.RunID]
	existing, ok := byRun[item.ID]
	if !ok {
		return fmt.Errorf("%w: backlog item %q in run %q", ErrNotFound, item.ID, item.RunID)
	}
	now := s.now()
	cp := item.Clone()
	cp.CreatedAt = existing.CreatedAt
	cp.UpdatedAt = now
	history := slices.Clone(existing.History)
	history = append(history, BacklogHistoryEntry{
		At:     now,
		Actor:  actor,
		Change: reason,
	})
	cp.History = history
	byRun[cp.ID] = &cp
	return nil
}

func (b *memBacklogStore) Reorder(_ context.Context, runID string, itemIDs []string, actor string) error {
	s := b.s()
	s.mu.Lock()
	defer s.mu.Unlock()
	byRun := s.backlog[runID]
	for _, id := range itemIDs {
		if _, ok := byRun[id]; !ok {
			return fmt.Errorf("%w: backlog item %q in run %q", ErrNotFound, id, runID)
		}
	}
	now := s.now()
	for idx, id := range itemIDs {
		item := byRun[id]
		item.Order = idx + 1
		item.UpdatedAt = now
		item.History = append(slices.Clone(item.History), BacklogHistoryEntry{
			At:     now,
			Actor:  actor,
			Change: fmt.Sprintf("reordered to position %d", idx+1),
		})
	}
	return nil
}

func (b *memBacklogStore) Close(_ context.Context, runID, itemID string, status BacklogStatus, actor, reason string) error {
	if !status.Terminal() {
		return fmt.Errorf("%w: Close requires terminal status (done or dropped), got %q", ErrInvalidArgument, status)
	}
	s := b.s()
	s.mu.Lock()
	defer s.mu.Unlock()
	byRun := s.backlog[runID]
	item, ok := byRun[itemID]
	if !ok {
		return fmt.Errorf("%w: backlog item %q in run %q", ErrNotFound, itemID, runID)
	}
	now := s.now()
	item.Status = status
	item.UpdatedAt = now
	item.History = append(slices.Clone(item.History), BacklogHistoryEntry{
		At:     now,
		Actor:  actor,
		Change: fmt.Sprintf("closed (%s): %s", status, reason),
	})
	return nil
}

func (b *memBacklogStore) List(_ context.Context, filter BacklogFilter) ([]BacklogItem, error) {
	s := b.s()
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []BacklogItem
	for _, item := range s.backlog[filter.RunID] {
		if filter.LaneID != "" && item.LaneID != filter.LaneID {
			continue
		}
		if len(filter.Statuses) > 0 && !slices.Contains(filter.Statuses, item.Status) {
			continue
		}
		out = append(out, item.Clone())
	}
	sort.Slice(out, func(i, j int) bool {
		if ri, rj := out[i].Priority.Rank(), out[j].Priority.Rank(); ri != rj {
			return ri < rj
		}
		if out[i].Order != out[j].Order {
			return out[i].Order < out[j].Order
		}
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	if filter.Limit > 0 && len(out) > filter.Limit {
		out = out[:filter.Limit]
	}
	return out, nil
}

// --- LaneStore ---

type memLaneStore MemoryStore

func (l *memLaneStore) s() *MemoryStore { return (*MemoryStore)(l) }

func (l *memLaneStore) Upsert(_ context.Context, ln Lane) error {
	if err := ln.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	s := l.s()
	s.mu.Lock()
	defer s.mu.Unlock()
	byRun := s.lanes[ln.RunID]
	if byRun == nil {
		byRun = make(map[string]*Lane)
		s.lanes[ln.RunID] = byRun
	}
	now := s.now()
	cp := ln.Clone()
	if existing, ok := byRun[cp.ID]; ok {
		if cp.CreatedAt.IsZero() {
			cp.CreatedAt = existing.CreatedAt
		}
	} else if cp.CreatedAt.IsZero() {
		cp.CreatedAt = now
	}
	cp.UpdatedAt = now
	byRun[cp.ID] = &cp
	return nil
}

func (l *memLaneStore) Get(_ context.Context, runID, laneID string) (*Lane, error) {
	s := l.s()
	s.mu.RLock()
	defer s.mu.RUnlock()
	ln, ok := s.lanes[runID][laneID]
	if !ok {
		return nil, fmt.Errorf("%w: lane %q in run %q", ErrNotFound, laneID, runID)
	}
	cp := ln.Clone()
	return &cp, nil
}

func (l *memLaneStore) List(_ context.Context, filter LaneFilter) ([]Lane, error) {
	s := l.s()
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Lane
	for _, ln := range s.lanes[filter.RunID] {
		if len(filter.Statuses) > 0 && !slices.Contains(filter.Statuses, ln.Status) {
			continue
		}
		out = append(out, ln.Clone())
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (l *memLaneStore) Delete(_ context.Context, runID, laneID string) error {
	s := l.s()
	s.mu.Lock()
	defer s.mu.Unlock()
	byRun := s.lanes[runID]
	if _, ok := byRun[laneID]; !ok {
		return fmt.Errorf("%w: lane %q in run %q", ErrNotFound, laneID, runID)
	}
	delete(byRun, laneID)
	return nil
}

// --- DirectiveStore ---

type memDirectiveStore MemoryStore

func (d *memDirectiveStore) s() *MemoryStore { return (*MemoryStore)(d) }

func (d *memDirectiveStore) Add(_ context.Context, dir Directive) error {
	if strings.TrimSpace(dir.RunID) == "" || strings.TrimSpace(dir.ID) == "" {
		return fmt.Errorf("%w: directive requires run_id and id", ErrInvalidArgument)
	}
	if strings.TrimSpace(dir.Text) == "" && strings.TrimSpace(dir.Ref) == "" {
		return fmt.Errorf("%w: directive %q requires text or ref", ErrInvalidArgument, dir.ID)
	}
	s := d.s()
	s.mu.Lock()
	defer s.mu.Unlock()
	byRun := s.directives[dir.RunID]
	if byRun == nil {
		byRun = make(map[string]*Directive)
		s.directives[dir.RunID] = byRun
	}
	if _, exists := byRun[dir.ID]; exists {
		return fmt.Errorf("%w: directive %q in run %q", ErrAlreadyExists, dir.ID, dir.RunID)
	}
	cp := dir.Clone()
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = s.now()
	}
	if cp.Status == "" {
		cp.Status = DirectiveActive
	}
	if cp.Kind == "" {
		cp.Kind = DirectiveStanding
	}
	if cp.Source == "" {
		cp.Source = DirectiveSourceOwner
	}
	byRun[cp.ID] = &cp
	return nil
}

func (d *memDirectiveStore) Get(_ context.Context, runID, directiveID string) (*Directive, error) {
	s := d.s()
	s.mu.RLock()
	defer s.mu.RUnlock()
	dir, ok := s.directives[runID][directiveID]
	if !ok {
		return nil, fmt.Errorf("%w: directive %q in run %q", ErrNotFound, directiveID, runID)
	}
	cp := dir.Clone()
	return &cp, nil
}

func (d *memDirectiveStore) UpdateStatus(_ context.Context, runID, directiveID string, status DirectiveStatus) error {
	s := d.s()
	s.mu.Lock()
	defer s.mu.Unlock()
	byRun := s.directives[runID]
	dir, ok := byRun[directiveID]
	if !ok {
		return fmt.Errorf("%w: directive %q in run %q", ErrNotFound, directiveID, runID)
	}
	dir.Status = status
	return nil
}

func (d *memDirectiveStore) List(_ context.Context, filter DirectiveFilter) ([]Directive, error) {
	s := d.s()
	s.mu.RLock()
	defer s.mu.RUnlock()
	now := s.now()
	var out []Directive
	for _, dir := range s.directives[filter.RunID] {
		if filter.ActiveOnly && !dir.IsActive(now) {
			continue
		}
		if filter.Scope != "" && dir.Scope != filter.Scope {
			continue
		}
		if len(filter.Kinds) > 0 && !slices.Contains(filter.Kinds, dir.Kind) {
			continue
		}
		out = append(out, dir.Clone())
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// --- DecisionStore ---

type memDecisionStore MemoryStore

func (d *memDecisionStore) s() *MemoryStore { return (*MemoryStore)(d) }

func (d *memDecisionStore) Append(_ context.Context, dec Decision) (Decision, error) {
	if strings.TrimSpace(dec.RunID) == "" || strings.TrimSpace(dec.Tool) == "" {
		return Decision{}, fmt.Errorf("%w: decision requires run_id and tool", ErrInvalidArgument)
	}
	s := d.s()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.decisionSeqs[dec.RunID]++
	seq := s.decisionSeqs[dec.RunID]
	cp := dec.Clone()
	cp.Seq = seq
	if cp.ID == "" {
		cp.ID = fmt.Sprintf("dec-%d", seq)
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = s.now()
	}
	if cp.Category == "" {
		cp.Category = DecisionAuto
	}
	s.decisions[cp.RunID] = append(s.decisions[cp.RunID], cp)
	return cp.Clone(), nil
}

func (d *memDecisionStore) List(_ context.Context, filter DecisionFilter) ([]Decision, error) {
	s := d.s()
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Decision
	decs := s.decisions[filter.RunID]
	for i := range decs {
		dec := &decs[i]
		if dec.Seq <= filter.SinceSeq {
			continue
		}
		if filter.SinceTime != nil && dec.CreatedAt.Before(*filter.SinceTime) {
			continue
		}
		if filter.LaneID != "" && dec.LaneID != filter.LaneID {
			continue
		}
		if filter.Actor != "" && dec.Actor != filter.Actor {
			continue
		}
		if filter.Category != "" && dec.Category != filter.Category {
			continue
		}
		out = append(out, dec.Clone())
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	return out, nil
}

// --- EventStore ---

type memEventStore MemoryStore

func (e *memEventStore) s() *MemoryStore { return (*MemoryStore)(e) }

func (e *memEventStore) Append(_ context.Context, ev Event) (Event, error) {
	if strings.TrimSpace(ev.RunID) == "" || strings.TrimSpace(string(ev.Kind)) == "" {
		return Event{}, fmt.Errorf("%w: event requires run_id and kind", ErrInvalidArgument)
	}
	s := e.s()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.eventSeqs[ev.RunID]++
	seq := s.eventSeqs[ev.RunID]
	cp := ev.Clone()
	cp.Seq = seq
	if cp.ID == "" {
		cp.ID = fmt.Sprintf("ev-%d", seq)
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = s.now()
	}
	s.events[cp.RunID] = append(s.events[cp.RunID], cp)
	return cp.Clone(), nil
}

func (e *memEventStore) List(_ context.Context, filter EventFilter) ([]Event, error) {
	s := e.s()
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Event
	evs := s.events[filter.RunID]
	for i := range evs {
		ev := &evs[i]
		if ev.Seq <= filter.AfterSeq {
			continue
		}
		if filter.LaneID != "" && ev.LaneID != filter.LaneID {
			continue
		}
		if len(filter.Kinds) > 0 && !slices.Contains(filter.Kinds, ev.Kind) {
			continue
		}
		out = append(out, ev.Clone())
		if filter.Limit > 0 && len(out) >= filter.Limit {
			break
		}
	}
	return out, nil
}

func (e *memEventStore) GetCursor(_ context.Context, runID, consumer string) (EventCursor, error) {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(consumer) == "" {
		return EventCursor{}, fmt.Errorf("%w: runID and consumer are required", ErrInvalidArgument)
	}
	s := e.s()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if cur, ok := s.cursors[runID][consumer]; ok {
		return cur, nil
	}
	return EventCursor{
		RunID:    runID,
		Consumer: consumer,
		Seq:      0,
	}, nil
}

func (e *memEventStore) SetCursor(_ context.Context, runID, consumer string, seq int64) error {
	if strings.TrimSpace(runID) == "" || strings.TrimSpace(consumer) == "" || seq < 0 {
		return fmt.Errorf("%w: valid runID, consumer, and non-negative seq are required", ErrInvalidArgument)
	}
	s := e.s()
	s.mu.Lock()
	defer s.mu.Unlock()
	byRun := s.cursors[runID]
	if byRun == nil {
		byRun = make(map[string]EventCursor)
		s.cursors[runID] = byRun
	}
	byRun[consumer] = EventCursor{
		RunID:     runID,
		Consumer:  consumer,
		Seq:       seq,
		UpdatedAt: s.now(),
	}
	return nil
}

// --- ThreadStore ---

type memThreadStore MemoryStore

func (t *memThreadStore) s() *MemoryStore { return (*MemoryStore)(t) }

func (t *memThreadStore) StartThread(_ context.Context, th Thread) (Thread, error) {
	if strings.TrimSpace(th.ID) == "" || strings.TrimSpace(th.RunID) == "" || strings.TrimSpace(string(th.Role)) == "" {
		return Thread{}, fmt.Errorf("%w: thread requires id, run_id, and role", ErrInvalidArgument)
	}
	s := t.s()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.threads[th.ID]; exists {
		return Thread{}, fmt.Errorf("%w: thread %q", ErrAlreadyExists, th.ID)
	}
	now := s.now()
	cp := th.Clone()
	if cp.Status == "" {
		cp.Status = ThreadStatusIdle
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = now
	}
	if cp.UpdatedAt.IsZero() {
		cp.UpdatedAt = cp.CreatedAt
	}
	s.threads[cp.ID] = &cp
	return cp.Clone(), nil
}

func (t *memThreadStore) GetThread(_ context.Context, threadID string) (*Thread, error) {
	s := t.s()
	s.mu.RLock()
	defer s.mu.RUnlock()
	th, ok := s.threads[threadID]
	if !ok {
		return nil, fmt.Errorf("%w: thread %q", ErrNotFound, threadID)
	}
	cp := th.Clone()
	return &cp, nil
}

func (t *memThreadStore) ListThreads(_ context.Context, filter ThreadFilter) ([]Thread, error) {
	s := t.s()
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Thread
	for _, th := range s.threads {
		if filter.RunID != "" && th.RunID != filter.RunID {
			continue
		}
		if filter.Role != "" && th.Role != filter.Role {
			continue
		}
		if len(filter.Statuses) > 0 && !slices.Contains(filter.Statuses, th.Status) {
			continue
		}
		out = append(out, th.Clone())
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

func (t *memThreadStore) UpdateThreadStatus(_ context.Context, threadID string, status ThreadStatus) error {
	s := t.s()
	s.mu.Lock()
	defer s.mu.Unlock()
	th, ok := s.threads[threadID]
	if !ok {
		return fmt.Errorf("%w: thread %q", ErrNotFound, threadID)
	}
	th.Status = status
	th.UpdatedAt = s.now()
	return nil
}

func (t *memThreadStore) SetThreadGoal(_ context.Context, threadID, goal string) error {
	s := t.s()
	s.mu.Lock()
	defer s.mu.Unlock()
	th, ok := s.threads[threadID]
	if !ok {
		return fmt.Errorf("%w: thread %q", ErrNotFound, threadID)
	}
	th.Goal = goal
	th.UpdatedAt = s.now()
	return nil
}

func (t *memThreadStore) ForkThread(_ context.Context, parentThreadID, newThreadID, upToTurnID string) (Thread, error) {
	if strings.TrimSpace(newThreadID) == "" {
		return Thread{}, fmt.Errorf("%w: newThreadID is required", ErrInvalidArgument)
	}
	s := t.s()
	s.mu.Lock()
	defer s.mu.Unlock()
	parent, ok := s.threads[parentThreadID]
	if !ok {
		return Thread{}, fmt.Errorf("%w: parent thread %q", ErrNotFound, parentThreadID)
	}
	if _, exists := s.threads[newThreadID]; exists {
		return Thread{}, fmt.Errorf("%w: thread %q", ErrAlreadyExists, newThreadID)
	}
	now := s.now()
	forked := Thread{
		ID:             newThreadID,
		RunID:          parent.RunID,
		Role:           parent.Role,
		Goal:           parent.Goal,
		ParentThreadID: parentThreadID,
		ForkedAtTurnID: upToTurnID,
		Status:         ThreadStatusIdle,
		CreatedAt:      now,
		UpdatedAt:      now,
	}

	includedTurns := make(map[string]bool)
	var copiedTurns []Turn
	foundUpTo := upToTurnID == ""
	parentTurns := s.turns[parentThreadID]
	for i := range parentTurns {
		tr := &parentTurns[i]
		cpTurn := tr.Clone()
		cpTurn.ThreadID = newThreadID
		copiedTurns = append(copiedTurns, cpTurn)
		includedTurns[tr.ID] = true
		if upToTurnID != "" && tr.ID == upToTurnID {
			foundUpTo = true
			break
		}
	}
	if !foundUpTo {
		return Thread{}, fmt.Errorf("%w: turn %q in parent thread %q", ErrNotFound, upToTurnID, parentThreadID)
	}

	var copiedItems []Item
	parentItems := s.items[parentThreadID]
	for i := range parentItems {
		it := &parentItems[i]
		if !includedTurns[it.TurnID] {
			continue
		}
		cpItem := it.Clone()
		cpItem.ThreadID = newThreadID
		copiedItems = append(copiedItems, cpItem)
	}

	s.threads[newThreadID] = &forked
	s.turns[newThreadID] = copiedTurns
	s.items[newThreadID] = copiedItems
	return forked.Clone(), nil
}

func (t *memThreadStore) StartTurn(_ context.Context, turn Turn) (Turn, error) {
	if strings.TrimSpace(turn.ID) == "" || strings.TrimSpace(turn.ThreadID) == "" {
		return Turn{}, fmt.Errorf("%w: turn requires id and thread_id", ErrInvalidArgument)
	}
	s := t.s()
	s.mu.Lock()
	defer s.mu.Unlock()
	th, ok := s.threads[turn.ThreadID]
	if !ok {
		return Turn{}, fmt.Errorf("%w: thread %q", ErrNotFound, turn.ThreadID)
	}
	existingTurns := s.turns[turn.ThreadID]
	for i := range existingTurns {
		if existingTurns[i].ID == turn.ID {
			return Turn{}, fmt.Errorf("%w: turn %q in thread %q", ErrAlreadyExists, turn.ID, turn.ThreadID)
		}
	}
	now := s.now()
	cp := turn.Clone()
	cp.RunID = th.RunID
	cp.Seq = len(existingTurns) + 1
	cp.Status = TurnStatusRunning
	if cp.StartedAt.IsZero() {
		cp.StartedAt = now
	}
	if len(th.QueuedSteers) > 0 {
		cp.Steers = append(slices.Clone(th.QueuedSteers), cp.Steers...)
		th.QueuedSteers = nil
	}
	th.Status = ThreadStatusRunning
	th.UpdatedAt = now
	s.turns[turn.ThreadID] = append(existingTurns, cp)
	return cp.Clone(), nil
}

func (t *memThreadStore) SteerTurn(_ context.Context, threadID, turnID, message string) error {
	if strings.TrimSpace(message) == "" {
		return fmt.Errorf("%w: steer message is required", ErrInvalidArgument)
	}
	s := t.s()
	s.mu.Lock()
	defer s.mu.Unlock()
	th, ok := s.threads[threadID]
	if !ok {
		return fmt.Errorf("%w: thread %q", ErrNotFound, threadID)
	}
	now := s.now()
	turns := s.turns[threadID]
	if turnID != "" {
		for i := range turns {
			if turns[i].ID == turnID {
				if turns[i].Status == TurnStatusRunning {
					turns[i].Steers = append(turns[i].Steers, message)
					s.turns[threadID] = turns
					seq := len(s.items[threadID]) + 1
					s.items[threadID] = append(s.items[threadID], Item{
						ID:        fmt.Sprintf("item-%d", seq),
						ThreadID:  threadID,
						TurnID:    turnID,
						Seq:       seq,
						Kind:      ItemKindSteer,
						Content:   message,
						CreatedAt: now,
					})
					th.UpdatedAt = now
					return nil
				}
				break
			}
		}
	}
	th.QueuedSteers = append(th.QueuedSteers, message)
	th.UpdatedAt = now
	return nil
}

func (t *memThreadStore) CompleteTurn(_ context.Context, threadID, turnID string, status TurnStatus, usage model.Usage, errMsg string) error {
	if status == TurnStatusRunning || status == "" {
		return fmt.Errorf("%w: CompleteTurn requires a non-running status", ErrInvalidArgument)
	}
	s := t.s()
	s.mu.Lock()
	defer s.mu.Unlock()
	th, ok := s.threads[threadID]
	if !ok {
		return fmt.Errorf("%w: thread %q", ErrNotFound, threadID)
	}
	turns := s.turns[threadID]
	idx := -1
	for i := range turns {
		if turns[i].ID == turnID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("%w: turn %q in thread %q", ErrNotFound, turnID, threadID)
	}
	now := s.now()
	turns[idx].Status = status
	turns[idx].CompletedAt = &now
	turns[idx].Error = errMsg
	turns[idx].InputTokens += usage.InputTokens
	turns[idx].OutputTokens += usage.OutputTokens
	turns[idx].CostUSD += usage.CostUSD
	s.turns[threadID] = turns

	th.InputTokens += usage.InputTokens
	th.OutputTokens += usage.OutputTokens
	th.CostUSD += usage.CostUSD
	th.Status = ThreadStatusIdle
	th.UpdatedAt = now
	return nil
}

func (t *memThreadStore) GetTurn(_ context.Context, threadID, turnID string) (*Turn, error) {
	s := t.s()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.threads[threadID]; !ok {
		return nil, fmt.Errorf("%w: thread %q", ErrNotFound, threadID)
	}
	turns := s.turns[threadID]
	for i := range turns {
		if turns[i].ID == turnID {
			cp := turns[i].Clone()
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("%w: turn %q in thread %q", ErrNotFound, turnID, threadID)
}

func (t *memThreadStore) ListTurns(_ context.Context, threadID string) ([]Turn, error) {
	s := t.s()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.threads[threadID]; !ok {
		return nil, fmt.Errorf("%w: thread %q", ErrNotFound, threadID)
	}
	turns := s.turns[threadID]
	out := make([]Turn, len(turns))
	for i := range turns {
		out[i] = turns[i].Clone()
	}
	return out, nil
}

func (t *memThreadStore) AppendItem(_ context.Context, item Item) (Item, error) {
	if strings.TrimSpace(item.ThreadID) == "" || strings.TrimSpace(item.TurnID) == "" || strings.TrimSpace(string(item.Kind)) == "" {
		return Item{}, fmt.Errorf("%w: item requires thread_id, turn_id, and kind", ErrInvalidArgument)
	}
	s := t.s()
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.threads[item.ThreadID]; !ok {
		return Item{}, fmt.Errorf("%w: thread %q", ErrNotFound, item.ThreadID)
	}
	turnFound := false
	turns := s.turns[item.ThreadID]
	for i := range turns {
		if turns[i].ID == item.TurnID {
			turnFound = true
			break
		}
	}
	if !turnFound {
		return Item{}, fmt.Errorf("%w: turn %q in thread %q", ErrNotFound, item.TurnID, item.ThreadID)
	}
	seq := len(s.items[item.ThreadID]) + 1
	cp := item.Clone()
	cp.Seq = seq
	if cp.ID == "" {
		cp.ID = fmt.Sprintf("item-%d", seq)
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = s.now()
	}
	s.items[item.ThreadID] = append(s.items[item.ThreadID], cp)
	return cp.Clone(), nil
}

func (t *memThreadStore) ListItems(_ context.Context, threadID, turnID string, limit, offset int) ([]Item, error) {
	s := t.s()
	s.mu.RLock()
	defer s.mu.RUnlock()
	if _, ok := s.threads[threadID]; !ok {
		return nil, fmt.Errorf("%w: thread %q", ErrNotFound, threadID)
	}
	var filtered []Item
	items := s.items[threadID]
	for i := range items {
		it := &items[i]
		if turnID != "" && it.TurnID != turnID {
			continue
		}
		filtered = append(filtered, it.Clone())
	}
	if offset > 0 {
		if offset >= len(filtered) {
			return nil, nil
		}
		filtered = filtered[offset:]
	}
	if limit > 0 && len(filtered) > limit {
		filtered = filtered[:limit]
	}
	return filtered, nil
}

// --- RecipeStore ---

type memRecipeStore MemoryStore

func (r *memRecipeStore) s() *MemoryStore { return (*MemoryStore)(r) }

func (r *memRecipeStore) Save(_ context.Context, rec Recipe) (Recipe, error) {
	if strings.TrimSpace(rec.RunID) == "" {
		return Recipe{}, fmt.Errorf("%w: recipe run_id is required", ErrInvalidArgument)
	}
	if err := rec.Validate(); err != nil {
		return Recipe{}, fmt.Errorf("%w: %v", ErrInvalidArgument, err)
	}
	s := r.s()
	s.mu.Lock()
	defer s.mu.Unlock()
	byRun := s.recipes[rec.RunID]
	if byRun == nil {
		byRun = make(map[string][]Recipe)
		s.recipes[rec.RunID] = byRun
	}
	versions := byRun[rec.Name]
	cp := rec.Clone()
	if cp.Version == 0 {
		maxV := 0
		for i := range versions {
			if versions[i].Version > maxV {
				maxV = versions[i].Version
			}
		}
		cp.Version = maxV + 1
	} else {
		for i := range versions {
			if versions[i].Version == cp.Version {
				return Recipe{}, fmt.Errorf("%w: recipe %q v%d in run %q", ErrAlreadyExists, cp.Name, cp.Version, cp.RunID)
			}
		}
	}
	if cp.CreatedAt.IsZero() {
		cp.CreatedAt = s.now()
	}
	versions = append(versions, cp)
	sort.Slice(versions, func(i, j int) bool {
		return versions[i].Version < versions[j].Version
	})
	byRun[cp.Name] = versions
	return cp.Clone(), nil
}

func (r *memRecipeStore) Get(_ context.Context, runID, name string, version int) (*Recipe, error) {
	s := r.s()
	s.mu.RLock()
	defer s.mu.RUnlock()
	versions := s.recipes[runID][name]
	if len(versions) == 0 {
		return nil, fmt.Errorf("%w: recipe %q in run %q", ErrNotFound, name, runID)
	}
	if version <= 0 {
		cp := versions[len(versions)-1].Clone()
		return &cp, nil
	}
	for i := range versions {
		if versions[i].Version == version {
			cp := versions[i].Clone()
			return &cp, nil
		}
	}
	return nil, fmt.Errorf("%w: recipe %q v%d in run %q", ErrNotFound, name, version, runID)
}

func (r *memRecipeStore) ListVersions(_ context.Context, runID, name string) ([]Recipe, error) {
	s := r.s()
	s.mu.RLock()
	defer s.mu.RUnlock()
	versions := s.recipes[runID][name]
	out := make([]Recipe, len(versions))
	for i := range versions {
		out[i] = versions[i].Clone()
	}
	return out, nil
}

func (r *memRecipeStore) ListLatest(_ context.Context, runID string) ([]Recipe, error) {
	s := r.s()
	s.mu.RLock()
	defer s.mu.RUnlock()
	byRun := s.recipes[runID]
	var out []Recipe
	for _, versions := range byRun {
		if len(versions) > 0 {
			out = append(out, versions[len(versions)-1].Clone())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Name < out[j].Name
	})
	return out, nil
}
