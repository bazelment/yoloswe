package jiradozer

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/llmendpoint"
	"github.com/bazelment/yoloswe/jiradozer/tracker"
	"github.com/bazelment/yoloswe/multiagent/agent"
)

// Config is the top-level configuration for jiradozer.
//
//nolint:govet // fieldalignment: keep YAML fields in config-file order; skipPhaseSource is internal metadata.
type Config struct {
	Tracker      TrackerConfig `yaml:"tracker"`
	Source       SourceConfig  `yaml:"source"`
	States       StatesConfig  `yaml:"states"`
	Agent        AgentConfig   `yaml:"agent"`
	WorkDir      string        `yaml:"work_dir"`
	BaseBranch   string        `yaml:"base_branch"`
	Plan         StepConfig    `yaml:"plan"`
	Build        StepConfig    `yaml:"build"`
	CreatePR     StepConfig    `yaml:"create_pr"`
	Validate     StepConfig    `yaml:"validate"`
	Ship         StepConfig    `yaml:"ship"`
	SkipPhases   []string      `yaml:"skip_phases"`
	MaxBudgetUSD float64       `yaml:"max_budget_usd"`
	PollInterval time.Duration `yaml:"poll_interval"`
	Notify       NotifyConfig  `yaml:"notify"`

	skipPhaseSource skipPhaseSource
}

// NotifyConfig configures where a run reports a hard failure (after retries are
// exhausted). All sinks are opt-in: an unset field disables that sink, and a
// notification failure never changes the run's exit path.
type NotifyConfig struct {
	// SlackWebhook is a Slack incoming-webhook URL. Supports "$ENV_VAR"
	// expansion (e.g. "$JIRADOZER_SLACK_WEBHOOK"). Empty disables Slack
	// notification.
	SlackWebhook string `yaml:"slack_webhook"`
}

type skipPhaseSource string

const (
	skipPhaseSourceConfig skipPhaseSource = "config"
	skipPhaseSourceCLI    skipPhaseSource = "cli"
	skipPhaseSourceLabel  skipPhaseSource = "label"
	skipPhaseSourceDone   skipPhaseSource = "done"
)

// TrackerConfig specifies the issue tracker backend.
type TrackerConfig struct {
	Kind   string `yaml:"kind"`    // "linear", "github", "local"
	APIKey string `yaml:"api_key"` // supports $ENV_VAR expansion
}

// AgentConfig specifies the agent backend.
//
//nolint:govet // fieldalignment: keep YAML fields in config-file order.
type AgentConfig struct {
	Model string `yaml:"model"` // model ID from agent.AllModels (e.g. "fable", "gpt-5.5")
	// FallbackModels are tried, in order, when the primary model fails with a
	// workspace-wide out-of-credits error. Each should name a model on a
	// *different* provider (a same-provider swap can't escape an out-of-credits
	// workspace). Failover starts a fresh session. Empty = no fallback.
	FallbackModels []string           `yaml:"fallback_models"`
	Effort         string             `yaml:"effort"`       // reasoning effort; see agent.EffortLevel constants (low, medium, high, max, auto)
	LLMEndpoint    *LLMEndpointConfig `yaml:"llm_endpoint"` // optional third-party LLM API endpoint
}

// LLMEndpointConfig points the underlying CLI at a third-party LLM endpoint
// (e.g. Baseten, OpenRouter, LiteLLM). All fields are optional; when set,
// the endpoint is applied via agent.WithProviderLLMEndpoint.
//
// Prefer api_key_env over api_key so the literal key never lands in the
// config file.
type LLMEndpointConfig struct {
	Headers      map[string]string `yaml:"headers"`       // optional extra HTTP headers (only honored where the wrapper supports them, e.g. codex http_headers)
	BaseURL      string            `yaml:"base_url"`      // e.g. "https://inference.baseten.co/v1"
	APIKey       string            `yaml:"api_key"`       // resolved literal (avoid; use api_key_env)
	APIKeyEnv    string            `yaml:"api_key_env"`   // env var holding the key (e.g. "BASETEN_API_KEY")
	ProviderName string            `yaml:"provider_name"` // codex model_providers.<name>; default "custom"
	WireAPI      string            `yaml:"wire_api"`      // "chat" (default, OpenAI-compat) | "responses"
}

