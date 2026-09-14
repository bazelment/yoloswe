package decide

import (
	"os"
	"path/filepath"
	"testing"
)

// Re-raising an unanswered question every tick would bury the queue and train
// the operator to ignore it.
func TestAppendEscalationDeduplicatesOpenQuestions(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	e := Escalation{Lane: "a", Question: "completion claim refused by verification"}

	added, err := AppendEscalation(dir, e)
	if err != nil || !added {
		t.Fatalf("first append: added=%v err=%v", added, err)
	}
	added, err = AppendEscalation(dir, e)
	if err != nil {
		t.Fatal(err)
	}
	if added {
		t.Error("the same open question must not be enqueued twice")
	}

	open, err := OpenEscalations(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Errorf("got %d open escalations, want 1", len(open))
	}
}

func TestAppendEscalationsDeduplicatesWithinABatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	es := []Escalation{
		{Lane: "a", Question: "completion claim refused"},
		{Lane: "a", Question: "completion claim refused"},
		{Lane: "b", Question: "missing approval"},
	}
	added, err := AppendEscalations(dir, es)
	if err != nil {
		t.Fatal(err)
	}
	if added != 2 {
		t.Fatalf("added %d escalations, want 2", added)
	}
	open, err := OpenEscalations(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 {
		t.Errorf("got %d open escalations, want 2", len(open))
	}
}

