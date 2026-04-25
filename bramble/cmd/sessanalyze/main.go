// Command sessanalyze analyzes Claude Code session history from JSONL files.
//
// Usage:
//
//	sessanalyze [flags] [project-dir-or-jsonl-file ...]
//
// If no paths are given, lists available projects under ~/.claude/projects/.
// If a directory is given, analyzes all JSONL sessions in it.
// If JSONL file(s) are given, analyzes those specific sessions.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/bramble/sessionanalysis"
	"github.com/bazelment/yoloswe/logging/klogfmt"
)

type config struct { //nolint:govet // fieldalignment: readability over packing
	summaryWordLimit int
	sinceStr         string
	untilStr         string
	pricingFile      string
	modelStr         string
	limit            int
	statsMaxRows     int
	minTurns         int
	concurrency      int
	jsonOutput       bool
	verbose          bool
	listProjects     bool
	allProjects      bool
	summarize        bool
	stats            bool
	topLevelOnly     bool
}

func main() {
	// Suppress noisy slog warnings from protocol parser (unknown message types
	// like "last-prompt", "custom-title" that don't affect analysis).
	klogfmt.Init(klogfmt.WithLevel(slog.LevelError))

	cfg := parseFlags(os.Args[1:])

	if cfg.listProjects {
		listProjects()
		return
	}

	if cfg.stats {
		if err := runStats(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		return
	}

	analysisCfg := sessionanalysis.Config{
		SkipEmpty: true,
		MinTurns:  cfg.minTurns,
	}

	if cfg.sinceStr != "" {
		since, err := parseSince(cfg.sinceStr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid --since value: %v\n", err)
			os.Exit(2)
		}
		analysisCfg.Since = since
	}

	var queryFunc sessionanalysis.QueryFunc
	if cfg.summarize {
		queryFunc = buildQueryFunc(cfg.modelStr)
	}

	if cfg.allProjects {
		sessions, err := sessionanalysis.ParseAllProjects(analysisCfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error: %v\n", err)
			os.Exit(1)
		}
		sessions = limitSessions(sessions, cfg.limit)
		if cfg.summarize {
			summarizeWithProgress(sessions, cfg.summaryWordLimit, queryFunc, cfg.concurrency)
		}
		render(os.Stdout, sessions, cfg)
		return
	}

	if flags.NArg() == 0 {
		listProjects()
		return
	}

	exitCode := 0
	var allSessions []*sessionanalysis.Session
	for _, path := range flags.Args() {
		sessions, err := parsePath(path, analysisCfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			exitCode = 1
			continue
		}
		allSessions = append(allSessions, sessions...)
	}
	allSessions = limitSessions(allSessions, cfg.limit)
	if cfg.summarize {
		summarizeWithProgress(allSessions, cfg.summaryWordLimit, queryFunc, cfg.concurrency)
	}
	render(os.Stdout, allSessions, cfg)
	os.Exit(exitCode)
}

var flags = flag.NewFlagSet("sessanalyze", flag.ExitOnError)

func parseFlags(args []string) config {
	cfg := config{
		summaryWordLimit: 100,
		statsMaxRows:     25,
	}
	flags.IntVar(&cfg.summaryWordLimit, "summary-limit", 100,
		"word limit before summarizing agent responses with Haiku (0=no summarization)")
	flags.BoolVar(&cfg.jsonOutput, "json", false, "output as JSON")
	flags.BoolVar(&cfg.verbose, "v", false, "show full agent responses (no truncation in display)")
	flags.BoolVar(&cfg.listProjects, "list", false, "list available projects")
	flags.StringVar(&cfg.sinceStr, "since", "", "filter sessions after this time (e.g. '2d', '24h', '2026-03-04')")
	flags.StringVar(&cfg.untilStr, "until", "", "filter sessions before this time (e.g. '2026-04-23T12:00:00Z'); stats mode only")
	flags.BoolVar(&cfg.allProjects, "all", false, "scan all projects under ~/.claude/projects/")
	flags.BoolVar(&cfg.summarize, "summarize", false, "use an LLM to generate session summaries")
	flags.StringVar(&cfg.modelStr, "model", "haiku", "model for summarization: haiku (default) or gemini")
	flags.StringVar(&cfg.pricingFile, "pricing-file", "", "JSON file with model pricing table for estimated cost")
	flags.IntVar(&cfg.limit, "n", 0, "limit to the N most recent sessions (0=no limit)")
	flags.IntVar(&cfg.statsMaxRows, "max-rows", 25, "max rows per stats breakdown table")
	flags.IntVar(&cfg.minTurns, "min-turns", 0, "exclude sessions with fewer than N turns")
	flags.IntVar(&cfg.concurrency, "j", 10, "number of concurrent LLM summarization workers")
	flags.BoolVar(&cfg.stats, "stats", false, "show usage/cost stats instead of per-session transcripts")
	flags.BoolVar(&cfg.topLevelOnly, "top-level-only", false, "in --stats mode, exclude subagent logs")
	flags.Parse(args) //nolint:errcheck // ExitOnError mode handles errors
	return cfg
}

func buildQueryFunc(model string) sessionanalysis.QueryFunc {
	switch model {
	case "gemini":
		return sessionanalysis.GeminiQueryFunc()
	default:
		return sessionanalysis.HaikuQueryFunc()
	}
}

func runStats(cfg config) error {
	statsCfg := sessionanalysis.DefaultStatsConfig()
	statsCfg.IncludeSubagents = !cfg.topLevelOnly

	now := time.Now()

	if cfg.sinceStr != "" {
		since, err := parseTimeBound(cfg.sinceStr, now)
		if err != nil {
			return fmt.Errorf("invalid --since value: %w", err)
		}
		statsCfg.Since = since
	}
	if cfg.untilStr != "" {
		until, err := parseTimeBound(cfg.untilStr, now)
		if err != nil {
			return fmt.Errorf("invalid --until value: %w", err)
		}
		statsCfg.Until = until
	}
	if !statsCfg.Since.IsZero() && !statsCfg.Until.IsZero() && statsCfg.Since.After(statsCfg.Until) {
		return fmt.Errorf("--since must be before --until")
	}

	if cfg.pricingFile != "" {
		tbl, err := sessionanalysis.LoadPricingTable(cfg.pricingFile)
		if err != nil {
			return fmt.Errorf("load pricing file: %w", err)
		}
		statsCfg.Pricing = tbl
	}

	var paths []string
	if cfg.allProjects {
		projects, err := sessionanalysis.ListProjects()
		if err != nil {
			return err
		}
		for i := range projects {
			paths = append(paths, projects[i].Path)
		}
	} else {
		if flags.NArg() == 0 {
			listProjects()
			return nil
		}
		paths = append(paths, flags.Args()...)
	}

	report, err := sessionanalysis.AnalyzeUsageStats(paths, statsCfg)
	if err != nil {
		return err
	}

	if cfg.jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(report)
	}
	renderStatsMarkdown(os.Stdout, report, cfg.statsMaxRows)
	return nil
}

