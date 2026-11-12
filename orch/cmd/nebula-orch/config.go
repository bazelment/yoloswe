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
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bazelment/yoloswe/orch/clock"
	"github.com/bazelment/yoloswe/orch/controller"
	"github.com/bazelment/yoloswe/orch/dispatcher"
	"github.com/bazelment/yoloswe/orch/lane/nebula"
	"github.com/bazelment/yoloswe/orch/model"
	"github.com/bazelment/yoloswe/orch/recipe"
	"github.com/bazelment/yoloswe/orch/store"
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
	DefaultDispatcherModel     = dispatcher.DefaultModel
	DefaultDispatcherHardModel = dispatcher.DefaultHardBatchModel
	DefaultConciergeModel      = "claude-haiku-4-5-20251001"
	DefaultPlannerModel        = recipe.DefaultCompilerModel
	DefaultConcurrencyCap      = 4
	DefaultStoreSpec           = "sqlite"
	DefaultDriverKind          = "nebula"
)

var (
	ErrRunIDRequired          = errors.New("nebula-orch: --run <id> is required")
	ErrNebulaStoreUnsupported = errors.New("nebula-orch: --store nebula backend is not yet available in MVP; use --store sqlite:<path>")
)

// RoleModels configures the model identifier used by each orchestrator role.
type RoleModels struct {
	Dispatcher     string `json:"dispatcher,omitempty"`
	DispatcherHard string `json:"dispatcher_hard,omitempty"`
	Concierge      string `json:"concierge,omitempty"`
	Planner        string `json:"planner,omitempty"`
}

func (m RoleModels) withDefaults() RoleModels {
	m.Dispatcher = cmp.Or(strings.TrimSpace(m.Dispatcher), DefaultDispatcherModel)
	m.DispatcherHard = cmp.Or(strings.TrimSpace(m.DispatcherHard), DefaultDispatcherHardModel)
	m.Concierge = cmp.Or(strings.TrimSpace(m.Concierge), DefaultConciergeModel)
	m.Planner = cmp.Or(strings.TrimSpace(m.Planner), DefaultPlannerModel)
	return m
}

// Config holds the runtime configuration for nebula-orch `run` and `chat`.
type Config struct {
	Models            RoleModels    `json:"models"`
	RunID             string        `json:"run_id,omitempty"`
	Objective         string        `json:"objective,omitempty"`
	PlanRef           string        `json:"plan_ref,omitempty"`
	StoreSpec         string        `json:"store,omitempty"`
	StoreBaseDir      string        `json:"store_base_dir,omitempty"`
	DriverKind        string        `json:"driver,omitempty"`
	LLMGatewayURL     string        `json:"llm_gateway_url,omitempty"`
	NebulaAPIURL      string        `json:"nebula_api_url,omitempty"`
	NebulaToken       string        `json:"nebula_token,omitempty"`
	HolderID          string        `json:"holder_id,omitempty"`
	ReplayPath        string        `json:"replay_path,omitempty"`
	RecordPath        string        `json:"record_path,omitempty"`
	FakeScenarioPath  string        `json:"fake_scenario_path,omitempty"`
	LogLevel          string        `json:"log_level,omitempty"`
	LogFormat         string        `json:"log_format,omitempty"`
	Prompt            string        `json:"prompt,omitempty"`
	RecipeFiles       []string      `json:"recipe_files,omitempty"`
	LeaseTTL          time.Duration `json:"lease_ttl,omitempty"`
	HeartbeatInterval time.Duration `json:"heartbeat_interval,omitempty"`
	PollInterval      time.Duration `json:"poll_interval,omitempty"`
	JudgmentDebounce  time.Duration `json:"judgment_debounce,omitempty"`
	StallTimeout      time.Duration `json:"stall_timeout,omitempty"`
	ConcurrencyCap    int           `json:"concurrency_cap,omitempty"`
	MaxSteps          int           `json:"max_steps,omitempty"`
	Once              bool          `json:"once,omitempty"`
	FakeAutoComplete  bool          `json:"fake_auto_complete,omitempty"`
	onceSet           bool
	fakeAutoSet       bool
}

func (c Config) withDefaults() Config {
	c.Models = c.Models.withDefaults()
	c.StoreSpec = cmp.Or(strings.TrimSpace(c.StoreSpec), DefaultStoreSpec)
	c.DriverKind = cmp.Or(strings.ToLower(strings.TrimSpace(c.DriverKind)), DefaultDriverKind)
	c.LLMGatewayURL = cmp.Or(
		strings.TrimSpace(c.LLMGatewayURL),
		strings.TrimSpace(os.Getenv("LLM_GATEWAY_URL")),
		strings.TrimSpace(os.Getenv("ANTHROPIC_BASE_URL")),
	)
	if c.ConcurrencyCap == 0 {
		c.ConcurrencyCap = DefaultConcurrencyCap
	}
	if c.HolderID == "" {
		host, err := os.Hostname()
		if err == nil && host != "" {
			c.HolderID = fmt.Sprintf("nebula-orch-%s-%d", host, os.Getpid())
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
			return fmt.Errorf("nebula-orch: unsupported --driver %q (want nebula or fake)", c.DriverKind)
		}
	}
	if c.ConcurrencyCap < 0 {
		return fmt.Errorf("nebula-orch: --concurrency-cap must be >= 0, got %d", c.ConcurrencyCap)
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
		if _, ok := raw["fake_auto_complete"]; ok {
			fileCfg.fakeAutoSet = true
		} else {
			fileCfg.FakeAutoComplete = true
		}
	}
	return mergeConfig(fileCfg, base), nil
}

