package engine

import (
	"testing"

	"github.com/bazelment/yoloswe/medivac/github"
	"github.com/bazelment/yoloswe/medivac/issue"
)

func TestGroupIssues_TSErrorCodes(t *testing.T) {
	issues := []*issue.Issue{
		{Signature: "sig1", Category: "lint/ts", Summary: "Parameter 'e' implicitly has an 'any' type", File: "src/components/foo.tsx", Details: "error TS7006"},
		{Signature: "sig2", Category: "lint/ts", Summary: "Parameter 'file' implicitly has an 'any' type", File: "src/components/bar.tsx", Details: "error TS7006"},
		{Signature: "sig3", Category: "lint/ts", Summary: "Cannot find module '@sycamore-labs/ui'", File: "src/admin/viewer.tsx", Details: "error TS2307"},
	}

	groups := GroupIssues(issues)

	if len(groups) != 2 {
		t.Fatalf("expected 2 groups (TS7006 + TS2307), got %d", len(groups))
	}

	// First group should have the two TS7006 issues
	if len(groups[0].Issues) != 2 {
		t.Errorf("expected 2 issues in TS7006 group, got %d", len(groups[0].Issues))
	}
	if len(groups[1].Issues) != 1 {
		t.Errorf("expected 1 issue in TS2307 group, got %d", len(groups[1].Issues))
	}
}

func TestGroupIssues_Dependabot(t *testing.T) {
	issues := []*issue.Issue{
		{Signature: "sig1", Category: github.CategoryInfraDepbot, Summary: "No solution found when resolving dependencies for cryptography"},
		{Signature: "sig2", Category: github.CategoryInfraDepbot, Summary: "Dependency resolution failed for cryptography>=46.0.0"},
	}

	groups := GroupIssues(issues)

	if len(groups) != 1 {
		t.Fatalf("expected 1 group (both cryptography), got %d", len(groups))
	}
	if len(groups[0].Issues) != 2 {
		t.Errorf("expected 2 issues in cryptography group, got %d", len(groups[0].Issues))
	}
}

func TestGroupIssues_Singleton(t *testing.T) {
	issues := []*issue.Issue{
		{Signature: "sig1", Category: github.CategoryLintGo, Summary: "unused variable x", File: "main.go"},
		{Signature: "sig2", Category: github.CategoryInfraCI, Summary: "parallel golangci-lint is running"},
	}

	groups := GroupIssues(issues)

	if len(groups) != 2 {
		t.Fatalf("expected 2 singleton groups, got %d", len(groups))
	}
}

func TestGroupIssues_DockerBuildGroupsWithTS(t *testing.T) {
	// Docker build failures with TS error codes should group with lint/ts issues
	issues := []*issue.Issue{
		{Signature: "sig1", Category: "lint/ts", Summary: "Parameter 'e' implicitly has an 'any' type", File: "src/foo.tsx", Details: "TS7006"},
		{Signature: "sig2", Category: "build/docker", Summary: "Parameter 'idx' implicitly has an 'any' type", File: "src/bar.tsx", Details: "TS7006"},
	}

	groups := GroupIssues(issues)

	if len(groups) != 1 {
		t.Fatalf("expected 1 group (both TS7006), got %d", len(groups))
	}
	if len(groups[0].Issues) != 2 {
		t.Errorf("expected 2 issues in TS7006 group, got %d", len(groups[0].Issues))
	}
}

func TestExtractTSErrorCode(t *testing.T) {
	tests := []struct {
		summary string
		details string
		want    string
	}{
		{"Parameter 'e' implicitly has an 'any' type", "error TS7006: ...", "TS7006"},
		{"Cannot find module '@sycamore-labs/ui'", "", "TS2307"},
		{"Parameter 'e' implicitly has an 'any' type", "", "TS7006"},
		{"some random build error", "", ""},
	}

	for _, tt := range tests {
		iss := &issue.Issue{Summary: tt.summary, Details: tt.details}
		got := extractTSErrorCode(iss)
		if got != tt.want {
			t.Errorf("extractTSErrorCode(%q, %q) = %q, want %q", tt.summary, tt.details, got, tt.want)
		}
	}
}

func TestExtractPackageName(t *testing.T) {
	tests := []struct {
		summary string
		want    string
	}{
		{"No solution found when resolving dependencies for cryptography", "cryptography"},
		{"Dependency resolution failed for cryptography>=46.0.0", "cryptography"},
		{"no security update needed as cryptography is no longer vulnerable", "cryptography"},
		{"Dependency resolution failed for clerk-backend-api", "clerk-backend-api"},
		{"random failure", ""},
	}

	for _, tt := range tests {
		got := extractPackageName(tt.summary)
		if got != tt.want {
			t.Errorf("extractPackageName(%q) = %q, want %q", tt.summary, got, tt.want)
		}
	}
}
