// Package replay implements Validation Gate 2 for orch/: historical decision
// replay dataset loading, scrubbing checks, and offline evaluation of the
// Dispatcher and Concierge against past Nebula swarm runs.
package replay

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/orch/lane"
	"github.com/bazelment/yoloswe/orch/store"
)

//go:embed testdata/*.json
var embeddedTestdata embed.FS

const (
	DefaultDatasetPath   = "testdata/gate2_dataset.json"
	InnerLoopDatasetPath = "testdata/inner_loop_10.json"
	// SourceExcerptPath holds the export records and run-record lanes every
	// label in the datasets cites; see ValidateSources.
	SourceExcerptPath = "testdata/source_excerpt.json"
)

// Verdict indicates whether the owner endorsed or corrected the legacy orchestrator's decision.
type Verdict string

const (
	VerdictEndorsed  Verdict = "endorsed"
	VerdictCorrected Verdict = "corrected"
)

// MomentState captures the store and driver state at the timestamp of a historical moment.
type MomentState struct {
	Reports    map[string]lane.PhaseReport `json:"reports,omitempty"`
	Run        store.Run                   `json:"run"`
	Directives []store.Directive           `json:"directives,omitempty"`
	Backlog    []store.BacklogItem         `json:"backlog,omitempty"`
	Lanes      []lane.Lane                 `json:"lanes,omitempty"`
	Decisions  []store.Decision            `json:"decisions,omitempty"`
}

// Grounding says whether a moment's expected decision is backed by the source
// run's own records. Only verified moments count toward the Gate 2 bars.
type Grounding string

const (
	// GroundingVerified means the event, target, timing and the old orchestrator's
	// action were checked against the source run's records (see Dataset.ProvenanceRubric).
	GroundingVerified Grounding = "verified"
	// GroundingSynthetic means the moment was authored or unchecked; it is run
	// and reported but never counted toward a bar.
	GroundingSynthetic Grounding = "synthetic"
)

// BaselineDecision records what the legacy shell/prompt orchestrator did at a moment.
type BaselineDecision struct {
	Tool    string `json:"tool"`
	Target  string `json:"target,omitempty"`
	Summary string `json:"summary"`
	// SourceIDs cite the export records (or run-record facts) that show what
	// the legacy orchestrator did; required on verified moments.
	SourceIDs  []string `json:"source_ids,omitempty"`
	AskedOwner bool     `json:"asked_owner,omitempty"`
}

// Relation says how the cited owner record bears on the label.
type Relation string

const (
	// RelationOrder: the owner (or a host relaying the owner) ordered the work.
	RelationOrder Relation = "order"
	// RelationCorrection: the owner corrected how the old orchestrator behaved.
	RelationCorrection Relation = "correction"
	// RelationFeedback: the owner rejected or criticised earlier output and asked for it again.
	RelationFeedback Relation = "feedback"
	// RelationAnswer: the owner answered a card the orchestrator put to them.
	RelationAnswer Relation = "answer"
	// RelationQuestion: the owner asked a status question.
	RelationQuestion Relation = "question"
)

// Evidence tiers for a source.
const (
	// EvidenceOwnerQuote: OwnerQuote is a verbatim owner statement found in a cited record.
	EvidenceOwnerQuote = "owner_quote"
	// EvidenceOwnerAnswer: the cited record is the owner's own answer to a card.
	EvidenceOwnerAnswer = "owner_answer"
	// EvidenceOwnerAttributed: a cited record explicitly attributes the decision to the owner.
	EvidenceOwnerAttributed = "owner_attributed"
)

// Source ties a label to the export records that justify it.
type Source struct {
	Relation   Relation `json:"relation"`
	Evidence   string   `json:"evidence"`
	OwnerQuote string   `json:"owner_quote,omitempty"`
	LabelBasis string   `json:"label_basis"`
	IDs        []string `json:"ids"`
}

// OwnerFollowUp records the owner's reaction and ground-truth expectation.
type OwnerFollowUp struct {
	Verdict         Verdict  `json:"verdict"`
	OwnerNote       string   `json:"owner_note,omitempty"`
	ExpectedTools   []string `json:"expected_tools"`
	ExpectedTargets []string `json:"expected_targets,omitempty"`
	DecideDontAsk   bool     `json:"decide_dont_ask,omitempty"`
	ForbidOwnerAsk  bool     `json:"forbid_owner_ask,omitempty"`
}

