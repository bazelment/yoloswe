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
	// writing holds the recipients a write is currently in flight to.
	//
	// One rule for both paths that can write: a direct send to an idle
	// recipient, and a drain. Neither holds c.mu across the write — it can take
	// seconds, pasting and reading a pane back — so without a claim two callers
	// both see "idle" and both write, and the second lands mid-turn, which is
	// the interruption this whole type exists to prevent. Anything that cannot
	// take the claim queues instead.
	writing map[SessionID]bool
	// retryArmed records that a failed delivery has a retry scheduled. Only
	// read by tests, which would otherwise have to wait out retryDelay.
	retryArmed bool
	seq        uint64
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
	// 0700: queue files hold message text meant for one session's operator.
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("failed to create delivery dir %s: %w", dir, err)
	}
	c := &Courier{
		target:   target,
		panes:    panes,
		dir:      dir,
		pending:  make(map[SessionID][]Delivery),
		reported: make(map[SessionID]map[SessionStatus]bool),
		writing:  make(map[SessionID]bool),
	}
	if err := c.load(); err != nil {
		return nil, err
	}
	return c, nil
}

// Send delivers text to a session, writing it now if the recipient is idle and
// queueing it otherwise. It reports whether the message was queued.
//
// A recipient in a terminal state is refused rather than queued: nothing will
// ever make it idle again, so the message would sit on disk forever.
func (c *Courier) Send(ctx context.Context, from, to SessionID, text string, submit bool) (queued bool, err error) {
	// A child speaking to its own parent replaces the report the courier would
	// otherwise generate for it — see noteChildSpoke. Only a message the child
	// composed itself counts; the courier's own report goes through deliver,
	// below, which skips this.
	if from != "" {
		if sender, ok := c.target.SessionInfo(from); ok && sender.ParentSessionID == to {
			c.noteChildSpoke(from)
		}
	}
	return c.deliver(ctx, from, to, text, submit)
}

// deliver is Send without the child-spoke bookkeeping: it writes to an idle
// recipient and queues for a busy one, reporting which it did.
func (c *Courier) deliver(ctx context.Context, from, to SessionID, text string, submit bool) (queued bool, err error) {
	info, ok := c.target.SessionInfo(to)
	if !ok {
		return false, fmt.Errorf("session not found: %s", to)
	}
	if info.Status.IsTerminal() {
		return false, fmt.Errorf("session %s is %s and cannot receive messages", to, info.Status)
	}

	// Write only under the claim, and re-read the status once holding it: two
	// callers can both have seen StatusIdle above, and only one of them may
	// write. The loser queues, which is the right answer — the winner's write
	// starts a turn, so the recipient is no longer idle.
	if info.Status == StatusIdle && c.claimWrite(to) {
		defer c.releaseWrite(to)
		if fresh, ok := c.target.SessionInfo(to); ok && fresh.Status == StatusIdle {
			if err := c.write(ctx, fresh, text, submit); err != nil {
				return false, err
			}
			return false, nil
		}
	}
	if err := c.enqueue(from, to, text, submit); err != nil {
		return false, err
	}
	return true, nil
}

// enqueue appends a delivery to the recipient's queue and persists it.
//
// A queue that cannot be written is not a queue: "queued" is a promise the
// message survives a restart, and the caller acts on it — the CLI tells the
// user the message is waiting, and a subagent report stops being retried. So a
// failed persist rolls the delivery back out of memory and is returned, rather
// than leaving the caller holding a promise this process cannot keep.
func (c *Courier) enqueue(from, to SessionID, text string, submit bool) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.seq++
	d := Delivery{
		ID:        fmt.Sprintf("%d-%d", time.Now().UnixNano(), c.seq),
		From:      from,
		To:        to,
		Text:      text,
		Submit:    submit,
		CreatedAt: time.Now(),
	}
	before := c.pending[to]
	c.pending[to] = append(append([]Delivery(nil), before...), d)
	if err := c.persistLocked(to); err != nil {
		if len(before) == 0 {
			delete(c.pending, to)
		} else {
			c.pending[to] = before
		}
		logDeliveryWarn("failed to persist delivery queue", to, err)
		return fmt.Errorf("queue delivery for %s: %w", to, err)
	}
	return nil
}

// Pending returns a copy of the queue for a recipient, oldest first.
func (c *Courier) Pending(to SessionID) []Delivery {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]Delivery(nil), c.pending[to]...)
}

// retryDelay is how long a failed delivery waits before it is tried again.
// Long enough that a recipient stuck behind a modal is not hammered, short
// enough that a transient tmux error costs one pause rather than a turn.
const retryDelay = 30 * time.Second

