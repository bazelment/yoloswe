package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	chatui "github.com/bazelment/yoloswe/orca/chat"
)

// ghPRTimeout bounds one `gh pr view`.
const ghPRTimeout = 20 * time.Second

func ghAvailable() bool { _, err := exec.LookPath("gh"); return err == nil }

// ghPRStatus reads a PR's state, draft flag and head checks with `gh pr view`.
// Without repo, gh resolves the repository from the checkout at dir.
func ghPRStatus(ctx context.Context, repo, dir, number string) (chatui.PRStatus, error) {
	ctx, cancel := context.WithTimeout(ctx, ghPRTimeout)
	defer cancel()
	args := []string{"pr", "view", number, "--json", "state,isDraft,statusCheckRollup"}
	if repo != "" {
		args = append(args, "--repo", repo)
	}
	cmd := exec.CommandContext(ctx, "gh", args...)
	if repo == "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			if line, _, _ := strings.Cut(strings.TrimSpace(string(exit.Stderr)), "\n"); line != "" {
				return chatui.PRStatus{}, fmt.Errorf("gh pr view %s: %s", number, line)
			}
		}
		return chatui.PRStatus{}, fmt.Errorf("gh pr view %s: %w", number, err)
	}
	return parseGHPR(out)
}

// ghCheck is one statusCheckRollup entry: a CheckRun (status, conclusion)
// or a StatusContext (state).
type ghCheck struct {
	Name       string `json:"name"`
	Context    string `json:"context"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}

func parseGHPR(b []byte) (chatui.PRStatus, error) {
	var v struct {
		State  string    `json:"state"`
		Checks []ghCheck `json:"statusCheckRollup"`
		Draft  bool      `json:"isDraft"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return chatui.PRStatus{}, fmt.Errorf("parse gh pr view: %w", err)
	}
	return chatui.PRStatus{State: v.State, Draft: v.Draft, Checks: summarizeChecks(v.Checks)}, nil
}

// summarizeChecks counts checks as passed, failed or pending and names the
// failed ones.
func summarizeChecks(checks []ghCheck) string {
	if len(checks) == 0 {
		return "none reported"
	}
	var passed, pending int
	var failed []string
	for _, c := range checks {
		result := c.Conclusion
		if result == "" {
			result = c.State
		}
		if c.Status != "" && c.Status != "COMPLETED" {
			result = "PENDING"
		}
		switch result {
		case "SUCCESS", "NEUTRAL", "SKIPPED":
			passed++
		case "PENDING", "EXPECTED", "QUEUED", "IN_PROGRESS", "":
			pending++
		default:
			failed = append(failed, firstNonEmpty(c.Name, c.Context, "check"))
		}
	}
	parts := []string{fmt.Sprintf("%d passed", passed)}
	if len(failed) > 0 {
		parts = append(parts, fmt.Sprintf("%d failed (%s)", len(failed), strings.Join(failed, ", ")))
	}
	if pending > 0 {
		parts = append(parts, fmt.Sprintf("%d pending", pending))
	}
	return strings.Join(parts, ", ")
}

func firstNonEmpty(v ...string) string {
	for _, s := range v {
		if s != "" {
			return s
		}
	}
	return ""
}
