package session

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// PaneWriter is the narrow slice of a tmux controller the courier needs in
// order to type into a session's pane.
//
// It is declared here, on the consumer side, because tmuxctl imports session
// for PaneStatus — so session cannot import tmuxctl back. bramble/main.go
// adapts a tmuxctl.Controller to this interface; tests supply a fake.
type PaneWriter interface {
	Paste(ctx context.Context, target, text string) error
	SendEnter(ctx context.Context, target string) error
}

// Delivery is one queued message waiting for its recipient to become idle.
type Delivery struct {
	CreatedAt time.Time `json:"created_at"`
	ID        string    `json:"id"`
	// From is the sender, when there is one. Empty for a message sent from a
	// plain shell rather than from another session.
	From   SessionID `json:"from,omitempty"`
	To     SessionID `json:"to"`
	Text   string    `json:"text"`
	Submit bool      `json:"submit"`
}

// DeliveryTarget is the narrow slice of the session registry the courier needs.
// Mirrors the consumer-side interface style of control.Registry so the courier
// can be exercised with a fake instead of live managers and real tmux windows.
type DeliveryTarget interface {
	SessionInfo(id SessionID) (SessionInfo, bool)
	SendFollowUp(id SessionID, message string) error
	ResolveTmuxTarget(id SessionID) (string, error)
	// CapturePaneText reads a tmux session's scrollback. It is how a tmux-mode
	// subagent produces a result at all: that mode never runs the TUI turn
	// loop, so bramble holds no transcript of it — the pane is the only record.
	CapturePaneText(id SessionID, n int) ([]string, error)
	// MarkRunning records that a turn has started. Only bramble knows this for
	// a tmux session it just typed into; see Manager.SetSessionRunning.
	MarkRunning(id SessionID)
}

// registryTarget adapts a *SessionRegistry to DeliveryTarget.
type registryTarget struct{ reg *SessionRegistry }

// NewRegistryDeliveryTarget wraps a registry so it can drive a Courier.
func NewRegistryDeliveryTarget(reg *SessionRegistry) DeliveryTarget {
	return &registryTarget{reg: reg}
}

func (t *registryTarget) SessionInfo(id SessionID) (SessionInfo, bool) {
	info, _, ok := t.reg.GetSessionInfo(id)
	return info, ok
}

func (t *registryTarget) SendFollowUp(id SessionID, message string) error {
	_, mgr, ok := t.reg.GetSessionInfo(id)
	if !ok || mgr == nil {
		return fmt.Errorf("session not found: %s", id)
	}
	return mgr.SendFollowUp(id, message)
}

func (t *registryTarget) ResolveTmuxTarget(id SessionID) (string, error) {
	return t.reg.ResolveTmuxTarget(id)
}

func (t *registryTarget) CapturePaneText(id SessionID, n int) ([]string, error) {
	return t.reg.CapturePaneText(id, n)
}

func (t *registryTarget) MarkRunning(id SessionID) { t.reg.SetSessionRunning(id) }

// Courier delivers text into a session regardless of how that session runs,
// holding a message back while the recipient is mid-turn.
//
// This is the piece that makes session-to-session messaging safe. Without it a
// caller has exactly two options, and both are wrong some of the time:
// SendFollowUp reaches only TUI-mode sessions and refuses anything but an idle
// one, while pasting into a tmux pane always "succeeds" — even mid-turn, where
// the text lands in the recipient's *next* prompt, stripped of the context that
// made it make sense. Checking for idleness first only narrows the race.
//
// A queued delivery is durable, ordered per recipient, and written exactly once,
// when the recipient is genuinely ready for it.
type Courier struct { //nolint:govet // fieldalignment: grouping by role reads better
	target  DeliveryTarget
	panes   PaneWriter
	dir     string
	mu      sync.Mutex
	pending map[SessionID][]Delivery
	// reported remembers which (child, status) pairs have already been
	// reported to a parent, so a child is not announced twice.
	reported map[SessionID]map[SessionStatus]bool
	seq      uint64
	// onDelivered, when set, is called after a delivery is written. Tests use
	// it to observe the drain without polling.
	onDelivered func(Delivery)
}

// NewCourier creates a courier that persists its queue under dir. If dir is
// empty it defaults to ~/.bramble/deliveries. Any queue already on disk is
// loaded, so a message queued before a bramble restart is not silently lost.
func NewCourier(target DeliveryTarget, panes PaneWriter, dir string) (*Courier, error) {
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("failed to get home directory: %w", err)
		}
		dir = filepath.Join(home, ".bramble", "deliveries")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("failed to create delivery dir %s: %w", dir, err)
	}
	c := &Courier{
		target:   target,
		panes:    panes,
		dir:      dir,
		pending:  make(map[SessionID][]Delivery),
		reported: make(map[SessionID]map[SessionStatus]bool),
	}
	if err := c.load(); err != nil {
		return nil, err
	}
	return c, nil
}

