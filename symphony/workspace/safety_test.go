package workspace

import (
	"testing"
)

func TestValidateWorkspaceKey(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		key     string
		wantErr bool
	}{
		{name: "simple alpha", key: "ABC-123", wantErr: false},
		{name: "dots and underscores", key: "my_project.v2", wantErr: false},
		{name: "all allowed chars", key: "AZaz09._-", wantErr: false},
		{name: "empty string", key: "", wantErr: true},
		{name: "contains slash", key: "abc/def", wantErr: true},
		{name: "contains space", key: "abc def", wantErr: true},
		{name: "contains backslash", key: `abc\def`, wantErr: true},
		{name: "path traversal dots", key: "..", wantErr: true},
		{name: "contains at sign", key: "user@host", wantErr: true},
		{name: "unicode", key: "issue-\u00e9", wantErr: true},
		{name: "single char allowed", key: "a", wantErr: false},
		{name: "single dash", key: "-", wantErr: false},
		{name: "single dot", key: ".", wantErr: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateWorkspaceKey(tt.key)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateWorkspaceKey(%q) error = %v, wantErr %v", tt.key, err, tt.wantErr)
			}
		})
	}
}

func TestValidatePathContainment(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name          string
		workspacePath string
		workspaceRoot string
		wantErr       bool
	}{
		{
			name:          "valid child",
			workspacePath: "/tmp/workspaces/ABC-123",
			workspaceRoot: "/tmp/workspaces",
			wantErr:       false,
		},
		{
			name:          "path equals root",
			workspacePath: "/tmp/workspaces",
			workspaceRoot: "/tmp/workspaces",
			wantErr:       true,
		},
		{
			name:          "path traversal outside root",
			workspacePath: "/tmp/workspaces/../../../etc/passwd",
			workspaceRoot: "/tmp/workspaces",
			wantErr:       true,
		},
		{
			name:          "sibling directory",
			workspacePath: "/tmp/other/ABC-123",
			workspaceRoot: "/tmp/workspaces",
			wantErr:       true,
		},
		{
			name:          "prefix matching trap",
			workspacePath: "/tmp/workspaces_evil/ABC-123",
			workspaceRoot: "/tmp/workspaces",
			wantErr:       true,
		},
		{
			name:          "root with trailing slash",
			workspacePath: "/tmp/workspaces/ABC-123",
			workspaceRoot: "/tmp/workspaces/",
			wantErr:       false,
		},
		{
			name:          "nested child",
			workspacePath: "/tmp/workspaces/a/b/c",
			workspaceRoot: "/tmp/workspaces",
			wantErr:       false,
		},
		{
			name:          "dot-dot in middle that resolves inside",
			workspacePath: "/tmp/workspaces/foo/../bar",
			workspaceRoot: "/tmp/workspaces",
			wantErr:       false,
		},
		{
			name:          "dot-dot that escapes root",
			workspacePath: "/tmp/workspaces/foo/../../other",
			workspaceRoot: "/tmp/workspaces",
			wantErr:       true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidatePathContainment(tt.workspacePath, tt.workspaceRoot)
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidatePathContainment(%q, %q) error = %v, wantErr %v",
					tt.workspacePath, tt.workspaceRoot, err, tt.wantErr)
			}
		})
	}
}
