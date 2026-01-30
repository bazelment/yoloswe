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

if golangci-lint run --timeout=10m ./...; then
    echo "✓ Go lint passed"
else
    echo "✗ Go lint failed"
    FAILED=1
fi

echo ""
echo "=== Linting Bazel files ==="

# Check if buildifier is installed
if ! command -v buildifier &> /dev/null; then
    echo "Error: buildifier is not installed."
    echo "Install it with: brew install buildifier"
    echo "Or see: https://github.com/bazelbuild/buildtools"
    exit 1
fi

# Find all BUILD and .bzl files
BAZEL_FILES=$(find . -type f \( -name "BUILD" -o -name "BUILD.bazel" -o -name "*.bzl" -o -name "WORKSPACE" -o -name "WORKSPACE.bazel" -o -name "MODULE.bazel" \) -not -path "./bazel-*" 2>/dev/null || true)

if [[ -z "${BAZEL_FILES}" ]]; then
    echo "No Bazel files found"
else
    # Check formatting (mode=check returns non-zero if files need formatting)
    if echo "${BAZEL_FILES}" | xargs buildifier --mode=check --lint=warn; then
        echo "✓ Bazel lint passed"
    else
        echo "✗ Bazel lint failed"
        echo ""
        echo "To fix formatting issues, run:"
        echo "  buildifier -r ."
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
