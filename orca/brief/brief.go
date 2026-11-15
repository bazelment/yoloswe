// Package brief adds durable run guidance to worker instructions.
package brief

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
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
	return RenderWithContract(text, nil, memories, maxBytes)
}

// SortContractDirectives sorts contract directives in deterministic CreatedAt
// (then ID) order in place.
func SortContractDirectives(directives []store.Directive) {
	slices.SortFunc(directives, func(a, b store.Directive) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
}

// FormatRunContract renders active contract directives into a shared Run Contract section.
func FormatRunContract(directives []store.Directive) string {
	var contractDirs []store.Directive
	for i := range directives {
		if directives[i].IsContract() {
			contractDirs = append(contractDirs, directives[i])
		}
	}
	SortContractDirectives(contractDirs)
	var lines []string
	seen := make(map[string]bool)
	for i := range contractDirs {
		d := &contractDirs[i]
		txt := strings.TrimSpace(d.Text)
		if txt == "" {
			continue
		}
		key := strings.TrimSpace(d.ID) + "\x00" + txt
		if seen[key] {
			continue
		}
		seen[key] = true
		if id := strings.TrimSpace(d.ID); id != "" {
			lines = append(lines, fmt.Sprintf("- [%s] %s", id, txt))
		} else {
			lines = append(lines, "- "+txt)
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "## Run Contract\n" + strings.Join(lines, "\n") + "\n"
}

// RenderWithContract includes active run-contract directives and applicable
// memories in the worker brief, capping the output to maxBytes when positive.
func RenderWithContract(text string, directives []store.Directive, memories []store.MemoryEntry, maxBytes int) (string, error) {
	if maxBytes > 0 && len(text) > maxBytes {
		return "", fmt.Errorf("%w: worker brief is %d bytes, exceeds cap %d", store.ErrInvalidArgument, len(text), maxBytes)
	}
	var b strings.Builder
	b.WriteString(text)
	contractSection := FormatRunContract(directives)
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		if contractSection != "" {
			b.WriteString("\n")
			b.WriteString(strings.TrimSuffix(contractSection, "\n"))
		}
		for i := range memories {
			if line := memoryLine(&memories[i]); line != "" {
				b.WriteString("\n")
				b.WriteString(line)
			}
		}
		if maxBytes > 0 && b.Len() > maxBytes {
			return "", fmt.Errorf("%w: rendered worker brief is %d bytes, exceeds cap %d", store.ErrInvalidArgument, b.Len(), maxBytes)
		}
		return b.String(), nil
	}
	if contractSection != "" && !strings.Contains(text, "## Run Contract") {
		block := "\n\n" + contractSection
		if maxBytes > 0 && b.Len()+len(block) > maxBytes {
			return "", fmt.Errorf("%w: run contract would exceed worker brief cap %d", store.ErrInvalidArgument, maxBytes)
		}
		b.WriteString(block)
	}
	// Mechanical cleanup and review phases receive their task only through the
	// goal line. Their phase skill is the complete instruction and must not be
	// followed by task descriptions or memory bodies.
	if strings.HasPrefix(trimmed, "/simplify") || strings.HasPrefix(trimmed, "/pr-polish") {
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
	if m.Scope == "owner" || m.Scope == "repo" || m.Scope == "contract" || m.Type == "contract" || strings.HasPrefix(m.Source, "contract:") {
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

func (d *memoryDriver) ReleaseSession(ctx context.Context, sessionID string, force ...bool) error {
	if releaser, ok := d.LaneDriver.(lane.SessionReleaser); ok {
		return releaser.ReleaseSession(ctx, sessionID, force...)
	}
	return nil
}

func (d *memoryDriver) ReportOnlyNoWork(ctx context.Context, ln lane.Lane) (bool, error) {
	if rc, ok := d.LaneDriver.(lane.ReportOnlyChecker); ok {
		return rc.ReportOnlyNoWork(ctx, ln)
	}
	return false, nil
}

func (d *memoryDriver) TerminalAgentState(ctx context.Context, terminalID string) (string, error) {
	if tr, ok := d.LaneDriver.(lane.TerminalStateReader); ok {
		return tr.TerminalAgentState(ctx, terminalID)
	}
	return "", nil
}

func (d *memoryDriver) LaneGitState(ctx context.Context, ln lane.Lane) (lane.GitState, error) {
	if gr, ok := d.LaneDriver.(lane.GitStateReader); ok {
		return gr.LaneGitState(ctx, ln)
	}
	return lane.GitState{}, nil
}

func (d *memoryDriver) LaneSessionHealth(ctx context.Context, ln lane.Lane) (lane.SessionHealth, error) {
	if hr, ok := d.LaneDriver.(lane.SessionHealthReader); ok {
		return hr.LaneSessionHealth(ctx, ln)
	}
	return lane.SessionHealth{}, nil
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
	directives, err := d.store.Directives().List(ctx, store.DirectiveFilter{RunID: runID, ActiveOnly: true})
	if err != nil {
		return "", err
	}
	return RenderWithContract(text, directives, memories, limit)
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
