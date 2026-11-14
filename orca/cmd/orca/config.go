package main

import (
	"cmp"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/dispatcher"
	"github.com/bazelment/yoloswe/orca/lane/nebula"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/recipe"
	"github.com/bazelment/yoloswe/orca/store"
)

var (
	globalIDSeq  atomic.Uint64
	processNonce = initProcessNonce()
)

func initProcessNonce() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err == nil {
		return fmt.Sprintf("%x", b[:])
	}
	return fmt.Sprintf("%x", os.Getpid())
}

func nextUniqueID(prefix string, clk clock.Clock) string {
	seq := globalIDSeq.Add(1)
	var ts int64
	if clk != nil {
		ts = clk.Now().UTC().UnixNano()
	}
	return fmt.Sprintf("%s-%d-%s-%d", prefix, ts, processNonce, seq)
}

const (
	DefaultDispatcherModel       = "claude-sonnet-5-5"
	DefaultDispatcherHardModel   = dispatcher.DefaultHardBatchModel
	DefaultConciergeModel        = "claude-haiku-5-5"
	DefaultPlannerModel          = "claude-opus-5-5"
	DefaultOpenAIDispatcherModel = "gpt-6.1-sol"
	DefaultOpenAIConciergeModel  = "gpt-6.1-sol"
	DefaultOpenAIPlannerModel    = "gpt-6-astra"
	DefaultConcurrencyCap        = 4
	DefaultStoreSpec             = "sqlite"
	DefaultDriverKind            = "nebula"
)

var (
	ErrRunIDRequired          = errors.New("orca: --run <id> is required")
	ErrNebulaStoreUnsupported = errors.New("orca: --store nebula backend is not yet available in MVP; use --store sqlite:<path>")
)

// RoleModels configures the model identifier used by each orchestrator role.
type RoleModels struct {
	Roles          map[string]RoleSelection `json:"roles,omitempty"`
	Dispatcher     string                   `json:"dispatcher,omitempty"`
	DispatcherHard string                   `json:"dispatcher_hard,omitempty"`
	Concierge      string                   `json:"concierge,omitempty"`
	Planner        string                   `json:"planner,omitempty"`
}

type RoleSelection struct {
	Provider string `json:"provider,omitempty"`
	Model    string `json:"model,omitempty"`
	Effort   string `json:"effort,omitempty"`
}

func hasModelEnvironment() bool {
	for _, name := range []string{"OPENAI_API_KEY", "NEBULA_LLM_KEY", "ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "OPENAI_BASE_URL", "ANTHROPIC_BASE_URL", "LLM_GATEWAY_URL"} {
		if strings.TrimSpace(os.Getenv(name)) != "" {
			return true
		}
	}
	return false
}

func providerVendor(provider string) string {
	switch provider {
	case "codex-cli":
		return "openai"
	case "claude-cli":
		return "anthropic"
	default:
		return provider
	}
}

func (m RoleModels) withDefaults(provider string, httpConfigured bool) RoleModels {
	m.Roles = maps.Clone(m.Roles)
	if m.Roles == nil {
		m.Roles = map[string]RoleSelection{}
	}
	defaultProvider := strings.ToLower(strings.TrimSpace(provider))
	automatic := defaultProvider == ""
	if automatic {
		defaultProvider = "openai"
		if !httpConfigured {
			defaultProvider = "claude-cli"
		}
	}
	legacy := map[string]string{"dispatcher": m.Dispatcher, "planner": m.Planner, "concierge": m.Concierge}
	for _, role := range []string{"planner", "dispatcher", "concierge", "router", "critic"} {
		sel := m.Roles[role]
		fallbackProvider := defaultProvider
		if automatic && httpConfigured && (role == "planner" || role == "dispatcher") {
			fallbackProvider = "anthropic"
		}
		if role == "critic" {
			plannerProvider := m.Roles["planner"].Provider
			fallbackProvider = "anthropic"
			if providerVendor(plannerProvider) == "anthropic" {
				fallbackProvider = "openai"
			}
			if defaultProvider == "codex-cli" || defaultProvider == "claude-cli" {
				fallbackProvider = "claude-cli"
				if providerVendor(plannerProvider) == "anthropic" {
					fallbackProvider = "codex-cli"
				}
			}
		}
		sel.Provider = strings.ToLower(cmp.Or(strings.TrimSpace(sel.Provider), fallbackProvider))
		defaultModel := DefaultOpenAIDispatcherModel
		if providerVendor(sel.Provider) == "anthropic" {
			defaultModel = DefaultDispatcherModel
			if role == "planner" {
				defaultModel = DefaultPlannerModel
			}
			if role == "concierge" || role == "router" {
				defaultModel = DefaultConciergeModel
			}
		} else if role == "planner" {
			defaultModel = DefaultOpenAIPlannerModel
		}
		sel.Model = cmp.Or(strings.TrimSpace(sel.Model), strings.TrimSpace(legacy[role]), defaultModel)
		if role == "planner" {
			sel.Effort = cmp.Or(strings.TrimSpace(sel.Effort), "high")
		}
		m.Roles[role] = sel
	}
	m.Dispatcher = m.Roles["dispatcher"].Model
	m.Planner = m.Roles["planner"].Model
	m.Concierge = m.Roles["concierge"].Model
	m.DispatcherHard = cmp.Or(m.DispatcherHard, m.Dispatcher)
	return m
}

