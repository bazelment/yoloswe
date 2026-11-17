// Package nebula implements lane.LaneDriver against the Nebula HTTP API and
// streams terminal, usage, and capacity events into store.EventStore.
package nebula

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/store"
)

const (
	EnvAPIURL     = "NEBULA_API_URL"
	EnvToken      = "NEBULA_TOKEN"
	EnvSessionID  = "NEBULA_SESSION_ID"
	EnvTerminalID = "NEBULA_TERMINAL_ID"

	defaultBaseBranch    = "main"
	defaultHarness       = "claude"
	defaultPollInterval  = 2 * time.Second
	defaultSettleTimeout = 5 * time.Minute
	defaultHTTPTimeout   = 60 * time.Second
	maxSSELineBytes      = 1024 * 1024
)

var (
	ErrMissingCredentials = errors.New("orca/lane/nebula: NEBULA_API_URL and NEBULA_TOKEN are required")
	ErrNoReport           = fmt.Errorf("%w: no phase report filed for terminal", store.ErrNotFound)
	ErrStreamEnded        = errors.New("orca/lane/nebula: event stream ended before stop condition met")
)

// APIError represents a non-2xx HTTP response from the Nebula API.
type APIError struct {
	Method     string
	Path       string
	Code       string
	Message    string
	StatusCode int
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("nebula %s %s: status %d (%s): %s", e.Method, e.Path, e.StatusCode, e.Code, e.Message)
	}
	return fmt.Sprintf("nebula %s %s: status %d: %s", e.Method, e.Path, e.StatusCode, e.Message)
}

func (e *APIError) Unwrap() error {
	switch e.StatusCode {
	case http.StatusNotFound:
		return store.ErrNotFound
	case http.StatusConflict:
		return store.ErrConflict
	case http.StatusBadRequest:
		return store.ErrInvalidArgument
	default:
		return nil
	}
}

// IsNotReady reports whether err indicates a code session is sleeping or transitioning (HTTP 409).
func IsNotReady(err error) bool {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.StatusCode == http.StatusConflict
	}
	return false
}

// Config configures a Driver. Empty APIURL and Token fall back to env vars.
//
//nolint:govet // configuration fields are grouped by API concern.
type Config struct {
	Clock              clock.Clock
	Events             store.EventStore
	HTTPClient         *http.Client
	APIURL             string
	Token              string
	GitHubToken        string
	RepositoryID       string
	RepositoryFullName string
	CurrentSessionID   string
	HostOrchestrator   bool
	DefaultBaseBranch  string
	PollInterval       time.Duration
	SettleTimeout      time.Duration
}

// Option customizes a Config when constructing a Driver.
type Option func(*Config)

func WithAPIURL(apiURL string) Option {
	return func(c *Config) { c.APIURL = apiURL }
}

func WithToken(token string) Option {
	return func(c *Config) { c.Token = token }
}

func WithGitHubToken(token string) Option {
	return func(c *Config) { c.GitHubToken = token }
}

func WithRepositoryID(repoID string) Option {
	return func(c *Config) { c.RepositoryID = repoID }
}

func WithRepositoryFullName(fullName string) Option {
	return func(c *Config) { c.RepositoryFullName = fullName }
}

func WithHTTPClient(client *http.Client) Option {
	return func(c *Config) { c.HTTPClient = client }
}

func WithClock(clk clock.Clock) Option {
	return func(c *Config) { c.Clock = clk }
}

func WithEventStore(events store.EventStore) Option {
	return func(c *Config) { c.Events = events }
}

func WithPollInterval(d time.Duration) Option {
	return func(c *Config) { c.PollInterval = d }
}

func WithSettleTimeout(d time.Duration) Option {
	return func(c *Config) { c.SettleTimeout = d }
}

// WithHostOrchestrator declares the run's external host mailbox.
func WithHostOrchestrator() Option {
	return func(c *Config) { c.HostOrchestrator = true }
}

// Driver implements lane.LaneDriver using the Nebula HTTP API.
//
//nolint:govet // driver state is grouped by API concern.
type Driver struct {
	clock            clock.Clock
	events           store.EventStore
	httpClient       *http.Client
	streamClient     *http.Client
	laneLocks        map[string]*sync.Mutex
	lastUsage        map[string]string
	lastCapacity     map[string]string
	activeTerminals  map[string]string
	stoppedTerminals map[string]bool
	lastResumed      map[string]TerminalEvent
	apiURL           string
	token            string
	githubToken      string
	repoID           string
	repoFullName     string
	currSessID       string
	baseBranch       string
	pollInterval     time.Duration
	settleWait       time.Duration
	spawned          map[string][]watchTarget
	host             bool
	mu               sync.Mutex
}

type watchTarget struct {
	laneID, sessionID, terminalID string
}

func driverLaneKey(runID, laneID string) string {
	return strings.TrimSpace(runID) + "/" + strings.TrimSpace(laneID)
}

// TrackLane attaches an imported terminal to report polling and stream fallback.
func (d *Driver) TrackLane(ln lane.Lane) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if ln.ID != "" && ln.TerminalID != "" {
		if d.activeTerminals == nil {
			d.activeTerminals = make(map[string]string)
		}
		d.activeTerminals[driverLaneKey(ln.RunID, ln.ID)] = ln.TerminalID
	}
	for _, target := range d.spawned[ln.RunID] {
		if target.terminalID == ln.TerminalID {
			return
		}
	}
	d.spawned[ln.RunID] = append(d.spawned[ln.RunID], watchTarget{laneID: ln.ID, sessionID: ln.SessionID, terminalID: ln.TerminalID})
}

var (
	_ lane.LaneDriver          = (*Driver)(nil)
	_ lane.GitStateReader      = (*Driver)(nil)
	_ lane.SessionHealthReader = (*Driver)(nil)
)

func (d *Driver) ReportInstructions(runID, laneID, phase string, round int) string {
	return fmt.Sprintf("File the phase report in this terminal with `nebula phase report --outcome <done|no-change|needs-swe|blocked|needs-input> --stdin [--head <sha>] [--pr <n>]` (run %s, lane %s, phase %s, round %d). Pipe the report body to stdin; it must include exact `Outcome:`, `HEAD:`, and `PR:` lines; for no-change include a Reason: line. The phase is not finished until the report is filed.", runID, laneID, phase, round)
}

func (d *Driver) PhaseSkill(phase string) string {
	switch phase {
	case "clean":
		return "/simplify"
	case "review":
		return "/pr-polish-nebula"
	}
	return ""
}

// EnsureRun creates runID before spawning lanes. A RUN_EXISTS conflict means
// another request already created the run, so it is safe to continue.
func (d *Driver) EnsureRun(ctx context.Context, runID string) error {
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return errors.New("nebula ensure run: run id is required")
	}
	path := "/api/code-runs/" + url.PathEscape(runID)
	if err := d.doJSON(ctx, http.MethodPut, path, struct{}{}, nil); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusConflict && apiErr.Code == "RUN_EXISTS" {
			// Continue so a host invocation also declares the mailbox on an
			// already-created run.
		} else {
			return fmt.Errorf("nebula ensure run %q: %w", runID, err)
		}
	}
	if d.host {
		if err := d.doJSON(ctx, http.MethodPut, path+"/swarm", map[string]string{"needsInputTo": "host"}, nil); err != nil {
			return fmt.Errorf("nebula declare host mailbox for run %q: %w", runID, err)
		}
	}
	return nil
}

// Watch streams all terminals belonging to runID as uniform lane events.
func (d *Driver) Watch(ctx context.Context, runID string) <-chan lane.Event {
	out := make(chan lane.Event, 32)
	go func() {
		defer close(out)
		if d.events == nil {
			return
		}
		seen := map[int64]bool{}
		started := map[string]bool{}
		reported := map[string]string{}
		if existing, err := d.events.List(ctx, store.EventFilter{RunID: runID}); err == nil {
			for i := range existing {
				ev := &existing[i]
				if ev.Source != "lane.watch" && !strings.HasPrefix(ev.Source, "nebula:terminal:") {
					continue
				}
				seen[ev.Seq] = true
				if ev.Kind == store.EventLaneReport {
					var p StreamEventPayload
					if json.Unmarshal(ev.Payload, &p) == nil && p.Report != nil {
						termID := cmp.Or(p.TerminalID, p.Report.TerminalID)
						if p.TerminalEvent != nil && termID == "" {
							termID = p.TerminalEvent.TerminalID
						}
						key := reportKey(termID, p.Report)
						reported[key] = key
						continue
					}
					var rep lane.PhaseReport
					if json.Unmarshal(ev.Payload, &rep) == nil && rep.Outcome != "" {
						key := reportKey(rep.TerminalID, &rep)
						reported[key] = key
					}
				}
			}
		}
		interval := d.pollInterval
		if interval <= 0 {
			interval = 50 * time.Millisecond
		}
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		last := d.clock.Now()
		quietEmitted := false
		quietTerminals := make(map[string]bool)
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
			hasWorking := false
			hasIdle := false
			hasUnreportedIdle := false
			var idleTermIDs []string
			seenPollTerms := make(map[string]bool)
			recordTerminalActivity := func(termID, status, agentState string) {
				if termID == "" || seenPollTerms[termID] {
					return
				}
				st := strings.ToLower(strings.TrimSpace(status))
				if st == "stopped" || st == "failed" || st == "exited" || st == "terminated" || st == "deleted" {
					return
				}
				seenPollTerms[termID] = true
				if isWorkingAgentState(agentState) {
					hasWorking = true
					delete(quietTerminals, termID)
					return
				}
				hasIdle = true
				idleTermIDs = append(idleTermIDs, termID)
				if !quietTerminals[termID] {
					hasUnreportedIdle = true
				}
			}
			var sessions []SessionInfo
			if err := d.doJSON(ctx, http.MethodGet, "/api/code-sessions", nil, &struct {
				Sessions *[]SessionInfo `json:"sessions"`
			}{Sessions: &sessions}); err == nil {
				for i := range sessions {
					sess := &sessions[i]
					if sess.Metadata["swarmRun"] != runID {
						continue
					}
					detail, err := d.GetSession(ctx, sess.ID)
					if err != nil {
						continue
					}
					laneID := sess.Metadata["swarmLane"]
					for i := range detail.Terminals {
						tm := &detail.Terminals[i]
						if !d.isStaleTerminal(runID, laneID, tm.ID) {
							recordTerminalActivity(tm.ID, tm.Status, tm.AgentState)
						}
						if started[tm.ID] {
							continue
						}
						started[tm.ID] = true
						go d.watchStream(ctx, StreamRequest{Events: d.events, RunID: runID, LaneID: laneID, SessionID: sess.ID, TerminalID: tm.ID, StopOnExit: true})
					}
				}
			}
			// Host orchestrators are not themselves code sessions, and the API
			// may omit swarm metadata from a session listing. Keep streaming
			// terminals returned by our own spawn calls as a fallback.
			d.mu.Lock()
			targets := append([]watchTarget(nil), d.spawned[runID]...)
			d.mu.Unlock()
			for _, target := range targets {
				if target.terminalID == "" {
					continue
				}
				if !seenPollTerms[target.terminalID] && !d.isStaleTerminal(runID, target.laneID, target.terminalID) {
					if term, err := d.GetTerminal(ctx, target.terminalID); err == nil && term != nil {
						recordTerminalActivity(target.terminalID, term.Status, term.AgentState)
					}
				}
				// Polling the report endpoint is the host-safe fallback: an
				// external orchestrator can miss the terminal SSE publication
				// while the report itself is already durable.
				if rep, err := d.ReadReport(ctx, runID, target.laneID, target.terminalID); err == nil && rep != nil {
					key := reportKey(target.terminalID, rep)
					if _, already := reported[key]; !already {
						reported[key] = key
						select {
						case out <- lane.Event{Kind: lane.EventPhaseReported, RunID: runID, LaneID: target.laneID, SessionID: target.sessionID, TerminalID: target.terminalID, Phase: rep.Phase, Report: rep, At: rep.ReportedAt}:
						case <-ctx.Done():
							return
						}
						last = d.clock.Now()
						quietEmitted = false
						clear(quietTerminals)
					}
				}
				if started[target.terminalID] {
					continue
				}
				started[target.terminalID] = true
				go d.watchStream(ctx, StreamRequest{Events: d.events, RunID: runID, LaneID: target.laneID, SessionID: target.sessionID, TerminalID: target.terminalID, StopOnExit: true})
			}
			evs, err := d.events.List(ctx, store.EventFilter{RunID: runID})
			if err != nil {
				continue
			}
			for i := range evs {
				ev := &evs[i]
				if seen[ev.Seq] {
					continue
				}
				seen[ev.Seq] = true
				if e, ok := uniformEvent(*ev, runID); ok {
					// A host fallback report may race the SSE stream. Treat the
					// terminal/report payload as its identity so the controller sees
					// one lifecycle event regardless of which path delivered it.
					if e.Kind == lane.EventPhaseReported && e.Report != nil {
						key := reportKey(e.TerminalID, e.Report)
						if _, already := reported[key]; already {
							continue
						}
						reported[key] = key
					}
					select {
					case out <- e:
					case <-ctx.Done():
						return
					}
					last = d.clock.Now()
					quietEmitted = false
					clear(quietTerminals)
				}
			}
			if hasWorking && !hasIdle {
				last = d.clock.Now()
				quietEmitted = false
				clear(quietTerminals)
				continue
			}
			if d.clock.Now().Sub(last) >= time.Minute && (!quietEmitted || hasUnreportedIdle) {
				select {
				case out <- lane.Event{Kind: lane.EventNoActivity, RunID: runID, Since: last}:
				case <-ctx.Done():
					return
				}
				quietEmitted = true
				for _, id := range idleTermIDs {
					quietTerminals[id] = true
				}
			}
		}
	}()
	return out
}

