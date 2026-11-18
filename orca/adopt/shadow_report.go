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

	"github.com/bazelment/yoloswe/orca/model"
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
	usage, err := shadowUsage(ctx, st, runID, decs)
	if err != nil {
		return err
	}
	var judgments []store.Decision
	for i := range decs {
		if decs[i].Category == store.DecisionJudgment || decs[i].Tool == "judgment.dispatch" {
			judgments = append(judgments, decs[i])
		}
	}
	sort.Slice(judgments, func(i, j int) bool { return judgments[i].CreatedAt.Before(judgments[j].CreatedAt) })
	if err := writeShadowReport(filepath.Join(outDir, "summary.md"), func(b *bufio.Writer) error {
		fmt.Fprintf(b, "# Shadow adopt summary\n\nRun: `%s`\nWindow: %s – %s\nEstimated tokens: %d input, %d output\nEstimated cost: $%.4f\nActions: %d\n\n## Counts by action\n", runID, started.UTC().Format(time.RFC3339), ended.UTC().Format(time.RFC3339), usage.InputTokens, usage.OutputTokens, usage.CostUSD, total)
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

// HostEvent is a timestamped host ledger row. Detail stays textual.
//
//nolint:govet // report fields follow the ledger columns.
type HostEvent struct {
	At     time.Time `json:"time"`
	Lane   string    `json:"lane"`
	Kind   string    `json:"kind"`
	Target string    `json:"target,omitempty"`
	Detail string    `json:"detail"`
}

// HostActionMatch describes mechanical agreement, never correctness.
//
//nolint:govet // group the action and its corresponding host row.
type HostActionMatch struct {
	Action         Action     `json:"action"`
	Classification string     `json:"classification"`
	Host           *HostEvent `json:"host,omitempty"`
}

type HostDiff struct {
	Counts          map[string]int    `json:"counts"`
	Actions         []HostActionMatch `json:"actions"`
	OrcaMissed      []HostEvent       `json:"orca_missed"`
	EventsAvailable bool              `json:"events_available"`
}

func writeHostDiff(outDir, runDir, actionPath string, started, ended time.Time) error {
	events, available, err := readHostEvents(filepath.Join(runDir, "EVENTS"), started.Add(-30*time.Minute), ended.Add(30*time.Minute))
	if err != nil {
		return err
	}
	report := HostDiff{Counts: map[string]int{"agree": 0, "differs": 0, "host-did-not": 0, "orca-missed": 0}, Actions: []HostActionMatch{}, OrcaMissed: []HostEvent{}, EventsAvailable: available}
	if err := walkShadowActions(actionPath, func(a Action) error {
		switch a.Kind {
		case "exec_read", "http_refused", "read_degraded":
			return nil
		}
		report.Actions = append(report.Actions, HostActionMatch{Action: a, Classification: "host-did-not"})
		return nil
	}); err != nil {
		return err
	}
	sort.SliceStable(report.Actions, func(i, j int) bool { return report.Actions[i].Action.At.Before(report.Actions[j].Action.At) })
	type eventKey struct{ lane, kind string }
	byKind := make(map[eventKey][]int)
	for i, e := range events {
		key := eventKey{e.Lane, e.Kind}
		byKind[key] = append(byKind[key], i)
	}
	for _, indices := range byKind {
		sort.SliceStable(indices, func(i, j int) bool { return events[indices[i]].At.Before(events[indices[j]].At) })
	}
	used := make([]bool, len(events))
	// Reserve exact matches before pairing differences, so an earlier disagreement
	// cannot consume a later action's matching host row.
	for _, classification := range []string{"agree", "differs"} {
		for i := range report.Actions {
			match := &report.Actions[i]
			if match.Host != nil {
				continue
			}
			kind, target := shadowActionIdentity(match.Action)
			best := -1
			distance := 31 * time.Minute
			indices := byKind[eventKey{match.Action.Lane, kind}]
			earliest, latest := match.Action.At.Add(-30*time.Minute), match.Action.At.Add(30*time.Minute)
			first := sort.Search(len(indices), func(i int) bool { return !events[indices[i]].At.Before(earliest) })
			for _, j := range indices[first:] {
				e := &events[j]
				if e.At.After(latest) {
					break
				}
				delta := match.Action.At.Sub(e.At)
				if delta < 0 {
					delta = -delta
				}
				if used[j] {
					continue
				}
				same := target != "" && target == e.Target
				if (classification == "agree") != same {
					continue
				}
				if delta < distance || (delta == distance && j < best) {
					best, distance = j, delta
				}
			}
			if best >= 0 {
				used[best] = true
				match.Host = &events[best]
				match.Classification = classification
			}
		}
	}
	for i := range report.Actions {
		m := &report.Actions[i]
		report.Counts[m.Classification]++
	}
	for i, e := range events {
		if !used[i] && !e.At.Before(started) && !e.At.After(ended) {
			report.OrcaMissed = append(report.OrcaMissed, e)
			report.Counts["orca-missed"]++
		}
	}
	if err := writeShadowReport(filepath.Join(outDir, "host-diff.json"), func(b *bufio.Writer) error {
		enc := json.NewEncoder(b)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}); err != nil {
		return err
	}
	return writeShadowReport(filepath.Join(outDir, "host-diff.md"), func(b *bufio.Writer) error {
		b.WriteString("# Host diff\n\nMechanical matching by lane, kind and target within ±30 minutes. Each host row is used at most once. These classifications do not judge correctness.\n\n")
		if !available {
			b.WriteString("EVENTS unavailable; host-did-not means no host evidence is available.\n\n")
		}
		for _, pair := range sortedCounts(report.Counts) {
			fmt.Fprintf(b, "- %s: %d\n", pair[0], pair[1])
		}
		b.WriteString("\n## Orca actions\n")
		for i := range report.Actions {
			m := &report.Actions[i]
			fmt.Fprintf(b, "- %s `%s` lane=%s **%s**", m.Action.At.UTC().Format(time.RFC3339), m.Action.Kind, m.Action.Lane, m.Classification)
			if m.Host != nil {
				fmt.Fprintf(b, " — host %s `%s` target=%s: %s", m.Host.At.UTC().Format(time.RFC3339), m.Host.Kind, m.Host.Target, m.Host.Detail)
			}
			b.WriteByte('\n')
		}
		b.WriteString("\n## Orca-missed host decisions\n")
		for _, e := range report.OrcaMissed {
			fmt.Fprintf(b, "- %s lane=%s `%s` target=%s: %s\n", e.At.UTC().Format(time.RFC3339), e.Lane, e.Kind, e.Target, e.Detail)
		}
		return nil
	})
}

func shadowActionIdentity(a Action) (string, string) {
	kind := hostDecisionKind(a.Kind)
	var args map[string]json.RawMessage
	_ = json.Unmarshal(a.Arguments, &args)
	field := func(names ...string) string {
		for _, name := range names {
			var value string
			if raw, ok := args[name]; ok && json.Unmarshal(raw, &value) == nil && value != "" {
				return value
			}
		}
		return ""
	}
	switch kind {
	case "start_phase":
		return kind, field("phase", "Phase")
	case "stop", "steer":
		target := field("terminal_id", "TerminalID", "target", "Target")
		if target == "" {
			target = field("session_id", "SessionID")
		}
		return kind, target
	default:
		return kind, field("target", "Target", "id", "ID")
	}
}

func hostDecisionKind(kind string) string {
	switch strings.ToLower(kind) {
	case "spawn", "start_phase", "phase.start", "start", "started", "phase-start", "phase", "step":
		return "start_phase"
	case "stop", "stopped", "terminal-stop":
		return "stop"
	case "steer", "steered", "steering":
		return "steer"
	default:
		return kind
	}
}

func hostEventTarget(kind, detail string) string {
	for _, field := range strings.Fields(detail) {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch strings.ReplaceAll(strings.ToLower(key), "_", "") {
		case "phase":
			if kind == "start_phase" {
				return strings.Trim(value, ",;")
			}
		case "terminal", "terminalid", "target":
			if kind != "start_phase" {
				return strings.Trim(value, ",;")
			}
		}
	}
	fields := strings.Fields(detail)
	if kind == "start_phase" && len(fields) > 0 {
		if _, next, ok := strings.Cut(detail, "->"); ok {
			fields = strings.Fields(next)
		}
		if len(fields) > 0 {
			return fields[0]
		}
	}
	// Some ledger writers spell targets as "terminal <id>".
	if kind == "stop" || kind == "steer" {
		for i := 0; i+1 < len(fields); i++ {
			if fields[i] == "terminal" {
				return strings.Trim(fields[i+1], "(),;")
			}
		}
	}
	return ""
}

func readHostEvents(path string, started, ended time.Time) ([]HostEvent, bool, error) {
	f, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer f.Close()
	var events []HostEvent
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.SplitN(scanner.Text(), "\t", 4)
		if len(fields) < 4 {
			continue
		}
		at, err := time.Parse(time.RFC3339Nano, fields[0])
		if err != nil || at.Before(started) || at.After(ended) {
			continue
		}
		// Observations are not host decisions. Unrecognized kinds remain visible as
		// host-only decisions instead of guessing a meaning from their prose.
		switch fields[2] {
		case "health-state", "heartbeat", "poll", "report", "completed", "exited":
			continue
		}
		kind := hostDecisionKind(fields[2])
		events = append(events, HostEvent{At: at, Lane: fields[1], Kind: kind, Target: hostEventTarget(kind, fields[3]), Detail: fields[3]})
	}
	if err := scanner.Err(); err != nil {
		return nil, true, fmt.Errorf("read host EVENTS: %w", err)
	}
	return events, true, nil
}

func shadowUsage(ctx context.Context, st store.Store, runID string, decisions []store.Decision) (model.Usage, error) {
	threads, err := st.Threads().ListThreads(ctx, store.ThreadFilter{RunID: runID})
	if err != nil {
		return model.Usage{}, err
	}
	byTurn := map[string]model.Usage{}
	var total model.Usage
	for i := range decisions {
		d := &decisions[i]
		usage := model.Usage{InputTokens: d.InputTokens, OutputTokens: d.OutputTokens, CostUSD: d.CostUSD}
		if d.TurnID == "" {
			total = total.Add(usage)
		} else {
			byTurn[d.TurnID] = byTurn[d.TurnID].Add(usage)
		}
	}
	for i := range threads {
		thread := &threads[i]
		turns, err := st.Threads().ListTurns(ctx, thread.ID)
		if err != nil {
			return model.Usage{}, err
		}
		for j := range turns {
			turn := &turns[j]
			usage := byTurn[turn.ID]
			usage.InputTokens = max(usage.InputTokens, turn.InputTokens)
			usage.OutputTokens = max(usage.OutputTokens, turn.OutputTokens)
			usage.CostUSD = max(usage.CostUSD, turn.CostUSD)
			byTurn[turn.ID] = usage
		}
	}
	for _, usage := range byTurn {
		total = total.Add(usage)
	}
	return total, nil
}
