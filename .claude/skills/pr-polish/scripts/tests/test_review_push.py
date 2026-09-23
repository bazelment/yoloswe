#!/usr/bin/env python3
"""Hang detection for the review push stream."""

from __future__ import annotations

import json
import sys
import textwrap
import time
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

    def test_silence_after_reading_diff_is_a_hang(self) -> None:
        # bramble starts its heartbeat timer at reading_diff, so a child
        # that goes quiet before the first tick is hung. If the clock waited
        # for the first heartbeat, only the outer timeout would catch it.
        script = textwrap.dedent(
            """\
            import json, time
            print(json.dumps({"event": "phase", "phase": "reading_diff"}), flush=True)
            time.sleep(30)
            """
        )
        start = time.monotonic()
        code = review_push.supervise(
            [sys.executable, "-c", script],
            envelope=self.envelope,
            interval_ms=100,
            backend="codex",
            sigterm_grace_s=0.2,
        )
        self.assertEqual(code, 1)
        self.assertLess(time.monotonic() - start, 5)
        env = json.loads(self.envelope.read_text())
        self.assertIn("hang", env["error"])

    def test_stdout_closed_while_alive_is_a_hang(self) -> None:
        # EOF on stdout without a terminal event must not become an
        # unbounded wait on a child that is still running.
        script = textwrap.dedent(
            """\
            import json, os, sys, time
            print(json.dumps({"event": "phase", "phase": "analyzing", "interval_ms": 100}), flush=True)
            os.close(1)
            time.sleep(30)
            """
        )
        start = time.monotonic()
        code = review_push.supervise(
            [sys.executable, "-c", script],
            envelope=self.envelope,
            backend="cursor",
            sigterm_grace_s=0.2,
        )
        self.assertEqual(code, 1)
        self.assertLess(time.monotonic() - start, 5)
        env = json.loads(self.envelope.read_text())
        self.assertIn("stdout closed", env["error"])

    def test_phase_interval_sizes_clock_before_first_heartbeat(self) -> None:
        # With the 20s default a silent child would survive 40s. The phase
        # line's interval_ms (100ms) must size the clock instead.
        script = textwrap.dedent(
            """\
            import json, time
            print(json.dumps({"event": "phase", "phase": "reading_diff", "interval_ms": 100}), flush=True)
            time.sleep(30)
            """
        )
        start = time.monotonic()
        code = review_push.supervise(
            [sys.executable, "-c", script],
            envelope=self.envelope,
            sigterm_grace_s=0.2,
        )
        self.assertEqual(code, 1)
        self.assertLess(time.monotonic() - start, 5)

    def test_child_that_never_prints_is_a_hang(self) -> None:
        # The clock is armed at spawn. A child silent from the start (a
        # frozen setup, a wrong binary) must not wait for the outer timeout.
        start = time.monotonic()
        code = review_push.supervise(
            [sys.executable, "-c", "import time; time.sleep(30)"],
            envelope=self.envelope,
            interval_ms=100,
            backend="codex",
            sigterm_grace_s=0.2,
        )
        self.assertEqual(code, 1)
        self.assertLess(time.monotonic() - start, 5)
        env = json.loads(self.envelope.read_text())
        self.assertIn("hang", env["error"])

    def test_terminal_event_bounds_teardown_wait(self) -> None:
        script = textwrap.dedent(
            """\
            import json, time
            print(json.dumps({"event": "done", "verdict": "accepted", "envelope": "", "status": "ok", "issues": 0}), flush=True)
            time.sleep(30)
            """
        )
        start = time.monotonic()
        code = review_push.supervise(
            [sys.executable, "-c", script],
            envelope=self.envelope,
            sigterm_grace_s=0.1,
            terminal_grace_s=0.1,
        )
        self.assertEqual(code, 1)
        self.assertLess(time.monotonic() - start, 1)

    def test_forwarded_events_and_stderr_name_their_backend(self) -> None:
        # Reviewers share one job stdout. Every forwarded JSON event carries
        # the backend, including a hang terminal event review_push writes
        # itself. Stderr lines are prefixed. Non-JSON stdout passes through
        # unchanged.
        import contextlib
        import io

        script = textwrap.dedent(
            """\
            import json, sys
            print("stderr detail", file=sys.stderr, flush=True)
            print("not json", flush=True)
            print(json.dumps({"event": "phase", "phase": "analyzing"}), flush=True)
            print(json.dumps({"event": "heartbeat", "elapsed_ms": 1, "interval_ms": 60000}), flush=True)
            print(json.dumps({"event": "done", "verdict": "accepted", "envelope": "x", "status": "ok", "issues": 0}), flush=True)
            """
        )
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            code = review_push.supervise(
                [sys.executable, "-c", script],
                envelope=self.envelope,
                backend="claude",
                sigterm_grace_s=0.2,
            )
        self.assertEqual(code, 0)
        lines = out.getvalue().splitlines()
        self.assertEqual(lines[0], "not json")
        events = [json.loads(line) for line in lines[1:]]
        self.assertEqual([e["event"] for e in events], ["phase", "heartbeat", "done"])
        self.assertEqual({e["backend"] for e in events}, {"claude"})
        self.assertEqual(err.getvalue(), "[claude] stderr detail\n")

    def test_hang_terminal_event_names_backend(self) -> None:
        import contextlib
        import io

        script = textwrap.dedent(
            """\
            import json, time
            print(json.dumps({"event": "phase", "phase": "analyzing"}), flush=True)
            time.sleep(30)
            """
        )
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            code = review_push.supervise(
                [sys.executable, "-c", script],
                envelope=self.envelope,
                interval_ms=100,
                backend="codex",
                sigterm_grace_s=0.2,
            )
        self.assertEqual(code, 1)
        terminal = json.loads(out.getvalue().splitlines()[-1])
        self.assertEqual(terminal["event"], "error")
        self.assertEqual(terminal["backend"], "codex")


if __name__ == "__main__":
    unittest.main()
