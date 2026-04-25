package sessionanalysis

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// UsageTotals stores token usage counters.
type UsageTotals struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

// TotalTokens returns the sum of all token counters.
func (u UsageTotals) TotalTokens() int64 {
	return u.InputTokens + u.OutputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
}

// BucketStats stores aggregated stats for one bucket (global/model/project/etc).
type BucketStats struct {
	Sessions int         `json:"sessions"`
	Usage    UsageTotals `json:"usage"`

	UsageEvents int64 `json:"usage_events"`
	ToolUses    int64 `json:"tool_uses"`

	ObservedCostUSD  float64 `json:"observed_cost_usd"`
	EstimatedCostUSD float64 `json:"estimated_cost_usd"`

	PricedTokens   int64 `json:"priced_tokens"`
	UnpricedTokens int64 `json:"unpriced_tokens"`
}

// Coverage returns the fraction of tokens that were priced for estimate.
func (b BucketStats) Coverage() float64 {
	total := b.PricedTokens + b.UnpricedTokens
	if total == 0 {
		return 1
	}
	return float64(b.PricedTokens) / float64(total)
}

// ModelBucket is one model-level aggregate row.
type ModelBucket struct {
	Model string `json:"model"`
	BucketStats
}

// FamilyBucket is one model-family aggregate row.
type FamilyBucket struct {
	Family string `json:"family"`
	BucketStats
}

// ProjectBucket is one project-level aggregate row.
type ProjectBucket struct {
	Project string `json:"project"`
	BucketStats
}

// ProjectModelBucket is one project+model aggregate row.
type ProjectModelBucket struct {
	Project string `json:"project"`
	Model   string `json:"model"`
	BucketStats
}

// PricingMetadata describes pricing source/version in the report.
type PricingMetadata struct {
	Version string `json:"version"`
	Source  string `json:"source"`
}

// StatsReport is the output of usage/cost aggregation across JSONL sessions.
type StatsReport struct { //nolint:govet // fieldalignment: readability over packing
	GeneratedAt time.Time `json:"generated_at"`
	Since       time.Time `json:"since,omitempty"`
	Until       time.Time `json:"until,omitempty"`

	FilesScanned  int   `json:"files_scanned"`
	EventsScanned int64 `json:"events_scanned"`
	ParseErrors   int64 `json:"parse_errors"`

	Total          BucketStats          `json:"total"`
	ByFamily       []FamilyBucket       `json:"by_family"`
	ByModel        []ModelBucket        `json:"by_model"`
	ByProject      []ProjectBucket      `json:"by_project"`
	ByProjectModel []ProjectModelBucket `json:"by_project_model"`

	Pricing PricingMetadata `json:"pricing"`
}

// ModelPricing defines USD price per 1M tokens for each token class.
type ModelPricing struct {
	InputPerMTok         float64 `json:"input_per_mtok"`
	OutputPerMTok        float64 `json:"output_per_mtok"`
	CacheReadPerMTok     float64 `json:"cache_read_per_mtok"`
	CacheCreationPerMTok float64 `json:"cache_creation_per_mtok"`
}

// PricingTable contains model pricing and metadata.
type PricingTable struct { //nolint:govet // fieldalignment: readability over packing
	Version string                  `json:"version"`
	Source  string                  `json:"source"`
	Models  map[string]ModelPricing `json:"models"`
}

// DefaultPricingTable returns builtin model pricing for cost estimates.
func DefaultPricingTable() PricingTable {
	return PricingTable{
		Version: "builtin-2026-04-23",
		Source:  "builtin",
		Models: map[string]ModelPricing{
			"claude-opus-4-7": {
				InputPerMTok:         15.0,
				OutputPerMTok:        75.0,
				CacheReadPerMTok:     1.5,
				CacheCreationPerMTok: 18.75,
			},
			"claude-opus-4-6": {
				InputPerMTok:         15.0,
				OutputPerMTok:        75.0,
				CacheReadPerMTok:     1.5,
				CacheCreationPerMTok: 18.75,
			},
			"claude-sonnet-4-6": {
				InputPerMTok:         3.0,
				OutputPerMTok:        15.0,
				CacheReadPerMTok:     0.3,
				CacheCreationPerMTok: 3.75,
			},
			"claude-sonnet-4-5-20250929": {
				InputPerMTok:         3.0,
				OutputPerMTok:        15.0,
				CacheReadPerMTok:     0.3,
				CacheCreationPerMTok: 3.75,
			},
			"claude-haiku-4-5-20251001": {
				InputPerMTok:         0.8,
				OutputPerMTok:        4.0,
				CacheReadPerMTok:     0.08,
				CacheCreationPerMTok: 1.0,
			},
		},
	}
}

