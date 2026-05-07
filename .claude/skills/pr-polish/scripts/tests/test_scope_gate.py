"""Unit tests for scope_gate. Hermetic: no real git, no network."""

from __future__ import annotations

import json
import sys
import tempfile
import unittest
from pathlib import Path
from unittest.mock import patch

HERE = Path(__file__).resolve().parent
PARENT = HERE.parent
for p in (str(PARENT), str(HERE)):
    if p not in sys.path:
        sys.path.insert(0, p)

import scope_gate  # noqa: E402
from _common import RunResult  # noqa: E402


def _stub_run(stdout: str, returncode: int = 0):
    """Build a fake _common.run that always returns the given stdout."""

    def fake(cmd, *, check=True, env=None, cwd=None, input_text=None, timeout=None):
        return RunResult(stdout=stdout, stderr="", returncode=returncode)

    return fake


class TestBucketPath(unittest.TestCase):
    def test_services_uses_three_segments(self) -> None:
        # services/<lang>/<svc>/... is the kernel layout — depth=3 keeps
        # tenant-service and api-gateway-service as distinct buckets even
        # though they share the same Python prefix.
        self.assertEqual(
            scope_gate._bucket_path(
                "services/python/tenant-service/src/x.py",
                scope_gate.DEFAULT_CROSS_SERVICE_ROOTS,
            ),
            "services/python/tenant-service",
        )
        self.assertEqual(
            scope_gate._bucket_path(
                "services/typescript/forge-v2/src/x.tsx",
                scope_gate.DEFAULT_CROSS_SERVICE_ROOTS,
            ),
            "services/typescript/forge-v2",
        )

    def test_top_level_dir_falls_through(self) -> None:
        # yoloswe-style: bramble/, yoloswe/, jiradozer/ each become their
        # own bucket without a configured prefix match.
        self.assertEqual(
            scope_gate._bucket_path(
                "bramble/cmd/codereview/codereview.go",
                scope_gate.DEFAULT_CROSS_SERVICE_ROOTS,
            ),
            "bramble",
        )

    def test_repo_root_files_skip(self) -> None:
        # README.md / Makefile at repo root shouldn't ever be the only
        # signal that triggers a multi-package sweep.
        self.assertIsNone(
            scope_gate._bucket_path(
                "README.md", scope_gate.DEFAULT_CROSS_SERVICE_ROOTS
            )
        )

    def test_too_short_for_prefix_depth(self) -> None:
        # ``services/python`` lacks the third segment (the service name);
        # bucketing returns None rather than producing a stub like
        # ``services/python`` because that would cluster every Python
        # service under one bucket and miss real cross-service edits.
        self.assertIsNone(
            scope_gate._bucket_path(
                "services/python", scope_gate.DEFAULT_CROSS_SERVICE_ROOTS
            )
        )


class TestDetectCrossServicePackages(unittest.TestCase):
    def test_below_file_threshold_no_trigger(self) -> None:
        # Two packages but only two files — too small for a meaningful
        # contract sweep. Filter cuts noise from one-line cross-cutting
        # tweaks (e.g. a copyright bump) that nominally span trees.
        paths = [
            "services/python/a/src/x.py",
            "services/python/b/src/y.py",
        ]
        self.assertEqual(scope_gate.detect_cross_service_packages(paths), [])

    def test_single_package_no_trigger(self) -> None:
        # All five files in one bucket — nothing to sweep across.
        paths = [
            "services/python/a/src/x.py",
            "services/python/a/src/y.py",
            "services/python/a/src/z.py",
            "services/python/a/tests/test_x.py",
            "services/python/a/tests/test_y.py",
        ]
        self.assertEqual(scope_gate.detect_cross_service_packages(paths), [])

    def test_threshold_met(self) -> None:
        # Two packages, three files total — minimum trigger.
        paths = [
            "services/python/a/src/x.py",
            "services/python/b/src/y.py",
            "services/python/b/tests/test_y.py",
        ]
        self.assertEqual(
            scope_gate.detect_cross_service_packages(paths),
            ["services/python/a", "services/python/b"],
        )

    def test_three_buckets_sorted(self) -> None:
        # kernel-2998-shaped diff: tenant-service (Python) + forge-v2
        # (TypeScript). Output is sorted for deterministic prompts.
        paths = [
            "services/typescript/forge-v2/src/components/x.tsx",
            "services/python/tenant-service/src/api/v1/y.py",
            "services/python/tenant-service/migrations/z.py",
            "services/python/tenant-service/src/services/w.py",
        ]
        got = scope_gate.detect_cross_service_packages(paths)
        self.assertEqual(
            got,
            [
                "services/python/tenant-service",
                "services/typescript/forge-v2",
            ],
        )

    def test_custom_roots(self) -> None:
        # ``modules/<name>/...`` layout, depth=2 — bucket on first two segs.
        paths = [
            "modules/auth/src/x.go",
            "modules/billing/src/y.go",
            "modules/auth/tests/x_test.go",
        ]
        roots = scope_gate.parse_cross_service_roots("modules/:2")
        self.assertEqual(
            scope_gate.detect_cross_service_packages(paths, roots),
            ["modules/auth", "modules/billing"],
        )


