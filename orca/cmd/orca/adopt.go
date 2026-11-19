package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/orca/adopt"
	chatui "github.com/bazelment/yoloswe/orca/chat"
	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/controller"
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
	return newHostSwarmsWithRecorder(cfg, st, clk, nil)
}

func newHostSwarmsWithRecorder(cfg Config, st store.Store, clk clock.Clock, recorder *adopt.ActionRecorder) hostSwarms {
	var h hostSwarms
	// Each run's watch_lanes.sh resolves from its own skill directory;
	// ORCA_WATCH_LANES overrides it for every run.
	h.watcher = adopt.FormatWatcher{
		Subagent:      adopt.ScriptWatcher{Script: os.Getenv("ORCA_WATCH_LANES")},
		NebulaMissing: "swarm_nebula.sh not found (install nebula-swarm or set ORCA_NEBULA_SWARM_SH)",
		Panes:         adopt.TmuxPanes,
	}
	// The step loop is found from /proc even when swarm_nebula.sh is not
	// installed here; only restarting it on release needs the script.
	h.watcher.Nebula = adopt.StepLoopWatcher{Script: adopt.FindSwarmScript()}
	h.importer.Loops = h.watcher
	driverCfg := nebula.Config{APIURL: cfg.NebulaAPIURL, Token: cfg.NebulaToken, Events: driverEventStore{st.Events()}, Clock: clk}
	if recorder != nil {
		driverCfg.HTTPClient = adopt.ShadowHTTPClient(nil, recorder)
	}
	client, err := nebula.New(driverCfg)
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

func executeShadowAdopt(ctx context.Context, cfg Config, runDir, out string, logger *slog.Logger) (retErr error) {
	if strings.TrimSpace(runDir) == "" {
		return errors.New("orca adopt --shadow requires --dir <run>")
	}
	if strings.TrimSpace(out) == "" {
		return errors.New("orca adopt --shadow requires --out <dir>")
	}
	if err := validateShadowPaths(runDir, out); err != nil {
		return err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return err
	}
	workspace := filepath.Join(out, "session-workspace")
	if err := adopt.SyncWorkspace(runDir, workspace); err != nil {
		return fmt.Errorf("prepare shadow session workspace: %w", err)
	}
	cfg.SessionWorkDir = workspace
	if _, err := os.Stat(filepath.Join(out, "store.db")); err == nil {
		return fmt.Errorf("shadow output %q already contains store.db; choose a fresh output directory", out)
	} else if !os.IsNotExist(err) {
		return err
	}
	clk := clock.NewRealClock()
	st, err := store.OpenSQLite(filepath.Join(out, "store.db"), clk)
	if err != nil {
		return err
	}
	defer st.Close()
	recorder, err := adopt.NewActionRecorder(out)
	if err != nil {
		return err
	}
	defer func() {
		if err := recorder.Close(); retErr == nil && err != nil {
			retErr = err
		}
	}()
	run, err := adopt.Read(runDir)
	if err != nil {
		return err
	}
	h := newHostSwarmsWithRecorder(cfg, st, clk, recorder)
	if h.client == nil {
		return errors.New("orca adopt --shadow: Nebula client is required (set NEBULA_API_URL and NEBULA_TOKEN)")
	}
	client, err := openModelClient(cfg, clk)
	if err != nil {
		return err
	}
	ctrls := adopt.NewControllers(ctx, st, h.client, clk, adoptedDispatchers(cfg, st, client))
	ctrls.Shadow = true
	ctrls.DecideAsOperator = cfg.DecideAsOperator
	ctrls.Recorder = recorder
	ctrls.Mailboxes = adoptedMailboxesWithRecorder(cfg, st, recorder)
	ctrls.MaxCostPerHourUSD = cfg.MaxCostPerHourUSD
	ctrls.SubscriptionActors = subscriptionActors(effectiveAgentBackend(cfg, client))
	started := clk.Now().UTC()
	if err := ctrls.Start(ctx, run); err != nil {
		return err
	}
	shadowCtx := ctx
	var cancel context.CancelFunc
	if cfg.ShadowFor > 0 {
		shadowCtx, cancel = context.WithTimeout(ctx, cfg.ShadowFor)
		defer cancel()
	}
	<-shadowCtx.Done()
	_ = ctrls.Stop(context.WithoutCancel(ctx), run)
	ended := clk.Now().UTC()
	if err := recorder.Err(); err != nil {
		return err
	}
	return adopt.WriteShadowReportsWithOptions(context.WithoutCancel(ctx), out, runDir, run.ID, st, started, ended, cfg.DecideAsOperator)
}

func validateShadowPaths(runDir, out string) error {
	runAbs, err := filepath.Abs(runDir)
	if err != nil {
		return fmt.Errorf("resolve shadow run directory: %w", err)
	}
	outAbs, err := filepath.Abs(out)
	if err != nil {
		return fmt.Errorf("resolve shadow output directory: %w", err)
	}
	runAbs = resolveShadowPath(runAbs)
	outAbs = resolveShadowPath(outAbs)
	inside := func(parent, child string) bool {
		rel, relErr := filepath.Rel(parent, child)
		return relErr == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	if runAbs == outAbs || inside(runAbs, outAbs) || inside(outAbs, runAbs) {
		return fmt.Errorf("shadow output %q overlaps run directory %q", out, runDir)
	}
	return nil
}

func resolveShadowPath(path string) string {
	path = filepath.Clean(path)
	var suffix []string
	for {
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return filepath.Clean(resolved)
		}
		parent := filepath.Dir(path)
		if parent == path {
			return filepath.Clean(path)
		}
		suffix = append(suffix, filepath.Base(path))
		path = parent
	}
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
	ctrls := adopt.NewControllers(ctx, st, h.client, clk, adoptedDispatchers(cfg, st, client))
	ctrls.Mailboxes = adoptedMailboxes(cfg, st)
	ctrls.MaxCostPerHourUSD = cfg.MaxCostPerHourUSD
	ctrls.SubscriptionActors = subscriptionActors(effectiveAgentBackend(cfg, client))
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
	return adoptedMailboxesWithRecorder(cfg, st, nil)
}

func adoptedMailboxesWithRecorder(cfg Config, st store.Store, recorder *adopt.ActionRecorder) func(runID string) []mailbox.Mailbox {
	baseURL := cmp.Or(cfg.NebulaAPIURL, os.Getenv("NEBULA_API_URL"))
	token := cmp.Or(cfg.NebulaToken, os.Getenv("NEBULA_TOKEN"))
	if strings.TrimSpace(baseURL) == "" || strings.TrimSpace(token) == "" {
		return nil
	}
	return func(runID string) []mailbox.Mailbox {
		boxCfg := mailboxnebula.Config{
			OwnerSlackWebhook: os.Getenv("ORCA_OWNER_ASK_SLACK_WEBHOOK"),
			RunID:             runID,
			Events:            st.Events(),
			BaseURL:           baseURL,
			Token:             token,
			Host:              true,
		}
		if recorder != nil {
			boxCfg.Client = adopt.ShadowHTTPClient(nil, recorder)
		}
		box, err := mailboxnebula.New(boxCfg)
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

// adoptedDispatchers gives each adopted run a persistent Claude CLI session
// that acts through the orca toolbox, the way the host orchestrator does.
func adoptedDispatchers(cfg Config, st store.Store, client model.ModelClient) adopt.JudgeFactory {
	// Replay and unconfigured embedded callers are offline: no live CLI.
	if cfg.ReplayPath != "" || (cfg.AgentBackend == "" && client == nil && cfg.ClaudeBin == "") {
		return nil
	}
	// Unset, the dispatcher runs on the host's model (adopt.HostModel).
	dispatcherModel := ""
	if cfg.dispatcherModelSet {
		dispatcherModel = cfg.Models.Dispatcher
	}
	return func(string) controller.Judge {
		return &adopt.ShellDispatcher{Store: st, Config: model.AgentSessionConfig{
			Provider: "claude", Model: dispatcherModel, Effort: cfg.Models.Roles["dispatcher"].Effort,
			ClaudeBin: cfg.ClaudeBin, LLMGatewayURL: cfg.LLMGatewayURL,
			WorkDir: cfg.SessionWorkDir, AllowBuiltins: true,
		}, Shadow: cfg.Shadow, IntentDir: cfg.AdoptOut}
	}
}
