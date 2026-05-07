#!/usr/bin/env python3
"""Compute scope hints for ``bramble code-review --scope-hints-file``.

Walks the diff against the merge base and emits a JSON file that bramble
loads as ``reviewer.ScopeHints``. The schema is defined in
``yoloswe/reviewer/scope_hints.go``; keep this in sync. The kernel post-push
gap analysis (see plan ``plans/issue-175-widen-review-scope.md``) showed
~5/14 substantive bot comments arrive after /pr-polish converges that
the reviewers could have caught with broader scope:

  1. Co-located test files don't get the same level of scrutiny as source
     (tautological asserts, broad ``Exception`` catches, ineffective mocks).
  2. PRs that span multiple top-level packages miss producer/consumer
     contract desyncs (signature shape, async state, route ordering).

This module computes the inputs for both bramble-side prompt clauses:
``test_paths`` (co-located test files) and ``cross_service_packages``
(detected when the diff touches >=2 top-level buckets).

Run cadence: **once at the start of each /pr-polish round**, overwriting
``<state_dir>/scope-hints.json``. The kernel-2755 evidence shows the scope
set genuinely grows across rounds (a fix-introduced test file in r2, a
helper module in r3); recomputing per round catches that. Cost is ~100ms,
which is noise compared to a 60–400s backend turn.

Usage:
    python3 scope_gate.py --state-dir <dir> [--base BRANCH] \\
        [--cross-service-roots services/<lang>/<svc>/,...]

Output: ``<state_dir>/scope-hints.json`` and the path printed to stdout
so the orchestrator can pipe it straight into ``--scope-hints-file=``.

This module never invokes bramble. Pure stdlib + git.
"""

from __future__ import annotations

import argparse
import os
import sys
from pathlib import Path
from typing import Iterable

SCRIPT_DIR = Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

from _common import (  # noqa: E402
    CommandError,
    GO_EXTENSIONS,
    JS_TS_EXTENSIONS,
    PY_EXTENSIONS,
    atomic_write_json,
    changed_files,
    detect_base_branch,
    run,
)


# Schema version of the JSON file. Must match
# reviewer.ScopeHintsSchemaVersion in yoloswe/reviewer/scope_hints.go.
# v1: original shape (cross_service_packages only)
# v2: adds changed_packages and dependency_packages alongside cross_service_packages
SCHEMA_VERSION = 2

# Cap on inlined test paths. Keep in sync with reviewer.testScopeHintsCap.
# The cap exists to keep bramble's prompt token count bounded on giant
# multi-package PRs; bramble truncates with a "(... and N more)" suffix.
MAX_TEST_PATHS = 50

# Multi-package detection threshold. Below this many distinct top-level
# buckets, the cross-service clause stays off; below this many changed
# files total, we assume the PR is too small for a useful sweep even if
# it nominally touches two trees.
MIN_PACKAGES_FOR_SWEEP = 2
MIN_FILES_FOR_SWEEP = 3


# ---------------------------------------------------------------------------
# Per-language test-path enumeration
# ---------------------------------------------------------------------------


def _is_python_test(name: str) -> bool:
    """Filename-only test detection.

    pytest discovers ``test_*.py`` and ``*_test.py``; both are common. We
    accept either rather than picking sides. Note this does NOT match
    arbitrary ``.py`` files inside a directory named ``tests`` — directory
    membership is handled by ``_walk_tests``, which accepts any ``.py``
    under a ``tests`` tree (conftest.py, fixtures, helpers).
    """
    return (name.startswith("test_") and name.endswith(".py")) or name.endswith("_test.py")


def _is_go_test(name: str) -> bool:
    return name.endswith("_test.go")


