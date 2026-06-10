package meetingbot

import (
	"context"
	"errors"
	"time"

	"github.com/bazelment/yoloswe/multiagent/agent"
)

// AgentRole identifies the specialized agent layer used by the meeting bot.
type AgentRole string

const (
	RoleFastAnswer       AgentRole = "fast-answer"
	RoleSummary          AgentRole = "summary"
	RoleInternalResearch AgentRole = "internal-research"
	RoleCodebaseResearch AgentRole = "codebase-research"
	RoleWebResearch      AgentRole = "web-research"
)

// Profile names a supported configuration posture.
type Profile string

const (
	ProfileDefault  Profile = "default"
	ProfileReplay   Profile = "replay"
	ProfileLiveSafe Profile = "live-safe"
	ProfileLiveWeb  Profile = "live-web"
)

var (
	errAutoResearchScheduler = errors.New("auto research requires ResearchScheduler")
	errLiveAutoResearch      = errors.New("live profiles require AutoResearch=false")
	errLiveSafeWeb           = errors.New("live-safe profile cannot include web research scope")
	errUnknownProfile        = errors.New("unknown meetingbot profile")
)

// ResearchScope names the research corpus consulted by a background agent.
type ResearchScope string

const (
	ScopeInternal ResearchScope = "internal"
	ScopeCodebase ResearchScope = "codebase"
	ScopeWeb      ResearchScope = "web"
)

// OutputStatus distinguishes validated output from explicit degraded fallback.
type OutputStatus string

const (
	OutputStatusNormal   OutputStatus = "normal"
	OutputStatusDegraded OutputStatus = "degraded"
	OutputStatusInvalid  OutputStatus = "invalid"
)

// ValidationResult captures the deterministic validation profile used by v1.
type ValidationResult struct {
	Status        OutputStatus
	Reason        string
	MissingInputs []string
}

// EvidenceStatus records whether one scope/topic produced useful research.
type EvidenceStatus string

const (
	EvidenceStatusSuccess EvidenceStatus = "success"
	EvidenceStatusEmpty   EvidenceStatus = "empty"
	EvidenceStatusFailed  EvidenceStatus = "failed"
)

// CoverageState records whether summary research coverage is fresh enough.
type CoverageState string

const (
	CoverageNotSearched CoverageState = "not_searched"
	CoverageFresh       CoverageState = "fresh"
	CoverageStale       CoverageState = "stale"
	CoverageEmpty       CoverageState = "empty"
	CoverageFailed      CoverageState = "failed"
)

// SummaryCoverage describes one selected topic/scope coverage decision.
type SummaryCoverage struct {
	Topic  string
	Scope  ResearchScope
	State  CoverageState
	Reason string
}

// ResearchJob is the queue handoff format for live-safe background research.
type ResearchJob struct {
	Topic           string
	Scopes          []ResearchScope
	TranscriptStart int
	TranscriptEnd   int
}

// ResearchSnapshot is the immutable transcript input for queued research.
type ResearchSnapshot struct {
	Events []MeetingEvent
}

// ResearchWork is the complete queue payload for one background research unit.
type ResearchWork struct {
	Snapshot ResearchSnapshot
	Job      ResearchJob
}

// ResearchScheduler queues research work outside the transcript hot path.
type ResearchScheduler interface {
	Enqueue(ctx context.Context, work ResearchWork) error
}

// ResearchExecutor runs a queued research payload.
type ResearchExecutor interface {
	RunResearch(ctx context.Context, work ResearchWork) ([]Evidence, error)
}

// AnswerStream makes the live opening/final write boundary explicit.
type AnswerStream interface {
	OnOpening(ctx context.Context, opening string, t time.Time) error
	OnFinal(ctx context.Context, answer Answer, t time.Time) error
}

