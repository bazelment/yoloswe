// Package toolbox provides the small, file-backed command surface used by an
// adopted dispatcher.  It deliberately speaks text or JSON and keeps all
// mutations idempotent through an action key recorded beside the run.
package toolbox

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
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
type Options struct {
	Dir       string
	JSON      bool
	Timeout   time.Duration
	Shadow    bool
	IntentDir string
	Script    string
	ActionKey string
}

//nolint:govet // textual ledger projection follows the run JSON shape.
type task struct {
	ID             string                       `json:"id"`
	Title          string                       `json:"title"`
	Status         string                       `json:"status"`
	Phase          string                       `json:"phase"`
	Brief          string                       `json:"brief"`
	Mission        string                       `json:"mission"`
	Description    string                       `json:"description"`
	WaitingOn      string                       `json:"waiting_on"`
	Deadline       any                          `json:"deadline"`
	TerminalState  string                       `json:"terminal_state"`
	NebulaTerminal string                       `json:"nebula_terminal"`
	SessionID      string                       `json:"session_id"`
	PR             int                          `json:"pr"`
	PRHead         string                       `json:"pr_head"`
	MergeSHA       string                       `json:"merge_sha"`
	MergeClaim     string                       `json:"merge_claim"`
	PRState        string                       `json:"pr_state"`
	Checks         string                       `json:"checks"`
	Base           string                       `json:"base"`
	Branch         string                       `json:"branch"`
	Hold           string                       `json:"hold"`
	HoldNote       string                       `json:"hold_note"`
	Target         string                       `json:"target"`
	ApprovalSHA    string                       `json:"approval_sha"`
	Round          int                          `json:"round"`
	Terminals      map[string]map[string]string `json:"terminals"`
	Worktree       string                       `json:"worktree"`
	DependsOn      []string                     `json:"depends_on"`
}

type state struct {
	Config map[string]any `json:"config"`
	Tasks  []task         `json:"tasks"`
}

// helper invokes the installed swarm operation, passing all user input as argv.
// It never manufactures lifecycle state; the skill owns ledger updates.
func helper(ctx context.Context, o Options, op, id, text string) (string, error) {
	script := o.Script
	if script == "" {
		script = os.Getenv("ORCA_TOOLBOX_SCRIPT")
	}
	if script == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		script = filepath.Join(home, ".claude", "skills", "nebula-swarm", "scripts", "swarm_nebula.sh")
	}
	if _, err := os.Stat(script); err != nil {
		return "", refuse("swarm skill script unavailable")
	}
	cmd := exec.CommandContext(ctx, "bash", "-c", skillAdapter, "orca-toolbox", script, o.Dir, op, id, text)
	raw, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("swarm helper %s failed: %w: %s", op, err, strings.TrimSpace(string(raw)))
	}
	return strings.TrimSpace(string(raw)), nil
}

const skillAdapter = `set -e
source "$1"
` + AdoptedLeaseGuard + `
run=$2; op=$3; lane=$4; text=$5
case "$op" in
 start) sw_spawn "$run" "$lane" ;;
 steer)
  note=$(mktemp); trap 'rm -f "$note"' EXIT
  printf '%s' "$text" > "$note"
  sw_nudge "$run" "$lane" "$note" ;;
 stop) tid=$(sw_terminal "$run" "$lane")
  [[ -n "$tid" ]] || { echo "no terminal for $lane" >&2; exit 2; }
  sw_cli term stop "$tid" ;;
 merge) sw_merge "$run" "$lane" ;;
 ask) sw_mail "$run" fyi owner "$text" ;;
 note) sw_event "$run" "$lane" note "$text" ;;
 *) exit 2 ;;
esac`

func load(dir string) (state, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil {
		return state{}, fmt.Errorf("read state.json: %w", err)
	}
	var s state
	if err := json.Unmarshal(raw, &s); err != nil {
		return state{}, fmt.Errorf("parse state.json: %w", err)
	}
	return s, nil
}

