package wt

import (
	"context"
	"encoding/json"
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
	Number      int    `json:"number"`
	URL         string `json:"url"`
	HeadRefName string `json:"headRefName"`
}

// GetPRInfo fetches PR information from GitHub.
func GetPRInfo(ctx context.Context, runner GHRunner, prNumber int, dir string) (*PRInfo, error) {
	result, err := runner.Run(ctx, []string{
		"pr", "view", strconv.Itoa(prNumber),
		"--json", "number,url,headRefName",
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