// ForbidsAsk reports whether the label makes an owner.ask a wrong answer.
func (f OwnerFollowUp) ForbidsAsk() bool { return f.ForbidOwnerAsk || f.DecideDontAsk }

// JudgmentMoment is a single historical judgment batch extracted from a past Nebula swarm run.
type JudgmentMoment struct {
	Timestamp   time.Time   `json:"timestamp"`
	State       MomentState `json:"state"`
	ID          string      `json:"id"`
	RunID       string      `json:"run_id"`
	Category    string      `json:"category,omitempty"`
	Description string      `json:"description"`
	// Grounding and GroundingEvidence record how this moment's label was checked.
	Grounding         Grounding     `json:"grounding"`
	GroundingEvidence string        `json:"grounding_evidence"`
	Events            []store.Event `json:"events"`
	// Source cites the export records behind the label (see ValidateSources).
	Source        Source           `json:"source"`
	Baseline      BaselineDecision `json:"baseline"`
	OwnerFollowUp OwnerFollowUp    `json:"owner_follow_up"`
}

// FactGroup requires at least one token in AnyOf to match a Concierge answer case-insensitively.
type FactGroup struct {
	Label string   `json:"label"`
	AnyOf []string `json:"any_of"`
}

// ConciergeQuestion is a historical owner status question paired with the run state at that time.
type ConciergeQuestion struct {
	Timestamp       time.Time   `json:"timestamp"`
	ID              string      `json:"id"`
	RunID           string      `json:"run_id"`
	Question        string      `json:"question"`
	RequiredFacts   []FactGroup `json:"required_facts"`
	ForbiddenClaims []string    `json:"forbidden_claims,omitempty"`
	State           MomentState `json:"state"`
	Source          Source      `json:"source"`
}

// Dataset holds the extracted historical Validation Gate 2 dataset.
type Dataset struct {
	// ProvenanceRubric states the checks a moment must pass to be verified.
	ProvenanceRubric string              `json:"provenance_rubric"`
	Version          string              `json:"version"`
	Runs             []string            `json:"runs"`
	Moments          []JudgmentMoment    `json:"moments"`
	Questions        []ConciergeQuestion `json:"questions"`
}

func LoadDefaultDataset() (*Dataset, error) {
	return LoadEmbeddedDataset(DefaultDatasetPath)
}

func LoadInnerLoopDataset() (*Dataset, error) {
	return LoadEmbeddedDataset(InnerLoopDatasetPath)
}