func actionKey(dir, key string) string {
	h := sha256.Sum256([]byte(key))
	return filepath.Join(dir, ".orca-actions", hex.EncodeToString(h[:]))
}

func refuse(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrRefused, fmt.Sprintf(format, args...))
}

func taskFor(s state, id string) (*task, error) {
	for i := range s.Tasks {
		if s.Tasks[i].ID == id {
			return &s.Tasks[i], nil
		}
	}
	return nil, refuse("lane %q does not exist", id)
}

func taskBrief(t task) string {
	return strings.TrimSpace(t.Brief + "\n" + t.Mission + "\n" + t.Description)
}

func recordIntent(dir, lane, kind string, args any) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "actions.jsonl"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	// Keep the shadow ledger compatible with adopt.Action.  Reports and the
	// host-diff scorer rely on a timestamp and stable decision identity even
	// when the action is only recorded locally.
	identity := "shadow-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	b, err := json.Marshal(map[string]any{
		"time":        time.Now().UTC().Format(time.RFC3339Nano),
		"lane":        lane,
		"action":      kind,
		"arguments":   args,
		"decision_id": identity,
		"event_id":    identity,
		"reason":      "dispatcher toolbox shadow intent",
		"recorded":    "shadow: recorded",
	})
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return err
}

func output(w io.Writer, jsonMode bool, v any, text string) error {
	if jsonMode {
		b, err := json.Marshal(v)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(w, string(b))
		return err
	}
	_, err := fmt.Fprintln(w, text)
	return err
}

func Run(ctx context.Context, args []string, in io.Reader, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("orca: toolbox command required")
	}
	switch args[0] {
	case "wait":
		return wait(ctx, args[1:], out)
	case "status":
		return status(ctx, args[1:], out)
	case "check":
		return check(ctx, args[1:], out)
	case "lane":
		return laneCommand(ctx, args[1:], out)
	case "pr":
		return prCommand(ctx, args[1:], out)
	case "ask":
		return writeMessage(ctx, args[1:], out, "ask")
	case "note":
		return writeMessage(ctx, args[1:], out, "note")
	default:
		return fmt.Errorf("orca: unknown toolbox command %q", args[0])
	}
}

func parseDir(args []string) (Options, []string, error) {
	o := Options{Dir: os.Getenv("ORCA_TOOLBOX_DIR"), Shadow: os.Getenv("ORCA_TOOLBOX_SHADOW") == "1", IntentDir: os.Getenv("ORCA_TOOLBOX_INTENT_DIR")}
	var rest []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--dir":
			if i+1 >= len(args) {
				return o, nil, errors.New("--dir requires a value")
			}
			o.Dir = args[i+1]
			i++
		case "--json":
			o.JSON = true
		case "--shadow":
			o.Shadow = true
		case "--script", "--action-key":
			if i+1 >= len(args) {
				return o, nil, errors.New(args[i] + " requires a value")
			}
			if args[i] == "--script" {
				o.Script = args[i+1]
			} else {
				o.ActionKey = args[i+1]
			}
			i++
		case "--intent-dir":
			if i+1 >= len(args) {
				return o, nil, errors.New("--intent-dir requires a value")
			}
			o.IntentDir = args[i+1]
			i++
		case "--timeout":
			if i+1 >= len(args) {
				return o, nil, errors.New("--timeout requires a value")
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil {
				return o, nil, err
			}
			o.Timeout = d
			i++
		default:
			rest = append(rest, args[i])
		}
	}
	if strings.TrimSpace(o.Dir) == "" {
		o.Dir = "."
	}
	if bound := os.Getenv("ORCA_TOOLBOX_DIR"); bound != "" {
		want, err := filepath.Abs(bound)
		if err != nil {
			return o, nil, err
		}
		got, err := filepath.Abs(o.Dir)
		if err != nil {
			return o, nil, err
		}
		if got != want {
			return o, nil, refuse("session toolbox is bound to its run workspace")
		}
	}
	if os.Getenv("ORCA_TOOLBOX_SHADOW") == "1" {
		o.Shadow = true
		o.IntentDir = os.Getenv("ORCA_TOOLBOX_INTENT_DIR")
	}
	if o.Shadow {
		dir, dirErr := filepath.Abs(o.Dir)
		intent, intentErr := filepath.Abs(o.IntentDir)
		if dirErr != nil || intentErr != nil || o.IntentDir == "" || dir == intent || strings.HasPrefix(intent, dir+string(os.PathSeparator)) {
			return o, nil, refuse("shadow intent directory must be outside the run workspace")
		}
	}
	return o, rest, nil
}