// Config holds the runtime configuration for orca `run` and `chat`.
type Config struct {
	ModelProvider    string     `json:"model_provider,omitempty"`
	Models           RoleModels `json:"models"`
	RunID            string     `json:"run_id,omitempty"`
	Goal             string     `json:"goal,omitempty"`
	Objective        string     `json:"objective,omitempty"`
	PlanRef          string     `json:"plan_ref,omitempty"`
	Plan             string     `json:"plan,omitempty"`
	Repository       string     `json:"repository,omitempty"`
	StoreSpec        string     `json:"store,omitempty"`
	StoreBaseDir     string     `json:"store_base_dir,omitempty"`
	DriverKind       string     `json:"driver,omitempty"`
	LLMGatewayURL    string     `json:"llm_gateway_url,omitempty"`
	NebulaAPIURL     string     `json:"nebula_api_url,omitempty"`
	NebulaToken      string     `json:"nebula_token,omitempty"`
	HolderID         string     `json:"holder_id,omitempty"`
	ReplayPath       string     `json:"replay_path,omitempty"`
	RecordPath       string     `json:"record_path,omitempty"`
	FakeScenarioPath string     `json:"fake_scenario_path,omitempty"`
	LogLevel         string     `json:"log_level,omitempty"`
	LogFormat        string     `json:"log_format,omitempty"`
	Prompt           string     `json:"prompt,omitempty"`
	ReportDay        string     `json:"report_day,omitempty"`
	RecipeFiles      []string   `json:"recipe_files,omitempty"`
	// launchArgs are the command-line flags a controller launched from this
	// invocation inherits; see controllerFlags.
	launchArgs          []string
	LeaseTTL            time.Duration `json:"lease_ttl,omitempty"`
	HeartbeatInterval   time.Duration `json:"heartbeat_interval,omitempty"`
	PollInterval        time.Duration `json:"poll_interval,omitempty"`
	JudgmentDebounce    time.Duration `json:"judgment_debounce,omitempty"`
	StallTimeout        time.Duration `json:"stall_timeout,omitempty"`
	ConcurrencyCap      int           `json:"concurrency_cap,omitempty"`
	MaxSteps            int           `json:"max_steps,omitempty"`
	Once                bool          `json:"once,omitempty"`
	Standing            bool          `json:"standing,omitempty"`
	FakeAutoComplete    bool          `json:"fake_auto_complete,omitempty"`
	onceSet             bool
	standingSet         bool
	fakeAutoSet         bool
	stallTimeoutSet     bool
	stallTimeoutFlagSet bool
}

// HasExplicitStallTimeout reports whether --stall-timeout or its config value was set.
func (c Config) HasExplicitStallTimeout() bool { return c.stallTimeoutSet }