// SourceConfig specifies how to discover issues for multi-issue mode.
type SourceConfig struct {
	Filters       map[string]string `yaml:"filters"`        // Generic key-value filters (see tracker.IssueFilter)
	BranchPrefix  string            `yaml:"branch_prefix"`  // Worktree branch prefix (default: "jiradozer")
	MaxConcurrent int               `yaml:"max_concurrent"` // Max parallel workflows (default: 3)
	DryRun        bool              `yaml:"dry_run"`        // Print equivalent bramble new-session command instead of launching a workflow
}

// ToFilter converts the source config to a tracker.IssueFilter.
func (s SourceConfig) ToFilter() tracker.IssueFilter {
	return tracker.IssueFilter{Filters: s.Filters}
}

// HasSource reports whether the source config specifies enough to enter
// multi-issue mode (at least one filter key must be set).
func (s SourceConfig) HasSource() bool {
	return len(s.Filters) > 0
}

// StepConfig configures a single workflow step (plan or build).
type StepConfig struct {
	Prompt               string             `yaml:"prompt"`                 // Go text/template; required unless rounds is set. No built-in default — run `jiradozer bootstrap` to scaffold.
	SystemPrompt         string             `yaml:"system_prompt"`          // optional system prompt passed to the agent
	Model                string             `yaml:"model"`                  // override agent.model; empty = inherit
	FallbackModels       []string           `yaml:"fallback_models"`        // override agent.fallback_models; nil = inherit. See AgentConfig.FallbackModels.
	Effort               string             `yaml:"effort"`                 // override agent.effort; empty = inherit
	PermissionMode       string             `yaml:"permission_mode"`        // "plan", "bypass", etc.; empty = step default
	CommentTemplate      string             `yaml:"comment_template"`       // text/template rendered with CommentData; required for single-shot steps (rounds-only steps may omit it)
	RoundCommentTemplate string             `yaml:"round_comment_template"` // text/template rendered with CommentData per round; required when rounds is non-empty
	LLMEndpoint          *LLMEndpointConfig `yaml:"llm_endpoint"`           // override agent.llm_endpoint; nil = inherit
	Rounds               []RoundConfig      `yaml:"rounds"`                 // multi-round execution; mutually exclusive with Prompt
	MaxBudgetUSD         float64            `yaml:"max_budget_usd"`         // override top-level; 0 = inherit
	MaxTurns             int                `yaml:"max_turns"`
	MaxToolErrorRetries  int                `yaml:"max_tool_error_retries"` // retries when a turn ends with an unresolved tool error; 0 = disabled
	TransientRetries     int                `yaml:"transient_retries"`      // retries when provider execution fails with a transient error; 0 = default (2)
	IdleTimeout          time.Duration      `yaml:"idle_timeout"`           // parent watchdog kills the subprocess if it emits no log line for this long while inside this step; 0 disables
	// StreamTurnGracePeriod overrides how long a turn waits, after completion,
	// for an outstanding background tool_use to terminate before the turn is
	// force-completed. Raise it for steps (e.g. /pr-polish) that background
	// long-running work like bramble reviewers; 0 = provider default.
	StreamTurnGracePeriod time.Duration `yaml:"stream_turn_grace_period"`
	AutoApprove           bool          `yaml:"auto_approve"` // skip human review after this step
}