// Config controls agent selection, timeouts, and background research volume.
type Config struct { //nolint:govet // fieldalignment: keep related knobs grouped for callers.
	Now                  func() time.Time
	WorkDir              string
	Profile              Profile
	FastAnswerModel      string
	ResearchModel        string
	CodeResearchModel    string
	WebResearchModel     string
	SummaryModel         string
	FastAnswerEffort     agent.EffortLevel
	ResearchEffort       agent.EffortLevel
	SummaryEffort        agent.EffortLevel
	FastAnswerTimeout    time.Duration
	ResearchTimeout      time.Duration
	SummaryTimeout       time.Duration
	ResearchConcurrency  int
	AnswerConcurrency    int
	ResearchChunkEvents  int
	MaxResearchTopics    int
	MaxSnippetsPerPrompt int
	AutoResearch         bool
	ResearchScopes       []ResearchScope
	ResearchScheduler    ResearchScheduler
}

// DefaultConfig returns development/evaluation defaults. Fast answers use a low
// effort Codex model; deeper research and summaries can use slower models.
func DefaultConfig() Config {
	return Config{
		WorkDir:              ".",
		Profile:              ProfileDefault,
		FastAnswerModel:      "gpt-5.4-mini",
		ResearchModel:        "sonnet",
		CodeResearchModel:    "gpt-5.4",
		WebResearchModel:     "gpt-5.4",
		SummaryModel:         "gpt-5.5",
		FastAnswerEffort:     agent.EffortLow,
		ResearchEffort:       agent.EffortMedium,
		SummaryEffort:        agent.EffortHigh,
		FastAnswerTimeout:    8 * time.Second,
		ResearchTimeout:      90 * time.Second,
		SummaryTimeout:       2 * time.Minute,
		ResearchConcurrency:  4,
		AnswerConcurrency:    4,
		ResearchChunkEvents:  40,
		MaxResearchTopics:    4,
		MaxSnippetsPerPrompt: 18,
		AutoResearch:         false,
		ResearchScopes:       []ResearchScope{ScopeInternal, ScopeCodebase, ScopeWeb},
	}
}

// ReplayConfig returns deterministic replay/evaluation defaults.
func ReplayConfig() Config {
	cfg := DefaultConfig()
	cfg.Profile = ProfileReplay
	return cfg
}

// LiveSafeConfig returns defaults for live transcript intake without web or
// synchronous provider work on the Observe path.
func LiveSafeConfig() Config {
	cfg := DefaultConfig()
	cfg.Profile = ProfileLiveSafe
	cfg.AutoResearch = false
	cfg.ResearchScopes = []ResearchScope{ScopeInternal, ScopeCodebase}
	return cfg
}

// LiveWebConfig returns live-safe defaults with web scope admitted by the
// caller's provider capability checks.
func LiveWebConfig() Config {
	cfg := LiveSafeConfig()
	cfg.Profile = ProfileLiveWeb
	cfg.ResearchScopes = []ResearchScope{ScopeInternal, ScopeCodebase, ScopeWeb}
	return cfg
}

