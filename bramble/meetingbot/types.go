package meetingbot

import (
	"context"
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

// ResearchScope names the research corpus consulted by a background agent.
type ResearchScope string

const (
	ScopeInternal ResearchScope = "internal"
	ScopeCodebase ResearchScope = "codebase"
	ScopeWeb      ResearchScope = "web"
)

// Config controls agent selection, timeouts, and background research volume.
type Config struct { //nolint:govet // fieldalignment: keep related knobs grouped for callers.
	Now                  func() time.Time
	WorkDir              string
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
	ResearchChunkEvents  int
	MaxResearchTopics    int
	MaxSnippetsPerPrompt int
	AutoResearch         bool
	ResearchScopes       []ResearchScope
}

// DefaultConfig returns production-oriented defaults. Fast answers use a low
// effort Codex model; deeper research and summaries can use slower models.
func DefaultConfig() Config {
	return Config{
		WorkDir:              ".",
		FastAnswerModel:      "gpt-5.3-codex",
		ResearchModel:        "sonnet",
		CodeResearchModel:    "gpt-5.3-codex",
		WebResearchModel:     "gpt-5.3-codex",
		SummaryModel:         "gpt-5.5",
		FastAnswerEffort:     agent.EffortLow,
		ResearchEffort:       agent.EffortMedium,
		SummaryEffort:        agent.EffortHigh,
		FastAnswerTimeout:    8 * time.Second,
		ResearchTimeout:      90 * time.Second,
		SummaryTimeout:       2 * time.Minute,
		ResearchChunkEvents:  40,
		MaxResearchTopics:    4,
		MaxSnippetsPerPrompt: 18,
		AutoResearch:         true,
		ResearchScopes:       []ResearchScope{ScopeInternal, ScopeCodebase, ScopeWeb},
	}
}

func normalizeConfig(cfg Config) Config {
	def := DefaultConfig()
	if cfg.WorkDir == "" {
		cfg.WorkDir = def.WorkDir
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
		cfg.ResearchScopes = append([]ResearchScope(nil), def.ResearchScopes...)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return cfg
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
type Evidence struct {
	CreatedAt time.Time
	Scope     ResearchScope
	Topic     string
	Text      string
	Sources   []string
}

// Answer is the result of a user question during a meeting.
type Answer struct { //nolint:govet // fieldalignment: user-facing fields stay grouped.
	First10WordsLatency time.Duration
	Question            string
	Opening             string
	Text                string
	Model               string
	Error               string
	Evidence            []Evidence
	ResearchRefs        []string
}

// Summary is the post-meeting synthesis cross-referenced with research.
type Summary struct { //nolint:govet // fieldalignment: user-facing fields stay grouped.
	Latency  time.Duration
	Text     string
	Model    string
	Error    string
	Evidence []Evidence
}

// AgentRequest is the provider-neutral request sent to one agent layer.
type AgentRequest struct { //nolint:govet // fieldalignment: request fields mirror CLI concepts.
	Timeout        time.Duration
	Role           AgentRole
	Model          string
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