// retryLater schedules one more drain for a recipient whose write failed.
//
// A timer rather than a ticker: nothing polls in this design, and a failure is
// the only thing that arms this. Each failed attempt arms exactly one more, so
// a recipient that stays broken is retried at a steady low rate and one that
// recovers stops rescheduling as soon as a write succeeds or its queue is
// discarded. The context stops it at shutdown.
func (c *Courier) retryLater(ctx context.Context, to SessionID) {
	c.mu.Lock()
	c.retryArmed = true
	c.mu.Unlock()

	timer := time.AfterFunc(retryDelay, func() {
		if ctx.Err() != nil {
			return
		}
		c.Drain(ctx, to)
	})
	context.AfterFunc(ctx, func() { timer.Stop() })
}

// claimWrite reserves the right to write to a recipient, reporting whether it
// got it. A caller that does not must not write: it queues instead.
func (c *Courier) claimWrite(to SessionID) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.writing[to] {
		return false
	}
	c.writing[to] = true
	return true
}

func (c *Courier) releaseWrite(to SessionID) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.writing, to)
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
// On a write failure the delivery stays queued and a retry is scheduled. It
// cannot simply wait for the next transition: the common case is a recipient
// that was already idle when the drain ran, and a session that never leaves
// idle produces no further transition to ride — the parent would wait out the
// whole process lifetime for a report sitting on disk.
//
// Deliveries for a session that has reached a terminal state are discarded —
// they can never be written, and keeping them would leak the queue forever.
func (c *Courier) Drain(ctx context.Context, to SessionID) {
	c.mu.Lock()
	queue := c.pending[to]
	if len(queue) == 0 {
		c.mu.Unlock()
		return
	}
	next := queue[0]
	c.mu.Unlock()

	if !c.claimWrite(to) {
		// Another write is already in flight; that write will leave the
		// recipient running, and this queue rides the next idle transition.
		return
	}
	defer c.releaseWrite(to)

	info, ok := c.target.SessionInfo(to)
	if !ok {
		// Not "gone" — just not visible from here yet. A recipient can be
		// missing because its repo has not been opened, or because the startup
		// sweep ran before reconciliation re-adopted it. Discarding on a failed
		// lookup deleted persisted queues on every restart, which is precisely
		// what the on-disk queue exists to survive. A queue is only ever
		// dropped on an observed terminal transition — see Watch.
		return
	}
	if info.Status.IsTerminal() {
		c.discard(to)
		return
	}
	if info.Status != StatusIdle {
		return
	}

	if err := c.write(ctx, info, next.Text, next.Submit); err != nil {
		logDeliveryWarn("failed to write queued delivery", to, err)
		c.retryLater(ctx, to)
		return
	}

	c.mu.Lock()
	if remaining := c.pending[to]; len(remaining) <= 1 {
		delete(c.pending, to)
	} else {
		// Reslice rather than re-copy: Pending already hands callers a copy,
		// so nothing outside aliases this, and copying the tail on every drain
		// makes emptying an n-deep queue quadratic — worst exactly in the
		// fan-out case, where n subagents report into one busy parent.
		c.pending[to] = remaining[1:]
	}
	err := c.persistLocked(to)
	c.mu.Unlock()

	if err != nil {
		// Delivery is at-least-once, deliberately. The message is written
		// before the queue file is updated, so a persist that fails here
		// leaves the delivered item on disk and a restart will deliver it a
		// second time. That is the trade this queue exists to make: a
		// duplicate report is noise, a dropped one is a parent waiting forever
		// for a subagent that already finished. Any later successful persist
		// for this recipient clears the stale entry.
		logDeliveryWarn("delivery written but queue not persisted (may be redelivered after a restart)", to, err)
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
		// Wait before every attempt but the first: a paste needs a frame to
		// show up, and sleeping *after* the last one only delays the verdict.
		if i > 0 {
			select {
			case <-ctx.Done():
				return false
			case <-time.After(pasteVerifyInterval):
			}
		}
		lines, err := c.target.CapturePaneText(id, pasteVerifyLines)
		if err != nil {
			continue
		}
		for _, line := range lines {
			if strings.Contains(line, probe) {
				return true
			}
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
//
// This runs on every terminal transition, and most sessions never had a queue,
// so an absent one returns before persistLocked can unlink a path that was
// never written.
func (c *Courier) discard(to SessionID) {
	c.mu.Lock()
	if _, queued := c.pending[to]; !queued {
		c.mu.Unlock()
		return
	}
	delete(c.pending, to)
	err := c.persistLocked(to)
	c.mu.Unlock()
	if err != nil {
		logDeliveryWarn("failed to clear delivery queue", to, err)
	}
}

// DrainIdle delivers to every recipient that is already idle right now.
//
// Watch only reacts to idle *transitions*, which is one event short of
// correct after a restart: NewCourier reloads the queues from disk, but a
// recipient that was already idle when bramble came up will not transition
// again until something else gives it work — so its mail would sit there
// indefinitely. Called once per manager as it registers.
func (c *Courier) DrainIdle(ctx context.Context) {
	c.mu.Lock()
	recipients := make([]SessionID, 0, len(c.pending))
	for to := range c.pending {
		recipients = append(recipients, to)
	}
	c.mu.Unlock()

	// Drain re-checks status itself, so a recipient that is busy or gone is a
	// no-op here rather than a special case.
	for _, to := range recipients {
		c.Drain(ctx, to)
	}
}

// Watch drains a session's queue whenever it becomes idle. It returns an
// unsubscribe function and runs until ctx is canceled.
func (c *Courier) Watch(ctx context.Context, mgr *Manager) func() {
	return watchStateChanges(ctx, mgr, func(evt SessionStateChangeEvent) {
		// A session is both a recipient of queued mail and, when it has a
		// parent, a subagent whose progress that parent is waiting on. One
		// transition can mean both things.
		//
		// The child comes off the event, not from a lookup: the tmux monitor
		// deletes a completed session from the manager immediately after
		// emitting, so by the time this callback runs the lookup would usually
		// miss — which is exactly the window-close path Gemini and Agy report
		// on. Fall back to a lookup only for an event that predates the
		// snapshot field.
		child := evt.Info
		if child.ID == "" {
			child, _ = c.target.SessionInfo(evt.SessionID)
		}
		// Report on transitions only. Re-adoption emits a synthetic
		// same-status event so a restored session's mail can be drained, and
		// that is not news: the dedup map lives only in memory, so reporting
		// on it would hand the parent the same "is idle" report, with the same
		// result path, after every single restart.
		if child.ParentSessionID != "" && evt.OldStatus != evt.NewStatus {
			c.reportToParent(ctx, child)
		}
		switch {
		case evt.NewStatus == StatusIdle:
			c.Drain(ctx, evt.SessionID)
		case evt.NewStatus.IsTerminal():
			// Nothing will make this session idle again; reclaim the queue
			// rather than leaving it on disk forever.
			c.discard(evt.SessionID)
			c.forgetChild(evt.SessionID)
		}
	})
}

// --- persistence -------------------------------------------------------------

// queuePath returns the on-disk file backing a recipient's queue.
func (c *Courier) queuePath(to SessionID) (string, error) {
	// Belt and braces: the name is already reduced to an allowlist, so this
	// can only fail if that ever regresses. Cheap enough to keep as a guard
	// that does not depend on the sanitizer being right.
	return containedPath(c.dir, sanitizeFileName(string(to))+".json")
}

// persistLocked writes a recipient's current queue to disk, removing the file
// when the queue empties so the directory does not accumulate empty stubs. The
// caller must hold c.mu.
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
	path, err := c.queuePath(to)
	if err != nil {
		return err
	}
	queue := c.pending[to]
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
	// 0600: a queue file holds message text meant for one session's operator.
	return writeFileAtomic(path, data, 0o600)
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
		// Drop anything too old to be worth delivering. A queue is only ever
		// discarded on an observed terminal transition, so a recipient that
		// vanished without one — a deleted session, a crash — would otherwise
		// keep its file forever. Age is the one signal available here: nothing
		// in the file says whether its recipient still exists.
		fresh := queue[:0]
		for _, d := range queue {
			if time.Since(d.CreatedAt) < maxDeliveryAge {
				fresh = append(fresh, d)
			}
		}
		if len(fresh) == 0 {
			// Reclaim the file too, or it is re-read and re-pruned every start.
			_ = os.Remove(filepath.Join(c.dir, e.Name()))
			continue
		}
		c.pending[fresh[0].To] = fresh
	}
	return nil
}

// maxDeliveryAge bounds how long an undelivered message is kept. Generous: the
// queue exists to survive a restart, and a bramble that was down over a weekend
// should still deliver. Past it the recipient is almost certainly gone, and a
// week-old "your subagent finished" is of no use to anyone anyway.
const maxDeliveryAge = 7 * 24 * time.Hour

// logDeliveryWarn reports a non-fatal courier problem. Delivery failures are
// never returned to the state-change watcher — there is nobody to return them
// to — so they surface here instead of vanishing.
func logDeliveryWarn(msg string, to SessionID, err error) {
	log.Printf("WARNING: %s for session %s: %v", msg, to, err)
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
//
// Under the user's home rather than os.TempDir(). A world-writable temp dir
// cannot be secured from inside this process: another local user can
// pre-create the directory — or a symlink standing in for it — and no amount
// of MkdirAll/Chmod on that path fixes it, because both follow the symlink and
// would hand an attacker's directory our transcripts. $HOME is not writable by
// anyone else, so the question does not arise.
const resultDirName = "research"

// ResultFilePath returns the path a session's result file is written to,
// creating the directory. Shared by the TUI transcript writer and the tmux
// pane capture so a parent is handed the same shape of path either way.
func ResultFilePath(id SessionID) (string, error) {
	dir := filepath.Join(os.TempDir(), resultDirName)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("create result dir: %w", err)
	}
	return containedPath(dir, sanitizeFileName(string(id))+".md")
}
