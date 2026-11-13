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
	// Mechanical cleanup and review phases receive their task only through the
	// goal line. Their phase skill is the complete instruction and must not be
	// followed by task descriptions or memory bodies.
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "/simplify") || strings.HasPrefix(trimmed, "/pr-polish") {
		return text, nil
	}
	if strings.TrimSpace(text) == "" {
		for i := range memories {
			if line := memoryLine(&memories[i]); line != "" {
				b.WriteString("\n")
				b.WriteString(line)
			}
		}
		return b.String(), nil
	}
	var applicable []store.MemoryEntry
	for i := range memories {
		if applies(text, memories[i]) {
			applicable = append(applicable, memories[i])
		}
	}
	if len(applicable) > 0 {
		b.WriteString("\n\n## Memories for this task\n")
		for i := range applicable {
			entry := memoryLine(&applicable[i]) + "\n"
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

func memoryLine(m *store.MemoryEntry) string {
	if m == nil {
		return ""
	}
	description := strings.TrimSpace(m.Description)
	if description == "" {
		return fmt.Sprintf("- [%s] %s", m.ID, strings.TrimSpace(m.Name))
	}
	return fmt.Sprintf("- [%s] %s: %s", m.ID, strings.TrimSpace(m.Name), description)
}

func applies(text string, m store.MemoryEntry) bool {
	if m.Scope == "owner" || m.Scope == "repo" {
		return true
	}
	words := strings.Fields(strings.ToLower(text))
	hay := strings.ToLower(m.Name + " " + m.Description + " " + m.Body)
	for _, w := range words {
		w = strings.Trim(w, ".,:;()[]{}\"")
		if len(w) >= 4 && strings.Contains(hay, w) {
			return true
		}
	}
	return false
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
