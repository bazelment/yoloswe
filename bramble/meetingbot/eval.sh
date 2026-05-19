#!/usr/bin/env bash
set -euo pipefail

if [[ -n "${BUILD_WORKSPACE_DIRECTORY:-}" ]]; then
    WORKSPACE_ROOT="${BUILD_WORKSPACE_DIRECTORY}"
else
    WORKSPACE_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
fi

cd "${WORKSPACE_ROOT}"

if [[ -z "${MEETINGBOT_NOTES_GLOB:-}" ]]; then
    echo "Error: set MEETINGBOT_NOTES_GLOB to the private transcript glob before running eval." >&2
    exit 2
fi

NOTES_GLOB="${MEETINGBOT_NOTES_GLOB}"
AGENT_MODE="${MEETINGBOT_AGENT:-local}"
REPORT_PATH="${MEETINGBOT_EVAL_REPORT:-/tmp/meetingbot-eval-report.json}"

bazel run //bramble:bramble -- meetingbot \
    --agent="${AGENT_MODE}" \
    --notes-glob="${NOTES_GLOB}" \
    --evaluate \
    --quality-gate \
    --eval-report="${REPORT_PATH}" \
    --work-dir="${WORKSPACE_ROOT}" \
    "$@"

echo "meetingbot eval report: ${REPORT_PATH}"