class TestSplitChangedDependencyPackages(unittest.TestCase):
    def test_below_file_threshold_returns_empty(self) -> None:
        # Same MIN_FILES_FOR_SWEEP gate as detect_cross_service_packages.
        paths = [
            "services/python/a/src/x.py",
            "services/python/b/src/y.py",
        ]
        self.assertEqual(
            scope_gate.split_changed_dependency_packages(paths),
            ([], []),
        )

    def test_below_package_threshold_returns_empty(self) -> None:
        # Three files but all in one bucket → not multi-package.
        paths = [
            "services/python/a/src/x.py",
            "services/python/a/src/y.py",
            "services/python/a/src/z.py",
        ]
        self.assertEqual(
            scope_gate.split_changed_dependency_packages(paths),
            ([], []),
        )

    def test_dominant_bucket_split(self) -> None:
        # b has 2 changed files, a has 1 → b is changed, a is dependency.
        paths = [
            "services/python/a/src/x.py",
            "services/python/b/src/y.py",
            "services/python/b/src/z.py",
        ]
        changed, dep = scope_gate.split_changed_dependency_packages(paths)
        self.assertEqual(changed, ["services/python/b"])
        self.assertEqual(dep, ["services/python/a"])

    def test_tie_treated_as_co_changed(self) -> None:
        # Two packages tied at the max → both are "changed", neither is
        # a dependency. Models reviewing both as primary is the right
        # default; we don't have signal to pick one.
        paths = [
            "services/python/a/src/x.py",
            "services/python/a/src/y.py",
            "services/python/b/src/y.py",
            "services/python/b/src/z.py",
        ]
        changed, dep = scope_gate.split_changed_dependency_packages(paths)
        self.assertEqual(
            changed,
            ["services/python/a", "services/python/b"],
        )
        self.assertEqual(dep, [])

    def test_three_buckets_one_dominant(self) -> None:
        paths = [
            "services/python/tenant-service/src/x.py",
            "services/python/tenant-service/src/y.py",
            "services/python/tenant-service/src/z.py",
            "services/typescript/forge-v2/src/a.tsx",
            "services/python/billing/src/b.py",
        ]
        changed, dep = scope_gate.split_changed_dependency_packages(paths)
        self.assertEqual(changed, ["services/python/tenant-service"])
        self.assertEqual(
            dep,
            [
                "services/python/billing",
                "services/typescript/forge-v2",
            ],
        )

    def test_custom_roots(self) -> None:
        # Verify the same depth-aware bucketing works under custom roots.
        paths = [
            "modules/auth/src/x.go",
            "modules/auth/src/y.go",
            "modules/billing/src/z.go",
        ]
        roots = scope_gate.parse_cross_service_roots("modules/:2")
        changed, dep = scope_gate.split_changed_dependency_packages(
            paths, roots,
        )
        self.assertEqual(changed, ["modules/auth"])
        self.assertEqual(dep, ["modules/billing"])


