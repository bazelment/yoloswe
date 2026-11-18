package adopt

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/orca/store"
)

// WriteShadowReports writes the human readable shadow summary and host diff.
func WriteShadowReports(ctx context.Context, outDir, runDir, runID string, st store.Store, started, ended time.Time) error {
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	actionPath := filepath.Join(outDir, "actions.jsonl")
	total := 0
	counts := map[string]int{}
	lanes := map[string]int{}
	refused := map[string]int{}
	degraded := map[string]string{}
	if err := walkShadowActions(actionPath, func(a Action) error {
		total++
		counts[a.Kind]++
		lanes[a.Lane]++
		if a.Kind != "http_refused" && a.Kind != "read_degraded" {
			return nil
		}
		var args struct {
			Method    string `json:"method"`
			PathClass string `json:"path_class"`
			Kind      string `json:"kind"`
		}
		if err := json.Unmarshal(a.Arguments, &args); err != nil {
			return fmt.Errorf("decode shadow action: %w", err)
		}
		if a.Kind == "http_refused" {
			refused[args.Method+" "+args.PathClass]++
		}
		if a.Kind == "read_degraded" {
			degraded[args.Kind] = a.Reason
		}
		return nil
	}); err != nil {
		return err
	}
	decs, err := st.Decisions().List(ctx, store.DecisionFilter{RunID: runID})
	if err != nil {
		return fmt.Errorf("list shadow decisions: %w", err)
	}
	var spend float64
	var judgments []store.Decision
	for i := range decs {
		spend += decs[i].CostUSD
		if decs[i].Category == store.DecisionJudgment || decs[i].Tool == "judgment.dispatch" {
			judgments = append(judgments, decs[i])
		}
	}
	sort.Slice(judgments, func(i, j int) bool { return judgments[i].CreatedAt.Before(judgments[j].CreatedAt) })
	if err := writeShadowReport(filepath.Join(outDir, "summary.md"), func(b *bufio.Writer) error {
		fmt.Fprintf(b, "# Shadow adopt summary\n\nRun: `%s`\nWindow: %s – %s\nToken spend: $%.4f\nActions: %d\n\n## Counts by action\n", runID, started.UTC().Format(time.RFC3339), ended.UTC().Format(time.RFC3339), spend, total)
		for _, pair := range sortedCounts(counts) {
			fmt.Fprintf(b, "- %s: %d\n", pair[0], pair[1])
		}
		b.WriteString("\n## Refused HTTP writes (method and path class)\n")
		if len(refused) == 0 {
			b.WriteString("None (0).\n")
		}
		for _, pair := range sortedCounts(refused) {
			fmt.Fprintf(b, "- %s: %d\n", pair[0], pair[1])
		}
		b.WriteString("\n## Degraded decisions\n")
		keys := make([]string, 0, len(degraded))
		for k := range degraded {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(b, "- %s: %s\n", k, degraded[k])
		}
		b.WriteString("Recorder-launched sessions remain shadow-launched, with no report; their reads never reach Nebula. Session settling/wake is unavailable. Terminal lifecycle GETs and existing GET/local reports remain available.\n")
		b.WriteString("\n## Counts by lane\n")
		for _, pair := range sortedCounts(lanes) {
			k := fmt.Sprint(pair[0])
			if k == "" {
				k = "(run)"
			}
			fmt.Fprintf(b, "- %s: %d\n", k, pair[1])
		}
		b.WriteString("\n## Would do\n")
		if err := walkShadowActions(actionPath, func(a Action) error {
			fmt.Fprintf(b, "- %s `%s` lane=%s args=%s", a.At.UTC().Format(time.RFC3339), a.Kind, a.Lane, string(a.Arguments))
			if a.Reason != "" {
				fmt.Fprintf(b, " — %s", a.Reason)
			}
			b.WriteByte('\n')
			return nil
		}); err != nil {
			return err
		}
		b.WriteString("\n## Dispatcher judgments\n")
		for i := range judgments {
			d := &judgments[i]
			fmt.Fprintf(b, "- %s %s: %s", d.CreatedAt.UTC().Format(time.RFC3339), d.Tool, strings.TrimSpace(d.Reason))
			if d.Result != "" {
				fmt.Fprintf(b, " (%s)", strings.TrimSpace(d.Result))
			}
			b.WriteByte('\n')
		}
		b.WriteString("\n## Timeline\n")
		return walkShadowActions(actionPath, func(a Action) error {
			_, err := fmt.Fprintf(b, "- %s %s/%s\n", a.At.UTC().Format(time.RFC3339), a.Lane, a.Kind)
			return err
		})
	}); err != nil {
		return err
	}
	return writeHostDiff(outDir, runDir, actionPath, started, ended)
}

func sortedCounts(m map[string]int) [][2]any {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([][2]any, 0, len(keys))
	for _, k := range keys {
		out = append(out, [2]any{k, m[k]})
	}
	return out
}
func walkShadowActions(path string, visit func(Action) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var a Action
		if err := json.Unmarshal(scanner.Bytes(), &a); err != nil {
			return fmt.Errorf("decode shadow action: %w", err)
		}
		if err := visit(a); err != nil {
			return err
		}
	}
	return scanner.Err()
}

func writeShadowReport(path string, render func(*bufio.Writer) error) (retErr error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		if err := f.Close(); retErr == nil {
			retErr = err
		}
	}()
	b := bufio.NewWriter(f)
	if err := render(b); err != nil {
		return err
	}
	return b.Flush()
}

func writeHostDiff(outDir, runDir, actionPath string, started, ended time.Time) error {
	return writeShadowReport(filepath.Join(outDir, "host-diff.md"), func(b *bufio.Writer) error {
		b.WriteString("# Host diff\n\n")
		b.WriteString("## Orca shadow actions by lane\n")
		if err := walkShadowActions(actionPath, func(a Action) error {
			_, err := fmt.Fprintf(b, "- %s `%s` lane=%s\n", a.At.UTC().Format(time.RFC3339), a.Kind, a.Lane)
			return err
		}); err != nil {
			return err
		}
		b.WriteString("\n## Host EVENTS rows\n")
		f, err := os.Open(filepath.Join(runDir, "EVENTS"))
		if err == nil {
			defer f.Close()
			s := bufio.NewScanner(f)
			rows := 0
			for s.Scan() {
				row := s.Text()
				fields := strings.Fields(row)
				if len(fields) == 0 {
					continue
				}
				t, parseErr := time.Parse(time.RFC3339Nano, fields[0])
				if parseErr != nil {
					continue
				}
				if t.Before(started) || t.After(ended) {
					continue
				}
				b.WriteString("- " + row + "\n")
				rows++
			}
			if err := s.Err(); err != nil {
				return fmt.Errorf("read host EVENTS: %w", err)
			}
			if rows == 0 {
				b.WriteString("(no host EVENTS rows in shadow window)\n")
			}
		} else if !os.IsNotExist(err) {
			return err
		} else {
			b.WriteString("(EVENTS unavailable)\n")
		}
		return nil
	})
}
