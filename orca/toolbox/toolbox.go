// Package toolbox is the shell command surface of an adopted run's dispatcher.
// Reads come from the run ledger; every mutation runs the swarm skill's own
// scripts, so PR readiness, merges, leases and probes stay the skill's.
package toolbox

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

var (
	ErrRefused = errors.New("orca toolbox refused")
	ErrTimeout = errors.New("orca wait timed out")
)

//nolint:govet // CLI options are grouped by parse concern.
type options struct {
	dir, intent, skill, key, reason, accept, phase, brief, dependsOn string
	acceptProvided                                                   bool
	shadow                                                           bool
	timeout                                                          time.Duration
}

//nolint:govet // ledger projection follows the run JSON shape.
type task struct {
	ID, Status, Phase, Brief, Mission, Description, Checks, Hold string
	WaitingOn                                                    string `json:"waiting_on"`
	NebulaSession                                                string `json:"nebula_session"`
	NebulaTerminal                                               string `json:"nebula_terminal"`
	PRHead                                                       string `json:"pr_head"`
	ApprovalSHA                                                  string `json:"approval_sha"`
	MergeSHA                                                     string `json:"merge_sha"`
	PRState                                                      string `json:"pr_state"`
	HoldNote                                                     string `json:"hold_note"`
	PR, Round                                                    int
	DependsOn                                                    []string `json:"depends_on"`
	holds                                                        []string
	snapshot                                                     string
}

// overlayEntry applies only intent fields while the mirrored row is unchanged.
type overlayEntry struct {
	Fields map[string]any
	Base   string
}

// ledgerHolds lists a ledger row's set holds: hold and any hold_* field, as
// the skill's enqueue reads them, and waiting_on, blocked_on and park, as
// orca adopt reads them.
func ledgerHolds(fields map[string]any) []string {
	var holds []string
	for k, v := range fields {
		if (k == "hold" || strings.HasPrefix(k, "hold_") || k == "waiting_on" || k == "blocked_on" || k == "park") && v != nil && v != false && v != "" {
			holds = append(holds, fmt.Sprintf("%s=%v", k, v))
		}
	}
	slices.Sort(holds)
	return holds
}

// holdFiles are the per-lane hold files: the skill's .hold-merge, and those
// orca adopt reads as host holds. .hold-advance is not one (see validate).
var holdFiles = []string{".hold", ".hold-merge", ".hold-note", ".park", ".blocked"}

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, args...))
}

// Run executes one toolbox command: wait, status, check, lane, pr, ask or note.
func Run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) > 1 && (args[0] == "lane" || args[0] == "pr") {
		args = append([]string{args[0] + "." + args[1]}, args[2:]...)
	}
	if len(args) == 0 {
		return errors.New("orca: toolbox command required")
	}
	cmd := args[0]
	o, rest, err := parse(args[1:])
	if err != nil {
		return err
	}
	if absDir, absErr := filepath.Abs(o.dir); absErr == nil {
		o.dir = absDir
	}
	if filepath.IsAbs(o.brief) {
		if rel, relErr := filepath.Rel(o.dir, o.brief); relErr == nil && !strings.HasPrefix(rel, ".."+string(os.PathSeparator)) && rel != ".." {
			o.brief = rel
		}
	}
	if cmd == "wait" {
		ctx, cancel := context.WithTimeout(ctx, cmp.Or(o.timeout, Heartbeat))
		defer cancel()
		changed, err := waitBaseline(ctx, o.dir)
		if err == nil {
			_, err = fmt.Fprintln(out, "changed: "+strings.Join(changed, ", "))
		}
		return err
	}
	if o.shadow {
		// Take the overlay lock before mutate's action lock on every shadow path.
		if err := os.MkdirAll(o.intent, 0o700); err != nil {
			return err
		}
		lock, err := os.OpenFile(filepath.Join(o.intent, ".ledger-overlay.lock"), os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			return err
		}
		defer lock.Close()
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
			return err
		}
	}
	if cmd == "ask" || cmd == "note" {
		if len(rest) == 0 {
			return errors.New("message text is required")
		}
		return mutate(ctx, o, cmd, &task{ID: "run"}, strings.Join(rest, " "), out)
	}
	tasks, err := loadFor(o)
	if err != nil {
		return err
	}
	if cmd == "status" {
		return json.NewEncoder(out).Encode(tasks)
	}
	if len(rest) == 0 {
		return errors.New("a lane id or pull request number is required")
	}
	match := func(t task) bool { return t.ID == rest[0] }
	if strings.HasPrefix(cmd, "pr.") {
		match = func(t task) bool { return t.PR > 0 && strconv.Itoa(t.PR) == rest[0] }
	}
	i := slices.IndexFunc(tasks, match)
	if i < 0 {
		if cmd == "lane.start" && o.phase != "" && o.brief != "" {
			t := &task{ID: rest[0], Status: "planned", Phase: "", Brief: o.brief}
			for _, dep := range strings.Split(o.dependsOn, ",") {
				if dep = strings.TrimSpace(dep); dep != "" {
					t.DependsOn = append(t.DependsOn, dep)
				}
			}
			if err := validateFunding(o, t, tasks); err != nil {
				return err
			}
			return mutate(ctx, o, cmd, t, "", out)
		}
		return refuse("%q is not in this run", rest[0])
	}
	t := &tasks[i]
	switch cmd {
	case "check":
		res, err := skill(ctx, o, "check", t, "")
		if err == nil {
			_, err = fmt.Fprintf(out, "lane %s status=%s %s\n", t.ID, t.Status, res)
		}
		return err
	case "lane.start", "lane.steer", "lane.stop":
		if cmd == "lane.start" && o.phase != "" {
			var err error
			if t.Phase == o.phase && !sweRework(o, t) {
				err = validate(ctx, o, cmd, t, tasks)
			} else {
				err = validateAdvance(ctx, o, t)
			}
			if err != nil {
				return err
			}
		}
		return mutate(ctx, o, cmd, t, strings.Join(rest[1:], " "), out)
	case "lane.hold":
		if isFile(filepath.Join(o.dir, t.ID+".hold-advance")) {
			_, err := fmt.Fprintf(out, "lane %s already holds advance\n", t.ID)
			return err
		}
		return mutate(ctx, o, cmd, t, cmp.Or(o.reason, "orca: the orchestrator advances and merges this lane by hand"), out)
	case "pr.approve":
		// On a run that holds advance, no gate records a review PASS: approve
		// records it, as snl_gate does, once the report's HEAD is the PR head.
		if err := validate(ctx, o, cmd, t, tasks); err != nil {
			return err
		}
		op, text := "approval", ""
		if o.acceptProvided {
			if strings.TrimSpace(o.accept) == "" {
				return refuse("PR #%d acceptance reason is empty", t.PR)
			}
			if t.Round < 2 {
				return refuse("PR #%d named review Breaks can be accepted only after review round 2", t.PR)
			}
		}
		if o.acceptProvided {
			// Acceptance preflight remains read-only. The live mutation repeats
			// these exact-head checks under the receipt lock before rewriting review.
			head, err := skill(ctx, o, "accept-review-check", t, "")
			if err != nil {
				return refuse("PR #%d has no review PASS at its head: %v", t.PR, err)
			}
			return mutate(ctx, o, cmd, t, head, out)
		}
		head, err := skill(ctx, o, op, t, text)
		if err != nil {
			return refuse("PR #%d has no review PASS at its head: %v", t.PR, err)
		}
		return mutate(ctx, o, cmd, t, head, out)
	case "pr.rerun":
		if err := validate(ctx, o, cmd, t, tasks); err != nil {
			return err
		}
		if t.PRHead == "" {
			return refuse("PR #%d has no exact head to rerun", t.PR)
		}
		return mutate(ctx, o, cmd, t, t.PRHead, out)
	case "pr.ready", "pr.update", "pr.merge":
		if err := validate(ctx, o, cmd, t, tasks); err != nil {
			return err
		}
		// Exact head: the ledger's head and approval agree, and the skill's
		// claim and check gates then verify that head against the live PR.
		if t.PRHead == "" || t.ApprovalSHA != t.PRHead {
			return refuse("PR #%d approval %q does not match ledger head %q", t.PR, t.ApprovalSHA, t.PRHead)
		}
		if cmd == "pr.update" {
			return mutate(ctx, o, cmd, t, t.PRHead, out)
		}
		if o.shadow && cmd == "pr.ready" {
			// Shadow runs perform the same live read-only readiness checks, but
			// never invoke the skill's gh pr ready mutation.  The mutation path
			// below records the intent in the shadow ledger instead.
			if _, err := skill(ctx, o, "ready-check", t, t.PRHead); err != nil {
				return refuse("PR #%d is not ready at %s: %v", t.PR, t.PRHead, err)
			}
			if tasks, err = loadFor(o); err != nil {
				return err
			}
			if i = slices.IndexFunc(tasks, func(c task) bool { return c.ID == t.ID }); i < 0 {
				return refuse("lane %s left the ledger", t.ID)
			}
			t = &tasks[i]
			return mutate(ctx, o, cmd, t, t.PRHead, out)
		}
		readyOp := "ready"
		if cmd == "pr.merge" {
			// Merge still performs the ready checks, but a draft PR must remain a
			// draft: only the explicit pr ready action may promote it.
			readyOp = "merge-check"
		}
		if _, err := skill(ctx, o, readyOp, t, t.PRHead); err != nil {
			return refuse("PR #%d is not ready at %s: %v", t.PR, t.PRHead, err)
		}
		// ready records the head it passed: the approved head or its base-only update.
		if tasks, err = loadFor(o); err != nil {
			return err
		}
		if i = slices.IndexFunc(tasks, func(c task) bool { return c.ID == t.ID }); i < 0 {
			return refuse("lane %s left the ledger", t.ID)
		}
		t = &tasks[i]
		if cmd == "pr.ready" {
			_, err := fmt.Fprintf(out, "PR #%d ready at %s\n", t.PR, t.PRHead)
			return err
		}
		return mutate(ctx, o, cmd, t, t.PRHead, out)
	}
	return fmt.Errorf("orca: unknown toolbox command %q", cmd)
}

