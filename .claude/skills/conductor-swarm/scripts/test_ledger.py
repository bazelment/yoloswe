#!/usr/bin/env python3
"""Recovery-state checks for conductor-swarm's local ledger."""

import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path


SCRIPT = Path(__file__).with_name("ledger.py")


def call(*args, check=True):
    return subprocess.run(
        [sys.executable, "-B", str(SCRIPT), *map(str, args)],
        capture_output=True, text=True, check=check,
    )


class LedgerTests(unittest.TestCase):
    def test_phase_sessions_keep_their_own_message_ids_and_cursors(self):
        with tempfile.TemporaryDirectory() as run:
            call("init", run, "--goal", "open PR", "--proof", "verified PR",
                 "--phases", "swe,clean,review,integrate", "--concurrency", 1)
            call("add", run, "--id", "a", "--title", "task", "--project-id", "p",
                 "--branch", "main")
            call("set", run, "--id", "a", "--status", "running", "--session-id", "s1",
                 "--initial-message-id", "m1", "--cursor", "c1", "--observed-working")
            call("set", run, "--id", "a", "--phase", "clean", "--session-id", "s2",
                 "--initial-message-id", "m2")
            call("set", run, "--id", "a", "--phase", "review", "--session-id", "s3",
                 "--initial-message-id", "m3", "--cursor", "c3")
            state = json.loads(call("show", run).stdout)["lanes"]["a"]
            self.assertEqual(state["initial_message_ids"],
                             {"s1": "m1", "s2": "m2", "s3": "m3"})
            self.assertEqual(state["cursors"], {"s1": "c1", "s3": "c3"})
            self.assertTrue(state["observed_working"]["s1"])

    def test_message_state_requires_a_recorded_session(self):
        with tempfile.TemporaryDirectory() as run:
            call("init", run, "--goal", "open PR", "--proof", "verified PR",
                 "--phases", "swe,review", "--concurrency", 1)
            call("add", run, "--id", "a", "--title", "task", "--project-id", "p",
                 "--branch", "main")
            result = call("set", run, "--id", "a", "--initial-message-id", "m1",
                          check=False)
            self.assertNotEqual(result.returncode, 0)
            self.assertIn("record a session", result.stderr)


if __name__ == "__main__":
    unittest.main()
