package main

import (
	"context"

	"github.com/bazelment/yoloswe/orca/adopt"
	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/controller"
	"github.com/bazelment/yoloswe/orca/dispatcher"
	"github.com/bazelment/yoloswe/orca/lane/nebula"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/store"
)

// newAdoption wires /adopt and /release to the real adapters: the Nebula API
// client for the run label and watch_lanes.sh plus OS signals for the host
// watcher. Each adopted run gets its own dispatcher on client. When an
// adapter cannot be built it returns the one-line reason chat shows instead.
func newAdoption(ctx context.Context, cfg Config, st store.Store, clk clock.Clock, client model.ModelClient) (*adopt.Service, string) {
	lanes, err := nebula.New(nebula.Config{APIURL: cfg.NebulaAPIURL, Token: cfg.NebulaToken, Events: driverEventStore{st.Events()}, Clock: clk})
	if err != nil {
		return nil, "Nebula API client: " + err.Error() + " (set NEBULA_API_URL and NEBULA_TOKEN)"
	}
	script := adopt.FindWatchScript()
	if script == "" {
		return nil, "watch_lanes.sh not found (install subagent-swarm-nebula or set ORCA_WATCH_LANES)"
	}
	return &adopt.Service{
		Importer: adopt.Importer{},
		Labeler:  lanes,
		Watcher:  adopt.ScriptWatcher{Script: script},
		Holder:   adopt.ChatHolder(cfg.RunID),
		Driver:   adopt.NewControllers(ctx, st, lanes, clk, adoptedDispatchers(cfg, st, lanes, clk, client)),
	}, ""
}

// adoptedDispatchers gives each adopted run the dispatcher `orca run` uses,
// so owner instructions and escalations on that run are judged.
func adoptedDispatchers(cfg Config, st store.Store, lanes *nebula.Driver, clk clock.Clock, client model.ModelClient) adopt.JudgeFactory {
	if client == nil {
		return nil
	}
	return func(_ string, answer func(context.Context, string, string, string) error) controller.Judge {
		disp := dispatcher.New(dispatcher.Config{
			Model:          cfg.Models.Dispatcher,
			Provider:       cfg.Models.Roles["dispatcher"].Provider,
			PromptVersion:  dispatcher.SystemPromptVersion,
			HardBatchModel: cfg.Models.DispatcherHard,
		}, st, lanes, clk, roleClient(client, cfg, "dispatcher"))
		disp.SetOwnerAnswer(answer)
		return disp
	}
}