// parse accepts flags anywhere among the positional arguments.
func parse(args []string) (options, []string, error) {
	o := options{}
	fs := flag.NewFlagSet("orca", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.StringVar(&o.dir, "dir", cmp.Or(os.Getenv("ORCA_TOOLBOX_DIR"), "."), "run directory")
	fs.StringVar(&o.intent, "intent-dir", os.Getenv("ORCA_TOOLBOX_INTENT_DIR"), "shadow intent directory")
	fs.StringVar(&o.skill, "skill", os.Getenv("ORCA_SNL_DIR"), "swarm skill scripts directory")
	fs.StringVar(&o.key, "action-key", "", "idempotency key")
	fs.StringVar(&o.reason, "reason", "", "lane hold reason")
	fs.StringVar(&o.accept, "accept", "", "accept a named review Break as follow-up")
	fs.StringVar(&o.phase, "phase", "", "target lane phase")
	fs.StringVar(&o.brief, "brief", "", "run-local lane brief")
	fs.StringVar(&o.dependsOn, "depends-on", "", "comma-separated lane dependencies")
	fs.BoolVar(&o.shadow, "shadow", os.Getenv("ORCA_TOOLBOX_SHADOW") == "1", "record intent only")
	fs.DurationVar(&o.timeout, "timeout", 0, "wait timeout")
	var rest []string
	for {
		if err := fs.Parse(args); err != nil {
			return o, nil, err
		}
		if args = fs.Args(); len(args) == 0 {
			break
		}
		rest, args = append(rest, args[0]), args[1:]
	}
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "accept" {
			o.acceptProvided = true
		}
	})
	if bound := os.Getenv("ORCA_TOOLBOX_DIR"); bound != "" && !(inside(bound, o.dir) && inside(o.dir, bound)) {
		return o, nil, refuse("session toolbox is bound to its run workspace")
	}
	if o.shadow && (o.intent == "" || inside(o.dir, o.intent)) {
		return o, nil, refuse("shadow intent directory must be outside the run workspace")
	}
	return o, rest, nil
}

// inside reports whether path is dir itself or below it.
func inside(dir, path string) bool {
	d, err1 := filepath.Abs(dir)
	p, err2 := filepath.Abs(path)
	return err1 == nil && err2 == nil && (p == d || strings.HasPrefix(p, d+string(os.PathSeparator)))
}

func load(dir string) ([]task, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return nil, fmt.Errorf("read state.json: %w", err)
	}
	var s struct{ Tasks []json.RawMessage }
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse state.json: %w", err)
	}
	tasks := make([]task, len(s.Tasks))
	for i, row := range s.Tasks {
		var fields map[string]any
		if err := errors.Join(json.Unmarshal(row, &tasks[i]), json.Unmarshal(row, &fields)); err != nil {
			return nil, fmt.Errorf("parse state.json: %w", err)
		}
		tasks[i].holds = ledgerHolds(fields)
		canonical, _ := json.Marshal(fields)
		tasks[i].snapshot = string(canonical)
	}
	return tasks, nil
}

func loadFor(o options) ([]task, error) {
	tasks, err := load(o.dir)
	if err != nil {
		return nil, err
	}
	if !o.shadow || o.intent == "" {
		return tasks, nil
	}
	raw, err := os.ReadFile(filepath.Join(o.intent, "ledger-overlay.json"))
	if os.IsNotExist(err) {
		return tasks, nil
	}
	if err != nil {
		return nil, err
	}
	var rows map[string]overlayEntry
	if err := json.Unmarshal(raw, &rows); err != nil {
		return nil, fmt.Errorf("parse shadow ledger overlay: %w", err)
	}
	dirty := false
	for id := range rows {
		entry := rows[id]
		i := slices.IndexFunc(tasks, func(t task) bool { return t.ID == id })
		if i < 0 && entry.Base == "" && entry.Fields != nil {
			tasks = append(tasks, task{ID: id})
			i = len(tasks) - 1
		}
		if i < 0 || entry.Fields == nil || entry.Base != tasks[i].snapshot {
			delete(rows, id)
			dirty = true
			continue
		}
		fields, _ := json.Marshal(entry.Fields)
		if err := json.Unmarshal(fields, &tasks[i]); err != nil {
			return nil, err
		}
		if tasks[i].Hold == "__hold-advance" {
			tasks[i].holds = append(tasks[i].holds, id+".hold-advance")
		}
	}
	if dirty {
		return tasks, saveOverlay(o, rows)
	}
	return tasks, nil
}

