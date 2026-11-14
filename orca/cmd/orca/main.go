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
	"os/exec"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	chatui "github.com/bazelment/yoloswe/orca/chat"
	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/mailbox"
	mailboxnebula "github.com/bazelment/yoloswe/orca/mailbox/nebula"
	"github.com/bazelment/yoloswe/orca/router"
	"github.com/bazelment/yoloswe/orca/store"
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
	// Only a bare `orca` opens chat; flags without a subcommand keep meaning
	// `orca run`.
	subcmd := "run"
	rest := args
	if len(args) == 0 {
		subcmd = "chat"
	} else {
		switch args[0] {
		case "run", "chat", "report", "export":
			subcmd = args[0]
			rest = args[1:]
		case "-h", "--help", "help":
			printUsage(stdout)
			return nil
		}
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
		// A chat without --run picks its run later, from its store.
		vcfg := cfg
		vcfg.RunID = cmp.Or(vcfg.RunID, frontDeskRunID)
		if err := vcfg.Validate(false); err != nil {
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
			Host:    strings.TrimSpace(os.Getenv("NEBULA_SESSION_ID")) == "",
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
	runs, err := openChatRuns(cfg, clk)
	if err != nil {
		return err
	}
	defer func() { _ = runs.Close() }()
	if strings.TrimSpace(cfg.RunID) == "" {
		if found, _ := runs.List(ctx); len(found) > 0 {
			cfg.RunID = found[0].ID
		}
	}
	frontDesk := strings.TrimSpace(cfg.RunID) == ""
	storeCfg := cfg
	if frontDesk {
		storeCfg.RunID = frontDeskRunID
	}
	st, resolvedStore, err := runs.Open(storeCfg)
	if err != nil {
		return err
	}
	// The backend owns the attached store from here on and closes it, and
	// every store it later switches to, by exit.
	backend := chatui.NewStoreBackend(st, storeCfg.RunID)
	defer func() { _ = backend.Close() }()
	if frontDesk {
		backend.FrontDesk = frontDeskRunID
		if err := st.Runs().Create(ctx, store.Run{ID: storeCfg.RunID, Repository: cfg.Repository, Objective: ""}); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
			return err
		}
	} else if err := ensureChatRun(ctx, st, cfg); err != nil {
		return err
	}

	client, err := openModelClient(cfg, clk)
	if err != nil {
		return err
	}

	logger.Info("orca chat starting",
		"run_id", cfg.RunID,
		"store", resolvedStore,
		"model", cfg.Models.Concierge,
	)

	concierge := NewConcierge(storeCfg.RunID, st, clk, roleClient(client, cfg, "concierge"), cfg.Models.Concierge)
	defer func() { _ = concierge.Close(context.WithoutCancel(ctx)) }()

	question := strings.TrimSpace(cfg.Prompt)
	if question == "" && len(posArgs) > 0 {
		question = strings.TrimSpace(strings.Join(posArgs, " "))
	}
	backend.OnAttach = concierge.Attach
	adoption, adoptionOff := newAdoption(ctx, cfg, st, clk)
	backend.Adoption, backend.AdoptionUnavailable = adoption, adoptionOff
	if adoption != nil {
		defer func() {
			if closer, ok := adoption.Driver.(interface{ Close(context.Context) error }); ok {
				_ = closer.Close(context.WithoutCancel(ctx))
			}
		}()
		if question == "" {
			if ids, err := adoption.Adopted(); err == nil {
				for _, id := range ids {
					_, _ = adoption.Adopt(ctx, id)
				}
			}
		}
	}
	routerSel := cfg.Models.Roles["router"]
	routingLog := attachedRoutingLog{backend: backend, model: routerSel.Model, provider: routerSel.Provider}
	r := router.Select(roleClient(client, cfg, "router"), routerSel.Model, os.Getenv("ORCA_GEMINI_API_KEY"), os.Getenv("GEMINI_DECISIONS_BASE_URL"), routingLog)
	// run.start runs inside a concierge stream turn; the new run is attached
	// once that turn ends, so the turn finishes on the store it started on.
	var pendingRunID string
	var pendingRunStore store.Store
	backend.Stream = func(streamCtx context.Context, text string, emit func(string)) error {
		err := concierge.AskStream(context.WithValue(streamCtx, runStartEchoKey{}, emit), text, emit)
		if pendingRunStore != nil {
			runStore, runID := pendingRunStore, pendingRunID
			pendingRunStore, pendingRunID = nil, ""
			err = errors.Join(err, backend.Attach(runStore, runID))
		}
		return err
	}
	backend.ListRuns = runs.List
	backend.OpenRun = func(_ context.Context, runID string) (store.Store, error) {
		runCfg := cfg
		runCfg.RunID = runID
		st, _, err := runs.Open(runCfg)
		return st, err
	}
	concierge.RunStart = func(ctx context.Context, goal string) (string, error) {
		runID := nextUniqueID("run", clk)
		runStore, spec, err := runs.Create(ctx, runID, goal)
		if err != nil {
			return "", err
		}
		launchCfg := cfg
		launchCfg.StoreSpec = spec
		if err := runLauncher(ctx, launchCfg, runID, goal); err != nil {
			_ = runStore.Close()
			return "", err
		}
		if pendingRunStore != nil {
			_ = pendingRunStore.Close()
		}
		pendingRunID, pendingRunStore = runID, runStore
		// Echo through the turn's stream so the chat view renders it.
		if emit, ok := ctx.Value(runStartEchoKey{}).(func(string)); ok {
			emit(fmt.Sprintf("Starting run %s for: %s (say stop to cancel)\n", runID, goal))
		}
		return runID, nil
	}
	if question != "" {
		var d router.Decision
		if chatui.IsNewGoal(question) {
			d = router.Decision{Handler: router.Concierge, Confidence: 1, Backend: "goal-detection"}
		} else {
			d, err = r.Route(ctx, question)
			if err != nil {
				return err
			}
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
	ui := chatui.New(ctx, storeCfg.RunID, concierge.Ask, r)
	ui.Backend = backend
	ui.Dispatch = backend.Dispatch
	return chatui.Run(ctx, stdin, stdout, ui)
}

// runStartEchoKey carries the chat turn's emit func to run.start.
type runStartEchoKey struct{}

// attachedRoutingLog records routing decisions on the run chat is attached
// to when the decision is made.
type attachedRoutingLog struct {
	backend         *chatui.StoreBackend
	model, provider string
}

func (l attachedRoutingLog) LogRouting(ctx context.Context, text string, d router.Decision) error {
	return l.backend.WithRun(func(st store.Store, runID string) error {
		return router.StoreLogger{Store: st, RunID: runID, Model: l.model, Provider: l.provider}.LogRouting(ctx, text, d)
	})
}

func launchDetachedRun(ctx context.Context, cfg Config, runID, goal string) error {
	cmd, logPath, err := detachedRunCommand(cfg, runID, goal)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return err
	}
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	cmd.Stdout, cmd.Stderr = logFile, logFile
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return err
	}
	go func() { _ = cmd.Wait(); _ = logFile.Close() }()
	return nil
}

