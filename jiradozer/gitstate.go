package jiradozer

import (
	"context"
	"fmt"
	"strings"

	"github.com/bazelment/yoloswe/wt"
)

var workflowGitRunner wt.GitRunner = &wt.DefaultGitRunner{}

func worktreeIsDirty(ctx context.Context, workDir string) (bool, error) {
	manager := wt.NewManager("", "", wt.WithGitRunner(workflowGitRunner))
	status, err := manager.GetGitStatus(ctx, wt.Worktree{Path: workDir})
	if err != nil {
		return false, err
	}
	return status.IsDirty, nil
}

func gitDiffHasChanges(ctx context.Context, workDir string, base string) (bool, error) {
	result, err := workflowGitRunner.Run(ctx, []string{"diff", "--quiet", base + "...HEAD"}, workDir)
	if err == nil {
		return false, nil
	}
	if result != nil && result.ExitCode == 1 {
		return true, nil
	}
	return false, gitError(err, result)
}

func gitError(err error, result *wt.CmdResult) error {
	if result == nil || strings.TrimSpace(result.Stderr) == "" {
		return err
	}
	return fmt.Errorf("%w: %s", err, strings.TrimSpace(result.Stderr))
}
