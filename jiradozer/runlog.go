package jiradozer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// RunsRoot holds one directory per task execution.
//
// It lives OUTSIDE any worktree so reclaiming the ephemeral worktree never
// takes the record with it: the worktree is disposable, the record of what
// happened is not. It is also outside the log directory, because logs rotate
// and this must not.
const RunsRoot = "~/.jiradozer/runs"

// RunState is the lifecycle state of one task execution.
type RunState string

const (
	// RunStateRunning is written at start, before any tracker mutation, so a
	// run that dies without ever updating is still discoverable.
	RunStateRunning   RunState = "running"
	RunStateDone      RunState = "done"
	RunStateFailed    RunState = "failed"
	RunStateCancelled RunState = "cancelled"
)

// IsTerminal reports whether the run has stopped.
func (s RunState) IsTerminal() bool {
	return s == RunStateDone || s == RunStateFailed || s == RunStateCancelled
}

// HeartbeatInterval is how often a live run refreshes HeartbeatAt.
//
// Meta is rewritten in full each time, which is a few hundred bytes — cheap
// enough to do often, and often is what makes the signal useful: the staleness
// threshold a reader applies can only be as tight as this interval.
const HeartbeatInterval = 30 * time.Second

// RunMeta is the machine-readable summary of one task execution, written at
// start and kept current for the run's lifetime. A dispatcher agent can
// classify every run on a box without parsing a single log line.
//
//nolint:govet // fieldalignment: JSON shape is grouped by concern, not by size.
type RunMeta struct {
	StartedAt time.Time `json:"started_at"`
	// HeartbeatAt is refreshed every HeartbeatInterval while the run lives.
	//
	// This is the field that makes State meaningful. A SIGKILL, an OOM, a host
	// reboot, or a deallocated devbox never runs a deferred Finish, so State
	// stays "running" forever — indistinguishable from a healthy long run
	// without a clock to compare it against. Read staleness, not State, to
	// answer "is this alive".
	HeartbeatAt time.Time `json:"heartbeat_at"`
	EndedAt     time.Time `json:"ended_at,omitempty"`

	RunID string `json:"run_id"`
	Host  string `json:"host"`
	PID   int    `json:"pid"`
	// TmuxSession is set when the run was dispatched into a tmux session, so a
	// reader can attach to it. Empty for an in-process run.
	TmuxSession string `json:"tmux_session,omitempty"`

	// IssueIdentifier is the human-readable key ("INF-1234"). Empty for a
	// --description run, where TaskID carries the correlation instead.
	IssueIdentifier string `json:"issue_identifier,omitempty"`
	IssueID         string `json:"issue_id,omitempty"`
	IssueURL        string `json:"issue_url,omitempty"`
	// TaskID correlates a run back to the dispatcher's task list. Local-tracker
	// issue IDs are numbered per directory and therefore collide across hosts,
	// so they cannot serve this purpose.
	TaskID      string `json:"task_id,omitempty"`
	Description string `json:"description,omitempty"`

	// LeaseTarget is the flock lock this run actually holds. It is recorded
	// rather than re-derived because Target() and the lease can legitimately
	// disagree: a --description run leases a name derived from the description
	// (the dispatcher must compute it before the worker exists) and only later
	// acquires a local-tracker identifier, which is what Target() then reports.
	// Anything asking "is a worker still alive on this task" must use THIS —
	// asking about the wrong lock name silently answers "no".
	LeaseTarget string `json:"lease_target,omitempty"`

	Repo         string `json:"repo"`
	WTRoot       string `json:"wt_root,omitempty"`
	Branch       string `json:"branch,omitempty"`
	BaseBranch   string `json:"base_branch,omitempty"`
	WorktreePath string `json:"worktree_path,omitempty"`

	// PRURL is what a sweeper keys off to decide a worktree is reclaimable.
	// Run state cannot serve that purpose: this tool's terminal state is "a PR
	// is open", never "it merged", so state alone would never authorise
	// cleanup.
	PRURL    string `json:"pr_url,omitempty"`
	PRNumber int    `json:"pr_number,omitempty"`

	State RunState `json:"state"`
	// Phase is the last workflow step observed, for a human reading a listing.
	Phase string `json:"phase,omitempty"`

	LogPath string `json:"log_path,omitempty"`
	LogDir  string `json:"log_dir"`

	// WorktreeKept records that the worktree was deliberately left in place, so
	// its disk cost is never silent.
	WorktreeKept       bool   `json:"worktree_kept,omitempty"`
	WorktreeKeptReason string `json:"worktree_kept_reason,omitempty"`
	Note               string `json:"note,omitempty"`
	Error              string `json:"error,omitempty"`
}