// detachedRunCommand builds the `orca run` controller for a run started from
// chat, on the run's store cfg.StoreSpec ("sqlite:<absolute path>"). The
// controller inherits the chat's configuration flags and runs in its own
// session, so the chat's terminal signals do not reach it. It keeps the chat's
// working directory, so relative paths in inherited flags (--config,
// --store-dir, ...) still resolve. Its log sits next to the store: orca.log in
// a per-run store's directory, orca-<run>.log beside a shared --store.
func detachedRunCommand(cfg Config, runID, goal string) (cmd *exec.Cmd, logPath string, err error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, "", err
	}
	kind, path, err := parseStoreSpec(cfg.StoreSpec, cfg.StoreBaseDir, runID)
	if err != nil {
		return nil, "", err
	}
	if kind != "sqlite" {
		return nil, "", fmt.Errorf("orca: a launched controller needs a sqlite store, not %q", cfg.StoreSpec)
	}
	if path, err = filepath.Abs(path); err != nil {
		return nil, "", err
	}
	args := append([]string{"run"}, cfg.launchArgs...)
	args = append(args, "--run", runID, "--goal", goal, "--store", "sqlite:"+path, "--driver", cfg.DriverKind)
	if cfg.Repository != "" {
		args = append(args, "--repo", cfg.Repository)
	}
	cmd = exec.Command(exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	logPath = filepath.Join(filepath.Dir(path), "orca.log")
	if perRun, err := store.RunDBPath(chatStoreBaseDir(cfg), runID); err != nil || !samePath(perRun, path) {
		logPath = filepath.Join(filepath.Dir(path), "orca-"+runID+".log")
	}
	return cmd, logPath, nil
}