func isWorkingAgentState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "working", "busy":
		return true
	default:
		return false
	}
}

func reportKey(terminalID string, rep *lane.PhaseReport) string {
	if rep == nil {
		return terminalID
	}
	if rep.Raw != "" {
		return terminalID + ":" + rep.Raw
	}
	return fmt.Sprintf("%s:%s:%d:%s:%s", terminalID, rep.Phase, rep.Round, rep.Outcome, rep.HeadSHA)
}

func (d *Driver) watchStream(ctx context.Context, req StreamRequest) {
	backoff := 50 * time.Millisecond
	for {
		err := d.StreamEvents(ctx, req)
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			return
		}
		t := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-t.C:
		}
		if backoff < 2*time.Second {
			backoff *= 2
		}
	}
}

func uniformEvent(ev store.Event, runID string) (lane.Event, bool) {
	var p StreamEventPayload
	_ = json.Unmarshal(ev.Payload, &p)
	e := lane.Event{RunID: runID, LaneID: ev.LaneID, At: ev.CreatedAt, Text: ev.Summary, TerminalID: p.TerminalID}
	if p.Report != nil {
		e.Phase = p.Report.Phase
	}
	if p.TerminalEvent != nil {
		e.TerminalID = p.TerminalEvent.TerminalID
		if p.TerminalEvent.Text != "" {
			e.Text = p.TerminalEvent.Text
		}
	}
	quota := isQuotaPayload(p)
	switch ev.Kind {
	case store.EventLaneReport:
		e.Kind = lane.EventPhaseReported
		e.Report = p.Report
	case store.EventPhaseExit:
		if p.TerminalEvent != nil && strings.EqualFold(strings.TrimSpace(p.TerminalEvent.Type), "input.waiting") && inputWaitingIsWorking(p.TerminalEvent) {
			return lane.Event{}, false
		}
		outcome := ""
		if p.TerminalEvent != nil {
			outcome = strings.ToLower(strings.TrimSpace(p.TerminalEvent.Outcome))
		}
		if strings.Contains(outcome, "auth") {
			e.Kind = lane.EventAuthFailure
		} else if quota || isQuotaOutcome(outcome) {
			e.Kind = lane.EventQuotaFailure
		} else if p.TerminalEvent != nil && (p.TerminalEvent.Type == "turn.ended" || p.TerminalEvent.Type == "api_error") {
			e.Kind = lane.EventTurnEndedNoReport
		} else {
			e.Kind = lane.EventExited
		}
	case store.EventStall:
		if p.TerminalEvent != nil {
			if strings.EqualFold(strings.TrimSpace(p.TerminalEvent.Type), "input.waiting") && inputWaitingIsWorking(p.TerminalEvent) {
				// agy uses input.waiting while it owns a background timer/task. It
				// is still working; do not turn this heartbeat into a phase exit.
				return lane.Event{}, false
			}
			outcome := strings.ToLower(strings.TrimSpace(p.TerminalEvent.Outcome))
			if quota || isQuotaOutcome(outcome) {
				e.Kind = lane.EventQuotaFailure
				break
			}
			switch strings.ToLower(p.TerminalEvent.Type) {
			case "input.waiting":
				e.Kind = lane.EventWaitingForInput
			case "turn.capacity", "terminal.usage_limited":
				e.Kind = lane.EventQuotaFailure
			default:
				if quota || isQuotaOutcome(outcome) {
					e.Kind = lane.EventQuotaFailure
				} else {
					e.Kind = lane.EventTurnEndedNoReport
				}
			}
		} else if quota {
			e.Kind = lane.EventQuotaFailure
		} else {
			return lane.Event{}, false
		}
	default:
		return lane.Event{}, false
	}
	return e, true
}

// inputWaitingIsWorking recognizes agy's intentional idle screen. The
// terminal emits input.waiting while a background task or timer is checking
// back in; stopping that terminal would discard live work.
func inputWaitingIsWorking(ev *TerminalEvent) bool {
	if ev == nil {
		return false
	}
	text := strings.ToLower(ev.Text)
	if lane.WorkingInputWaiting(text) {
		return true
	}
	raw := strings.ToLower(strings.TrimSpace(string(ev.BackgroundTasks)))
	if raw == "" || raw == "null" {
		return false
	}
	var tasks []json.RawMessage
	if json.Unmarshal([]byte(raw), &tasks) == nil {
		return len(tasks) > 0
	}
	var state struct {
		Tasks []json.RawMessage `json:"tasks"`
	}
	if json.Unmarshal([]byte(raw), &state) == nil {
		return len(state.Tasks) > 0
	}
	return false
}

func isQuotaOutcome(outcome string) bool {
	return strings.Contains(outcome, "usage_limit") || strings.Contains(outcome, "usage_limited") || strings.Contains(outcome, "capacity")
}

func isQuotaPayload(p StreamEventPayload) bool {
	if p.UsageLimit != nil || strings.EqualFold(p.AgentSignal, "usage_limited") {
		return true
	}
	if p.Capacity != nil {
		for _, b := range p.Capacity.Backends {
			if b.Full || b.PoolAtMax || b.Waiting > 0 {
				return true
			}
		}
	}
	return false
}

// New constructs a Driver from cfg and options.
func New(cfg Config, opts ...Option) (*Driver, error) {
	for _, opt := range opts {
		opt(&cfg)
	}
	apiURL := strings.TrimRight(cmp.Or(strings.TrimSpace(cfg.APIURL), strings.TrimSpace(os.Getenv(EnvAPIURL))), "/")
	token := cmp.Or(strings.TrimSpace(cfg.Token), strings.TrimSpace(os.Getenv(EnvToken)))
	if apiURL == "" || token == "" {
		return nil, ErrMissingCredentials
	}

	currSessID := cmp.Or(strings.TrimSpace(cfg.CurrentSessionID), strings.TrimSpace(os.Getenv(EnvSessionID)))

	clk := cfg.Clock
	if clk == nil {
		clk = clock.NewRealClock()
	}

	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: defaultHTTPTimeout}
	}
	streamClient := *httpClient
	streamClient.Timeout = 0

	baseBranch := cmp.Or(strings.TrimSpace(cfg.DefaultBaseBranch), defaultBaseBranch)

	pollInterval := cfg.PollInterval
	if pollInterval <= 0 {
		pollInterval = defaultPollInterval
	}

	settleWait := cfg.SettleTimeout
	if settleWait <= 0 {
		settleWait = defaultSettleTimeout
	}

	return &Driver{
		clock:            clk,
		events:           cfg.Events,
		httpClient:       httpClient,
		streamClient:     &streamClient,
		laneLocks:        make(map[string]*sync.Mutex),
		lastUsage:        make(map[string]string),
		lastCapacity:     make(map[string]string),
		activeTerminals:  make(map[string]string),
		stoppedTerminals: make(map[string]bool),
		lastResumed:      make(map[string]TerminalEvent),
		apiURL:           apiURL,
		token:            token,
		githubToken:      strings.TrimSpace(cfg.GitHubToken),
		repoID:           strings.TrimSpace(cfg.RepositoryID),
		repoFullName:     strings.TrimSpace(cfg.RepositoryFullName),
		currSessID:       currSessID,
		baseBranch:       baseBranch,
		pollInterval:     pollInterval,
		settleWait:       settleWait,
		spawned:          make(map[string][]watchTarget),
		host:             cfg.HostOrchestrator,
	}, nil
}

// NewFromEnv constructs a Driver configured from NEBULA_API_URL and NEBULA_TOKEN.
func NewFromEnv(opts ...Option) (*Driver, error) {
	return New(Config{}, opts...)
}