func normalizeConfig(cfg Config) Config {
	def := DefaultConfig()
	if cfg.WorkDir == "" {
		cfg.WorkDir = def.WorkDir
	}
	if cfg.Profile == "" {
		cfg.Profile = def.Profile
	}
	if cfg.FastAnswerModel == "" {
		cfg.FastAnswerModel = def.FastAnswerModel
	}
	if cfg.ResearchModel == "" {
		cfg.ResearchModel = def.ResearchModel
	}
	if cfg.CodeResearchModel == "" {
		cfg.CodeResearchModel = def.CodeResearchModel
	}
	if cfg.WebResearchModel == "" {
		cfg.WebResearchModel = def.WebResearchModel
	}
	if cfg.SummaryModel == "" {
		cfg.SummaryModel = def.SummaryModel
	}
	if cfg.FastAnswerEffort == "" {
		cfg.FastAnswerEffort = def.FastAnswerEffort
	}
	if cfg.ResearchEffort == "" {
		cfg.ResearchEffort = def.ResearchEffort
	}
	if cfg.SummaryEffort == "" {
		cfg.SummaryEffort = def.SummaryEffort
	}
	if cfg.FastAnswerTimeout == 0 {
		cfg.FastAnswerTimeout = def.FastAnswerTimeout
	}
	if cfg.ResearchTimeout == 0 {
		cfg.ResearchTimeout = def.ResearchTimeout
	}
	if cfg.SummaryTimeout == 0 {
		cfg.SummaryTimeout = def.SummaryTimeout
	}
	if cfg.ResearchConcurrency == 0 {
		cfg.ResearchConcurrency = def.ResearchConcurrency
	}
	if cfg.AnswerConcurrency == 0 {
		cfg.AnswerConcurrency = def.AnswerConcurrency
	}
	if cfg.ResearchChunkEvents == 0 {
		cfg.ResearchChunkEvents = def.ResearchChunkEvents
	}
	if cfg.MaxResearchTopics == 0 {
		cfg.MaxResearchTopics = def.MaxResearchTopics
	}
	if cfg.MaxSnippetsPerPrompt == 0 {
		cfg.MaxSnippetsPerPrompt = def.MaxSnippetsPerPrompt
	}
	if len(cfg.ResearchScopes) == 0 {
		switch cfg.Profile {
		case ProfileLiveSafe:
			cfg.ResearchScopes = []ResearchScope{ScopeInternal, ScopeCodebase}
		default:
			cfg.ResearchScopes = append([]ResearchScope(nil), def.ResearchScopes...)
		}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return cfg
}

// Validate rejects profile combinations that would violate the architecture's
// live/replay boundary.
func (cfg Config) Validate() error {
	switch cfg.Profile {
	case "", ProfileDefault, ProfileReplay:
	case ProfileLiveSafe:
		if cfg.AutoResearch {
			return errLiveAutoResearch
		}
		if containsScope(cfg.ResearchScopes, ScopeWeb) {
			return errLiveSafeWeb
		}
	case ProfileLiveWeb:
		if cfg.AutoResearch {
			return errLiveAutoResearch
		}
	default:
		return errUnknownProfile
	}
	if cfg.AutoResearch && cfg.ResearchScheduler == nil {
		return errAutoResearchScheduler
	}
	return nil
}

// MeetingEvent is one transcript turn.
type MeetingEvent struct { //nolint:govet // fieldalignment: chronological fields are clearer first.
	Start   time.Duration
	End     time.Duration
	Speaker string
	Text    string
	Raw     string
	Index   int
}

// Topic is a candidate subject for background research.
type Topic struct {
	Name  string
	Score int
}

// Evidence is a research result cached for answer and summary synthesis.
type Evidence struct { //nolint:govet // fieldalignment: user-facing metadata stays grouped.
	CreatedAt  time.Time
	Scope      ResearchScope
	Topic      string
	Text       string
	Sources    []string
	Status     EvidenceStatus
	Error      string
	StartIndex int
	EndIndex   int
}

// Answer is the result of a user question during a meeting.
type Answer struct { //nolint:govet // fieldalignment: user-facing fields stay grouped.
	OpeningReadinessLatency    time.Duration
	TimeToFinalValidatedAnswer time.Duration
	Question                   string
	Opening                    string
	Text                       string
	Model                      string
	Error                      string
	Status                     OutputStatus
	Validation                 ValidationResult
	Evidence                   []Evidence
	ResearchRefs               []string
}

// Summary is the post-meeting synthesis cross-referenced with research.
type Summary struct { //nolint:govet // fieldalignment: user-facing fields stay grouped.
	Latency    time.Duration
	Text       string
	Model      string
	Error      string
	Status     OutputStatus
	Validation ValidationResult
	Evidence   []Evidence
	Coverage   []SummaryCoverage
}

// AgentRequest is the provider-neutral request sent to one agent layer.
type AgentRequest struct { //nolint:govet // fieldalignment: request fields mirror CLI concepts.
	Timeout        time.Duration
	Role           AgentRole
	Model          string
	Question       string
	Opening        string
	Effort         agent.EffortLevel
	Prompt         string
	SystemPrompt   string
	WorkDir        string
	PermissionMode string
}

// AgentResponse is the provider-neutral response from an agent layer.
type AgentResponse struct { //nolint:govet // fieldalignment: response fields mirror request fields.
	Latency  time.Duration
	Text     string
	Model    string
	Provider string
	Usage    agent.AgentUsage
}

// AgentClient runs one agent request. Production uses ProviderAgentClient;
// tests and offline demos use LocalAgentClient.
type AgentClient interface {
	Run(ctx context.Context, req AgentRequest) (AgentResponse, error)
}
