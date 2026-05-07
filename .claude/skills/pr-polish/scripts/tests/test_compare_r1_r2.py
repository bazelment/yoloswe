"""Focused tests for compare-r1-r2.load_envelope.

The hyphenated filename means we import via importlib rather than a regular
``import`` statement. Tests cover the four shapes the loader must handle:
plain dict envelope, NDJSON with trailing envelope, malformed middle lines
followed by a good last line, and lines lacking ``schema_version`` (which
must be rejected to avoid returning a progress event by mistake).
"""

from __future__ import annotations

import importlib.util
import json
import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
SCRIPT = HERE.parent / "compare-r1-r2.py"


def _load_module():
    spec = importlib.util.spec_from_file_location("compare_r1_r2", SCRIPT)
    mod = importlib.util.module_from_spec(spec)
    sys.modules["compare_r1_r2"] = mod
    assert spec.loader is not None
    spec.loader.exec_module(mod)
    return mod


class TestLoadEnvelope(unittest.TestCase):
    def setUp(self) -> None:
        self.mod = _load_module()

    def _write(self, text: str) -> Path:
        f = tempfile.NamedTemporaryFile(mode="w", delete=False, suffix=".json")
        f.write(text)
        f.close()
        self.addCleanup(lambda: Path(f.name).unlink(missing_ok=True))
        return Path(f.name)

    def test_plain_dict_envelope(self) -> None:
        env = {"schema_version": 1, "status": "ok", "review": {"issues": []}}
        path = self._write(json.dumps(env))
        self.assertEqual(self.mod.load_envelope(path), env)

    def test_ndjson_trailing_envelope(self) -> None:
        progress = {"event": "progress", "msg": "starting"}
        env = {"schema_version": 1, "status": "ok", "review": {"issues": [{"x": 1}]}}
        text = json.dumps(progress) + "\n" + json.dumps(env) + "\n"
        path = self._write(text)
        self.assertEqual(self.mod.load_envelope(path), env)

    def test_malformed_middle_with_good_last(self) -> None:
        env = {"schema_version": 2, "status": "ok"}
        text = "not-json\n{also bad\n" + json.dumps(env) + "\n"
        path = self._write(text)
        self.assertEqual(self.mod.load_envelope(path), env)

    def test_lines_without_schema_version_rejected(self) -> None:
        # A stream of pure progress lines with no envelope must not be
        # accepted as a fallback envelope — that's the whole reason we
        # gate on the schema_version sentinel.
        progress_only = (
            json.dumps({"event": "progress", "msg": "a"}) + "\n"
            + json.dumps({"event": "progress", "msg": "b"}) + "\n"
        )
        path = self._write(progress_only)
        self.assertIsNone(self.mod.load_envelope(path))

    def test_missing_file_returns_none(self) -> None:
        self.assertIsNone(self.mod.load_envelope(Path("/nonexistent/path.json")))


class TestFormatResultsAllPass(unittest.TestCase):
    """``all_pass`` is the gate that ``main`` returns as exit code.

    Each branch must yield the right value: kernel-2755 keys on
    regressions, other PRs require full recall (caught == total), and
    total==0 is treated as skipped (neither passes nor fails the run).
    """

    def setUp(self) -> None:
        self.mod = _load_module()

    def _result(self, **overrides) -> dict:
        base = {
            "pr": "1234",
            "total_substantive": 0,
            "caught": 0,
            "findings": [],
            "r1_issue_count": 0,
            "r2_issue_count": 0,
            "r1_only_count": 0,
            "r2_cursor_loaded": True,
            "r2_codex_loaded": True,
        }
        base.update(overrides)
        return base

    def test_2755_zero_regressions_passes(self) -> None:
        r = self._result(pr="2755", r1_only_count=0, r1_issue_count=3, r2_issue_count=3)
        _, all_pass = self.mod.format_results([r])
        self.assertTrue(all_pass)

    def test_2755_with_regressions_fails(self) -> None:
        r = self._result(pr="2755", r1_only_count=2, r1_issue_count=5, r2_issue_count=3)
        _, all_pass = self.mod.format_results([r])
        self.assertFalse(all_pass)

    def test_partial_recall_fails(self) -> None:
        # Pre-fix bug: caught=1/3 used to read as a pass. Lock in full-recall.
        r = self._result(pr="2978", total_substantive=3, caught=1)
        _, all_pass = self.mod.format_results([r])
        self.assertFalse(all_pass)

    def test_full_recall_passes(self) -> None:
        r = self._result(pr="2978", total_substantive=3, caught=3)
        _, all_pass = self.mod.format_results([r])
        self.assertTrue(all_pass)

    def test_no_substantive_comments_neither_passes_nor_fails(self) -> None:
        # total==0 used to read as a pass via caught>=1 → False but
        # all_pass was only cleared on caught==0; really there's nothing
        # to verify. Treat as skipped: doesn't flip all_pass either way.
        r = self._result(pr="2978", total_substantive=0, caught=0)
        _, all_pass = self.mod.format_results([r])
        self.assertTrue(all_pass)


class TestMainExitCode(unittest.TestCase):
    """main() must return non-zero on regression in both Markdown and --json."""

    def setUp(self) -> None:
        self.mod = _load_module()

    def _patch_compare(self, stub_results):
        def _stub(pr, *_args, **_kw):
            return stub_results[pr]
        return _stub

    def test_markdown_failing_returns_one(self) -> None:
        # Single PR with partial recall — should return 1.
        results = {
            "1234": {
                "pr": "1234", "total_substantive": 3, "caught": 1, "findings": [],
                "r1_issue_count": 1, "r2_issue_count": 1, "r1_only_count": 0,
                "r2_cursor_loaded": True, "r2_codex_loaded": True,
            }
        }
        from unittest.mock import patch  # noqa: PLC0415
        with patch.object(self.mod, "compare_pr", self._patch_compare(results)):
            rc = self.mod.main(["--prs", "1234"])
        self.assertEqual(rc, 1)

    def test_json_failing_returns_one(self) -> None:
        # Pre-fix bug: --json returned 0 even on regression. Lock the new
        # behavior: --json must share the exit-code contract with Markdown.
        results = {
            "1234": {
                "pr": "1234", "total_substantive": 3, "caught": 1, "findings": [],
                "r1_issue_count": 1, "r2_issue_count": 1, "r1_only_count": 0,
                "r2_cursor_loaded": True, "r2_codex_loaded": True,
            }
        }
        from unittest.mock import patch  # noqa: PLC0415
        with patch.object(self.mod, "compare_pr", self._patch_compare(results)):
            rc = self.mod.main(["--prs", "1234", "--json"])
        self.assertEqual(rc, 1)

    def test_json_passing_returns_zero(self) -> None:
        results = {
            "1234": {
                "pr": "1234", "total_substantive": 3, "caught": 3, "findings": [],
                "r1_issue_count": 1, "r2_issue_count": 1, "r1_only_count": 0,
                "r2_cursor_loaded": True, "r2_codex_loaded": True,
            }
        }
        from unittest.mock import patch  # noqa: PLC0415
        with patch.object(self.mod, "compare_pr", self._patch_compare(results)):
            rc = self.mod.main(["--prs", "1234", "--json"])
        self.assertEqual(rc, 0)


if __name__ == "__main__":
    unittest.main()
