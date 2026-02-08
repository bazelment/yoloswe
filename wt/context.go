package wt

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// WorktreeContext provides structured context about a worktree for agent consumption.
// When an agent runs in a worktree, this type automatically provides the tree's
// diffs, changed files, branch history, and PR status.
type WorktreeContext struct {
	// Identity
	Branch string `json:"branch"`
	Path   string `json:"path"`
	Goal   string `json:"goal,omitempty"`
	Parent string `json:"parent,omitempty"`

	// Git state
	DiffStat       string   `json:"diff_stat,omitempty"`
	DiffContent    string   `json:"diff_content,omitempty"`
	ChangedFiles   []string `json:"changed_files,omitempty"`
	UntrackedFiles []string `json:"untracked_files,omitempty"`

	// Branch state
	Ahead   int  `json:"ahead"`
	Behind  int  `json:"behind"`
	IsDirty bool `json:"is_dirty"`

	// Recent history
	RecentCommits []CommitInfo `json:"recent_commits,omitempty"`

	// PR context
	PRNumber int    `json:"pr_number,omitempty"`
	PRState  string `json:"pr_state,omitempty"`
	PRURL    string `json:"pr_url,omitempty"`

	// Metadata
	GatheredAt time.Time `json:"gathered_at"`
}

// CommitInfo holds information about a single commit.
type CommitInfo struct {
	Hash    string    `json:"hash"`
	Subject string    `json:"subject"`
	Author  string    `json:"author"`
	Date    time.Time `json:"date"`
}

// ContextOptions controls what data to gather for a WorktreeContext.
type ContextOptions struct {
	IncludeDiff     bool // Include full diff content (can be large)
	IncludeDiffStat bool // Include diff stat summary
	IncludeFileList bool // Include changed/untracked file lists
	IncludeCommits  int  // Number of recent commits (0 = none)
	IncludePRInfo   bool // Include PR information (requires gh CLI)
	MaxDiffBytes    int  // Truncate diff at this size (0 = unlimited)
}

// DefaultContextOptions returns options suitable for agent consumption.
func DefaultContextOptions() ContextOptions {
	return ContextOptions{
		IncludeDiff:     true,
		IncludeDiffStat: true,
		IncludeFileList: true,
		IncludeCommits:  10,
		IncludePRInfo:   true,
		MaxDiffBytes:    100_000, // 100KB max diff
	}
}

// GatherContext collects structured context about a worktree.
func (m *Manager) GatherContext(ctx context.Context, wt Worktree, opts ContextOptions) (*WorktreeContext, error) {
	wctx := &WorktreeContext{
		Branch:     wt.Branch,
		Path:       wt.Path,
		GatheredAt: time.Now(),
	}

	// Get goal and parent from git config
	wctx.Goal, _ = m.GetGoal(ctx, wt.Branch, wt.Path)
	wctx.Parent, _ = m.GetParentBranch(ctx, wt.Branch, wt.Path)

	// Diff stat
	if opts.IncludeDiffStat {
		result, err := m.git.Run(ctx, []string{"diff", "--stat"}, wt.Path)
		if err == nil && result != nil {
			wctx.DiffStat = strings.TrimSpace(result.Stdout)
		}
	}

	// Full diff (staged + unstaged)
	if opts.IncludeDiff {
		// Unstaged changes
		result, err := m.git.Run(ctx, []string{"diff"}, wt.Path)
		if err == nil && result != nil {
			wctx.DiffContent = result.Stdout
		}
		// Also include staged changes
		staged, err := m.git.Run(ctx, []string{"diff", "--cached"}, wt.Path)
		if err == nil && staged != nil && staged.Stdout != "" {
			if wctx.DiffContent != "" {
				wctx.DiffContent += "\n"
			}
			wctx.DiffContent += staged.Stdout
		}
		// Truncate if needed
		if opts.MaxDiffBytes > 0 && len(wctx.DiffContent) > opts.MaxDiffBytes {
			wctx.DiffContent = wctx.DiffContent[:opts.MaxDiffBytes] + "\n... (truncated)"
		}
	}

	// Changed files
	if opts.IncludeFileList {
		// Modified files (staged + unstaged)
		result, err := m.git.Run(ctx, []string{"diff", "--name-only", "HEAD"}, wt.Path)
		if err == nil && result != nil {
			wctx.ChangedFiles = splitNonEmpty(result.Stdout)
		}
		// Untracked files
		result, err = m.git.Run(ctx, []string{"ls-files", "--others", "--exclude-standard"}, wt.Path)
		if err == nil && result != nil {
			wctx.UntrackedFiles = splitNonEmpty(result.Stdout)
		}
	}

	// Get git status (fast, local only)
	status, err := m.GetGitStatus(ctx, wt)
	if err == nil {
		wctx.Ahead = status.Ahead
		wctx.Behind = status.Behind
		wctx.IsDirty = status.IsDirty
	}

	// PR info requires a network call; only fetch when requested
	if opts.IncludePRInfo {
		pr, _ := m.FetchPRInfo(ctx, wt)
		if pr != nil {
			wctx.PRNumber = pr.Number
			wctx.PRState = pr.State
			wctx.PRURL = pr.URL
		}
	}

	// Recent commits
	if opts.IncludeCommits > 0 {
		result, err := m.git.Run(ctx, []string{
			"log", fmt.Sprintf("-%d", opts.IncludeCommits),
			"--format=%H|%s|%an|%ct",
		}, wt.Path)
		if err == nil && result != nil {
			wctx.RecentCommits = parseCommitLog(result.Stdout)
		}
	}

	return wctx, nil
}