def _is_ts_js_test(name: str) -> bool:
    """Match the common Jest/Vitest/Playwright suffix conventions plus
    ``__tests__`` colocation. Picking up everything under ``__tests__``
    rather than only ``*.test.{ext}`` keeps fixtures and shared helpers
    in scope, which is usually what the reviewer wants.
    """
    # Cover the same module formats _bucket() recognizes; otherwise a
    # changed source file with extension .mjs/.cjs would route into the JS
    # bucket but its co-located *.test.mjs / *.spec.cjs would never match.
    for suffix in (".test.ts", ".test.tsx", ".test.js", ".test.jsx",
                   ".test.mjs", ".test.cjs",
                   ".spec.ts", ".spec.tsx", ".spec.js", ".spec.jsx",
                   ".spec.mjs", ".spec.cjs"):
        if name.endswith(suffix):
            return True
    return False


def _bucket(path: str) -> str | None:
    """Return ``py``/``go``/``ts`` for a file extension, else None.

    Extension lists are shared with lint_gate via ``_common`` so polyglot
    diffs route consistently across both gates.
    """
    p = path.lower()
    if p.endswith(PY_EXTENSIONS):
        return "py"
    if p.endswith(GO_EXTENSIONS):
        return "go"
    if p.endswith(JS_TS_EXTENSIONS):
        return "ts"
    return None


def _walk_tests(repo_root: Path, dir_path: Path,
                lang: str) -> Iterable[Path]:
    """Yield co-located test files under ``dir_path`` matching ``lang``.

    For Python and TS/JS this descends — a changed source file in a
    package directory should pull tests from a nested ``tests/`` or
    ``__tests__/`` subdir. For Go we stay shallow because Go's testing
    convention is strictly per-package (sibling ``_test.go`` files only).
    """
    if not dir_path.is_dir():
        return
    if lang == "go":
        # Sibling _test.go files in the same package only.
        for entry in dir_path.iterdir():
            if entry.is_file() and _is_go_test(entry.name):
                yield entry
        return

    # Python and TS/JS: walk down into tests/ and __tests__/ subtrees.
    for current, dirs, files in os.walk(dir_path):
        # Skip vendored / heavyweight subtrees that wouldn't host
        # in-repo tests but bloat traversal time on monorepos.
        dirs[:] = [d for d in dirs if d not in (
            "node_modules", ".git", "vendor", "dist", "build", "__pycache__")]
        cur = Path(current)
        in_py_tests_dir = lang == "py" and "tests" in cur.parts
        for fname in files:
            if lang == "py":
                # Include any .py inside a tests/ tree (conftest.py, fixtures,
                # shared helpers) so the reviewer sees the full picture, not
                # only files whose name matches test_*.py / *_test.py.
                if _is_python_test(fname) or (in_py_tests_dir and fname.endswith(".py")):
                    yield cur / fname
            elif lang == "ts" and _is_ts_js_test(fname):
                yield cur / fname
            elif "__tests__" in cur.parts and lang == "ts":
                yield cur / fname


