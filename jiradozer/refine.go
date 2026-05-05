package jiradozer

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/bazelment/yoloswe/agent-cli-wrapper/claude/render"
	"github.com/bazelment/yoloswe/jiradozer/tracker"
	ghtracker "github.com/bazelment/yoloswe/jiradozer/tracker/github"
	"github.com/bazelment/yoloswe/wt"
)

// RefineOptions configures a refinement run against an existing PR worktree.
//
//nolint:govet // fieldalignment: public options stay grouped by call-site concern.
type RefineOptions struct {
	Issue       *tracker.Issue
	Tracker     tracker.IssueTracker
	Config      *Config
	Logger      *slog.Logger
	Renderer    *render.Renderer
	Feedback    string
	PRRef       string
	WorkDir     string
	BranchName  string
	NoPoll      bool
	GH          wt.GHRunner
	RunWorkflow func(*Workflow) error
}

// RunRefine locates the preserved worktree for issue, gathers PR feedback, and
// resumes the workflow at validation.
func RunRefine(ctx context.Context, opts RefineOptions) error {
	if opts.Issue == nil {
		return fmt.Errorf("refine requires an issue")
	}
	if opts.Tracker == nil {
		return fmt.Errorf("refine requires a tracker")
	}
	if opts.Config == nil {
		return fmt.Errorf("refine requires a config")
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	gh := opts.GH
	if gh == nil {
		gh = &wt.DefaultGHRunner{}
	}

	branch := opts.BranchName
	if branch == "" && opts.Issue.BranchName != nil {
		branch = *opts.Issue.BranchName
	}
	if branch == "" {
		branch = fmt.Sprintf("%s/%s", opts.Config.Source.BranchPrefix, opts.Issue.Identifier)
	}

	workDir := opts.WorkDir
	if workDir == "" {
		var err error
		workDir, err = findGitWorktreeForBranch(ctx, branch)
		if err != nil {
			return err
		}
	}

	feedback := strings.TrimSpace(opts.Feedback)
	if feedback == "" {
		prRef := opts.PRRef
		if prRef == "" {
			pr, err := findPRForBranch(ctx, gh, workDir, branch)
			if err != nil {
				return err
			}
			prRef = strconv.Itoa(pr.Number)
			logger.Info("found PR for refine", "issue", opts.Issue.Identifier, "branch", branch, "pr", pr.URL)
		}
		comments, err := ghtracker.FetchPRReviewComments(ctx, gh, workDir, prRef)
		if err != nil {
			return err
		}
		feedback = ghtracker.FormatPRReviewFeedback(comments)
	}
	if feedback == "" {
		return fmt.Errorf("no refinement feedback found; pass --feedback/--feedback-file or add PR review comments")
	}

	cfg := cloneConfig(opts.Config)
	cfg.WorkDir = workDir
	wf := NewWorkflow(opts.Tracker, opts.Issue, cfg, logger)
	wf.SetRenderer(opts.Renderer)
	wf.PrepareRefine(feedback)
	wf.SetStopAtReview(opts.NoPoll)
	if opts.RunWorkflow != nil {
		return opts.RunWorkflow(wf)
	}
	return wf.Run(ctx)
}

func findGitWorktreeForBranch(ctx context.Context, branch string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "worktree", "list", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("list git worktrees: %w", err)
	}
	worktrees := parseGitWorktreePorcelain(string(out))
	if p := worktrees[branch]; p != "" {
		return p, nil
	}
	return "", fmt.Errorf("worktree for branch %q not found; pass --work-dir pointing at the preserved worktree or recreate it with gh pr checkout", branch)
}

func parseGitWorktreePorcelain(output string) map[string]string {
	result := make(map[string]string)
	record := make(map[string]string)
	flush := func() {
		path := record["worktree"]
		branch := strings.TrimPrefix(record["branch"], "refs/heads/")
		if path != "" && branch != "" {
			result[branch] = path
		}
		record = make(map[string]string)
	}
	for _, line := range strings.Split(output, "\n") {
		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "worktree "):
			record["worktree"] = line[len("worktree "):]
		case strings.HasPrefix(line, "branch "):
			record["branch"] = line[len("branch "):]
		}
	}
	flush()
	return result
}

func findPRForBranch(ctx context.Context, gh wt.GHRunner, dir, branch string) (*wt.PRInfo, error) {
	result, err := gh.Run(ctx, []string{
		"pr", "list",
		"--head", branch,
		"--json", "number,url,headRefName",
		"--limit", "1",
	}, dir)
	if err != nil {
		return nil, fmt.Errorf("find PR for branch %q: %w", branch, err)
	}
	var prs []wt.PRInfo
	if err := json.Unmarshal([]byte(result.Stdout), &prs); err != nil {
		return nil, fmt.Errorf("parse PR list: %w", err)
	}
	if len(prs) == 0 {
		return nil, fmt.Errorf("no open PR found for branch %q; pass --pr or --feedback", branch)
	}
	return &prs[0], nil
}

// RefineFeedbackFromIssueComment returns the latest issue comment that starts
// with "refine:". Team mode uses this as a lightweight signal: move/label the
// issue so discovery sees it again, add "refine: <instructions>", and the
// preserved PR branch is revised in place.
func RefineFeedbackFromIssueComment(ctx context.Context, t tracker.IssueTracker, issueID string) (string, bool, error) {
	comments, err := t.FetchComments(ctx, issueID, timeZero())
	if err != nil {
		return "", false, err
	}
	for i := len(comments) - 1; i >= 0; i-- {
		body := strings.TrimSpace(comments[i].Body)
		prefix, rest, ok := strings.Cut(body, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(prefix), "refine") {
			continue
		}
		return strings.TrimSpace(rest), true, nil
	}
	return "", false, nil
}

func timeZero() time.Time {
	return time.Time{}
}