// SessionInfo describes a Nebula code session returned by /api/code-sessions.
type SessionInfo struct {
	Metadata     map[string]string `json:"metadata,omitempty"`
	ID           string            `json:"id"`
	Status       string            `json:"status"`
	Title        string            `json:"title,omitempty"`
	URL          string            `json:"url,omitempty"`
	RepositoryID string            `json:"repositoryId,omitempty"`
	GitURL       string            `json:"gitUrl,omitempty"`
	BaseBranch   string            `json:"baseBranch,omitempty"`
	BaseCommit   string            `json:"baseCommit,omitempty"`
	WorkBranch   string            `json:"workBranch,omitempty"`
	SharedHome   bool              `json:"sharedHome,omitempty"`
}

// TerminalUsage holds token and cost metrics reported by Nebula for a terminal.
type TerminalUsage struct {
	InputTokens      int64   `json:"inputTokens,omitempty"`
	OutputTokens     int64   `json:"outputTokens,omitempty"`
	CacheReadTokens  int64   `json:"cacheReadTokens,omitempty"`
	CacheWriteTokens int64   `json:"cacheWriteTokens,omitempty"`
	EstimatedCostUSD float64 `json:"estimatedCostUsd,omitempty"`
}

// UsageLimitInfo holds rate/quota reset information for a usage-limited terminal.
type UsageLimitInfo struct {
	ResetsAt string `json:"resetsAt,omitempty"`
}

// TerminalInfo describes a Nebula code terminal.
type TerminalInfo struct {
	Env             map[string]string `json:"env,omitempty"`
	Usage           *TerminalUsage    `json:"usage,omitempty"`
	UsageLimit      *UsageLimitInfo   `json:"usageLimit,omitempty"`
	ID              string            `json:"id"`
	CodeSessionID   string            `json:"codeSessionId,omitempty"`
	Kind            string            `json:"kind,omitempty"`
	Title           string            `json:"title,omitempty"`
	Status          string            `json:"status,omitempty"`
	AgentState      string            `json:"agentState,omitempty"`
	AgentSignal     string            `json:"agentSignal,omitempty"`
	Model           string            `json:"model,omitempty"`
	ReasoningEffort string            `json:"reasoningEffort,omitempty"`
	URL             string            `json:"url,omitempty"`
}

// SessionDetail combines a code session and its terminals from GET /api/code-sessions/{id}.
type SessionDetail struct {
	Terminals []TerminalInfo `json:"terminals,omitempty"`
	Session   SessionInfo    `json:"session"`
}

func (d *SessionDetail) UnmarshalJSON(data []byte) error {
	type plain SessionDetail
	if err := json.Unmarshal(data, (*plain)(d)); err != nil {
		return err
	}
	if d.Session.ID == "" {
		return json.Unmarshal(data, &d.Session)
	}
	return nil
}

// BackendCapacity describes capacity for a single Nebula compute backend pool.
type BackendCapacity struct {
	Backend      string `json:"backend"`
	Running      int    `json:"running"`
	Waiting      int    `json:"waiting"`
	PendingPods  int    `json:"pendingPods,omitempty"`
	Full         bool   `json:"full"`
	NodeStarting bool   `json:"nodeStarting,omitempty"`
	PoolAtMax    bool   `json:"poolAtMax,omitempty"`
}

// CapacityStatus describes cluster-wide code session capacity from GET /api/code-sessions/capacity.
type CapacityStatus struct {
	Backends       []BackendCapacity `json:"backends"`
	MaxWaitSeconds int               `json:"maxWaitSeconds,omitempty"`
}

// SpawnSession ensures a lane code session and optionally starts its initial phase terminal.
func (d *Driver) SpawnSession(ctx context.Context, req lane.SpawnRequest) (lane.SpawnResult, error) {
	sess, err := d.EnsureSession(ctx, req)
	if err != nil {
		return lane.SpawnResult{}, err
	}
	baseCommit := strings.TrimSpace(sess.BaseCommit)
	res := lane.SpawnResult{
		SessionID:  sess.ID,
		BaseCommit: baseCommit,
		Branch:     cmp.Or(strings.TrimSpace(sess.WorkBranch), strings.TrimSpace(req.Branch)),
		BaseBranch: cmp.Or(strings.TrimSpace(sess.BaseBranch), strings.TrimSpace(req.BaseBranch)),
		StartedAt:  d.clock.Now().UTC(),
	}
	if req.Phase == "" && req.Harness == "" && req.Brief == "" {
		return res, nil
	}
	termRes, err := d.StartPhaseTerminal(ctx, lane.StartPhaseRequest{
		RunDir:       req.RunDir,
		RunID:        req.RunID,
		Repository:   req.Repository,
		RepositoryID: req.RepositoryID,
		LaneID:       req.LaneID,
		SessionID:    sess.ID,
		Phase:        req.Phase,
		Harness:      req.Harness,
		Model:        req.Model,
		Effort:       req.Effort,
		Brief:        req.Brief,
		Round:        req.Round,
		Env:          req.Env,
	})
	if err != nil {
		return lane.SpawnResult{}, err
	}
	res.TerminalID = termRes.TerminalID
	res.StartedAt = termRes.StartedAt
	return res, nil
}

func (d *Driver) laneLock(runID, laneID string) *sync.Mutex {
	key := strings.TrimSpace(runID) + "/" + strings.TrimSpace(laneID)
	d.mu.Lock()
	defer d.mu.Unlock()
	lk, ok := d.laneLocks[key]
	if !ok {
		lk = &sync.Mutex{}
		d.laneLocks[key] = lk
	}
	return lk
}

// EnsureSession reuses or creates a lane code session and waits until it is ready.
func (d *Driver) EnsureSession(ctx context.Context, req lane.SpawnRequest) (*SessionInfo, error) {
	if strings.TrimSpace(req.LaneID) == "" {
		return nil, fmt.Errorf("%w: lane_id is required", store.ErrInvalidArgument)
	}
	// Serialize EnsureSession per (runID, laneID) so concurrent callers for the
	// same lane reuse the session created by whichever caller runs first rather
	// than racing to POST duplicate code sessions.
	lk := d.laneLock(req.RunID, req.LaneID)
	lk.Lock()
	defer lk.Unlock()

	workBranch := strings.TrimSpace(req.Branch)
	if workBranch == "" {
		if strings.TrimSpace(req.RunID) != "" {
			workBranch = fmt.Sprintf("swarm-lane/%s/%s", req.RunID, req.LaneID)
		} else {
			workBranch = fmt.Sprintf("swarm-lane/%s", req.LaneID)
		}
	}
	baseBranch := cmp.Or(strings.TrimSpace(req.BaseBranch), d.baseBranch)

	requireVerified := strings.TrimSpace(req.RepositoryID) != "" || strings.TrimSpace(req.Repository) != ""
	repoID, repoSlug, err := d.resolveTargetRepository(ctx, req.RepositoryID, req.Repository)
	if err != nil {
		return nil, err
	}

	existing, err := d.findReusableSession(ctx, req.RunID, req.LaneID, workBranch, baseBranch, repoID, repoSlug)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		settled, err := d.settleSession(ctx, *existing)
		if err != nil {
			return nil, err
		}
		if err := d.verifySessionRepository(ctx, *settled, repoID, repoSlug, requireVerified); err != nil {
			if relErr := d.ReleaseSession(ctx, settled.ID, true); relErr != nil {
				return nil, errors.Join(err, fmt.Errorf("release mismatched session %s: %w", settled.ID, relErr))
			}
			return nil, err
		}
		return settled, nil
	}

	meta := map[string]string{
		"swarmLane": req.LaneID,
	}
	if strings.TrimSpace(req.RunID) != "" {
		meta["swarmRun"] = req.RunID
	}
	title := strings.TrimSpace(fmt.Sprintf("swarm %s %s", req.RunID, req.LaneID))

	createBody := struct {
		Metadata     map[string]string `json:"metadata,omitempty"`
		Env          map[string]string `json:"env,omitempty"`
		RepositoryID string            `json:"repositoryId"`
		BaseBranch   string            `json:"baseBranch"`
		WorkBranch   string            `json:"workBranch"`
		Title        string            `json:"title,omitempty"`
		SharedHome   bool              `json:"sharedHome"`
	}{
		RepositoryID: repoID,
		BaseBranch:   baseBranch,
		WorkBranch:   workBranch,
		SharedHome:   req.SharedHome,
		Title:        title,
		Metadata:     meta,
	}
	if len(req.Env) > 0 {
		createBody.Env = maps.Clone(req.Env)
	}

	var created SessionDetail
	if err := d.doJSON(ctx, http.MethodPost, "/api/code-sessions", createBody, &created); err != nil {
		return nil, err
	}
	if created.Session.ID == "" {
		return nil, fmt.Errorf("nebula create session returned empty session id")
	}
	settled, err := d.settleSession(ctx, created.Session)
	if err != nil {
		return nil, err
	}
	if err := d.verifySessionRepository(ctx, *settled, repoID, repoSlug, requireVerified); err != nil {
		if relErr := d.ReleaseSession(ctx, settled.ID, true); relErr != nil {
			return nil, errors.Join(err, fmt.Errorf("release mismatched session %s: %w", settled.ID, relErr))
		}
		return nil, err
	}
	return settled, nil
}

func (d *Driver) findReusableSession(ctx context.Context, runID, laneID, workBranch, baseBranch, repoID, repoSlug string) (*SessionInfo, error) {
	q := url.Values{}
	if laneID != "" {
		q.Set("metadata.swarmLane", laneID)
	}
	if runID != "" {
		q.Set("metadata.swarmRun", runID)
	}
	path := "/api/code-sessions"
	if encoded := q.Encode(); encoded != "" {
		path += "?" + encoded
	}
	var resp struct {
		Sessions []SessionInfo `json:"sessions"`
	}
	if err := d.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		return nil, err
	}
	var fallback *SessionInfo
	for i := range resp.Sessions {
		s := &resp.Sessions[i]
		if !d.matchesLaneSession(s, runID, laneID, workBranch, baseBranch, repoID, repoSlug) {
			continue
		}
		if s.Status == "ready" {
			return s, nil
		}
		if fallback == nil {
			fallback = s
		}
	}
	return fallback, nil
}

func (d *Driver) matchesLaneSession(s *SessionInfo, runID, laneID, workBranch, baseBranch, repoID, repoSlug string) bool {
	if s == nil || s.ID == "" || !isSessionReusableStatus(s.Status) {
		return false
	}
	if repoID != "" && s.RepositoryID != "" && s.RepositoryID != repoID {
		return false
	}
	if d.repoFullName != "" && s.RepositoryID != repoID {
		return false
	}
	if s.GitURL != "" {
		gotSlug := lane.NormalizeRepositorySlug(s.GitURL)
		if repoSlug != "" && !strings.EqualFold(gotSlug, repoSlug) {
			return false
		}
		if repoSlug == "" && s.RepositoryID == "" && repoID != "" && !strings.EqualFold(gotSlug, repoID) {
			return false
		}
	}
	if d.currSessID != "" && s.ID == d.currSessID {
		return false
	}
	if s.WorkBranch != "" && workBranch != "" && s.WorkBranch != workBranch {
		return false
	}
	if s.BaseBranch != "" && baseBranch != "" && s.BaseBranch != baseBranch {
		return false
	}
	sessLane := s.Metadata["swarmLane"]
	sessRun := s.Metadata["swarmRun"]
	if sessLane != "" && sessLane != laneID {
		return false
	}
	if runID != "" && sessRun != "" && sessRun != runID {
		return false
	}
	hasBranchMatch := workBranch != "" && s.WorkBranch == workBranch
	hasMetaMatch := laneID != "" && sessLane == laneID && (runID == "" || sessRun == runID || hasBranchMatch)
	return hasMetaMatch || hasBranchMatch
}

func isSessionReusableStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "ready", "sleeping", "provisioning", "resuming", "suspending":
		return true
	default:
		return false
	}
}

func isSessionTerminalStatus(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed", "released", "deleted", "archived", "terminated", "stopped":
		return true
	default:
		return false
	}
}

func (d *Driver) settleSession(ctx context.Context, initial SessionInfo) (*SessionInfo, error) {
	sessionID := initial.ID
	status := strings.ToLower(strings.TrimSpace(initial.Status))
	if status == "ready" {
		initial.Status = "ready"
		return &initial, nil
	}

	deadline := d.clock.Now().Add(d.settleWait)
	woke := false
	if status == "sleeping" {
		if err := d.WakeSession(ctx, sessionID); err != nil && !IsNotReady(err) {
			return nil, err
		}
		woke = true
	}

	for {
		if status != "" && isSessionTerminalStatus(status) {
			return nil, fmt.Errorf("nebula code session %s entered terminal status %q", sessionID, status)
		}
		if !d.clock.Now().Before(deadline) {
			return nil, fmt.Errorf("nebula code session %s did not become ready within %s (last status %q)", sessionID, d.settleWait, status)
		}

		if status != "" {
			timer := d.clock.NewTimer(d.pollInterval)
			select {
			case <-ctx.Done():
				timer.Stop()
				return nil, ctx.Err()
			case <-timer.C():
			}
		}

		detail, err := d.GetSession(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		status = strings.ToLower(strings.TrimSpace(detail.Session.Status))
		if status == "ready" {
			return &detail.Session, nil
		}
		if status == "sleeping" && !woke {
			if err := d.WakeSession(ctx, sessionID); err != nil && !IsNotReady(err) {
				return nil, err
			}
			woke = true
		}
	}
}

// WakeSession wakes a sleeping code session.
func (d *Driver) WakeSession(ctx context.Context, sessionID string) error {
	path := fmt.Sprintf("/api/code-sessions/%s/wake", url.PathEscape(sessionID))
	return d.doJSON(ctx, http.MethodPost, path, struct{}{}, nil)
}

// GetSession fetches a code session and its terminals.
func (d *Driver) GetSession(ctx context.Context, sessionID string) (*SessionDetail, error) {
	path := fmt.Sprintf("/api/code-sessions/%s", url.PathEscape(sessionID))
	var detail SessionDetail
	if err := d.doJSON(ctx, http.MethodGet, path, nil, &detail); err != nil {
		return nil, err
	}
	return &detail, nil
}

// StartPhaseTerminal starts a phase terminal on an existing or newly ensured session.
func (d *Driver) StartPhaseTerminal(ctx context.Context, req lane.StartPhaseRequest) (lane.StartPhaseResult, error) {
	sessionID := strings.TrimSpace(req.SessionID)
	if sessionID == "" {
		sess, err := d.EnsureSession(ctx, lane.SpawnRequest{
			RunDir:       req.RunDir,
			RunID:        req.RunID,
			Repository:   req.Repository,
			RepositoryID: req.RepositoryID,
			LaneID:       req.LaneID,
			Env:          req.Env,
		})
		if err != nil {
			return lane.StartPhaseResult{}, err
		}
		sessionID = sess.ID
	}

	kind := cmp.Or(strings.TrimSpace(req.Harness), defaultHarness)
	round := max(req.Round, 1)
	var title string
	if req.LaneID != "" && req.Phase != "" {
		title = fmt.Sprintf("%s %s r%d", req.LaneID, req.Phase, round)
	} else if req.Phase != "" {
		title = fmt.Sprintf("%s r%d", req.Phase, round)
	}

	body := struct {
		Env             map[string]string `json:"env,omitempty"`
		Kind            string            `json:"kind"`
		Model           string            `json:"model,omitempty"`
		ReasoningEffort string            `json:"reasoningEffort,omitempty"`
		Prompt          string            `json:"prompt,omitempty"`
		Title           string            `json:"title,omitempty"`
		Start           bool              `json:"start"`
	}{
		Kind:            kind,
		Model:           strings.TrimSpace(req.Model),
		ReasoningEffort: strings.TrimSpace(req.Effort),
		Prompt:          req.Brief,
		Start:           true,
		Title:           title,
	}
	if len(req.Env) > 0 {
		body.Env = maps.Clone(req.Env)
	}

	path := fmt.Sprintf("/api/code-sessions/%s/terminals", url.PathEscape(sessionID))
	var resp TerminalInfo
	err := d.doJSON(ctx, http.MethodPost, path, body, &resp)
	if IsNotReady(err) {
		if _, settleErr := d.settleSession(ctx, SessionInfo{ID: sessionID, Status: "sleeping"}); settleErr != nil {
			return lane.StartPhaseResult{}, settleErr
		}
		err = d.doJSON(ctx, http.MethodPost, path, body, &resp)
	}
	if err != nil {
		return lane.StartPhaseResult{}, err
	}
	if resp.ID == "" {
		return lane.StartPhaseResult{}, fmt.Errorf("nebula start terminal returned empty id")
	}

	d.stopOtherSessionTerminals(ctx, req.RunID, req.LaneID, sessionID, resp.ID)

	result := lane.StartPhaseResult{
		SessionID:  sessionID,
		TerminalID: resp.ID,
		StartedAt:  d.clock.Now().UTC(),
	}
	d.TrackLane(lane.Lane{RunID: req.RunID, ID: req.LaneID, SessionID: result.SessionID, TerminalID: result.TerminalID})
	return result, nil
}

// stopOtherSessionTerminals stops any previous active terminal for (runID, laneID)
// and any non-shell pre-handover terminals belonging to the same lane in sessionID,
// while preserving the host session, other lanes' sessions/terminals, and operator shells.
func (d *Driver) stopOtherSessionTerminals(ctx context.Context, runID, laneID, sessionID, keepTerminalID string) {
	key := driverLaneKey(runID, laneID)
	d.mu.Lock()
	if d.activeTerminals == nil {
		d.activeTerminals = make(map[string]string)
	}
	if d.stoppedTerminals == nil {
		d.stoppedTerminals = make(map[string]bool)
	}
	prev := d.activeTerminals[key]
	if prev != "" && prev != keepTerminalID {
		d.stoppedTerminals[prev] = true
	}
	d.activeTerminals[key] = keepTerminalID
	currSessID := d.currSessID
	d.mu.Unlock()

	if prev != "" && prev != keepTerminalID {
		_ = d.Stop(ctx, lane.StopRequest{RunID: runID, LaneID: laneID, SessionID: sessionID, TerminalID: prev, Reason: "superseded by new phase terminal"})
	}
	if sessionID == "" || (currSessID != "" && sessionID == currSessID) {
		return
	}
	detail, err := d.GetSession(ctx, sessionID)
	if err != nil || detail == nil {
		return
	}
	sessLane := strings.TrimSpace(detail.Session.Metadata["swarmLane"])
	trimmedLane := strings.TrimSpace(laneID)
	if sessLane != "" && trimmedLane != "" && sessLane != trimmedLane {
		return
	}
	for i := range detail.Terminals {
		tm := &detail.Terminals[i]
		if tm.ID == "" || tm.ID == keepTerminalID || tm.ID == prev {
			continue
		}
		kind := strings.ToLower(strings.TrimSpace(tm.Kind))
		if kind == "shell" || kind == "bash" || kind == "sh" || kind == "operator" {
			continue
		}
		if termLane := strings.TrimSpace(tm.Env["SNL_GH_LANE"]); termLane != "" && trimmedLane != "" && termLane != trimmedLane {
			continue
		}
		st := strings.ToLower(strings.TrimSpace(tm.Status))
		if st == "running" || st == "starting" || st == "idle" || st == "" {
			_ = d.Stop(ctx, lane.StopRequest{RunID: runID, LaneID: laneID, SessionID: sessionID, TerminalID: tm.ID, Reason: "superseded by new phase terminal"})
		}
	}
}

func (d *Driver) isStaleTerminal(runID, laneID, terminalID string) bool {
	if terminalID == "" {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.stoppedTerminals[terminalID] {
		return true
	}
	if strings.TrimSpace(laneID) != "" {
		if active := d.activeTerminals[driverLaneKey(runID, laneID)]; active != "" && active != terminalID {
			return true
		}
	}
	return false
}

// Steer submits text or an option key and confirms that the agent resumes.
func (d *Driver) Steer(ctx context.Context, req lane.SteerRequest) error {
	if req.Select < 0 || len(strconv.Itoa(req.Select)) > 15 {
		return fmt.Errorf("%w: select must be a positive option number of at most 15 digits", store.ErrInvalidArgument)
	}
	if strings.TrimSpace(req.Message) == "" && req.Select == 0 {
		return fmt.Errorf("%w: steer message or select is required", store.ErrInvalidArgument)
	}
	terminalID := strings.TrimSpace(req.TerminalID)
	sessionID := strings.TrimSpace(req.SessionID)
	if d.isStaleTerminal(req.RunID, req.LaneID, terminalID) {
		return fmt.Errorf("%w: %s", lane.ErrStaleTerminal, terminalID)
	}
	if terminalID != "" && sessionID == "" {
		if term, err := d.GetTerminal(ctx, terminalID); err == nil {
			sessionID = term.CodeSessionID
		}
	}
	if terminalID == "" && sessionID != "" {
		detail, err := d.GetSession(ctx, sessionID)
		if err != nil {
			return err
		}
		for i := len(detail.Terminals) - 1; i >= 0; i-- {
			t := detail.Terminals[i]
			if d.isStaleTerminal(req.RunID, req.LaneID, t.ID) {
				continue
			}
			if t.Status == "running" || t.Status == "starting" || t.Status == "" {
				terminalID = t.ID
				break
			}
		}
	}
	if terminalID == "" {
		return fmt.Errorf("%w: terminal_id is required for Steer", store.ErrInvalidArgument)
	}
	if d.isStaleTerminal(req.RunID, req.LaneID, terminalID) {
		return fmt.Errorf("%w: %s", lane.ErrStaleTerminal, terminalID)
	}

	sentAt := d.clock.Now()
	d.mu.Lock()
	resumedSeq := d.lastResumed[terminalID].Seq
	d.mu.Unlock()
	submit := func() error {
		if req.Select > 0 {
			return d.pressKeys(ctx, terminalID, append(strings.Split(strconv.Itoa(req.Select), ""), "Enter"))
		}
		path := fmt.Sprintf("/api/code-terminals/%s/input", url.PathEscape(terminalID))
		return d.doJSON(ctx, http.MethodPost, path, struct {
			Text string `json:"text"`
		}{Text: req.Message}, nil)
	}
	err := submit()
	if IsNotReady(err) {
		if sessionID == "" {
			if term, getErr := d.GetTerminal(ctx, terminalID); getErr == nil {
				sessionID = term.CodeSessionID
			}
		}
		if sessionID != "" {
			if _, settleErr := d.settleSession(ctx, SessionInfo{ID: sessionID, Status: "sleeping"}); settleErr != nil {
				return settleErr
			}
			sentAt = d.clock.Now()
			err = submit()
		}
	}
	if err != nil {
		return err
	}
	return d.confirmSteer(ctx, terminalID, sentAt, resumedSeq)
}

// pressKeys uses the same endpoint and body as nebula term key. These are
// keystrokes, not pasted text, so numbered dialogs can consume the selection.
func (d *Driver) pressKeys(ctx context.Context, terminalID string, keys []string) error {
	path := fmt.Sprintf("/api/code-terminals/%s/keys", url.PathEscape(terminalID))
	return d.doJSON(ctx, http.MethodPost, path, struct {
		Keys []string `json:"keys"`
	}{Keys: keys}, nil)
}

func (d *Driver) confirmSteer(ctx context.Context, terminalID string, sentAt time.Time, resumedSeq int64) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	deadline := d.clock.Now().Add(lane.AnswerDeliveryWindow)
	timeout := d.clock.AfterFunc(lane.AnswerDeliveryWindow, cancel)
	defer timeout.Stop()
	flushAt := d.clock.Now().Add(lane.AnswerDeliveryWindow / 2)
	flushed := false
	cause := errors.New("agent did not resume within delivery window")
	for {
		term, err := d.GetTerminal(ctx, terminalID)
		if err == nil && isWorkingAgentState(term.AgentState) {
			return nil
		}
		d.mu.Lock()
		resumed := d.lastResumed[terminalID]
		d.mu.Unlock()
		if resumed.Seq > resumedSeq && !resumed.At.IsZero() && !resumed.At.Before(sentAt) {
			return nil
		}
		if ctx.Err() != nil {
			break
		}
		now := d.clock.Now()
		if !now.Before(deadline) {
			break
		}
		if err != nil {
			cause = err
		} else if !flushed && !now.Before(flushAt) && strings.EqualFold(term.AgentState, "idle") {
			flushed = true
			var keys []string
			switch strings.ToLower(term.Kind) {
			case "codex":
				keys = []string{"Esc", "Enter"}
			case "claude":
				keys = []string{"Enter"}
			case "agy":
				keys = []string{"M-m"}
			}
			if len(keys) > 0 {
				if err := d.pressKeys(ctx, terminalID, keys); err != nil {
					return &lane.UndeliveredError{TerminalID: terminalID, Cause: err}
				}
				continue
			}
		}
		timer := d.clock.NewTimer(min(d.pollInterval, deadline.Sub(now)))
		select {
		case <-ctx.Done():
		case <-timer.C():
		}
		timer.Stop()
		if ctx.Err() != nil {
			break
		}
	}
	if ctx.Err() != nil {
		cause = errors.Join(cause, ctx.Err())
	}
	return &lane.UndeliveredError{TerminalID: terminalID, Cause: cause}
}

// GetTerminal fetches metadata for a single terminal via GET /api/code-terminals/{id}.
func (d *Driver) GetTerminal(ctx context.Context, terminalID string) (*TerminalInfo, error) {
	path := fmt.Sprintf("/api/code-terminals/%s", url.PathEscape(terminalID))
	var term TerminalInfo
	if err := d.doJSON(ctx, http.MethodGet, path, nil, &term); err != nil {
		return nil, err
	}
	return &term, nil
}

// TerminalAgentState reports the agent lifecycle state used by the controller
// to avoid restarting a terminal that is actively working.
func (d *Driver) TerminalAgentState(ctx context.Context, terminalID string) (string, error) {
	term, err := d.GetTerminal(ctx, terminalID)
	if err != nil {
		return "", err
	}
	return term.AgentState, nil
}

// Stop stops a terminal or, when TerminalID is empty, all terminals in the session.
func (d *Driver) Stop(ctx context.Context, req lane.StopRequest) error {
	terminalID := strings.TrimSpace(req.TerminalID)
	sessionID := strings.TrimSpace(req.SessionID)
	if terminalID != "" {
		d.mu.Lock()
		if d.stoppedTerminals == nil {
			d.stoppedTerminals = make(map[string]bool)
		}
		d.stoppedTerminals[terminalID] = true
		d.mu.Unlock()
		path := fmt.Sprintf("/api/code-terminals/%s/stop", url.PathEscape(terminalID))
		return d.doJSON(ctx, http.MethodPost, path, struct{}{}, nil)
	}
	if sessionID != "" {
		d.mu.Lock()
		if d.stoppedTerminals == nil {
			d.stoppedTerminals = make(map[string]bool)
		}
		if req.LaneID != "" {
			if active := d.activeTerminals[driverLaneKey(req.RunID, req.LaneID)]; active != "" {
				d.stoppedTerminals[active] = true
			}
		}
		for _, target := range d.spawned[req.RunID] {
			if target.sessionID == sessionID && target.terminalID != "" {
				d.stoppedTerminals[target.terminalID] = true
			}
		}
		d.mu.Unlock()
		path := fmt.Sprintf("/api/code-sessions/%s/stop-all", url.PathEscape(sessionID))
		return d.doJSON(ctx, http.MethodPost, path, struct{}{}, nil)
	}
	return fmt.Errorf("%w: terminal_id or session_id is required for Stop", store.ErrInvalidArgument)
}

// ExecResult describes the output of a command executed inside a Nebula code session.
type ExecResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode int    `json:"exitCode"`
}