func samePath(a, b string) bool {
	a, errA := filepath.Abs(a)
	b, errB := filepath.Abs(b)
	return errA == nil && errB == nil && a == b
}

// runLauncher is injectable for deterministic tests.
var runLauncher = launchDetachedRun

func chatStoreBaseDir(cfg Config) string {
	if strings.TrimSpace(cfg.StoreBaseDir) != "" {
		return cfg.StoreBaseDir
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".", ".swarm")
	}
	return filepath.Join(home, ".swarm")
}

// frontDeskRunID is the run a chat without a run attaches to. It is a
// conversation, not a run: discovery never offers it and no controller runs it.
const frontDeskRunID = "front-desk"

// chatRuns is where chat finds, opens and creates runs. An explicit --store
// (sqlite:<path> or memory) holds every run chat picks, switches to, starts or
// keeps its front desk in; otherwise each run has its own SQLite store under
// the store dir and the front desk lives in memory.
//
//nolint:govet // field order follows the doc
type chatRuns struct {
	cfg Config
	clk clock.Clock
	// shared is the explicit --store, opened once for the whole chat; nil
	// for per-run stores. spec is its resolved form ("sqlite:<abs>", "memory").
	shared *sharedStore
	spec   string
}

// sharedStore is the explicit store as handed to the chat backend: every run
// attaches the same value, so switching runs never closes it. chatRuns.Close
// closes the underlying store.
type sharedStore struct{ store.Store }

func (*sharedStore) Close() error { return nil }

func openChatRuns(cfg Config, clk clock.Clock) (*chatRuns, error) {
	runs := &chatRuns{cfg: cfg, clk: clk}
	kind, path, err := parseStoreSpec(cfg.StoreSpec, cfg.StoreBaseDir, frontDeskRunID)
	if err != nil {
		return nil, err
	}
	spec := strings.ToLower(strings.TrimSpace(cfg.StoreSpec))
	switch {
	case kind == "memory":
		runs.shared, runs.spec = &sharedStore{store.NewMemoryStore(clk)}, "memory"
	case strings.HasPrefix(spec, "sqlite:"):
		abs, err := filepath.Abs(path)
		if err != nil {
			return nil, err
		}
		st, err := store.OpenSQLite(abs, clk)
		if err != nil {
			return nil, err
		}
		runs.shared, runs.spec = &sharedStore{st}, "sqlite:"+abs
	}
	return runs, nil
}

// Close closes the explicit store.
func (r *chatRuns) Close() error {
	if r.shared == nil {
		return nil
	}
	return r.shared.Store.Close()
}

// Open opens the store holding cfg.RunID: the explicit store, a memory store
// for the front desk, or the run's own SQLite store.
func (r *chatRuns) Open(cfg Config) (store.Store, string, error) {
	if r.shared != nil {
		return r.shared, r.spec, nil
	}
	if cfg.RunID == frontDeskRunID {
		cfg.StoreSpec = "memory"
	}
	return openStore(cfg, r.clk)
}

// Create records a new run for goal and returns its store and spec, for the
// controller chat launches on it.
func (r *chatRuns) Create(ctx context.Context, runID, goal string) (store.Store, string, error) {
	run := store.Run{ID: runID, Repository: r.cfg.Repository, Objective: goal}
	if r.shared != nil {
		if r.spec == "memory" {
			return nil, "", errors.New("orca: cannot start a run in --store memory: a controller cannot share this chat's memory store")
		}
		if err := r.shared.Runs().Create(ctx, run); err != nil {
			return nil, "", err
		}
		return r.shared, r.spec, nil
	}
	path, err := store.RunDBPath(chatStoreBaseDir(r.cfg), runID)
	if err != nil {
		return nil, "", err
	}
	st, err := store.OpenSQLite(path, r.clk)
	if err != nil {
		return nil, "", err
	}
	if err := st.Runs().Create(ctx, run); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		_ = st.Close()
		return nil, "", err
	}
	return st, "sqlite:" + path, nil
}

