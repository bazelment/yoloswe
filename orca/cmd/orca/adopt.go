package main

import (
	"cmp"
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bazelment/yoloswe/orca/adopt"
	chatui "github.com/bazelment/yoloswe/orca/chat"
	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/dispatcher"
	"github.com/bazelment/yoloswe/orca/lane/nebula"
	"github.com/bazelment/yoloswe/orca/mailbox"
	mailboxnebula "github.com/bazelment/yoloswe/orca/mailbox/nebula"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/store"
)

// hostSwarms is how chat sees this machine's host swarms: one importer for
// both run formats, the loop controller /adopt stops, and the live reads the
// concierge's swarm summaries use.
type hostSwarms struct {
	// client is nil without Nebula credentials; off then says why.
	client   *nebula.Driver
	off      string
	importer adopt.Importer
	watcher  adopt.FormatWatcher
	probe    chatui.HostProbe
}

// newHostSwarms wires discovery to the real adapters: the Nebula API for run
// orchestrators and terminal states, /proc and tmux for live step loops,
// watch_lanes.sh and swarm_nebula.sh for the host loops, gh for PR state.
func newHostSwarms(cfg Config, st store.Store, clk clock.Clock) hostSwarms {
	var h hostSwarms
	h.watcher = adopt.FormatWatcher{
		SubagentMissing: "watch_lanes.sh not found (install subagent-swarm-nebula or set ORCA_WATCH_LANES)",
		NebulaMissing:   "swarm_nebula.sh not found (install nebula-swarm or set ORCA_NEBULA_SWARM_SH)",
		Panes:           adopt.TmuxPanes,
	}
	if script := adopt.FindWatchScript(); script != "" {
		h.watcher.Subagent = adopt.ScriptWatcher{Script: script}
	}
	// The step loop is found from /proc even when swarm_nebula.sh is not
	// installed here; only restarting it on release needs the script.
	h.watcher.Nebula = adopt.StepLoopWatcher{Script: adopt.FindSwarmScript()}
	h.importer.Loops = h.watcher
	client, err := nebula.New(nebula.Config{APIURL: cfg.NebulaAPIURL, Token: cfg.NebulaToken, Events: driverEventStore{st.Events()}, Clock: clk})
	if err != nil {
		h.off = "Nebula API client: " + err.Error() + " (set NEBULA_API_URL and NEBULA_TOKEN)"
		h.probe.Off = append(h.probe.Off, "lane terminal agent states (no Nebula client)")
	} else {
		h.client = client
		h.importer.Orchestrators = client
		h.probe.TerminalState = client.TerminalAgentState
	}
	if ghAvailable() {
		h.probe.PR = ghPRStatus
	} else {
		h.probe.Off = append(h.probe.Off, "PR states (gh not on PATH)")
	}
	return h
}

// newAdoption wires /adopt and /release to the real adapters: the Nebula API
// client for the run label and the format's loop controller for the host
// watcher or step loop. Each adopted run gets its own dispatcher on client.
// When an adapter cannot be built it returns the one-line reason chat shows
// instead. holder leases the adopted runs; leases under a former holder id are
// its own.
func newAdoption(ctx context.Context, cfg Config, h hostSwarms, holder string, former []string, st store.Store, clk clock.Clock, client model.ModelClient, logger *slog.Logger) (*adopt.Service, string) {
	if h.client == nil {
		return nil, h.off
	}
	prProbe := adoptedPRProbe(h, st)
	judges, plannerFn := adoptedDispatchers(ctx, cfg, h, st, h.client, clk, client, logger, prProbe)
	ctrls := adopt.NewControllers(ctx, st, h.client, clk, judges)
	ctrls.PRProbe = prProbe
	ctrls.Mailboxes = adoptedMailboxes(cfg, st)
	ctrls.Planner = plannerFn
	ctrls.MaxCostPerHourUSD = cfg.MaxCostPerHourUSD
	return &adopt.Service{
		Importer:           h.importer,
		Labeler:            h.client,
		Watcher:            h.watcher,
		Holder:             holder,
		Former:             former,
		Driver:             ctrls,
		LoopVerifyInterval: loopVerifyInterval(),
	}, ""
}

func adoptedMailboxes(cfg Config, st store.Store) func(runID string) []mailbox.Mailbox {
	baseURL := cmp.Or(cfg.NebulaAPIURL, os.Getenv("NEBULA_API_URL"))
	token := cmp.Or(cfg.NebulaToken, os.Getenv("NEBULA_TOKEN"))
	if strings.TrimSpace(baseURL) == "" || strings.TrimSpace(token) == "" {
		return nil
	}
	return func(runID string) []mailbox.Mailbox {
		box, err := mailboxnebula.New(mailboxnebula.Config{
			OwnerSlackWebhook: os.Getenv("ORCA_OWNER_ASK_SLACK_WEBHOOK"),
			RunID:             runID,
			Events:            st.Events(),
			BaseURL:           baseURL,
			Token:             token,
			Host:              true,
		})
		if err != nil {
			slog.Warn("unable to open adopted run mailbox", "run", runID, "error", err)
			return nil
		}
		return []mailbox.Mailbox{box}
	}
}

func loopVerifyInterval() time.Duration {
	if raw := strings.TrimSpace(os.Getenv("ORCA_LOOP_VERIFY_INTERVAL")); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			return d
		}
	}
	return 0
}