// RoundConfig configures a single round within a multi-round step.
// Zero-value fields inherit from the parent StepConfig.
// Exactly one of Prompt or Command must be set.
type RoundConfig struct {
	Prompt              string  `yaml:"prompt"`                 // Go text/template; mutually exclusive with Command
	Command             string  `yaml:"command"`                // Shell command template (sh -c); mutually exclusive with Prompt. WARNING: tracker fields like Title/Description are user-controlled — only interpolate them when the issue source is fully trusted.
	SystemPrompt        string  `yaml:"system_prompt"`          // optional system prompt (agent rounds only)
	Model               string  `yaml:"model"`                  // override; empty = inherit from step (agent rounds only)
	MaxTurns            int     `yaml:"max_turns"`              // override; 0 = inherit from step (agent rounds only)
	MaxToolErrorRetries int     `yaml:"max_tool_error_retries"` // override; 0 = inherit from step (agent rounds only)
	MaxBudgetUSD        float64 `yaml:"max_budget_usd"`         // override; 0 = inherit from step (agent rounds only)
}

// IsCommand reports whether this round runs a shell command instead of an agent session.
func (r RoundConfig) IsCommand() bool {
	return r.Command != ""
}

// StatesConfig maps logical workflow states to tracker-specific state names.
type StatesConfig struct {
	InProgress string `yaml:"in_progress"`
	InReview   string `yaml:"in_review"`
	Done       string `yaml:"done"`
}

// LoadConfig reads and parses a jiradozer YAML config file.
func LoadConfig(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}

	cfg := defaultConfig()
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}

	// Expand environment variables in API key when present.
	// Local tracker has no API key; GitHub tracker uses gh CLI auth (API key optional).
	if cfg.Tracker.APIKey != "" {
		apiKey, err := resolveEnv(cfg.Tracker.APIKey)
		if err != nil {
			return nil, fmt.Errorf("tracker.api_key: %w", err)
		}
		cfg.Tracker.APIKey = apiKey
	}

	// Expand the Slack webhook env var when present. Notification is opt-in and
	// best-effort, so a "$VAR" that resolves to empty (unset) just disables the
	// sink rather than failing the load.
	if strings.HasPrefix(cfg.Notify.SlackWebhook, "$") {
		cfg.Notify.SlackWebhook = os.Getenv(strings.TrimPrefix(cfg.Notify.SlackWebhook, "$"))
	}

	// Expand ~ in work_dir.
	cfg.WorkDir = ExpandHome(cfg.WorkDir)

	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// DefaultConfig returns the framework defaults: tracker kind, agent model,
// branch prefix, work_dir, and step PermissionMode/MaxTurns. It does NOT
// seed prompts or comment templates — those live in jiradozer.yaml and
// must be supplied (via `jiradozer bootstrap`, LoadConfig, or explicit
// field assignment) before the result can pass validate() or feed into
// RunStepAgent. Library / test code that needs a runnable Config should
// load it from disk or layer prompts on top.
func DefaultConfig() *Config {
	cfg := defaultConfig()
	return &cfg
}

func defaultConfig() Config {
	return Config{
		Tracker: TrackerConfig{Kind: "linear"},
		Agent:   AgentConfig{Model: "sonnet"},
		Source: SourceConfig{
			MaxConcurrent: 3,
			BranchPrefix:  "jiradozer",
		},
		Plan:     StepConfig{PermissionMode: "plan", MaxTurns: 10},
		Build:    StepConfig{PermissionMode: "bypass", MaxTurns: 30},
		CreatePR: StepConfig{PermissionMode: "bypass", MaxTurns: 5},
		Validate: StepConfig{PermissionMode: "bypass", MaxTurns: 10},
		Ship:     StepConfig{PermissionMode: "bypass", MaxTurns: 10},
		States: StatesConfig{
			InProgress: "In Progress",
			InReview:   "In Review",
			Done:       "Done",
		},
		WorkDir:      ".",
		BaseBranch:   "main",
		PollInterval: 15 * time.Second,
		MaxBudgetUSD: 50.0,
	}
}