class TestParseCrossServiceRoots(unittest.TestCase):
    def test_csv_with_depths(self) -> None:
        roots = scope_gate.parse_cross_service_roots("services/:3,apps/:2")
        self.assertEqual(roots, (("services/", 3), ("apps/", 2)))

    def test_bare_prefix_defaults_to_two(self) -> None:
        # ``modules/`` without ``:depth`` defaults to depth=2 — the
        # commonest layout in the wild.
        roots = scope_gate.parse_cross_service_roots("modules/")
        self.assertEqual(roots, (("modules/", 2),))

    def test_appends_trailing_slash(self) -> None:
        # Forgetting the trailing slash on the prefix would cause a
        # substring match across dir boundaries (``services-v2/`` would
        # match ``services/``). The parser fixes it up.
        roots = scope_gate.parse_cross_service_roots("services:3")
        self.assertEqual(roots, (("services/", 3),))

    def test_malformed_depth_dropped_with_warning(self) -> None:
        # Round 17 fix: prior r16 fallback set prefix=entry on a
        # non-numeric depth like "services:typo", baking the bad
        # token in as a path prefix. The entry is now dropped
        # entirely (drop-with-warning).
        import io, contextlib  # noqa: PLC0415
        buf = io.StringIO()
        with contextlib.redirect_stderr(buf):
            roots = scope_gate.parse_cross_service_roots("services:typo,apps/:2")
        self.assertEqual(roots, (("apps/", 2),))
        self.assertIn("non-numeric depth", buf.getvalue())

    def test_non_positive_depth_dropped_with_warning(self) -> None:
        # Round 27 fix: depth=0 or negative produces empty buckets;
        # drop with a stderr warning rather than silently emit wrong
        # scope hints.
        import io, contextlib  # noqa: PLC0415
        buf = io.StringIO()
        with contextlib.redirect_stderr(buf):
            roots = scope_gate.parse_cross_service_roots(
                "services/:0,apps/:2,bad/:-1"
            )
        self.assertEqual(roots, (("apps/", 2),))
        self.assertIn("non-positive depth", buf.getvalue())