func validateFunding(o options, t *task, tasks []task) error {
	if o.phase != "swe" {
		return refuse("lane %s can only be funded at phase swe", t.ID)
	}
	if t.ID == "" || t.ID == "run" || strings.ContainsAny(t.ID, `/\\`) || t.ID == "." || t.ID == ".." {
		return refuse("lane %s is not a valid lane id", t.ID)
	}
	if !runFile(o.dir, t.Brief) {
		return refuse("lane %s brief file is missing, empty or outside the run", t.ID)
	}
	raw, err := os.ReadFile(filepath.Join(o.dir, t.Brief))
	if err != nil || !strings.Contains(string(raw), "Cause check:") {
		return refuse("lane %s brief must contain a Cause check:", t.ID)
	}
	for _, dep := range t.DependsOn {
		i := slices.IndexFunc(tasks, func(d task) bool { return d.ID == dep })
		if i < 0 {
			return refuse("lane %s depends on unknown lane %s", t.ID, dep)
		}
	}
	return nil
}

func validateAdvance(ctx context.Context, o options, t *task) error {
	rework := sweRework(o, t)
	if t.Status != "running" || o.phase == "" || (t.Phase == o.phase && !rework) {
		return refuse("lane %s cannot advance from phase %q to %q", t.ID, t.Phase, o.phase)
	}
	wantPhase := map[string][]string{"swe": {"clean", "review", "swe"}, "clean": {"review"}, "review": {"swe"}}[t.Phase]
	if !slices.Contains(wantPhase, o.phase) {
		return refuse("lane %s cannot advance from phase %q to %q", t.ID, t.Phase, o.phase)
	}
	if (t.Phase == "review" && o.phase == "swe" || rework) && (o.brief == "" || !runFile(o.dir, o.brief)) {
		return refuse("lane %s rework needs a non-empty --brief inside the run", t.ID)
	}
	advanceHeld := slices.Contains(t.holds, t.ID+".hold-advance")
	if _, err := os.Stat(filepath.Join(o.dir, t.ID+".hold-advance")); err == nil {
		advanceHeld = true
	}
	if !advanceHeld {
		return refuse("lane %s is not held for hand advance", t.ID)
	}
	for _, hold := range t.holds {
		if hold != t.ID+".hold-advance" {
			return refuse("lane %s is held: %s", t.ID, hold)
		}
	}
	for _, suffix := range holdFiles {
		if _, err := os.Stat(filepath.Join(o.dir, t.ID+suffix)); err == nil {
			return refuse("lane %s is held: %s%s", t.ID, t.ID, suffix)
		}
	}
	name := t.ID + "." + t.Phase + ".md"
	if !runFile(o.dir, name) {
		return refuse("lane %s current phase %q has no verified done report", t.ID, t.Phase)
	}
	raw, _ := os.ReadFile(filepath.Join(o.dir, name))
	if t.Phase == "review" && o.phase == "swe" {
		if t.Round >= reviewRoundCap() {
			return refuse("lane %s review round %d reaches the review round cap %d", t.ID, t.Round, reviewRoundCap())
		}
		if reportNeedsSWEWithBreaks(string(raw)) {
			return nil
		}
	} else if rework {
		if t.Round >= reviewRoundCap() {
			return refuse("lane %s swe round %d reaches the review round cap %d", t.ID, t.Round, reviewRoundCap())
		}
		if reportOutcomeDone(string(raw)) {
			return nil
		}
	} else if reportOutcomeDone(string(raw)) {
		return nil
	}
	return refuse("lane %s current phase %q has no verified done report", t.ID, t.Phase)
}

func sweDoneReport(dir, id string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, id+".swe.md"))
	return err == nil && reportOutcomeDone(string(raw))
}

func sweRework(o options, t *task) bool {
	return t.Phase == "swe" && o.phase == "swe" && o.brief != "" &&
		isFile(filepath.Join(o.dir, t.ID+".hold-advance")) && sweDoneReport(o.dir, t.ID)
}

// reviewRoundCap mirrors the swarm skill's default and environment override.
// The current review round is inclusive: a lane at the cap is escalated rather
// than starting another SWE session.
func reviewRoundCap() int {
	const defaultCap = 3
	raw := os.Getenv("SNL_MAX_ROUNDS")
	cap, err := strconv.Atoi(raw)
	if err != nil || cap < 1 {
		return defaultCap
	}
	return cap
}

func reportOutcomeDone(raw string) bool {
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "Outcome: done" {
			return true
		}
	}
	return false
}

func reportNeedsSWEWithBreaks(raw string) bool {
	lines := strings.Split(raw, "\n")
	needsSWE := false
	breaks := false
	for i, line := range lines {
		line = strings.TrimSpace(line)
		if line == "Outcome: needs-swe" {
			needsSWE = true
		}
		if !strings.HasPrefix(line, "Breaks:") {
			continue
		}
		if strings.TrimSpace(strings.TrimPrefix(line, "Breaks:")) != "" {
			breaks = true
			continue
		}
		for _, next := range lines[i+1:] {
			next = strings.TrimSpace(next)
			if next == "" {
				continue
			}
			if !strings.HasPrefix(next, "-") && !strings.HasPrefix(next, "*") && strings.Contains(next, ":") {
				break
			}
			breaks = true
			break
		}
	}
	return needsSWE && breaks
}

// validate holds the refusals: host holds, waits and finished lanes win over
// any dispatcher action, and a live action needs a live current terminal.
func validate(ctx context.Context, o options, op string, t *task, tasks []task) error {
	if t.Status == "done" || t.Status == "cancelled" || t.MergeSHA != "" || strings.EqualFold(t.PRState, "merged") {
		return refuse("lane %s is done or merged", t.ID)
	}
	if op == "lane.hold" { // adding a hold is never unsafe
		return nil
	}
	// .hold-advance stops the watcher advancing and integrating a lane; it
	// reserves those for the orchestrator, so steering and a hand merge
	// (pr ready|update|merge) pass it. Every other hold refuses every action.
	held := make([]string, 0, len(t.holds))
	virtualAdvance := false
	for _, hold := range t.holds {
		if hold == t.ID+".hold-advance" {
			virtualAdvance = true
			continue
		}
		held = append(held, hold)
	}
	for _, suffix := range holdFiles {
		if _, err := os.Stat(filepath.Join(o.dir, t.ID+suffix)); err == nil {
			held = append(held, t.ID+suffix)
		}
	}
	if _, err := os.Stat(filepath.Join(o.dir, t.ID+".hold-advance")); err == nil {
		virtualAdvance = true
	}
	if virtualAdvance && strings.HasPrefix(op, "lane.") && op != "lane.steer" && !(op == "lane.start" && (o.phase != "" || t.Status == "planned")) {
		held = append(held, t.ID+".hold-advance")
	}
	if len(held) > 0 {
		return refuse("lane %s is held: %s", t.ID, strings.Join(held, ", "))
	}
	switch op {
	case "lane.start":
		// A running lane whose current terminal is gone or no longer alive gets
		// a fresh terminal for its current phase from the phase's stored brief.
		if t.Status == "running" {
			if phaseBrief(o.dir, t) == "" {
				return refuse("lane %s has no brief for its current phase %q", t.ID, t.Phase)
			}
			if t.NebulaTerminal == "" {
				return nil
			}
			state, err := skill(ctx, o, "state", t, "")
			if errors.Is(err, ErrRefused) {
				return err
			}
			if err != nil || state == "working" || state == "waiting" {
				return refuse("lane %s current terminal %s is alive or unreadable (%s)", t.ID, t.NebulaTerminal, cmp.Or(state, "unknown"))
			}
			return nil
		}
		if t.Status != "planned" || t.NebulaSession != "" || t.NebulaTerminal != "" {
			return refuse("lane %s is already started or not planned", t.ID)
		}
		if strings.TrimSpace(t.Brief+t.Mission+t.Description) == "" {
			return refuse("lane %s has no brief", t.ID)
		}
		if name := strings.TrimSpace(t.Brief); strings.HasSuffix(name, ".brief") && !runFile(o.dir, name) {
			return refuse("lane %s brief file is missing, empty or outside the run", t.ID)
		}
		for _, id := range t.DependsOn {
			if i := slices.IndexFunc(tasks, func(d task) bool { return d.ID == id }); i < 0 || tasks[i].Status != "done" {
				return refuse("lane %s depends on unfinished lane %s", t.ID, id)
			}
		}
	case "lane.steer", "lane.stop":
		if t.NebulaSession == "" || t.NebulaTerminal == "" {
			return refuse("lane %s has no current terminal", t.ID)
		}
		state, err := skill(ctx, o, "state", t, "")
		if errors.Is(err, ErrRefused) {
			return err
		}
		// An idle terminal is alive: a steer to it is terminal care's nudge.
		if err != nil || (state != "working" && state != "waiting" && state != "idle") {
			return refuse("lane %s current terminal is not alive (%s)", t.ID, cmp.Or(state, "unknown"))
		}
	}
	return nil
}