func status(ctx context.Context, args []string, out io.Writer) error {
	o, _, err := parseDir(args)
	if err != nil {
		return err
	}
	s, err := load(o.Dir)
	if err != nil {
		return err
	}
	rows := make([]map[string]any, 0, len(s.Tasks))
	for i := range s.Tasks {
		t := &s.Tasks[i]
		rows = append(rows, map[string]any{"lane": t.ID, "phase": t.Phase, "status": t.Status, "terminal": t.TerminalState, "pr": t.PR, "pr_head": t.PRHead, "checks": t.Checks, "wait": t.WaitingOn})
	}
	return output(out, o.JSON, rows, formatRows(rows))
}

func formatRows(rows []map[string]any) string {
	var b strings.Builder
	for _, r := range rows {
		fmt.Fprintf(&b, "lane %v: phase=%v status=%v terminal=%v pr=%v checks=%v wait=%v\n", r["lane"], r["phase"], r["status"], r["terminal"], r["pr"], r["checks"], r["wait"])
	}
	return strings.TrimRight(b.String(), "\n")
}

func check(ctx context.Context, args []string, out io.Writer) error {
	o, rest, err := parseDir(args)
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return errors.New("orca check requires a lane id")
	}
	s, err := load(o.Dir)
	if err != nil {
		return err
	}
	t, err := taskFor(s, rest[0])
	if err != nil {
		return err
	}
	v := inspectLane(ctx, *t)
	return output(out, o.JSON, v, fmt.Sprint(v))
}

func laneCommand(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("orca lane requires start, steer or stop")
	}
	op := args[0]
	o, rest, err := parseDir(args[1:])
	if err != nil {
		return err
	}
	if len(rest) < 1 {
		return errors.New("orca lane command requires a lane id")
	}
	id := rest[0]
	if id == "." || id == ".." || filepath.Base(id) != id || strings.ContainsAny(id, "\\\x00") {
		return refuse("invalid lane id")
	}
	key := o.ActionKey
	if key == "" {
		key = "lane:" + op + ":" + id + ":" + strings.Join(rest[1:], " ")
	}
	return mutate(ctx, o, "lane."+op, id, key, strings.Join(rest[1:], " "), out)
}