func adoptedPRProbe(h hostSwarms, st store.Store) controller.PRProbe {
	if h.probe.PR == nil {
		return nil
	}
	return func(ctx context.Context, runID, branch string, pr int) (controller.PRStatus, error) {
		target := strings.TrimSpace(branch)
		if pr > 0 {
			target = strconv.Itoa(pr)
		}
		if target == "" {
			return controller.PRStatus{}, nil
		}
		var repo string
		if st != nil && runID != "" {
			if run, err := st.Runs().Get(ctx, runID); err == nil && run != nil {
				repo = run.Repository
			}
		}
		var runDir string
		if runID != "" {
			runDir, _ = h.importer.Find(runID)
		}
		if repo == "" && runDir != "" {
			if hr, err := adopt.Read(runDir); err == nil {
				repo = hr.Repo
			}
		}
		res, err := h.probe.PR(ctx, repo, runDir, target)
		if err != nil {
			return controller.PRStatus{}, err
		}
		return controller.PRStatus{
			State:    res.State,
			MergeSHA: res.MergeSHA,
			Number:   cmp.Or(res.Number, max(pr, 0)),
		}, nil
	}
}

func newAdoptedPlanner(cfg Config, h hostSwarms, st store.Store, clk clock.Clock, client model.ModelClient, logger *slog.Logger, backend model.AgentBackend, runID string) (*Planner, model.InvestigateConfig) {
	var runDir string
	if runID != "" {
		runDir, _ = h.importer.Find(runID)
	}
	investigate := model.InvestigateConfig{
		RunDir:  runDir,
		RepoDir: model.ResolveRunRepoDir(runDir, ""),
	}
	planner := NewPlanner(st, clk, roleClient(client, cfg, "planner"), cfg.Models.Planner, logger)
	planner.critic = roleClient(client, cfg, "critic")
	planner.criticModel = cfg.Models.Roles["critic"].Model
	planner.providerName = cfg.Models.Roles["planner"].Provider
	planner.criticProvider = cfg.Models.Roles["critic"].Provider
	planner.effort = cfg.Models.Roles["planner"].Effort
	planner.agentBackend = backend
	planner.llmGatewayURL = cfg.LLMGatewayURL
	planner.investigate = investigate
	planner.claudeBin = cfg.ClaudeBin
	planner.codexBin = cfg.CodexBin
	return planner, investigate
}

// adoptedDispatchers gives each adopted run the dispatcher and planner `orca run`
// uses, so owner instructions, escalations, and backlog-drained replans on that
// run share the same per-run Planner and investigation roots.
func adoptedDispatchers(
	ctx context.Context,
	cfg Config,
	h hostSwarms,
	st store.Store,
	lanes *nebula.Driver,
	clk clock.Clock,
	client model.ModelClient,
	logger *slog.Logger,
	prProbe controller.PRProbe,
) (adopt.JudgeFactory, func(context.Context, string, string, string) error) {
	backend := effectiveAgentBackend(cfg, client)
	if client == nil && backend != model.AgentBackendCLI {
		return nil, nil
	}
	var (
		mu       sync.Mutex
		planners = make(map[string]*Planner)
	)
	if ctx != nil {
		context.AfterFunc(ctx, func() {
			mu.Lock()
			active := planners
			planners = make(map[string]*Planner)
			mu.Unlock()
			for _, p := range active {
				if p != nil {
					_ = p.Close()
				}
			}
		})
	}
	judges := func(runID string, answer func(context.Context, string, string, string) error) controller.Judge {
		planner, investigate := newAdoptedPlanner(cfg, h, st, clk, client, logger, backend, runID)
		mu.Lock()
		prev := planners[runID]
		planners[runID] = planner
		mu.Unlock()
		if prev != nil {
			_ = prev.Close()
		}

		disp := dispatcher.New(dispatcher.Config{
			AgentBackend:     backend,
			Investigate:      investigate,
			Model:            cfg.Models.Dispatcher,
			Provider:         cfg.Models.Roles["dispatcher"].Provider,
			Effort:           cfg.Models.Roles["dispatcher"].Effort,
			LLMGatewayURL:    cfg.LLMGatewayURL,
			ClaudeBin:        cfg.ClaudeBin,
			CodexBin:         cfg.CodexBin,
			HardBatchModel:   cfg.Models.DispatcherHard,
			AllowedModels:    cfg.AllowedModels,
			RecipeCompiler:   planner,
			OnPlannerRequest: planner.HandlePlannerRequest,
			Logger:           logger,
			PRProbe:          prProbe,
		}, st, lanes, clk, roleClient(client, cfg, "dispatcher"))
		disp.SetOwnerAnswer(answer)
		return &adoptedJudge{
			Dispatcher: disp,
			planner:    planner,
			onClose: func() {
				mu.Lock()
				if planners[runID] == planner {
					delete(planners, runID)
				}
				mu.Unlock()
			},
		}
	}
	plannerFn := func(pCtx context.Context, runID, reason, planRef string) error {
		mu.Lock()
		planner := planners[runID]
		mu.Unlock()
		if planner != nil {
			return planner.HandlePlannerRequest(pCtx, runID, reason, planRef)
		}
		tmpPlanner, _ := newAdoptedPlanner(cfg, h, st, clk, client, logger, backend, runID)
		defer func() { _ = tmpPlanner.Close() }()
		return tmpPlanner.HandlePlannerRequest(pCtx, runID, reason, planRef)
	}
	return judges, plannerFn
}

type adoptedJudge struct {
	*dispatcher.Dispatcher
	planner *Planner
	onClose func()
}

func (j *adoptedJudge) Close() error {
	if j.onClose != nil {
		j.onClose()
	}
	var err error
	if j.Dispatcher != nil {
		err = j.Dispatcher.Close()
	}
	if j.planner != nil {
		if pErr := j.planner.Close(); pErr != nil && err == nil {
			err = pErr
		}
	}
	return err
}