// LoadPricingTable reads pricing JSON from disk.
func LoadPricingTable(path string) (PricingTable, error) {
	f, err := os.Open(path)
	if err != nil {
		return PricingTable{}, err
	}
	defer f.Close()

	var t PricingTable
	if err := json.NewDecoder(f).Decode(&t); err != nil {
		return PricingTable{}, fmt.Errorf("decode pricing JSON: %w", err)
	}
	if t.Models == nil {
		t.Models = make(map[string]ModelPricing)
	}
	if t.Version == "" {
		t.Version = "custom"
	}
	if t.Source == "" {
		t.Source = path
	}
	return t, nil
}

// StatsConfig controls usage/cost stats analysis behavior.
type StatsConfig struct { //nolint:govet // fieldalignment: readability over packing
	Since            time.Time
	Until            time.Time
	IncludeSubagents bool
	Pricing          PricingTable
}

// DefaultStatsConfig returns default settings for stats analysis.
func DefaultStatsConfig() StatsConfig {
	return StatsConfig{
		IncludeSubagents: true,
		Pricing:          DefaultPricingTable(),
	}
}

type statsEnvelope struct { //nolint:govet // fieldalignment: readability over packing
	Type         string                `json:"type"`
	Subtype      string                `json:"subtype,omitempty"`
	Timestamp    string                `json:"timestamp,omitempty"`
	SessionID    string                `json:"sessionId,omitempty"`
	SessionIDAlt string                `json:"session_id,omitempty"`
	AgentID      string                `json:"agentId,omitempty"`
	Model        string                `json:"model,omitempty"`
	UUID         string                `json:"uuid,omitempty"`
	TotalCostUSD float64               `json:"total_cost_usd,omitempty"`
	ModelUsage   map[string]modelUsage `json:"modelUsage,omitempty"`
	Message      *statsMessage         `json:"message,omitempty"`
}

type statsMessage struct {
	ID      string          `json:"id,omitempty"`
	Role    string          `json:"role,omitempty"`
	Model   string          `json:"model,omitempty"`
	Usage   *statsUsage     `json:"usage,omitempty"`
	Content json.RawMessage `json:"content,omitempty"`
}

type statsUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
}

func (u statsUsage) totals() UsageTotals {
	return UsageTotals{
		InputTokens:              u.InputTokens,
		OutputTokens:             u.OutputTokens,
		CacheReadInputTokens:     u.CacheReadInputTokens,
		CacheCreationInputTokens: u.CacheCreationInputTokens,
	}
}

type modelUsage struct {
	CostUSD float64 `json:"costUSD"`
}

type contentBlock struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
	ID   string `json:"id,omitempty"`
}

type mutableBucket struct { //nolint:govet // fieldalignment: readability over packing
	stats    BucketStats
	sessions map[string]struct{}
}

func newMutableBucket() *mutableBucket {
	return &mutableBucket{
		sessions: make(map[string]struct{}),
	}
}

func (b *mutableBucket) addSession(sessionKey string) {
	if sessionKey == "" {
		return
	}
	b.sessions[sessionKey] = struct{}{}
}

func (b *mutableBucket) addUsage(sessionKey string, usage UsageTotals, estimateUSD float64, priced, unpriced int64) {
	b.addSession(sessionKey)
	b.stats.UsageEvents++
	b.stats.Usage.InputTokens += usage.InputTokens
	b.stats.Usage.OutputTokens += usage.OutputTokens
	b.stats.Usage.CacheReadInputTokens += usage.CacheReadInputTokens
	b.stats.Usage.CacheCreationInputTokens += usage.CacheCreationInputTokens
	b.stats.EstimatedCostUSD += estimateUSD
	b.stats.PricedTokens += priced
	b.stats.UnpricedTokens += unpriced
}