func (c Config) withDefaults() Config {
	c.Objective = cmp.Or(c.Goal, c.Objective)
	if strings.TrimSpace(c.Goal) != "" && !c.Standing {
		c.Once = true
	}
	c.ModelProvider = strings.ToLower(cmp.Or(strings.TrimSpace(c.ModelProvider), strings.TrimSpace(os.Getenv("ORCH_MODEL_PROVIDER"))))
	c.Models = c.Models.withDefaults(c.ModelProvider, hasModelEnvironment() || strings.TrimSpace(c.LLMGatewayURL) != "")
	c.StoreSpec = cmp.Or(strings.TrimSpace(c.StoreSpec), DefaultStoreSpec)
	c.DriverKind = cmp.Or(strings.ToLower(strings.TrimSpace(c.DriverKind)), DefaultDriverKind)
	c.LLMGatewayURL = strings.TrimSpace(c.LLMGatewayURL)
	if c.ConcurrencyCap == 0 {
		c.ConcurrencyCap = DefaultConcurrencyCap
	}
	if c.HolderID == "" {
		host, err := os.Hostname()
		if err == nil && host != "" {
			c.HolderID = fmt.Sprintf("orca-%s-%d", host, os.Getpid())
		} else {
			c.HolderID = controller.DefaultHolderID
		}
	}
	if c.LeaseTTL <= 0 {
		c.LeaseTTL = controller.DefaultLeaseTTL
	}
	if c.HeartbeatInterval <= 0 {
		c.HeartbeatInterval = min(controller.DefaultHeartbeatInterval, max(c.LeaseTTL/3, time.Millisecond))
	}
	if c.PollInterval <= 0 {
		if c.DriverKind == "fake" || c.Once {
			c.PollInterval = 10 * time.Millisecond
		} else {
			c.PollInterval = controller.DefaultPollInterval
		}
	}
	if c.JudgmentDebounce == 0 {
		if c.DriverKind == "fake" || c.Once {
			c.JudgmentDebounce = -1
		} else {
			c.JudgmentDebounce = controller.DefaultJudgmentDebounce
		}
	}
	if c.StallTimeout <= 0 {
		c.StallTimeout = controller.DefaultStallTimeout
	}
	c.LogLevel = cmp.Or(strings.ToLower(strings.TrimSpace(c.LogLevel)), "info")
	c.LogFormat = cmp.Or(strings.ToLower(strings.TrimSpace(c.LogFormat)), "json")
	return c
}

// Validate checks that c has valid required fields for command execution.
func (c Config) Validate(requireDriver bool) error {
	provider := strings.ToLower(strings.TrimSpace(c.ModelProvider))
	if provider != "" && provider != "anthropic" && provider != "openai" && provider != "claude-cli" && provider != "codex-cli" {
		return fmt.Errorf("orca: unsupported model provider %q (want anthropic, openai, claude-cli, or codex-cli)", c.ModelProvider)
	}
	for role, sel := range c.Models.Roles {
		if sel.Provider != "openai" && sel.Provider != "anthropic" && sel.Provider != "claude-cli" && sel.Provider != "codex-cli" {
			return fmt.Errorf("orca: unsupported provider %q for role %s", sel.Provider, role)
		}
		switch sel.Effort {
		case "", "low", "medium", "high":
		default:
			return fmt.Errorf("orca: unsupported effort %q for role %s", sel.Effort, role)
		}
	}
	if c.Models.Roles["planner"].Provider != "" && providerVendor(c.Models.Roles["planner"].Provider) == providerVendor(c.Models.Roles["critic"].Provider) {
		return errors.New("orca: critic and planner must use different providers")
	}
	if c.Repository != "" && (len(strings.Split(c.Repository, "/")) != 2 || strings.ContainsAny(c.Repository, " \t\n") || strings.HasPrefix(c.Repository, "/") || strings.HasSuffix(c.Repository, "/")) {
		return fmt.Errorf("orca: --repo must be owner/name, got %q", c.Repository)
	}
	if strings.TrimSpace(c.RunID) == "" {
		return ErrRunIDRequired
	}
	if _, _, err := parseStoreSpec(c.StoreSpec, c.StoreBaseDir, c.RunID); err != nil {
		return err
	}
	if requireDriver {
		switch strings.ToLower(strings.TrimSpace(c.DriverKind)) {
		case "nebula", "fake", "":
		default:
			return fmt.Errorf("orca: unsupported --driver %q (want nebula or fake)", c.DriverKind)
		}
	}
	if c.ConcurrencyCap < 0 {
		return fmt.Errorf("orca: --concurrency-cap must be >= 0, got %d", c.ConcurrencyCap)
	}
	return nil
}

