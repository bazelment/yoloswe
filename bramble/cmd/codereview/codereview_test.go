package codereview

import (
	"testing"

	"github.com/bazelment/yoloswe/yoloswe/reviewer"
)

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
