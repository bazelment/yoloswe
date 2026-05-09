// Command yoloswe provides commands for AI-assisted software engineering.
//
// Commands:
//   - plan: Run planning mode to design implementations before execution
//   - build: Run a builder-reviewer loop for autonomous task execution
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/acp"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/agent-cli-wrapper/llmendpoint"
	"github.com/bazelment/yoloswe/cliapp"
	"github.com/bazelment/yoloswe/multiagent/agent"
	"github.com/bazelment/yoloswe/wt"
	"github.com/bazelment/yoloswe/yoloswe"
	"github.com/bazelment/yoloswe/yoloswe/planner"
)

var rootOpts = cliapp.Options{ToolName: "yoloswe"}

func main() {
	rootCmd := &cobra.Command{
		Use:   "yoloswe",
		Short: "AI-assisted software engineering tool",
		Long: `yoloswe provides AI-assisted software engineering capabilities.

Use 'plan' to design and plan implementations before execution.
Use 'build' to run a builder-reviewer loop for autonomous task execution.`,
	}

	cliapp.RegisterStandardFlags(rootCmd, &rootOpts)
	rootCmd.AddCommand(newPlanCmd())
	rootCmd.AddCommand(newBuildCmd())
	rootCmd.AddCommand(newCodeTalkCmd())

	os.Exit(cliapp.Run(&rootOpts, func(ctx context.Context, app *cliapp.App) error {
		return rootCmd.ExecuteContext(cliapp.WithApp(ctx, app))
	}))
}

// Plan command flags
type planFlags struct {
	model           string
	workDir         string
	recordDir       string
	systemPrompt    string
	build           string
	externalBuilder string
	buildModel      string
	simple          bool
}

func newPlanCmd() *cobra.Command {
	flags := &planFlags{}

	cmd := &cobra.Command{
		Use:   "plan [flags] <prompt>",
		Short: "Run planning mode to design implementations",
		Long: `Plan mode helps you design implementations by analyzing requirements and designing solutions.
The AI will explore the codebase, consider approaches, and produce a detailed plan.`,
		Example: `  yoloswe plan "Create a hello world Go program"
  yoloswe plan --model opus "Implement a REST API"
  echo "Add tests" | yoloswe plan
  yoloswe plan --build new --external-builder ./yoloswe "Add comprehensive tests"`,
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runPlan(cmd, args, flags)
		},
	}

	cmd.Flags().StringVar(&flags.model, "model", "opus", "Model to use for planning: haiku, sonnet, opus")
	cmd.Flags().StringVar(&flags.workDir, "dir", "", "Working directory (defaults to current directory)")
	cmd.Flags().StringVar(&flags.recordDir, "record", "", "Directory for session recordings (defaults to ~/.yoloswe)")
	cmd.Flags().StringVar(&flags.systemPrompt, "system", "", "Custom system prompt")
	cmd.Flags().BoolVar(&flags.simple, "simple", false, "Auto-answer questions with first option and export plan on completion")
	cmd.Flags().StringVar(&flags.build, "build", "", "After planning, execute: 'current' (same session) or 'new' (fresh session)")
	cmd.Flags().StringVar(&flags.externalBuilder, "external-builder", "", "Path to external builder executable (e.g., yoloswe build). Used with --build new.")
	cmd.Flags().StringVar(&flags.buildModel, "build-model", "sonnet", "Model to use for build phase (defaults to sonnet)")

	return cmd
}

func runPlan(cmd *cobra.Command, args []string, flags *planFlags) error {
	app := cliapp.FromContext(cmd.Context())

	prompt := strings.Join(args, " ")
	if prompt == "" {
		prompt = readFromStdin()
	}
	if prompt == "" {
		_ = cmd.Usage()
		return fmt.Errorf("no prompt provided")
	}

	workDir, err := resolveWorkDir(flags.workDir)
	if err != nil {
		return err
	}

	buildMode := planner.BuildMode(flags.build)
	if !buildMode.IsValid() {
		return fmt.Errorf("invalid build mode %q (valid: 'current', 'new', or empty)", flags.build)
	}

	config := planner.Config{
		Model:               flags.model,
		WorkDir:             workDir,
		RecordingDir:        flags.recordDir,
		SystemPrompt:        flags.systemPrompt,
		Verbose:             app.Verbosity >= render.VerbosityVerbose,
		Simple:              flags.simple,
		Prompt:              prompt,
		BuildMode:           buildMode,
		ExternalBuilderPath: flags.externalBuilder,
		BuildModel:          flags.buildModel,
	}

	p := planner.NewPlannerWrapper(config)

	ctx := cmd.Context()
	if err := p.Start(ctx); err != nil {
		return fmt.Errorf("start session: %w", err)
	}
	defer p.Stop()

	if err := p.Run(ctx, prompt); err != nil {
		if ctx.Err() != nil {
			return nil
		}
		return err
	}

	p.PrintUsageSummary()
	if path := p.RecordingPath(); path != "" {
		fmt.Fprintf(os.Stderr, "\nSession recorded to: %s\n", path)
	}
	return nil
}

