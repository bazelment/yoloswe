package replay_test

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/orca/replay"
)

func datasetsUnderTest(t *testing.T) map[string]*replay.Dataset {
	t.Helper()
	inner, err := replay.LoadInnerLoopDataset()
	if err != nil {
		t.Fatalf("LoadInnerLoopDataset: %v", err)
	}
	full, err := replay.LoadDefaultDataset()
	if err != nil {
		t.Fatalf("LoadDefaultDataset: %v", err)
	}
	return map[string]*replay.Dataset{"inner-loop-10": inner, "gate2-full": full}
}

// TestEveryLabelCitesARecordInTheSourceExcerpt: a label with no source id in
// the committed excerpt, a quote that is not verbatim, or a target lane that
// is not in the run records fails the build.
func TestEveryLabelCitesARecordInTheSourceExcerpt(t *testing.T) {
	t.Parallel()
	ex, err := replay.LoadSourceExcerpt()
	if err != nil {
		t.Fatal(err)
	}
	if len(ex.Records) == 0 || len(ex.Lanes) == 0 {
		t.Fatalf("empty source excerpt: %d records, %d lanes", len(ex.Records), len(ex.Lanes))
	}
	for name, ds := range datasetsUnderTest(t) {
		if err := replay.ValidateSources(ds, ex); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestInnerLoopIsASubsetOfTheFullDataset keeps the 10-moment inner loop honest:
// every moment and question in it is the same label as in the full dataset.
func TestInnerLoopIsASubsetOfTheFullDataset(t *testing.T) {
	t.Parallel()
	all := datasetsUnderTest(t)
	full := make(map[string]string)
	for _, m := range all["gate2-full"].Moments {
		b, _ := json.Marshal(m)
		full[m.ID] = string(b)
	}
	for _, q := range all["gate2-full"].Questions {
		b, _ := json.Marshal(q)
		full[q.ID] = string(b)
	}
	check := func(id string, v any) {
		b, _ := json.Marshal(v)
		if full[id] != string(b) {
			t.Errorf("inner-loop %s differs from the full dataset", id)
		}
	}
	for _, m := range all["inner-loop-10"].Moments {
		check(m.ID, m)
	}
	for _, q := range all["inner-loop-10"].Questions {
		check(q.ID, q)
	}
}

func cloneDataset(t *testing.T, ds *replay.Dataset) *replay.Dataset {
	t.Helper()
	raw, err := json.Marshal(ds)
	if err != nil {
		t.Fatal(err)
	}
	var out replay.Dataset
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	return &out
}

func TestSourceValidationRejectsUngroundedLabels(t *testing.T) {
	t.Parallel()
	ex, err := replay.LoadSourceExcerpt()
	if err != nil {
		t.Fatal(err)
	}
	full, err := replay.LoadDefaultDataset()
	if err != nil {
		t.Fatal(err)
	}
	quoted := -1
	for i, m := range full.Moments {
		if m.Source.Evidence == replay.EvidenceOwnerQuote && len(m.OwnerFollowUp.ExpectedTargets) > 0 {
			quoted = i
			break
		}
	}
	if quoted < 0 {
		t.Fatal("no quoted moment with a target to mutate")
	}
	nonOwner := ""
	for _, r := range ex.Records {
		if r.FromKind != "user" && r.Run == full.Questions[0].RunID {
			nonOwner = r.ID
			break
		}
	}
	if nonOwner == "" {
		t.Fatal("no non-owner record for the question's run in the excerpt")
	}
	oneWay := oneWayMoment(t, full)
	cases := map[string]struct {
		mutate  func(ds *replay.Dataset)
		wantErr string
	}{
		"owner answer with no attribution to the owner": {
			// Only the card: its from_kind is user, but nothing says the owner decided.
			mutate: func(ds *replay.Dataset) {
				ds.Moments[oneWay].Source.IDs = ds.Moments[oneWay].Source.IDs[:1]
			},
			wantErr: "attributes the answer to the owner",
		},
		"source id missing from the excerpt": {
			mutate:  func(ds *replay.Dataset) { ds.Moments[quoted].Source.IDs = []string{"s:00000000"} },
			wantErr: "not in the source excerpt",
		},
		"baseline source missing from the excerpt": {
			mutate:  func(ds *replay.Dataset) { ds.Moments[quoted].Baseline.SourceIDs = []string{"s:00000000"} },
			wantErr: "baseline cites",
		},
		"owner quote not verbatim": {
			mutate: func(ds *replay.Dataset) {
				ds.Moments[quoted].Source.OwnerQuote += " and also ship it on Friday"
			},
			wantErr: "not verbatim",
		},
		"target lane absent from the run records": {
			mutate: func(ds *replay.Dataset) {
				ds.Moments[quoted].OwnerFollowUp.ExpectedTargets = []string{"item-no-such-lane"}
			},
			wantErr: "no such lane",
		},
		"question not owner authored": {
			mutate:  func(ds *replay.Dataset) { ds.Questions[0].Source.IDs = append(ds.Questions[0].Source.IDs, nonOwner) },
			wantErr: "not owner-authored",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ds := cloneDataset(t, full)
			tc.mutate(ds)
			err := replay.ValidateSources(ds, ex)
			if err == nil {
				t.Fatal("ValidateSources accepted an ungrounded label")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not mention %q", err, tc.wantErr)
			}
		})
	}
}

// oneWayMoment returns the index of the first owner_only_one_way moment.
func oneWayMoment(t *testing.T, ds *replay.Dataset) int {
	t.Helper()
	i := slices.IndexFunc(ds.Moments, func(m replay.JudgmentMoment) bool { return m.Category == replay.CategoryOwnerOnlyOneWay })
	if i < 0 {
		t.Fatal("no owner_only_one_way moment")
	}
	return i
}

// TestOneWayLabelNeedsAOneWayCard: the card cited by an owner_only_one_way
// moment must be marked ONE-WAY. Two cards the first rebuild labelled this way
// said "Not one-way" and "(two-way)" in their own text.
func TestOneWayLabelNeedsAOneWayCard(t *testing.T) {
	t.Parallel()
	ex, err := replay.LoadSourceExcerpt()
	if err != nil {
		t.Fatal(err)
	}
	full, err := replay.LoadDefaultDataset()
	if err != nil {
		t.Fatal(err)
	}
	target := &full.Moments[oneWayMoment(t, full)]
	for name, card := range map[string]string{
		"not one-way":  "Ask 19 (by 23:00Z): S2 crash guarantee? Not one-way.",
		"two-way":      "Ask D1 (two-way). Why now: ONE-WAY for cost only if (c).",
		"never marked": "Q4: may S2 re-enable rely on keep-parked? (a) and (b) are reversible.",
		"one-way: no":  "Recommendation: (b). ONE-WAY: no.",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			mutated := *ex
			mutated.Records = slices.Clone(ex.Records)
			for i := range mutated.Records {
				if mutated.Records[i].ID == target.Source.IDs[0] {
					mutated.Records[i].Body = card
				}
			}
			err := replay.ValidateSources(full, &mutated)
			if err == nil || !strings.Contains(err.Error(), "not marked ONE-WAY") {
				t.Fatalf("ValidateSources = %v, want a 'not marked ONE-WAY' error", err)
			}
		})
	}
}

