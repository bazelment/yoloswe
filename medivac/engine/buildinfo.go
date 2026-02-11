package engine

import (
	"os"
	"path/filepath"
)

// BuildSystem identifies the project's build tooling.
type BuildSystem string

const (
	BuildSystemBazel   BuildSystem = "bazel"
	BuildSystemNx      BuildSystem = "nx"
	BuildSystemMake    BuildSystem = "make"
	BuildSystemNPM     BuildSystem = "npm"
	BuildSystemUnknown BuildSystem = "unknown"
)

// BuildInfo holds detected build system metadata for a repository.
type BuildInfo struct {
	System     BuildSystem
	LintCmd    string
	BuildCmd   string
	TestCmd    string
	ClaudeMD   string // contents of CLAUDE.md if present
	ExtraRules []string
}

// DetectBuildInfo probes the repo root for build system markers and returns
// structured build metadata. Detection order matters: more specific systems
// (Bazel, Nx) are checked before generic ones (Make, npm).
func DetectBuildInfo(repoDir string) BuildInfo {
	info := BuildInfo{System: BuildSystemUnknown}

	// Read CLAUDE.md if present (always, regardless of build system)
	claudeMDPath := filepath.Join(repoDir, "CLAUDE.md")
	if data, err := os.ReadFile(claudeMDPath); err == nil {
		info.ClaudeMD = string(data)
	}

	// Check for Bazel
	if fileExists(filepath.Join(repoDir, "BUILD.bazel")) ||
		fileExists(filepath.Join(repoDir, "WORKSPACE")) ||
		fileExists(filepath.Join(repoDir, "WORKSPACE.bazel")) ||
		fileExists(filepath.Join(repoDir, "MODULE.bazel")) {
		info.System = BuildSystemBazel
		info.LintCmd = "scripts/lint.sh (if it exists) or bazel test //..."
		info.BuildCmd = "bazel build //..."
		info.TestCmd = "bazel test //..."
		info.ExtraRules = []string{
			"Never use `go build` or `go test` directly -- use Bazel",
			"Never manually edit BUILD.bazel or go.mod -- use the proper Bazel commands",
		}
		return info
	}

	// Check for Nx monorepo
	if fileExists(filepath.Join(repoDir, "nx.json")) {
		info.System = BuildSystemNx
		info.LintCmd = "pnpm dlx nx affected -t lint"
		info.BuildCmd = "pnpm dlx nx affected -t build"
		info.TestCmd = "pnpm dlx nx affected -t test"
		info.ExtraRules = []string{
			"Use pnpm for package management, not npm or yarn",
			"Run Nx targets through `pnpm dlx nx` or `npx nx`",
		}
		// Check for Python tooling in the same repo
		if fileExists(filepath.Join(repoDir, "pyproject.toml")) {
			info.ExtraRules = append(info.ExtraRules,
				"For Python code, use `uv run ruff check .` for linting and `uv run pytest` for tests",
			)
		}
		return info
	}

	// Check for Makefile
	if fileExists(filepath.Join(repoDir, "Makefile")) {
		info.System = BuildSystemMake
		info.LintCmd = "make lint (if target exists)"
		info.BuildCmd = "make build (if target exists)"
		info.TestCmd = "make test (if target exists)"
		return info
	}

	// Check for npm/pnpm/yarn
	if fileExists(filepath.Join(repoDir, "package.json")) {
		info.System = BuildSystemNPM
		pkgManager := "npm"
		if fileExists(filepath.Join(repoDir, "pnpm-lock.yaml")) {
			pkgManager = "pnpm"
		} else if fileExists(filepath.Join(repoDir, "yarn.lock")) {
			pkgManager = "yarn"
		}
		info.LintCmd = pkgManager + " run lint"
		info.BuildCmd = pkgManager + " run build"
		info.TestCmd = pkgManager + " run test"
		return info
	}

	// Fallback
	info.LintCmd = "check CLAUDE.md for lint instructions"
	info.BuildCmd = "check CLAUDE.md for build instructions"
	info.TestCmd = "check CLAUDE.md for test instructions"
	return info
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// TruncateClaudeMD returns the CLAUDE.md contents, truncated to maxLen
// characters with an ellipsis if needed. This prevents the fix prompt from
// becoming excessively large.
func TruncateClaudeMD(content string, maxLen int) string {
	if len(content) <= maxLen {
		return content
	}
	return content[:maxLen] + "\n... (truncated, read the full CLAUDE.md for complete instructions)"
}