class TestCollectTestPaths(unittest.TestCase):
    """Walks a real on-disk tree under tmpdir.

    Tests build a fake repo with tempfile so the path-walking logic runs
    against actual ``os.walk`` rather than mocked filesystem APIs.
    """

    def _make_tree(self, files: list[str]) -> Path:
        root = Path(tempfile.mkdtemp())
        self.addCleanup(self._rmtree, root)
        for rel in files:
            p = root / rel
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_text("# stub\n")
        return root

    @staticmethod
    def _rmtree(path: Path) -> None:
        import shutil

        shutil.rmtree(path, ignore_errors=True)

    def test_python_sibling_test(self) -> None:
        # Most basic case: changed src/foo.py picks up sibling
        # tests/test_foo.py via the src→pkg-root candidate.
        root = self._make_tree(
            [
                "pkg/src/foo.py",
                "pkg/tests/test_foo.py",
                "pkg/tests/test_bar.py",  # not directly a sibling but in tests/
                "unrelated/src/x.py",
                "unrelated/tests/test_x.py",
            ]
        )
        got = scope_gate.collect_test_paths(root, ["pkg/src/foo.py"])
        self.assertIn("pkg/tests/test_foo.py", got)
        self.assertIn("pkg/tests/test_bar.py", got)
        # Tests for unrelated packages must not leak in.
        self.assertNotIn("unrelated/tests/test_x.py", got)

    def test_python_underscore_test_suffix(self) -> None:
        # Both ``test_foo.py`` and ``foo_test.py`` are valid pytest names.
        root = self._make_tree(
            [
                "pkg/src/foo.py",
                "pkg/foo_test.py",  # sibling, _test.py suffix
            ]
        )
        got = scope_gate.collect_test_paths(root, ["pkg/src/foo.py"])
        self.assertIn("pkg/foo_test.py", got)

    def test_go_sibling_only(self) -> None:
        # Go's testing convention is strictly per-package: sibling
        # ``_test.go`` files only. We must NOT descend into ``tests/``
        # subdirs (no such Go convention) — that would pull in testdata
        # files or unrelated packages.
        root = self._make_tree(
            [
                "pkg/foo.go",
                "pkg/foo_test.go",
                "pkg/sub/bar_test.go",  # different package; should not appear
            ]
        )
        got = scope_gate.collect_test_paths(root, ["pkg/foo.go"])
        self.assertEqual(got, ["pkg/foo_test.go"])

    def test_ts_test_and_spec_suffixes(self) -> None:
        # Jest/Vitest accept .test.ts(x) and .spec.ts(x); both must be
        # picked up. ``__tests__`` dir is also recognized.
        root = self._make_tree(
            [
                "ui/comp.tsx",
                "ui/comp.test.tsx",
                "ui/comp.spec.tsx",
                "ui/__tests__/comp.helper.ts",
                "ui/sibling.ts",  # not a test
            ]
        )
        got = scope_gate.collect_test_paths(root, ["ui/comp.tsx"])
        self.assertIn("ui/comp.test.tsx", got)
        self.assertIn("ui/comp.spec.tsx", got)
        self.assertIn("ui/__tests__/comp.helper.ts", got)
        self.assertNotIn("ui/sibling.ts", got)

    def test_mjs_cjs_test_suffixes(self) -> None:
        # ``_bucket`` routes .mjs/.cjs into the JS bucket, so co-located
        # *.test.mjs / *.spec.cjs must be picked up too. Without this the
        # JS bucket would silently drop tests for those module formats.
        root = self._make_tree(
            [
                "esm/foo.mjs",
                "esm/foo.test.mjs",
                "esm/foo.spec.mjs",
                "cjs/bar.cjs",
                "cjs/bar.test.cjs",
                "cjs/bar.spec.cjs",
            ]
        )
        got = scope_gate.collect_test_paths(root, ["esm/foo.mjs", "cjs/bar.cjs"])
        self.assertIn("esm/foo.test.mjs", got)
        self.assertIn("esm/foo.spec.mjs", got)
        self.assertIn("cjs/bar.test.cjs", got)
        self.assertIn("cjs/bar.spec.cjs", got)

    def test_ancestor_walk_does_not_recurse_into_sibling_subpackages(self) -> None:
        # Round 31 fix: bare-ancestor candidates were fed to
        # _walk_tests recursively, which pulled tests from sibling
        # subpackages of the ancestor. A change in pkg/module/foo.py
        # leaked pkg/other/test_bar.py into scope. Now bare ancestors
        # use a shallow same-level scan; only tests/__tests__ subtrees
        # are walked recursively.
        root = self._make_tree(
            [
                "pkg/module/foo.py",
                "pkg/module/test_foo.py",  # canonical sibling — kept
                "pkg/other/bar.py",
                "pkg/other/test_bar.py",  # MUST NOT leak in
                "pkg/foo_test.py",  # bare-ancestor sibling — kept
                "pkg/tests/test_module.py",  # canonical tests/ subtree — kept
            ]
        )
        got = scope_gate.collect_test_paths(root, ["pkg/module/foo.py"])
        self.assertIn("pkg/module/test_foo.py", got)
        self.assertIn("pkg/foo_test.py", got)
        self.assertIn("pkg/tests/test_module.py", got)
        self.assertNotIn("pkg/other/test_bar.py", got)

    def test_go_strictly_per_package_no_ancestor_walk(self) -> None:
        # Round 23 fix: Go's testing convention is strictly per-package.
        # The ancestor walk previously included Go too, so a change in
        # pkg/sub/foo.go would pull _test.go files from pkg/, violating
        # _walk_tests's documented "sibling only for Go" contract.
        root = self._make_tree(
            [
                "pkg/sub/foo.go",
                "pkg/sub/foo_test.go",  # canonical sibling — kept
                "pkg/foo_test.go",  # ancestor leak — must NOT appear
                "other/_test.go",  # unrelated package — must NOT appear
            ]
        )
        got = scope_gate.collect_test_paths(root, ["pkg/sub/foo.go"])
        self.assertIn("pkg/sub/foo_test.go", got)
        self.assertNotIn("pkg/foo_test.go", got)
        self.assertNotIn("other/_test.go", got)

    def test_root_level_file_finds_sibling_test_at_root(self) -> None:
        # Round 17 fix: repo-root containment guard from r16 had also
        # disabled finding sibling foo_test.py at the repo root. The
        # shallow non-recursive scan added in r17 restores that.
        root = self._make_tree(
            [
                "foo.py",
                "foo_test.py",
                "tests/test_foo.py",  # also valid; both should appear
                "unrelated/tests/test_x.py",  # MUST NOT leak
            ]
        )
        got = scope_gate.collect_test_paths(root, ["foo.py"])
        self.assertIn("foo_test.py", got)
        self.assertIn("tests/test_foo.py", got)
        self.assertNotIn("unrelated/tests/test_x.py", got)

    def test_changed_file_at_repo_root_does_not_scan_whole_tree(self) -> None:
        # Round 16 fix: when the changed file lives at repo_root
        # itself (foo.py with no package wrapper), abs_path.parent IS
        # repo_root. The walker must NOT recurse from repo_root or
        # it would pull unrelated package tests into scope.
        root = self._make_tree(
            [
                "foo.py",  # changed file at repo root
                "tests/test_foo.py",  # canonical sibling
                "unrelated/tests/test_x.py",  # MUST NOT leak in
                "another/tests/test_y.py",  # MUST NOT leak in
            ]
        )
        got = scope_gate.collect_test_paths(root, ["foo.py"])
        self.assertIn("tests/test_foo.py", got)
        self.assertNotIn("unrelated/tests/test_x.py", got)
        self.assertNotIn("another/tests/test_y.py", got)

    def test_finds_tests_at_repo_root(self) -> None:
        # Common layout: src/foo.py with tests/test_foo.py at the repo
        # root. The ancestor walker must include repo_root/tests, not
        # bail out the moment it reaches repo_root.
        root = self._make_tree(
            [
                "src/foo.py",
                "tests/test_foo.py",
            ]
        )
        got = scope_gate.collect_test_paths(root, ["src/foo.py"])
        self.assertIn("tests/test_foo.py", got)

    def test_walks_up_to_find_tests_in_non_src_layout(self) -> None:
        # Common layout without ``src/``: pkg/module/foo.py with tests
        # at pkg/tests/. Round 12 broadened the ancestor search beyond
        # the literal ``src`` parent so these tests get picked up.
        root = self._make_tree(
            [
                "pkg/module/foo.py",
                "pkg/tests/test_foo.py",
            ]
        )
        got = scope_gate.collect_test_paths(root, ["pkg/module/foo.py"])
        self.assertIn("pkg/tests/test_foo.py", got)

    def test_dedupe_across_changed_files(self) -> None:
        # Two source files in the same package both pull the same
        # tests/test_foo.py — output must be deduped.
        root = self._make_tree(
            [
                "pkg/src/a.py",
                "pkg/src/b.py",
                "pkg/tests/test_a.py",
            ]
        )
        got = scope_gate.collect_test_paths(
            root, ["pkg/src/a.py", "pkg/src/b.py"]
        )
        self.assertEqual(got.count("pkg/tests/test_a.py"), 1)

    def test_skips_node_modules(self) -> None:
        # Heavy vendored trees would explode the walk and yield nothing
        # useful. Confirm node_modules is pruned even when a changed
        # file's parent has one.
        root = self._make_tree(
            [
                "ui/comp.tsx",
                "ui/node_modules/foo/src/x.test.ts",
            ]
        )
        got = scope_gate.collect_test_paths(root, ["ui/comp.tsx"])
        self.assertEqual(got, [])

    def test_non_source_file_ignored(self) -> None:
        # README/Markdown changes shouldn't pull in any tests — the
        # bucket is None and the changed file is skipped entirely.
        root = self._make_tree(
            [
                "README.md",
                "pkg/src/foo.py",
                "pkg/tests/test_foo.py",
            ]
        )
        got = scope_gate.collect_test_paths(root, ["README.md"])
        self.assertEqual(got, [])