func (c *Config) validate() error {
	if c.Tracker.Kind == "" {
		return fmt.Errorf("tracker.kind is required")
	}
	if c.Tracker.Kind != "local" && c.Tracker.Kind != "github" && c.Tracker.APIKey == "" {
		return fmt.Errorf("tracker.api_key is required (set via config or environment variable)")
	}
	if c.Agent.Model == "" {
		return fmt.Errorf("agent.model is required")
	}
	if err := ValidateWorkDir(c.WorkDir); err != nil {
		return err
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 15 * time.Second
	}
	if c.Agent.Effort != "" {
		if _, err := agent.ParseEffort(c.Agent.Effort); err != nil {
			return fmt.Errorf("agent.effort: %w", err)
		}
	}
	if err := validateFallbackModels("agent.fallback_models", c.Agent.Model, c.Agent.FallbackModels); err != nil {
		return err
	}
	if err := c.Agent.LLMEndpoint.ToEndpoint().Validate(); err != nil {
		return fmt.Errorf("agent.llm_endpoint: %w", err)
	}
	skipPhases, err := NormalizeSkipPhases(c.SkipPhases)
	if err != nil {
		return fmt.Errorf("skip_phases: %w", err)
	}
	c.SkipPhases = skipPhases
	if len(c.SkipPhases) > 0 {
		if c.skipPhaseSource == "" {
			c.skipPhaseSource = skipPhaseSourceConfig
		}
	}
	for _, ns := range c.orderedSteps() {
		if err := validateStep(ns.name, ns.step); err != nil {
			return err
		}
		// Validate fallback models against the step's *resolved* primary model
		// (a step with its own model: override and an inherited fallback list,
		// or vice versa, must not name a fallback equal to its effective
		// primary). Only validate when the step actually sets fallbacks; an
		// inherited list is already validated at the agent level above.
		if len(ns.step.FallbackModels) > 0 {
			primary := ns.step.Model
			if primary == "" {
				primary = c.Agent.Model
			}
			if err := validateFallbackModels(ns.name+".fallback_models", primary, ns.step.FallbackModels); err != nil {
				return err
			}
		}
	}
	return nil
}

// validateFallbackModels checks that each fallback model ID resolves via
// agent.ModelByID and that none duplicates the primary model (a fallback equal
// to the primary can never help). label names the config field for error
// messages (e.g. "agent.fallback_models").
func validateFallbackModels(label, primary string, fallbacks []string) error {
	for _, m := range fallbacks {
		if m == "" {
			return fmt.Errorf("%s: fallback model must not be empty", label)
		}
		if _, ok := agent.ModelByID(m); !ok {
			return fmt.Errorf("%s: unknown model %q", label, m)
		}
		if m == primary {
			return fmt.Errorf("%s: fallback model %q must differ from the primary model", label, m)
		}
	}
	return nil
}

// NormalizeSkipPhases validates phase names, trims whitespace, removes
// duplicates, and preserves first-seen order.
func NormalizeSkipPhases(phases []string) ([]string, error) {
	seen := make(map[string]bool)
	out := make([]string, 0, len(phases))
	for _, phase := range phases {
		phase = strings.TrimSpace(phase)
		if phase == "" {
			continue
		}
		if startStepForPhase(phase) == StepInit {
			return nil, fmt.Errorf("unknown phase %q (valid: %s)", phase, strings.Join(validUserPhases(), ", "))
		}
		if seen[phase] {
			continue
		}
		seen[phase] = true
		out = append(out, phase)
	}
	return out, nil
}

// ApplySkipPhases replaces the configured skip set and records its source for
// workflow logs. This is used by CLI overrides after LoadConfig validation.
func (c *Config) ApplySkipPhases(phases []string, source string) error {
	normalized, err := NormalizeSkipPhases(phases)
	if err != nil {
		return err
	}
	parsedSource, err := parseSkipPhaseSource(source)
	if err != nil {
		return err
	}
	c.SkipPhases = normalized
	c.skipPhaseSource = parsedSource
	return nil
}

