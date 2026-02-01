#!/usr/bin/env bash
set -euo pipefail

# Run manual tests that require real CLI tools and login tokens
# These tests are excluded from `bazel test //...`

WORKSPACE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${WORKSPACE_ROOT}"

echo "=== Running manual tests ==="
echo "These tests require real CLI tools and authentication tokens."
echo ""

bazel test \
    //agent-cli-wrapper/codex/integration:integration_test \
    //yoloswe:yoloswe_test \
    //yoloswe/planner:planner_test \
    //yoloswe/reviewer/integration:integration_test \
    "$@"
