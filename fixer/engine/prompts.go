package engine

import (
	"fmt"
	"strings"

	"github.com/bazelment/yoloswe/fixer/issue"
)

// buildFixPrompt constructs the prompt for a fix agent given an issue.
func buildFixPrompt(iss *issue.Issue, branch string) string {
	var b strings.Builder

	b.WriteString("You are a CI failure fixer agent. Your goal is to fix a specific CI failure in this repository.\n\n")

	b.WriteString("## Failure Details\n\n")
	b.WriteString(fmt.Sprintf("- **Category:** %s\n", iss.Category))
	b.WriteString(fmt.Sprintf("- **Summary:** %s\n", iss.Summary))
	if iss.File != "" {
		b.WriteString(fmt.Sprintf("- **File:** %s", iss.File))
		if iss.Line > 0 {
			b.WriteString(fmt.Sprintf(":%d", iss.Line))
		}
		b.WriteString("\n")
	}
	b.WriteString(fmt.Sprintf("- **Branch:** %s\n", branch))
	b.WriteString(fmt.Sprintf("- **Times seen:** %d\n", iss.SeenCount))

	if iss.Details != "" {
		b.WriteString(fmt.Sprintf("\n### Error Details\n\n```\n%s\n```\n\n", iss.Details))
	}

	b.WriteString("## Instructions\n\n")
	b.WriteString("1. **Investigate** the failure by reading the relevant files and understanding the root cause.\n")
	b.WriteString("2. **Fix** the issue with the minimal change needed.\n")
	b.WriteString("3. **Verify** your fix by running the appropriate checks:\n")

	switch {
	case strings.HasPrefix(string(iss.Category), "lint/"):
		b.WriteString("   - Run `scripts/lint.sh` to verify lint passes\n")
	case strings.HasPrefix(string(iss.Category), "build/"):
		b.WriteString("   - Run `bazel build //...` to verify the build passes\n")
	case strings.HasPrefix(string(iss.Category), "test/"):
		b.WriteString("   - Run the specific failing test with `bazel test <target> --test_timeout=60`\n")
		b.WriteString("   - If that passes, run `bazel test //...` to check for regressions\n")
	default:
		b.WriteString("   - Run `scripts/lint.sh` and `bazel test //...`\n")
	}

	b.WriteString("4. **Commit** your changes with a clear commit message explaining the fix.\n")
	b.WriteString("5. **Push** to the remote branch.\n\n")

	b.WriteString("## Important Rules\n\n")
	b.WriteString("- Follow the CLAUDE.md instructions in the repo root\n")
	b.WriteString("- Never use `go build` or `go test` directly — use Bazel\n")
	b.WriteString("- Make minimal, focused changes — do not refactor unrelated code\n")
	b.WriteString("- If you cannot fix the issue, explain why clearly\n")
	b.WriteString("- Never manually edit BUILD.bazel or go.mod — use the proper Bazel commands\n")

	return b.String()
}
