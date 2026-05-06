#!/usr/bin/env bash
set -euo pipefail

# Lint Go code and Bazel build files
# Run this before sending PRs to catch issues early

# Find workspace root
if [[ -n "${BUILD_WORKSPACE_DIRECTORY:-}" ]]; then
    # Running under Bazel
    WORKSPACE_ROOT="${BUILD_WORKSPACE_DIRECTORY}"
else
    # Running directly
    WORKSPACE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fi

cd "${WORKSPACE_ROOT}"

# Track if any checks fail
FAILED=0

echo "=== Linting Go code ==="

# Check if golangci-lint is installed
if ! command -v golangci-lint &> /dev/null; then
    echo "Error: golangci-lint is not installed."
    echo "Install it with: brew install golangci-lint"
    echo "Or see: https://golangci-lint.run/welcome/install/"
    exit 1
fi

# Use a persistent per-worktree cache so concurrent worktrees do not fight over
# golangci-lint's global cache lock, while repeated runs still stay warm.
if [[ -z "${GOLANGCI_LINT_CACHE:-}" ]]; then
    cache_root="${XDG_CACHE_HOME:-${HOME:-/tmp}/.cache}/golangci-lint/workspaces"
    workspace_key="$(printf '%s' "${WORKSPACE_ROOT}" | cksum | cut -d ' ' -f 1)"
    workspace_slug="$(basename "${WORKSPACE_ROOT}" | tr -c '[:alnum:]_.-' '_')"
    GOLANGCI_LINT_CACHE="${cache_root}/${workspace_slug}-${workspace_key}"
    mkdir -p "${GOLANGCI_LINT_CACHE}"
    export GOLANGCI_LINT_CACHE
fi

# Run golangci-lint on each module in go.work
GO_LINT_FAILED=0

# Parse module directories from go.work (lines starting with ./ or whitespace then ./)
MODULES=$(grep -E '^\s*\./' go.work | sed 's/^[[:space:]]*\.\///' | tr -d '\r')

if [[ -z "${MODULES}" ]]; then
    echo "Error: No modules found in go.work"
    exit 1
fi

for module in ${MODULES}; do
    if [[ -d "${module}" ]]; then
        echo "  Linting ${module}..."
        if ! (cd "${module}" && golangci-lint run --timeout=10m ./...); then
            GO_LINT_FAILED=1
        fi
    fi
done

if [[ ${GO_LINT_FAILED} -eq 0 ]]; then
    echo "✓ Go lint passed"
else
    echo "✗ Go lint failed"
    FAILED=1
fi

echo ""
echo "=== Linting Bazel files ==="

# Always use the Bazel-pinned buildifier (//tools:buildifier) so local runs match
# CI exactly. Falls back to a system buildifier only if Bazel is unavailable.
if command -v bazel &> /dev/null; then
    BUILDIFIER_CMD=(bazel run --ui_event_filters=-info,-stdout,-stderr --noshow_progress //tools:buildifier --)
    BUILDIFIER_FIX_HINT="bazel run //tools:buildifier -- -r \"\$(pwd)\""
elif command -v buildifier &> /dev/null; then
    echo "Warning: bazel not found; using system buildifier ($(buildifier --version 2>&1 | head -1))." >&2
    echo "         CI uses the Bazel-pinned buildifier; results may diverge." >&2
    BUILDIFIER_CMD=(buildifier)
    BUILDIFIER_FIX_HINT="buildifier -r ."
else
    echo "Error: neither bazel nor buildifier is available."
    echo "Install Bazel (recommended) or buildifier directly."
    echo "See: https://github.com/bazelbuild/buildtools"
    exit 1
fi

# Find all BUILD and .bzl files (absolute paths so `bazel run`'s cwd switch
# into the runfiles dir doesn't break path resolution).
BAZEL_FILES=$(find "${WORKSPACE_ROOT}" -type f \( -name "BUILD" -o -name "BUILD.bazel" -o -name "*.bzl" -o -name "WORKSPACE" -o -name "WORKSPACE.bazel" -o -name "MODULE.bazel" \) -not -path "${WORKSPACE_ROOT}/bazel-*" 2>/dev/null || true)

if [[ -z "${BAZEL_FILES}" ]]; then
    echo "No Bazel files found"
else
    # Check formatting (mode=check returns non-zero if files need formatting)
    if echo "${BAZEL_FILES}" | xargs "${BUILDIFIER_CMD[@]}" --mode=check --lint=warn; then
        echo "✓ Bazel lint passed"
    else
        echo "✗ Bazel lint failed"
        echo ""
        echo "To fix formatting issues, run:"
        echo "  ${BUILDIFIER_FIX_HINT}"
        FAILED=1
    fi
fi

echo ""
if [[ ${FAILED} -eq 0 ]]; then
    echo "=== All checks passed ==="
    exit 0
else
    echo "=== Some checks failed ==="
    exit 1
fi