// Build command flags
type buildFlags struct {
	builderModel    string
	reviewerModel   string
	dir             string
	record          string
	systemPrompt    string
	resumeSession   string
	budget          float64
	timeout         int
	maxIterations   int
	requireApproval bool
	reviewFirst     bool
}

func newBuildCmd() *cobra.Command {
	flags := &buildFlags{}

	cmd := &cobra.Command{
		Use:   "build [flags] <prompt>",
		Short: "Run a builder-reviewer loop for software engineering tasks",
		Long: `Build runs a builder-reviewer loop for software engineering tasks.
The builder (Claude) implements the task, and the reviewer (Codex) reviews.
The loop continues until the reviewer accepts or limits are reached.`,
		Example: `  yoloswe build "Add unit tests for the user service"
  yoloswe build --budget 10 --timeout 1800 "Refactor the database layer"
  yoloswe build --builder-model opus "Fix the authentication bug"
  yoloswe build "Implement feature X" --timeout 7200`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runBuild(cmd, args, flags)
		},
	}

	cmd.Flags().StringVar(&flags.builderModel, "builder-model", "sonnet", "Builder model: haiku, sonnet, opus")
	cmd.Flags().StringVar(&flags.reviewerModel, "reviewer-model", "", "Reviewer model (default: gpt-5.4-mini)")
	cmd.Flags().StringVar(&flags.dir, "dir", "", "Working directory (default: current)")
	cmd.Flags().Float64Var(&flags.budget, "budget", 100.0, "Max USD for builder session")
	cmd.Flags().IntVar(&flags.timeout, "timeout", 3600, "Max seconds")
	cmd.Flags().IntVar(&flags.maxIterations, "max-iterations", 100, "Max builder-reviewer iterations")
	cmd.Flags().StringVar(&flags.record, "record", "", "Session recordings directory (default: ~/.yoloswe)")
	cmd.Flags().StringVar(&flags.systemPrompt, "system", "", "Custom system prompt for builder")
	cmd.Flags().BoolVar(&flags.requireApproval, "require-approval", false, "Require user approval for tool executions (default: auto-approve)")
	cmd.Flags().StringVar(&flags.resumeSession, "resume", "", "Resume from a previous session ID")
	cmd.Flags().BoolVar(&flags.reviewFirst, "review-first", false, "Skip first builder turn and start with review")

	return cmd
}

func runBuild(cmd *cobra.Command, args []string, flags *buildFlags) error {
	app := cliapp.FromContext(cmd.Context())
	prompt := strings.Join(args, " ")

	workDir, err := resolveWorkDir(flags.dir)
	if err != nil {
		return err
	}

	recordingDir := flags.record
	if recordingDir == "" {
		homeDir, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("get home directory: %w", err)
		}
		recordingDir = filepath.Join(homeDir, ".yoloswe")
	}

	config := yoloswe.Config{
		BuilderModel:    flags.builderModel,
		BuilderWorkDir:  workDir,
		RecordingDir:    recordingDir,
		SystemPrompt:    flags.systemPrompt,
		RequireApproval: flags.requireApproval,
		ResumeSessionID: flags.resumeSession,
		ReviewFirst:     flags.reviewFirst,
		ReviewerModel:   flags.reviewerModel,
		Goal:            prompt,
		MaxBudgetUSD:    flags.budget,
		MaxTimeSeconds:  flags.timeout,
		MaxIterations:   flags.maxIterations,
		Verbose:         app.Verbosity >= render.VerbosityVerbose,
	}

	app.Logger.Info("yoloswe build config",
		"builder_model", config.BuilderModel,
		"reviewer_model", config.ReviewerModel,
		"work_dir", config.BuilderWorkDir,
		"budget_usd", config.MaxBudgetUSD,
		"timeout_seconds", config.MaxTimeSeconds,
		"max_iterations", config.MaxIterations,
		"prompt", prompt,
	)

	swe := yoloswe.New(config)
	runErr := swe.Run(cmd.Context(), prompt)
	swe.PrintSummary()
	if runErr != nil {
		return runErr
	}
	if swe.Stats().ExitReason != yoloswe.ExitReasonAccepted {
		return fmt.Errorf("build did not complete successfully (reason: %v)", swe.Stats().ExitReason)
	}
	return nil
}