// ExecSession executes a non-interactive command inside a Nebula code session.
func (d *Driver) ExecSession(ctx context.Context, sessionID string, command []string) (ExecResult, error) {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return ExecResult{}, fmt.Errorf("%w: session_id is required for ExecSession", store.ErrInvalidArgument)
	}
	path := fmt.Sprintf("/api/code-sessions/%s/exec", url.PathEscape(sessionID))
	body := struct {
		Command []string `json:"command"`
	}{Command: command}
	var out ExecResult
	if err := d.doJSON(ctx, http.MethodPost, path, body, &out); err != nil {
		return ExecResult{}, err
	}
	return out, nil
}

// LaneGitState inspects the lane session's git working tree and unpushed commits.
func (d *Driver) LaneGitState(ctx context.Context, ln lane.Lane) (lane.GitState, error) {
	if strings.TrimSpace(ln.SessionID) == "" {
		return lane.GitState{}, nil
	}
	branch := strings.TrimPrefix(strings.TrimSpace(ln.Branch), "origin/")
	base := strings.TrimPrefix(cmp.Or(strings.TrimSpace(ln.BaseBranch), d.baseBranch, "main"), "origin/")
	const script = `set -e
base="$1"
branch="$2"
ref="origin/$base"
if [ -n "$branch" ] && git rev-parse --verify "origin/$branch" >/dev/null 2>&1; then
  ref="origin/$branch"
fi
count=$(git rev-list --count "$ref..HEAD" 2>/dev/null || echo 0)
oldest=$(git log --reverse --format=%ct "$ref..HEAD" 2>/dev/null | head -n 1)
printf "COMMITS:%s\nOLDEST_COMMIT:%s\n" "$count" "$oldest"
git status --porcelain 2>/dev/null | while IFS= read -r line; do
  [ -z "$line" ] && continue
  file=$(printf "%s" "$line" | cut -c4-)
  case "$file" in
    *" -> "*) file=${file##* -> } ;;
  esac
  if [ "$file" = ".claude/settings.local.json" ]; then
    continue
  fi
  mtime=$(stat -c %Y "$file" 2>/dev/null || echo 0)
  printf "DIRTY:%s\t%s\n" "$mtime" "$file"
done
`
	res, err := d.ExecSession(ctx, ln.SessionID, []string{"sh", "-c", script, "--", base, branch})
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return lane.GitState{}, nil
		}
		return lane.GitState{}, err
	}
	return parseGitStateOutput(res.Stdout), nil
}

func parseGitStateOutput(out string) lane.GitState {
	var st lane.GitState
	var oldestUnix int64
	for _, rawLine := range strings.Split(out, "\n") {
		line := strings.TrimSpace(rawLine)
		switch {
		case strings.HasPrefix(line, "COMMITS:"):
			n, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "COMMITS:")))
			if err == nil && n > 0 {
				st.UnpushedCommits = n
			}
		case strings.HasPrefix(line, "OLDEST_COMMIT:"):
			ts, err := strconv.ParseInt(strings.TrimSpace(strings.TrimPrefix(line, "OLDEST_COMMIT:")), 10, 64)
			if err == nil && ts > 0 && (oldestUnix == 0 || ts < oldestUnix) {
				oldestUnix = ts
			}
		case strings.HasPrefix(line, "DIRTY:"):
			rest := strings.TrimPrefix(line, "DIRTY:")
			mtimeStr, file, ok := strings.Cut(rest, "\t")
			if !ok {
				file = strings.TrimSpace(rest)
			} else {
				file = strings.TrimSpace(file)
				if ts, err := strconv.ParseInt(strings.TrimSpace(mtimeStr), 10, 64); err == nil && ts > 0 && (oldestUnix == 0 || ts < oldestUnix) {
					oldestUnix = ts
				}
			}
			if file != "" && file != ".claude/settings.local.json" {
				st.DirtyFiles = append(st.DirtyFiles, file)
			}
		}
	}
	if oldestUnix > 0 {
		st.OldestUnpushedAt = time.Unix(oldestUnix, 0).UTC()
	}
	return st
}

// LaneSessionHealth inspects the lane session's disk usage and active gate/review processes.
func (d *Driver) LaneSessionHealth(ctx context.Context, ln lane.Lane) (lane.SessionHealth, error) {
	if strings.TrimSpace(ln.SessionID) == "" {
		return lane.SessionHealth{}, nil
	}
	script := `df -P . 2>/dev/null | awk 'NR==2 {gsub(/%/,"",$5); print "DISK:" $5}'
ps -eo args= 2>/dev/null | while IFS= read -r cmd; do
  printf "PROC:%s\n" "$cmd"
done
`
	res, err := d.ExecSession(ctx, ln.SessionID, []string{"sh", "-c", script})
	if err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound {
			return lane.SessionHealth{}, nil
		}
		return lane.SessionHealth{}, err
	}
	return parseSessionHealthOutput(res.Stdout), nil
}