// Target returns the best human label for this run.
func (m RunMeta) Target() string {
	if m.IssueIdentifier != "" {
		return m.IssueIdentifier
	}
	if m.TaskID != "" {
		return m.TaskID
	}
	return m.RunID
}

// RemoverKey identifies the worktree manager that owns this run's checkout.
//
// Repo alone is not enough: the root can differ between runs (WT_ROOT changes,
// or historical runs made under another root), and a manager built on the wrong
// root looks in a directory the worktree was never in — so gc would silently
// never reclaim it.
//
// It keys on EffectiveWTRoot, not the raw WTRoot field, because that is the
// root the manager is actually built on. Keying on the raw field would collapse
// every pre-wt_root record for a repo into one bucket regardless of the
// different historical roots their paths name, and the first one seen would
// hand its manager to all the rest — reintroducing the wrong-tree lookup from
// the other direction. Records that can name no root share the empty key, which
// is correct: they all resolve through the same ambient fallback.
func (m RunMeta) RemoverKey() string {
	return m.EffectiveWTRoot() + "\x00" + m.Repo
}

// EffectiveWTRoot returns the worktree root this run's checkout actually lives
// under, or "" when the record does not pin one.
//
// WTRoot is authoritative when recorded. Records written before it existed have
// it empty, and for those WorktreePath still answers the question: worktrees are
// laid out as <root>/<repo>/<branch>, so trimming the repo and branch off the
// recorded path recovers the root the run was created under. That matters
// because the alternative — falling through to the sweeper's ambient WT_ROOT —
// is only right when the root never moved. After a migration it points gc at a
// tree those worktrees were never in, and they are never reclaimed.
//
// Returning "" means "this record cannot say"; the caller decides what to
// assume, which is the one case where the ambient root is the best guess left.
func (m RunMeta) EffectiveWTRoot() string {
	if m.WTRoot != "" {
		return m.WTRoot
	}
	if m.WorktreePath == "" || m.Repo == "" || m.Branch == "" {
		return ""
	}
	// Join first so a branch containing slashes ("feature/x") is matched whole.
	suffix := string(filepath.Separator) + filepath.Join(m.Repo, m.Branch)
	root := strings.TrimSuffix(filepath.Clean(m.WorktreePath), suffix)
	if root == filepath.Clean(m.WorktreePath) || root == "" {
		return "" // not the layout we build; the path tells us nothing
	}
	return root
}

// LeaseKey names the lock this run holds, for a liveness check.
//
// Returning "" means "this record cannot say", the same convention
// EffectiveWTRoot uses — and here the caller has exactly one safe reading of
// it: refuse to reclaim. Guessing is worse than not answering, because a lock
// name that was never taken answers "not held", which reads as permission to
// delete a live worker's checkout.
//
// In practice nothing reaches the fallbacks: exec is the only writer of these
// records, it records LeaseTarget on every run, and TestEveryRunShapeRecordsALeaseName
// holds it to that for both run shapes. A record without one is therefore a
// damaged record — a truncated write, a hand-edit, or a leftover from a build
// that predates the field, none of which are shapes to reclaim disk on faith.
// So the fallbacks are salvage, not compatibility, and they follow the same
// precedence the lease name is derived by, because the only useful salvage is
// one that reconstructs the name actually taken.
//
// Target() is deliberately NOT among them. For a --description run it reports a
// local-tracker identifier or the run id, while the lock was named for the
// description — so Target() hands back a name no lock ever had, and asking
// about it answers "free" about a directory a worker is still using. Refusing
// to answer costs a stranded checkout that `jiradozer gc` names, with its
// reason, on every sweep; answering wrongly costs a live worker's work.
func (m RunMeta) LeaseKey() string {
	if m.LeaseTarget != "" {
		return m.LeaseTarget
	}
	if m.Description != "" {
		// A --description run: the lock is a hash of the description, unless
		// the dispatcher named the task, in which case the task id won. The
		// hash is not reproduced here on purpose — a second copy of the
		// derivation is a second thing to drift, and this branch exists only
		// for records the writer never produced.
		return m.TaskID
	}
	if m.IssueIdentifier != "" {
		return m.IssueIdentifier
	}
	return m.TaskID
}