// LoadConfigFile reads a JSON configuration file and merges it into base.
func LoadConfigFile(path string, base Config) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return base, fmt.Errorf("read config file %q: %w", path, err)
	}
	var fileCfg Config
	if err := json.Unmarshal(data, &fileCfg); err != nil {
		return base, fmt.Errorf("parse config file %q: %w", path, err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err == nil {
		if _, ok := raw["once"]; ok {
			fileCfg.onceSet = true
		}
		if _, ok := raw["standing"]; ok {
			fileCfg.standingSet = true
		}
		if _, ok := raw["fake_auto_complete"]; ok {
			fileCfg.fakeAutoSet = true
		}
		if _, ok := raw["stall_timeout"]; ok {
			fileCfg.stallTimeoutSet = fileCfg.StallTimeout > 0
		}
	}
	return mergeConfig(fileCfg, base), nil
}

func mergeConfig(lower, higher Config) Config {
	out := lower
	out.Models.Roles = maps.Clone(lower.Models.Roles)
	out.RunID = cmp.Or(higher.RunID, lower.RunID)
	out.ModelProvider = cmp.Or(higher.ModelProvider, lower.ModelProvider)
	out.Goal = cmp.Or(higher.Goal, lower.Goal)
	out.Objective = cmp.Or(higher.Objective, lower.Objective)
	out.PlanRef = cmp.Or(higher.PlanRef, lower.PlanRef)
	out.Plan = cmp.Or(higher.Plan, lower.Plan)
	out.Repository = cmp.Or(higher.Repository, lower.Repository)
	out.StoreSpec = cmp.Or(higher.StoreSpec, lower.StoreSpec)
	out.StoreBaseDir = cmp.Or(higher.StoreBaseDir, lower.StoreBaseDir)
	out.DriverKind = cmp.Or(higher.DriverKind, lower.DriverKind)
	out.LLMGatewayURL = cmp.Or(higher.LLMGatewayURL, lower.LLMGatewayURL)
	out.NebulaAPIURL = cmp.Or(higher.NebulaAPIURL, lower.NebulaAPIURL)
	out.NebulaToken = cmp.Or(higher.NebulaToken, lower.NebulaToken)
	out.HolderID = cmp.Or(higher.HolderID, lower.HolderID)
	out.ReplayPath = cmp.Or(higher.ReplayPath, lower.ReplayPath)
	out.RecordPath = cmp.Or(higher.RecordPath, lower.RecordPath)
	out.FakeScenarioPath = cmp.Or(higher.FakeScenarioPath, lower.FakeScenarioPath)
	out.LogLevel = cmp.Or(higher.LogLevel, lower.LogLevel)
	out.LogFormat = cmp.Or(higher.LogFormat, lower.LogFormat)
	out.Prompt = cmp.Or(higher.Prompt, lower.Prompt)
	out.ReportDay = cmp.Or(higher.ReportDay, lower.ReportDay)
	out.Models.Dispatcher = cmp.Or(higher.Models.Dispatcher, lower.Models.Dispatcher)
	out.Models.DispatcherHard = cmp.Or(higher.Models.DispatcherHard, lower.Models.DispatcherHard)
	out.Models.Concierge = cmp.Or(higher.Models.Concierge, lower.Models.Concierge)
	out.Models.Planner = cmp.Or(higher.Models.Planner, lower.Models.Planner)
	if len(higher.Models.Roles) > 0 {
		if out.Models.Roles == nil {
			out.Models.Roles = map[string]RoleSelection{}
		}
		for role, sel := range higher.Models.Roles {
			prev := out.Models.Roles[role]
			out.Models.Roles[role] = RoleSelection{Provider: cmp.Or(sel.Provider, prev.Provider), Model: cmp.Or(sel.Model, prev.Model), Effort: cmp.Or(sel.Effort, prev.Effort)}
		}
	}
	if len(higher.RecipeFiles) > 0 {
		out.RecipeFiles = append(append([]string(nil), lower.RecipeFiles...), higher.RecipeFiles...)
	}
	if higher.ConcurrencyCap != 0 {
		out.ConcurrencyCap = higher.ConcurrencyCap
	}
	if higher.stallTimeoutFlagSet {
		out.StallTimeout = higher.StallTimeout
		out.stallTimeoutSet = higher.StallTimeout > 0
	}
	if higher.MaxSteps > 0 {
		out.MaxSteps = higher.MaxSteps
	}
	if higher.LeaseTTL != 0 {
		out.LeaseTTL = higher.LeaseTTL
	}
	if higher.HeartbeatInterval != 0 {
		out.HeartbeatInterval = higher.HeartbeatInterval
	}
	if higher.PollInterval != 0 {
		out.PollInterval = higher.PollInterval
	}
	if higher.JudgmentDebounce != 0 {
		out.JudgmentDebounce = higher.JudgmentDebounce
	}
	if higher.StallTimeout != 0 {
		out.StallTimeout = higher.StallTimeout
	}
	if higher.onceSet {
		out.Once = higher.Once
		out.onceSet = true
	} else if !lower.onceSet && higher.Once {
		out.Once = true
	}
	if higher.standingSet {
		out.Standing = higher.Standing
		out.standingSet = true
	}
	if higher.fakeAutoSet {
		out.FakeAutoComplete = higher.FakeAutoComplete
		out.fakeAutoSet = true
	} else if !lower.fakeAutoSet {
		out.FakeAutoComplete = higher.FakeAutoComplete
	}
	return out
}

type stringSliceFlag []string

func (s *stringSliceFlag) String() string {
	return strings.Join(*s, ",")
}

func (s *stringSliceFlag) Set(val string) error {
	for _, part := range strings.Split(val, ",") {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			*s = append(*s, trimmed)
		}
	}
	return nil
}