func parseSessionHealthOutput(out string) lane.SessionHealth {
	var h lane.SessionHealth
	for _, rawLine := range strings.Split(out, "\n") {
		line := strings.TrimSpace(rawLine)
		switch {
		case strings.HasPrefix(line, "DISK:"):
			pct, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(line, "DISK:")))
			if err == nil && pct > 0 {
				h.DiskUsedPercent = pct
			}
		case strings.HasPrefix(line, "PROC:"):
			cmd := strings.TrimSpace(strings.TrimPrefix(line, "PROC:"))
			if lane.IsBusyProcess(cmd) {
				h.BusyProcesses = append(h.BusyProcesses, cmd)
			}
		}
	}
	return h
}

// ReleaseSession releases a code session via POST /api/code-sessions/{id}/release.
func (d *Driver) ReleaseSession(ctx context.Context, sessionID string, forceArg ...bool) error {
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return fmt.Errorf("%w: session_id is required for ReleaseSession", store.ErrInvalidArgument)
	}
	path := fmt.Sprintf("/api/code-sessions/%s/release", url.PathEscape(sessionID))
	var body any = struct{}{}
	if len(forceArg) > 0 && forceArg[0] {
		body = struct {
			Force bool `json:"force"`
		}{Force: true}
	}
	return d.doJSON(ctx, http.MethodPost, path, body, nil)
}