func summarizeWithProgress(sessions []*sessionanalysis.Session, wordLimit int, query sessionanalysis.QueryFunc, concurrency int) {
	ctx := context.Background()
	fmt.Fprintf(os.Stderr, "Summarizing %d sessions (concurrency=%d)...\n", len(sessions), concurrency)
	sessionanalysis.ConcurrentSummarizeSessions(ctx, sessions, query, concurrency,
		func(done, total int) {
			fmt.Fprintf(os.Stderr, "\rSummarized %d/%d sessions", done, total)
		},
	)
	fmt.Fprintln(os.Stderr)
	// Summarize long turn responses.
	if wordLimit > 0 {
		fmt.Fprintf(os.Stderr, "Summarizing long responses (>%d words)...\n", wordLimit)
		sessionanalysis.ConcurrentSummarizeTurns(ctx, sessions, wordLimit, query, concurrency)
	}
}

func render(w io.Writer, sessions []*sessionanalysis.Session, cfg config) {
	if cfg.jsonOutput {
		renderJSON(w, sessions)
	} else {
		renderMarkdown(w, sessions, cfg)
	}
}

func renderStatsMarkdown(w io.Writer, report *sessionanalysis.StatsReport, maxRows int) {
	if maxRows <= 0 {
		maxRows = 25
	}

	fmt.Fprintln(w, "# Usage Stats")
	fmt.Fprintln(w)

	if !report.Since.IsZero() || !report.Until.IsZero() {
		fmt.Fprintf(w, "- Window: %s → %s\n", formatBound(report.Since), formatBound(report.Until))
	} else {
		fmt.Fprintln(w, "- Window: all events")
	}
	fmt.Fprintf(w, "- Files scanned: %d\n", report.FilesScanned)
	fmt.Fprintf(w, "- Events scanned: %d (parse errors: %d)\n", report.EventsScanned, report.ParseErrors)
	fmt.Fprintf(w, "- Pricing: %s (%s)\n",
		strings.ReplaceAll(report.Pricing.Version, "|", "\\|"),
		strings.ReplaceAll(report.Pricing.Source, "|", "\\|"))
	fmt.Fprintln(w)

	fmt.Fprintln(w, "## Totals")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "| Metric | Value |")
	fmt.Fprintln(w, "|---|---:|")
	fmt.Fprintf(w, "| Sessions | %d |\n", report.Total.Sessions)
	fmt.Fprintf(w, "| Usage Events | %d |\n", report.Total.UsageEvents)
	fmt.Fprintf(w, "| Tool Uses | %d |\n", report.Total.ToolUses)
	fmt.Fprintf(w, "| Input Tokens | %s |\n", formatInt(report.Total.Usage.InputTokens))
	fmt.Fprintf(w, "| Output Tokens | %s |\n", formatInt(report.Total.Usage.OutputTokens))
	fmt.Fprintf(w, "| Cache Read Tokens | %s |\n", formatInt(report.Total.Usage.CacheReadInputTokens))
	fmt.Fprintf(w, "| Cache Creation Tokens | %s |\n", formatInt(report.Total.Usage.CacheCreationInputTokens))
	fmt.Fprintf(w, "| Observed Cost (USD) | $%.4f |\n", report.Total.ObservedCostUSD)
	fmt.Fprintf(w, "| Estimated Cost (USD) | $%.4f |\n", report.Total.EstimatedCostUSD)
	fmt.Fprintf(w, "| Estimated Coverage | %.1f%% |\n", report.Total.Coverage()*100)
	fmt.Fprintln(w)

	renderBucketTable(w, "By Family", []string{"Family"}, report.ByFamily, maxRows,
		func(b sessionanalysis.FamilyBucket) ([]string, sessionanalysis.BucketStats) {
			return []string{b.Family}, b.BucketStats
		})

	renderBucketTable(w, "By Model", []string{"Model"}, report.ByModel, maxRows,
		func(b sessionanalysis.ModelBucket) ([]string, sessionanalysis.BucketStats) {
			return []string{b.Model}, b.BucketStats
		})

	renderBucketTable(w, "By Project", []string{"Project"}, report.ByProject, maxRows,
		func(b sessionanalysis.ProjectBucket) ([]string, sessionanalysis.BucketStats) {
			return []string{b.Project}, b.BucketStats
		})

	renderBucketTable(w, "By Project + Model", []string{"Project", "Model"}, report.ByProjectModel, maxRows,
		func(b sessionanalysis.ProjectModelBucket) ([]string, sessionanalysis.BucketStats) {
			return []string{b.Project, b.Model}, b.BucketStats
		})
}