// FormatForPrompt formats the WorktreeContext as structured text suitable for
// inclusion in an agent's system prompt or message.
func (wc *WorktreeContext) FormatForPrompt() string {
	var b strings.Builder

	b.WriteString("## Worktree Context\n\n")

	b.WriteString(fmt.Sprintf("**Branch:** %s\n", wc.Branch))
	b.WriteString(fmt.Sprintf("**Path:** %s\n", wc.Path))
	if wc.Goal != "" {
		b.WriteString(fmt.Sprintf("**Goal:** %s\n", wc.Goal))
	}
	if wc.Parent != "" {
		b.WriteString(fmt.Sprintf("**Parent:** %s\n", wc.Parent))
	}

	// Branch state
	var stateItems []string
	if wc.IsDirty {
		stateItems = append(stateItems, "dirty")
	} else {
		stateItems = append(stateItems, "clean")
	}
	if wc.Ahead > 0 {
		stateItems = append(stateItems, fmt.Sprintf("%d ahead", wc.Ahead))
	}
	if wc.Behind > 0 {
		stateItems = append(stateItems, fmt.Sprintf("%d behind", wc.Behind))
	}
	b.WriteString(fmt.Sprintf("**Status:** %s\n", strings.Join(stateItems, ", ")))

	// PR info
	if wc.PRNumber > 0 {
		b.WriteString(fmt.Sprintf("**PR:** #%d (%s) %s\n", wc.PRNumber, wc.PRState, wc.PRURL))
	}

	b.WriteString("\n")

	// Diff stat
	if wc.DiffStat != "" {
		b.WriteString("### Changes Summary\n```\n")
		b.WriteString(wc.DiffStat)
		b.WriteString("\n```\n\n")
	}

	// Changed files
	if len(wc.ChangedFiles) > 0 {
		b.WriteString("### Modified Files\n")
		for _, f := range wc.ChangedFiles {
			b.WriteString(fmt.Sprintf("- %s\n", f))
		}
		b.WriteString("\n")
	}

	// Untracked files
	if len(wc.UntrackedFiles) > 0 {
		b.WriteString("### Untracked Files\n")
		for _, f := range wc.UntrackedFiles {
			b.WriteString(fmt.Sprintf("- %s\n", f))
		}
		b.WriteString("\n")
	}

	// Diff content
	if wc.DiffContent != "" {
		b.WriteString("### Diff\n```diff\n")
		b.WriteString(wc.DiffContent)
		b.WriteString("\n```\n\n")
	}

	// Recent commits
	if len(wc.RecentCommits) > 0 {
		b.WriteString("### Recent Commits\n")
		for _, c := range wc.RecentCommits {
			b.WriteString(fmt.Sprintf("- `%s` %s (%s)\n", c.Hash[:minInt(8, len(c.Hash))], c.Subject, c.Author))
		}
		b.WriteString("\n")
	}

	return b.String()
}

// splitNonEmpty splits a newline-separated string and removes empty entries.
func splitNonEmpty(s string) []string {
	var result []string
	for _, line := range strings.Split(strings.TrimSpace(s), "\n") {
		line = strings.TrimSpace(line)
		if line != "" {
			result = append(result, line)
		}
	}
	return result
}

// parseCommitLog parses git log output in "%H|%s|%an|%ct" format.
func parseCommitLog(output string) []CommitInfo {
	var commits []CommitInfo
	for _, line := range strings.Split(strings.TrimSpace(output), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "|", 4)
		if len(parts) < 4 {
			continue
		}
		ci := CommitInfo{
			Hash:    parts[0],
			Subject: parts[1],
			Author:  parts[2],
		}
		if ts, err := strconv.ParseInt(parts[3], 10, 64); err == nil {
			ci.Date = time.Unix(ts, 0)
		}
		commits = append(commits, ci)
	}
	return commits
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