func parseFlags(cmdName string, args []string, errOut io.Writer) (Config, []string, error) {
	fs := flag.NewFlagSet(cmdName, flag.ContinueOnError)
	fs.SetOutput(errOut)

	var (
		cfg        Config
		configPath string
		recipes    stringSliceFlag
	)

	fs.StringVar(&configPath, "config", "", "Optional path to JSON config file")
	fs.StringVar(&cfg.RunID, "run", "", "Swarm run ID (optional with --goal)")
	fs.StringVar(&cfg.Objective, "objective", "", "Run objective when creating or updating the run")
	fs.StringVar(&cfg.Goal, "goal", "", "Run goal to plan at startup (generates a run ID if omitted)")
	fs.StringVar(&cfg.PlanRef, "plan-ref", "", "Optional plan reference (post ID, URL, or path)")
	fs.StringVar(&cfg.Plan, "plan", "", "Plan file or post ID to load through the Planner at startup")
	fs.StringVar(&cfg.Repository, "repo", "", "Repository owner/name for worker drivers")
	fs.BoolVar(&cfg.Standing, "standing", false, "Keep a goal run alive after it becomes settled")
	fs.StringVar(&cfg.StoreSpec, "store", "", "Store backend: sqlite:<path>, sqlite, memory, or nebula")
	fs.StringVar(&cfg.StoreBaseDir, "store-dir", "", "Base directory for default per-run SQLite store (~/.swarm)")
	fs.StringVar(&cfg.DriverKind, "driver", "", "Worker lane driver: nebula or fake (default nebula)")
	fs.StringVar(&cfg.ModelProvider, "model-provider", "", "Model provider: anthropic, openai, claude-cli, or codex-cli")
	fs.StringVar(&cfg.LLMGatewayURL, "llm-gateway-url", "", "Messages API / llm-gateway base URL")
	fs.StringVar(&cfg.Models.Dispatcher, "dispatcher-model", "", "Model for Dispatcher turns (default "+DefaultDispatcherModel+")")
	fs.StringVar(&cfg.Models.DispatcherHard, "dispatcher-hard-model", "", "Escalation model for hard Dispatcher batches")
	fs.StringVar(&cfg.Models.Concierge, "concierge-model", "", "Model for Concierge chat turns")
	fs.StringVar(&cfg.Models.Planner, "planner-model", "", "Model for Planner compilation and replanning turns (default "+DefaultPlannerModel+")")
	for _, role := range []string{"planner", "critic", "dispatcher", "concierge", "router"} {
		for _, field := range []string{"provider", "model", "effort"} {
			if field == "model" && (role == "planner" || role == "dispatcher" || role == "concierge") {
				continue
			}
			fs.Func(role+"-"+field, field+" for "+role+" turns", func(value string) error {
				if cfg.Models.Roles == nil {
					cfg.Models.Roles = map[string]RoleSelection{}
				}
				sel := cfg.Models.Roles[role]
				switch field {
				case "provider":
					sel.Provider = value
				case "model":
					sel.Model = value
				case "effort":
					sel.Effort = value
				}
				cfg.Models.Roles[role] = sel
				return nil
			})
		}
	}

	fs.IntVar(&cfg.ConcurrencyCap, "concurrency-cap", 0, "Maximum concurrent active lanes")
	fs.Var(&recipes, "recipe-file", "Recipe file path (.json compiled recipe or plain-text NL source; repeatable)")
	fs.StringVar(&cfg.ReplayPath, "replay", "", "Recorded model replay JSON path for deterministic execution")
	fs.StringVar(&cfg.RecordPath, "record", "", "Path to record model request/response exchanges")
	fs.StringVar(&cfg.FakeScenarioPath, "fake-scenario", "", "Optional JSON scenario file for --driver fake")
	fs.BoolVar(&cfg.FakeAutoComplete, "fake-auto", true, "Automatically complete unscripted phases when --driver fake is used")
	fs.StringVar(&cfg.HolderID, "holder-id", "", "Controller lease holder identifier")
	fs.DurationVar(&cfg.LeaseTTL, "lease-ttl", 0, "Controller lease TTL (default 30s)")
	fs.DurationVar(&cfg.HeartbeatInterval, "heartbeat-interval", 0, "Lease heartbeat interval (default 10s)")
	fs.DurationVar(&cfg.PollInterval, "poll-interval", 0, "Controller poll interval (default 5s)")
	fs.DurationVar(&cfg.JudgmentDebounce, "judgment-debounce", 0, "Debounce window for judgment event batches (default 2m, <0 for immediate)")
	fs.DurationVar(&cfg.StallTimeout, "stall-timeout", 0, "Default phase stall timeout (default 45m)")
	fs.BoolVar(&cfg.Once, "once", false, "Exit once all backlog items and lanes reach a terminal state")
	fs.IntVar(&cfg.MaxSteps, "max-steps", 0, "Optional bound on controller steps before exiting (0 = unlimited)")
	fs.StringVar(&cfg.LogLevel, "log-level", "", "Structured log level: debug, info, warn, error (default info)")
	fs.StringVar(&cfg.LogFormat, "log-format", "", "Structured log format: json or text (default json)")
	fs.StringVar(&cfg.Prompt, "prompt", "", "Single prompt for `chat` subcommand")
	fs.StringVar(&cfg.ReportDay, "day", "", "Report day (YYYY-MM-DD)")

	if err := fs.Parse(args); err != nil {
		return Config{}, nil, err
	}
	launchArgs := controllerFlags(fs, args[:len(args)-len(fs.Args())])
	cfg.RecipeFiles = recipes
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "once":
			cfg.onceSet = true
		case "fake-auto":
			cfg.fakeAutoSet = true
		case "standing":
			cfg.standingSet = true
		case "stall-timeout":
			cfg.stallTimeoutFlagSet = true
			cfg.stallTimeoutSet = cfg.StallTimeout > 0
		}
	})

	cliCfg := cfg
	if strings.TrimSpace(configPath) != "" {
		merged, err := LoadConfigFile(configPath, cfg)
		if err != nil {
			return Config{}, nil, err
		}
		cfg = merged
	}
	if cliCfg.Goal != "" {
		cfg.Objective = cliCfg.Goal
	} else if cliCfg.Objective != "" {
		cfg.Goal = ""
	}
	if cliCfg.Plan != "" {
		cfg.PlanRef = ""
	} else if cliCfg.PlanRef != "" {
		cfg.Plan = ""
	}
	fs.Visit(func(f *flag.Flag) {
		var value string
		switch f.Name {
		case "planner-model":
			value = cfg.Models.Planner
		case "dispatcher-model":
			value = cfg.Models.Dispatcher
		case "concierge-model":
			value = cfg.Models.Concierge
		}
		if value != "" {
			if cfg.Models.Roles == nil {
				cfg.Models.Roles = map[string]RoleSelection{}
			}
			role := strings.TrimSuffix(f.Name, "-model")
			sel := cfg.Models.Roles[role]
			sel.Model = value
			cfg.Models.Roles[role] = sel
		}
	})
	if cmdName == "run" && cfg.RunID == "" && strings.TrimSpace(cmp.Or(cfg.Goal, cfg.Objective)) != "" {
		cfg.RunID = nextUniqueID("run", clock.NewRealClock())
	}
	if (cmdName == "run" || cmdName == "chat") && strings.TrimSpace(cfg.Repository) == "" {
		cfg.Repository = inferRepository()
	}
	cfg.launchArgs = launchArgs
	return cfg.withDefaults(), fs.Args(), nil
}

