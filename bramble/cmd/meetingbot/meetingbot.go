// Package meetingbot provides a CLI harness for the meeting bot.
package meetingbot

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	botpkg "github.com/bazelment/yoloswe/bramble/meetingbot"
	"github.com/bazelment/yoloswe/multiagent/agent"
)

var (
	notesGlob       string
	notePaths       []string
	questions       []string
	agentMode       string
	workDir         string
	fastModel       string
	researchModel   string
	codeModel       string
	webModel        string
	summaryModel    string
	latencyBudget   time.Duration
	answerTimeout   time.Duration
	researchTimeout time.Duration
	summaryTimeout  time.Duration
	maxTopics       int
	maxSnippets     int
	evaluate        bool
	qualityGate     bool
	evalReport      string
)

// Cmd is the meeting bot command.
var Cmd = &cobra.Command{
	Use:   "meetingbot [note files...]",
	Short: "Run the research-backed meeting bot on transcript notes",
	Long: `Run a meeting bot that follows timestamped meeting transcripts,
builds background research through specialized Codex/Claude-style agents,
answers live questions with a low-latency opening, and writes a final summary.`,
	RunE: run,
}

func init() {
	Cmd.Flags().StringVar(&notesGlob, "notes-glob", "", "Glob of transcript files to load")
	Cmd.Flags().StringArrayVar(&notePaths, "note", nil, "Transcript file to load; may be repeated")
	Cmd.Flags().StringArrayVarP(&questions, "question", "q", nil, "Question to ask; may be repeated")
	Cmd.Flags().StringVar(&agentMode, "agent", "local", "Agent mode: real or local")
	Cmd.Flags().StringVar(&workDir, "work-dir", ".", "Repository work directory for codebase research")
	Cmd.Flags().StringVar(&fastModel, "fast-model", "gpt-5.3-codex", "Model for live fast answers")
	Cmd.Flags().StringVar(&researchModel, "research-model", "sonnet", "Model for internal research")
	Cmd.Flags().StringVar(&codeModel, "code-model", "gpt-5.3-codex", "Model for codebase research")
	Cmd.Flags().StringVar(&webModel, "web-model", "gpt-5.3-codex", "Model for public web research")
	Cmd.Flags().StringVar(&summaryModel, "summary-model", "gpt-5.5", "Model for final summaries")
	Cmd.Flags().DurationVar(&latencyBudget, "latency-budget", 10*time.Second, "Target latency for grounded opening")
	Cmd.Flags().DurationVar(&answerTimeout, "answer-timeout", 45*time.Second, "Timeout for full fast-answer model synthesis")
	Cmd.Flags().DurationVar(&researchTimeout, "research-timeout", 90*time.Second, "Timeout for each background research model call")
	Cmd.Flags().DurationVar(&summaryTimeout, "summary-timeout", 2*time.Minute, "Timeout for final summary model synthesis")
	Cmd.Flags().IntVar(&maxTopics, "max-topics", 4, "Maximum background topics per note")
	Cmd.Flags().IntVar(&maxSnippets, "max-snippets", 18, "Maximum transcript snippets per agent prompt")
	Cmd.Flags().BoolVar(&evaluate, "evaluate", false, "Run the default interaction evaluation set")
	Cmd.Flags().BoolVar(&qualityGate, "quality-gate", false, "Fail the command when automated evaluation quality checks fail")
	Cmd.Flags().StringVar(&evalReport, "eval-report", "", "Optional JSON path for automated evaluation results")
}

func run(cmd *cobra.Command, args []string) error {
	paths := append([]string(nil), notePaths...)
	paths = append(paths, args...)
	if notesGlob != "" {
		matches, err := filepath.Glob(notesGlob)
		if err != nil {
			return err
		}
		paths = append(paths, matches...)
	}
	if len(paths) == 0 {
		return fmt.Errorf("provide note files, --note, or --notes-glob")
	}
	paths = dedupePaths(paths)

	client, err := buildClient(agentMode)
	if err != nil {
		return err
	}
	cfg := botpkg.ReplayConfig()
	cfg.WorkDir = workDir
	cfg.FastAnswerModel = fastModel
	cfg.ResearchModel = researchModel
	cfg.CodeResearchModel = codeModel
	cfg.WebResearchModel = webModel
	cfg.SummaryModel = summaryModel
	cfg.MaxResearchTopics = maxTopics
	cfg.MaxSnippetsPerPrompt = maxSnippets
	cfg.FastAnswerTimeout = answerTimeout
	cfg.ResearchTimeout = researchTimeout
	cfg.SummaryTimeout = summaryTimeout
	cfg.FastAnswerEffort = agent.EffortLow
	cfg.ResearchEffort = agent.EffortMedium
	cfg.SummaryEffort = agent.EffortHigh
	if err := cfg.Validate(); err != nil {
		return err
	}

	interactions := make([]botpkg.Interaction, 0, len(questions))
	for _, q := range questions {
		trimmed := strings.TrimSpace(q)
		if trimmed != "" {
			interactions = append(interactions, botpkg.Interaction{Question: trimmed})
		}
	}
	if evaluate || len(interactions) == 0 {
		interactions = append(botpkg.DefaultInteractions(), interactions...)
	}

	ctx := cmd.Context()
	results := make([]botpkg.FileEvaluation, 0, len(paths))
	for _, path := range paths {
		result, err := botpkg.EvaluateFile(ctx, path, client, cfg, interactions)
		if err != nil {
			return err
		}
		printEvaluation(result, latencyBudget)
		results = append(results, result)
	}

	var gate botpkg.QualityGateResult
	if qualityGate || evalReport != "" {
		gate = botpkg.EvaluateQualityGate(results, botpkg.DefaultQualityGateConfig(cfg, latencyBudget))
		printQualityGate(gate)
	}
	if evalReport != "" {
		if err := writeEvalReport(evalReport, results, gate); err != nil {
			return err
		}
	}
	if qualityGate && !gate.Passed {
		return fmt.Errorf("meetingbot quality gate failed")
	}
	return nil
}