class TestBuildHints(unittest.TestCase):
    def test_caps_test_paths_at_50(self) -> None:
        # 73 paths in → 50 out. Truncation happens here (pre-write) so
        # the on-disk file matches what bramble's prompt actually sees.
        many = [f"pkg/tests/t_{i:03d}.py" for i in range(73)]
        h = scope_gate.build_hints(many, [])
        self.assertEqual(len(h["test_paths"]), 50)
        self.assertEqual(h["test_paths"][0], "pkg/tests/t_000.py")
        self.assertEqual(h["test_paths"][49], "pkg/tests/t_049.py")

    def test_schema_version(self) -> None:
        h = scope_gate.build_hints([], [])
        self.assertEqual(h["schema_version"], scope_gate.SCHEMA_VERSION)
        # Must match the Go-side constant exactly. If this changes,
        # update reviewer.ScopeHintsSchemaVersion on the same PR.
        self.assertEqual(h["schema_version"], 2)

    def test_keys_present_when_empty(self) -> None:
        # Empty arrays are a valid "no clause" signal — bramble
        # specifically accepts them. Don't omit the keys.
        h = scope_gate.build_hints([], [])
        self.assertEqual(h["test_paths"], [])
        self.assertEqual(h["cross_service_packages"], [])


class TestChangedFiles(unittest.TestCase):
    def test_git_failure_returns_empty(self) -> None:
        # Shallow clone / missing remote / fork-of-fork — bramble's
        # malformed-file fallback handles the empty hints file we emit.
        # ``changed_files`` lives in _common.py and calls _common.run, so
        # patching scope_gate.run alone wouldn't intercept the subprocess.
        import _common  # noqa: PLC0415
        with patch.object(_common, "run", _stub_run("", returncode=128)):
            self.assertEqual(scope_gate.changed_files("main"), [])

    def test_strips_blank_lines(self) -> None:
        # ``git diff --name-only`` always trails a newline; the helper
        # must drop that and any blank lines between renames.
        import _common  # noqa: PLC0415
        out = "a/foo.py\nb/bar.go\n\nc/baz.ts\n"
        with patch.object(_common, "run", _stub_run(out)):
            got = scope_gate.changed_files("main")
        self.assertEqual(got, ["a/foo.py", "b/bar.go", "c/baz.ts"])