// perInvocationFlags name the run, its goal and store, and this process's own
// I/O; a controller launched from chat gets its own values for them.
var perInvocationFlags = map[string]bool{
	"run": true, "goal": true, "objective": true, "plan": true, "plan-ref": true,
	"store": true, "repo": true, "prompt": true, "day": true, "replay": true, "record": true,
}

// controllerFlags returns the flag tokens of parsed (already accepted by fs)
// that a controller launched from this invocation inherits: everything that
// selects configuration, such as --config, models, providers and driver.
func controllerFlags(fs *flag.FlagSet, parsed []string) []string {
	var out []string
	for i := 0; i < len(parsed); i++ {
		tok := parsed[i]
		if tok == "--" || !strings.HasPrefix(tok, "-") {
			break
		}
		name, _, inline := strings.Cut(strings.TrimLeft(tok, "-"), "=")
		n := 1
		if f := fs.Lookup(name); f != nil && !inline && !isBoolFlag(f) && i+1 < len(parsed) {
			n = 2
		}
		if !perInvocationFlags[name] {
			out = append(out, parsed[i:i+n]...)
		}
		i += n - 1
	}
	return out
}

func isBoolFlag(f *flag.Flag) bool {
	b, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && b.IsBoolFlag()
}

func inferRepository() string {
	out, err := exec.Command("git", "config", "--get", "remote.origin.url").Output()
	if err != nil {
		return ""
	}
	return repositoryFromOrigin(strings.TrimSpace(string(out)))
}

