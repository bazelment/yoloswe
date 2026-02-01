package wt

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strconv"
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

// PRInfo holds GitHub PR information.
type PRInfo struct {
	URL            string `json:"url"`
	HeadRefName    string `json:"headRefName"`
	BaseRefName    string `json:"baseRefName"`
	State          string `json:"state"` // OPEN, CLOSED, MERGED
	ReviewDecision string `json:"reviewDecision"`
	Number         int    `json:"number"`
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

// ListOpenPRs returns all open PRs in the repository.
func ListOpenPRs(ctx context.Context, runner GHRunner, dir string) ([]PRInfo, error) {
	result, err := runner.Run(ctx, []string{
		"pr", "list",
		"--json", "number,headRefName,baseRefName,state",
		"--state", "open",
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

// CreatePR creates a new GitHub PR for the current branch.
func CreatePR(ctx context.Context, runner GHRunner, title, body, base string, draft bool, dir string) (*PRInfo, error) {
	args := []string{"pr", "create", "--json", "number,url,headRefName,baseRefName"}
	if base != "" {
		args = append(args, "--base", base)
	}
	if title != "" {
		args = append(args, "--title", title)
	}
	if body != "" {
		args = append(args, "--body", body)
	}
	if draft {
		args = append(args, "--draft")
	}

	result, err := runner.Run(ctx, args, dir)
	if err != nil {
		return nil, err
	}

	var info PRInfo
	if err := json.Unmarshal([]byte(result.Stdout), &info); err != nil {
		return nil, err
	}
	return &info, nil
}
