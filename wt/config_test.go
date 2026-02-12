package wt

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadRepoConfig(t *testing.T) {
	t.Run("missing file returns defaults", func(t *testing.T) {
		tmpDir := t.TempDir()
		config, err := LoadRepoConfig(tmpDir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if config.DefaultBase != "main" {
			t.Errorf("DefaultBase = %q, want %q", config.DefaultBase, "main")
		}
		if len(config.PostCreate) != 0 {
			t.Errorf("PostCreate = %v, want empty", config.PostCreate)
		}
		if len(config.WorktreeCreateCommands()) != 0 {
			t.Errorf("WorktreeCreateCommands() = %v, want empty", config.WorktreeCreateCommands())
		}
		if len(config.WorktreeDeleteCommands()) != 0 {
			t.Errorf("WorktreeDeleteCommands() = %v, want empty", config.WorktreeDeleteCommands())
		}
	})

	t.Run("valid yaml file", func(t *testing.T) {
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, ".wt.yaml")
		content := `
default_base: develop
post_create:
  - npm install
  - npm run build
post_remove:
  - echo "cleanup"
`
		if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
			t.Fatalf("failed to write config: %v", err)
		}

		config, err := LoadRepoConfig(tmpDir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if config.DefaultBase != "develop" {
			t.Errorf("DefaultBase = %q, want %q", config.DefaultBase, "develop")
		}
		if len(config.PostCreate) != 2 {
			t.Errorf("len(PostCreate) = %d, want 2", len(config.PostCreate))
		}
		if len(config.PostRemove) != 1 {
			t.Errorf("len(PostRemove) = %d, want 1", len(config.PostRemove))
		}
		if len(config.WorktreeCreateCommands()) != 2 {
			t.Errorf("len(WorktreeCreateCommands()) = %d, want 2", len(config.WorktreeCreateCommands()))
		}
		if len(config.WorktreeDeleteCommands()) != 1 {
			t.Errorf("len(WorktreeDeleteCommands()) = %d, want 1", len(config.WorktreeDeleteCommands()))
		}
	})

	t.Run("empty default_base uses main", func(t *testing.T) {
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, ".wt.yaml")
		content := `
post_create:
  - npm install
`
		if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
			t.Fatalf("failed to write config: %v", err)
		}

		config, err := LoadRepoConfig(tmpDir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if config.DefaultBase != "main" {
			t.Errorf("DefaultBase = %q, want %q", config.DefaultBase, "main")
		}
	})

	t.Run("supports bramble worktree lifecycle keys", func(t *testing.T) {
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, ".wt.yaml")
		content := `
on_worktree_create:
  - npm ci
on_worktree_delete:
  - rm -rf .cache
`
		if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
			t.Fatalf("failed to write config: %v", err)
		}

		config, err := LoadRepoConfig(tmpDir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(config.WorktreeCreateCommands()) != 1 {
			t.Errorf("len(WorktreeCreateCommands()) = %d, want 1", len(config.WorktreeCreateCommands()))
		}
		if got := config.WorktreeCreateCommands()[0]; got != "npm ci" {
			t.Errorf("WorktreeCreateCommands()[0] = %q, want %q", got, "npm ci")
		}

		if len(config.WorktreeDeleteCommands()) != 1 {
			t.Errorf("len(WorktreeDeleteCommands()) = %d, want 1", len(config.WorktreeDeleteCommands()))
		}
		if got := config.WorktreeDeleteCommands()[0]; got != "rm -rf .cache" {
			t.Errorf("WorktreeDeleteCommands()[0] = %q, want %q", got, "rm -rf .cache")
		}
	})

	t.Run("merges legacy and bramble lifecycle keys", func(t *testing.T) {
		tmpDir := t.TempDir()
		configPath := filepath.Join(tmpDir, ".wt.yaml")
		content := `
post_create:
  - npm install
on_worktree_create:
  - npm run generate
post_remove:
  - echo old
on_worktree_delete:
  - echo new
`
		if err := os.WriteFile(configPath, []byte(content), 0644); err != nil {
			t.Fatalf("failed to write config: %v", err)
		}

		config, err := LoadRepoConfig(tmpDir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		create := config.WorktreeCreateCommands()
		if len(create) != 2 {
			t.Fatalf("len(WorktreeCreateCommands()) = %d, want 2", len(create))
		}
		if create[0] != "npm install" || create[1] != "npm run generate" {
			t.Errorf("WorktreeCreateCommands() = %v, want [npm install npm run generate]", create)
		}

		del := config.WorktreeDeleteCommands()
		if len(del) != 2 {
			t.Fatalf("len(WorktreeDeleteCommands()) = %d, want 2", len(del))
		}
		if del[0] != "echo old" || del[1] != "echo new" {
			t.Errorf("WorktreeDeleteCommands() = %v, want [echo old echo new]", del)
		}
	})
}