func (c *Config) skipSourceForPhase(phase string) skipPhaseSource {
	if c == nil {
		return ""
	}
	for _, skippedPhase := range c.SkipPhases {
		if skippedPhase == phase {
			if c.skipPhaseSource == "" {
				return skipPhaseSourceConfig
			}
			return c.skipPhaseSource
		}
	}
	return ""
}

func parseSkipPhaseSource(source string) (skipPhaseSource, error) {
	switch skipPhaseSource(source) {
	case skipPhaseSourceConfig, skipPhaseSourceCLI, skipPhaseSourceLabel:
		return skipPhaseSource(source), nil
	default:
		return "", fmt.Errorf("unknown skip phase source %q", source)
	}
}

func validUserPhases() []string {
	out := make([]string, 0, len(phaseTable))
	for _, p := range phaseTable {
		out = append(out, p.name)
	}
	return out
}

// validateStep checks one named step.
func validateStep(name string, step *StepConfig) error {
	if step.Effort != "" {
		if _, err := agent.ParseEffort(step.Effort); err != nil {
			return fmt.Errorf("%s.effort: %w", name, err)
		}
	}
	if err := step.LLMEndpoint.ToEndpoint().Validate(); err != nil {
		return fmt.Errorf("%s.llm_endpoint: %w", name, err)
	}
	// IdleTimeout treats 0 as "watchdog disabled by config"; negative
	// values would silently get the same behavior at runWatchdog,
	// turning a typo like `-5m` into a security hole. Reject loudly.
	if step.IdleTimeout < 0 {
		return fmt.Errorf("%s.idle_timeout: must not be negative (got %s); use 0 to disable", name, step.IdleTimeout)
	}
	// StreamTurnGracePeriod treats 0 as "use provider default"; a negative
	// value would be meaningless (and silently coerced away at the option
	// boundary, which only honors >0). Reject loudly so a typo surfaces.
	if step.StreamTurnGracePeriod < 0 {
		return fmt.Errorf("%s.stream_turn_grace_period: must not be negative (got %s); use 0 for the provider default", name, step.StreamTurnGracePeriod)
	}
	if name == "create_pr" && len(step.Rounds) > 0 {
		return fmt.Errorf("create_pr does not support rounds")
	}
	if step.Prompt != "" && len(step.Rounds) > 0 {
		return fmt.Errorf("%s: prompt and rounds are mutually exclusive", name)
	}
	if step.Prompt == "" && len(step.Rounds) == 0 {
		return fmt.Errorf("%s: prompt is required (run `jiradozer bootstrap` to generate a starter config)", name)
	}
	if step.Prompt != "" {
		if err := validatePromptTemplate(name+".prompt", step.Prompt); err != nil {
			return err
		}
	}
	// comment_template feeds runStep (single-shot steps) and is not rendered
	// by runStepRounds, so round-only steps don't need it. Single-shot steps
	// still require it; bootstrap seeds both for convenience.
	if len(step.Rounds) == 0 {
		if step.CommentTemplate == "" {
			return fmt.Errorf("%s: comment_template is required (run `jiradozer bootstrap` to generate a starter config)", name)
		}
	}
	if step.CommentTemplate != "" {
		if err := validateCommentTemplate(name+".comment_template", step.CommentTemplate); err != nil {
			return err
		}
	}
	if len(step.Rounds) > 0 && step.RoundCommentTemplate == "" {
		return fmt.Errorf("%s: round_comment_template is required when rounds is set (run `jiradozer bootstrap` to generate a starter config)", name)
	}
	// Validate round_comment_template whenever it is set, even on steps
	// with no rounds — bootstrap seeds it on every rounds-capable step,
	// and a typo there should fail at LoadConfig instead of waiting for
	// the day someone enables rounds.
	if step.RoundCommentTemplate != "" {
		if err := validateCommentTemplate(name+".round_comment_template", step.RoundCommentTemplate); err != nil {
			return err
		}
	}
	for i, round := range step.Rounds {
		if round.Prompt == "" && round.Command == "" {
			return fmt.Errorf("%s.rounds[%d]: prompt or command is required", name, i)
		}
		if round.Prompt != "" && round.Command != "" {
			return fmt.Errorf("%s.rounds[%d]: prompt and command are mutually exclusive", name, i)
		}
		if round.Prompt != "" {
			if err := validatePromptTemplate(fmt.Sprintf("%s.rounds[%d].prompt", name, i), round.Prompt); err != nil {
				return err
			}
		}
		if round.Command != "" {
			if err := validatePromptTemplate(fmt.Sprintf("%s.rounds[%d].command", name, i), round.Command); err != nil {
				return err
			}
		}
	}
	return nil
}