// Matches reports whether this run answers to name — its issue identifier, its
// task id, or the lease the dispatcher named it by. All three are printed to
// operators at some point, so all three have to find the run again.
func (m RunMeta) Matches(name string) bool {
	return name == m.IssueIdentifier || name == m.TaskID || name == m.LeaseTarget
}

// StaleFor reports how far past its expected heartbeat a non-terminal run is,
// or 0 when it is terminal or beating on time. Two missed beats is the
// threshold: one can be lost to a slow write or a paused VM without the run
// being dead.
func (m RunMeta) StaleFor(now time.Time) time.Duration {
	if m.State.IsTerminal() || m.HeartbeatAt.IsZero() {
		return 0
	}
	gap := now.Sub(m.HeartbeatAt)
	if gap <= 2*HeartbeatInterval {
		return 0
	}
	return gap
}

// NewRunID returns a short random identifier distinguishing concurrent runs on
// the same target — including a re-dispatch of an issue whose previous run left
// a directory behind.
func NewRunID() (string, error) {
	var b [3]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("generate run id: %w", err)
	}
	return hex.EncodeToString(b[:]), nil
}

// RunLog owns one run's directory: meta.json and the append-only events.jsonl.
type RunLog struct {
	dir  string
	meta RunMeta
	mu   sync.Mutex
}

// sanitizeForRunDir keeps an identifier usable as a single path segment.
// GitHub-style identifiers ("acme/app#42") contain separators that must not
// become directories.
func sanitizeForRunDir(s string) string {
	r := strings.NewReplacer("/", "-", string(filepath.Separator), "-", "#", "-", " ", "-")
	return r.Replace(s)
}

// RunDirFor returns the directory for a target/run pair.
func RunDirFor(target, runID string) string {
	return filepath.Join(ExpandHome(RunsRoot), fmt.Sprintf("%s-%s", sanitizeForRunDir(target), runID))
}

// NewRunLog creates the run directory and writes the initial meta.json.
func NewRunLog(meta RunMeta) (*RunLog, error) {
	dir := RunDirFor(meta.Target(), meta.RunID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create run dir: %w", err)
	}
	meta.LogDir = dir
	if meta.State == "" {
		meta.State = RunStateRunning
	}
	now := time.Now().UTC()
	if meta.StartedAt.IsZero() {
		meta.StartedAt = now
	}
	meta.HeartbeatAt = now
	if meta.Host == "" {
		meta.Host, _ = os.Hostname()
	}
	if meta.PID == 0 {
		meta.PID = os.Getpid()
	}
	rl := &RunLog{dir: dir, meta: meta}
	if err := rl.writeMeta(); err != nil {
		return nil, err
	}
	return rl, nil
}

// Dir returns the run's directory. It outlives the worktree.
func (r *RunLog) Dir() string { return r.dir }

// Meta returns a copy of the current metadata.
func (r *RunLog) Meta() RunMeta {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.meta
}

// writeMeta persists meta.json via temp-file + rename, so a reader on another
// process (or over ssh) never observes a half-written file. Caller holds the
// lock, except in NewRunLog where no other reference exists yet.
func (r *RunLog) writeMeta() error {
	data, err := json.MarshalIndent(r.meta, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal run meta: %w", err)
	}
	path := filepath.Join(r.dir, "meta.json")
	tmp, err := os.CreateTemp(r.dir, "meta.*.tmp")
	if err != nil {
		return fmt.Errorf("create run meta temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("write run meta: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close run meta: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename run meta: %w", err)
	}
	return nil
}

// UpdateMeta applies fn to the metadata and rewrites meta.json. It also
// refreshes the heartbeat: any deliberate update is itself proof of life.
func (r *RunLog) UpdateMeta(fn func(*RunMeta)) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	fn(&r.meta)
	if !r.meta.State.IsTerminal() {
		r.meta.HeartbeatAt = time.Now().UTC()
	}
	return r.writeMeta()
}