// SetOnDelivered installs a callback invoked after each successful write.
func (c *Courier) SetOnDelivered(fn func(Delivery)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.onDelivered = fn
}

// Send delivers text to a session, writing it now if the recipient is idle and
// queueing it otherwise. It reports whether the message was queued.
//
// A recipient in a terminal state is refused rather than queued: nothing will
// ever make it idle again, so the message would sit on disk forever.
func (c *Courier) Send(ctx context.Context, from, to SessionID, text string, submit bool) (queued bool, err error) {
	info, ok := c.target.SessionInfo(to)
	if !ok {
		return false, fmt.Errorf("session not found: %s", to)
	}
	if isTerminalStatus(info.Status) {
		return false, fmt.Errorf("session %s is %s and cannot receive messages", to, info.Status)
	}

	// A child speaking to its own parent replaces the report the courier would
	// otherwise generate for it — see noteChildSpoke.
	if from != "" {
		if sender, ok := c.target.SessionInfo(from); ok && sender.ParentSessionID == to {
			c.noteChildSpoke(from)
		}
	}

	if info.Status == StatusIdle {
		if err := c.write(ctx, info, text, submit); err != nil {
			return false, err
		}
		return false, nil
	}
	c.enqueue(from, to, text, submit)
	return true, nil
}

// enqueue appends a delivery to the recipient's queue and persists it.
func (c *Courier) enqueue(from, to SessionID, text string, submit bool) {
	c.mu.Lock()
	c.seq++
	d := Delivery{
		ID:        fmt.Sprintf("%d-%d", time.Now().UnixNano(), c.seq),
		From:      from,
		To:        to,
		Text:      text,
		Submit:    submit,
		CreatedAt: time.Now(),
	}
	c.pending[to] = append(c.pending[to], d)
	err := c.persistLocked(to)
	c.mu.Unlock()

	if err != nil {
		// A failed persist costs durability across a restart, not delivery:
		// the in-memory queue is still authoritative for this process.
		logDeliveryWarn("failed to persist delivery queue", to, err)
	}
}

// Pending returns a copy of the queue for a recipient, oldest first.
func (c *Courier) Pending(to SessionID) []Delivery {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Delivery(nil), c.pending[to]...)
}

// Drain writes the oldest queued delivery for a session that is now ready to
// take it, and returns.
//
// Exactly one per idle transition is deliberate. Writing a message — as a TUI
// follow-up or as a paste into a pane — starts the recipient's next turn, so it
// is no longer idle by the time the second would go out. Draining the whole
// queue here would mean a TUI SendFollowUp rejected for "not idle" and a tmux
// paste landing mid-turn, which is exactly the interruption the queue exists to
// prevent. The remainder rides the next transition; the recipient goes idle
// again at the end of the turn this delivery just started.
//
// On a write failure the delivery stays queued and retries on that same next
// transition, so a transient tmux error does not drop it.
//
// Deliveries for a session that has reached a terminal state are discarded —
// they can never be written, and keeping them would leak the queue forever.
func (c *Courier) Drain(ctx context.Context, to SessionID) {
	c.mu.Lock()
	queue := append([]Delivery(nil), c.pending[to]...)
	c.mu.Unlock()
	if len(queue) == 0 {
		return
	}

	info, ok := c.target.SessionInfo(to)
	if !ok || isTerminalStatus(info.Status) {
		c.discard(to)
		return
	}
	if info.Status != StatusIdle {
		return
	}

	next := queue[0]
	if err := c.write(ctx, info, next.Text, next.Submit); err != nil {
		logDeliveryWarn("failed to write queued delivery", to, err)
		return
	}

	c.mu.Lock()
	if remaining := c.pending[to]; len(remaining) <= 1 {
		delete(c.pending, to)
	} else {
		c.pending[to] = append([]Delivery(nil), remaining[1:]...)
	}
	err := c.persistLocked(to)
	cb := c.onDelivered
	c.mu.Unlock()

	if err != nil {
		logDeliveryWarn("failed to persist delivery queue", to, err)
	}
	if cb != nil {
		cb(next)
	}
}

