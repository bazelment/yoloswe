package wt

import (
	"os"
	"os/exec"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

// RepoConfig holds per-repository configuration from .wt.yaml.
type RepoConfig struct {
	DefaultBase      string   `yaml:"default_base"`
	PostCreate       []string `yaml:"post_create"`
	PostRemove       []string `yaml:"post_remove"`
	OnWorktreeCreate []string `yaml:"on_worktree_create"`
	OnWorktreeDelete []string `yaml:"on_worktree_delete"`
}

// LoadRepoConfig loads .wt.yaml from a repository path.
// Returns a default config if the file doesn't exist.
func LoadRepoConfig(repoPath string) (*RepoConfig, error) {
	configPath := filepath.Join(repoPath, ".wt.yaml")

	data, err := os.ReadFile(configPath)
	if os.IsNotExist(err) {
		return &RepoConfig{DefaultBase: "main"}, nil
	}
	if err != nil {
		return nil, err
	}

	var config RepoConfig
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, err
	}

	if config.DefaultBase == "" {
		config.DefaultBase = "main"
	}

	return &config, nil
}

// WorktreeCreateCommands returns commands that should run after creating a worktree.
// It supports both legacy wt keys and bramble-specific keys.
func (c *RepoConfig) WorktreeCreateCommands() []string {
	if c == nil {
		return nil
	}
	cmds := make([]string, 0, len(c.PostCreate)+len(c.OnWorktreeCreate))
	cmds = append(cmds, c.PostCreate...)
	cmds = append(cmds, c.OnWorktreeCreate...)
	return cmds
}

// WorktreeDeleteCommands returns commands that should run before deleting a worktree.
// It supports both legacy wt keys and bramble-specific keys.
func (c *RepoConfig) WorktreeDeleteCommands() []string {
	if c == nil {
		return nil
	}
	cmds := make([]string, 0, len(c.PostRemove)+len(c.OnWorktreeDelete))
	cmds = append(cmds, c.PostRemove...)
	cmds = append(cmds, c.OnWorktreeDelete...)
	return cmds
}

// RunHooks executes hook commands in a worktree.
func RunHooks(commands []string, worktreePath, branch string, output *Output) error {
	env := os.Environ()
	env = append(env, "WT_BRANCH="+branch, "WT_PATH="+worktreePath)

	for _, cmdStr := range commands {
		output.Info("Running: " + cmdStr)

		cmd := exec.Command("sh", "-c", cmdStr)
		cmd.Dir = worktreePath
		cmd.Env = env
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr

		if err := cmd.Run(); err != nil {
			output.Error("Hook failed: " + cmdStr)
			return err
		}
	}

	return nil
}