// Heartbeat refreshes HeartbeatAt without changing anything else.
func (r *RunLog) Heartbeat() error {
	return r.UpdateMeta(func(*RunMeta) {})
}

// StartHeartbeat beats until ctx ends, and returns a stop function that waits
// for the goroutine to exit. Failures are returned to onErr rather than
// swallowed, but a caller should log-and-continue: losing a beat must not kill
// a run that is otherwise healthy — it only makes the run look stale, which is
// the safe direction to fail.
func (r *RunLog) StartHeartbeat(ctx context.Context, interval time.Duration, onErr func(error)) (stop func()) {
	if interval <= 0 {
		interval = HeartbeatInterval
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := r.Heartbeat(); err != nil && onErr != nil {
					onErr(err)
				}
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() { <-done })
	}
}

// Finish records the terminal state and end time. After this the heartbeat is
// frozen deliberately: a terminal run is not supposed to look alive.
func (r *RunLog) Finish(state RunState, note string, runErr error) error {
	return r.UpdateMeta(func(m *RunMeta) {
		m.State = state
		m.EndedAt = time.Now().UTC()
		if note != "" {
			m.Note = note
		}
		if runErr != nil {
			m.Error = runErr.Error()
		}
	})
}

// Event is one line of the append-only audit trail.
//
// The trail is what meta.json cannot be: meta is a snapshot, its Phase
// overwritten on every transition and its State on every settle, so the
// sequence a run passed through — and how long each step took — survives
// nowhere else. A run that planned, built and then failed in ship is
// indistinguishable in meta.json from one that failed in ship immediately.
type Event struct {
	At     time.Time      `json:"at"`
	Fields map[string]any `json:"fields,omitempty"`
	Kind   string         `json:"kind"`
	Detail string         `json:"detail,omitempty"`
}

// The event kinds a run writes. A reader on another box filters on these
// strings, so they are part of the on-disk format: renaming one silently
// breaks every consumer rather than failing a build.
const (
	EventRunStarted      = "run_started"
	EventWorktreeCreated = "worktree_created"
	EventPhase           = "phase"
	EventRunFinished     = "run_finished"
)

// Append writes one event to events.jsonl.
func (r *RunLog) Append(kind, detail string, fields map[string]any) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	ev := Event{At: time.Now().UTC(), Kind: kind, Detail: detail, Fields: fields}
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(r.dir, "events.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open events.jsonl: %w", err)
	}
	defer f.Close()
	if _, err := f.Write(append(line, '\n')); err != nil {
		return fmt.Errorf("append event: %w", err)
	}
	return nil
}

// LoadRunMeta reads a single run's meta.json.
func LoadRunMeta(dir string) (RunMeta, error) {
	data, err := os.ReadFile(filepath.Join(dir, "meta.json"))
	if err != nil {
		return RunMeta{}, fmt.Errorf("read run meta: %w", err)
	}
	var m RunMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return RunMeta{}, fmt.Errorf("parse run meta %s: %w", dir, err)
	}
	return m, nil
}

// LoadEvents reads a run's events.jsonl. A truncated trailing line is skipped
// rather than failing the read: the file is appended to by a live process.
func LoadEvents(dir string) ([]Event, error) {
	data, err := os.ReadFile(filepath.Join(dir, "events.jsonl"))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read events: %w", err)
	}
	var out []Event
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		out = append(out, ev)
	}
	return out, nil
}

// ListRuns returns every run under RunsRoot, newest first. A directory without
// a readable meta.json is skipped rather than failing the listing — a run
// killed before its first write must not break `jiradozer runs`.
func ListRuns() ([]RunMeta, error) {
	root := ExpandHome(RunsRoot)
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read runs dir: %w", err)
	}
	out := make([]RunMeta, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		m, err := LoadRunMeta(filepath.Join(root, e.Name()))
		if err != nil {
			continue
		}
		out = append(out, m)
	}
	sortRunsNewestFirst(out)
	return out, nil
}

func sortRunsNewestFirst(runs []RunMeta) {
	for i := 1; i < len(runs); i++ {
		for j := i; j > 0 && runs[j].StartedAt.After(runs[j-1].StartedAt); j-- {
			runs[j], runs[j-1] = runs[j-1], runs[j]
		}
	}
}
