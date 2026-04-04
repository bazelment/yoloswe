package jiradozer

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/bazelment/yoloswe/jiradozer/tracker"
	"github.com/bazelment/yoloswe/wt"
)

// PRResult holds the outcome of PR creation.
type PRResult struct {
	URL    string
	Number int
}

// CreatePR creates a GitHub pull request for the issue's changes.
func CreatePR(ctx context.Context, issue *tracker.Issue, baseBranch, workDir string, logger *slog.Logger) (*PRResult, error) {
	runner := &wt.DefaultGHRunner{}

	if err := wt.CheckGitHubAuth(ctx, runner); err != nil {
		return nil, err
	}

	title := fmt.Sprintf("[%s] %s", issue.Identifier, issue.Title)
	body := buildPRBody(issue)
	headBranch := determineBranch(issue)

	logger.Info("creating PR", "head", headBranch, "base", baseBranch)

	pr, err := wt.CreatePR(ctx, runner, title, body, baseBranch, headBranch, false, workDir)
	if err != nil {
		return nil, fmt.Errorf("create PR: %w", err)
	}

	logger.Info("PR created", "number", pr.Number, "url", pr.URL)
	return &PRResult{
		Number: pr.Number,
		URL:    pr.URL,
	}, nil
}

func determineBranch(issue *tracker.Issue) string {
	if issue.BranchName != nil && *issue.BranchName != "" {
		return *issue.BranchName
	}
	// Generate from identifier: ENG-123 -> eng-123
	return strings.ToLower(issue.Identifier)
}

func buildPRBody(issue *tracker.Issue) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n\n", issue.Title)

	if issue.URL != nil {
		fmt.Fprintf(&b, "Linear: %s\n\n", *issue.URL)
	}

	if issue.Description != nil && *issue.Description != "" {
		desc := *issue.Description
		if len(desc) > 500 {
			desc = desc[:500] + "..."
		}
		fmt.Fprintf(&b, "### Description\n\n%s\n", desc)
	}

	return b.String()
}