func (b *mutableBucket) addToolUses(sessionKey string, toolUses int64) {
	if toolUses <= 0 {
		return
	}
	b.addSession(sessionKey)
	b.stats.ToolUses += toolUses
}

func (b *mutableBucket) addObservedCost(sessionKey string, costUSD float64) {
	b.addSession(sessionKey)
	b.stats.ObservedCostUSD += costUSD
}

func (b *mutableBucket) finalized() BucketStats {
	out := b.stats
	out.Sessions = len(b.sessions)
	return out
}

type statsAggregator struct {
	cfg StatsConfig

	total *mutableBucket

	byFamily       map[string]*mutableBucket
	byModel        map[string]*mutableBucket
	byProject      map[string]*mutableBucket
	byProjectModel map[string]*mutableBucket

	filesScanned  int
	eventsScanned int64
	parseErrors   int64
}

func newStatsAggregator(cfg StatsConfig) *statsAggregator {
	return &statsAggregator{
		cfg:            cfg,
		total:          newMutableBucket(),
		byFamily:       make(map[string]*mutableBucket),
		byModel:        make(map[string]*mutableBucket),
		byProject:      make(map[string]*mutableBucket),
		byProjectModel: make(map[string]*mutableBucket),
	}
}

func (a *statsAggregator) getFamilyBucket(family string) *mutableBucket {
	if family == "" {
		family = "(unknown)"
	}
	if b, ok := a.byFamily[family]; ok {
		return b
	}
	b := newMutableBucket()
	a.byFamily[family] = b
	return b
}

func (a *statsAggregator) getModelBucket(model string) *mutableBucket {
	if model == "" {
		model = "(unknown)"
	}
	if b, ok := a.byModel[model]; ok {
		return b
	}
	b := newMutableBucket()
	a.byModel[model] = b
	return b
}

func (a *statsAggregator) getProjectBucket(project string) *mutableBucket {
	if project == "" {
		project = "(unknown)"
	}
	if b, ok := a.byProject[project]; ok {
		return b
	}
	b := newMutableBucket()
	a.byProject[project] = b
	return b
}

func (a *statsAggregator) getProjectModelBucket(project, model string) *mutableBucket {
	if project == "" {
		project = "(unknown)"
	}
	if model == "" {
		model = "(unknown)"
	}
	key := project + "\x00" + model
	if b, ok := a.byProjectModel[key]; ok {
		return b
	}
	b := newMutableBucket()
	a.byProjectModel[key] = b
	return b
}

func (a *statsAggregator) addUsage(sessionKey, project, model string, usage UsageTotals) {
	estimateUSD, priced, unpriced := estimateCost(model, usage, a.cfg.Pricing)
	family := modelFamily(model)

	a.total.addUsage(sessionKey, usage, estimateUSD, priced, unpriced)
	a.getFamilyBucket(family).addUsage(sessionKey, usage, estimateUSD, priced, unpriced)
	a.getModelBucket(model).addUsage(sessionKey, usage, estimateUSD, priced, unpriced)
	a.getProjectBucket(project).addUsage(sessionKey, usage, estimateUSD, priced, unpriced)
	a.getProjectModelBucket(project, model).addUsage(sessionKey, usage, estimateUSD, priced, unpriced)
}

func (a *statsAggregator) addToolUses(sessionKey, project, model string, toolUses int64) {
	if toolUses <= 0 {
		return
	}
	family := modelFamily(model)
	a.total.addToolUses(sessionKey, toolUses)
	a.getFamilyBucket(family).addToolUses(sessionKey, toolUses)
	a.getModelBucket(model).addToolUses(sessionKey, toolUses)
	a.getProjectBucket(project).addToolUses(sessionKey, toolUses)
	a.getProjectModelBucket(project, model).addToolUses(sessionKey, toolUses)
}

