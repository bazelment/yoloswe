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
	dir, intent, skill, key string
	shadow                  bool
	timeout                 time.Duration
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
}

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
	if cmd == "wait" {
		ctx, cancel := context.WithTimeout(ctx, cmp.Or(o.timeout, Heartbeat))
		defer cancel()
		changed, err := waitBaseline(ctx, o.dir)
		if err == nil {
			_, err = fmt.Fprintln(out, "changed: "+strings.Join(changed, ", "))
		}
		return err
	}
	if cmd == "ask" || cmd == "note" {
		if len(rest) == 0 {
			return errors.New("message text is required")
		}
		return mutate(ctx, o, cmd, &task{ID: "run"}, strings.Join(rest, " "), out)
	}
	tasks, err := load(o.dir)
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
	if cmd == "pr.ready" || cmd == "pr.merge" {
		match = func(t task) bool { return t.PR > 0 && strconv.Itoa(t.PR) == rest[0] }
	}
	i := slices.IndexFunc(tasks, match)
	if i < 0 {
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
		return mutate(ctx, o, cmd, t, strings.Join(rest[1:], " "), out)
	case "pr.ready", "pr.merge":
		if err := validate(ctx, o, cmd, t, tasks); err != nil {
			return err
		}
		// Exact head: the ledger's head and approval agree, and the skill's
		// claim and check gates then verify that head against the live PR.
		if t.PRHead == "" || t.ApprovalSHA != t.PRHead {
			return refuse("PR #%d approval %q does not match ledger head %q", t.PR, t.ApprovalSHA, t.PRHead)
		}
		if _, err := skill(ctx, o, "ready", t, t.PRHead); err != nil {
			return refuse("PR #%d is not ready at %s: %v", t.PR, t.PRHead, err)
		}
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
	if bound := os.Getenv("ORCA_TOOLBOX_DIR"); bound != "" && !(inside(bound, o.dir) && inside(o.dir, bound)) {
		return o, nil, refuse("session toolbox is bound to its run workspace")
	}
	if o.shadow && (o.intent == "" || inside(o.dir, o.intent)) {
		return o, nil, refuse("shadow intent directory must be outside the run workspace")
	}
	if o.skill == "" {
		home, _ := os.UserHomeDir()
		o.skill = filepath.Join(home, ".claude", "skills", "subagent-swarm-nebula", "scripts")
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
	var s struct{ Tasks []task }
	if err := json.Unmarshal(raw, &s); err != nil {
		return nil, fmt.Errorf("parse state.json: %w", err)
	}
	return s.Tasks, nil
}

// validate holds the refusals: host holds, waits and finished lanes win over
// any dispatcher action, and a live action needs a live current terminal.
func validate(ctx context.Context, o options, op string, t *task, tasks []task) error {
	if t.Status == "done" || t.Status == "cancelled" || t.MergeSHA != "" || strings.EqualFold(t.PRState, "merged") {
		return refuse("lane %s is done or merged", t.ID)
	}
	if _, err := os.Stat(filepath.Join(o.dir, t.ID+".hold-advance")); err == nil || t.Hold+t.HoldNote+t.WaitingOn != "" {
		return refuse("lane %s is held %s", t.ID, strings.TrimSpace(t.Hold+" "+t.HoldNote+" "+t.WaitingOn))
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
			if state, err := skill(ctx, o, "state", t, ""); err != nil || state == "working" || state == "waiting" {
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
		if state, err := skill(ctx, o, "state", t, ""); err != nil || (state != "working" && state != "waiting") {
			return refuse("lane %s current terminal is not alive (%s)", t.ID, cmp.Or(state, "unknown"))
		}
	}
	return nil
}

// runFile reports whether name is a non-empty regular file inside the run.
func runFile(dir, name string) bool {
	path, err := filepath.EvalSymlinks(filepath.Join(dir, name))
	if err != nil || !inside(dir, path) {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 0
}

// phaseBrief is the run-relative brief the skill stored for the lane's
// current phase: the copy snl_spawn keeps, else the written phase brief.
func phaseBrief(dir string, t *task) string {
	if t.Phase == "" {
		return ""
	}
	for _, name := range []string{t.ID + "." + t.Phase + ".brief.txt", filepath.Join("briefs", t.ID+"."+t.Phase+".md")} {
		if runFile(dir, name) {
			return name
		}
	}
	return ""
}

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
	sum := sha256.Sum256([]byte(cmp.Or(o.key, fingerprint)))
	receipt := filepath.Join(actions, hex.EncodeToString(sum[:]))
	switch raw, err := os.ReadFile(receipt); {
	case err == nil && string(raw) == "done\n"+fingerprint:
		_, err = fmt.Fprintln(out, "already recorded")
		return err
	case err == nil && string(raw) == "pending\n"+fingerprint:
		return refuse("action %s has uncertain effects; inspect before issuing a new action key", op)
	case err == nil:
		return refuse("action key %q was already used with different arguments", o.key)
	case !os.IsNotExist(err):
		return err
	}
	if t.ID != "run" { // recheck under the lock against the current ledger
		tasks, err := load(o.dir)
		if err != nil {
			return err
		}
		if i := slices.IndexFunc(tasks, func(c task) bool { return c.ID == t.ID }); i >= 0 {
			t = &tasks[i]
		}
		if err := validate(ctx, o, op, t, tasks); err != nil {
			return err
		}
	}
	if err := os.WriteFile(receipt, []byte("pending\n"+fingerprint), 0o600); err != nil {
		return err
	}
	result := "shadow: recorded"
	if o.shadow {
		err = recordIntent(dir, op, t, text, o.key)
	} else {
		result, err = skill(ctx, o, op, t, text)
	}
	if err == nil {
		err = os.WriteFile(receipt, []byte("done\n"+fingerprint), 0o600)
	}
	if err == nil {
		_, err = fmt.Fprintln(out, result)
	}
	return err
}

// recordIntent appends a shadow action in the adopt.Action shape that the
// shadow reports and host-diff scorer read.
func recordIntent(dir, op string, t *task, text, key string) error {
	id := "shadow-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	b, err := json.Marshal(map[string]any{
		"time": time.Now().UTC().Format(time.RFC3339Nano), "lane": t.ID, "action": op,
		"arguments":   map[string]string{"action_key": key, "text": text, "phase": t.Phase, "terminal_id": t.NebulaTerminal},
		"decision_id": id, "event_id": id, "reason": "dispatcher toolbox shadow intent",
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

// skillOps maps toolbox operations onto the subagent-swarm-nebula scripts.
const skillOps = `set -eu
SW=$1; RUN=$2; op=$3; lane=$4; sid=$5; tid=$6; text=$7; phase=$8; brief=$9
run=$RUN; [ ! -r "$RUN/env.sh" ] || . "$RUN/env.sh"
# env.sh names the live run; a shadow copy's skill calls stay in the copy.
export RUN=$run
export SNL_RUN_ID=${SNL_RUN_ID:-$(basename "$RUN")}
. "$SW/swarm_nebula_lanes.sh"; . "$SW/lane_ops.sh"; . "$SW/step.sh"
` + LeaseGuard + `
msg=$(mktemp); trap 'rm -f "$msg"' EXIT; printf '%s\n' "$text" > "$msg"
case $op in
 state) snl_terminal_agent_state "$sid" "$tid" ;;
 check) echo "terminal=$(snl_terminal_agent_state "$sid" "$tid") remote=$(snl_lane_remote_head "$lane")"
  if snl_sandbox_busy "$lane"; then echo "busy=yes"; else echo "busy=no"; fi ;;
 lane.start) if [ -n "$phase" ]; then snl_spawn "$lane" "$phase" "$RUN/$brief"; else snl_spawn "$lane"; fi ;;
 lane.steer) snl_nb term send "$tid" --file "$msg" ;;
 lane.stop) snl_nb term stop "$tid" ;;
 ready) snl_merge_claims "$lane" "$text"; snl_head_checks "$lane" "$(snl_task "$lane" pr)" "$text" ;;
 pr.merge) snl_merge "$lane" ;;
 ask) "$SNL_NEBULA" mail send --to owner --class fyi --run "$SNL_RUN_ID" --body-file "$msg" ;;
 note) snl_event "$lane" note "$text" ;;
 *) exit 2 ;;
esac`

func skill(ctx context.Context, o options, op string, t *task, text string) (string, error) {
	if _, err := os.Stat(filepath.Join(o.skill, "lane_ops.sh")); err != nil {
		return "", refuse("swarm skill scripts unavailable in %s", o.skill)
	}
	phase, brief := "", ""
	if op == "lane.start" && t.Status == "running" {
		phase, brief = t.Phase, phaseBrief(o.dir, t)
	}
	cmd := exec.CommandContext(ctx, "bash", "-c", skillOps, "orca-toolbox", o.skill, o.dir, op, t.ID, t.NebulaSession, t.NebulaTerminal, text, phase, brief)
	cmd.Dir = checkout(o.dir)
	raw, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("swarm skill %s failed: %w: %s", op, err, strings.TrimSpace(string(raw)))
	}
	return strings.TrimSpace(string(raw)), nil
}

// checkout is the host's repository checkout for the run, where the skill's
// git helpers run: the worktree of state.json's config.repo, the git common
// dir the skill's ledger records at init. Empty keeps the caller's cwd.
func checkout(dir string) string {
	var s struct{ Config struct{ Repo string } }
	raw, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil || json.Unmarshal(raw, &s) != nil || !filepath.IsAbs(s.Config.Repo) {
		return ""
	}
	repo := s.Config.Repo
	if filepath.Base(repo) == ".git" {
		repo = filepath.Dir(repo)
	}
	if info, err := os.Stat(repo); err != nil || !info.IsDir() {
		return ""
	}
	return repo
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
