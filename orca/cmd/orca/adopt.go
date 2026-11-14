package main

import (
	"github.com/bazelment/yoloswe/orca/adopt"
	"github.com/bazelment/yoloswe/orca/lane/nebula"
)

// newAdoption wires /adopt and /release to the real adapters: the Nebula API
// client for the run label and watch_lanes.sh plus OS signals for the host
// watcher. When an adapter cannot be built it returns the one-line reason
// chat shows instead.
func newAdoption(cfg Config) (*adopt.Service, string) {
	client, err := nebula.New(nebula.Config{APIURL: cfg.NebulaAPIURL, Token: cfg.NebulaToken})
	if err != nil {
		return nil, "Nebula API client: " + err.Error() + " (set NEBULA_API_URL and NEBULA_TOKEN)"
	}
	script := adopt.FindWatchScript()
	if script == "" {
		return nil, "watch_lanes.sh not found (install subagent-swarm-nebula or set ORCA_WATCH_LANES)"
	}
	return &adopt.Service{
		Importer: adopt.Importer{},
		Labeler:  client,
		Watcher:  adopt.ScriptWatcher{Script: script},
		Holder:   adopt.ChatHolder(cfg.RunID),
	}, ""
}