// validatePromptTemplate runs the eager validation pattern against
// PromptData: two Execute passes (zero-value and filled) so typos hidden
// behind a conditional branch (e.g. {{- if .X}}{{.Decsription}}{{- end}})
// can't sneak past — the zero-value pass would skip the false branch.
func validatePromptTemplate(label, tmpl string) error {
	return validateTemplate(label, tmpl, PromptData{}, samplePromptData)
}

// validateCommentTemplate is the CommentData counterpart of
// validatePromptTemplate; same two-pass strategy.
func validateCommentTemplate(label, tmpl string) error {
	return validateTemplate(label, tmpl, CommentData{}, sampleCommentData)
}

// validateTemplate parses tmpl once and runs Execute against each sample.
// All errors are wrapped with label so the caller sees which field failed
// (e.g. "plan.comment_template template: ...").
func validateTemplate(label, tmpl string, samples ...any) error {
	t, err := template.New(label).Parse(tmpl)
	if err != nil {
		return fmt.Errorf("%s template: %w", label, err)
	}
	for _, sample := range samples {
		if err := t.Execute(io.Discard, sample); err != nil {
			return fmt.Errorf("%s template: %w", label, err)
		}
	}
	return nil
}

// samplePromptData / sampleCommentData supply non-zero values so
// validation traverses {{- if .X}} branches that the zero-value pass
// would skip. Values are arbitrary strings of the right shape; only
// presence matters for branch coverage.
var (
	samplePromptData = PromptData{
		Identifier:  "ENG-1",
		Title:       "sample",
		Description: "sample description",
		URL:         "https://example.com/issue/1",
		Labels:      "bug",
		BaseBranch:  "main",
		Plan:        "sample plan",
		BuildOutput: "sample build output",
	}
	sampleCommentData = CommentData{
		Step:        "plan",
		Heading:     "Plan",
		Output:      "sample output",
		Round:       1,
		TotalRounds: 3,
	}
)

// namedStep pairs a step name with a pointer to its StepConfig on a Config.
type namedStep struct {
	step *StepConfig
	name string
}

// orderedSteps is the single source of truth for the workflow step set: the
// plan → build → create_pr → validate → ship sequence, each name paired with a
// pointer to its field on c. Every other step-name list (StepNames, StepByName,
// validate's iteration) derives from this so a new step can't be added in one
// place and forgotten in another.
func (c *Config) orderedSteps() []namedStep {
	return []namedStep{
		{&c.Plan, "plan"},
		{&c.Build, "build"},
		{&c.CreatePR, "create_pr"},
		{&c.Validate, "validate"},
		{&c.Ship, "ship"},
	}
}

// StepNames returns the canonical ordered workflow step names (plan → build →
// create_pr → validate → ship), the names StepByName accepts. It returns a
// fresh slice each call, so callers can iterate, join, or filter without
// risking mutation of shared state.
func StepNames() []string {
	steps := (&Config{}).orderedSteps()
	names := make([]string, len(steps))
	for i, s := range steps {
		names[i] = s.name
	}
	return names
}