func buildClient(mode string) (botpkg.AgentClient, error) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "local", "offline", "":
		// Fail closed: an absent or empty mode resolves to the local client,
		// matching the --agent flag default. Selecting the real provider only
		// happens on an explicit "real" so a blank value cannot silently
		// trigger network calls or require credentials.
		return botpkg.LocalAgentClient{}, nil
	case "real":
		return botpkg.ProviderAgentClient{}, nil
	default:
		return nil, fmt.Errorf("unknown --agent %q: use real or local", mode)
	}
}

func printEvaluation(result botpkg.FileEvaluation, budget time.Duration) {
	fmt.Fprintf(os.Stdout, "\n# %s\n", result.Path)
	fmt.Fprintf(os.Stdout, "events=%d topics=", result.Events)
	for i, topic := range result.Topics {
		if i > 0 {
			fmt.Fprint(os.Stdout, ", ")
		}
		fmt.Fprintf(os.Stdout, "%s(%d)", topic.Name, topic.Score)
	}
	fmt.Fprintln(os.Stdout)

	for i := range result.Interactions {
		r := &result.Interactions[i]
		status := "ok"
		if r.Answer.OpeningReadinessLatency > budget {
			status = "slow"
		}
		fmt.Fprintf(os.Stdout, "\n## Interaction %d [%s opening=%s total=%s status=%s]\n", i+1, status, r.Answer.OpeningReadinessLatency.Round(time.Millisecond), r.TotalLatency.Round(time.Millisecond), r.Answer.Status)
		if r.Answer.Error != "" {
			fmt.Fprintf(os.Stdout, "model_error: %s\n", r.Answer.Error)
		}
		fmt.Fprintf(os.Stdout, "Q: %s\n\n%s\n", r.Answer.Question, r.Answer.Text)
	}
	fmt.Fprintf(os.Stdout, "\n## Summary [%s status=%s]\n", result.Summary.Latency.Round(time.Millisecond), result.Summary.Status)
	if result.Summary.Error != "" {
		fmt.Fprintf(os.Stdout, "model_error: %s\n", result.Summary.Error)
	}
	fmt.Fprintf(os.Stdout, "%s\n", result.Summary.Text)
}

func printQualityGate(gate botpkg.QualityGateResult) {
	status := "PASS"
	if !gate.Passed {
		status = "FAIL"
	}
	fmt.Fprintf(os.Stdout, "\n## Quality Gate: %s\n", status)
	for _, check := range gate.Checks {
		fmt.Fprintf(os.Stdout, "- [%s] %s: %s\n", check.Status, check.Name, check.Detail)
	}
}

type reportFile struct {
	GeneratedAt time.Time                `json:"generated_at"`
	Results     []botpkg.FileEvaluation  `json:"results"`
	Gate        botpkg.QualityGateResult `json:"gate"`
}

func writeEvalReport(path string, results []botpkg.FileEvaluation, gate botpkg.QualityGateResult) error {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	data, err := json.MarshalIndent(reportFile{
		GeneratedAt: time.Now(),
		Results:     results,
		Gate:        gate,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0o644)
}

func dedupePaths(paths []string) []string {
	seen := make(map[string]struct{}, len(paths))
	out := make([]string, 0, len(paths))
	for _, path := range paths {
		path = strings.TrimSpace(path)
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		out = append(out, path)
	}
	return out
}

// Execute is useful for focused command tests.
func Execute(ctx context.Context, args ...string) error {
	cmd := *Cmd
	cmd.SetArgs(args)
	cmd.SetContext(ctx)
	return cmd.Execute()
}