func (a *statsAggregator) addObservedCost(sessionKey, project, model string, costUSD float64) {
	if costUSD == 0 {
		return
	}
	family := modelFamily(model)
	a.total.addObservedCost(sessionKey, costUSD)
	a.getFamilyBucket(family).addObservedCost(sessionKey, costUSD)
	a.getModelBucket(model).addObservedCost(sessionKey, costUSD)
	a.getProjectBucket(project).addObservedCost(sessionKey, costUSD)
	a.getProjectModelBucket(project, model).addObservedCost(sessionKey, costUSD)
}

func (a *statsAggregator) shouldIncludeEvent(ts time.Time, hasTimestamp bool) bool {
	if !hasTimestamp {
		return true
	}
	if !a.cfg.Since.IsZero() && ts.Before(a.cfg.Since) {
		return false
	}
	if !a.cfg.Until.IsZero() && ts.After(a.cfg.Until) {
		return false
	}
	return true
}

// AnalyzeUsageStats scans session logs from the provided paths and returns
// token/cost aggregates.
func AnalyzeUsageStats(paths []string, cfg StatsConfig) (*StatsReport, error) {
	files, err := collectJSONLFiles(paths, cfg.IncludeSubagents)
	if err != nil {
		return nil, err
	}
	return AnalyzeUsageStatsFromFiles(files, cfg)
}

// AnalyzeUsageStatsFromFiles scans explicit JSONL files and returns stats.
func AnalyzeUsageStatsFromFiles(files []string, cfg StatsConfig) (*StatsReport, error) {
	a := newStatsAggregator(cfg)

	for _, path := range files {
		if err := a.scanFile(path); err != nil {
			return nil, err
		}
	}

	report := &StatsReport{
		GeneratedAt:   time.Now().UTC(),
		Since:         cfg.Since,
		Until:         cfg.Until,
		FilesScanned:  a.filesScanned,
		EventsScanned: a.eventsScanned,
		ParseErrors:   a.parseErrors,
		Total:         a.total.finalized(),
		Pricing: PricingMetadata{
			Version: cfg.Pricing.Version,
			Source:  cfg.Pricing.Source,
		},
	}

	for family, bucket := range a.byFamily {
		report.ByFamily = append(report.ByFamily, FamilyBucket{
			Family:      family,
			BucketStats: bucket.finalized(),
		})
	}
	sort.Slice(report.ByFamily, func(i, j int) bool {
		if report.ByFamily[i].EstimatedCostUSD == report.ByFamily[j].EstimatedCostUSD {
			return report.ByFamily[i].Family < report.ByFamily[j].Family
		}
		return report.ByFamily[i].EstimatedCostUSD > report.ByFamily[j].EstimatedCostUSD
	})

	for model, bucket := range a.byModel {
		report.ByModel = append(report.ByModel, ModelBucket{
			Model:       model,
			BucketStats: bucket.finalized(),
		})
	}
	sort.Slice(report.ByModel, func(i, j int) bool {
		if report.ByModel[i].EstimatedCostUSD == report.ByModel[j].EstimatedCostUSD {
			return report.ByModel[i].Model < report.ByModel[j].Model
		}
		return report.ByModel[i].EstimatedCostUSD > report.ByModel[j].EstimatedCostUSD
	})

	for project, bucket := range a.byProject {
		report.ByProject = append(report.ByProject, ProjectBucket{
			Project:     project,
			BucketStats: bucket.finalized(),
		})
	}
	sort.Slice(report.ByProject, func(i, j int) bool {
		if report.ByProject[i].EstimatedCostUSD == report.ByProject[j].EstimatedCostUSD {
			return report.ByProject[i].Project < report.ByProject[j].Project
		}
		return report.ByProject[i].EstimatedCostUSD > report.ByProject[j].EstimatedCostUSD
	})

	for key, bucket := range a.byProjectModel {
		parts := strings.SplitN(key, "\x00", 2)
		if len(parts) != 2 {
			continue
		}
		report.ByProjectModel = append(report.ByProjectModel, ProjectModelBucket{
			Project:     parts[0],
			Model:       parts[1],
			BucketStats: bucket.finalized(),
		})
	}
	sort.Slice(report.ByProjectModel, func(i, j int) bool {
		if report.ByProjectModel[i].EstimatedCostUSD == report.ByProjectModel[j].EstimatedCostUSD {
			if report.ByProjectModel[i].Project == report.ByProjectModel[j].Project {
				return report.ByProjectModel[i].Model < report.ByProjectModel[j].Model
			}
			return report.ByProjectModel[i].Project < report.ByProjectModel[j].Project
		}
		return report.ByProjectModel[i].EstimatedCostUSD > report.ByProjectModel[j].EstimatedCostUSD
	})

	return report, nil
}