// renderBucketTable writes a markdown breakdown table for a slice of bucket
// rows. labels are the leading label-column names (rendered with backticks);
// the remaining columns are the standard usage/cost stats. row returns the
// label cell values plus the BucketStats for each row.
func renderBucketTable[T any](
	w io.Writer,
	title string,
	labels []string,
	rows []T,
	maxRows int,
	row func(T) ([]string, sessionanalysis.BucketStats),
) {
	fmt.Fprintf(w, "## %s\n\n", title)

	statHeaders := []string{"Sessions", "Input", "Output", "Cache Read", "Cache Create", "Tool Uses", "Observed $", "Estimated $", "Coverage"}
	header := "|"
	sep := "|"
	for _, l := range labels {
		header += " " + l + " |"
		sep += "---|"
	}
	for _, h := range statHeaders {
		header += " " + h + " |"
		sep += "---:|"
	}
	fmt.Fprintln(w, header)
	fmt.Fprintln(w, sep)

	for _, r := range limitRows(rows, maxRows) {
		labelVals, stats := row(r)
		var b strings.Builder
		b.WriteByte('|')
		for _, v := range labelVals {
			// Sanitize control chars, backticks, and pipes to avoid breaking
			// the Markdown table structure.
			safe := strings.Map(func(r rune) rune {
				switch {
				case r == '`':
					return -1 // strip (breaks inline code span)
				case r == '|':
					return -1 // strip (breaks table column boundary; escaping isn't reliable)
				case r < 0x20 || r == 0x7f:
					return -1 // strip control characters
				}
				return r
			}, v)
			fmt.Fprintf(&b, " `%s` |", safe)
		}
		fmt.Fprintf(&b, " %d | %s | %s | %s | %s | %d | %.4f | %.4f | %.1f%% |",
			stats.Sessions,
			formatInt(stats.Usage.InputTokens),
			formatInt(stats.Usage.OutputTokens),
			formatInt(stats.Usage.CacheReadInputTokens),
			formatInt(stats.Usage.CacheCreationInputTokens),
			stats.ToolUses,
			stats.ObservedCostUSD,
			stats.EstimatedCostUSD,
			stats.Coverage()*100,
		)
		fmt.Fprintln(w, b.String())
	}
	if len(rows) > maxRows {
		fmt.Fprintf(w, "\n*Showing top %d of %d rows.*\n", maxRows, len(rows))
	}
	fmt.Fprintln(w)
}