def collect_test_paths(repo_root: Path, changed: list[str]) -> list[str]:
    """For each changed source file, find co-located tests.

    The dirs we look at are:
      - The file's parent directory (sibling tests).
      - For Python: a sibling ``tests`` dir at the same level.
      - For TS/JS: a sibling ``__tests__`` dir at the same level.
      - The package root one level up (covers ``src/foo.py`` ↔ ``tests/test_foo.py``
        layouts where tests are a sibling of ``src``).

    Returns sorted, deduplicated, repo-relative paths. The caller caps
    at MAX_TEST_PATHS — we leave that to the caller so the truncation
    happens once, after the union across all changed files.
    """
    found: set[str] = set()
    # Key by (directory, language) so a polyglot package — e.g. a folder
    # holding both .py and .ts files — gets walked once per language.
    # Keying by directory alone would let the first language seen suppress
    # the others' test enumeration.
    seen_dirs: set[tuple[Path, str]] = set()

    for rel in changed:
        lang = _bucket(rel)
        if lang is None:
            continue

        abs_path = (repo_root / rel).resolve()
        candidate_dirs: list[Path] = []
        # Sibling tests/__tests__ at the same level. Skip the bare
        # parent when it IS repo_root — recursing from repo_root
        # would scan every package in the tree and pull unrelated
        # tests into scope. For root-level files we still need to
        # find sibling foo_test.py / foo.test.ts at repo_root, so do
        # a *shallow* (non-recursive) scan via _walk_root_siblings
        # below instead of feeding the directory to _walk_tests.
        parent_at_root = abs_path.parent == repo_root
        if not parent_at_root:
            candidate_dirs.append(abs_path.parent)
        if lang == "py":
            candidate_dirs.append(abs_path.parent / "tests")
        elif lang == "ts":
            candidate_dirs.append(abs_path.parent / "__tests__")
        # Walk up to MAX_ANCESTORS levels looking for tests/ or
        # __tests__ subdirs (and the ancestor directory itself, since
        # ``pkg/foo_test.py`` is a valid pytest layout). ``src/`` is
        # the canonical case but layouts like ``pkg/module/foo.py``
        # with tests at ``pkg/tests/`` or ``pkg/`` also need coverage.
        # Bounded so we don't scan the whole repo tree on a deeply
        # nested file.
        MAX_ANCESTORS = 3
        ancestor = abs_path.parent
        for _ in range(MAX_ANCESTORS):
            # Stop climbing if we've already reached repo_root — the
            # canonical repo_root/tests was added (or skipped) before
            # the loop. Climbing above repo_root would scan every
            # package in the tree and pull unrelated tests into scope.
            if ancestor == repo_root:
                break
            ancestor = ancestor.parent
            if ancestor.parent == ancestor:
                # Filesystem root; defensive belt-and-braces.
                break
            at_root = ancestor == repo_root
            # Bare ancestor is needed for ``pkg/foo_test.py``-style
            # layouts (test sits next to the package, not in a
            # tests/ subdir) — but never at repo_root, where it
            # would scan every package.
            if not at_root:
                candidate_dirs.append(ancestor)
            if lang == "py":
                candidate_dirs.append(ancestor / "tests")
            elif lang == "ts":
                candidate_dirs.append(ancestor / "__tests__")

        for d in candidate_dirs:
            key = (d, lang)
            if key in seen_dirs:
                continue
            seen_dirs.add(key)
            for test_file in _walk_tests(repo_root, d, lang):
                try:
                    rel_test = test_file.relative_to(repo_root)
                except ValueError:
                    # Symlink or otherwise outside repo_root; skip.
                    continue
                found.add(str(rel_test))

        # Root-level file: scan repo_root *non-recursively* for sibling
        # test files (foo_test.py next to foo.py at the repo root).
        # We can't add repo_root as a candidate_dir because _walk_tests
        # descends into every subpackage; do the shallow scan directly.
        if parent_at_root:
            for entry in repo_root.iterdir():
                if not entry.is_file():
                    continue
                fname = entry.name
                hit = False
                if lang == "py" and _is_python_test(fname):
                    hit = True
                elif lang == "ts" and _is_ts_js_test(fname):
                    hit = True
                elif lang == "go" and _is_go_test(fname):
                    hit = True
                if hit:
                    found.add(fname)

    return sorted(found)


# ---------------------------------------------------------------------------
# Multi-package detection
# ---------------------------------------------------------------------------


# Default cross-service roots. Buckets are rooted at the first ``depth``
# segments under each prefix. The kernel/yoloswe layouts are covered:
#   - ``services/python/foo/`` → bucket ``services/python/foo``
#   - ``services/typescript/bar/`` → bucket ``services/typescript/bar``
#   - top-level dirs (``bramble``, ``yoloswe``, ``jiradozer``, ...) →
#     each becomes its own bucket via the default depth=1 fallback.
DEFAULT_CROSS_SERVICE_ROOTS: tuple[tuple[str, int], ...] = (
    ("services/", 3),
    ("apps/", 2),
    ("packages/", 2),
)