// StepByName returns the StepConfig for a named step.
func (c *Config) StepByName(name string) (StepConfig, bool) {
	for _, s := range c.orderedSteps() {
		if s.name == name {
			return *s.step, true
		}
	}
	return StepConfig{}, false
}

// ResolveStep fills zero-value fields in a StepConfig from top-level defaults.
func (c *Config) ResolveStep(step StepConfig) StepConfig {
	if step.Model == "" {
		step.Model = c.Agent.Model
	}
	if step.FallbackModels == nil {
		step.FallbackModels = c.Agent.FallbackModels
	}
	if step.Effort == "" {
		step.Effort = c.Agent.Effort
	}
	if step.MaxBudgetUSD == 0 {
		step.MaxBudgetUSD = c.MaxBudgetUSD
	}
	if step.LLMEndpoint == nil {
		step.LLMEndpoint = c.Agent.LLMEndpoint
	}
	return step
}

// ToEndpoint converts the YAML config into a runtime llmendpoint.Endpoint.
func (l *LLMEndpointConfig) ToEndpoint() llmendpoint.Endpoint {
	if l == nil {
		return llmendpoint.Endpoint{}
	}
	return llmendpoint.Endpoint{
		BaseURL:      l.BaseURL,
		APIKey:       l.APIKey,
		APIKeyEnv:    l.APIKeyEnv,
		ProviderName: l.ProviderName,
		Wire:         llmendpoint.WireAPI(l.WireAPI),
		Headers:      l.Headers,
	}
}

// ResolveRound converts a RoundConfig into a fully-resolved StepConfig,
// inheriting zero-value fields from the parent step.
func ResolveRound(round RoundConfig, parent StepConfig) StepConfig {
	systemPrompt := round.SystemPrompt
	if systemPrompt == "" {
		systemPrompt = parent.SystemPrompt
	}
	resolved := StepConfig{
		Prompt:                round.Prompt,
		SystemPrompt:          systemPrompt,
		PermissionMode:        parent.PermissionMode,
		Effort:                parent.Effort,
		TransientRetries:      parent.TransientRetries,
		StreamTurnGracePeriod: parent.StreamTurnGracePeriod,
		LLMEndpoint:           parent.LLMEndpoint,
	}
	if round.Model != "" {
		resolved.Model = round.Model
	} else {
		resolved.Model = parent.Model
	}
	if round.MaxTurns > 0 {
		resolved.MaxTurns = round.MaxTurns
	} else {
		resolved.MaxTurns = parent.MaxTurns
	}
	if round.MaxBudgetUSD > 0 {
		resolved.MaxBudgetUSD = round.MaxBudgetUSD
	} else {
		resolved.MaxBudgetUSD = parent.MaxBudgetUSD
	}
	if round.MaxToolErrorRetries > 0 {
		resolved.MaxToolErrorRetries = round.MaxToolErrorRetries
	} else {
		resolved.MaxToolErrorRetries = parent.MaxToolErrorRetries
	}
	return resolved
}

// ValidateWorkDir checks that a work_dir path exists and is a directory.
func ValidateWorkDir(path string) error {
	if path != "" && path != "." {
		if info, err := os.Stat(path); err != nil {
			return fmt.Errorf("work_dir %q: %w", path, err)
		} else if !info.IsDir() {
			return fmt.Errorf("work_dir %q is not a directory", path)
		}
	}
	return nil
}

// ExpandHome replaces a leading ~ with the user's home directory.
func ExpandHome(path string) string {
	if strings.HasPrefix(path, "~/") || path == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, path[1:])
		}
	}
	return path
}

// resolveEnv expands a value that starts with $ as an environment variable.
// Returns an error if the value references an env var that is not set.
func resolveEnv(value string) (string, error) {
	if strings.HasPrefix(value, "$") {
		envName := strings.TrimPrefix(value, "$")
		if v := os.Getenv(envName); v != "" {
			return v, nil
		}
		return "", fmt.Errorf("environment variable %s is not set", envName)
	}
	return value, nil
}
