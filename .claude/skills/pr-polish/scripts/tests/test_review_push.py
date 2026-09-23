#!/usr/bin/env python3
"""Hang detection for the review push stream."""

from __future__ import annotations

import json
import sys
import textwrap
import unittest
from pathlib import Path

SCRIPTS = Path(__file__).resolve().parents[1]
if str(SCRIPTS) not in sys.path:
    sys.path.insert(0, str(SCRIPTS))

import review_push  # noqa: E402


class SuperviseTests(unittest.TestCase):
    def setUp(self) -> None:
        import tempfile
        self._dir = tempfile.TemporaryDirectory()
        self.tmp = Path(self._dir.name)
        self.envelope = self.tmp / "envelope.json"

    def tearDown(self) -> None:
        self._dir.cleanup()

    def test_terminal_event_is_forwarded_and_not_a_hang(self) -> None:
        script = textwrap.dedent(
            """\
            import json, sys
            print(json.dumps({"event": "phase", "phase": "analyzing"}), flush=True)
            print(json.dumps({"event": "heartbeat", "elapsed_ms": 1, "interval_ms": 60000, "idle": True}), flush=True)
            print(json.dumps({"event": "done", "verdict": "accepted", "envelope": "x", "status": "ok", "issues": 0}), flush=True)
            """
        )
        code = review_push.supervise(
            [sys.executable, "-c", script],
            envelope=self.envelope,
            interval_ms=200,
            sigterm_grace_s=0.2,
        )
        self.assertEqual(code, 0)
        self.assertFalse(self.envelope.exists())

    def test_silence_after_analyzing_is_a_hang(self) -> None:
        script = textwrap.dedent(
            """\
            import json, sys, time
            print(json.dumps({"event": "phase", "phase": "analyzing"}), flush=True)
            time.sleep(30)
            """
        )
        code = review_push.supervise(
            [sys.executable, "-c", script],
            envelope=self.envelope,
            interval_ms=200,
            backend="codex",
            sigterm_grace_s=0.2,
        )
        self.assertEqual(code, 1)
        env = json.loads(self.envelope.read_text())
        self.assertEqual(env["status"], "error")
        self.assertIn("hang", env["error"])
        self.assertEqual(env["backend"], "codex")

    def test_exit_without_terminal_event_writes_error_envelope(self) -> None:
        code = review_push.supervise(
            [sys.executable, "-c", "import sys; sys.exit(0)"],
            envelope=self.envelope,
            interval_ms=200,
            sigterm_grace_s=0.2,
        )
        self.assertEqual(code, 1)
        env = json.loads(self.envelope.read_text())
        self.assertEqual(env["status"], "error")
        self.assertIn("terminal event", env["error"])

    def test_reading_diff_does_not_arm_hang_clock(self) -> None:
        # reading_diff covers backend start, which can outlast 2x the
        # heartbeat interval. Only analyzing/heartbeat arm the clock, and
        # a prompt done after reading_diff must not be killed.
        script = textwrap.dedent(
            """\
            import json, sys
            print(json.dumps({"event": "phase", "phase": "reading_diff"}), flush=True)
            print(json.dumps({"event": "done", "verdict": "accepted", "envelope": "", "status": "ok", "issues": 0}), flush=True)
            """
        )
        code = review_push.supervise(
            [sys.executable, "-c", script],
            envelope=self.envelope,
            interval_ms=50,
            sigterm_grace_s=0.2,
        )
        self.assertEqual(code, 0)
        self.assertFalse(self.envelope.exists())


if __name__ == "__main__":
    unittest.main()
