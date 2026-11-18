// Command orca runs the Nebula swarm orchestrator harness (`run`) and
// terminal concierge (`chat`).
package main

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime/debug"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/term"

	"github.com/bazelment/yoloswe/orca/adopt"
	chatui "github.com/bazelment/yoloswe/orca/chat"
	"github.com/bazelment/yoloswe/orca/clock"
	"github.com/bazelment/yoloswe/orca/lane"
	"github.com/bazelment/yoloswe/orca/mailbox"
	mailboxnebula "github.com/bazelment/yoloswe/orca/mailbox/nebula"
	"github.com/bazelment/yoloswe/orca/model"
	"github.com/bazelment/yoloswe/orca/router"
	"github.com/bazelment/yoloswe/orca/store"
)

// restartEnvVar carries "<pid>:<version>" across an in-place /restart exec
// (or "<pid>" from older binaries prior to version tagging).
const restartEnvVar = "ORCA_RESTARTED_FROM_PID"

var (
	// buildCommit can be set via linker -X main.buildCommit=<sha>.
	buildCommit = ""
	// startupPath and startupVersion are captured at process init so replacing
	// the binary on disk (rm + mv) before /restart neither points exec at a
	// deleted inode nor overwrites the running process's old version.
	startupPath    = resolveStartupPath(os.Args)
	startupVersion = detectBinaryVersion(startupPath)
	execProcess    = syscall.Exec
)

func resolveStartupPath(args []string) string {
	if len(args) > 0 && strings.TrimSpace(args[0]) != "" {
		arg0 := args[0]
		if looked, err := exec.LookPath(arg0); err == nil || (errors.Is(err, exec.ErrDot) && looked != "") {
			if abs, absErr := filepath.Abs(looked); absErr == nil {
				return abs
			}
			return looked
		}
		if abs, err := filepath.Abs(arg0); err == nil {
			if _, statErr := os.Stat(abs); statErr == nil {
				return abs
			}
		}
	}
	if exe, err := os.Executable(); err == nil {
		exe = strings.TrimSuffix(exe, " (deleted)")
		if abs, absErr := filepath.Abs(exe); absErr == nil {
			return abs
		}
		return exe
	}
	return ""
}

func stampedBuildCommit() string {
	if c := strings.TrimSpace(buildCommit); c != "" && c != "unknown" && !strings.HasPrefix(c, "{") {
		return c
	}
	return ""
}

func detectBinaryVersion(binPath string) string {
	if c := stampedBuildCommit(); c != "" {
		return c
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		var rev string
		var dirty bool
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				rev = s.Value
			case "vcs.modified":
				dirty = s.Value == "true"
			}
		}
		if rev != "" {
			if len(rev) > 12 {
				rev = rev[:12]
			}
			if dirty {
				rev += "-dirty"
			}
			return rev
		}
	}
	for _, candidate := range []string{binPath, "/proc/self/exe"} {
		if candidate == "" {
			continue
		}
		if f, err := os.Open(candidate); err == nil {
			h := sha256.New()
			_, copyErr := io.Copy(h, f)
			_ = f.Close()
			if copyErr == nil {
				return fmt.Sprintf("%x", h.Sum(nil)[:6])
			}
		}
	}
	return "dev"
}

func recordRunningVersion(resolvedStore, baseDir, ver string) string {
	var primaryPath string
	if dbPath, ok := strings.CutPrefix(strings.TrimSpace(resolvedStore), "sqlite:"); ok && dbPath != "" && dbPath != ":memory:" {
		if filepath.Base(dbPath) == "orca.db" {
			primaryPath = filepath.Join(filepath.Dir(dbPath), "running-version")
		} else {
			primaryPath = dbPath + ".running-version"
		}
	}
	frontDeskDir := filepath.Join(strings.TrimSpace(baseDir), frontDeskRunID)
	frontDeskVerFile := filepath.Join(frontDeskDir, "running-version")

	var prev string
	for _, candidate := range []string{primaryPath, frontDeskVerFile} {
		if candidate == "" {
			continue
		}
		if data, err := os.ReadFile(candidate); err == nil {
			if s := strings.TrimSpace(string(data)); s != "" {
				prev = s
				break
			}
		}
	}
	ver = strings.TrimSpace(ver)
	if ver != "" && ver != "unknown" {
		if primaryPath != "" {
			_ = os.WriteFile(primaryPath, []byte(ver+"\n"), 0o644)
		}
		if primaryPath != frontDeskVerFile && strings.TrimSpace(baseDir) != "" {
			if info, err := os.Stat(frontDeskDir); err == nil && info.IsDir() {
				_ = os.WriteFile(frontDeskVerFile, []byte(ver+"\n"), 0o644)
			}
		}
	}
	return prev
}

