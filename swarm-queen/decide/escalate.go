package decide

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// EscalationsName is the queue file inside a run directory.
const EscalationsName = "escalations.jsonl"

// NudgesName is the human input queue.
const NudgesName = "nudges.jsonl"

// Escalation is a question the rules could not settle, waiting for an answer.
//
// It is durable and addressable because the alternative -- asking in a chat
// message -- is how a correction reaches one holder and not the others. A queue
// on disk survives compaction, restart, and the operator walking away.
type Escalation struct {
	// ID is stable for a given lane+question, so re-raising the same issue on a
	// later tick does not enqueue a duplicate.
	ID       string `json:"id"`
	Lane     string `json:"lane"`
	Question string `json:"question"`
	Raised   string `json:"raised"`
	// Answered is set when a human resolves it; an unanswered escalation keeps
	// its lane held rather than letting the next tick improvise past it.
	Answered string   `json:"answered,omitempty"`
	Answer   string   `json:"answer,omitempty"`
	Evidence []string `json:"evidence,omitempty"`
}

// Open reports whether the escalation still needs a person.
func (e Escalation) Open() bool { return e.Answered == "" }

// EscalationID derives a stable id from the lane and question.
func EscalationID(lane, question string) string {
	q := strings.ToLower(strings.Join(strings.Fields(question), " "))
	return lane + ":" + q
}

// Nudge is an operator instruction applied at the next tick.
//
// Standing rules go here rather than into a chat message, because a rule that
// lives only in conversation is lost at the next compaction -- and the run's own
// recurring prompt then keeps relaying the superseded version.
type Nudge struct {
	Text string `json:"text"`
	// Lane scopes the nudge; empty means it applies to the run.
	Lane string `json:"lane,omitempty"`
	At   string `json:"at"`
	// AppliedAt is set once a tick has consumed a one-shot nudge.
	AppliedAt string `json:"applied_at,omitempty"`
	// Standing marks a rule that applies on every future tick, not just once.
	Standing bool `json:"standing"`
}

// Pending reports whether the nudge still needs applying.
func (n Nudge) Pending() bool { return n.Standing || n.AppliedAt == "" }

// AppendEscalation adds an escalation unless an open one with the same id
// already exists. Re-raising an unanswered question every tick would bury the
// queue in duplicates and train the operator to ignore it.
func AppendEscalation(runDir string, e Escalation) (added bool, err error) {
	count, err := AppendEscalations(runDir, []Escalation{e})
	return count == 1, err
}

// AppendEscalations adds every escalation that is not already open. A tick can
// raise several questions, so load and index the append-only queue once rather
// than decoding its entire history for every decision.
func AppendEscalations(runDir string, escalations []Escalation) (int, error) {
	existing, err := LoadEscalations(runDir)
	if err != nil {
		return 0, err
	}
	open := make(map[string]bool, len(existing))
	for _, prior := range existing {
		if prior.Open() {
			open[prior.ID] = true
		}
	}

	added := 0
	for _, e := range escalations {
		if e.ID == "" {
			e.ID = EscalationID(e.Lane, e.Question)
		}
		if e.Raised == "" {
			e.Raised = time.Now().UTC().Format(time.RFC3339)
		}
		if open[e.ID] {
			continue
		}
		if err := appendJSONL(filepath.Join(runDir, EscalationsName), e); err != nil {
			return added, err
		}
		open[e.ID] = true
		added++
	}
	return added, nil
}

// LoadEscalations reads the queue, collapsing each id to its latest record so an
// answer supersedes the question it answers.
func LoadEscalations(runDir string) ([]Escalation, error) {
	lines, err := readJSONL(filepath.Join(runDir, EscalationsName))
	if err != nil {
		return nil, err
	}
	latest := map[string]Escalation{}
	var order []string
	for _, raw := range lines {
		var e Escalation
		if err := json.Unmarshal(raw, &e); err != nil {
			continue // a malformed line must not hide the rest of the queue
		}
		if _, seen := latest[e.ID]; !seen {
			order = append(order, e.ID)
		}
		latest[e.ID] = e
	}
	out := make([]Escalation, 0, len(order))
	for _, id := range order {
		out = append(out, latest[id])
	}
	return out, nil
}

// OpenEscalations returns only those still awaiting an answer.
func OpenEscalations(runDir string) ([]Escalation, error) {
	all, err := LoadEscalations(runDir)
	if err != nil {
		return nil, err
	}
	var out []Escalation
	for _, e := range all {
		if e.Open() {
			out = append(out, e)
		}
	}
	return out, nil
}

// AnswerEscalation records a human's answer by appending a superseding record.
// Append-only: the original question and its answer both remain readable.
func AnswerEscalation(runDir, id, answer string) error {
	all, err := LoadEscalations(runDir)
	if err != nil {
		return err
	}
	for _, e := range all {
		if e.ID != id {
			continue
		}
		e.Answered = time.Now().UTC().Format(time.RFC3339)
		e.Answer = answer
		return appendJSONL(filepath.Join(runDir, EscalationsName), e)
	}
	return fmt.Errorf("no escalation %q in %s", id, runDir)
}

// AppendNudge queues an operator instruction.
func AppendNudge(runDir string, n Nudge) error {
	if n.At == "" {
		n.At = time.Now().UTC().Format(time.RFC3339)
	}
	return appendJSONL(filepath.Join(runDir, NudgesName), n)
}

// PendingNudges returns nudges a tick should apply: every standing rule, plus
// one-shot nudges not yet consumed.
func PendingNudges(runDir string) ([]Nudge, error) {
	lines, err := readJSONL(filepath.Join(runDir, NudgesName))
	if err != nil {
		return nil, err
	}
	var out []Nudge
	for _, raw := range lines {
		var n Nudge
		if err := json.Unmarshal(raw, &n); err != nil {
			continue
		}
		if n.Pending() {
			out = append(out, n)
		}
	}
	return out, nil
}

// StandingRules returns the text of every standing nudge, oldest first. These
// are re-applied on every tick, which is what makes a correction survive a
// compaction that would otherwise drop it.
func StandingRules(runDir string) ([]string, error) {
	ns, err := PendingNudges(runDir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, n := range ns {
		if n.Standing {
			out = append(out, n.Text)
		}
	}
	return out, nil
}

// EscalationsFrom converts blocking decisions into queue entries.
func EscalationsFrom(ds []Decision) []Escalation {
	var out []Escalation
	for _, d := range ds {
		if d.Kind != KindEscalate {
			continue
		}
		ev := append([]string(nil), d.Evidence...)
		sort.Strings(ev)
		out = append(out, Escalation{
			ID:       EscalationID(d.Lane, d.Reason),
			Lane:     d.Lane,
			Question: d.Reason,
			Evidence: ev,
		})
	}
	return out
}

func appendJSONL(path string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

func readJSONL(path string) ([][]byte, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out [][]byte
	for _, line := range strings.Split(string(b), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		out = append(out, []byte(line))
	}
	return out, nil
}
