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
	"time"

	"github.com/bazelment/yoloswe/orch/lane"
	"github.com/bazelment/yoloswe/orch/store"
)

//go:embed testdata/*.json
var embeddedTestdata embed.FS

// DefaultDatasetPath is the embedded testdata path for the full Gate 2 dataset.
const DefaultDatasetPath = "testdata/gate2_dataset.json"

// InnerLoopDatasetPath is the embedded testdata path for the 5 hand-checked inner-loop moments.
const InnerLoopDatasetPath = "testdata/inner_loop_5.json"

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

// BaselineDecision records what the legacy shell/prompt orchestrator did at a moment.
type BaselineDecision struct {
	Tool       string `json:"tool"`
	Target     string `json:"target,omitempty"`
	Summary    string `json:"summary"`
	AskedOwner bool   `json:"asked_owner,omitempty"`
}

// OwnerFollowUp records the owner's subsequent reaction and ground-truth expectation.
// When Verdict is VerdictCorrected, the owner correction marks the baseline wrong
// and states the expected decision.
type OwnerFollowUp struct {
	Verdict         Verdict  `json:"verdict"`
	OwnerNote       string   `json:"owner_note,omitempty"`
	ExpectedTools   []string `json:"expected_tools"`
	ExpectedTargets []string `json:"expected_targets,omitempty"`
	DecideDontAsk   bool     `json:"decide_dont_ask,omitempty"`
	ForbidOwnerAsk  bool     `json:"forbid_owner_ask,omitempty"`
}

// JudgmentMoment is a single historical judgment batch extracted from a past Nebula swarm run.
type JudgmentMoment struct {
	Timestamp     time.Time        `json:"timestamp"`
	State         MomentState      `json:"state"`
	ID            string           `json:"id"`
	RunID         string           `json:"run_id"`
	Category      string           `json:"category,omitempty"`
	Description   string           `json:"description"`
	Events        []store.Event    `json:"events"`
	Baseline      BaselineDecision `json:"baseline"`
	OwnerFollowUp OwnerFollowUp    `json:"owner_follow_up"`
}

// FactGroup represents a verifiable ground-truth fact from Store state that must
// appear in a Concierge status answer. At least one token in AnyOf must match
// case-insensitively.
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
}

// Dataset holds the extracted historical Validation Gate 2 dataset.
type Dataset struct {
	Version   string              `json:"version"`
	Runs      []string            `json:"runs"`
	Moments   []JudgmentMoment    `json:"moments"`
	Questions []ConciergeQuestion `json:"questions"`
}

// LoadDefaultDataset loads the embedded full Gate 2 dataset.
func LoadDefaultDataset() (*Dataset, error) {
	return LoadEmbeddedDataset(DefaultDatasetPath)
}

// LoadInnerLoopDataset loads the embedded 5-moment inner-loop dataset.
func LoadInnerLoopDataset() (*Dataset, error) {
	return LoadEmbeddedDataset(InnerLoopDatasetPath)
}

// LoadEmbeddedDataset loads a dataset from the embedded testdata filesystem.
func LoadEmbeddedDataset(name string) (*Dataset, error) {
	f, err := embeddedTestdata.Open(name)
	if err != nil {
		return nil, fmt.Errorf("orch/replay: open embedded %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	return LoadDatasetFromReader(f)
}

// LoadDatasetFromFile loads a Dataset from a JSON file on disk.
func LoadDatasetFromFile(path string) (*Dataset, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("orch/replay: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	return LoadDatasetFromReader(f)
}

// LoadDatasetFromReader decodes and validates a Dataset from r.
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
	}
	return nil
}

var (
	reEmail   = regexp.MustCompile(`[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`)
	reRawUUID = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}\b`)
	reToken   = regexp.MustCompile(`(?i)(?:ghp_[A-Za-z0-9]{16,}|ghs_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{16,}|sk-ant-[A-Za-z0-9\-_]{12,}|neb_[A-Za-z0-9\-_]{12,}|xox[baprs]-[A-Za-z0-9\-]{10,}|AKIA[0-9A-Z]{16}|Bearer\s+[A-Za-z0-9._\-]{20,})`)
)

// ValidateScrubbedBytes verifies that raw dataset JSON contains no unscrubbed emails,
// credential tokens, or raw 36-char session/account/terminal UUIDs.
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

// ScrubText replaces any tokens, email addresses, or raw UUIDs in s with deterministic
// scrubbed placeholders.
func ScrubText(s string) string {
	s = reToken.ReplaceAllString(s, "<scrubbed-token>")
	s = reEmail.ReplaceAllString(s, "<scrubbed-email>")
	s = reRawUUID.ReplaceAllString(s, "<scrubbed-id>")
	return s
}
