// Package github provides GitHub Actions CI data access via the gh CLI.
package github

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/bazelment/yoloswe/wt"
)

// Client wraps the gh CLI for GitHub Actions data.
type Client struct {
	gh  wt.GHRunner
	dir string
}

// NewClient creates a Client that runs gh commands in dir.
func NewClient(gh wt.GHRunner, dir string) *Client {
	return &Client{gh: gh, dir: dir}
}

// WorkflowRun represents a GitHub Actions workflow run.
type WorkflowRun struct {
	CreatedAt  time.Time `json:"createdAt"`
	Name       string    `json:"name"`
	Status     string    `json:"status"`
	Conclusion string    `json:"conclusion"`
	HeadBranch string    `json:"headBranch"`
	HeadSHA    string    `json:"headSha"`
	URL        string    `json:"url"`
	ID         int64     `json:"databaseId"`
}

// JobResult represents a single job within a workflow run.
type JobResult struct {
	Name       string `json:"name"`
	Conclusion string `json:"conclusion"`
	ID         int64  `json:"databaseId"`
	RunID      int64  `json:"-"`
}

// jobsResponse is the JSON structure returned by `gh run view --json jobs`.
type jobsResponse struct {
	Jobs []JobResult `json:"jobs"`
}

// Annotation represents a check-run annotation (error/warning from CI).
type Annotation struct {
	Path      string `json:"path"`
	Level     string `json:"annotation_level"`
	Message   string `json:"message"`
	Title     string `json:"title"`
	JobName   string `json:"-"` // set by caller
	StartLine int    `json:"start_line"`
	EndLine   int    `json:"end_line"`
}

// ListFailedRuns returns recent failed workflow runs for the given branch.
func (c *Client) ListFailedRuns(ctx context.Context, branch string, limit int) ([]WorkflowRun, error) {
	if limit <= 0 {
		limit = 10
	}
	result, err := c.gh.Run(ctx, []string{
		"run", "list",
		"--branch", branch,
		"--status", "failure",
		"--json", "databaseId,name,status,conclusion,headBranch,headSha,url,createdAt",
		"--limit", fmt.Sprintf("%d", limit),
	}, c.dir)
	if err != nil {
		return nil, fmt.Errorf("gh run list: %w", err)
	}

	var runs []WorkflowRun
	if err := json.Unmarshal([]byte(result.Stdout), &runs); err != nil {
		return nil, fmt.Errorf("parse run list: %w", err)
	}
	return runs, nil
}

// GetJobsForRun returns the jobs for a specific workflow run.
func (c *Client) GetJobsForRun(ctx context.Context, runID int64) ([]JobResult, error) {
	result, err := c.gh.Run(ctx, []string{
		"run", "view", fmt.Sprintf("%d", runID),
		"--json", "jobs",
	}, c.dir)
	if err != nil {
		return nil, fmt.Errorf("gh run view jobs: %w", err)
	}

	var resp jobsResponse
	if err := json.Unmarshal([]byte(result.Stdout), &resp); err != nil {
		return nil, fmt.Errorf("parse jobs: %w", err)
	}

	for i := range resp.Jobs {
		resp.Jobs[i].RunID = runID
	}
	return resp.Jobs, nil
}

// GetAnnotations returns annotations for all check-runs in a workflow run.
// It fetches jobs first, then annotations for each failed job.
func (c *Client) GetAnnotations(ctx context.Context, runID int64) ([]Annotation, error) {
	jobs, err := c.GetJobsForRun(ctx, runID)
	if err != nil {
		return nil, err
	}

	var all []Annotation
	for _, job := range jobs {
		if job.Conclusion != "failure" {
			continue
		}

		result, err := c.gh.Run(ctx, []string{
			"api",
			fmt.Sprintf("repos/{owner}/{repo}/check-runs/%d/annotations", job.ID),
		}, c.dir)
		if err != nil {
			continue // best-effort
		}

		var anns []Annotation
		if err := json.Unmarshal([]byte(result.Stdout), &anns); err != nil {
			continue
		}
		for i := range anns {
			anns[i].JobName = job.Name
		}
		all = append(all, anns...)
	}
	return all, nil
}

// GetJobLog returns the failed-job log output for a workflow run.
func (c *Client) GetJobLog(ctx context.Context, runID int64) (string, error) {
	result, err := c.gh.Run(ctx, []string{
		"run", "view", fmt.Sprintf("%d", runID),
		"--log-failed",
	}, c.dir)
	if err != nil {
		if result != nil {
			return result.Stdout, fmt.Errorf("gh run view --log-failed: %w", err)
		}
		return "", fmt.Errorf("gh run view --log-failed: %w", err)
	}
	return result.Stdout, nil
}
