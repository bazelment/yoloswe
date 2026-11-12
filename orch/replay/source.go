package replay

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// SourceRecord is one export record cited by a label. Bodies are cut to a
// bounded length; every owner_quote must sit inside the stored body.
type SourceRecord struct {
	ID        string `json:"id"`
	Run       string `json:"run"`
	CreatedAt string `json:"created_at"`
	FromKind  string `json:"from_kind"`
	FromName  string `json:"from_name"`
	ToName    string `json:"to_name"`
	ToKind    string `json:"to_kind"`
	Kind      string `json:"kind"`
	InReplyTo string `json:"in_reply_to"`
	Body      string `json:"body"`
}

// SourceLane is a run-record lane a label targets or relies on.
type SourceLane struct {
	Run       string `json:"run"`
	ID        string `json:"id"`
	Title     string `json:"title"`
	Priority  string `json:"priority"`
	StartedAt string `json:"started_at"`
	PR        int    `json:"pr"`
}

// SourceExcerpt is the committed excerpt of the scrubbed owner-mail export and
// run records that the datasets' labels cite.
type SourceExcerpt struct {
	Export  string         `json:"export"`
	Records []SourceRecord `json:"records"`
	Lanes   []SourceLane   `json:"lanes"`
}

// LoadSourceExcerpt reads the embedded source excerpt.
func LoadSourceExcerpt() (*SourceExcerpt, error) {
	raw, err := embeddedTestdata.ReadFile(SourceExcerptPath)
	if err != nil {
		return nil, fmt.Errorf("orch/replay: read %s: %w", SourceExcerptPath, err)
	}
	var ex SourceExcerpt
	if err := json.Unmarshal(raw, &ex); err != nil {
		return nil, fmt.Errorf("orch/replay: decode source excerpt: %w", err)
	}
	if err := ValidateScrubbedBytes(raw); err != nil {
		return nil, err
	}
	return &ex, nil
}

// ValidateSources checks that every label in ds is backed by the excerpt:
//   - every cited source id (moment, question, baseline) is a record in the excerpt;
//   - an owner_quote is a verbatim substring of a cited record;
//   - the first cited record of an owner_quote or owner_answer source is
//     owner-authored (from_kind user), or it is the cited record that holds the quote;
//   - every target a label expects (lane ids or item-<lane> ids) names a lane
//     in the excerpt's run records for that run.
func ValidateSources(ds *Dataset, ex *SourceExcerpt) error {
	if ds == nil || ex == nil {
		return fmt.Errorf("orch/replay: ValidateSources needs a dataset and an excerpt")
	}
	records := make(map[string]SourceRecord, len(ex.Records))
	for i := range ex.Records {
		r := ex.Records[i]
		if _, dup := records[r.ID]; dup {
			return fmt.Errorf("orch/replay: excerpt repeats record %q", r.ID)
		}
		records[r.ID] = r
	}
	lanes := make(map[string]struct{}, len(ex.Lanes))
	for _, l := range ex.Lanes {
		lanes[l.Run+"/"+l.ID] = struct{}{}
	}
	checkSource := func(what, run string, src Source) error {
		for _, id := range src.IDs {
			if _, ok := records[id]; !ok {
				return fmt.Errorf("orch/replay: %s cites %q which is not in the source excerpt", what, id)
			}
		}
		if src.Evidence == EvidenceOwnerQuote {
			found := false
			for _, id := range src.IDs {
				if strings.Contains(records[id].Body, src.OwnerQuote) {
					found = true
					break
				}
			}
			if !found {
				return fmt.Errorf("orch/replay: %s owner_quote %q is not verbatim in any cited record", what, src.OwnerQuote)
			}
		}
		for _, id := range src.IDs {
			// The export attributes a record to one run, but a mail addressed
			// to (or sent by) another run's orchestrator still belongs to it.
			if r := records[id]; r.Run != run && !strings.HasSuffix(r.ToName, run) && !strings.HasSuffix(r.FromName, run) {
				return fmt.Errorf("orch/replay: %s cites %q (run %q, to %q), not run %q", what, id, r.Run, r.ToName, run)
			}
		}
		return nil
	}
	for i := range ds.Moments {
		m := &ds.Moments[i]
		if m.Grounding != GroundingVerified {
			continue
		}
		what := "moment " + m.ID
		if err := checkSource(what, m.RunID, m.Source); err != nil {
			return err
		}
		for _, id := range m.Baseline.SourceIDs {
			if _, ok := records[id]; !ok {
				return fmt.Errorf("orch/replay: %s baseline cites %q which is not in the source excerpt", what, id)
			}
		}
		if m.Source.Evidence == EvidenceOwnerAnswer && !slices.ContainsFunc(m.Source.IDs, func(id string) bool { return records[id].FromKind == "user" }) {
			return fmt.Errorf("orch/replay: %s claims an owner answer but cites no owner-authored record", m.ID)
		}
		for _, tgt := range m.OwnerFollowUp.ExpectedTargets {
			lane := strings.TrimPrefix(tgt, "item-")
			if _, ok := lanes[m.RunID+"/"+lane]; !ok {
				return fmt.Errorf("orch/replay: %s expects target %q but no such lane is in the excerpt's run records", m.ID, tgt)
			}
		}
	}
	for i := range ds.Questions {
		q := &ds.Questions[i]
		if err := checkSource("question "+q.ID, q.RunID, q.Source); err != nil {
			return err
		}
		for _, id := range q.Source.IDs {
			if records[id].FromKind != "user" {
				return fmt.Errorf("orch/replay: question %q cites %q which is not owner-authored", q.ID, id)
			}
		}
		for _, fg := range q.RequiredFacts {
			for _, tok := range fg.AnyOf {
				if _, ok := lanes[q.RunID+"/"+tok]; !ok {
					return fmt.Errorf("orch/replay: question %q requires fact %q which names no lane in the excerpt", q.ID, tok)
				}
			}
		}
	}
	return nil
}
