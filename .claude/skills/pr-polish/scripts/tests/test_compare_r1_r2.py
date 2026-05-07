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


if __name__ == "__main__":
    unittest.main()