// codetalk command flags
type codeTalkFlags struct {
	backend         string
	model           string
	workDir         string
	recordDir       string
	systemPrompt    string
	llmBaseURL      string
	llmAPIKey       string
	llmAPIKeyEnv    string
	llmProviderName string
	llmWireAPI      string
}

func newCodeTalkCmd() *cobra.Command {
	flags := &codeTalkFlags{}
	cmd := &cobra.Command{
		Use:   "codetalk [flags] <prompt>",
		Short: "One-shot code understanding session",
		Long: `Codetalk runs a single read-only code-understanding turn against the chosen
backend. The agent has Read/Grep/Glob and read-only Bash; it cannot modify files.

Pass --backend=claude (default), codex, gemini, or cursor to pick the underlying
CLI. The --llm-* flags route inference through a third-party LLM API endpoint
(e.g. Baseten, OpenRouter, LiteLLM). Note: the claude backend requires an
Anthropic-shaped endpoint — use codex or gemini for raw OpenAI-compatible
endpoints.`,
		Example: `  yoloswe codetalk "explain agent-cli-wrapper"
  yoloswe codetalk --backend codex --model moonshotai/Kimi-K2.6 \
    --llm-base-url https://inference.baseten.co/v1 \
    --llm-api-key-env BASETEN_API_KEY --llm-provider-name baseten \
    --llm-wire-api chat \
    "explain the agent-cli-wrapper structure"`,
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runCodeTalk(cmd, args, flags)
		},
	}
	cmd.Flags().StringVar(&flags.backend, "backend", agent.ProviderClaude, "Backend CLI: "+strings.Join(agent.AllProviders, ", "))
	cmd.Flags().StringVar(&flags.model, "model", "", "Model to use (defaults: claude=opus, codex=gpt-5.5, gemini=gemini-2.5-pro)")
	cmd.Flags().StringVar(&flags.workDir, "dir", "", "Working directory (default: current)")
	cmd.Flags().StringVar(&flags.recordDir, "record", "", "Recording directory (default: ~/.yoloswe)")
	cmd.Flags().StringVar(&flags.systemPrompt, "system", "", "Custom system prompt")
	cmd.Flags().StringVar(&flags.llmBaseURL, "llm-base-url", "", "Custom LLM endpoint base URL")
	cmd.Flags().StringVar(&flags.llmAPIKey, "llm-api-key", "", "Custom LLM API key — UNSAFE on shared/CI hosts (visible in shell history and `ps`); prefer --llm-api-key-env")
	// Hide --llm-api-key from --help so the env-var path is the path of
	// least resistance. The flag still works for local dev convenience but
	// shouldn't show up in help output where operators discover defaults.
	if err := cmd.Flags().MarkHidden("llm-api-key"); err != nil {
		// Cobra only errors here when the flag was never registered; the
		// preceding StringVar guarantees it was, so a non-nil err is a
		// programming bug worth surfacing.
		panic(fmt.Errorf("MarkHidden(llm-api-key): %w", err))
	}
	cmd.Flags().StringVar(&flags.llmAPIKeyEnv, "llm-api-key-env", "", "Env var name holding the LLM API key (e.g. BASETEN_API_KEY)")
	cmd.Flags().StringVar(&flags.llmProviderName, "llm-provider-name", "", "Provider name label (codex model_providers.<name>)")
	cmd.Flags().StringVar(&flags.llmWireAPI, "llm-wire-api", "chat", "Wire API: chat (OpenAI-compatible) or responses")
	return cmd
}