// write puts text into the session using whichever path its runner supports.
// This switch is the whole point of the courier: it is the only place in
// bramble that can address a session without first knowing how it runs.
func (c *Courier) write(ctx context.Context, info SessionInfo, text string, submit bool) error {
	// Anything written starts the recipient's next turn, so whatever it says at
	// the end of that turn is fresh news for its parent.
	defer c.resetIdleReport(info.ID)

	switch info.RunnerType {
	case RunnerTypeTUI:
		// The TUI turn loop delivers a follow-up as a real prompt, so there is
		// no keystroke to submit — Submit is meaningless here, not ignored.
		return c.target.SendFollowUp(info.ID, text)
	case RunnerTypeTmux, RunnerTypeTmuxTracked:
		if c.panes == nil {
			return fmt.Errorf("no tmux writer configured")
		}
		target, err := c.target.ResolveTmuxTarget(info.ID)
		if err != nil {
			return err
		}
		if err := c.panes.Paste(ctx, target, text); err != nil {
			return err
		}
		// Confirm the text actually reached the prompt before pressing Enter.
		//
		// An agent CLI announces it is idle the moment its turn ends, but its
		// TUI can still be finalizing that turn and will drop a paste that
		// arrives in the gap — observed with codex, whose notify hook fires
		// ahead of its prompt being ready. tmux reports success either way, so
		// without this check the message is lost silently and, worse, the
		// session is then marked running for a turn that never started,
		// wedging it until something else moves it.
		if !c.pasteLanded(ctx, info.ID, text) {
			if err := c.panes.Paste(ctx, target, text); err != nil {
				return err
			}
			if !c.pasteLanded(ctx, info.ID, text) {
				// Returning an error keeps the delivery queued for the next
				// idle transition rather than dropping it.
				return fmt.Errorf("paste did not reach session %s's prompt", info.ID)
			}
		}
		if !submit {
			// Staged in the pane for someone to review; no turn has started.
			return nil
		}
		if err := c.panes.SendEnter(ctx, target); err != nil {
			return err
		}
		// There is deliberately no read-back check that the Enter was taken.
		// The signal is not separable: an agent CLI echoes the submitted prompt
		// into its transcript directly above the composer, so a pane scrape
		// cannot tell "still pending" from "just submitted". A false negative
		// would re-queue a message the recipient already received and answered,
		// which is worse than the case it guards. The reliable cause of a
		// swallowed Enter — a pane sitting in tmux copy mode — is handled at
		// the source, in tmuxctl's PaneWriter.
		//
		// Submitting started a turn. Say so, or the session stays "idle" for
		// its whole duration and its next notify is discarded.
		c.target.MarkRunning(info.ID)
		return nil
	case "":
		// The runner type is only set once runSession picks one. A session
		// still pending has no way to receive anything yet.
		return fmt.Errorf("session %s has not started yet", info.ID)
	default:
		return fmt.Errorf("session %s has unknown runner type %q", info.ID, info.RunnerType)
	}
}

// pasteVerify bounds how long a paste is given to show up in the pane before
// it is treated as dropped. Short: this only covers a TUI finishing its
// previous turn, not real latency.
const (
	pasteVerifyAttempts = 12
	pasteVerifyInterval = 150 * time.Millisecond
	pasteVerifyLines    = 40
	pasteProbeLen       = 24
)

// pasteLanded reports whether text is visible in the session's pane.
//
// It looks for a prefix of the first line rather than the whole message: a TUI
// re-renders a long prompt with its own wrapping and decoration, so only a
// short run of characters can be relied on to survive verbatim.
func (c *Courier) pasteLanded(ctx context.Context, id SessionID, text string) bool {
	probe := pasteProbe(text)
	if probe == "" {
		return true // nothing distinctive to look for; do not block delivery
	}
	for i := 0; i < pasteVerifyAttempts; i++ {
		lines, err := c.target.CapturePaneText(id, pasteVerifyLines)
		if err == nil {
			for _, line := range lines {
				if strings.Contains(line, probe) {
					return true
				}
			}
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(pasteVerifyInterval):
		}
	}
	return false
}

// pasteProbe picks the substring to look for in the pane.
func pasteProbe(text string) string {
	first := text
	if i := strings.IndexByte(first, '\n'); i >= 0 {
		first = first[:i]
	}
	first = strings.TrimSpace(first)
	if len(first) > pasteProbeLen {
		first = first[:pasteProbeLen]
	}
	return first
}

// discard drops a recipient's whole queue, on disk and in memory.
func (c *Courier) discard(to SessionID) {
	c.mu.Lock()
	delete(c.pending, to)
	err := c.persistLocked(to)
	c.mu.Unlock()
	if err != nil {
		logDeliveryWarn("failed to clear delivery queue", to, err)
	}
}