func mergeConfig(lower, higher Config) Config {
	out := lower
	out.RunID = cmp.Or(higher.RunID, lower.RunID)
	out.Objective = cmp.Or(higher.Objective, lower.Objective)
	out.PlanRef = cmp.Or(higher.PlanRef, lower.PlanRef)
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
	out.Models.Dispatcher = cmp.Or(higher.Models.Dispatcher, lower.Models.Dispatcher)
	out.Models.DispatcherHard = cmp.Or(higher.Models.DispatcherHard, lower.Models.DispatcherHard)
	out.Models.Concierge = cmp.Or(higher.Models.Concierge, lower.Models.Concierge)
	out.Models.Planner = cmp.Or(higher.Models.Planner, lower.Models.Planner)
	if len(higher.RecipeFiles) > 0 {
		out.RecipeFiles = append(append([]string(nil), lower.RecipeFiles...), higher.RecipeFiles...)
	}
	if higher.ConcurrencyCap != 0 {
		out.ConcurrencyCap = higher.ConcurrencyCap
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
	fs.StringVar(&cfg.RunID, "run", "", "Swarm run ID (required)")
	fs.StringVar(&cfg.Objective, "objective", "", "Run objective when creating or updating the run")
	fs.StringVar(&cfg.PlanRef, "plan-ref", "", "Optional plan reference (post ID, URL, or path)")
	fs.StringVar(&cfg.StoreSpec, "store", "", "Store backend: sqlite:<path>, sqlite, memory, or nebula")
	fs.StringVar(&cfg.StoreBaseDir, "store-dir", "", "Base directory for default per-run SQLite store (~/.swarm)")
	fs.StringVar(&cfg.DriverKind, "driver", "", "Worker lane driver: nebula or fake (default nebula)")
	fs.StringVar(&cfg.LLMGatewayURL, "llm-gateway-url", "", "Messages API / llm-gateway base URL")
	fs.StringVar(&cfg.Models.Dispatcher, "dispatcher-model", "", "Model for Dispatcher turns")
	fs.StringVar(&cfg.Models.DispatcherHard, "dispatcher-hard-model", "", "Escalation model for hard Dispatcher batches")
	fs.StringVar(&cfg.Models.Concierge, "concierge-model", "", "Model for Concierge chat turns")
	fs.StringVar(&cfg.Models.Planner, "planner-model", "", "Model for Planner compilation and replanning turns")
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

	if err := fs.Parse(args); err != nil {
		return Config{}, nil, err
	}
	cfg.RecipeFiles = recipes
	fs.Visit(func(f *flag.Flag) {
		switch f.Name {
		case "once":
			cfg.onceSet = true
		case "fake-auto":
			cfg.fakeAutoSet = true
		}
	})

	if strings.TrimSpace(configPath) != "" {
		merged, err := LoadConfigFile(configPath, cfg)
		if err != nil {
			return Config{}, nil, err
		}
		cfg = merged
	}
	return cfg.withDefaults(), fs.Args(), nil
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
	return "", "", fmt.Errorf("nebula-orch: unsupported --store %q (expected sqlite:<path>, sqlite, memory, or nebula)", spec)
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
		return nil, "", fmt.Errorf("nebula-orch: unsupported store kind %q", kind)
	}
}

func openModelClient(cfg Config, clk clock.Clock) (model.ModelClient, error) {
	if strings.TrimSpace(cfg.ReplayPath) != "" {
		return model.LoadFakeClient(cfg.ReplayPath)
	}
	var opts []model.ClientOption
	if clk != nil {
		opts = append(opts, model.WithClock(clk))
	}
	if strings.TrimSpace(cfg.LLMGatewayURL) != "" {
		opts = append(opts, model.WithBaseURL(cfg.LLMGatewayURL))
	}
	httpClient := model.NewHTTPClient(opts...)
	if strings.TrimSpace(cfg.RecordPath) != "" {
		return model.NewRecordingFakeClient(cfg.RecordPath, httpClient), nil
	}
	return httpClient, nil
}

func openNebulaDriver(cfg Config, st store.Store, clk clock.Clock) (*nebula.Driver, error) {
	return nebula.New(nebula.Config{
		Clock:  clk,
		Events: st.Events(),
		APIURL: cfg.NebulaAPIURL,
		Token:  cfg.NebulaToken,
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
