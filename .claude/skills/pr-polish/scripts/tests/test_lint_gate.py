"""Unit tests for lint_gate. Hermetic: no real linters, no subprocess."""

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

import lint_gate  # noqa: E402
from _common import RunResult  # noqa: E402


def _stub_run(stdout: str, returncode: int = 0):
    """Build a fake _common.run that always returns the given stdout."""

    def fake(cmd, *, check=True, env=None, cwd=None, input_text=None, timeout=None):
        return RunResult(stdout=stdout, stderr="", returncode=returncode)

    return fake


class TestBucket(unittest.TestCase):
    def test_extension_routing(self) -> None:
        # Files routed to the right bucket; everything else returns None so we
        # don't try to lint random binaries or markdown.
        self.assertEqual(lint_gate._bucket("foo/bar.py"), "py")
        self.assertEqual(lint_gate._bucket("FOO/BAR.PY"), "py")  # case-insensitive
        self.assertEqual(lint_gate._bucket("svc/main.go"), "go")
        self.assertEqual(lint_gate._bucket("ui/comp.tsx"), "js")
        self.assertEqual(lint_gate._bucket("ui/comp.spec.ts"), "js")
        self.assertIsNone(lint_gate._bucket("README.md"))
        self.assertIsNone(lint_gate._bucket("Cargo.lock"))


class TestRunRuff(unittest.TestCase):
    def test_skipped_when_binary_missing(self) -> None:
        with patch.object(lint_gate, "_have", return_value=False):
            self.assertEqual(lint_gate.run_ruff(["a.py"]), [])

    def test_empty_paths_short_circuits(self) -> None:
        # Even if ruff is on PATH, no .py files means no run() call.
        with patch.object(lint_gate, "_have", return_value=True):
            with patch.object(lint_gate, "run", side_effect=AssertionError("ruff invoked")):
                self.assertEqual(lint_gate.run_ruff([]), [])

    def test_severity_mapping_e9_high_e_medium_w_low(self) -> None:
        # Three findings, one per severity tier — exercises the code-prefix
        # ladder (E9/F8 → high, E/F → medium, else → low). Locked because
        # severity routes feed the triage must_fix/consider_fix split.
        ruff_out = json.dumps(
            [
                {
                    "filename": "a.py",
                    "code": "E902",
                    "message": "Indentation",
                    "location": {"row": 3},
                },
                {
                    "filename": "a.py",
                    "code": "E711",
                    "message": "comparison to None",
                    "location": {"row": 5},
                },
                {
                    "filename": "a.py",
                    "code": "W291",
                    "message": "trailing whitespace",
                    "location": {"row": 7},
                },
            ]
        )
        with patch.object(lint_gate, "_have", return_value=True):
            with patch.object(lint_gate, "run", side_effect=_stub_run(ruff_out, returncode=1)):
                got = lint_gate.run_ruff(["a.py"])
        sevs = [g["severity"] for g in got]
        self.assertEqual(sevs, ["high", "medium", "low"])
        # Topic must include the rule code so two findings on the same line
        # under different rules don't dedupe.
        self.assertIn("e902", got[0]["topic"])
        self.assertIn("w291", got[2]["topic"])

    def test_malformed_output_returns_empty(self) -> None:
        # ruff is supposed to emit JSON; if it doesn't (e.g. internal error
        # printed to stdout), return empty rather than crashing the round.
        with patch.object(lint_gate, "_have", return_value=True):
            with patch.object(lint_gate, "run", side_effect=_stub_run("not json", 1)):
                self.assertEqual(lint_gate.run_ruff(["a.py"]), [])


class TestRunGolangci(unittest.TestCase):
    def test_passes_packages_not_files(self) -> None:
        # golangci-lint resolves imports per package, so we need to give it
        # directories. Verify the dispatcher dedupes parents.
        captured: list[list[str]] = []

        def capture(cmd, *, check=True, **kw):
            captured.append(cmd)
            return RunResult(stdout=json.dumps({"Issues": []}), stderr="", returncode=0)

        with patch.object(lint_gate, "_have", return_value=True):
            with patch.object(lint_gate, "run", side_effect=capture):
                lint_gate.run_golangci(["pkg/a.go", "pkg/b.go", "pkg2/c.go"])
        self.assertEqual(len(captured), 1)
        # First three tokens are the binary + flags; trailing tokens are pkgs.
        pkgs = sorted(captured[0][3:])
        self.assertEqual(pkgs, ["pkg", "pkg2"])

    def test_known_linters_severity_mapping(self) -> None:
        gci_out = json.dumps(
            {
                "Issues": [
                    {
                        "FromLinter": "errcheck",
                        "Text": "unchecked error",
                        "Pos": {"Filename": "a.go", "Line": 4},
                    },
                    {
                        "FromLinter": "gofmt",
                        "Text": "format diff",
                        "Pos": {"Filename": "a.go", "Line": 8},
                    },
                    {
                        "FromLinter": "neverheardof",
                        "Text": "?",
                        "Pos": {"Filename": "a.go", "Line": 1},
                    },
                ]
            }
        )
        with patch.object(lint_gate, "_have", return_value=True):
            with patch.object(lint_gate, "run", side_effect=_stub_run(gci_out)):
                got = lint_gate.run_golangci(["a.go"])
        # errcheck → medium, gofmt → low, unknown → low (conservative default).
        self.assertEqual([g["severity"] for g in got], ["medium", "low", "low"])


