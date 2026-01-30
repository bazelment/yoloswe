// yoloplanner is a simple wrapper over claude-cli for planner mode.
//
// Usage:
//
//	yoloplanner [flags] <prompt>
//
// Examples:
//
//	yoloplanner "Create a hello world Go program"
//	yoloplanner -model opus "Implement a REST API"
//	echo "Add tests" | yoloplanner
package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/bazelment/yoloswe/yoloswe/planner"
)

var (
	model           string
	workDir         string
	recordDir       string
	systemPrompt    string
	verbose         bool
	simple          bool
	build           string
	externalBuilder string
	buildModel      string
)

func main() {
	rootCmd := &cobra.Command{
		Use:   "yoloplanner [flags] <prompt>",
		Short: "A simple wrapper over claude-cli for planner mode",
		Long: `yoloplanner is a simple wrapper over claude-cli for planner mode.
It helps you plan implementations by analyzing requirements and designing solutions.`,
		Example: `  yoloplanner "Create a hello world Go program"
  yoloplanner -model opus "Implement a REST API"
  echo "Add tests" | yoloplanner
  yoloplanner --build new --external-builder ./yoloswe "Add comprehensive tests"`,
		Args: cobra.ArbitraryArgs,
		Run:  run,
	}

	// Define flags
	rootCmd.Flags().StringVar(&model, "model", "opus", "Model to use for planning: haiku, sonnet, opus")
	rootCmd.Flags().StringVar(&workDir, "dir", "", "Working directory (defaults to current directory)")
	rootCmd.Flags().StringVar(&recordDir, "record", "", "Directory for session recordings (defaults to ~/.yoloswe)")
	rootCmd.Flags().StringVar(&systemPrompt, "system", "", "Custom system prompt")
	rootCmd.Flags().BoolVar(&verbose, "verbose", false, "Show detailed tool results (errors are always shown)")
	rootCmd.Flags().BoolVar(&simple, "simple", false, "Auto-answer questions with first option and export plan on completion")
	rootCmd.Flags().StringVar(&build, "build", "", "After planning, execute: 'current' (same session) or 'new' (fresh session)")
	rootCmd.Flags().StringVar(&externalBuilder, "external-builder", "", "Path to external builder executable (e.g., yoloswe). Used with --build new.")
	rootCmd.Flags().StringVar(&buildModel, "build-model", "sonnet", "Model to use for build phase (defaults to sonnet)")

	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, args []string) {
	// Get prompt from args or stdin
	prompt := strings.Join(args, " ")
	if prompt == "" {
		prompt = readFromStdin()
	}
	if prompt == "" {
		fmt.Fprintln(os.Stderr, "Error: no prompt provided")
		cmd.Usage()
		os.Exit(1)
	}

	// Set default working directory
	if workDir == "" {
		var err error
		workDir, err = os.Getwd()
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error getting working directory: %v\n", err)
			os.Exit(1)
		}
	}

	// Validate build mode
	buildMode := planner.BuildMode(build)
	if !buildMode.IsValid() {
		fmt.Fprintf(os.Stderr, "Error: invalid build mode %q. Valid values: 'current', 'new', or empty\n", build)
		os.Exit(1)
	}

	// Create config
	config := planner.Config{
		Model:               model,
		WorkDir:             workDir,
		RecordingDir:        recordDir,
		SystemPrompt:        systemPrompt,
		Verbose:             verbose,
		Simple:              simple,
		Prompt:              prompt,
		BuildMode:           buildMode,
		ExternalBuilderPath: externalBuilder,
		BuildModel:          buildModel,
	}

	// Create planner wrapper
	planner := planner.NewPlannerWrapper(config)

	// Setup context with signal handling
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigChan
		fmt.Fprintln(os.Stderr, "\nInterrupted, shutting down...")
		cancel()
	}()

	// Start the session
	if err := planner.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "Error starting session: %v\n", err)
		os.Exit(1)
	}
	defer planner.Stop()

	// Run the planner
	if err := planner.Run(ctx, prompt); err != nil {
		if ctx.Err() != nil {
			// Context cancelled, exit gracefully
			os.Exit(0)
		}
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	// Print usage summary
	planner.PrintUsageSummary()

	// Print recording path
	if path := planner.RecordingPath(); path != "" {
		fmt.Fprintf(os.Stderr, "\nSession recorded to: %s\n", path)
	}
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