func repositoryFromOrigin(u string) string {
	u = strings.TrimSpace(u)
	var path string
	if strings.HasPrefix(u, "git@github.com:") {
		path = strings.TrimPrefix(u, "git@github.com:")
	} else if parsed, err := url.Parse(u); err == nil && parsed.Hostname() == "github.com" {
		path = parsed.Path
	} else {
		return ""
	}
	path = strings.Trim(path, "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) == 2 && parts[0] != "" && parts[1] != "" {
		return parts[0] + "/" + parts[1]
	}
	return ""
}

func parseStoreSpec(spec, baseDir, runID string) (kind, path string, err error) {
	trimmed := strings.TrimSpace(spec)
	if trimmed == "" || strings.EqualFold(trimmed, "sqlite") {
		p, err := store.RunDBPath(baseDir, runID)
		if err != nil {
			return "", "", err
		}
		return "sqlite", p, nil
	}
	if strings.EqualFold(trimmed, "memory") {
		return "memory", "", nil
	}
	if strings.EqualFold(trimmed, "nebula") || strings.HasPrefix(strings.ToLower(trimmed), "nebula:") {
		return "nebula", "", ErrNebulaStoreUnsupported
	}
	if strings.HasPrefix(strings.ToLower(trimmed), "sqlite:") {
		rawPath := strings.TrimSpace(trimmed[len("sqlite:"):])
		if rawPath == "" {
			return "", "", fmt.Errorf("%w: empty path in --store %q", store.ErrInvalidArgument, spec)
		}
		return "sqlite", rawPath, nil
	}
	return "", "", fmt.Errorf("orca: unsupported --store %q (expected sqlite:<path>, sqlite, memory, or nebula)", spec)
}

func openStore(cfg Config, clk clock.Clock) (store.Store, string, error) {
	kind, dbPath, err := parseStoreSpec(cfg.StoreSpec, cfg.StoreBaseDir, cfg.RunID)
	if err != nil {
		return nil, "", err
	}
	switch kind {
	case "memory":
		return store.NewMemoryStore(clk), "memory", nil
	case "sqlite":
		st, err := store.OpenSQLite(dbPath, clk)
		if err != nil {
			return nil, "", err
		}
		return st, "sqlite:" + dbPath, nil
	default:
		return nil, "", fmt.Errorf("orca: unsupported store kind %q", kind)
	}
}

func roleClient(client model.ModelClient, cfg Config, role string) model.ModelClient {
	if client == nil {
		return nil
	}
	sel := cfg.Models.Roles[role]
	return modelClientFunc(func(ctx context.Context, req model.Request) (*model.Response, error) {
		req = req.Clone()
		if req.Metadata == nil {
			req.Metadata = map[string]string{}
		}
		req.Metadata["role"] = role
		req.Metadata["provider"] = sel.Provider
		req.ReasoningEffort = sel.Effort
		if role != "dispatcher" || req.Model == "" {
			req.Model = sel.Model
		}
		return client.CreateMessage(ctx, req)
	})
}

type modelClientFunc func(context.Context, model.Request) (*model.Response, error)

func (f modelClientFunc) CreateMessage(ctx context.Context, req model.Request) (*model.Response, error) {
	return f(ctx, req)
}