class TestRunEslint(unittest.TestCase):
    def test_severity_2_medium_1_low(self) -> None:
        eslint_out = json.dumps(
            [
                {
                    "filePath": "/x/foo.ts",
                    "messages": [
                        {"ruleId": "no-unused-vars", "severity": 2, "message": "x", "line": 1},
                        {"ruleId": "prefer-const", "severity": 1, "message": "y", "line": 2},
                    ],
                }
            ]
        )
        with patch.object(lint_gate, "_have", return_value=True):
            with patch.object(lint_gate, "run", side_effect=_stub_run(eslint_out, 1)):
                got = lint_gate.run_eslint(["/x/foo.ts"])
        self.assertEqual([g["severity"] for g in got], ["medium", "low"])
        self.assertEqual(got[0]["file"], "/x/foo.ts")
        self.assertEqual(got[0]["line"], 1)


class TestBuildEnvelope(unittest.TestCase):
    def test_envelope_shape_matches_bramble_parse_envelope(self) -> None:
        # bramble_ops.parse_envelope keys on status=="ok" and review.issues;
        # if either drifts, lint findings silently disappear from triage.
        env = lint_gate.build_envelope([{"file": "a.py", "line": 1, "severity": "low"}])
        self.assertEqual(env["status"], "ok")
        self.assertEqual(env["schema_version"], 1)
        self.assertEqual(env["backend"], "lint")
        self.assertEqual(len(env["review"]["issues"]), 1)


class TestCollectFindings(unittest.TestCase):
    def test_dispatches_per_bucket_and_concatenates(self) -> None:
        # Each linter sees only its own files; no cross-pollination.
        seen: dict[str, list[str]] = {"ruff": [], "golangci": [], "eslint": []}

        def fake_ruff(paths):
            seen["ruff"] = list(paths)
            return [{"file": "a.py", "severity": "low"}]

        def fake_gci(paths):
            seen["golangci"] = list(paths)
            return [{"file": "a.go", "severity": "low"}]

        def fake_eslint(paths):
            seen["eslint"] = list(paths)
            return [{"file": "a.ts", "severity": "medium"}]

        with patch.object(lint_gate, "run_ruff", side_effect=fake_ruff):
            with patch.object(lint_gate, "run_golangci", side_effect=fake_gci):
                with patch.object(lint_gate, "run_eslint", side_effect=fake_eslint):
                    out = lint_gate.collect_findings(
                        ["a.py", "a.go", "a.ts", "README.md"]
                    )
        self.assertEqual(seen["ruff"], ["a.py"])
        self.assertEqual(seen["golangci"], ["a.go"])
        self.assertEqual(seen["eslint"], ["a.ts"])
        self.assertEqual(len(out), 3)


class TestEnvelopePathFor(unittest.TestCase):
    def test_layout_matches_round_dir_convention(self) -> None:
        # Mirrors <state_dir>/r<n>/<backend>-envelope.json so the SKILL.md
        # --stream lint=<state_dir>/r$ROUND/lint-envelope.json wiring lines up
        # with the existing codex/cursor envelope layout.
        sd = Path("/tmp/x")
        self.assertEqual(
            lint_gate.envelope_path_for(sd, 2),
            Path("/tmp/x/r2/lint-envelope.json"),
        )


class TestMainEndToEnd(unittest.TestCase):
    def test_writes_envelope_even_with_no_findings(self) -> None:
        # An empty round should still produce a parseable envelope so triage
        # treats it as "lint ran clean," not "lint failed silently."
        with tempfile.TemporaryDirectory() as td:
            with patch.object(lint_gate, "changed_files", return_value=[]):
                with patch.object(lint_gate, "detect_base_branch", return_value="main"):
                    rc = lint_gate.main(["--state-dir", td, "--round", "1"])
            self.assertEqual(rc, 0)
            out = Path(td) / "r1" / "lint-envelope.json"
            self.assertTrue(out.exists())
            obj = json.loads(out.read_text())
            self.assertEqual(obj["status"], "ok")
            self.assertEqual(obj["review"]["issues"], [])


if __name__ == "__main__":
    unittest.main()
