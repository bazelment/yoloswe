package main

import (
	"context"
	"log/slog"

	"github.com/bazelment/yoloswe/orca/adopt"
	chatui "github.com/bazelment/yoloswe/orca/chat"
	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/dispatcher"
	"github.com/bazelment/yoloswe/orca/lane/nebula"
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
// instead.
func newAdoption(ctx context.Context, cfg Config, h hostSwarms, st store.Store, clk clock.Clock, client model.ModelClient, logger *slog.Logger) (*adopt.Service, string) {
	if h.client == nil {
		return nil, h.off
	}
	return &adopt.Service{
		Importer: h.importer,
		Labeler:  h.client,
		Watcher:  h.watcher,
		Holder:   adopt.ChatHolder(cfg.RunID),
		Driver:   adopt.NewControllers(ctx, st, h.client, clk, adoptedDispatchers(cfg, st, h.client, clk, client, logger)),
	}, ""
}

// adoptedDispatchers gives each adopted run the dispatcher `orca run` uses,
// so owner instructions and escalations on that run are judged.
func adoptedDispatchers(cfg Config, st store.Store, lanes *nebula.Driver, clk clock.Clock, client model.ModelClient, logger *slog.Logger) adopt.JudgeFactory {
	if client == nil {
		return nil
	}
	return func(_ string, answer func(context.Context, string, string, string) error) controller.Judge {
		disp := dispatcher.New(dispatcher.Config{
			Model:          cfg.Models.Dispatcher,
			Provider:       cfg.Models.Roles["dispatcher"].Provider,
			PromptVersion:  dispatcher.SystemPromptVersion,
			HardBatchModel: cfg.Models.DispatcherHard,
			Logger:         logger,
		}, st, lanes, clk, roleClient(client, cfg, "dispatcher"))
		disp.SetOwnerAnswer(answer)
		return disp
	}
}
