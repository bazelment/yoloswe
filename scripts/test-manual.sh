#!/usr/bin/env bash
set -euo pipefail

# Run manual tests that require real CLI tools and login tokens
# These tests are excluded from `bazel test //...`

WORKSPACE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${WORKSPACE_ROOT}"

echo "=== Discovering manual tests ==="

REGISTERED_MANUAL_TESTS=(
    "//orch/replay/integration:eval_test"
)

# Find all test targets with the "manual" tag
QUERY_MANUAL_TESTS=$(bazel query 'attr(tags, "manual", tests(//...))' 2>/dev/null || true)
MANUAL_TESTS=$(printf "%s\n%s\n" "${QUERY_MANUAL_TESTS}" "${REGISTERED_MANUAL_TESTS[@]}" | sed '/^$/d' | sort -u)

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

# Pass HOME and PATH so tests can find CLI tools (gh, claude, codex, agent)
# and their authentication configs (~/.config/gh/hosts.yml, etc.).
# Bazel sandboxing strips these by default.
# shellcheck disable=SC2086
bazel test ${MANUAL_TESTS} \
    --test_env=HOME="${HOME}" \
    --test_env=PATH="${PATH}" \
    "$@"