func LoadEmbeddedDataset(name string) (*Dataset, error) {
	f, err := embeddedTestdata.Open(name)
	if err != nil {
		return nil, fmt.Errorf("orch/replay: open embedded %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	return LoadDatasetFromReader(f)
}

func LoadDatasetFromFile(path string) (*Dataset, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("orch/replay: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return LoadDatasetFromReader(f)
}

func LoadDatasetFromReader(r io.Reader) (*Dataset, error) {
	raw, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("orch/replay: read dataset: %w", err)
	}
	var ds Dataset
	if err := json.Unmarshal(raw, &ds); err != nil {
		return nil, fmt.Errorf("orch/replay: decode dataset: %w", err)
	}
	if err := ds.Validate(); err != nil {
		return nil, err
	}
	if err := ValidateScrubbedBytes(raw); err != nil {
		return nil, err
	}
	return &ds, nil
}

// Validate checks structural invariants on the dataset.
func (ds *Dataset) Validate() error {
	if ds == nil {
		return errors.New("orch/replay: dataset is nil")
	}
	if len(ds.Moments) == 0 {
		return errors.New("orch/replay: dataset has no judgment moments")
	}
	if strings.TrimSpace(ds.ProvenanceRubric) == "" {
		return errors.New("orch/replay: dataset missing provenance_rubric")
	}
	seenMoments := make(map[string]struct{}, len(ds.Moments))
	for i := range ds.Moments {
		m := &ds.Moments[i]
		if m.ID == "" {
			return fmt.Errorf("orch/replay: moment[%d] missing id", i)
		}
		if _, dup := seenMoments[m.ID]; dup {
			return fmt.Errorf("orch/replay: duplicate moment id %q", m.ID)
		}
		seenMoments[m.ID] = struct{}{}
		if m.RunID == "" || m.State.Run.ID == "" || m.RunID != m.State.Run.ID {
			return fmt.Errorf("orch/replay: moment %q has mismatched run_id (%q vs %q)", m.ID, m.RunID, m.State.Run.ID)
		}
		if len(m.Events) == 0 {
			return fmt.Errorf("orch/replay: moment %q has no events", m.ID)
		}
		if m.Baseline.Tool == "" {
			return fmt.Errorf("orch/replay: moment %q missing baseline tool", m.ID)
		}
		if m.OwnerFollowUp.Verdict != VerdictEndorsed && m.OwnerFollowUp.Verdict != VerdictCorrected {
			return fmt.Errorf("orch/replay: moment %q invalid verdict %q", m.ID, m.OwnerFollowUp.Verdict)
		}
		if len(m.OwnerFollowUp.ExpectedTools) == 0 {
			return fmt.Errorf("orch/replay: moment %q missing expected_tools", m.ID)
		}
		if m.Grounding != GroundingVerified && m.Grounding != GroundingSynthetic {
			return fmt.Errorf("orch/replay: moment %q has grounding %q, want %q or %q", m.ID, m.Grounding, GroundingVerified, GroundingSynthetic)
		}
		if strings.TrimSpace(m.GroundingEvidence) == "" {
			return fmt.Errorf("orch/replay: moment %q missing grounding_evidence", m.ID)
		}
		if err := validateMomentNoLeakage(m); err != nil {
			return err
		}
		if m.Grounding == GroundingVerified {
			if err := validateMomentSource(m); err != nil {
				return err
			}
		}
	}

	seenQuestions := make(map[string]struct{}, len(ds.Questions))
	for i := range ds.Questions {
		q := &ds.Questions[i]
		if q.ID == "" {
			return fmt.Errorf("orch/replay: question[%d] missing id", i)
		}
		if _, dup := seenQuestions[q.ID]; dup {
			return fmt.Errorf("orch/replay: duplicate question id %q", q.ID)
		}
		seenQuestions[q.ID] = struct{}{}
		if q.RunID == "" || q.State.Run.ID == "" || q.RunID != q.State.Run.ID {
			return fmt.Errorf("orch/replay: question %q has mismatched run_id", q.ID)
		}
		if q.Question == "" {
			return fmt.Errorf("orch/replay: question %q has empty question text", q.ID)
		}
		if len(q.RequiredFacts) == 0 {
			return fmt.Errorf("orch/replay: question %q has no required_facts", q.ID)
		}
		if err := validateSourceShape("question "+q.ID, q.Source, RelationQuestion); err != nil {
			return err
		}
		if q.Source.Evidence != EvidenceOwnerQuote || !strings.Contains(q.Source.OwnerQuote, q.Question) {
			return fmt.Errorf("orch/replay: question %q must be a verbatim owner question (evidence owner_quote containing the question text)", q.ID)
		}
	}
	return nil
}

func validateSourceShape(what string, src Source, allowed ...Relation) error {
	if len(src.IDs) == 0 {
		return fmt.Errorf("orch/replay: %s has no source ids", what)
	}
	for _, id := range src.IDs {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("orch/replay: %s has an empty source id", what)
		}
	}
	if !slices.Contains(allowed, src.Relation) {
		return fmt.Errorf("orch/replay: %s has source relation %q, want one of %v", what, src.Relation, allowed)
	}
	switch src.Evidence {
	case EvidenceOwnerQuote:
		if strings.TrimSpace(src.OwnerQuote) == "" {
			return fmt.Errorf("orch/replay: %s claims owner_quote evidence without a quote", what)
		}
	case EvidenceOwnerAnswer, EvidenceOwnerAttributed:
	default:
		return fmt.Errorf("orch/replay: %s has unknown source evidence %q", what, src.Evidence)
	}
	if strings.TrimSpace(src.LabelBasis) == "" {
		return fmt.Errorf("orch/replay: %s has no label_basis", what)
	}
	return nil
}

// validateMomentSource requires a verified moment to cite its evidence and
// refuses to show the model the owner's correction or answer, which is the label.
func validateMomentSource(m *JudgmentMoment) error {
	what := "moment " + m.ID
	if err := validateSourceShape(what, m.Source, RelationOrder, RelationCorrection, RelationFeedback, RelationAnswer); err != nil {
		return err
	}
	if len(m.Baseline.SourceIDs) == 0 {
		return fmt.Errorf("orch/replay: %s baseline cites no source", what)
	}
	if m.Source.Relation == RelationCorrection || m.Source.Relation == RelationAnswer {
		quote := strings.ToLower(strings.TrimSpace(m.Source.OwnerQuote))
		if quote != "" {
			for j := range m.Events {
				if strings.Contains(strings.ToLower(m.Events[j].Summary), quote) {
					return fmt.Errorf("orch/replay: %s event %q shows the owner %s that is the label", m.ID, m.Events[j].ID, m.Source.Relation)
				}
			}
		}
	}
	return nil
}

var (
	reEmail          = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)
	reRawUUID        = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	reToken          = regexp.MustCompile(`(?i)(?:ghp_[A-Za-z0-9]{16,}|ghs_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{16,}|sk-ant-[A-Za-z0-9\-_]{12,}|neb_[A-Za-z0-9\-_]{12,}|xox[baprs]-[A-Za-z0-9\-]{10,}|AKIA[0-9A-Z]{16}|Bearer\s+[A-Za-z0-9._\-]{20,})`)
	reLeakedToolName = regexp.MustCompile(`(?i)\b(?:lane[._](?:spawn|steer|retry|stop)|backlog[._](?:add|update|reorder|close)|planner[._]request|directive[._]note|owner[._]ask|read[._](?:lane_report|pr|ci))\b`)
)