def _bucket_path(path: str,
                 roots: tuple[tuple[str, int], ...]) -> str | None:
    """Return the top-level bucket name for a path, or None to skip.

    Falls back to the first path segment when no configured root matches,
    so a flat monorepo with top-level Go packages still buckets cleanly.
    Files at the repo root (no ``/``) are not bucketed — they're usually
    config/CI/docs and shouldn't trigger a multi-package sweep alone.
    """
    parts = path.split("/")
    if len(parts) < 2:
        return None
    for prefix, depth in roots:
        if path.startswith(prefix):
            if len(parts) < depth:
                return None
            return "/".join(parts[:depth])
    return parts[0]


def detect_cross_service_packages(
    paths: list[str],
    roots: tuple[tuple[str, int], ...] = DEFAULT_CROSS_SERVICE_ROOTS,
) -> list[str]:
    """Return the sorted list of top-level packages touched, or [] when
    the threshold for a cross-service sweep isn't met.

    Triggering requires both:
      - >= MIN_PACKAGES_FOR_SWEEP distinct buckets;
      - >= MIN_FILES_FOR_SWEEP changed files total.

    The file-count gate filters out trivial cross-cutting tweaks (e.g.
    a one-line copyright update touching every package) that nominally
    span two trees but don't have producer/consumer surface to sweep.
    """
    if len(paths) < MIN_FILES_FOR_SWEEP:
        return []
    buckets: set[str] = set()
    for p in paths:
        b = _bucket_path(p, roots)
        if b is not None:
            buckets.add(b)
    if len(buckets) < MIN_PACKAGES_FOR_SWEEP:
        return []
    return sorted(buckets)


def split_changed_dependency_packages(
    paths: list[str],
    roots: tuple[tuple[str, int], ...] = DEFAULT_CROSS_SERVICE_ROOTS,
) -> tuple[list[str], list[str]]:
    """Split the touched buckets into (changed_packages, dependency_packages).

    Strategy: count changed files per bucket. The bucket(s) with the most
    changed files are the "primary changed" packages; the remainder are
    treated as callers/dependencies. Ties go to changed (both sides changed
    heavily means both are primary). Returns ([], []) when the sweep
    threshold isn't met.

    This heuristic is intentionally simple — import graph analysis would be
    more precise but adds external dependencies and parse complexity for a
    marginal gain. The dominant-bucket signal is usually unambiguous in
    practice (a backend service PR that also bumps a shared proto file).
    """
    if len(paths) < MIN_FILES_FOR_SWEEP:
        return [], []

    counts: dict[str, int] = {}
    for p in paths:
        b = _bucket_path(p, roots)
        if b is not None:
            counts[b] = counts.get(b, 0) + 1

    if len(counts) < MIN_PACKAGES_FOR_SWEEP:
        return [], []

    max_count = max(counts.values())
    changed: list[str] = sorted(b for b, c in counts.items() if c == max_count)
    dependency: list[str] = sorted(b for b, c in counts.items() if c < max_count)
    return changed, dependency


def parse_cross_service_roots(spec: str) -> tuple[tuple[str, int], ...]:
    """Parse a CSV of ``prefix:depth`` (or bare ``prefix`` defaulting to
    depth=2) into the tuple form ``_bucket_path`` expects. Trailing
    slashes on the prefix are required so substring matches don't
    accidentally cross dir boundaries.
    """
    out: list[tuple[str, int]] = []
    for entry in spec.split(","):
        entry = entry.strip()
        if not entry:
            continue
        if ":" in entry:
            prefix, depth_s = entry.rsplit(":", 1)
            try:
                depth = int(depth_s)
            except ValueError:
                # Malformed "prefix:abc" — drop the entry entirely
                # rather than silently using "prefix:abc/" as a path
                # prefix (which never matches anything but pollutes
                # logs). Prior fix used `prefix=entry` which baked the
                # bad token into the live config.
                import sys as _sys  # noqa: PLC0415
                print(
                    f"scope_gate: ignoring --cross-service-roots entry "
                    f"with non-numeric depth: {entry!r}",
                    file=_sys.stderr,
                )
                continue
        else:
            prefix, depth = entry, 2
        if not prefix.endswith("/"):
            prefix += "/"
        out.append((prefix, depth))
    return tuple(out)


