package wt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
)

// GHRunner executes gh CLI commands.
type GHRunner interface {
	Run(ctx context.Context, args []string, dir string) (*CmdResult, error)
}

// DefaultGHRunner implements GHRunner using os/exec.
type DefaultGHRunner struct{}

// Run executes a gh command.
func (r *DefaultGHRunner) Run(ctx context.Context, args []string, dir string) (*CmdResult, error) {
	cmd := exec.CommandContext(ctx, "gh", args...)
	if dir != "" {
		cmd.Dir = dir
	}

	stdout, err := cmd.Output()
	result := &CmdResult{
		Stdout: string(stdout),
	}

	if exitErr, ok := err.(*exec.ExitError); ok {
		result.Stderr = string(exitErr.Stderr)
		result.ExitCode = exitErr.ExitCode()
		return result, err
	}

	return result, err
}

// ErrGitHubAuthRequired indicates that GitHub authentication is needed.
var ErrGitHubAuthRequired = errors.New("GitHub authentication required: run 'gh auth login' to authenticate")

// CheckGitHubAuth verifies that the user is authenticated with GitHub CLI.
func CheckGitHubAuth(ctx context.Context, runner GHRunner) error {
	_, err := runner.Run(ctx, []string{"auth", "status"}, "")
	if err != nil {
		return fmt.Errorf("%w: %w", ErrGitHubAuthRequired, err)
	}
	return nil
}

// IsAuthError checks if command output indicates an authentication failure.
func IsAuthError(stderr string) bool {
	lower := strings.ToLower(stderr)
	patterns := []string{
		"could not authenticate",
		"authorization failed",
		"authentication required",
		"invalid credentials",
		"could not read username",
		"terminal prompts disabled",
		"authentication token",
		"bad credentials",
	}
	for _, p := range patterns {
		if strings.Contains(lower, p) {
			return true
		}
	}
	return false
}

// wrapAuthError wraps an error with a helpful auth message if the stderr
// indicates an authentication failure.
func wrapAuthError(err error, result *CmdResult) error {
	if result != nil && IsAuthError(result.Stderr) {
		return fmt.Errorf("%w: %w", ErrGitHubAuthRequired, err)
	}
	return err
}

// PRInfo holds GitHub PR information.
type PRInfo struct {
	URL            string `json:"url"`
	HeadRefName    string `json:"headRefName"`
	BaseRefName    string `json:"baseRefName"`
	State          string `json:"state"` // OPEN, CLOSED, MERGED
	ReviewDecision string `json:"reviewDecision"`
	Number         int    `json:"number"`
	IsDraft        bool   `json:"isDraft"`
}

// StatusCheck represents a CI status check.
type StatusCheck struct {
	State string `json:"state"` // SUCCESS, FAILURE, PENDING
}

// IsMergeable returns true if the PR is approved and all checks pass.
func (p *PRInfo) IsMergeable() bool {
	return p.ReviewDecision == "APPROVED"
}

// GetPRForBranch fetches PR information for the current branch.
func GetPRForBranch(ctx context.Context, runner GHRunner, dir string) (*PRInfo, error) {
	result, err := runner.Run(ctx, []string{
		"pr", "view",
		"--json", "number,url",
	}, dir)
	if err != nil {
		return nil, err
	}

	var info PRInfo
	if err := json.Unmarshal([]byte(result.Stdout), &info); err != nil {
		return nil, err
	}

	return &info, nil
}

// GetPRByBranch fetches PR information for a specific branch.
func GetPRByBranch(ctx context.Context, runner GHRunner, branch, dir string) (*PRInfo, error) {
	result, err := runner.Run(ctx, []string{
		"pr", "view", branch,
		"--json", "number,url,headRefName,baseRefName,state,reviewDecision",
	}, dir)
	if err != nil {
		if result != nil && result.Stderr != "" {
			return nil, fmt.Errorf("%w: %s", err, result.Stderr)
		}
		return nil, err
	}

	var info PRInfo
	if err := json.Unmarshal([]byte(result.Stdout), &info); err != nil {
		return nil, err
	}

	return &info, nil
}

// ListOpenPRs returns all open PRs in the repository with full details.
// Uses --limit 1000 to avoid gh's default cap of 30 results.
func ListOpenPRs(ctx context.Context, runner GHRunner, dir string) ([]PRInfo, error) {
	result, err := runner.Run(ctx, []string{
		"pr", "list",
		"--json", "number,headRefName,baseRefName,state,isDraft,reviewDecision,url",
		"--state", "open",
		"--limit", "1000",
	}, dir)
	if err != nil {
		return nil, err
	}

	var prs []PRInfo
	if err := json.Unmarshal([]byte(result.Stdout), &prs); err != nil {
		return nil, err
	}

	return prs, nil
}

// ListMergedPRs returns recently merged PRs in the repository.
// Uses --limit 200 to cover typical worktree counts without excessive pagination.
func ListMergedPRs(ctx context.Context, runner GHRunner, dir string) ([]PRInfo, error) {
	result, err := runner.Run(ctx, []string{
		"pr", "list",
		"--json", "number,headRefName,baseRefName,state,url",
		"--state", "merged",
		"--limit", "200",
	}, dir)
	if err != nil {
		return nil, err
	}

	var prs []PRInfo
	if err := json.Unmarshal([]byte(result.Stdout), &prs); err != nil {
		return nil, err
	}

	return prs, nil
}

// UpdatePRBase changes the base branch of a PR.
func UpdatePRBase(ctx context.Context, runner GHRunner, prNumber int, newBase, dir string) error {
	_, err := runner.Run(ctx, []string{
		"pr", "edit", strconv.Itoa(prNumber),
		"--base", newBase,
	}, dir)
	return err
}

// IsPRMerged checks if the PR for a branch is merged.
func IsPRMerged(ctx context.Context, runner GHRunner, branch, dir string) (bool, error) {
	info, err := GetPRByBranch(ctx, runner, branch, dir)
	if err != nil {
		return false, err
	}
	return info.State == "MERGED", nil
}

// createPRResponse maps the JSON returned by the GitHub REST API for PR creation.
type createPRResponse struct {
	HTMLURL string `json:"html_url"`
	Head    struct {
		Ref string `json:"ref"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Number int  `json:"number"`
	Draft  bool `json:"draft"`
}

// CreatePR creates a new GitHub PR using the REST API (gh api).
// The head parameter is the branch name containing the changes.
// The dir parameter is used by gh to resolve {owner}/{repo} from git remotes.
func CreatePR(ctx context.Context, runner GHRunner, title, body, base, head string, draft bool, dir string) (*PRInfo, error) {
	args := []string{
		"api", "repos/{owner}/{repo}/pulls",
		"-f", "title=" + title,
		"-f", "body=" + body,
		"-f", "head=" + head,
		"-f", "base=" + base,
	}
	if draft {
		args = append(args, "-F", "draft=true")
	}

	result, err := runner.Run(ctx, args, dir)
	if err != nil {
		if result != nil && result.Stderr != "" {
			return nil, fmt.Errorf("%w: %s", err, result.Stderr)
		}
		return nil, err
	}

	var resp createPRResponse
	if err := json.Unmarshal([]byte(result.Stdout), &resp); err != nil {
		return nil, fmt.Errorf("parse PR response: %w", err)
	}

	return &PRInfo{
		Number:      resp.Number,
		URL:         resp.HTMLURL,
		HeadRefName: resp.Head.Ref,
		BaseRefName: resp.Base.Ref,
		State:       "OPEN",
		IsDraft:     resp.Draft,
	}, nil
}