class TestMainCLI(unittest.TestCase):
    """End-to-end through ``main`` with a hermetic git+filesystem stub."""

    def test_writes_hints_file(self) -> None:
        root = Path(tempfile.mkdtemp())
        self.addCleanup(self._rmtree, root)
        # Build a tiny fake repo: src files + co-located tests.
        for rel in [
            "services/python/a/src/x.py",
            "services/python/a/tests/test_x.py",
            "services/python/b/src/y.py",
            "services/python/b/tests/test_y.py",
        ]:
            p = root / rel
            p.parent.mkdir(parents=True, exist_ok=True)
            p.write_text("# stub\n")

        state_dir = root / ".state"
        state_dir.mkdir()

        # First call: ``git rev-parse --show-toplevel`` → repo root.
        # Second call: ``git diff --name-only ...`` → the diff files.
        # detect_base_branch also calls git symbolic-ref; stub it too.
        diff_out = (
            "services/python/a/src/x.py\n"
            "services/python/b/src/y.py\n"
            "services/python/b/tests/test_y.py\n"
        )
        calls = []

        def fake_run(cmd, *, check=True, env=None, cwd=None,
                     input_text=None, timeout=None):
            calls.append(cmd)
            if cmd[:2] == ["git", "rev-parse"]:
                return RunResult(stdout=str(root) + "\n", stderr="",
                                 returncode=0)
            if cmd[:2] == ["git", "symbolic-ref"]:
                return RunResult(stdout="refs/remotes/origin/main\n",
                                 stderr="", returncode=0)
            if cmd[:2] == ["git", "diff"]:
                return RunResult(stdout=diff_out, stderr="", returncode=0)
            if cmd[:3] == ["git", "remote", "set-head"]:
                return RunResult(stdout="", stderr="", returncode=0)
            return RunResult(stdout="", stderr="", returncode=0)

        # ``run`` is imported into both modules; patch each at the
        # module level so any callsite is covered.
        with patch.object(scope_gate, "run", fake_run), \
             patch("_common.run", fake_run):
            rc = scope_gate.main(["--state-dir", str(state_dir)])
        self.assertEqual(rc, 0)

        out_path = state_dir / "scope-hints.json"
        self.assertTrue(out_path.exists())
        data = json.loads(out_path.read_text())
        self.assertEqual(data["schema_version"], 2)
        # Both packages bucketed.
        self.assertEqual(
            sorted(data["cross_service_packages"]),
            ["services/python/a", "services/python/b"],
        )
        # Both co-located tests included; sorted order.
        self.assertIn("services/python/a/tests/test_x.py", data["test_paths"])
        self.assertIn("services/python/b/tests/test_y.py", data["test_paths"])
        # v2 split: bucket b has 2 changed files, bucket a has 1 → b
        # is the dominant changed package, a is its dependency.
        self.assertEqual(data["changed_packages"], ["services/python/b"])
        self.assertEqual(data["dependency_packages"], ["services/python/a"])

    def test_empty_diff_writes_empty_hints(self) -> None:
        # No diff (e.g. branch already merged) → emit a no-op hints
        # file. bramble loads it, sees empty arrays, falls through to
        # the legacy narrow-review prompt. /pr-polish doesn't have to
        # special-case "no diff".
        root = Path(tempfile.mkdtemp())
        self.addCleanup(self._rmtree, root)
        state_dir = root / ".state"
        state_dir.mkdir()

        def fake_run(cmd, *, check=True, env=None, cwd=None,
                     input_text=None, timeout=None):
            if cmd[:2] == ["git", "rev-parse"]:
                return RunResult(stdout=str(root) + "\n", stderr="",
                                 returncode=0)
            if cmd[:2] == ["git", "symbolic-ref"]:
                return RunResult(stdout="refs/remotes/origin/main\n",
                                 stderr="", returncode=0)
            if cmd[:2] == ["git", "diff"]:
                return RunResult(stdout="", stderr="", returncode=0)
            return RunResult(stdout="", stderr="", returncode=0)

        with patch.object(scope_gate, "run", fake_run), \
             patch("_common.run", fake_run):
            rc = scope_gate.main(["--state-dir", str(state_dir)])
        self.assertEqual(rc, 0)
        data = json.loads((state_dir / "scope-hints.json").read_text())
        self.assertEqual(data["test_paths"], [])
        self.assertEqual(data["cross_service_packages"], [])
        # v2 split fields should also be empty when there's no diff —
        # otherwise a regression that drops the empty-paths short-circuit
        # in scope_gate.main would silently change envelope shape.
        self.assertEqual(data["changed_packages"], [])
        self.assertEqual(data["dependency_packages"], [])

    def test_outside_git_repo(self) -> None:
        # ``git rev-parse --show-toplevel`` fails outside a repo. We
        # still emit a hints file (empty) and exit 0; aborting here
        # would force every caller to special-case "not in a repo."
        root = Path(tempfile.mkdtemp())
        self.addCleanup(self._rmtree, root)
        state_dir = root / ".state"
        state_dir.mkdir()

        def fake_run(cmd, *, check=True, env=None, cwd=None,
                     input_text=None, timeout=None):
            if cmd[:2] == ["git", "rev-parse"]:
                return RunResult(stdout="", stderr="fatal: not a git repository",
                                 returncode=128)
            if cmd[:2] == ["git", "symbolic-ref"]:
                return RunResult(stdout="", stderr="", returncode=128)
            return RunResult(stdout="", stderr="", returncode=128)

        with patch.object(scope_gate, "run", fake_run), \
             patch("_common.run", fake_run):
            rc = scope_gate.main(["--state-dir", str(state_dir)])
        self.assertEqual(rc, 0)
        data = json.loads((state_dir / "scope-hints.json").read_text())
        self.assertEqual(data["schema_version"], 2)
        self.assertEqual(data["test_paths"], [])
        self.assertEqual(data["cross_service_packages"], [])
        # Shape parity with the empty-diff path: v2 envelope fields
        # must be present (as empty arrays) even on the no-git fallback,
        # so downstream consumers don't see a v2 schema with missing keys.
        self.assertEqual(data["changed_packages"], [])
        self.assertEqual(data["dependency_packages"], [])

    @staticmethod
    def _rmtree(path: Path) -> None:
        import shutil

        shutil.rmtree(path, ignore_errors=True)


if __name__ == "__main__":
    unittest.main()
