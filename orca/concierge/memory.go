package concierge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"

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
	saved, err := st.Memories().Put(ctx, m)
	if err != nil {
		return "", err
	}
	e.last, e.previous, e.directive = &saved, previous, directive
	return "Saved memory " + id + ": " + strings.Join(strings.Fields(m.Name), " ") + "; undo or forget to revert.", nil
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
