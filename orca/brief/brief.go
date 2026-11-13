// Package brief adds durable run guidance to worker instructions.
package brief

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/recipe"
	"github.com/bazelment/yoloswe/orca/store"
)

const DefaultMaxBytes = 16 * 1024

// Render includes all supplied memories and rejects briefs that exceed maxBytes.
// Callers must filter out memories belonging to other runs.
func Render(text string, memories []store.MemoryEntry, maxBytes int) (string, error) {
	if maxBytes > 0 && len(text) > maxBytes {
		return "", fmt.Errorf("%w: worker brief is %d bytes, exceeds cap %d", store.ErrInvalidArgument, len(text), maxBytes)
	}
	var b strings.Builder
	b.WriteString(text)
	if len(memories) > 0 {
		b.WriteString("\n\n## Memories for this task\n")
		b.WriteString(store.MemoryIndex(memories))
		for i := range memories {
			m := &memories[i]
			entry := fmt.Sprintf("\n### [%s] %s\n%s\nSource: %s\n", m.ID, m.Name, m.Body, m.Source)
			if maxBytes > 0 && b.Len()+len(entry) > maxBytes {
				break
			}
			b.WriteString(entry)
		}
	}
	if maxBytes > 0 && b.Len() > maxBytes {
		out := b.String()
		for len(out) > maxBytes && !utf8.ValidString(out[:maxBytes]) {
			maxBytes--
		}
		return out[:maxBytes], nil
	}
	return b.String(), nil
}

// WithMemories decorates the controller's driver so every fresh phase gets
// current memories without changing the controller's state machine.
func WithMemories(st store.Store, driver lane.LaneDriver) lane.LaneDriver {
	if driver == nil {
		return nil
	}
	return &memoryDriver{LaneDriver: driver, store: st}
}

type memoryDriver struct {
	lane.LaneDriver
	store store.Store
}

func (d *memoryDriver) render(ctx context.Context, runID, laneID, text string) (string, error) {
	memories, err := d.store.Memories().List(ctx, store.MemoryFilter{RunID: runID})
	if err != nil {
		return "", err
	}
	run, err := d.store.Runs().Get(ctx, runID)
	if err != nil {
		return "", err
	}
	name, version := run.DefaultRecipe, 0
	ln, err := d.store.Lanes().Get(ctx, runID, laneID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return "", err
	}
	if ln != nil && ln.RecipeName != "" {
		name, version = ln.RecipeName, ln.RecipeVersion
	}
	if name == "" {
		name = recipe.DefaultName
	}
	r, err := d.store.Recipes().Get(ctx, runID, name, version)
	if errors.Is(err, store.ErrNotFound) && name == recipe.DefaultName {
		fallback := recipe.Default()
		r, err = &fallback, nil
	}
	if err != nil {
		return "", err
	}
	limit := r.Limits.MaxBriefBytes
	if limit <= 0 {
		limit = DefaultMaxBytes
	}
	return Render(text, memories, limit)
}

func (d *memoryDriver) SpawnSession(ctx context.Context, req lane.SpawnRequest) (lane.SpawnResult, error) {
	text, err := d.render(ctx, req.RunID, req.LaneID, req.Brief)
	if err != nil {
		return lane.SpawnResult{}, err
	}
	req.Brief = text
	return d.LaneDriver.SpawnSession(ctx, req)
}

func (d *memoryDriver) StartPhaseTerminal(ctx context.Context, req lane.StartPhaseRequest) (lane.StartPhaseResult, error) {
	text, err := d.render(ctx, req.RunID, req.LaneID, req.Brief)
	if err != nil {
		return lane.StartPhaseResult{}, err
	}
	req.Brief = text
	return d.LaneDriver.StartPhaseTerminal(ctx, req)
}