// runFile reports whether name is a non-empty regular file inside the run.
func runFile(dir, name string) bool {
	pathName := name
	if !filepath.IsAbs(pathName) {
		pathName = filepath.Join(dir, pathName)
	}
	path, err := filepath.EvalSymlinks(pathName)
	if err != nil || !inside(dir, path) {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

// phaseBrief is the run-relative brief the skill stored for the lane's
// current phase: the copy snl_spawn keeps, else the written phase brief.
func phaseBriefFor(dir, id, phase string) string {
	for _, name := range []string{id + "." + phase + ".brief.txt", filepath.Join("briefs", id+"."+phase+".md")} {
		if runFile(dir, name) {
			return name
		}
	}
	return ""
}

func phaseBrief(dir string, t *task) string { return phaseBriefFor(dir, t.ID, t.Phase) }

// mutate refuses unsafe actions, then runs the skill once per action key. A
// pending receipt after a crash refuses a retry whose effect is uncertain.
func mutate(ctx context.Context, o options, op string, t *task, text string, out io.Writer) error {
	dir := o.dir
	if o.shadow {
		dir = o.intent
	}
	actions := filepath.Join(dir, ".orca-actions")
	if err := os.MkdirAll(actions, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(actions, "lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	fingerprint := op + "\x00" + t.ID + "\x00" + text
	if op == "lane.start" {
		fingerprint += "\x00phase=" + o.phase + "\x00brief=" + o.brief
	}
	sum := sha256.Sum256([]byte(cmp.Or(o.key, fingerprint)))
	receipt := filepath.Join(actions, hex.EncodeToString(sum[:]))
	if op == "pr.rerun" {
		entries, _ := os.ReadDir(actions)
		for _, entry := range entries {
			if entry.Name() == "lock" || entry.Name() == filepath.Base(receipt) || entry.IsDir() {
				continue
			}
			raw, readErr := os.ReadFile(filepath.Join(actions, entry.Name()))
			if readErr == nil && (strings.HasPrefix(string(raw), "done\n"+fingerprint+"\nkey=") || strings.HasPrefix(string(raw), "pending\n"+fingerprint+"\nkey=")) {
				return refuse("PR rerun is already recorded for exact head %s", text)
			}
		}
	}
	switch raw, err := os.ReadFile(receipt); {
	case err == nil && receiptMatches(string(raw), "done", fingerprint, op, o.key):
		_, err = fmt.Fprintln(out, "already recorded")
		return err
	case err == nil && receiptMatches(string(raw), "pending", fingerprint, op, o.key):
		return refuse("action %s has uncertain effects; inspect before issuing a new action key", op)
	case err == nil:
		return refuse("action key %q was already used with different arguments", o.key)
	case !os.IsNotExist(err):
		return err
	}
	if t.ID != "run" { // recheck under the lock against the current ledger
		tasks, err := loadFor(o)
		if err != nil {
			return err
		}
		found := false
		if i := slices.IndexFunc(tasks, func(c task) bool { return c.ID == t.ID }); i >= 0 {
			t = &tasks[i]
			found = true
		}
		if found {
			var validationErr error
			if op == "lane.start" && o.phase != "" {
				if t.Phase == o.phase && !sweRework(o, t) {
					validationErr = validate(ctx, o, op, t, tasks)
				} else {
					validationErr = validateAdvance(ctx, o, t)
				}
			} else {
				validationErr = validate(ctx, o, op, t, tasks)
			}
			if validationErr != nil {
				return validationErr
			}
		} else if !(op == "lane.start" && o.phase != "") {
			return refuse("lane %s is not in the ledger", t.ID)
		}
	}
	receiptBody := "pending\n" + fingerprint
	if op == "pr.rerun" {
		receiptBody += "\nkey=" + o.key
	}
	if err := os.WriteFile(receipt, []byte(receiptBody), 0o600); err != nil {
		return err
	}
	result := "shadow: recorded"
	if o.shadow {
		err = recordIntent(dir, op, t, text, o.key)
		if err == nil && t.ID != "run" && (op == "lane.start" || op == "lane.hold" || op == "pr.approve") {
			err = recordOverlay(o, op, t, text)
		}
	} else {
		result, err = skill(ctx, o, op, t, text)
	}
	if err == nil && o.shadow && op == "pr.ready" {
		result = fmt.Sprintf("PR #%d would mark ready at %s", t.PR, text)
	}
	if err == nil {
		done := "done\n" + fingerprint
		if op == "pr.rerun" {
			done += "\nkey=" + o.key
		}
		err = os.WriteFile(receipt, []byte(done), 0o600)
	}
	if err == nil {
		_, err = fmt.Fprintln(out, result)
	} else if op == "pr.rerun" && rerunPreflightRefusal(err) {
		// No GitHub mutation occurred for these checks, so a retry is safe.
		_ = os.Remove(receipt)
	}
	return err
}

func receiptMatches(raw, status, fingerprint, op, key string) bool {
	want := status + "\n" + fingerprint
	if op == "pr.rerun" {
		want += "\nkey=" + key
	}
	return raw == want
}

func rerunPreflightRefusal(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "head unreadable") ||
		strings.Contains(message, "does not match exact ledger head") ||
		strings.Contains(message, "no failed exact-head runs to rerun")
}

// recordIntent appends a shadow action in the adopt.Action shape that the
// shadow reports and host-diff scorer read.
func recordIntent(dir, op string, t *task, text, key string) error {
	id := "shadow-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	reason := "dispatcher toolbox shadow intent"
	if op == "pr.ready" {
		reason = "would mark ready"
	}
	b, err := json.Marshal(map[string]any{
		"time": time.Now().UTC().Format(time.RFC3339Nano), "lane": t.ID, "action": op,
		"arguments":   map[string]string{"action_key": key, "text": text, "phase": t.Phase, "terminal_id": t.NebulaTerminal},
		"decision_id": id, "event_id": id, "reason": reason,
	})
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "actions.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.Write(append(b, '\n'))
	return err
}

func recordOverlay(o options, op string, t *task, text string) error {
	path := filepath.Join(o.intent, "ledger-overlay.json")
	rows := map[string]overlayEntry{}
	if raw, err := os.ReadFile(path); err == nil {
		if err := json.Unmarshal(raw, &rows); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	entry := rows[t.ID]
	if entry.Fields == nil {
		entry = overlayEntry{Base: t.snapshot, Fields: map[string]any{}}
	}
	switch op {
	case "pr.approve":
		entry.Fields["pr_head"], entry.Fields["approval_sha"] = text, text
	case "lane.hold":
		entry.Fields["hold"] = "__hold-advance"
	case "lane.start":
		round := max(t.Round, 1)
		if (t.Phase == "review" && o.phase == "swe" && o.brief != "") || sweRework(o, t) {
			round++
		}
		entry.Fields["phase"], entry.Fields["round"] = cmp.Or(o.phase, t.Phase, "swe"), round
		if entry.Base == "" { // preserve funding a lane absent from the mirror
			entry.Fields["status"], entry.Fields["brief"], entry.Fields["depends_on"] = "running", t.Brief, t.DependsOn
		}
	}
	rows[t.ID] = entry
	return saveOverlay(o, rows)
}

func saveOverlay(o options, rows map[string]overlayEntry) error {
	b, err := json.Marshal(rows)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(o.intent, "ledger-overlay.json"), b, 0o600)
}

// skillOps maps toolbox operations onto the subagent-swarm-nebula scripts.
const skillOps = `set -eu
sw=$1; RUN=$2; op=$3; lane=$4; sid=$5; tid=$6; text=$7; phase=$8; brief=$9; status=${10}; round=${11:-1}; current=${12:-}; rework=${14:-}
run=$RUN; [ ! -r "$RUN/env.sh" ] || . "$RUN/env.sh"
# env.sh names the live run; a shadow copy's skill calls stay in the copy.
# SW is the scripts directory SkillDir resolved, env.sh's own unless overridden.
export RUN=$run SW=$sw
accept=${13:-}
export SNL_RUN_ID=${SNL_RUN_ID:-$(basename "$RUN")}
. "$SW/swarm_nebula_lanes.sh"; . "$SW/lane_ops.sh"; . "$SW/step.sh"
` + LeaseGuard + `
# care is the phase terminal as nebula term list reports it, for terminal care.
# background comes from the screen's tail, as the host reads it: an idle Claude
# terminal on its own gate shows "N shell[, M monitor] still running"; term list
# omits backgroundTasks both then and with no shell, so its absence proves nothing.
care='import datetime as d,json,re,sys
tid,ph,rep,scr=sys.argv[1:]
raw=open(scr).read()
try: s=json.loads(raw)
except ValueError: s=None
ls=s.get("lines") if isinstance(s,dict) else None
if not isinstance(ls,list): ls=raw.splitlines()
ls=[(l if isinstance(l,str) else str(l.get("text","")) if isinstance(l,dict) else str(l)).strip() for l in ls]
tail=[l for l in ls if any(c.isalnum() for c in l)][-6:]
bg="unknown" if not tail else "yes" if any(re.search(r"\b\d+ (shell|monitor)s?\b.*\bstill running\b",l) for l in tail) else "no"
for t in json.load(sys.stdin).get("terminals",[]):
    if t.get("id")!=tid: continue
    try:
        at=d.datetime.fromisoformat(t.get("lastOutputAt") or "")
        age=max(0,int((d.datetime.now(d.timezone.utc)-(at if at.tzinfo else at.replace(tzinfo=d.timezone.utc))).total_seconds()//60))
    except ValueError: age="unknown"
    bt=t.get("backgroundTasks")
    bt="" if bt is None else " backgroundTasks="+json.dumps(bt,separators=(",",":"))
    last=[l.strip() for l in (t.get("lastTurnText") or "").splitlines() if l.strip()][-1:] or [""]
    print("phase=%s agent=%s signal=%s output_min_ago=%s background=%s%s report_filed=%s" % (ph,t.get("agentState") or "",t.get("agentSignal") or "",age,bg,bt,rep))
    print("last_turn_line="+json.dumps(last[0][:200]))
    print("screen_tail="+json.dumps([l[:200] for l in tail],ensure_ascii=False))'
msg=$(mktemp); scr=$(mktemp); trap 'rm -f "$msg" "$scr"' EXIT; printf '%s\n' "$text" > "$msg"
accept_review_gate() {
  pr=$(snl_task "$lane" pr); head=$(snl_report_word "$lane" review HEAD); verdict=$(snl_report_word "$lane" review Verdict)
  [ "$verdict" = needs-swe ] || { echo "review Verdict is '$verdict', not needs-swe"; exit 3; }
  breaks=$(awk '/^Breaks:/ {seen=1; sub(/^Breaks:[[:space:]]*/, ""); if (length($0)) {print; exit}; next} seen && /^[[:alnum:] _-]+:/ {exit} seen && $0 !~ /^[[:space:]]*$/ {print; exit}' "$RUN/$lane.review.md")
  [ -n "$breaks" ] || { echo "review needs-swe has no named Breaks"; exit 3; }
  printf '%s' "$head" | grep -Eq '^[0-9a-fA-F]{40}$' || { echo "review HEAD is not a full commit sha"; exit 3; }
  [ -z "$text" ] || [ "$head" = "$text" ] || { echo "review HEAD $head changed since preflight $text"; exit 3; }
  new=$(snl_lane_gh "$lane" pr view "$pr" --json headRefOid --jq .headRefOid) || { echo "PR head unreadable"; exit 3; }
  [ "$head" = "$new" ] || { echo "review HEAD $head is not PR head $new"; exit 3; }
  snl_head_checks "$lane" "$pr" "$head" >/dev/null
}
head_checks_ignoring_draft() {
  # Reuse the skill's complete exact-head check and merge-state guard while
  # masking only isDraft.  This keeps ready's promotion behind the same live
  # statusCheckRollup and merge checks that follow it.
	if type head_check_guard >/dev/null 2>&1; then
		view=$(snl_lane_gh "$lane" pr view "$pr" --json headRefOid,state,isDraft,mergeStateStatus,mergeable,statusCheckRollup) || { echo "PR readiness view unreadable"; return 3; }
		live_head=$(jq -r '.headRefOid // empty' <<<"$view")
		[ "$live_head" = "$text" ] || { echo "PR head $live_head does not match approved head $text"; return 3; }
		[ "$(jq -r '.state // empty' <<<"$view")" = OPEN ] || { echo "PR is not open"; return 3; }
		merge_state=$(jq -r '.mergeStateStatus // empty' <<<"$view")
		is_draft=$(jq -r '.isDraft // false' <<<"$view")
		mergeable=$(jq -r '.mergeable // empty' <<<"$view")
		case "$merge_state" in
			CLEAN|UNSTABLE|HAS_HOOKS|BEHIND) ;;
			DRAFT) [ "$is_draft" = true ] || { echo "PR merge state is not ready ($merge_state)"; return 75; } ;;
			*) echo "PR merge state is not ready ($merge_state)"; return 75 ;;
		esac
		[ "$mergeable" != CONFLICTING ] || { echo "PR is conflicting"; return 3; }
		head_check_guard "$(jq -c '.statusCheckRollup // []' <<<"$view")" "$text" "$RUN/EVENTS"
		return $?
	fi
  eval "$(declare -f snl_lane_gh | sed '1s/snl_lane_gh/snl_lane_gh_original/')"
  snl_lane_gh() {
    if [ "$2" = pr ] && [ "$3" = view ]; then
      view=$(snl_lane_gh_original "$@") || return $?
      if jq -e 'type == "object" and has("isDraft")' <<<"$view" >/dev/null 2>&1; then
        jq -c '.isDraft = false' <<<"$view"
      else
        printf '%s\n' "$view"
      fi
    else
      snl_lane_gh_original "$@"
    fi
  }
  SNL_HEAD_CHECKS_IGNORE_DRAFT=1 snl_head_checks "$lane" "$pr" "$text"
  rc=$?
  unset -f snl_lane_gh
  eval "$(declare -f snl_lane_gh_original | sed '1s/snl_lane_gh_original/snl_lane_gh/')"
  unset -f snl_lane_gh_original
  return "$rc"
}
case $op in
 state) snl_terminal_agent_state "$sid" "$tid" ;;
 check) echo "terminal=$(snl_terminal_agent_state "$sid" "$tid") remote=$(snl_lane_remote_head "$lane")"
  if snl_sandbox_busy "$lane"; then echo "busy=yes"; else echo "busy=no"; fi
  ph=$(snl_term_phase "$lane" "$tid" 2>/dev/null) || ph=$phase
  if [ -s "$RUN/$lane.$ph.md" ]; then rep=yes; else rep=no; fi
  snl_nb term screen "$tid" > "$scr" 2>/dev/null || : > "$scr"
  snl_nb term list "$sid" | /usr/bin/env python3 -c "$care" "$tid" "$ph" "$rep" "$scr" ;;
 lane.start) if [ -n "$phase" ]; then
  if [ "$status" = running ] && [ -e "$RUN/$lane.hold-advance" ] && [ "$current" != "$phase" ] && [ "$phase" = review ]; then snl_to_review "$lane"
  elif [ "$status" = running ] && [ -e "$RUN/$lane.hold-advance" ] && [ "$current" != "$phase" ] && [ "$phase" = clean ]; then snl_to_clean "$lane"
  elif [ "$status" = running ] && [ -e "$RUN/$lane.hold-advance" ] && [ "$current" = swe ] && [ "$phase" = swe ] && [ "$rework" = 1 ]; then
    cap=${SNL_MAX_ROUNDS:-3}
    [ "$round" -lt "$cap" ] || { echo "lane $lane swe round $round reaches the review round cap $cap" >&2; exit 3; }
    snl_brief swe "$lane" "$RUN/$brief"
    python3 "$SW/ledger.py" set "$RUN" --id "$lane" --phase swe --round "$((round + 1))" >/dev/null
    snl_spawn "$lane" swe "$RUN/briefs/$lane.swe.md"
  elif [ "$status" = running ] && [ -e "$RUN/$lane.hold-advance" ] && [ "$current" = review ] && [ "$phase" = swe ]; then
    cap=${SNL_MAX_ROUNDS:-3}
    [ "$round" -lt "$cap" ] || { echo "lane $lane review round $round reaches the review round cap $cap" >&2; exit 3; }
    snl_brief swe "$lane" "$RUN/$brief"
    python3 "$SW/ledger.py" set "$RUN" --id "$lane" --phase swe --round "$((round + 1))" >/dev/null
    snl_spawn "$lane" swe "$RUN/briefs/$lane.swe.md"
  elif [ "$status" != running ] && type snl_fund_swe >/dev/null 2>&1; then snl_fund_swe "$lane" "$RUN/$brief"
  else
    [ "$status" = running ] || python3 "$SW/ledger.py" add "$RUN" --id "$lane" --title "$(basename "$brief")" --branch "${SNL_LANE_BRANCH:-swarm-lane/run/$SNL_RUN_ID/$lane}" --brief "$RUN/$brief" >/dev/null
    snl_spawn "$lane" "$phase" "$RUN/$brief"
  fi
 else snl_fund_planned "$lane"; fi ;;
 lane.steer) snl_nb term send "$tid" --file "$msg" ;;
 lane.stop) snl_nb term stop "$tid" ;;
 pr.rerun) pr=$(snl_task "$lane" pr)
  live=$(snl_lane_gh "$lane" pr view "$pr" --json headRefOid --jq .headRefOid) || { echo "PR head unreadable" >&2; exit 3; }
  [ "$live" = "$text" ] || { echo "PR #$pr head $live does not match exact ledger head $text" >&2; exit 3; }
  if type snl_pr_rerun_failed >/dev/null 2>&1; then
    snl_pr_rerun_failed "$pr" "$text"
  else
    ids=$(gh run list --repo "$(snl_lane_slug "$lane")" --limit 100 --json databaseId,headSha,status,conclusion --jq '.[] | select(.headSha=="'"$text"'") | select(.status=="completed" and (.conclusion=="failure" or .conclusion=="timed_out")) | .databaseId')
    [ -n "$ids" ] || { echo "PR #$pr has no failed exact-head runs to rerun" >&2; exit 3; }
    while IFS= read -r id; do [ -z "$id" ] || gh run rerun "$id" --failed; done <<EOF
$ids
EOF
  fi
  echo "PR #$pr rerun requested at $text" ;;
ready|ready-check|merge-check) pr=$(snl_task "$lane" pr); base=origin/${BASE:-main}
  git fetch -q origin "$(snl_task "$lane" branch)" "${BASE:-main}" 2>/dev/null || :
  # After orca pr update the PR head is the approved head plus base merges:
  # carry the approval and record the head as snl_gate does; refuse anything else.
  new=$(snl_lane_gh "$lane" pr view "$pr" --json headRefOid --jq .headRefOid 2>/dev/null) || new=$text
  if [ -n "$new" ] && [ "$new" != "$text" ]; then
   snl_only_base_merges "$text" "$new" || { echo "PR head $new is not approved head $text updated from base"; exit 3; }
   python3 "$SW/ledger.py" set "$RUN" --id "$lane" --pr-head "$new" --approval-sha "$new" >/dev/null
   [ "$(sed -n 's/^PASS \([^ ]*\).*/\1/p' "$RUN/$lane.gate" 2>/dev/null)" != "$text" ] || echo "PASS $new" > "$RUN/$lane.gate"
   text=$new
  fi
  snl_merge_claims "$lane" "$text"
  # SKILL.md's fresh-base guard: the oldest base commit missing from the head is at most two hours old.
  ages=$(git log --format=%ct "$text..$base" 2>"$scr") || { snl_head_checks "$lane" "$pr" "$text" || exit $?; echo "base age unknown: git cannot read $text..$base: $(head -c 300 "$scr")"; exit 3; }
  old=$(printf '%s\n' "$ages" | tail -1)
  [ -z "$old" ] || [ $(($(date +%s) - old)) -le 7200 ] ||
   { echo "stale base: the oldest $base commit missing from $text is $((($(date +%s) - old) / 60)) min old (limit 120); orca pr update"; exit 3; }
  if [ "$op" = ready ] || [ "$op" = ready-check ]; then
    head_checks_ignoring_draft || exit $?
  fi
  if [ "$op" = ready-check ]; then
    echo "PR #$pr ready checks passed at $text"
    exit 0
  fi
  if [ "$op" = ready ]; then
    draft=$(snl_lane_gh "$lane" pr view "$pr" --json isDraft --jq .isDraft) || { echo "PR draft state unreadable"; exit 3; }
    if [ "$draft" = true ]; then
      snl_lane_gh "$lane" pr ready "$pr" >/dev/null || { echo "PR #$pr could not be made ready"; exit 3; }
    fi
  fi
  snl_head_checks "$lane" "$pr" "$text" ;;
 lane.hold) [ -e "$RUN/$lane.hold-advance" ] || printf '%s\n' "$text" > "$RUN/$lane.hold-advance"; echo "lane $lane holds advance" ;;
 accept-review-check) accept_review_gate; echo "$head" ;;
 approval|accept-review) pr=$(snl_task "$lane" pr); head=$(snl_report_word "$lane" review HEAD); verdict=$(snl_report_word "$lane" review Verdict)
  case "$verdict" in
  pass) ;;
  needs-swe)
    breaks=$(awk '
      /^Breaks:/ {seen=1; sub(/^Breaks:[[:space:]]*/, ""); if (length($0)) {print; exit}; next}
      seen && /^[[:alnum:] _-]+:/ {exit}
      seen && $0 !~ /^[[:space:]]*$/ {print; exit}
    ' "$RUN/$lane.review.md")
    if [ "$op" = accept-review ] || [ "$op" = accept-review-check ]; then
      [ -n "$breaks" ] || { echo "review needs-swe has no named Breaks"; exit 3; }
      if ! printf '%s' "$head" | grep -Eq '^[0-9a-fA-F]{40}$'; then
        echo "review HEAD is not a full commit sha"
        exit 3
      fi
    else
      [ -z "$breaks" ] || { echo "review needs-swe names Breaks: $breaks"; exit 3; }
    fi
    ;;
  *) echo "review Verdict is '$verdict', not pass"; exit 3 ;;
  esac
  [ "$op" != accept-review ] || [ "$verdict" = needs-swe ] || { echo "review Verdict is '$verdict', not needs-swe"; exit 3; }
  new=$(snl_lane_gh "$lane" pr view "$pr" --json headRefOid --jq .headRefOid) || { echo "PR head unreadable"; exit 3; }
  [ -n "$head" ] && [ "$head" = "$new" ] || { echo "review HEAD ${head:-none} is not PR head ${new:-unknown}"; exit 3; }
  [ "$verdict" != needs-swe ] || snl_head_checks "$lane" "$pr" "$head" >/dev/null
  [ "$op" != accept-review ] || snl_accept_review "$lane" "$text" >/dev/null
  echo "$head" ;;
 pr.approve)
  if [ -n "$accept" ]; then
    # Recheck under the receipt lock, then rewrite only in the live action.
    accept_review_gate
    snl_accept_review "$lane" "$accept" >/dev/null
  fi
  python3 "$SW/ledger.py" set "$RUN" --id "$lane" --pr-head "$text" --approval-sha "$text" >/dev/null; echo "PR #$(snl_task "$lane" pr) approved at $text" ;;
 pr.update) ghx api -X PUT "repos/$(snl_lane_slug "$lane")/pulls/$(snl_task "$lane" pr)/update-branch" -f expected_head_sha="$text" >/dev/null ;;
 pr.merge) [ -e "$RUN/$lane.hold-advance" ] || { snl_merge "$lane"; exit; }
  # A held lane has no gate for snl_merge: merge the head ready passed by hand,
  # pinned, through pr_merge.sh, and record it as snl_merge does.
  pr=$(snl_task "$lane" pr); set --
  for p in swe clean review; do [ ! -f "$RUN/$lane.$p.md" ] || set -- "$@" --report "$RUN/$lane.$p.md"; done
  out=$(bash "$SW/pr_merge.sh" --repo "$(snl_lane_slug "$lane")" --head "$text" "$@" "$pr")
  sha=$(printf '%s\n' "$out" | sed -n "s/^merged $pr \([0-9a-f][0-9a-f]*\)\$/\1/p")
  [ -n "$sha" ] || { echo "PR #$pr merge not confirmed: $out"; exit 1; }
  python3 "$SW/ledger.py" set "$RUN" --id "$lane" --status done --merge-sha "$sha" --pr-head "$text" --approval-sha "$text" --note "merged #$pr by hand" >/dev/null
  echo "$lane: merged #$pr -> $sha" ;;
 ask) "$SNL_NEBULA" mail send --to owner --class fyi --run "$SNL_RUN_ID" --body-file "$msg" ;;
 note) snl_event "$lane" note "$text" ;;
 *) exit 2 ;;
esac`

// SkillDir is runDir's swarm skill scripts directory: explicit (--skill or
// ORCA_SNL_DIR) when given, else the SW the run's env.sh exports, else the
// home skill folder only when env.sh names none. Its error names the paths
// tried.
func SkillDir(ctx context.Context, explicit, runDir string) (string, error) {
	if explicit != "" {
		if !isFile(filepath.Join(explicit, "lane_ops.sh")) {
			return "", refuse("swarm skill scripts unavailable: no lane_ops.sh in --skill %s", explicit)
		}
		return explicit, nil
	}
	envFile := filepath.Join(runDir, "env.sh")
	sw := ""
	if isFile(envFile) {
		// Source it as skillOps does, so an SW set by any shell form is found.
		out, err := exec.CommandContext(ctx, "bash", "-c", `unset SW; . "$1" >/dev/null 2>&1; printf %s "${SW:-}"`, "orca-toolbox", envFile).Output()
		if err != nil {
			return "", refuse("swarm skill scripts unavailable: cannot source %s: %v", envFile, err)
		}
		sw = string(out)
	}
	if sw != "" {
		if !isFile(filepath.Join(sw, "lane_ops.sh")) {
			return "", refuse("swarm skill scripts unavailable: no lane_ops.sh in SW=%s from %s", sw, envFile)
		}
		return sw, nil
	}
	home, _ := os.UserHomeDir()
	fallback := filepath.Join(home, ".claude", "skills", "subagent-swarm-nebula", "scripts")
	if !isFile(filepath.Join(fallback, "lane_ops.sh")) {
		return "", refuse("swarm skill scripts unavailable: %s exports no SW and no lane_ops.sh in %s", envFile, fallback)
	}
	return fallback, nil
}

func skill(ctx context.Context, o options, op string, t *task, text string) (string, error) {
	sw, err := SkillDir(ctx, o.skill, o.dir)
	if err != nil {
		return "", err
	}
	phase, brief := "", ""
	if op == "lane.start" && o.phase != "" {
		phase, brief = o.phase, o.brief
		if brief == "" {
			brief = phaseBriefFor(o.dir, t.ID, o.phase)
		}
	} else if op == "lane.start" && t.Status == "running" {
		phase, brief = t.Phase, phaseBrief(o.dir, t)
	} else if op == "check" {
		phase = t.Phase
	}
	repo, err := checkout(ctx, o.dir)
	if err != nil {
		return "", err
	}
	rework := ""
	if op == "lane.start" && sweRework(o, t) {
		rework = "1"
	}
	cmd := exec.CommandContext(ctx, "bash", "-c", skillOps, "orca-toolbox", sw, o.dir, op, t.ID, t.NebulaSession, t.NebulaTerminal, text, phase, brief, t.Status, strconv.Itoa(max(t.Round, 1)), t.Phase, o.accept, rework)
	cmd.Dir = repo
	raw, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("swarm skill %s failed: %w: %s", op, err, strings.TrimSpace(string(raw)))
	}
	return strings.TrimSpace(string(raw)), nil
}

// checkout is the host's repository checkout for the run, where the skill's
// git helpers run: the worktree of state.json's config.repo, the git common
// dir the skill's ledger records at init. A bare repo or common dir with no
// work tree resolves to its worktree on config.base, else the first listed.
func checkout(ctx context.Context, dir string) (string, error) {
	var s struct{ Config struct{ Repo, Base string } }
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil || json.Unmarshal(raw, &s) != nil || !filepath.IsAbs(s.Config.Repo) {
		return "", refuse("state.json config.repo names no host checkout for run %s", dir)
	}
	repo := s.Config.Repo
	if filepath.Base(repo) == ".git" {
		repo = filepath.Dir(repo)
	}
	if !isDir(repo) {
		return "", refuse("state.json config.repo %s names no host checkout for run %s", repo, dir)
	}
	out, err := exec.CommandContext(ctx, "git", "-C", repo, "rev-parse", "--is-inside-work-tree").Output()
	if err != nil || strings.TrimSpace(string(out)) != "false" {
		return repo, nil
	}
	out, err = exec.CommandContext(ctx, "git", "-C", repo, "worktree", "list", "--porcelain").Output()
	if err != nil {
		return "", refuse("state.json config.repo %s has no work tree and git worktree list failed: %v", repo, err)
	}
	base, first := "refs/heads/"+cmp.Or(s.Config.Base, "main"), ""
	for _, rec := range strings.Split(string(out), "\n\n") {
		var path, branch string
		bare := false
		for _, line := range strings.Split(rec, "\n") {
			if p, ok := strings.CutPrefix(line, "worktree "); ok {
				path = p
			} else if b, ok := strings.CutPrefix(line, "branch "); ok {
				branch = b
			} else if line == "bare" {
				bare = true
			}
		}
		if path == "" || bare || !isDir(path) {
			continue
		}
		if branch == base {
			return path, nil
		}
		if first == "" {
			first = path
		}
	}
	if first == "" {
		return "", refuse("state.json config.repo %s has no work tree and no worktree of it exists", repo)
	}
	return first, nil
}

func isFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}

func isDir(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}

// Heartbeat bounds an orca wait that sees no change.
const Heartbeat = 20 * time.Minute

// SyncManifest is a shadow copy's record of the live evidence its sync copied,
// so the copy's evidence is what the host wrote, never the session's own edits.
const SyncManifest = ".orca-sync.json"

// waitBaseline is orca wait: it compares against the evidence the caller saw
// when its previous wait returned, saved in the run dir across calls.
func waitBaseline(ctx context.Context, dir string) ([]string, error) {
	path := filepath.Join(dir, ".orca-wait.json")
	var before map[string]string
	raw, err := os.ReadFile(path)
	if err == nil {
		err = json.Unmarshal(raw, &before)
	} else if os.IsNotExist(err) {
		if before, err = Snapshot(dir); err == nil {
			err = WriteJSON(path, before)
		}
	}
	if err != nil {
		return nil, err
	}
	after, changed, err := WaitSince(ctx, dir, before)
	if err != nil {
		return nil, err
	}
	return changed, WriteJSON(path, after)
}

// WriteJSON replaces path atomically, so a concurrent reader sees whole JSON.
func WriteJSON(path string, v any) error {
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// WaitSince blocks until run evidence differs from the before snapshot and
// returns the new snapshot with the paths that changed.
func WaitSince(ctx context.Context, dir string, before map[string]string) (map[string]string, []string, error) {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		after, err := Snapshot(dir)
		if err != nil {
			return nil, nil, err
		}
		var changed []string
		for path, stamp := range after {
			if before[path] != stamp {
				changed = append(changed, path)
			}
		}
		for path := range before {
			if _, ok := after[path]; !ok {
				changed = append(changed, path)
			}
		}
		if len(changed) > 0 {
			slices.Sort(changed)
			return after, changed, nil
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, nil, ErrTimeout
			}
			return nil, nil, ctx.Err()
		case <-tick.C:
		}
	}
}