func validateLane(ctx context.Context, o Options, op, id string) (*task, error) {
	s, err := load(o.Dir)
	if err != nil {
		return nil, err
	}
	t, err := taskFor(s, id)
	if err != nil {
		return nil, err
	}
	if t.Status == "done" || t.Status == "cancelled" || t.MergeSHA != "" || strings.EqualFold(t.PRState, "merged") {
		return nil, refuse("lane %s is done or merged", id)
	}
	if t.Hold != "" || t.HoldNote != "" {
		return nil, refuse("lane %s is held: %s %s", id, t.Hold, t.HoldNote)
	}
	if _, err := os.Stat(filepath.Join(o.Dir, id+".hold-advance")); err == nil {
		return nil, refuse("lane %s is held", id)
	}
	switch op {
	case "start":
		if t.Status != "planned" || t.SessionID != "" || currentTerminal(*t) != "" {
			return nil, refuse("lane %s is already started or not planned", id)
		}
		if taskBrief(*t) == "" {
			return nil, refuse("lane %s has no brief", id)
		}
		if strings.HasSuffix(strings.TrimSpace(t.Brief), ".brief") {
			name := strings.TrimSpace(t.Brief)
			path, err := filepath.EvalSymlinks(filepath.Join(o.Dir, name))
			root, rootErr := filepath.EvalSymlinks(o.Dir)
			if err != nil || rootErr != nil || !strings.HasPrefix(path, root+string(os.PathSeparator)) {
				return nil, refuse("lane %s brief file is missing or escapes the run", id)
			}
			info, err := os.Stat(path)
			if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
				return nil, refuse("lane %s brief file is missing or empty", id)
			}
		}
		if t.WaitingOn != "" {
			return nil, refuse("lane %s is held", id)
		}
		for _, dependency := range t.DependsOn {
			dep, err := taskFor(s, dependency)
			if err != nil {
				return nil, err
			}
			if dep.Status != "done" {
				return nil, refuse("lane %s depends on unfinished lane %s", id, dependency)
			}
		}
	case "steer", "stop":
		if t.WaitingOn != "" {
			if eventMentions(o.Dir, id, "resolved") {
				return nil, refuse("lane %s wait was already resolved by host EVENTS", id)
			}
			return nil, refuse("lane %s is held waiting on %s", id, t.WaitingOn)
		}
		state, err := liveTerminal(ctx, *t)
		if err != nil {
			return nil, err
		}
		if state != "working" && state != "waiting" {
			return nil, refuse("lane %s current terminal is not alive", id)
		}
	default:
		return nil, refuse("unknown lane action %s", op)
	}
	return t, nil
}

// mutate serializes receipt creation and the safety read across toolbox
// processes. A pending receipt refuses uncertain retries after a crash rather
// than duplicating an external side effect.
func mutate(ctx context.Context, o Options, op, id, key, message string, out io.Writer) error {
	dir := o.Dir
	if o.Shadow {
		dir = o.IntentDir
	}
	if dir == "" {
		return refuse("shadow requires an intent directory")
	}
	if err := os.MkdirAll(filepath.Join(dir, ".orca-actions"), 0700); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, ".orca-actions", "lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	fingerprint := op + "\x00" + id + "\x00" + message
	if raw, err := os.ReadFile(actionKey(dir, key)); err == nil {
		if string(raw) == "done\n"+fingerprint {
			return output(out, o.JSON, map[string]any{"action": op, "result": "already recorded"}, "already recorded")
		}
		if string(raw) == "pending\n"+fingerprint {
			return refuse("action %s has uncertain effects; inspect before issuing a new action key", key)
		}
		return refuse("action key %s was already used with different arguments", key)
	} else if !os.IsNotExist(err) {
		return err
	}
	args := map[string]string{"action_key": key, "text": message}
	if strings.HasPrefix(op, "lane.") {
		t, err := validateLane(ctx, o, strings.TrimPrefix(op, "lane."), id)
		if err != nil {
			return err
		}
		args["phase"], args["terminal_id"] = t.Phase, currentTerminal(*t)
	}
	if err := os.WriteFile(actionKey(dir, key), []byte("pending\n"+fingerprint), 0600); err != nil {
		return err
	}
	var result string
	if o.Shadow {
		err = recordIntent(dir, id, op, args)
		result = "shadow: recorded"
	} else {
		result, err = helper(ctx, o, strings.TrimPrefix(op, "lane."), id, message)
	}
	if err != nil {
		return err
	}
	if err := os.WriteFile(actionKey(dir, key), []byte("done\n"+fingerprint), 0600); err != nil {
		return err
	}
	return output(out, o.JSON, map[string]any{"action": op, "result": result}, result)
}

func eventMentions(dir, lane, word string) bool {
	f, err := os.Open(filepath.Join(dir, "EVENTS"))
	if err != nil {
		return false
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		x := strings.ToLower(sc.Text())
		if strings.Contains(x, "\t"+strings.ToLower(lane)+"\t") && strings.Contains(x, strings.ToLower(word)) {
			return true
		}
	}
	return false
}