func runCodeTalk(cmd *cobra.Command, args []string, flags *codeTalkFlags) error {
	app := cliapp.FromContext(cmd.Context())
	prompt := strings.Join(args, " ")

	workDir, err := resolveWorkDir(flags.workDir)
	if err != nil {
		return err
	}

	// Build the endpoint only when the user actually opted in (any of the
	// routing/auth flags non-empty). The wire flag defaults to "chat" so we
	// can't include it in the gate; a bare `yoloswe codetalk "..."` invocation
	// must produce a zero Endpoint that wrappers skip.
	var ep llmendpoint.Endpoint
	if flags.llmBaseURL != "" || flags.llmAPIKey != "" || flags.llmAPIKeyEnv != "" {
		ep = llmendpoint.Endpoint{
			BaseURL:      flags.llmBaseURL,
			APIKey:       flags.llmAPIKey,
			APIKeyEnv:    flags.llmAPIKeyEnv,
			ProviderName: flags.llmProviderName,
			Wire:         llmendpoint.WireAPI(flags.llmWireAPI),
		}
		if err := ep.Validate(); err != nil {
			return err
		}
	}
	if flags.llmAPIKey != "" {
		fmt.Fprintln(os.Stderr,
			"warning: --llm-api-key passes the secret via argv (visible in shell history "+
				"and `ps` listings); prefer --llm-api-key-env for shared/CI environments.")
	}
	app.Logger.Info("codetalk", "backend", flags.backend, "model", flags.model, "endpoint", ep.String())

	backend := strings.ToLower(flags.backend)
	if backend == "" {
		backend = agent.ProviderClaude
	}
	if backend == agent.ProviderClaude {
		return runCodeTalkClaude(cmd.Context(), flags, ep, workDir, prompt)
	}
	for _, p := range agent.AllProviders {
		if backend == p {
			return runCodeTalkProvider(cmd.Context(), backend, flags, ep, workDir, prompt)
		}
	}
	return fmt.Errorf("unknown backend %q (valid: %s)", flags.backend, strings.Join(agent.AllProviders, ", "))
}

func runCodeTalkClaude(ctx context.Context, flags *codeTalkFlags, ep llmendpoint.Endpoint, workDir, prompt string) error {
	model := flags.model
	if model == "" {
		model = "opus"
	}
	cfg := yoloswe.CodeTalkConfig{
		Model:        model,
		WorkDir:      workDir,
		RecordingDir: flags.recordDir,
		SystemPrompt: flags.systemPrompt,
		LLMEndpoint:  ep,
	}
	session := yoloswe.NewCodeTalkSession(cfg, os.Stdout)
	if err := session.Start(ctx); err != nil {
		return fmt.Errorf("start codetalk session: %w", err)
	}
	defer session.Stop()

	if _, err := session.RunTurn(ctx, prompt); err != nil {
		return err
	}
	return nil
}

func runCodeTalkProvider(ctx context.Context, backend string, flags *codeTalkFlags, ep llmendpoint.Endpoint, workDir, prompt string) error {
	model := flags.model
	var prov agent.Provider
	switch backend {
	case agent.ProviderCodex:
		if model == "" {
			model = "gpt-5.5"
		}
		prov = agent.NewCodexProvider()
	case agent.ProviderGemini:
		if model == "" {
			model = "gemini-2.5-pro"
		}
		prov = agent.NewGeminiProvider(acp.WithBinaryArgs("--experimental-acp"))
	case agent.ProviderCursor:
		if model == "" {
			model = "cursor-default"
		}
		prov = agent.NewCursorProvider()
	default:
		return fmt.Errorf("backend %q not supported by codetalk provider path", backend)
	}
	defer prov.Close()

	systemPrompt := flags.systemPrompt
	if systemPrompt == "" {
		systemPrompt = yoloswe.CodeTalkSystemPrompt
	}

	opts := []agent.ExecuteOption{
		agent.WithProviderModel(model),
		agent.WithProviderWorkDir(workDir),
		agent.WithProviderSystemPrompt(systemPrompt),
		agent.WithProviderPermissionMode("bypass"),
	}
	if !ep.IsZero() {
		opts = append(opts, agent.WithProviderLLMEndpoint(ep))
	}

	res, err := prov.Execute(ctx, prompt, (*wt.WorktreeContext)(nil), opts...)
	if err != nil {
		return err
	}
	if res.Text != "" {
		fmt.Println(res.Text)
	}
	if !res.Success && res.Error != nil {
		return res.Error
	}
	return nil
}

// resolveWorkDir returns flagDir, or the current working directory when
// flagDir is empty.
func resolveWorkDir(flagDir string) (string, error) {
	if flagDir != "" {
		return flagDir, nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	return wd, nil
}

// readFromStdin reads input from stdin if available.
func readFromStdin() string {
	stat, _ := os.Stdin.Stat()
	if (stat.Mode() & os.ModeCharDevice) != 0 {
		// stdin is a terminal, not piped input
		return ""
	}

	var lines []string
	scanner := bufio.NewScanner(os.Stdin)
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	return strings.Join(lines, "\n")
}
