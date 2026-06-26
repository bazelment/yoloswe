#!/usr/bin/env bash
# Bazel workspace status command. Emits STABLE_* keys that, when a build runs
# with --stamp (see .bazelrc's `stamp` config), are substituted into go_binary
# x_defs so the binary self-identifies the commit and build time it was built
# from. Resilient to a non-git tree (CI tarball, sandbox): falls back to
# "unknown" rather than failing the build.
#
# STABLE_ keys force a re-link when their value changes; volatile keys (no
# STABLE_ prefix) would not. We want the embedded revision to track the commit,
# so both keys are STABLE_.
set -euo pipefail

revision="unknown"
build_time="unknown"

if git rev-parse --git-dir >/dev/null 2>&1; then
    if rev="$(git rev-parse HEAD 2>/dev/null)"; then
        revision="$rev"
    fi
    if t="$(git show -s --format=%cI HEAD 2>/dev/null)"; then
        build_time="$t"
    fi
fi

echo "STABLE_GIT_REVISION ${revision}"
echo "STABLE_GIT_TIME ${build_time}"