// List returns the runs chat may attach to, live controllers first, then most
// recently updated. With an explicit store these are its runs (of the
// inferred repository, when there is one). Otherwise they are the per-run
// stores of the inferred repository; without one there is no safe way to
// decide which stored run belongs to this conversation, so none.
func (r *chatRuns) List(ctx context.Context) ([]store.Run, error) {
	repo := strings.TrimSpace(r.cfg.Repository)
	var runs []store.Run
	if r.shared != nil {
		all, err := r.shared.Runs().List(ctx)
		if err != nil {
			return nil, err
		}
		for i := range all {
			if all[i].ID != frontDeskRunID && (repo == "" || all[i].Repository == repo) {
				runs = append(runs, all[i])
			}
		}
	} else {
		var err error
		if runs, err = discoverChatRuns(ctx, r.cfg, r.clk); err != nil {
			return nil, err
		}
	}
	now := time.Now()
	if r.clk != nil {
		now = r.clk.Now()
	}
	slices.SortStableFunc(runs, func(a, b store.Run) int {
		aLive := a.Lease != nil && !a.Lease.Expired(now)
		bLive := b.Lease != nil && !b.Lease.Expired(now)
		if aLive != bLive {
			if aLive {
				return -1
			}
			return 1
		}
		return b.UpdatedAt.Compare(a.UpdatedAt)
	})
	return runs, nil
}

// discoverChatRuns reads the per-run stores of the inferred repository under
// the store dir.
func discoverChatRuns(ctx context.Context, cfg Config, clk clock.Clock) ([]store.Run, error) {
	repo := strings.TrimSpace(cfg.Repository)
	if repo == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(chatStoreBaseDir(cfg))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var runs []store.Run
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		id := entry.Name()
		path, pathErr := store.RunDBPath(chatStoreBaseDir(cfg), id)
		if pathErr != nil {
			continue
		}
		st, openErr := store.OpenSQLite(path, clk)
		if openErr != nil {
			continue
		}
		run, getErr := st.Runs().Get(ctx, id)
		_ = st.Close()
		if getErr != nil || run.Repository != repo {
			continue
		}
		runs = append(runs, *run)
	}
	return runs, nil
}

// ensureChatRun creates a run record when chat is the first entry point.
func ensureChatRun(ctx context.Context, st store.Store, cfg Config) error {
	if _, err := st.Runs().Get(ctx, cfg.RunID); err == nil {
		return nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return err
	}
	objective := strings.TrimSpace(cmp.Or(cfg.Objective, cfg.Goal))
	if objective == "" {
		objective = "Owner chat for run " + cfg.RunID
	}
	if err := st.Runs().Create(ctx, store.Run{ID: cfg.RunID, Repository: cfg.Repository, Objective: objective, PlanRef: cfg.PlanRef}); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
		return err
	}
	return nil
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
  orca                                     Start or reattach to owner chat
  orca [run] --goal "<text>" [--standing] [--repo <owner/name>] [flags]
  orca chat [--run <id>] [--store sqlite:<path>|nebula] [--prompt "<question>"] [flags]`)
	_, _ = fmt.Fprintln(w, "  orca report --run <id> [--day YYYY-MM-DD] [flags]\n  orca export --run <id> [flags]")
	_, _ = fmt.Fprintf(w, "\nOrchestrator model defaults (flags and config override):\n  Dispatcher: %s\n  Planner:    %s\n  Critic:     %s (OpenAI; different vendor from Planner)\n", DefaultDispatcherModel, DefaultPlannerModel, DefaultOpenAIDispatcherModel)
	_, _ = fmt.Fprintln(w, "Without HTTP credentials, Claude roles use claude-cli and the critic uses codex-cli.")
}