func prCommand(ctx context.Context, args []string, out io.Writer) error {
	if len(args) == 0 {
		return errors.New("orca pr requires ready or merge")
	}
	op := args[0]
	if op != "ready" && op != "merge" {
		return fmt.Errorf("orca pr: unknown action %q", op)
	}
	o, rest, err := parseDir(args[1:])
	if err != nil {
		return err
	}
	if len(rest) != 1 {
		return errors.New("orca pr command requires a pull request number")
	}
	n, err := strconv.Atoi(rest[0])
	if err != nil {
		return err
	}
	s, err := load(o.Dir)
	if err != nil {
		return err
	}
	var t *task
	for i := range s.Tasks {
		if s.Tasks[i].PR == n {
			t = &s.Tasks[i]
			break
		}
	}
	if t == nil {
		return refuse("pull request #%d is not in this run", n)
	}
	v := readyPR(ctx, o, s, *t)
	if op == "ready" {
		if err := output(out, o.JSON, v, readinessText(v)); err != nil {
			return err
		}
		if !v.Ready {
			return refuse("PR #%d is not ready", n)
		}
		return nil
	}
	if !v.Ready {
		return refuse("PR #%d is not ready", n)
	}
	return mutate(ctx, o, "merge", t.ID, "pr.merge:"+rest[0]+":"+t.PRHead, "", out)
}

func writeMessage(ctx context.Context, args []string, out io.Writer, kind string) error {
	o, rest, err := parseDir(args)
	if err != nil {
		return err
	}
	if len(rest) == 0 {
		return errors.New("message text is required")
	}
	text := strings.Join(rest, " ")
	key := o.ActionKey
	if key == "" {
		key = kind + ":" + text
	}
	return mutate(ctx, o, kind, "run", key, text, out)
}

func wait(ctx context.Context, args []string, out io.Writer) error {
	o, _, err := parseDir(args)
	if err != nil {
		return err
	}
	before, err := snapshot(o.Dir)
	if err != nil {
		return err
	}
	if o.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.Timeout)
		defer cancel()
	}
	changed, err := waitChanged(ctx, o.Dir, before)
	if err != nil {
		return err
	}
	return output(out, o.JSON, map[string]any{"changed": changed}, "changed: "+strings.Join(changed, ", "))
}

func waitChanged(ctx context.Context, dir string, before map[string]string) ([]string, error) {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		after, err := snapshot(dir)
		if err != nil {
			return nil, err
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
			sort.Strings(changed)
			return changed, nil
		}
		select {
		case <-ctx.Done():
			if errors.Is(ctx.Err(), context.DeadlineExceeded) {
				return nil, ErrTimeout
			}
			return nil, ctx.Err()
		case <-tick.C:
		}
	}
}

func snapshot(dir string) (map[string]string, error) {
	out := make(map[string]string)
	err := filepath.WalkDir(dir, func(p string, e os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		if e.IsDir() {
			if strings.HasPrefix(e.Name(), ".orca") && e.Name() != ".orca-inbox" || e.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		name := e.Name()
		evidence := strings.HasPrefix(rel, ".orca-inbox/") || name == "EVENTS" || name == "state.json" || name == "heartbeat" || strings.HasSuffix(name, ".md") || strings.HasSuffix(name, ".done") || strings.HasSuffix(name, ".needs-swe") || strings.HasSuffix(name, ".hold-advance") || strings.HasSuffix(name, ".signal") || strings.HasPrefix(rel, "reports/") || strings.HasPrefix(rel, "signals/") || strings.HasPrefix(rel, "mail/") || strings.HasPrefix(rel, "chat/")
		if !evidence {
			return nil
		}
		i, err := e.Info()
		if err != nil {
			return err
		}
		out[rel] = fmt.Sprintf("%d:%d", i.Size(), i.ModTime().UnixNano())
		return nil
	})
	return out, err
}

// Wait blocks on the same evidence as the CLI, with no model or network calls.
func Wait(ctx context.Context, dir string) ([]string, error) {
	before, err := snapshot(dir)
	if err != nil {
		return nil, err
	}
	return waitChanged(ctx, dir, before)
}