// Snapshot stamps each run evidence file under dir. In a shadow copy the
// sync manifest stands for every file but the inbox the supervisor writes.
func Snapshot(dir string) (map[string]string, error) {
	out := map[string]string{}
	raw, err := os.ReadFile(filepath.Join(dir, SyncManifest))
	synced := err == nil
	if synced {
		err = json.Unmarshal(raw, &out)
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	err = filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
		if os.IsNotExist(err) { // removed mid-sync
			return nil
		}
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		top, _, nested := strings.Cut(rel, string(os.PathSeparator))
		if e.IsDir() && rel != "." && (strings.HasPrefix(e.Name(), ".") || synced) && e.Name() != ".orca-inbox" {
			return filepath.SkipDir
		}
		name := e.Name()
		evidence := name == "EVENTS" || name == "state.json" || name == "heartbeat" ||
			slices.Contains([]string{".md", ".done", ".needs-swe", ".hold-advance", ".signal"}, filepath.Ext(name)) ||
			nested && slices.Contains([]string{".orca-inbox", "reports", "signals", "mail", "chat"}, top)
		if e.IsDir() || !evidence || synced && top != ".orca-inbox" {
			return nil
		}
		info, err := e.Info()
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		out[rel] = fmt.Sprintf("%d:%d", info.Size(), info.ModTime().UnixNano())
		return nil
	})
	return out, err
}

