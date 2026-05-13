package main

import (
	"testing"

	"github.com/bazelment/yoloswe/symphony/config"
)

func TestSelectHTTPPort(t *testing.T) {
	t.Parallel()

	cfgPort := 8080

	requireHTTPPort(t, 0, &config.ServiceConfig{}, 0, false)
	requireHTTPPort(t, 0, &config.ServiceConfig{ServerPort: &cfgPort}, cfgPort, true)
	requireHTTPPort(t, 9090, &config.ServiceConfig{ServerPort: &cfgPort}, 9090, true)
	requireHTTPPort(t, 7070, &config.ServiceConfig{}, 7070, true)
}

func requireHTTPPort(t *testing.T, cliPort int, cfg *config.ServiceConfig, want int, wantOK bool) {
	t.Helper()

	got, gotOK := selectHTTPPort(cliPort, cfg)
	if got != want || gotOK != wantOK {
		t.Fatalf("selectHTTPPort(%d, cfg) = (%d, %v), want (%d, %v)",
			cliPort, got, gotOK, want, wantOK)
	}
}
