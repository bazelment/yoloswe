package replay_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/orch/replay"
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
	cases := map[string]struct {
		mutate  func(ds *replay.Dataset)
		wantErr string
	}{
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

func TestDatasetValidateRequiresSourceOnVerifiedMoments(t *testing.T) {
	t.Parallel()
	inner, err := replay.LoadInnerLoopDataset()
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(m *replay.JudgmentMoment){
		"no source ids":    func(m *replay.JudgmentMoment) { m.Source.IDs = nil },
		"unknown relation": func(m *replay.JudgmentMoment) { m.Source.Relation = "inferred" },
		"unknown evidence": func(m *replay.JudgmentMoment) { m.Source.Evidence = "orchestrator_did_it" },
		"quote evidence, no quote": func(m *replay.JudgmentMoment) {
			m.Source.Evidence = replay.EvidenceOwnerQuote
			m.Source.OwnerQuote = ""
		},
		"no label basis":     func(m *replay.JudgmentMoment) { m.Source.LabelBasis = "" },
		"no baseline source": func(m *replay.JudgmentMoment) { m.Baseline.SourceIDs = nil },
		"owner correction shown to the model": func(m *replay.JudgmentMoment) {
			m.Source.Relation = replay.RelationCorrection
			m.Source.Evidence = replay.EvidenceOwnerQuote
			m.Source.OwnerQuote = "the owner decided already"
			m.Events[0].Summary += " the owner decided already"
		},
	}
	for name, fn := range cases {
		ds := cloneDataset(t, inner)
		fn(&ds.Moments[0])
		if err := ds.Validate(); err == nil {
			t.Errorf("%s: Validate accepted the moment", name)
		}
	}
}
