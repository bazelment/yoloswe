package codereview

import (
	"strings"
	"testing"

	"github.com/bazelment/yoloswe/yoloswe/reviewer"
)

func TestRedactPath(t *testing.T) {
	tests := []struct {
		name        string
		in          string
		wantSuffix  string
		forbidParts []string
	}{
		{
			name:        "absolute home path",
			in:          "/home/alice/work/project-x",
			wantSuffix:  "/project-x",
			forbidParts: []string{"/home/alice", "/work/"},
		},
		{
			name:        "worktree path",
			in:          "/home/bob/worktrees/repo/feature/foo",
			wantSuffix:  "/foo",
			forbidParts: []string{"/home/bob", "worktrees", "repo", "feature"},
		},
		{
			name:       "empty",
			in:         "",
			wantSuffix: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := redactPath(tt.in)
			if tt.in == "" {
				if got != "" {
					t.Errorf("redactPath(\"\") = %q, want empty", got)
				}
				return
			}
			if !strings.HasSuffix(got, tt.wantSuffix) {
				t.Errorf("redactPath(%q) = %q, want suffix %q", tt.in, got, tt.wantSuffix)
			}
			for _, forbidden := range tt.forbidParts {
				if strings.Contains(got, forbidden) {
					t.Errorf("redactPath(%q) = %q leaked %q", tt.in, got, forbidden)
				}
			}
		})
	}
}

func TestMaxSeverity(t *testing.T) {
	tests := []struct {
		name   string
		want   string
		issues []reviewer.ReviewIssue
	}{
		{
			name:   "empty",
			issues: nil,
			want:   "",
		},
		{
			name: "single low",
			issues: []reviewer.ReviewIssue{
				{Severity: "low"},
			},
			want: "low",
		},
		{
			name: "standard ordering picks critical",
			issues: []reviewer.ReviewIssue{
				{Severity: "low"},
				{Severity: "medium"},
				{Severity: "critical"},
				{Severity: "high"},
			},
			want: "critical",
		},
		{
			name: "skips empty severities",
			issues: []reviewer.ReviewIssue{
				{Severity: ""},
				{Severity: "medium"},
			},
			want: "medium",
		},
		{
			name: "unknown label outranks low even when low is seen first",
			issues: []reviewer.ReviewIssue{
				{Severity: "low"},
				{Severity: "blocker"},
			},
			want: "blocker",
		},
		{
			name: "unknown label still below medium",
			issues: []reviewer.ReviewIssue{
				{Severity: "blocker"},
				{Severity: "medium"},
			},
			want: "medium",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := maxSeverity(tt.issues)
			if got != tt.want {
				t.Errorf("maxSeverity(%+v) = %q, want %q", tt.issues, got, tt.want)
			}
		})
	}
}
