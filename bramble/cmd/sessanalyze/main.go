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
)

type config struct { //nolint:govet // fieldalignment: readability over packing
	summaryWordLimit int
	sinceStr         string
	modelStr         string
	limit            int
	minTurns         int
	concurrency      int
	jsonOutput       bool
	verbose          bool
	listProjects     bool
	allProjects      bool
	summarize        bool
}

func main() {
	// Suppress noisy slog warnings from protocol parser (unknown message types
	// like "last-prompt", "custom-title" that don't affect analysis).
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{
		Level: slog.LevelError,
	})))

	cfg := parseFlags(os.Args[1:])

	if cfg.listProjects {
		listProjects()
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

// flags is the FlagSet used by parseFlags, stored at package level so that
// flag.NArg() / flag.Args() callers can reference it after parsing.
var flags = flag.NewFlagSet("sessanalyze", flag.ExitOnError)

func parseFlags(args []string) config {
	cfg := config{
		summaryWordLimit: 100,
	}
	flags.IntVar(&cfg.summaryWordLimit, "summary-limit", 100,
		"word limit before summarizing agent responses with Haiku (0=no summarization)")
	flags.BoolVar(&cfg.jsonOutput, "json", false, "output as JSON")
	flags.BoolVar(&cfg.verbose, "v", false, "show full agent responses (no truncation in display)")
	flags.BoolVar(&cfg.listProjects, "list", false, "list available projects")
	flags.StringVar(&cfg.sinceStr, "since", "", "filter sessions after this time (e.g. '2d', '24h', '2026-03-04')")
	flags.BoolVar(&cfg.allProjects, "all", false, "scan all projects under ~/.claude/projects/")
	flags.BoolVar(&cfg.summarize, "summarize", false, "use an LLM to generate session summaries")
	flags.StringVar(&cfg.modelStr, "model", "haiku", "model for summarization: haiku (default) or gemini")
	flags.IntVar(&cfg.limit, "n", 0, "limit to the N most recent sessions (0=no limit)")
	flags.IntVar(&cfg.minTurns, "min-turns", 0, "exclude sessions with fewer than N turns")
	flags.IntVar(&cfg.concurrency, "j", 10, "number of concurrent LLM summarization workers")
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
	// Try relative duration with day suffix.
	if strings.HasSuffix(s, "d") {
		days := strings.TrimSuffix(s, "d")
		var n int
		if _, err := fmt.Sscanf(days, "%d", &n); err == nil && n > 0 {
			return time.Now().AddDate(0, 0, -n), nil
		}
	}

	// Try Go duration (e.g. "24h", "2h30m").
	if d, err := time.ParseDuration(s); err == nil {
		return time.Now().Add(-d), nil
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