func openModelClient(cfg Config, clk clock.Clock) (model.ModelClient, error) {
	if strings.TrimSpace(cfg.ReplayPath) != "" {
		return model.LoadFakeClient(cfg.ReplayPath)
	}
	chatHTTP := &http.Client{Transport: chatTransport{base: http.DefaultTransport}, Timeout: model.DefaultHTTPTimeout}
	var openOpts []model.OpenAIClientOption
	openOpts = append(openOpts, model.WithOpenAIHTTPClient(chatHTTP))
	var anthropicOpts []model.ClientOption
	anthropicOpts = append(anthropicOpts, model.WithHTTPClient(chatHTTP))
	if gateway := strings.TrimSpace(os.Getenv("LLM_GATEWAY_URL")); gateway != "" && strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")) == "" {
		anthropicOpts = append(anthropicOpts, model.WithBaseURL(gateway))
	}
	if clk != nil {
		openOpts = append(openOpts, model.WithOpenAIClock(clk))
		anthropicOpts = append(anthropicOpts, model.WithClock(clk))
	}
	if cfg.LLMGatewayURL != "" {
		openOpts = append(openOpts, model.WithOpenAIBaseURL(cfg.LLMGatewayURL))
		anthropicOpts = append(anthropicOpts, model.WithBaseURL(cfg.LLMGatewayURL))
	}
	clients := map[string]model.ModelClient{"openai": model.NewOpenAIClient(openOpts...), "anthropic": model.NewHTTPClient(anthropicOpts...), "claude-cli": model.NewCLIClient("claude-cli"), "codex-cli": model.NewCLIClient("codex-cli")}
	var client model.ModelClient = modelClientFunc(func(ctx context.Context, req model.Request) (*model.Response, error) {
		provider := cmp.Or(req.Metadata["provider"], cfg.ModelProvider, "openai")
		selected, ok := clients[provider]
		if !ok {
			return nil, fmt.Errorf("orca: unsupported provider %q", provider)
		}
		return selected.CreateMessage(ctx, req)
	})
	if cfg.RecordPath != "" {
		client = model.NewRecordingFakeClient(cfg.RecordPath, client)
	}
	return client, nil
}

func openNebulaDriver(cfg Config, st store.Store, clk clock.Clock) (*nebula.Driver, error) {
	return nebula.New(nebula.Config{
		Clock:              clk,
		Events:             driverEventStore{st.Events()},
		APIURL:             cfg.NebulaAPIURL,
		Token:              cfg.NebulaToken,
		GitHubToken:        cmp.Or(os.Getenv("GITHUB_TOKEN"), os.Getenv("GH_TOKEN")),
		RepositoryFullName: cfg.Repository,
		HostOrchestrator:   strings.TrimSpace(os.Getenv(nebula.EnvSessionID)) == "",
	})
}

// LoadRecipeFile loads a recipe from path (either compiled JSON or plain-text NL
// source compiled via planner) and saves it to st.Recipes() for runID.
func LoadRecipeFile(
	ctx context.Context,
	path, runID string,
	concurrencyCap int,
	st store.Store,
	planner *Planner,
) (recipe.Recipe, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return recipe.Recipe{}, fmt.Errorf("read recipe file %q: %w", path, err)
	}
	trimmed := strings.TrimSpace(string(data))
	if trimmed == "" {
		return recipe.Recipe{}, fmt.Errorf("recipe file %q is empty", path)
	}

	baseName := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	if baseName == "" {
		baseName = recipe.DefaultName
	}

	if strings.HasPrefix(trimmed, "{") {
		var r recipe.Recipe
		if err := json.Unmarshal([]byte(trimmed), &r); err != nil {
			return recipe.Recipe{}, fmt.Errorf("parse JSON recipe %q: %w", path, err)
		}
		r.RunID = runID
		r.Name = cmp.Or(strings.TrimSpace(r.Name), baseName)
		r.Version = 0
		if concurrencyCap > 0 {
			r.Limits.MaxConcurrentLanes = concurrencyCap
		}
		if err := r.Validate(); err != nil {
			return recipe.Recipe{}, fmt.Errorf("validate recipe %q: %w", path, err)
		}
		return st.Recipes().Save(ctx, r)
	}

	if planner == nil {
		planner = NewPlanner(st, nil, nil, "", nil)
	}
	return planner.CompileAndSaveRecipe(ctx, runID, baseName, trimmed, nil, concurrencyCap)
}