// Watch drains a session's queue whenever it becomes idle. It returns an
// unsubscribe function and runs until ctx is canceled.
func (c *Courier) Watch(ctx context.Context, mgr *Manager) func() {
	ch := make(chan SessionStateChangeEvent, 100)
	unsub := mgr.SubscribeStateChanges(ch)

	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case evt, ok := <-ch:
				if !ok {
					return
				}
				// A session is both a recipient of queued mail and, when it
				// has a parent, a subagent whose progress that parent is
				// waiting on. One transition can mean both things.
				if info, found := c.target.SessionInfo(evt.SessionID); found && info.ParentSessionID != "" {
					c.reportToParent(ctx, info)
				}
				switch evt.NewStatus {
				case StatusIdle:
					c.Drain(ctx, evt.SessionID)
				case StatusCompleted, StatusFailed, StatusStopped:
					// Nothing will make this session idle again; reclaim the
					// queue rather than leaving it on disk forever.
					c.discard(evt.SessionID)
					c.forgetChild(evt.SessionID)
				}
			}
		}
	}()

	return func() {
		unsub()
		close(ch)
	}
}

// --- persistence -------------------------------------------------------------

// queuePath returns the on-disk file backing a recipient's queue. Session IDs
// are generated from a worktree name plus hex (generateSessionID), but the file
// name is sanitized anyway so a hand-passed ID can never escape the directory.
func (c *Courier) queuePath(to SessionID) string {
	return filepath.Join(c.dir, sanitizeQueueName(string(to))+".json")
}

func sanitizeQueueName(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			return r
		case r == '-', r == '_':
			return r
		default:
			return '_'
		}
	}, s)
}

// persistLocked writes a recipient's current queue to disk. The caller must
// hold c.mu.
//
// Writing under the lock, rather than snapshotting and writing after releasing
// it, is what keeps the file agreeing with memory. Several subagents finishing
// at once all report to the same parent, and with the write outside the lock a
// goroutine that snapshotted first could write last, putting back a queue
// missing everything appended in between. Delivery still worked, so the loss
// only appeared after a restart — the one case the on-disk queue exists for.
//
// The cost is a small file write inside the critical section, at the rate
// subagents finish turns.
func (c *Courier) persistLocked(to SessionID) error {
	return c.persist(to, c.pending[to])
}

// persist writes a recipient's queue atomically, removing the file when the
// queue empties so the directory does not accumulate empty stubs.
func (c *Courier) persist(to SessionID, queue []Delivery) error {
	path := c.queuePath(to)
	if len(queue) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	data, err := json.MarshalIndent(queue, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(c.dir, ".queue-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	// Rename last: a reader either sees the old queue or the new one, never a
	// half-written file.
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}

// load reads every queue file back into memory.
func (c *Courier) load() error {
	entries, err := os.ReadDir(c.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(c.dir, e.Name()))
		if err != nil {
			continue
		}
		var queue []Delivery
		if err := json.Unmarshal(data, &queue); err != nil || len(queue) == 0 {
			continue
		}
		sort.SliceStable(queue, func(i, j int) bool {
			return queue[i].CreatedAt.Before(queue[j].CreatedAt)
		})
		c.pending[queue[0].To] = queue
	}
	return nil
}

// logDeliveryWarn reports a non-fatal courier problem. Delivery failures are
// never returned to the state-change watcher — there is nobody to return them
// to — so they surface here instead of vanishing.
func logDeliveryWarn(msg string, to SessionID, err error) {
	log.Printf("WARNING: %s for session %s: %v", msg, to, err)
}

// isTerminalStatus reports whether a session can still receive messages.
func isTerminalStatus(s SessionStatus) bool {
	return s == StatusCompleted || s == StatusFailed || s == StatusStopped
}

// parentSessionID reads the session's parent under the lock. The field is set
// once before runSession starts and never mutated, but every other reader in
// this package goes through the mutex, and an unsynchronized read here would
// be the one the race detector eventually catches.
func (s *Session) parentSessionID() SessionID {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.ParentSessionID
}

// resultDirName holds the files a subagent's parent is pointed at: the
// transcript of a TUI session, or the captured pane of a tmux one.
const resultDirName = "bramble-research"

// ResultFilePath returns the path a session's result file is written to,
// creating the directory. Shared by the TUI transcript writer and the tmux
// pane capture so a parent is handed the same shape of path either way.
func ResultFilePath(id SessionID) (string, error) {
	dir := filepath.Join(os.TempDir(), resultDirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create result dir: %w", err)
	}
	return filepath.Join(dir, sanitizeQueueName(string(id))+".md"), nil
}
