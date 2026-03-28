// Command symphony runs the Symphony orchestrator service.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/bazelment/yoloswe/symphony/config"
	symphttp "github.com/bazelment/yoloswe/symphony/http"
	"github.com/bazelment/yoloswe/symphony/logging"
	"github.com/bazelment/yoloswe/symphony/orchestrator"
	"github.com/bazelment/yoloswe/symphony/tracker"
)

func main() {
	port := flag.Int("port", 0, "HTTP server port (0 = disabled unless set in config)")
	flag.Parse()

	// Positional arg: workflow path.
	workflowPath := "./WORKFLOW.md"
	if flag.NArg() > 0 {
		workflowPath = flag.Arg(0)
	}

	logger := logging.NewLogger()

	// Load config with hot-reload watcher.
	reloader, err := config.NewReloader(workflowPath, logger)
	if err != nil {
		logger.Error("failed to load workflow", "path", workflowPath, "error", err)
		os.Exit(1)
	}
	defer reloader.Close()

	cfg := reloader.Config()

	// Create tracker.
	t, err := tracker.New(cfg.TrackerKind, cfg.TrackerEndpoint, cfg.TrackerAPIKey)
	if err != nil {
		logger.Error("failed to create tracker", "error", err)
		os.Exit(1)
	}

	// Create orchestrator.
	orch := orchestrator.New(reloader.Config, t, orchestrator.RealClock{}, logger)

	// Determine effective port: CLI --port overrides config server.port.
	effectivePort := 0
	hasPort := false
	if *port != 0 {
		effectivePort = *port
		hasPort = true
	} else if cfg.ServerPort != nil {
		effectivePort = *cfg.ServerPort
		hasPort = true
	}

	// Start HTTP server if configured.
	var httpSrv *symphttp.Server
	if hasPort {
		httpSrv = symphttp.NewServer(orch, effectivePort, logger)
		if err := httpSrv.Start(); err != nil {
			logger.Error("failed to start http server", "error", err)
			os.Exit(1)
		}
		logger.Info("http server started", "addr", httpSrv.Addr())
	}

	// Set up signal handling.
	ctx, cancel := context.WithCancel(context.Background())
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigCh
		logger.Info("received signal, shutting down", "signal", sig)
		cancel()
	}()

	// Run orchestrator (blocks until context is cancelled).
	if err := orch.Run(ctx); err != nil {
		logger.Error("orchestrator failed", "error", err)
		if httpSrv != nil {
			httpSrv.Shutdown(context.Background())
		}
		os.Exit(1)
	}

	// Graceful shutdown.
	if httpSrv != nil {
		if err := httpSrv.Shutdown(context.Background()); err != nil {
			logger.Error("http server shutdown error", "error", err)
		}
	}

	fmt.Fprintln(os.Stderr, "symphony stopped")
}
