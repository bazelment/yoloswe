// Command symphony runs the Symphony orchestrator service.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/cliapp"
	"github.com/bazelment/yoloswe/symphony/config"
	symphttp "github.com/bazelment/yoloswe/symphony/http"
	"github.com/bazelment/yoloswe/symphony/orchestrator"
	"github.com/bazelment/yoloswe/symphony/tracker"
)

func main() {
	var port int

	rootOpts := cliapp.Options{ToolName: "symphony"}

	rootCmd := &cobra.Command{
		Use:   "symphony [WORKFLOW_PATH]",
		Short: "Run the Symphony orchestrator service",
		Long:  "Symphony loads a workflow definition (default ./WORKFLOW.md), starts the orchestrator, and optionally exposes an HTTP API.",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			workflowPath := "./WORKFLOW.md"
			if len(args) > 0 {
				workflowPath = args[0]
			}
			app := cliapp.FromContext(cmd.Context())
			return run(cmd.Context(), app, workflowPath, port)
		},
	}

	cliapp.RegisterStandardFlags(rootCmd, &rootOpts)
	rootCmd.Flags().IntVar(&port, "port", 0, "HTTP server port (0 = disabled unless set in config)")

	os.Exit(cliapp.Run(rootOpts, func(ctx context.Context, app *cliapp.App) error {
		return rootCmd.ExecuteContext(cliapp.WithApp(ctx, app))
	}))
}

func run(ctx context.Context, app *cliapp.App, workflowPath string, port int) error {
	logger := app.Logger

	// Load config with hot-reload watcher.
	reloader, err := config.NewReloader(workflowPath, logger)
	if err != nil {
		return fmt.Errorf("load workflow %q: %w", workflowPath, err)
	}
	defer reloader.Close()

	cfg := reloader.Config()

	// Create tracker.
	t, err := tracker.New(cfg.TrackerKind, cfg.TrackerEndpoint, cfg.TrackerAPIKey)
	if err != nil {
		return fmt.Errorf("create tracker: %w", err)
	}

	// Create orchestrator.
	orch := orchestrator.New(reloader.Config, t, orchestrator.RealClock{}, logger)

	// Determine effective port: CLI --port overrides config server.port.
	effectivePort := 0
	hasPort := false
	if port != 0 {
		effectivePort = port
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
			return fmt.Errorf("start http server: %w", err)
		}
		logger.Info("http server started", "addr", httpSrv.Addr())
	}

	// Run orchestrator (blocks until ctx is cancelled).
	runErr := orch.Run(ctx)

	// Graceful HTTP shutdown regardless of orchestrator outcome.
	if httpSrv != nil {
		if err := httpSrv.Shutdown(context.Background()); err != nil {
			logger.Error("http server shutdown error", "error", err)
		}
	}

	if runErr != nil {
		return fmt.Errorf("orchestrator failed: %w", runErr)
	}

	fmt.Fprintln(os.Stderr, "symphony stopped")
	return nil
}