func currentVersion() string {
	if c := stampedBuildCommit(); c != "" {
		return c
	}
	if v := strings.TrimSpace(startupVersion); v != "" {
		return v
	}
	return detectBinaryVersion(startupPath)
}

func verifyRestartTarget(ctx context.Context, path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("restart target path is empty")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("restart target %s: %w", path, err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("restart target %s is not a regular file", path)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return "", fmt.Errorf("restart target %s is not executable (mode %s)", path, info.Mode())
	}
	checkCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(checkCtx, path, "--version").CombinedOutput()
	trimmed := strings.TrimSpace(string(out))
	if err != nil {
		if trimmed != "" {
			return "", fmt.Errorf("restart target %s --version failed: %w (%s)", path, err, trimmed)
		}
		return "", fmt.Errorf("restart target %s --version failed: %w", path, err)
	}
	line, _, _ := strings.Cut(trimmed, "\n")
	ver := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "orca"))
	return cmp.Or(ver, "unknown"), nil
}

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
	// A bare `orca` (with or without chat flags when no --run/--goal is given)
	// opens chat; flags with --run or --goal keep meaning `orca run`.
	subcmd := "run"
	explicitSubcmd := false
	rest := args
	if len(args) == 0 {
		subcmd = "chat"
		explicitSubcmd = true
	} else {
		switch args[0] {
		case "run", "chat", "adopt", "report", "export":
			subcmd = args[0]
			explicitSubcmd = true
			rest = args[1:]
		case "-h", "--help", "help":
			printUsage(stdout)
			return nil
		case "-v", "--version", "-version", "version":
			_, err := fmt.Fprintf(stdout, "orca %s\n", currentVersion())
			return err
		}
	}
	adoptSanitizePos := false
	if subcmd == "adopt" && len(rest) > 0 && strings.EqualFold(rest[0], "sanitize") {
		adoptSanitizePos = true
		rest = rest[1:]
	}

	cfg, posArgs, err := parseFlags(subcmd, rest, stderr)
	if err != nil {
		return err
	}
	if adoptSanitizePos {
		cfg.AdoptSanitize = true
	}
	if !explicitSubcmd && strings.TrimSpace(cfg.RunID) == "" && strings.TrimSpace(cfg.Goal) == "" && strings.TrimSpace(cfg.Objective) == "" {
		subcmd = "chat"
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
		return executeChat(ctx, cfg, slices.Clone(args), posArgs, stdin, stdout, stderr, logger)
	case "adopt":
		if cfg.AdoptSanitize || (len(posArgs) > 0 && strings.EqualFold(posArgs[0], "sanitize")) {
			if strings.TrimSpace(cfg.AdoptDir) == "" || strings.TrimSpace(cfg.AdoptOut) == "" {
				return errors.New("orca adopt sanitize requires --dir <run> and --out <dir>")
			}
			return adopt.SanitizeDir(cfg.AdoptDir, cfg.AdoptOut)
		}
		if cfg.AdoptDiff {
			diffs, err := adopt.DiffBinaries(ctx, cfg.AdoptBinA, cfg.AdoptBinB, cfg.AdoptDir)
			if err != nil {
				return err
			}
			for _, diff := range diffs {
				data, marshalErr := json.Marshal(diff)
				if marshalErr != nil {
					return marshalErr
				}
				if _, writeErr := fmt.Fprintln(stdout, string(data)); writeErr != nil {
					return writeErr
				}
			}
			if len(diffs) > 0 {
				return fmt.Errorf("adopt diff: %d lane decision(s) differ", len(diffs))
			}
			return nil
		}
		if !cfg.DryRun {
			return errors.New("orca adopt currently supports only --dry-run; use /adopt from chat for a live handover")
		}
		runID := strings.TrimSpace(cfg.RunID)
		if runID == "" && len(posArgs) > 0 {
			runID = strings.TrimSpace(posArgs[0])
		}
		if strings.TrimSpace(cfg.AdoptDir) == "" && runID == "" {
			return errors.New("orca adopt --dry-run requires a run id")
		}
		service := &adopt.Service{Importer: adopt.Importer{}}
		var result adopt.DryRunResult
		var err error
		if dir := strings.TrimSpace(cfg.AdoptDir); dir != "" {
			// An explicit copy is authoritative, even when a positional ID was
			// also supplied. This keeps the command safe for scrubbed fixtures.
			result, err = service.DryRunDir(ctx, dir)
		} else {
			result, err = service.DryRun(ctx, runID)
		}
		if err != nil {
			return err
		}
		if cfg.AdoptJSON {
			for i := range result.Lanes {
				data, marshalErr := json.Marshal(&result.Lanes[i])
				if marshalErr != nil {
					return marshalErr
				}
				if _, writeErr := fmt.Fprintln(stdout, string(data)); writeErr != nil {
					return writeErr
				}
			}
			return nil
		}
		if _, err := fmt.Fprintf(stdout, "adopt dry-run %s format=%s dir=%s\n", result.Run.ID, result.Run.Format, result.Run.Dir); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(stdout, "run: state=%s goal=%s\n", result.Run.State, result.Run.Goal); err != nil {
			return err
		}
		for i := range result.Lanes {
			lane := &result.Lanes[i]
			if _, err := fmt.Fprintf(stdout, "lane %s: phase=%s status=%s\n", lane.ID, lane.Phase, lane.Status); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(stdout, "  evidence: %s\n", strings.Join(lane.Evidence, "; ")); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(stdout, "  move: %s; reason: %s\n", lane.Move, lane.Reason); err != nil {
				return err
			}
		}
		for _, move := range result.Moves {
			if _, err := fmt.Fprintln(stdout, "- "+move); err != nil {
				return err
			}
		}
		_, err = fmt.Fprintln(stdout, result.Verdict)
		return err
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

type restartRequest struct {
	path string
	argv []string
	env  []string
}

type detachedChild struct {
	runID     string
	storeSpec string
}

func formatDetachedRunLine(runID, storeSpec string) string {
	if strings.TrimSpace(storeSpec) != "" {
		return fmt.Sprintf("detached orca run %s continues on old binary; to roll: stop old process and run `orca run --run %s --store %s`", runID, runID, storeSpec)
	}
	return fmt.Sprintf("detached orca run %s continues on old binary; to roll: stop old process and run `orca run --run %s`", runID, runID)
}

func executeChat(
	ctx context.Context,
	cfg Config,
	execArgs []string,
	posArgs []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	logger *slog.Logger,
) error {
	reexec, err := runChatSession(ctx, cfg, execArgs, posArgs, stdin, stdout, stderr, logger)
	if err != nil || reexec == nil {
		return err
	}
	if err := execProcess(reexec.path, reexec.argv, reexec.env); err != nil {
		return fmt.Errorf("restart exec %s failed (held leases preserved; run orca to reattach): %w", reexec.path, err)
	}
	return nil
}

func runChatSession(
	ctx context.Context,
	cfg Config,
	execArgs []string,
	posArgs []string,
	stdin io.Reader,
	stdout, stderr io.Writer,
	logger *slog.Logger,
) (*restartRequest, error) {
	restartedFrom := strings.TrimSpace(os.Getenv(restartEnvVar))
	if restartedFrom != "" {
		_ = os.Unsetenv(restartEnvVar)
	}
	sessionStartVersion := currentVersion()
	if cfg.Prompt == "" && len(posArgs) == 0 && isTerminal(stderr) {
		// The chat screen owns the terminal; log lines written to it would
		// tear the screen, so they go to a file.
		path := filepath.Join(chatStoreBaseDir(cfg), "orca-chat.log")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		_, _ = fmt.Fprintf(stderr, "orca: chat log: %s\n", path)
		logger = newLogger(f, cfg.LogLevel, cfg.LogFormat)
	}
	clk := clock.NewRealClock()
	runs, err := openChatRuns(cfg, clk)
	if err != nil {
		return nil, err
	}
	defer func() { _ = runs.Close() }()
	// Keep the adoption holder stable across restarts and attached runs.
	holder, formerHolders := chatHolder(cfg.RunID)
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
		return nil, err
	}
	// The backend owns the attached store from here on and closes it, and
	// every store it later switches to, by exit.
	backend := chatui.NewStoreBackend(st, storeCfg.RunID)
	defer func() { _ = backend.Close() }()
	if frontDesk {
		backend.FrontDesk = frontDeskRunID
		if err := st.Runs().Create(ctx, store.Run{ID: storeCfg.RunID, Repository: cfg.Repository, Objective: ""}); err != nil && !errors.Is(err, store.ErrAlreadyExists) {
			return nil, err
		}
	} else if err := ensureChatRun(ctx, st, cfg); err != nil {
		return nil, err
	}
	recordedPrevVer := recordRunningVersion(resolvedStore, chatStoreBaseDir(cfg), sessionStartVersion)

	client, err := openModelClient(cfg, clk)
	if err != nil {
		return nil, err
	}

	logger.Info("orca chat starting",
		"run_id", storeCfg.RunID,
		"store", resolvedStore,
		"model", cfg.Models.Concierge,
	)

	concierge := NewConcierge(storeCfg.RunID, st, clk, roleClient(client, cfg, "concierge"), cfg.Models.Concierge)
	concierge.AgentBackend = effectiveAgentBackend(cfg, client)
	concierge.Provider = cfg.Models.Roles["concierge"].Provider
	if concierge.AgentBackend == model.AgentBackendCLI && strings.TrimSpace(cfg.ModelProvider) == "" && cfg.Models.Roles["dispatcher"].Provider == "anthropic" && concierge.Provider == "openai" {
		concierge.Provider = "anthropic"
		if concierge.modelName == DefaultOpenAIConciergeModel {
			concierge.modelName = DefaultConciergeModel
		}
	}
	concierge.Effort = cfg.Models.Roles["concierge"].Effort
	concierge.LLMGatewayURL = cfg.LLMGatewayURL
	concierge.ClaudeBin = cfg.ClaudeBin
	concierge.CodexBin = cfg.CodexBin
	concierge.Investigate = runInvestigateConfig(storeCfg)
	concierge.Logger = logger
	wireChatConcierge(concierge, backend)
	defer func() { _ = concierge.Close(context.WithoutCancel(ctx)) }()

	chatPlanner := NewPlanner(st, clk, roleClient(client, cfg, "planner"), cfg.Models.Planner, logger)
	chatPlanner.critic = roleClient(client, cfg, "critic")
	chatPlanner.criticModel = cfg.Models.Roles["critic"].Model
	chatPlanner.providerName = cfg.Models.Roles["planner"].Provider
	chatPlanner.criticProvider = cfg.Models.Roles["critic"].Provider
	chatPlanner.effort = cfg.Models.Roles["planner"].Effort
	chatPlanner.agentBackend = concierge.AgentBackend
	chatPlanner.llmGatewayURL = cfg.LLMGatewayURL
	chatPlanner.investigate = concierge.Investigate
	chatPlanner.claudeBin = cfg.ClaudeBin
	chatPlanner.codexBin = cfg.CodexBin
	defer func() { _ = chatPlanner.Close() }()
	// A restarted chat continues the conversation it left.
	resumed, err := concierge.Resume(ctx)
	if err != nil {
		return nil, err
	}
	host := newHostSwarms(cfg, st, clk)
	backend.HostSwarms = host.importer.Discover
	backend.HostProbe = host.probe

	question := strings.TrimSpace(cfg.Prompt)
	if question == "" && len(posArgs) > 0 {
		question = strings.TrimSpace(strings.Join(posArgs, " "))
	}
	adoption, adoptionOff := newAdoption(ctx, cfg, host, holder, formerHolders, st, clk, client, logger)
	backend.Adoption, backend.AdoptionUnavailable = adoption, adoptionOff
	var restartedRuns []adopt.Run
	if adoption != nil {
		defer func() {
			if closer, ok := adoption.Driver.(interface{ Close(context.Context) error }); ok {
				_ = closer.Close(context.WithoutCancel(ctx))
			}
		}()
		var adoptErr error
		restartedRuns, adoptErr = backend.RestartHeld(ctx)
		if adoptErr != nil && logger != nil {
			logger.Warn("reattach adopted run failed", "error", adoptErr)
		}
	}
	routerSel := cfg.Models.Roles["router"]
	routingLog := attachedRoutingLog{backend: backend, model: routerSel.Model, provider: routerSel.Provider}
	r := router.Select(roleClient(client, cfg, "router"), routerSel.Model, os.Getenv("ORCA_GEMINI_API_KEY"), os.Getenv("GEMINI_DECISIONS_BASE_URL"), routingLog)
	// run.start runs inside a concierge stream turn; the new run is attached
	// once that turn ends, so the turn finishes on the store it started on.
	var pendingRunID string
	var pendingRunStore store.Store
	var detachedChildren []detachedChild
	backend.Stream = func(streamCtx context.Context, text string, emit func(string)) error {
		err := concierge.AskStream(context.WithValue(streamCtx, runStartEchoKey{}, emit), text, emit)
		if pendingRunStore != nil {
			runStore, runID := pendingRunStore, pendingRunID
			pendingRunStore, pendingRunID = nil, ""
			err = errors.Join(err, backend.Attach(runStore, runID))
		}
		return err
	}
	routerOnlyReplay := isRouterOnlyReplay(cfg.ReplayPath)
	backend.PlannerStream = func(streamCtx context.Context, runStore store.Store, activeRun, text string, emit func(string)) error {
		if routerOnlyReplay || (chatPlanner.client == nil && chatPlanner.agentBackend != model.AgentBackendCLI) {
			return chatui.ErrPlannerFallbackToQueue
		}
		_, askErr := chatPlanner.askStream(streamCtx, runStore, activeRun, text, "", concierge.investigateConfig(streamCtx), backend.ReadHostRun, emit)
		return askErr
	}
	concierge.AskPlanner = func(askCtx context.Context, targetRun, q, findings string, emit func(string)) (string, error) {
		var ans string
		err := backend.WithRun(func(runStore store.Store, activeRun string) error {
			runID := activeRun
			if requested := strings.TrimSpace(targetRun); requested != "" {
				if requested != activeRun {
					return fmt.Errorf("planner target run %q is not the attached active run %q", requested, activeRun)
				}
				runID = requested
			}
			var askErr error
			ans, askErr = chatPlanner.askStream(askCtx, runStore, runID, q, findings, concierge.investigateConfig(askCtx), backend.ReadHostRun, emit)
			return askErr
		})
		return ans, err
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
		detachedChildren = append(detachedChildren, detachedChild{runID: runID, storeSpec: spec})
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
	var reexec *restartRequest
	backend.Restart = func(restartCtx context.Context, requestedPath string) (string, error) {
		target := strings.TrimSpace(requestedPath)
		if target == "" {
			target = strings.TrimSpace(startupPath)
		} else if !filepath.IsAbs(target) {
			return "", fmt.Errorf("restart target %s must be an absolute path", target)
		}
		newVer, err := verifyRestartTarget(restartCtx, target)
		if err != nil {
			return "", err
		}
		oldVer := cmp.Or(sessionStartVersion, currentVersion())
		lines := []string{fmt.Sprintf("restarting orca (%s -> %s): %s", oldVer, newVer, target)}
		seenDetached := map[string]bool{}
		for _, child := range detachedChildren {
			seenDetached[child.runID] = true
			lines = append(lines, formatDetachedRunLine(child.runID, child.storeSpec))
		}
		if listed, listErr := runs.List(restartCtx); listErr == nil {
			now := clk.Now()
			for i := range listed {
				lr := &listed[i]
				if seenDetached[lr.ID] || lr.Lease == nil || lr.Lease.Expired(now) || strings.HasPrefix(lr.Lease.HolderID, adopt.ChatHolder("")) {
					continue
				}
				seenDetached[lr.ID] = true
				spec := runs.spec
				if spec == "" {
					if p, pErr := store.RunDBPath(chatStoreBaseDir(cfg), lr.ID); pErr == nil {
						spec = "sqlite:" + p
					}
				}
				lines = append(lines, formatDetachedRunLine(lr.ID, spec))
			}
		}
		env := append(os.Environ(), fmt.Sprintf("%s=%d:%s", restartEnvVar, os.Getpid(), oldVer))
		reexec = &restartRequest{
			path: target,
			argv: append([]string{target}, execArgs...),
			env:  env,
		}
		return strings.Join(lines, "\n"), nil
	}
	concierge.Restart = backend.Restart
	if question != "" {
		var d router.Decision
		if chatui.IsNewGoal(question) {
			d = router.Decision{Handler: router.Concierge, Confidence: 1, Backend: "goal-detection"}
		} else {
			d, err = r.Route(ctx, question)
			if err != nil {
				return nil, err
			}
		}
		echo := chatui.RouteEcho(string(d.Handler), question, d.Fallback)
		if _, err := fmt.Fprintln(stdout, echo); err != nil {
			return nil, err
		}
		var reply strings.Builder
		if err := backend.Dispatch(ctx, d, question, func(s string) { reply.WriteString(s) }); err != nil {
			return nil, err
		}
		_, err = fmt.Fprintln(stdout, chatui.SanitizeText(reply.String()))
		return nil, err
	}
	var heldLines []string
	reattachedSet := map[string]bool{}
	for i := range restartedRuns {
		r := &restartedRuns[i]
		reattachedSet[r.ID] = true
		heldLines = append(heldLines, fmt.Sprintf("reattached %s (%d lanes, driving again, pid %d)", r.ID, len(r.Lanes), os.Getpid()))
	}
	if !frontDesk && !reattachedSet[storeCfg.RunID] && restartedFrom != "" {
		heldLines = append(heldLines, fmt.Sprintf("reattached %s (driving again, pid %d)", storeCfg.RunID, os.Getpid()))
	}
	var reattachLines []string
	if restartedFrom != "" {
		_, oldVer, _ := strings.Cut(restartedFrom, ":")
		if strings.TrimSpace(oldVer) == "" || strings.EqualFold(strings.TrimSpace(oldVer), "unknown") {
			oldVer = recordedPrevVer
		}
		reattachLines = chatui.FormatRestartStatus(oldVer, cmp.Or(sessionStartVersion, currentVersion()), heldLines)
	} else {
		reattachLines = heldLines
	}
	if !isTerminal(stdin) {
		for _, line := range reattachLines {
			if _, err := fmt.Fprintln(stdout, line); err != nil {
				return nil, err
			}
		}
	}
	initialLines := append(transcriptLines(resumed), reattachLines...)
	ui := chatui.New(ctx, storeCfg.RunID, concierge.Ask, r).WithTranscript(initialLines)
	ui.Backend = backend
	ui.Dispatch = backend.Dispatch
	if err := chatui.Run(ctx, stdin, stdout, ui); err != nil {
		return nil, err
	}
	if reexec != nil {
		shutdownCtx := context.WithoutCancel(ctx)
		var shutdownErr error
		if adoption != nil {
			if closer, ok := adoption.Driver.(interface{ Close(context.Context) error }); ok {
				shutdownErr = errors.Join(shutdownErr, closer.Close(shutdownCtx))
			}
		}
		shutdownErr = errors.Join(
			shutdownErr,
			concierge.Close(shutdownCtx),
			chatPlanner.Close(),
			backend.Close(),
			runs.Close(),
		)
		if shutdownErr != nil {
			return nil, fmt.Errorf("restart shutdown failed: %w", shutdownErr)
		}
	}
	return reexec, nil
}

// wireChatConcierge connects run listing, directive routing, wakeups, held
// runs, and live probes.
func wireChatConcierge(c *Concierge, b *chatui.StoreBackend) {
	c.BindBackend(b)
}

func isRouterOnlyReplay(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	exchanges, err := model.LoadRecording(path)
	if err != nil || len(exchanges) == 0 {
		return false
	}
	for i := range exchanges {
		txt := strings.TrimSpace(exchanges[i].Response.Text())
		if !strings.HasPrefix(txt, "{") || !strings.Contains(txt, `"handler"`) || len(exchanges[i].Response.ToolCalls()) > 0 {
			return false
		}
	}
	return true
}

// chatHolder returns the stable holder and any legacy holder ids it owns.
func chatHolder(runID string) (holder string, former []string) {
	if id := strings.TrimSpace(runID); id != "" {
		return adopt.ChatHolder(id), nil
	}
	return adopt.ChatHolder(frontDeskRunID), []string{adopt.ChatHolder("")}
}

// transcriptLines renders the tail of resumed history.
func transcriptLines(history []model.Message) []string {
	plain := plainHistory(history)
	if len(plain) == 0 {
		return nil
	}
	var turns [][]model.Message
	var cur []model.Message
	for _, m := range plain {
		if m.Role == model.RoleUser && len(cur) > 0 {
			turns = append(turns, cur)
			cur = nil
		}
		cur = append(cur, m)
	}
	if len(cur) > 0 {
		turns = append(turns, cur)
	}
	var lines []string
	if len(turns) > chatui.DefaultMaxTurns {
		lines = append(lines, fmt.Sprintf("%d earlier turns; /log to see them", len(turns)-chatui.DefaultMaxTurns))
		turns = turns[len(turns)-chatui.DefaultMaxTurns:]
	}
	for _, turn := range turns {
		for _, m := range turn {
			prefix := "you> "
			if m.Role == model.RoleAssistant {
				prefix = "orca> "
			}
			lines = append(lines, prefix+m.Text())
		}
	}
	return lines
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
// keeps its front desk in; otherwise each run, the front desk included, has
// its own SQLite store under the store dir, as `orca run` does.
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
	err := r.shared.Store.Close()
	r.shared = nil
	return err
}

// Open opens the store holding cfg.RunID: the explicit store or the run's own
// SQLite store.
func (r *chatRuns) Open(cfg Config) (store.Store, string, error) {
	if r.shared != nil {
		return r.shared, r.spec, nil
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
			if all[i].ID != frontDeskRunID && all[i].DefaultRecipe != "adopted" && (repo == "" || all[i].Repository == repo) {
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
		if id == frontDeskRunID {
			continue
		}
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
		if getErr != nil || run.DefaultRecipe == "adopted" || run.Repository != repo {
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

func isTerminal(w any) bool {
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
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
	_, _ = fmt.Fprintln(w, "  orca adopt --dry-run <run-id> [--dir <copied-run-dir>]\n  orca report --run <id> [--day YYYY-MM-DD] [flags]\n  orca export --run <id> [flags]")
	_, _ = fmt.Fprintf(w, "\nOrchestrator model defaults (flags and config override):\n  Dispatcher: %s\n  Planner:    %s\n  Critic:     %s (OpenAI; different vendor from Planner)\n", DefaultDispatcherModel, DefaultPlannerModel, DefaultOpenAIDispatcherModel)
	_, _ = fmt.Fprintln(w, "Without HTTP credentials, Claude roles use claude-cli and the critic uses codex-cli.")
}