// ReportOnlyNoWork compares the lane branch against the current remote base.
// A missing branch is the normal no-change result for lanes that created no PR.
func (d *Driver) ReportOnlyNoWork(ctx context.Context, ln lane.Lane) (bool, error) {
	if strings.TrimSpace(d.repoFullName) == "" || strings.TrimSpace(ln.Branch) == "" {
		return false, nil
	}
	base := cmp.Or(strings.TrimSpace(ln.BaseBranch), d.baseBranch)
	path := "https://api.github.com/repos/" + d.repoFullName + "/compare/" + url.PathEscape(base) + "..." + url.PathEscape(strings.TrimSpace(ln.Branch))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, path, http.NoBody)
	if err != nil {
		return false, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if d.githubToken != "" {
		req.Header.Set("Authorization", "Bearer "+d.githubToken)
	}
	resp, err := d.httpClient.Do(req)
	if err != nil {
		return false, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		if d.githubToken == "" {
			return false, fmt.Errorf("github compare: status 404 without authenticated access")
		}
		accessReq, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+d.repoFullName, http.NoBody)
		if err != nil {
			return false, err
		}
		accessReq.Header.Set("Accept", "application/vnd.github+json")
		accessReq.Header.Set("Authorization", "Bearer "+d.githubToken)
		accessResp, err := d.httpClient.Do(accessReq)
		if err != nil {
			return false, err
		}
		defer accessResp.Body.Close()
		if accessResp.StatusCode < 200 || accessResp.StatusCode >= 300 {
			return false, fmt.Errorf("github repository access check: status %d", accessResp.StatusCode)
		}
		return true, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return false, fmt.Errorf("github compare: status %d", resp.StatusCode)
	}
	var result struct {
		AheadBy int `json:"ahead_by"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return false, err
	}
	return result.AheadBy == 0, nil
}

type phaseReportResponse struct {
	ReportedAt  time.Time `json:"reportedAt"`
	TerminalID  string    `json:"terminalId"`
	TerminalURL string    `json:"terminalUrl"`
	Outcome     string    `json:"outcome"`
	Head        string    `json:"head"`
	Body        string    `json:"body"`
	Seq         int64     `json:"seq"`
	Revision    int       `json:"revision"`
	PR          int       `json:"pr"`
}

// ReadReport reads the latest filed phase report for terminalID.
func (d *Driver) ReadReport(ctx context.Context, runID, laneID, terminalID string) (*lane.PhaseReport, error) {
	return d.ReadReportRevision(ctx, runID, laneID, terminalID, 0)
}

// ReadReportRevision reads a specific revision (or latest when revision <= 0) of a phase report.
func (d *Driver) ReadReportRevision(ctx context.Context, _, laneID, terminalID string, revision int) (*lane.PhaseReport, error) {
	terminalID = strings.TrimSpace(terminalID)
	if terminalID == "" {
		return nil, fmt.Errorf("%w: terminal_id is required", store.ErrInvalidArgument)
	}
	path := fmt.Sprintf("/api/code-terminals/%s/phase-report", url.PathEscape(terminalID))
	if revision > 0 {
		path += fmt.Sprintf("?revision=%d", revision)
	}
	var resp phaseReportResponse
	if err := d.doJSON(ctx, http.MethodGet, path, nil, &resp); err != nil {
		var apiErr *APIError
		if errors.As(err, &apiErr) && (apiErr.StatusCode == http.StatusNotFound || apiErr.Code == "NO_REPORT") {
			return nil, fmt.Errorf("%w: terminal %q", ErrNoReport, terminalID)
		}
		return nil, err
	}
	return parseAPIReport(resp, laneID, terminalID, d.clock.Now().UTC())
}

func parseAPIReport(resp phaseReportResponse, laneID, terminalID string, fallbackNow time.Time) (*lane.PhaseReport, error) {
	rawBody := resp.Body
	rep, err := lane.ParsePhaseReport(rawBody)
	if err != nil {
		if resp.Outcome != "" {
			synthesized := fmt.Sprintf("Outcome: %s\n%s", resp.Outcome, rawBody)
			rep, err = lane.ParsePhaseReport(synthesized)
			if err == nil {
				rep.Raw = rawBody
			}
		}
		if err != nil {
			return nil, err
		}
	}
	if resp.Outcome != "" && lane.ReportOutcome(resp.Outcome).Valid() {
		rep.Outcome = lane.ReportOutcome(resp.Outcome)
	}
	rep.HeadSHA = cmp.Or(rep.HeadSHA, resp.Head)
	if resp.PR > 0 && rep.PR == 0 {
		rep.PR = resp.PR
	}
	rep.LaneID = laneID
	rep.TerminalID = cmp.Or(resp.TerminalID, terminalID)
	if !resp.ReportedAt.IsZero() {
		rep.ReportedAt = resp.ReportedAt.UTC()
	} else if rep.ReportedAt.IsZero() {
		rep.ReportedAt = fallbackNow
	}
	return rep, nil
}

// GetCapacity queries GET /api/code-sessions/capacity for cluster pool state.
func (d *Driver) GetCapacity(ctx context.Context) (*CapacityStatus, error) {
	var capStatus CapacityStatus
	if err := d.doJSON(ctx, http.MethodGet, "/api/code-sessions/capacity", nil, &capStatus); err != nil {
		return nil, err
	}
	return &capStatus, nil
}

// TerminalEvent represents a single event frame from GET /api/code-terminals/{id}/events/stream.
type TerminalEvent struct {
	At               time.Time       `json:"at"`
	TerminalID       string          `json:"terminalId"`
	Type             string          `json:"type"`
	AgentState       string          `json:"agentState,omitempty"`
	Text             string          `json:"text,omitempty"`
	EOTSource        string          `json:"eotSource,omitempty"`
	Outcome          string          `json:"outcome,omitempty"`
	BackgroundTasks  json.RawMessage `json:"backgroundTasks,omitempty"`
	Detail           json.RawMessage `json:"detail,omitempty"`
	Seq              int64           `json:"seq"`
	PendingSubagents int             `json:"pendingSubagents,omitempty"`
}

// StreamEventPayload is serialized into store.Event.Payload for streamed lane events.
type StreamEventPayload struct {
	TerminalEvent *TerminalEvent    `json:"terminal_event,omitempty"`
	Report        *lane.PhaseReport `json:"report,omitempty"`
	Usage         *TerminalUsage    `json:"usage,omitempty"`
	UsageLimit    *UsageLimitInfo   `json:"usage_limit,omitempty"`
	Capacity      *CapacityStatus   `json:"capacity,omitempty"`
	SessionID     string            `json:"session_id,omitempty"`
	TerminalID    string            `json:"terminal_id,omitempty"`
	AgentSignal   string            `json:"agent_signal,omitempty"`
	ReportError   string            `json:"report_error,omitempty"`
}

// StreamCursorConsumer returns the per-terminal EventStore cursor key used by
// StreamEvents to persist a terminal's Nebula SSE sequence number without
// colliding across terminals or with store.Event.Seq consumers (such as "controller").
func StreamCursorConsumer(consumer, terminalID string) string {
	terminalID = strings.TrimSpace(terminalID)
	consumer = strings.TrimSpace(consumer)
	if consumer == "" {
		return "nebula:terminal:" + terminalID
	}
	if strings.HasPrefix(consumer, "nebula:") && strings.HasSuffix(consumer, ":"+terminalID) {
		return consumer
	}
	return fmt.Sprintf("nebula:terminal:%s:%s", consumer, terminalID)
}

// StreamRequest configures streaming a terminal's SSE events into store.EventStore.
type StreamRequest struct {
	Events                store.EventStore
	RunID                 string
	LaneID                string
	SessionID             string
	TerminalID            string
	Consumer              string
	AfterSeq              int64
	StopOnExit            bool
	StopOnReport          bool
	EmitIntermediateTurns bool
}

func (d *Driver) resolveEvents(events store.EventStore) (store.EventStore, error) {
	if events := cmp.Or(events, d.events); events != nil {
		return events, nil
	}
	return nil, fmt.Errorf("%w: store.EventStore is required", store.ErrInvalidArgument)
}

// StreamEvents streams terminal SSE events into store.EventStore.
func (d *Driver) StreamEvents(ctx context.Context, req StreamRequest) error {
	events, err := d.resolveEvents(req.Events)
	if err != nil {
		return err
	}
	if strings.TrimSpace(req.RunID) == "" || strings.TrimSpace(req.TerminalID) == "" {
		return fmt.Errorf("%w: run_id and terminal_id are required for StreamEvents", store.ErrInvalidArgument)
	}

	cursorConsumer := StreamCursorConsumer(req.Consumer, req.TerminalID)
	afterSeq := req.AfterSeq
	if afterSeq == 0 {
		if cur, err := events.GetCursor(ctx, req.RunID, cursorConsumer); err == nil && cur.Seq > 0 {
			afterSeq = cur.Seq
		}
	}

	path := fmt.Sprintf("/api/code-terminals/%s/events/stream", url.PathEscape(req.TerminalID))
	httpReq, err := d.newRequest(ctx, http.MethodGet, path, http.NoBody)
	if err != nil {
		return err
	}
	httpReq.Header.Set("Accept", "text/event-stream")
	if afterSeq > 0 {
		httpReq.Header.Set("Last-Event-ID", strconv.FormatInt(afterSeq, 10))
	}

	resp, err := d.streamClient.Do(httpReq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeAPIError(http.MethodGet, path, resp)
	}

	return d.consumeSSEStream(ctx, resp.Body, events, req, cursorConsumer, afterSeq)
}

func (d *Driver) consumeSSEStream(
	ctx context.Context,
	r io.Reader,
	events store.EventStore,
	req StreamRequest,
	cursorConsumer string,
	afterSeq int64,
) error {
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), maxSSELineBytes)

	var (
		frameID      string
		frameEvent   string
		dataLines    []string
		highWaterSeq = afterSeq
	)

	resetFrame := func() {
		frameID = ""
		frameEvent = ""
		dataLines = dataLines[:0]
	}

	dispatchFrame := func() (bool, error) {
		if len(dataLines) == 0 && frameEvent == "" {
			resetFrame()
			return false, nil
		}
		rawData := strings.Join(dataLines, "\n")
		evType := frameEvent
		idStr := frameID
		resetFrame()

		if isHeartbeatSSEEvent(evType) {
			return false, nil
		}

		var termEv TerminalEvent
		if rawData != "" {
			if err := json.Unmarshal([]byte(rawData), &termEv); err != nil {
				return false, fmt.Errorf("nebula sse decode event: %w", err)
			}
		}
		termEv.Type = cmp.Or(termEv.Type, evType)
		if termEv.Seq == 0 && idStr != "" {
			if seq, err := strconv.ParseInt(idStr, 10, 64); err == nil {
				termEv.Seq = seq
			}
		}
		termEv.TerminalID = cmp.Or(termEv.TerminalID, req.TerminalID)
		if termEv.Seq > 0 {
			if termEv.Seq <= highWaterSeq {
				return false, nil
			}
			highWaterSeq = termEv.Seq
		}

		if termEv.Type == "turn.resumed" {
			d.mu.Lock()
			if termEv.Seq > d.lastResumed[termEv.TerminalID].Seq {
				d.lastResumed[termEv.TerminalID] = termEv
			}
			d.mu.Unlock()
		}
		storeEv, ok, err := d.mapTerminalEvent(ctx, req, termEv)
		if err != nil {
			return false, err
		}
		if ok && !hasDuplicateStreamLaneReport(ctx, events, storeEv) {
			if _, err := events.Append(ctx, storeEv); err != nil {
				return false, err
			}
		}
		if cursorConsumer != "" && termEv.Seq > 0 {
			if err := events.SetCursor(ctx, req.RunID, cursorConsumer, termEv.Seq); err != nil {
				return false, err
			}
		}
		if (req.StopOnExit && isTerminalExitEvent(termEv.Type)) || (req.StopOnReport && termEv.Type == "phase.reported") {
			return true, nil
		}
		return false, nil
	}

	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			stop, err := dispatchFrame()
			if err != nil || stop {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, val, hasColon := strings.Cut(line, ":")
		if !hasColon {
			continue
		}
		val = strings.TrimPrefix(val, " ")
		switch field {
		case "id":
			frameID = val
		case "event":
			frameEvent = val
		case "data":
			dataLines = append(dataLines, val)
		}
	}
	if err := scanner.Err(); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return ctx.Err()
		}
		return err
	}
	stop, err := dispatchFrame()
	if err != nil {
		return err
	}
	if !stop && (req.StopOnExit || req.StopOnReport) {
		return ErrStreamEnded
	}
	return nil
}

func isHeartbeatSSEEvent(eventType string) bool {
	switch strings.ToLower(strings.TrimSpace(eventType)) {
	case "ping", "heartbeat", "keepalive":
		return true
	default:
		return false
	}
}

func isTerminalExitEvent(eventType string) bool {
	switch eventType {
	case "terminal.exited", "terminal.failed", "terminal.start_failed":
		return true
	default:
		return false
	}
}

func (d *Driver) mapTerminalEvent(ctx context.Context, req StreamRequest, termEv TerminalEvent) (store.Event, bool, error) {
	evTime := termEv.At.UTC()
	if evTime.IsZero() {
		evTime = d.clock.Now().UTC()
	}
	source := "nebula:terminal:" + termEv.TerminalID
	payload := StreamEventPayload{
		SessionID:     req.SessionID,
		TerminalID:    termEv.TerminalID,
		TerminalEvent: &termEv,
	}

	var (
		kind    store.EventKind
		class   store.EventClass
		summary string
		urgent  bool
	)

	switch termEv.Type {
	case "phase.reported":
		kind = store.EventLaneReport
		outcome := lane.ReportOutcome(strings.TrimSpace(termEv.Outcome))
		rep, err := d.ReadReport(ctx, req.RunID, req.LaneID, termEv.TerminalID)
		if err != nil {
			payload.ReportError = err.Error()
		} else if rep != nil {
			payload.Report = rep
			outcome = rep.Outcome
		}
		switch {
		case payload.ReportError != "":
			class = store.EventClassJudgment
			urgent = true
			if outcome.Valid() {
				summary = fmt.Sprintf("lane %s phase reported (%s): failed to read report: %s", req.LaneID, outcome, payload.ReportError)
			} else {
				summary = fmt.Sprintf("lane %s phase reported: failed to read report: %s", req.LaneID, payload.ReportError)
			}
		case !outcome.Valid():
			class = store.EventClassJudgment
			urgent = true
			summary = fmt.Sprintf("lane %s phase reported with invalid outcome %q", req.LaneID, outcome)
		case outcome == lane.OutcomeBlocked || outcome == lane.OutcomeNeedsInput:
			class = store.EventClassJudgment
			urgent = true
			summary = fmt.Sprintf("lane %s phase reported: %s", req.LaneID, outcome)
		default:
			class = store.EventClassAuto
			summary = fmt.Sprintf("lane %s phase reported: %s", req.LaneID, outcome)
		}

	case "turn.ended":
		outcome := strings.ToLower(strings.TrimSpace(termEv.Outcome))
		if termEv.PendingSubagents > 0 && !req.EmitIntermediateTurns && !isStopTurnOutcome(outcome) {
			return store.Event{}, false, nil
		}
		switch outcome {
		case "usage_limit", "usage_limited", "capacity":
			kind = store.EventStall
			class = store.EventClassJudgment
			urgent = true
			summary = fmt.Sprintf("lane %s turn hit %s", req.LaneID, outcome)
		case "auth_failed", "api_error":
			kind = store.EventPhaseExit
			class = store.EventClassJudgment
			urgent = true
			summary = fmt.Sprintf("lane %s turn failed: %s", req.LaneID, outcome)
		default:
			kind = store.EventPhaseExit
			class = store.EventClassAuto
			summary = fmt.Sprintf("lane %s turn ended (%s)", req.LaneID, cmp.Or(outcome, "completed"))
		}

	case "input.waiting":
		if inputWaitingIsWorking(&termEv) {
			return store.Event{}, false, nil
		}
		kind = store.EventStall
		class = store.EventClassJudgment
		urgent = strings.ToLower(strings.TrimSpace(termEv.EOTSource)) != "heuristic"
		summary = fmt.Sprintf("lane %s waiting for input", req.LaneID)
		if termEv.Text != "" {
			summary = fmt.Sprintf("lane %s waiting for input: %s", req.LaneID, termEv.Text)
		}

	case "terminal.exited", "terminal.failed", "terminal.start_failed":
		kind = store.EventPhaseExit
		outcome := strings.ToLower(strings.TrimSpace(termEv.Outcome))
		if termEv.Type != "terminal.exited" || outcome == "failed" || outcome == "error" || outcome == "start_failed" {
			class = store.EventClassJudgment
			urgent = true
		} else {
			class = store.EventClassAuto
		}
		summary = fmt.Sprintf("lane %s %s (%s)", req.LaneID, termEv.Type, cmp.Or(outcome, termEv.Type))

	case "turn.capacity", "terminal.usage_limited", "usage.updated", "capacity", "usage":
		kind = store.EventStall
		urgent = termEv.Type == "turn.capacity" || termEv.Type == "terminal.usage_limited"
		class = store.EventClassAuto
		if urgent {
			class = store.EventClassJudgment
		}
		summary = fmt.Sprintf("lane %s %s", req.LaneID, termEv.Type)
		if termEv.Text != "" {
			summary = fmt.Sprintf("lane %s %s: %s", req.LaneID, termEv.Type, termEv.Text)
		}

	default:
		return store.Event{}, false, nil
	}

	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return store.Event{}, false, err
	}

	return store.Event{
		CreatedAt: evTime,
		RunID:     req.RunID,
		LaneID:    req.LaneID,
		Kind:      kind,
		Class:     class,
		Source:    source,
		Summary:   summary,
		Payload:   rawPayload,
		Urgent:    urgent,
	}, true, nil
}

func isStopTurnOutcome(outcome string) bool {
	switch outcome {
	case "auth_failed", "usage_limit", "usage_limited", "api_error", "capacity":
		return true
	default:
		return false
	}
}

func hasDuplicateStreamLaneReport(ctx context.Context, events store.EventStore, candidate store.Event) bool {
	if candidate.Kind != store.EventLaneReport || events == nil {
		return false
	}
	existing, err := events.List(ctx, store.EventFilter{
		RunID:  candidate.RunID,
		LaneID: candidate.LaneID,
		Kinds:  []store.EventKind{store.EventLaneReport},
	})
	if err != nil || len(existing) == 0 {
		return false
	}
	var candPayload StreamEventPayload
	_ = json.Unmarshal(candidate.Payload, &candPayload)
	candKey := ""
	if candPayload.Report != nil {
		candKey = reportKey(candPayload.TerminalID, candPayload.Report)
	}
	for i := range existing {
		ev := &existing[i]
		if ev.Source == candidate.Source && ev.Summary == candidate.Summary {
			var p StreamEventPayload
			if json.Unmarshal(ev.Payload, &p) == nil && p.Report != nil && candKey != "" {
				if reportKey(p.TerminalID, p.Report) == candKey {
					return true
				}
				continue
			}
			return true
		}
		if candKey != "" {
			var rep lane.PhaseReport
			if json.Unmarshal(ev.Payload, &rep) == nil && rep.Outcome != "" {
				if reportKey(cmp.Or(rep.TerminalID, candPayload.TerminalID), &rep) == candKey {
					return true
				}
			}
		}
	}
	return false
}

// PollSessionUsage polls session terminal usage and appends stall/usage events
// for terminals whose usage/limit snapshot changed since the last poll.
func (d *Driver) PollSessionUsage(
	ctx context.Context,
	events store.EventStore,
	runID, laneID, sessionID, terminalID string,
) ([]store.Event, error) {
	events, err := d.resolveEvents(events)
	if err != nil {
		return nil, err
	}
	detail, err := d.GetSession(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	var appended []store.Event
	for i := range detail.Terminals {
		t := &detail.Terminals[i]
		if terminalID != "" && t.ID != terminalID {
			continue
		}
		if t.Usage == nil && t.UsageLimit == nil && t.AgentSignal == "" {
			continue
		}
		payload := StreamEventPayload{
			SessionID:   sessionID,
			TerminalID:  t.ID,
			Usage:       t.Usage,
			UsageLimit:  t.UsageLimit,
			AgentSignal: t.AgentSignal,
		}
		rawPayload, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		snapKey := runID + ":" + laneID + ":" + t.ID
		snapVal := string(rawPayload)
		d.mu.Lock()
		unchanged := d.lastUsage[snapKey] == snapVal
		d.mu.Unlock()
		if unchanged {
			continue
		}
		urgent := t.AgentSignal == "usage_limited" || t.UsageLimit != nil
		class := store.EventClassAuto
		summary := fmt.Sprintf("lane %s usage snapshot", laneID)
		if urgent {
			class = store.EventClassJudgment
			summary = fmt.Sprintf("lane %s terminal %s usage limited", laneID, t.ID)
		} else if t.Usage != nil {
			summary = fmt.Sprintf("lane %s usage: in=%d out=%d cost=$%.4f", laneID, t.Usage.InputTokens, t.Usage.OutputTokens, t.Usage.EstimatedCostUSD)
		}
		ev, err := events.Append(ctx, store.Event{
			CreatedAt: d.clock.Now().UTC(),
			RunID:     runID,
			LaneID:    laneID,
			Kind:      store.EventStall,
			Class:     class,
			Source:    "nebula:usage:" + t.ID,
			Summary:   summary,
			Payload:   rawPayload,
			Urgent:    urgent,
		})
		if err != nil {
			return nil, err
		}
		d.mu.Lock()
		d.lastUsage[snapKey] = snapVal
		d.mu.Unlock()
		appended = append(appended, ev)
	}
	return appended, nil
}

// PollCapacity polls cluster capacity and appends a capacity event when the
// snapshot changes (returning a zero store.Event and nil error when unchanged).
func (d *Driver) PollCapacity(ctx context.Context, events store.EventStore, runID, laneID string) (store.Event, error) {
	events, err := d.resolveEvents(events)
	if err != nil {
		return store.Event{}, err
	}
	capStatus, err := d.GetCapacity(ctx)
	if err != nil {
		return store.Event{}, err
	}
	urgent := false
	totalRunning := 0
	totalWaiting := 0
	for _, b := range capStatus.Backends {
		totalRunning += b.Running
		totalWaiting += b.Waiting
		if b.Full || b.PoolAtMax || b.Waiting > 0 {
			urgent = true
		}
	}
	class := store.EventClassAuto
	if urgent {
		class = store.EventClassJudgment
	}
	payload := StreamEventPayload{
		Capacity: capStatus,
	}
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return store.Event{}, err
	}
	snapKey := runID + ":" + laneID
	snapVal := string(rawPayload)
	d.mu.Lock()
	unchanged := d.lastCapacity[snapKey] == snapVal
	d.mu.Unlock()
	if unchanged {
		return store.Event{}, nil
	}
	summary := fmt.Sprintf("nebula capacity: running=%d waiting=%d", totalRunning, totalWaiting)
	ev, err := events.Append(ctx, store.Event{
		CreatedAt: d.clock.Now().UTC(),
		RunID:     runID,
		LaneID:    laneID,
		Kind:      store.EventStall,
		Class:     class,
		Source:    "nebula:capacity",
		Summary:   summary,
		Payload:   rawPayload,
		Urgent:    urgent,
	})
	if err != nil {
		return store.Event{}, err
	}
	d.mu.Lock()
	d.lastCapacity[snapKey] = snapVal
	d.mu.Unlock()
	return ev, nil
}

func (d *Driver) resolveTargetRepository(ctx context.Context, reqRepoID, reqRepo string) (repoID, repoSlug string, err error) {
	reqRepoID = strings.TrimSpace(reqRepoID)
	reqRepo = strings.TrimSpace(reqRepo)
	if reqRepoID != "" || reqRepo != "" {
		if reqRepoID != "" {
			if lane.IsRepositorySlug(reqRepoID) {
				repoSlug = lane.NormalizeRepositorySlug(reqRepoID)
			} else {
				repoID = reqRepoID
			}
		}
		if reqRepo != "" {
			if lane.IsRepositorySlug(reqRepo) {
				repoSlug = cmp.Or(repoSlug, lane.NormalizeRepositorySlug(reqRepo))
			} else if repoID == "" {
				repoID = reqRepo
			}
		}
		if repoID == "" && repoSlug != "" {
			id, err := d.lookupRepositoryID(ctx, repoSlug)
			if err != nil {
				return "", "", err
			}
			repoID = id
		}
		if repoID == "" {
			return "", "", fmt.Errorf("nebula resolve repository id: repository is required")
		}
		return repoID, repoSlug, nil
	}

	id, err := d.resolveRepositoryID(ctx)
	if err != nil {
		return "", "", err
	}
	d.mu.Lock()
	slug := lane.NormalizeRepositorySlug(d.repoFullName)
	d.mu.Unlock()
	return id, slug, nil
}

func (d *Driver) lookupRepositoryID(ctx context.Context, target string) (string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return "", fmt.Errorf("nebula resolve repository id: repository is required")
	}
	targetSlug := lane.NormalizeRepositorySlug(target)
	var resp struct {
		Repositories []struct {
			ID       string `json:"id"`
			FullName string `json:"fullName"`
			GitURL   string `json:"gitUrl"`
		} `json:"repositories"`
	}
	if err := d.doJSON(ctx, http.MethodGet, "/api/code-repositories", nil, &resp); err != nil {
		return "", fmt.Errorf("nebula resolve repository id: %w", err)
	}
	for _, r := range resp.Repositories {
		if r.ID == target ||
			(r.FullName != "" && strings.EqualFold(lane.NormalizeRepositorySlug(r.FullName), targetSlug)) ||
			(r.GitURL != "" && strings.EqualFold(lane.NormalizeRepositorySlug(r.GitURL), targetSlug)) {
			if r.ID != "" {
				return r.ID, nil
			}
		}
	}
	return "", fmt.Errorf("nebula resolve repository id: no matching repository found for %q", target)
}

func (d *Driver) verifySessionRepository(ctx context.Context, sess SessionInfo, wantRepoID, wantRepoSlug string, requireVerified bool) error {
	gotID := strings.TrimSpace(sess.RepositoryID)
	gotSlug := lane.NormalizeRepositorySlug(sess.GitURL)
	var getErr error
	if requireVerified && gotID == "" && gotSlug == "" && sess.ID != "" {
		if detail, err := d.GetSession(ctx, sess.ID); err == nil {
			gotID = strings.TrimSpace(detail.Session.RepositoryID)
			gotSlug = lane.NormalizeRepositorySlug(detail.Session.GitURL)
		} else {
			getErr = err
		}
	}
	if gotID != "" && wantRepoID != "" {
		if gotID != wantRepoID {
			return fmt.Errorf("nebula session %s repositoryId %q does not match expected %q", sess.ID, gotID, wantRepoID)
		}
		return nil
	}
	if gotSlug != "" {
		if wantRepoSlug != "" {
			if !strings.EqualFold(gotSlug, wantRepoSlug) {
				return fmt.Errorf("nebula session %s gitUrl %q does not match expected repository %q", sess.ID, sess.GitURL, wantRepoSlug)
			}
			return nil
		}
		if wantRepoID != "" && !strings.EqualFold(gotSlug, wantRepoID) {
			resolvedID, err := d.lookupRepositoryID(ctx, gotSlug)
			if err != nil {
				return fmt.Errorf("nebula session %s verify gitUrl %q against expected repository %q: %w", sess.ID, sess.GitURL, wantRepoID, err)
			}
			if resolvedID != wantRepoID {
				return fmt.Errorf("nebula session %s gitUrl %q (repository %q) does not match expected %q", sess.ID, sess.GitURL, resolvedID, wantRepoID)
			}
			return nil
		}
	}
	if requireVerified && (wantRepoID != "" || wantRepoSlug != "") {
		err := fmt.Errorf("nebula session %s omitted repositoryId and gitUrl; cannot verify expected repository %q", sess.ID, cmp.Or(wantRepoSlug, wantRepoID))
		if getErr != nil {
			return errors.Join(err, fmt.Errorf("get session %s: %w", sess.ID, getErr))
		}
		return err
	}
	return nil
}

func (d *Driver) resolveRepositoryID(ctx context.Context) (string, error) {
	d.mu.Lock()
	if d.repoID != "" {
		id := d.repoID
		d.mu.Unlock()
		return id, nil
	}
	currSessID := d.currSessID
	repoFullName := d.repoFullName
	d.mu.Unlock()

	if currSessID != "" && repoFullName == "" {
		if detail, err := d.GetSession(ctx, currSessID); err == nil && detail.Session.RepositoryID != "" {
			d.mu.Lock()
			d.repoID = detail.Session.RepositoryID
			d.mu.Unlock()
			return detail.Session.RepositoryID, nil
		}
	}
	if strings.TrimSpace(repoFullName) == "" {
		return "", fmt.Errorf("nebula resolve repository id: repository is required")
	}

	chosen, err := d.lookupRepositoryID(ctx, repoFullName)
	if err != nil {
		return "", err
	}
	d.mu.Lock()
	d.repoID = chosen
	d.mu.Unlock()
	return chosen, nil
}

func (d *Driver) newRequest(ctx context.Context, method, path string, body io.Reader) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, method, d.apiURL+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+d.token)
	req.Header.Set("X-Nebula-Client", "orch")
	return req, nil
}

func (d *Driver) doJSON(ctx context.Context, method, path string, reqBody, respBody any) error {
	var bodyReader io.Reader = http.NoBody
	if reqBody != nil {
		b, err := json.Marshal(reqBody)
		if err != nil {
			return fmt.Errorf("nebula marshal request %s %s: %w", method, path, err)
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := d.newRequest(ctx, method, path, bodyReader)
	if err != nil {
		return err
	}
	if reqBody != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := d.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeAPIError(method, path, resp)
	}
	if respBody == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return nil
	}
	return json.Unmarshal(data, respBody)
}

func decodeAPIError(method, path string, resp *http.Response) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	apiErr := &APIError{
		Method:     method,
		Path:       path,
		StatusCode: resp.StatusCode,
	}
	var envelope struct {
		Code    string          `json:"code"`
		Message string          `json:"message"`
		Error   json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(data, &envelope); err == nil {
		apiErr.Code = envelope.Code
		apiErr.Message = envelope.Message
		var errStr string
		var errObj struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		switch {
		case len(envelope.Error) == 0:
		case json.Unmarshal(envelope.Error, &errStr) == nil && errStr != "":
			apiErr.Message = errStr
		case json.Unmarshal(envelope.Error, &errObj) == nil:
			apiErr.Code = cmp.Or(errObj.Code, apiErr.Code)
			apiErr.Message = cmp.Or(errObj.Message, apiErr.Message)
		}
	}
	apiErr.Message = cmp.Or(apiErr.Message, strings.TrimSpace(string(data)), http.StatusText(resp.StatusCode))
	return apiErr
}