func TestDatasetValidateRequiresSourceOnVerifiedMoments(t *testing.T) {
	t.Parallel()
	inner, err := replay.LoadInnerLoopDataset()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		mutate  func(m *replay.JudgmentMoment)
		name    string
		wantErr string
	}{
		{name: "no source ids", wantErr: "no source ids", mutate: func(m *replay.JudgmentMoment) { m.Source.IDs = nil }},
		{name: "unknown relation", wantErr: "source relation", mutate: func(m *replay.JudgmentMoment) { m.Source.Relation = "inferred" }},
		{name: "unknown evidence", wantErr: "unknown source evidence", mutate: func(m *replay.JudgmentMoment) { m.Source.Evidence = "orchestrator_did_it" }},
		{name: "quote evidence, no quote", wantErr: "without a quote", mutate: func(m *replay.JudgmentMoment) {
			m.Source.Evidence = replay.EvidenceOwnerQuote
			m.Source.OwnerQuote = ""
		}},
		{name: "no label basis", wantErr: "no label_basis", mutate: func(m *replay.JudgmentMoment) { m.Source.LabelBasis = "" }},
		{name: "no baseline source", wantErr: "baseline cites no source", mutate: func(m *replay.JudgmentMoment) { m.Baseline.SourceIDs = nil }},
		{name: "owner correction shown to the model", wantErr: "shows the owner", mutate: func(m *replay.JudgmentMoment) {
			m.Source.Relation = replay.RelationCorrection
			m.Source.Evidence = replay.EvidenceOwnerQuote
			m.Source.OwnerQuote = "the owner decided already"
			m.Events[0].Summary += " the owner decided already"
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ds := cloneDataset(t, inner)
			tc.mutate(&ds.Moments[0])
			err := ds.Validate()
			if err == nil {
				t.Fatal("Validate accepted the moment")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err, tc.wantErr)
			}
		})
	}
}
