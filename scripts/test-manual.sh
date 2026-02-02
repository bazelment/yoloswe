#!/usr/bin/env bash
set -euo pipefail

# Run manual tests that require real CLI tools and login tokens
# These tests are excluded from `bazel test //...`

WORKSPACE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${WORKSPACE_ROOT}"

echo "=== Discovering manual tests ==="

# Find all test targets with the "manual" tag
MANUAL_TESTS=$(bazel query 'attr(tags, "manual", tests(//...))' 2>/dev/null)

if [[ -z "${MANUAL_TESTS}" ]]; then
    echo "No manual tests found."
    exit 0
fi

echo "Found manual tests:"
echo "${MANUAL_TESTS}" | sed 's/^/  /'
echo ""

echo "=== Running manual tests ==="
echo "These tests require real CLI tools and authentication tokens."
echo ""

# shellcheck disable=SC2086
bazel test ${MANUAL_TESTS} "$@"
