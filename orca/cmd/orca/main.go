// Command orca runs the Nebula swarm orchestrator harness (`run`) and
// terminal concierge (`chat`).
package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"

	chatui "github.com/bazelment/yoloswe/orca/chat"
	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/mailbox"
	mailboxnebula "github.com/bazelment/yoloswe/orca/mailbox/nebula"
	"github.com/bazelment/yoloswe/orca/router"
)

func main() {
	ctx, cancel := notifySignalContext(context.Background(), func() {
		fmt.Fprintln(os.Stderr, "Received second signal, forcing exit")
		os.Exit(130)
	})
	defer cancel()

	if err := Execute(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, context.Canceled) {
			os.Exit(130)
		}
		fmt.Fprintf(os.Stderr, "orca: %v\n", err)
		os.Exit(1)
	}
}

// Execute parses CLI arguments and runs either `run` or `chat`.
func Execute(
	ctx context.Context,
	args []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
) error {
	if len(args) == 0 {
		printUsage(stderr)
		return ErrRunIDRequired
	}

	subcmd := "run"
	rest := args
	switch args[0] {
	case "run", "chat", "report", "export":
		subcmd = args[0]
		rest = args[1:]
	case "-h", "--help", "help":
		printUsage(stdout)
		return nil
	}

	cfg, posArgs, err := parseFlags(subcmd, rest, stderr)
	if err != nil {
		return err
	}

	logger := newLogger(stderr, cfg.LogLevel, cfg.LogFormat)

	switch subcmd {
	case "run":
		if err := cfg.Validate(true); err != nil {
			return err
		}
		return executeRun(ctx, cfg, logger)
	case "chat":
		if err := cfg.Validate(false); err != nil {
			return err
		}
		return executeChat(ctx, cfg, posArgs, stdin, stdout, logger)
	case "report":
		if err := cfg.Validate(false); err != nil {
			return err
		}
		st, err := openReportStore(cfg)
		if err != nil {
			return err
		}
		defer st.Close()
		day := cfg.ReportDay
		if day == "" && len(posArgs) > 0 {
			day = posArgs[0]
		}
		return reportRun(ctx, st, cfg.RunID, day, stdout)
	case "export":
		if err := cfg.Validate(false); err != nil {
			return err
		}
		st, err := openReportStore(cfg)
		if err != nil {
			return err
		}
		defer st.Close()
		return exportRun(ctx, st, cfg.RunID, stdout)
	default:
		return fmt.Errorf("unknown subcommand %q", subcmd)
	}
}

func executeRun(ctx context.Context, cfg Config, logger *slog.Logger) error {
	driverKind := strings.ToLower(strings.TrimSpace(cfg.DriverKind))
	if strings.TrimSpace(cfg.Repository) == "" && strings.TrimSpace(cfg.Goal) != "" && (driverKind == "" || driverKind == "nebula") {
		return errors.New("orca: --repo is required when not running inside a GitHub checkout (could not infer origin)")
	}
	clk := clock.NewRealClock()
	st, resolvedStore, err := openStore(cfg, clk)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()
	cfg.StoreSpec = resolvedStore

	client, err := openModelClient(cfg, clk)
	if err != nil {
		return err
	}

	var drv lane.LaneDriver
	switch strings.ToLower(strings.TrimSpace(cfg.DriverKind)) {
	case "fake":
		var behaviors []FakePhaseBehavior
		if strings.TrimSpace(cfg.FakeScenarioPath) != "" {
			sc, scErr := LoadFakeScenario(cfg.FakeScenarioPath)
			if scErr != nil {
				return scErr
			}
			behaviors = sc.Behaviors
		}
		drv = NewScriptedFakeDriver(lane.NewFakeDriver(), st.Events(), clk, cfg.FakeAutoComplete, behaviors...)
	case "nebula", "":
		nebDrv, nebErr := openNebulaDriver(cfg, st, clk)
		if nebErr != nil {
			return nebErr
		}
		// A session may spawn lanes only into a run it created. Establish
		// ownership before constructing the runner, whose first step plans and
		// dispatches lanes.
		if err := nebDrv.EnsureRun(ctx, cfg.RunID); err != nil {
			return fmt.Errorf("orca: unable to create Nebula run %q: %w", cfg.RunID, err)
		}
		drv = nebDrv
	default:
		return fmt.Errorf("orca: unsupported --driver %q", cfg.DriverKind)
	}

	var boxes []mailbox.Mailbox
	if !strings.EqualFold(strings.TrimSpace(cfg.DriverKind), "fake") {
		box, err := mailboxnebula.New(mailboxnebula.Config{
			RunID: cfg.RunID, Events: st.Events(),
			BaseURL: cmp.Or(cfg.NebulaAPIURL, os.Getenv("NEBULA_API_URL")),
			Token:   cmp.Or(cfg.NebulaToken, os.Getenv("NEBULA_TOKEN")),
		})
		if err != nil {
			return err
		}
		boxes = append(boxes, box)
	}
	runner := NewRunner(cfg, st, drv, clk, client, logger, boxes...)
	return runner.Run(ctx)
}

