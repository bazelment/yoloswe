#!/usr/bin/env bash
set -euo pipefail

# Rebuild jiradozer from a local source worktree and atomically re-point the
# CLI symlink at the fresh binary.
#
# This is the local-build sibling of scripts/install.sh (which downloads
# released binaries from GitHub). It exists for the cron host, which runs
# jiradozer from a checked-out worktree: the cron invokes ~/bin/jiradozer, a
# symlink into <worktree>/bazel-bin/. Without a periodic rebuild that symlink
# silently keeps serving a stale build — which is exactly how a fix can land on
# main yet never reach the nightly cron.
#
# Usage:
#   scripts/refresh-jiradozer.sh                       # build from this worktree
#   scripts/refresh-jiradozer.sh --worktree ~/wt/main  # build from another worktree
#   scripts/refresh-jiradozer.sh --pull                # git pull before building
#   scripts/refresh-jiradozer.sh --link ~/bin/jiradozer
#
# Options:
#   --worktree, -w  Source worktree to build from (default: this repo).
#   --link, -l      Symlink path to update (default: ~/bin/jiradozer).
#   --pull          Run `git pull --ff-only` in the worktree before building.
#   --help, -h      Show this help.

TARGET="//jiradozer/cmd/jiradozer"
BIN_RELPATH="bazel-bin/jiradozer/cmd/jiradozer/jiradozer_/jiradozer"

WORKTREE=""
LINK="${HOME}/bin/jiradozer"
PULL=0

while [[ $# -gt 0 ]]; do
  case "$1" in
    --worktree|-w) WORKTREE="$2"; shift 2 ;;
    --link|-l)     LINK="$2"; shift 2 ;;
    --pull)        PULL=1; shift ;;
    --help|-h)
      sed -n '4,31p' "$0" | sed 's/^# \{0,1\}//'
      exit 0
      ;;
    *)
      echo "unknown option: $1" >&2
      exit 2
      ;;
  esac
done

# Default the source worktree to the repo this script lives in.
if [[ -z "${WORKTREE}" ]]; then
  WORKTREE="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fi

if [[ ! -d "${WORKTREE}" ]]; then
  echo "error: worktree not found: ${WORKTREE}" >&2
  exit 1
fi

cd "${WORKTREE}"

if [[ "${PULL}" == "1" ]]; then
  echo ">> git pull --ff-only in ${WORKTREE}"
  git pull --ff-only
fi

HEAD_COMMIT="$(git rev-parse --short HEAD)"
echo ">> building ${TARGET} in ${WORKTREE} (HEAD ${HEAD_COMMIT})"
bazel build "${TARGET}"

# Resolve the real built binary (follow Bazel's convenience symlink to the
# stable output-tree path) so the published link survives the next `bazel
# build` without dangling.
BUILT="$(readlink -f "${WORKTREE}/${BIN_RELPATH}")"
if [[ ! -x "${BUILT}" ]]; then
  echo "error: built binary not found at ${BUILT}" >&2
  exit 1
fi

# Atomically swap the symlink: write to a temp name in the same dir, then mv.
mkdir -p "$(dirname "${LINK}")"
TMP_LINK="$(mktemp -u "$(dirname "${LINK}")/.jiradozer.XXXXXX")"
ln -s "${BUILT}" "${TMP_LINK}"
mv -T "${TMP_LINK}" "${LINK}"

echo ">> ${LINK} -> ${BUILT}"

# Verify provenance for real: invoke the binary so cliapp.Run emits its startup
# banner (build_revision / build_time) to a fresh log, then surface that line.
# `validate-config` exits non-zero without a config, but it still runs far
# enough to write the banner — which is all we need, so its exit status is
# ignored deliberately.
echo ">> verifying build provenance:"
PROV_HOME="$(mktemp -d)"
HOME="${PROV_HOME}" "${LINK}" validate-config >/dev/null 2>&1 || true
PROV_LOG="$(find "${PROV_HOME}/.jiradozer/logs/" -name '*.log' -type f 2>/dev/null | head -1 || true)"
if [[ -n "${PROV_LOG}" ]] && grep -q "build_revision" "${PROV_LOG}"; then
  grep -o 'build_revision=[^ ]* build_time=[^ ]*' "${PROV_LOG}" | head -1 | sed 's/^/   /'
else
  echo "   warning: could not read build provenance banner from a fresh run" >&2
fi
rm -rf "${PROV_HOME}"
echo ">> done. jiradozer refreshed to ${HEAD_COMMIT}."
