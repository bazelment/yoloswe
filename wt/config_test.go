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
}