func validateMomentNoLeakage(m *JudgmentMoment) error {
	ownerNote := strings.ToLower(strings.TrimSpace(m.OwnerFollowUp.OwnerNote))
	for j := range m.Events {
		ev := &m.Events[j]
		if match := reLeakedToolName.FindString(ev.Summary); match != "" {
			return fmt.Errorf("orch/replay: moment %q event %q leaks tool name %q in summary", m.ID, ev.ID, match)
		}
		if match := reLeakedToolName.Find(ev.Payload); match != nil {
			return fmt.Errorf("orch/replay: moment %q event %q leaks tool name %q in payload", m.ID, ev.ID, string(match))
		}
		if ownerNote != "" && strings.Contains(strings.ToLower(ev.Summary), ownerNote) {
			return fmt.Errorf("orch/replay: moment %q event %q leaks owner_follow_up.owner_note in summary", m.ID, ev.ID)
		}
	}
	for j := range m.State.Directives {
		d := &m.State.Directives[j]
		if match := reLeakedToolName.FindString(d.Text); match != "" {
			return fmt.Errorf("orch/replay: moment %q directive %q leaks tool name %q in text", m.ID, d.ID, match)
		}
		if strings.Contains(strings.ToLower(d.ID), strings.ToLower(m.ID)) {
			return fmt.Errorf("orch/replay: moment %q directive %q is a per-moment leaked directive", m.ID, d.ID)
		}
		if ownerNote != "" && strings.Contains(strings.ToLower(d.Text), ownerNote) {
			return fmt.Errorf("orch/replay: moment %q directive %q leaks owner_follow_up.owner_note in text", m.ID, d.ID)
		}
	}
	return nil
}

// ValidateScrubbedBytes rejects raw dataset JSON containing unscrubbed tokens, emails, or UUIDs.
func ValidateScrubbedBytes(raw []byte) error {
	if match := reToken.Find(raw); match != nil {
		return fmt.Errorf("orch/replay: dataset contains unscrubbed token pattern %q", string(match))
	}
	if match := reEmail.Find(raw); match != nil {
		return fmt.Errorf("orch/replay: dataset contains unscrubbed email %q", string(match))
	}
	if match := reRawUUID.Find(raw); match != nil {
		return fmt.Errorf("orch/replay: dataset contains unscrubbed raw UUID %q", string(match))
	}
	return nil
}

// ScrubText replaces tokens, emails, and raw UUIDs in s with placeholders.
func ScrubText(s string) string {
	s = reToken.ReplaceAllString(s, "<scrubbed-token>")
	s = reEmail.ReplaceAllString(s, "<scrubbed-email>")
	s = reRawUUID.ReplaceAllString(s, "<scrubbed-id>")
	return s
}
