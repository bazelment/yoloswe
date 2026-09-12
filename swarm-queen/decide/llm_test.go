package decide

import (
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/swarm-queen/state"
)

func vetState() *state.State {
	return &state.State{
		Config: state.Config{
			Goal: "g", Base: "main", Target: "swarm/t",
			Phases: []state.Phase{{Name: "swe"}, {Name: "clean"}, {Name: "local-review"}},
		},
		Lanes: []*state.Lane{
			{ID: "running", Status: state.StatusRunning, Phase: "swe",
				Worktree: "/wt/running", Sessions: map[string]string{"swe": "s1", "swe2": "s2"}},
			{ID: "done", Status: state.StatusDone, Phase: "local-review"},
			{ID: "planned", Status: state.StatusPlanned},
		},
	}
}

// A model proposal is generated text. Every one of these is plausible-sounding
// and wrong, and each corresponds to a real failure from the mined runs.
func TestVetRejectsUnsafeProposals(t *testing.T) {
	t.Parallel()
	st := vetState()
	cases := []struct {
		name string
		want string
		d    Decision
	}{
		{
			name: "reap a running lane",
			d:    Decision{Lane: "running", Kind: KindReap},
			want: "not terminal",
		},
		{
			name: "spawn onto an occupied worktree",
			d:    Decision{Lane: "running", Kind: KindSpawn},
			want: "already running",
		},
		{
			name: "undeclared phase",
			d:    Decision{Lane: "done", Kind: KindAdvance, Phase: "rebase"},
			want: "not declared",
		},
		{
			name: "unknown lane",
			d:    Decision{Lane: "ghost", Kind: KindAdvance},
			want: "not in the ledger",
		},
		{
			name: "spawn unknown lane",
			d:    Decision{Lane: "ghost", Kind: KindSpawn},
			want: "not in the ledger",
		},
		{
			name: "rework reusing a round",
			d:    Decision{Lane: "running", Kind: KindRework, Phase: "swe", Round: 2},
			want: "would overwrite",
		},
	}
	for _, tc := range cases {
		accepted, rejected := Vet(st, Proposal{Decisions: []Decision{tc.d}}, DefaultInvariants())
		if len(accepted) != 0 {
			t.Errorf("%s: proposal was accepted", tc.name)
		}
		if len(rejected) != 1 || !strings.Contains(rejected[0], tc.want) {
			t.Errorf("%s: rejection = %v, want it to mention %q", tc.name, rejected, tc.want)
		}
	}
}

// A safe proposal passes, and is tagged as LLM-sourced so a bad decision can be
// traced to its origin.
func TestVetAcceptsSafeProposalAndTagsSource(t *testing.T) {
	t.Parallel()
	st := vetState()
	accepted, rejected := Vet(st, Proposal{Decisions: []Decision{
		{Lane: "running", Kind: KindRework, Phase: "swe", Round: 3, Reason: "review found a major issue"},
	}}, DefaultInvariants())

	if len(rejected) != 0 {
		t.Fatalf("safe proposal rejected: %v", rejected)
	}
	if len(accepted) != 1 {
		t.Fatalf("expected one accepted decision, got %d", len(accepted))
	}
	if accepted[0].Source != SourceLLM {
		t.Errorf("Source = %q, want %q", accepted[0].Source, SourceLLM)
	}
}

// Mixed proposals must be filtered, not rejected wholesale: one bad suggestion
// should not discard a good one.
func TestVetFiltersRatherThanRejectingWholesale(t *testing.T) {
	t.Parallel()
	st := vetState()
	accepted, rejected := Vet(st, Proposal{Decisions: []Decision{
		{Lane: "running", Kind: KindReap},                // unsafe
		{Lane: "planned", Kind: KindSpawn, Phase: "swe"}, // safe
	}}, DefaultInvariants())

	if len(accepted) != 1 || accepted[0].Lane != "planned" {
		t.Errorf("accepted = %+v, want just the planned spawn", accepted)
	}
	if len(rejected) != 1 {
		t.Errorf("rejected = %v, want one", rejected)
	}
}

// A reply that is not decodable JSON must error. Reading it as an empty proposal
// would look exactly like a model that correctly found nothing to do.
func TestParseProposalRejectsNonJSON(t *testing.T) {
	t.Parallel()
	if _, err := ParseProposal("I think you should probably reap lane a."); err == nil {
		t.Error("prose must not parse as an empty proposal")
	}
}

func TestParseProposalToleratesFencedJSON(t *testing.T) {
	t.Parallel()
	p, err := ParseProposal("```json\n{\"decisions\":[{\"lane\":\"a\",\"kind\":\"spawn\"}]}\n```")
	if err != nil {
		t.Fatalf("fenced JSON should parse: %v", err)
	}
	if len(p.Decisions) != 1 || p.Decisions[0].Lane != "a" {
		t.Errorf("parsed %+v", p.Decisions)
	}
}

// "I don't know" is a valid answer and must survive the round trip.
func TestParseProposalKeepsUnresolved(t *testing.T) {
	t.Parallel()
	p, err := ParseProposal(`{"decisions":[],"unresolved":"cannot tell whether the finding is major"}`)
	if err != nil {
		t.Fatal(err)
	}
	if p.Unresolved == "" {
		t.Error("an explicit non-answer must be preserved, not discarded")
	}
}

// The digest must carry what a decision depends on -- notably a stale approval,
// which is the fact most likely to be wrongly assumed.
func TestDigestSurfacesStaleApproval(t *testing.T) {
	t.Parallel()
	st := vetState()
	st.Lanes[0].PR = 11968
	st.Lanes[0].PRHead = "135c17a2"
	st.Lanes[0].ApprovalSHA = "fa365c16"
	st.Lanes[0].Checks = "passing"

	d := Digest(st, nil, []string{"never enable auto-merge"})
	for _, want := range []string{"approval=STALE", "pr=11968", "never enable auto-merge", "swe -> clean"} {
		if !strings.Contains(d, want) {
			t.Errorf("digest missing %q:\n%s", want, d)
		}
	}
}

func TestDigestIncludesOpenQuestions(t *testing.T) {
	t.Parallel()
	d := Digest(vetState(), []Escalation{{
		ID: "a:q", Lane: "a", Question: "merge without approval?",
	}}, nil)
	if !strings.Contains(d, "merge without approval?") {
		t.Errorf("open questions must reach the advisor:\n%s", d)
	}
}