func executeChat(
	ctx context.Context,
	cfg Config,
	posArgs []string,
	stdin io.Reader,
	stdout io.Writer,
	logger *slog.Logger,
) error {
	clk := clock.NewRealClock()
	st, resolvedStore, err := openStore(cfg, clk)
	if err != nil {
		return err
	}
	defer func() { _ = st.Close() }()

	client, err := openModelClient(cfg, clk)
	if err != nil {
		return err
	}

	logger.Info("orca chat starting",
		"run_id", cfg.RunID,
		"store", resolvedStore,
		"model", cfg.Models.Concierge,
	)

	concierge := NewConcierge(cfg.RunID, st, clk, roleClient(client, cfg, "concierge"), cfg.Models.Concierge)
	defer func() { _ = concierge.Close(context.WithoutCancel(ctx)) }()

	question := strings.TrimSpace(cfg.Prompt)
	if question == "" && len(posArgs) > 0 {
		question = strings.TrimSpace(strings.Join(posArgs, " "))
	}
	routerSel := cfg.Models.Roles["router"]
	routingLog := router.StoreLogger{Store: st, RunID: cfg.RunID, Model: routerSel.Model, Provider: routerSel.Provider}
	r := router.Select(roleClient(client, cfg, "router"), routerSel.Model, os.Getenv("ORCA_GEMINI_API_KEY"), os.Getenv("GEMINI_DECISIONS_BASE_URL"), routingLog)
	backend := &chatui.StoreBackend{Store: st, RunID: cfg.RunID, Stream: concierge.AskStream}
	if question != "" {
		d, err := r.Route(ctx, question)
		if err != nil {
			return err
		}
		echo := "→ " + string(d.Handler) + ": " + question
		if d.Fallback {
			echo += " (via fallback)"
		}
		if _, err := fmt.Fprintln(stdout, echo); err != nil {
			return err
		}
		var reply strings.Builder
		if err := backend.Dispatch(ctx, d, question, func(s string) { reply.WriteString(s) }); err != nil {
			return err
		}
		_, err = fmt.Fprintln(stdout, reply.String())
		return err
	}
	ui := chatui.New(ctx, cfg.RunID, concierge.Ask, r)
	ui.Backend = backend
	ui.Dispatch = backend.Dispatch
	return chatui.Run(ctx, stdin, stdout, ui)
}

func newLogger(out io.Writer, levelStr, format string) *slog.Logger {
	var level slog.Level
	switch strings.ToLower(strings.TrimSpace(levelStr)) {
	case "debug":
		level = slog.LevelDebug
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	default:
		level = slog.LevelInfo
	}
	opts := &slog.HandlerOptions{Level: level}
	if strings.EqualFold(strings.TrimSpace(format), "text") {
		return slog.New(slog.NewTextHandler(out, opts))
	}
	return slog.New(slog.NewJSONHandler(out, opts))
}

func notifySignalContext(parent context.Context, forceExit func()) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	done := make(chan struct{})
	var closeOnce sync.Once
	wrappedCancel := func() {
		closeOnce.Do(func() { close(done) })
		cancel()
	}

	go func() {
		defer signal.Stop(sigCh)
		select {
		case <-done:
			return
		case <-parent.Done():
			return
		case <-sigCh:
			cancel()
		}
		select {
		case <-done:
			return
		case <-sigCh:
			if forceExit != nil {
				forceExit()
			}
		}
	}()

	return ctx, wrappedCancel
}

func printUsage(w io.Writer) {
	_, _ = fmt.Fprintln(w, `Usage:
  orca run --goal "<text>" [--standing] [--repo <owner/name>] [flags]
  orca chat --run <id> [--store sqlite:<path>|nebula] [--prompt "<question>"] [flags]`)
	_, _ = fmt.Fprintln(w, "  orca report --run <id> [--day YYYY-MM-DD] [flags]\n  orca export --run <id> [flags]")
}