# ---------------------------------------------------------------------------
# Envelope assembly
# ---------------------------------------------------------------------------


def build_hints(
    test_paths: list[str],
    cross_service_packages: list[str],
    changed_packages: list[str] | None = None,
    dependency_packages: list[str] | None = None,
) -> dict:
    """Return the on-disk JSON shape ``reviewer.ScopeHints`` expects.

    Caps test_paths at MAX_TEST_PATHS — bramble's prompt builder also
    caps but we apply it pre-write so the file on disk reflects what
    will actually flow into the prompt. Saves an audit-trail surprise.

    Always emits ``schema_version = SCHEMA_VERSION`` (currently 2). Optional
    ``changed_packages`` / ``dependency_packages`` keys are included only
    when supplied; their absence does not downgrade the schema. Sync with
    ``reviewer.ScopeHintsSchemaVersion`` in
    ``yoloswe/reviewer/scope_hints.go`` when bumping.
    """
    if len(test_paths) > MAX_TEST_PATHS:
        test_paths = test_paths[:MAX_TEST_PATHS]
    out: dict = {
        "schema_version": SCHEMA_VERSION,
        "test_paths": test_paths,
        "cross_service_packages": cross_service_packages,
    }
    if changed_packages is not None:
        out["changed_packages"] = changed_packages
    if dependency_packages is not None:
        out["dependency_packages"] = dependency_packages
    return out


def hints_path(state_dir: Path) -> Path:
    """``<state_dir>/scope-hints.json``. No ``r<n>/`` subdir — we
    overwrite per round; the audit trail lives in bramble's run logs.
    """
    return state_dir / "scope-hints.json"


# ---------------------------------------------------------------------------
# CLI
# ---------------------------------------------------------------------------


def _build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(prog="scope_gate")
    p.add_argument("--state-dir", required=True,
                   help="<state_dir> from pr_ops identify")
    p.add_argument("--base", default=None,
                   help="base branch (default: auto-detect via origin/HEAD)")
    p.add_argument("--cross-service-roots", default=None,
                   help="CSV of prefix:depth pairs overriding the default "
                        "monorepo bucketing (e.g. 'services/:3,apps/:2'). "
                        "Trailing slash on the prefix is required.")
    return p


def main(argv: list[str] | None = None) -> int:
    args = _build_parser().parse_args(argv)
    base = args.base or detect_base_branch()
    state_dir = Path(args.state_dir)
    out_path = hints_path(state_dir)

    # Repo root is the cwd's git toplevel — same convention as lint_gate.
    res = run(["git", "rev-parse", "--show-toplevel"], check=False)
    if res.returncode != 0:
        # Not inside a git repo. Emit an empty hints file and exit 0
        # so the orchestrator's wire-up doesn't have to special-case
        # "git missing"; bramble's malformed-file fallback handles it.
        atomic_write_json(out_path, build_hints([], []))
        print(out_path)
        return 0
    # ``.resolve()`` so the repo_root containment guards in
    # collect_test_paths (abs_path.parent == repo_root, ancestor ==
    # repo_root) compare canonical paths on both sides — symlink
    # mismatches would otherwise silently break those checks.
    repo_root = Path(res.stdout.strip()).resolve()

    try:
        files = changed_files(base)
    except CommandError as e:
        print(f"scope_gate: changed_files failed: {e}", file=sys.stderr)
        files = []

    if args.cross_service_roots:
        roots = parse_cross_service_roots(args.cross_service_roots)
    else:
        roots = DEFAULT_CROSS_SERVICE_ROOTS

    test_paths = collect_test_paths(repo_root, files) if files else []
    cross_pkgs = detect_cross_service_packages(files, roots) if files else []
    changed_pkgs, dep_pkgs = split_changed_dependency_packages(files, roots) if files else ([], [])
    atomic_write_json(out_path, build_hints(test_paths, cross_pkgs, changed_pkgs, dep_pkgs))
    print(out_path)
    return 0


if __name__ == "__main__":  # pragma: no cover
    raise SystemExit(main())