// Answering closes it, and the same question may then be raised again if it
// genuinely recurs.
func TestAnswerClosesAndAllowsReRaise(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	e := Escalation{Lane: "a", Question: "merge without approval at head?"}
	if _, err := AppendEscalation(dir, e); err != nil {
		t.Fatal(err)
	}

	if err := AnswerEscalation(dir, EscalationID("a", e.Question), "no, re-request review"); err != nil {
		t.Fatal(err)
	}
	open, err := OpenEscalations(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 0 {
		t.Fatalf("answered escalation still open: %+v", open)
	}

	// The answer is retained, not overwritten.
	all, err := LoadEscalations(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Answer != "no, re-request review" {
		t.Errorf("answer not recorded: %+v", all)
	}

	// A genuine recurrence enqueues again.
	added, err := AppendEscalation(dir, e)
	if err != nil {
		t.Fatal(err)
	}
	if !added {
		t.Error("a recurrence after an answer should re-raise")
	}
}

// A malformed line must not hide the rest of the queue: silently reading zero
// escalations would look exactly like a healthy run with nothing outstanding.
func TestLoadEscalationsSurvivesMalformedLines(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if _, err := AppendEscalation(dir, Escalation{Lane: "a", Question: "q1"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, EscalationsName)
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	f.WriteString("{not json\n")
	f.Close()
	if _, err := AppendEscalation(dir, Escalation{Lane: "b", Question: "q2"}); err != nil {
		t.Fatal(err)
	}

	open, err := OpenEscalations(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 2 {
		t.Errorf("got %d escalations, want 2 — a bad line hid real work", len(open))
	}
}

// A standing rule applies on every tick. That is what makes a correction survive
// a compaction, instead of the run's recurring prompt relaying a stale version.
func TestStandingRulesPersistAcrossTicks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := AppendNudge(dir, Nudge{Text: "never enable auto-merge", Standing: true}); err != nil {
		t.Fatal(err)
	}
	if err := AppendNudge(dir, Nudge{Text: "increase swe slots to 8"}); err != nil {
		t.Fatal(err)
	}

	for range 3 {
		rules, err := StandingRules(dir)
		if err != nil {
			t.Fatal(err)
		}
		if len(rules) != 1 || rules[0] != "never enable auto-merge" {
			t.Fatalf("standing rule lost: %v", rules)
		}
	}

	pending, err := PendingNudges(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 2 {
		t.Errorf("got %d pending nudges, want 2", len(pending))
	}
}

// A one-shot nudge is consumed; a standing one is not.
func TestOneShotNudgeIsConsumedButStandingIsNot(t *testing.T) {
	t.Parallel()
	oneShot := Nudge{Text: "retry the rate-limited lane", AppliedAt: "2026-09-08T00:00:00Z"}
	if oneShot.Pending() {
		t.Error("an applied one-shot nudge must not stay pending")
	}
	standing := Nudge{Text: "no auto-merge", Standing: true, AppliedAt: "2026-09-08T00:00:00Z"}
	if !standing.Pending() {
		t.Error("a standing rule must remain pending forever")
	}
}

func TestEscalationsFromDecisions(t *testing.T) {
	t.Parallel()
	ds := []Decision{
		{Lane: "a", Kind: KindEscalate, Reason: "claim refused", Evidence: []string{"0 commits"}},
		{Lane: "b", Kind: KindAdvance, Reason: "verified"},
	}
	got := EscalationsFrom(ds)
	if len(got) != 1 || got[0].Lane != "a" {
		t.Fatalf("expected one escalation for lane a, got %+v", got)
	}
	if got[0].ID == "" {
		t.Error("escalation must carry a stable id")
	}
}

// Missing queue files are an empty queue, not an error: a fresh run has neither.
func TestMissingQueuesReadAsEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if es, err := LoadEscalations(dir); err != nil || len(es) != 0 {
		t.Errorf("LoadEscalations on a fresh run: %v %v", es, err)
	}
	if ns, err := PendingNudges(dir); err != nil || len(ns) != 0 {
		t.Errorf("PendingNudges on a fresh run: %v %v", ns, err)
	}
}

// A one-shot nudge must actually reach the lane it addresses. Queued by
// `swarm-queen nudge` and read by nobody -- only StandingRules consumed the
// queue and it filters to Standing -- a one-shot nudge never reached a brief,
// was never marked AppliedAt, and the command had no effect on orchestration.
func TestNudgesForDeliversOneShotNudges(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, n := range []Nudge{
		{Text: "run the integration suite", Lane: "lane-a"},
		{Text: "a rule for everyone", Standing: true},
		{Text: "for someone else", Lane: "lane-b"},
		{Text: "run-wide one-shot"},
	} {
		if err := AppendNudge(dir, n); err != nil {
			t.Fatal(err)
		}
	}

	got, err := NudgesFor(dir, "lane-a")
	if err != nil {
		t.Fatal(err)
	}
	var texts []string
	for _, n := range got {
		texts = append(texts, n.Text)
	}
	want := []string{"run the integration suite", "run-wide one-shot"}
	if len(texts) != len(want) {
		t.Fatalf("got %v, want its own nudge and the run-wide one", texts)
	}
	for i := range want {
		if texts[i] != want[i] {
			t.Errorf("got %v, want %v", texts, want)
		}
	}
}

// A consumed one-shot nudge is not re-delivered; a standing rule always is.
func TestConsumeNudgesRetiresOneShotButNotStanding(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	for _, n := range []Nudge{
		{Text: "once", Lane: "lane-a"},
		{Text: "every tick", Standing: true},
	} {
		if err := AppendNudge(dir, n); err != nil {
			t.Fatal(err)
		}
	}

	first, err := NudgesFor(dir, "lane-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 || first[0].Text != "once" {
		t.Fatalf("precondition: expected the one-shot nudge, got %v", first)
	}
	if err := ConsumeNudges(dir, first); err != nil {
		t.Fatal(err)
	}

	again, err := NudgesFor(dir, "lane-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != 0 {
		t.Errorf("a consumed nudge must not be re-delivered, got %v", again)
	}
	// The standing rule survives: re-applying it every tick is what makes a
	// correction outlive the compaction that would otherwise drop it.
	rules, err := StandingRules(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(rules) != 1 || rules[0] != "every tick" {
		t.Errorf("standing rules must survive consumption, got %v", rules)
	}
}

// Consuming is idempotent: a second call must not append forever or resurrect
// the nudge behind its own original record.
func TestConsumeNudgesIsIdempotent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := AppendNudge(dir, Nudge{Text: "once", Lane: "lane-a"}); err != nil {
		t.Fatal(err)
	}
	ns, err := NudgesFor(dir, "lane-a")
	if err != nil {
		t.Fatal(err)
	}
	for range 3 {
		if err := ConsumeNudges(dir, ns); err != nil {
			t.Fatal(err)
		}
	}
	pending, err := PendingNudges(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Errorf("the nudge must stay consumed, got %v", pending)
	}
}