// ReadLeaseAuthority returns the run's current handover, which guarded skill
// calls must still match; a re-adoption or release invalidates it.
func ReadLeaseAuthority(dir string) (string, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ".orca-lease.json"))
	if err != nil {
		return "", err
	}
	var lease struct{ Holder, Marker, State string }
	if err := json.Unmarshal(raw, &lease); err != nil {
		return "", err
	}
	if lease.State != "adopted" || lease.Holder == "" || lease.Marker == "" {
		return "", refuse("adopted lease is not current")
	}
	data, err := json.Marshal(map[string]string{"holder": lease.Holder, "marker": lease.Marker, "state": lease.State})
	return string(data), err
}

// LeaseGuard replaces the skill's stand-down check, after the skill scripts are
// sourced, with one that also requires this run path and exact handover.
const LeaseGuard = `if [ -n "${ORCA_ADOPT_AUTH:-}" ]; then
snl_orca_lease_check() {
  python3 -c 'import json,os,sys
root=os.path.realpath(os.environ["ORCA_TOOLBOX_DIR"]); want=json.loads(os.environ["ORCA_ADOPT_AUTH"])
cur=json.load(open(os.path.join(root,".orca-lease.json")))
sys.exit(os.path.realpath(sys.argv[1])!=root or any(cur.get(k)!=want.get(k) for k in ("holder","marker","state")))' "$1" ||
  { echo "orca: refused: adopted lease is no longer current" >&2; return 2; }
}
fi`