func (a *statsAggregator) scanFile(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()

	a.filesScanned++

	project := projectFromLogPath(path)
	sessionKey := sessionKeyFromLogPath(path)
	sessionModel := ""

	seenUsageMessageIDs := make(map[string]struct{})
	seenToolUseIDs := make(map[string]struct{})
	seenResultUUIDs := make(map[string]struct{})

	scanner := bufio.NewScanner(f)
	buf := make([]byte, 0, 1024*1024)
	scanner.Buffer(buf, 10*1024*1024)

	for scanner.Scan() {
		a.eventsScanned++
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		var env statsEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			a.parseErrors++
			continue
		}

		var eventTime time.Time
		hasTimestamp := false
		if env.Timestamp != "" {
			if ts, err := time.Parse(time.RFC3339Nano, env.Timestamp); err == nil {
				eventTime = ts
				hasTimestamp = true
			}
		}
		if !a.shouldIncludeEvent(eventTime, hasTimestamp) {
			continue
		}

		if env.Type == "system" && env.Subtype == "init" && env.Model != "" {
			sessionModel = env.Model
		}

		if env.Message != nil && env.Type == "assistant" {
			msgID := strings.TrimSpace(env.Message.ID)

			model := strings.TrimSpace(env.Message.Model)
			if model == "" {
				model = sessionModel
			}
			if model == "" {
				model = "(unknown)"
			}

			toolUses := extractToolUseCount(env.Message.Content, seenToolUseIDs)
			a.addToolUses(sessionKey, project, model, toolUses)

			if env.Message.Usage == nil {
				continue
			}

			if msgID != "" {
				if _, seen := seenUsageMessageIDs[msgID]; seen {
					continue
				}
				seenUsageMessageIDs[msgID] = struct{}{}
			}

			usage := env.Message.Usage.totals()
			a.addUsage(sessionKey, project, model, usage)
			continue
		}

		if env.Type == "result" {
			uuid := strings.TrimSpace(env.UUID)
			if uuid != "" {
				if _, seen := seenResultUUIDs[uuid]; seen {
					continue
				}
				seenResultUUIDs[uuid] = struct{}{}
			}

			if len(env.ModelUsage) > 0 {
				for model, mu := range env.ModelUsage {
					a.addObservedCost(sessionKey, project, model, mu.CostUSD)
				}
				continue
			}

			model := sessionModel
			if model == "" {
				model = "(unknown)"
			}
			a.addObservedCost(sessionKey, project, model, env.TotalCostUSD)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan %s: %w", path, err)
	}
	return nil
}

func extractToolUseCount(contentRaw json.RawMessage, seenToolUseIDs map[string]struct{}) int64 {
	if len(contentRaw) == 0 || contentRaw[0] != '[' {
		return 0
	}
	var blocks []contentBlock
	if err := json.Unmarshal(contentRaw, &blocks); err != nil {
		return 0
	}
	var n int64
	for _, b := range blocks {
		if b.Type != "tool_use" {
			continue
		}
		if b.ID != "" {
			if _, seen := seenToolUseIDs[b.ID]; seen {
				continue
			}
			seenToolUseIDs[b.ID] = struct{}{}
		}
		n++
	}
	return n
}

func projectFromLogPath(path string) string {
	clean := filepath.Clean(path)
	parent := filepath.Base(filepath.Dir(clean))
	if parent == "subagents" {
		return filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(clean))))
	}
	return filepath.Base(filepath.Dir(clean))
}