func formatBound(t time.Time) string {
	if t.IsZero() {
		return "(unset)"
	}
	return t.UTC().Format(time.RFC3339)
}

func limitRows[T any](rows []T, n int) []T {
	if n <= 0 || len(rows) <= n {
		return rows
	}
	return rows[:n]
}

func formatInt(v int64) string {
	s := fmt.Sprintf("%d", v)
	if len(s) <= 3 {
		return s
	}
	sign := ""
	if s[0] == '-' {
		sign = "-"
		s = s[1:]
	}
	var out []byte
	rem := len(s) % 3
	if rem > 0 {
		out = append(out, s[:rem]...)
		if len(s) > rem {
			out = append(out, ',')
		}
	}
	for i := rem; i < len(s); i += 3 {
		out = append(out, s[i:i+3]...)
		if i+3 < len(s) {
			out = append(out, ',')
		}
	}
	return sign + string(out)
}

func listProjects() {
	projects, err := sessionanalysis.ListProjects()
	if err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
	if len(projects) == 0 {
		fmt.Println("No projects found in ~/.claude/projects/")
		return
	}
	fmt.Printf("| %-58s | %s |\n", "Project", "Sessions")
	fmt.Printf("|%s|%s|\n", strings.Repeat("-", 60), strings.Repeat("-", 10))
	for _, p := range projects {
		fmt.Printf("| %-58s | %-8d |\n", p.Name, p.SessionCount)
	}
}

func parsePath(path string, cfg sessionanalysis.Config) ([]*sessionanalysis.Session, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if info.IsDir() {
		return sessionanalysis.ParseProjectWithConfig(path, cfg)
	}
	sess, err := sessionanalysis.ParseSessionWithConfig(path, cfg)
	if err != nil {
		return nil, err
	}
	// Apply the same filters as ParseProjectWithConfig for consistency.
	if len(sess.Turns) == 0 {
		return nil, nil
	}
	if cfg.MinTurns > 0 && len(sess.Turns) < cfg.MinTurns {
		return nil, nil
	}
	if !cfg.Since.IsZero() && sess.StartTime.Before(cfg.Since) {
		return nil, nil
	}
	return []*sessionanalysis.Session{sess}, nil
}

func renderJSON(w io.Writer, sessions []*sessionanalysis.Session) {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.Encode(sessions) //nolint:errcheck
}

func renderMarkdown(w io.Writer, sessions []*sessionanalysis.Session, cfg config) {
	// Group by project, sort each group by end time.
	groups := make(map[string][]*sessionanalysis.Session)
	var projectOrder []string
	for _, sess := range sessions {
		proj := sess.Project
		if proj == "" {
			proj = "(unknown)"
		}
		if _, exists := groups[proj]; !exists {
			projectOrder = append(projectOrder, proj)
		}
		groups[proj] = append(groups[proj], sess)
	}

	// Sort projects alphabetically.
	sort.Strings(projectOrder)

	// Sort sessions within each project by end time.
	for _, proj := range projectOrder {
		sort.Slice(groups[proj], func(i, j int) bool {
			return groups[proj][i].EndTime.Before(groups[proj][j].EndTime)
		})
	}

	first := true
	for _, proj := range projectOrder {
		if !first {
			fmt.Fprintln(w)
		}
		first = false
		fmt.Fprintf(w, "# Project: `%s`\n\n", proj)
		fmt.Fprintf(w, "*%d sessions*\n\n---\n", len(groups[proj]))
		for _, sess := range groups[proj] {
			fmt.Fprintln(w)
			renderSessionMD(w, sess, cfg)
		}
	}
}