func sessionKeyFromLogPath(path string) string {
	clean := filepath.Clean(path)
	parent := filepath.Base(filepath.Dir(clean))
	if parent == "subagents" {
		rootSession := filepath.Base(filepath.Dir(filepath.Dir(clean)))
		agent := strings.TrimSuffix(filepath.Base(clean), filepath.Ext(clean))
		return rootSession + "/" + agent
	}
	return strings.TrimSuffix(filepath.Base(clean), filepath.Ext(clean))
}

func collectJSONLFiles(paths []string, includeSubagents bool) ([]string, error) {
	seen := make(map[string]struct{})
	var out []string
	add := func(path string) {
		if !strings.HasSuffix(path, ".jsonl") {
			return
		}
		clean := filepath.Clean(path)
		if _, ok := seen[clean]; ok {
			return
		}
		seen[clean] = struct{}{}
		out = append(out, clean)
	}

	for _, p := range paths {
		info, err := os.Stat(p)
		if err != nil {
			return nil, err
		}
		if info.IsDir() {
			top, err := filepath.Glob(filepath.Join(p, "*.jsonl"))
			if err != nil {
				return nil, err
			}
			for _, f := range top {
				add(f)
			}
			if includeSubagents {
				sub, err := filepath.Glob(filepath.Join(p, "*", "subagents", "*.jsonl"))
				if err != nil {
					return nil, err
				}
				for _, f := range sub {
					add(f)
				}
			}
			continue
		}

		add(p)
		if includeSubagents {
			base := strings.TrimSuffix(p, filepath.Ext(p))
			sub, err := filepath.Glob(filepath.Join(base, "subagents", "*.jsonl"))
			if err != nil {
				return nil, err
			}
			for _, f := range sub {
				add(f)
			}
		}
	}

	sort.Strings(out)
	return out, nil
}

func estimateCost(model string, usage UsageTotals, pricing PricingTable) (usd float64, pricedTokens, unpricedTokens int64) {
	model = strings.TrimSpace(strings.ToLower(model))
	rate, ok := lookupPricing(model, pricing.Models)
	if !ok {
		return 0, 0, usage.TotalTokens()
	}

	usd += perTokenCost(usage.InputTokens, rate.InputPerMTok)
	usd += perTokenCost(usage.OutputTokens, rate.OutputPerMTok)
	usd += perTokenCost(usage.CacheReadInputTokens, rate.CacheReadPerMTok)
	usd += perTokenCost(usage.CacheCreationInputTokens, rate.CacheCreationPerMTok)
	return usd, usage.TotalTokens(), 0
}

func perTokenCost(tokens int64, usdPerMTok float64) float64 {
	if tokens <= 0 || usdPerMTok <= 0 {
		return 0
	}
	return (float64(tokens) / 1_000_000.0) * usdPerMTok
}

func lookupPricing(model string, table map[string]ModelPricing) (ModelPricing, bool) {
	if rate, ok := table[model]; ok {
		return rate, true
	}

	// Family fallbacks for versioned model IDs.
	switch {
	case strings.Contains(model, "opus"):
		if rate, ok := table["claude-opus-4-7"]; ok {
			return rate, true
		}
	case strings.Contains(model, "sonnet"):
		if rate, ok := table["claude-sonnet-4-6"]; ok {
			return rate, true
		}
	case strings.Contains(model, "haiku"):
		if rate, ok := table["claude-haiku-4-5-20251001"]; ok {
			return rate, true
		}
	}
	return ModelPricing{}, false
}

func modelFamily(model string) string {
	m := strings.ToLower(strings.TrimSpace(model))
	switch {
	case m == "", m == "(unknown)":
		return "(unknown)"
	case strings.Contains(m, "synthetic"):
		return "synthetic"
	case strings.Contains(m, "opus"):
		return "opus"
	case strings.Contains(m, "sonnet"):
		return "sonnet"
	case strings.Contains(m, "haiku"):
		return "haiku"
	default:
		return "other"
	}
}