func renderSessionMD(w io.Writer, sess *sessionanalysis.Session, cfg config) {
	// Session header
	shortID := sess.ID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	fmt.Fprintf(w, "## Session `%s`\n\n", shortID)

	// Metadata table
	fmt.Fprintln(w, "| Field | Value |")
	fmt.Fprintln(w, "|-------|-------|")
	if !sess.StartTime.IsZero() {
		fmt.Fprintf(w, "| Start | %s |\n", sess.StartTime.Format(time.RFC3339))
	}
	if sess.Duration() > 0 {
		fmt.Fprintf(w, "| Duration | %s |\n", sess.Duration().Round(time.Second))
	}
	if sess.GitBranch != "" {
		fmt.Fprintf(w, "| Branch | `%s` |\n", sess.GitBranch)
	}
	if sess.CWD != "" {
		fmt.Fprintf(w, "| CWD | `%s` |\n", sess.CWD)
	}
	fmt.Fprintf(w, "| Turns | %d |\n", len(sess.Turns))
	fmt.Fprintln(w)

	// Summary
	if sess.Summary != "" {
		fmt.Fprintln(w, "**Summary:**", sess.Summary)
		fmt.Fprintln(w)
	}

	// Turns
	fmt.Fprintln(w, "---")
	for i := range sess.Turns {
		renderTurnMD(w, &sess.Turns[i], cfg)
	}
}

func renderTurnMD(w io.Writer, turn *sessionanalysis.Turn, cfg config) {
	fmt.Fprintln(w)

	// Turn header
	header := fmt.Sprintf("### Turn %d", turn.Number)
	if !turn.StartTime.IsZero() {
		header += fmt.Sprintf(" — %s", turn.StartTime.Format("15:04:05"))
	}
	if turn.DurationMs > 0 {
		header += fmt.Sprintf(" (%.1fs)", float64(turn.DurationMs)/1000)
	}
	fmt.Fprintln(w, header)
	fmt.Fprintln(w)

	// User input
	input := turn.UserInput
	if len(input) > 500 {
		input = input[:500] + "..."
	}
	input = strings.ReplaceAll(input, "\n", " ")
	fmt.Fprintf(w, "**User:** %s\n\n", input)

	// Agent response
	if turn.Response == "" && len(turn.ToolCalls) > 0 {
		fmt.Fprintf(w, "**Agent:** *[no text, %d tool calls]*\n", len(turn.ToolCalls))
	} else if cfg.verbose || turn.ResponseSummary == "" {
		response := turn.Response
		if !cfg.verbose && len(response) > 2000 {
			response = response[:2000] + "\n\n*[... truncated ...]*"
		}
		fmt.Fprintf(w, "**Agent:** %s\n", response)
	} else {
		fmt.Fprintf(w, "**Agent** *(%d words, summarized):*\n\n%s\n", turn.ResponseWordCount(), turn.ResponseSummary)
	}
	fmt.Fprintln(w)

	// Errors
	for _, e := range turn.Errors {
		fmt.Fprintf(w, "> **Error:** %s\n", e)
	}

	// Cost
	if turn.CostUSD > 0 {
		fmt.Fprintf(w, "> **Cost:** $%.4f\n", turn.CostUSD)
	}
}

// parseSince parses duration strings like "2d", "24h" or date strings like "2026-03-04".
func parseSince(s string) (time.Time, error) {
	return parseTimeBound(s, time.Now())
}

// parseTimeBound parses duration strings like "2d", "24h" or date strings.
// Relative values are interpreted as "now - duration".
func parseTimeBound(s string, now time.Time) (time.Time, error) {
	// Try relative duration with day suffix.
	if strings.HasSuffix(s, "d") {
		days := strings.TrimSuffix(s, "d")
		var n int
		if _, err := fmt.Sscanf(days, "%d", &n); err == nil && n > 0 {
			return now.AddDate(0, 0, -n), nil
		}
	}

	// Try Go duration (e.g. "24h", "2h30m").
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}

	// Try date formats.
	for _, layout := range []string{"2006-01-02", "2006-01-02T15:04:05", time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t, nil
		}
	}

	return time.Time{}, fmt.Errorf("cannot parse %q (try '2d', '24h', or '2026-03-04')", s)
}

// limitSessions returns the last n sessions (most recent). Sessions are assumed
// to be sorted by start time. If n <= 0, returns all sessions.
func limitSessions(sessions []*sessionanalysis.Session, n int) []*sessionanalysis.Session {
	if n <= 0 || n >= len(sessions) {
		return sessions
	}
	return sessions[len(sessions)-n:]
}
