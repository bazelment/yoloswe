"""Unit tests for bramble_ops. Hermetic: no bramble, no subprocess."""

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

import _common  # noqa: E402
import bramble_ops  # noqa: E402


class TestBuildLaunchCommand(unittest.TestCase):
    def test_flags_are_exact_and_ordered(self) -> None:
        cmd = bramble_ops.build_launch_command("codex", "gpt-5.4-mini", "do thing")
        self.assertEqual(
            cmd,
            [
                "bramble",
                "code-review",
                "--backend",
                "codex",
                "--model",
                "gpt-5.4-mini",
                "--json",
                "--skip-test-execution",
                "--verbose",
                "--timeout",
                "10m",
                "--goal",
                "do thing",
            ],
        )

    def test_rejects_unknown_backend(self) -> None:
        with self.assertRaises(ValueError):
            bramble_ops.build_launch_command("claude", "m", "g")


class TestLaunchEnv(unittest.TestCase):
    def test_sets_run_tag_and_work_dir(self) -> None:
        env = bramble_ops.launch_env("kernel", 2443, "codex", 1, "/tmp/wd")
        self.assertEqual(env["BRAMBLE_RUN_TAG"], "pr-polish:kernel:2443:codex:r1")
        self.assertEqual(env["WORK_DIR"], "/tmp/wd")


class TestFormatMonitorCommand(unittest.TestCase):
    """Guards the exact shape of the string the orchestrator drops into Monitor.

    Small changes to quoting or flag order will confuse Monitor (it passes
    the string straight to the shell) or bramble (which flag-parses
    positionally), so we assert on the full string.
    """

    def test_command_has_cd_tag_flags_in_order(self) -> None:
        cmd = bramble_ops.format_monitor_command(
            "codex",
            "gpt-5.4-mini",
            3,
            "review branch X",
            repo="kernel",
            pr=2443,
            work_dir="/tmp/worktree",
        )
        self.assertEqual(
            cmd,
            "cd /tmp/worktree && BRAMBLE_RUN_TAG=pr-polish:kernel:2443:codex:r3 "
            "bramble code-review --backend codex --model gpt-5.4-mini --json "
            "--skip-test-execution --verbose --timeout 10m --goal 'review branch X'",
        )

    def test_goal_is_shell_quoted(self) -> None:
        # A realistic PR summary includes backticks, quotes, and parens;
        # bramble's --goal must receive those literally. shlex.quote is
        # responsible here — we guard it by asserting the round-tripped shell
        # split matches the original goal.
        import shlex

        goal = "fix `foo()` (really this time)"
        cmd = bramble_ops.format_monitor_command(
            "cursor", "composer-2", 1, goal, repo="kernel", pr=99, work_dir="/tmp/w"
        )
        tokens = shlex.split(cmd)
        # Last argument of the tokenized command is the goal text verbatim.
        self.assertEqual(tokens[-1], goal)

    def test_rejects_unknown_backend(self) -> None:
        with self.assertRaises(ValueError):
            bramble_ops.format_monitor_command(
                "claude", "m", 1, "g", repo="kernel", pr=1, work_dir="/tmp"
            )

    def test_requires_pr(self) -> None:
        with patch.dict("os.environ", {"PR_NUMBER": "0"}, clear=False):
            with self.assertRaises(ValueError):
                bramble_ops.format_monitor_command(
                    "codex", "m", 1, "g", repo="kernel", pr=0, work_dir="/tmp"
                )

    def test_accepts_branch_slug(self) -> None:
        # Branch-only mode: pass a string slug instead of a PR number.
        cmd = bramble_ops.format_monitor_command(
            "codex",
            "gpt-5.4-mini",
            1,
            "review branch X",
            repo="kernel",
            pr="branch-feature-foo",
            work_dir="/tmp/worktree",
        )
        self.assertIn("BRAMBLE_RUN_TAG=pr-polish:kernel:branch-feature-foo:codex:r1", cmd)


class TestExtractTerminalEnvelope(unittest.TestCase):
    def test_returns_last_envelope_ignoring_progress_lines(self) -> None:
        stream = "\n".join(
            [
                '{"event":"progress","kind":"session-started","backend":"codex"}',
                '{"event":"progress","kind":"tool-use","tool":"read"}',
                '{"schema_version":1,"status":"ok","backend":"codex","review":{"verdict":"accepted","issues":[]}}',
                "",
            ]
        )
        env = bramble_ops.extract_terminal_envelope(stream)
        self.assertIsNotNone(env)
        self.assertEqual(env["status"], "ok")

    def test_returns_none_when_no_envelope_line(self) -> None:
        stream = "\n".join(
            [
                '{"event":"progress","kind":"session-started"}',
                '{"event":"progress","kind":"tool-use","tool":"read"}',
            ]
        )
        self.assertIsNone(bramble_ops.extract_terminal_envelope(stream))

    def test_ignores_malformed_progress_lines(self) -> None:
        # A bramble-side bug could corrupt one progress line; the last good
        # envelope line should still be extractable. This is the guarantee
        # that justifies the "scan bottom-up, skip non-JSON" implementation
        # over a strict line-by-line parser.
        stream = "\n".join(
            [
                "random prose bramble writes sometimes",
                "{not json}",
                '{"schema_version":1,"status":"error","backend":"codex","error":"boom"}',
            ]
        )
        env = bramble_ops.extract_terminal_envelope(stream)
        self.assertIsNotNone(env)
        self.assertEqual(env["status"], "error")


class TestParseStream(unittest.TestCase):
    def test_ok_stream_extracts_issues(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "stream.txt"
            p.write_text(
                '{"event":"progress","kind":"session-started"}\n'
                '{"schema_version":1,"status":"ok","backend":"codex",'
                '"review":{"verdict":"rejected","issues":['
                '{"severity":"high","file":"a.py","line":12,'
                '"message":"Null check missing","suggestion":"add guard"}]}}\n'
            )
            got = bramble_ops.parse_stream(p, source="codex")
            self.assertEqual(len(got), 1)
            self.assertEqual(got[0]["severity"], "high")
            self.assertEqual(got[0]["source"], "codex")

    def test_missing_envelope_synthesizes_empty_envelope_finding(self) -> None:
        # This is the PR #162 regression: stream ended cleanly but no
        # envelope line ever landed. Triage must see a high-severity finding
        # so the round does not converge to a false "zero findings".
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "stream.txt"
            p.write_text(
                '{"event":"progress","kind":"session-started"}\n'
                '{"event":"progress","kind":"tool-use","tool":"read"}\n'
            )
            got = bramble_ops.parse_stream(p, source="codex")
            self.assertEqual(len(got), 1)
            self.assertEqual(got[0]["severity"], "high")
            self.assertEqual(got[0]["topic"], "bramble-empty-envelope")
            self.assertEqual(got[0]["source"], "codex")

    def test_missing_file_returns_empty(self) -> None:
        # A missing stream file is distinguishable from an empty one: it
        # means the Monitor call never ran (different bug class). We return
        # [] rather than a synthetic finding so triage isn't flooded with
        # false positives from rounds that genuinely didn't launch.
        self.assertEqual(
            bramble_ops.parse_stream(Path("/nonexistent/stream.txt"), source="codex"),
            [],
        )


class TestParseStreamArgs(unittest.TestCase):
    def test_splits_multiple_pairs(self) -> None:
        out = bramble_ops._parse_stream_args(["codex=/a.log", "cursor=/b.log"])
        self.assertEqual(out["codex"], Path("/a.log"))
        self.assertEqual(out["cursor"], Path("/b.log"))

    def test_rejects_missing_equals(self) -> None:
        with self.assertRaises(ValueError):
            bramble_ops._parse_stream_args(["codex"])

    def test_rejects_unknown_backend(self) -> None:
        with self.assertRaises(ValueError):
            bramble_ops._parse_stream_args(["claude=/a.log"])


class TestPrOrSlugConverter(unittest.TestCase):
    """Argparse `--pr` accepts either a numeric PR or a branch slug, matching
    the int|str contract on format_monitor_command/parse_round.
    """

    def test_numeric_returns_int(self) -> None:
        self.assertEqual(bramble_ops._pr_or_slug("234"), 234)

    def test_slug_returns_string(self) -> None:
        self.assertEqual(bramble_ops._pr_or_slug("branch-feature-foo"), "branch-feature-foo")

    def test_empty_rejected(self) -> None:
        import argparse as _ap

        with self.assertRaises(_ap.ArgumentTypeError):
            bramble_ops._pr_or_slug("")

    def test_parser_accepts_branch_slug_for_format_monitor_command(self) -> None:
        # End-to-end: argparse round-trips the slug through to the namespace.
        parser = bramble_ops._build_parser()
        ns = parser.parse_args(
            [
                "format-monitor-command",
                "codex",
                "gpt-5.4-mini",
                "1",
                "--goal",
                "g",
                "--repo",
                "kernel",
                "--pr",
                "branch-feature-foo",
                "--work-dir",
                "/tmp",
            ]
        )
        self.assertEqual(ns.pr, "branch-feature-foo")

    def test_parser_accepts_branch_slug_for_parse_subcommand(self) -> None:
        parser = bramble_ops._build_parser()
        ns = parser.parse_args(
            ["parse", "1", "--repo", "kernel", "--pr", "branch-feature-foo"]
        )
        self.assertEqual(ns.pr, "branch-feature-foo")

    def test_parser_accepts_branch_slug_for_triage_subcommand(self) -> None:
        parser = bramble_ops._build_parser()
        ns = parser.parse_args(
            ["triage", "1", "--repo", "kernel", "--pr", "branch-feature-foo"]
        )
        self.assertEqual(ns.pr, "branch-feature-foo")


class TestParseEnvelope(unittest.TestCase):
    def test_ok_extracts_issues_with_topic(self) -> None:
        env = {
            "status": "ok",
            "review": {
                "issues": [
                    {
                        "severity": "high",
                        "file": "a.py",
                        "line": 10,
                        "message": "Null check on line 10: foo may be None here!!",
                        "suggestion": "add guard",
                    }
                ]
            },
        }
        got = bramble_ops.parse_envelope(env, source="codex")
        self.assertEqual(len(got), 1)
        self.assertEqual(got[0]["source"], "codex")
        self.assertEqual(got[0]["severity"], "high")
        self.assertEqual(
            got[0]["topic"].split(), ["null", "check", "on", "line", "10", "foo", "may", "be"]
        )

    def test_error_yields_synthetic_stale(self) -> None:
        env = {"status": "error", "error": "backend crashed"}
        got = bramble_ops.parse_envelope(env, source="cursor")
        self.assertEqual(len(got), 1)
        self.assertEqual(got[0]["source"], "cursor")
        self.assertEqual(got[0]["status"], "error")
        self.assertIn("backend crashed", got[0]["message"])

    def test_missing_envelope_yields_empty_list(self) -> None:
        self.assertEqual(bramble_ops.parse_envelope(None, source="codex"), [])


class TestEnvelopeReady(unittest.TestCase):
    def test_rejects_empty_file(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "x.json"
            p.write_text("")
            self.assertIsNone(bramble_ops._envelope_ready(p))

    def test_rejects_non_json(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "x.json"
            p.write_text("not json")
            self.assertIsNone(bramble_ops._envelope_ready(p))

    def test_rejects_dict_without_status(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "x.json"
            p.write_text(json.dumps({"review": {}}))
            self.assertIsNone(bramble_ops._envelope_ready(p))

    def test_accepts_ok(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "x.json"
            p.write_text(json.dumps({"status": "ok", "review": {"issues": []}}))
            out = bramble_ops._envelope_ready(p)
            self.assertEqual(out["status"], "ok")


class TestTriage(unittest.TestCase):
    def _f(self, source: str, file: str, line: int, severity: str, message: str) -> dict:
        return {
            "source": source,
            "severity": severity,
            "file": file,
            "line": line,
            "message": message,
            "suggestion": None,
            "topic": _common.topic_of(message),
        }

    def test_consensus_when_two_sources_same_key(self) -> None:
        findings = [
            self._f("codex", "a.py", 10, "medium", "Null check missing foo"),
            self._f("cursor", "a.py", 10, "medium", "Null check missing foo"),
            self._f("cursor", "b.py", 5, "high", "Race condition on write path"),
        ]
        out = bramble_ops.triage(findings, prior_fixed_keys=set())
        self.assertEqual(len(out["consensus"]), 1)
        self.assertEqual(out["consensus"][0]["sources"], ["codex", "cursor"])
        self.assertEqual(len(out["single_critical"]), 1)
        self.assertEqual(out["single_critical"][0]["finding"]["file"], "b.py")
        self.assertEqual(out["total"], 3)
        self.assertEqual(out["unique"], 2)

    def test_low_acks_surface_separately(self) -> None:
        findings = [
            self._f("cursor", "a.py", 1, "low", "Consider renaming x to something"),
        ]
        out = bramble_ops.triage(findings, prior_fixed_keys=set())
        self.assertEqual(len(out["low_acks"]), 1)
        self.assertEqual(out["single_critical"], [])
        self.assertEqual(out["consensus"], [])

    def test_spiral_match_detected(self) -> None:
        f = self._f("codex", "a.py", 10, "medium", "Null check missing foo")
        key = (f["file"], f["line"], f["topic"])
        out = bramble_ops.triage([f], prior_fixed_keys={key})
        self.assertEqual(len(out["spiral_matches"]), 1)
        self.assertEqual(out["spiral_matches"][0]["key"], list(key))

    def test_action_plan_dispatch_shape(self) -> None:
        consensus_f1 = self._f("codex", "a.py", 10, "high", "Null check missing foo")
        consensus_f2 = self._f("cursor", "a.py", 10, "high", "Null check missing foo")
        spiral = self._f("codex", "c.py", 20, "medium", "Race on write path x")
        trivial_low = self._f("cursor", "d.py", 3, "nit", "Rename variable xyz")
        spiral_key = (spiral["file"], spiral["line"], spiral["topic"])

        out = bramble_ops.triage(
            [consensus_f1, consensus_f2, spiral, trivial_low],
            prior_fixed_keys={spiral_key},
        )
        plan = out["action_plan"]
        self.assertEqual(len(plan["must_fix"]), 1)  # consensus only; spiral routes to escalate
        self.assertEqual(len(plan["escalate"]), 1)
        self.assertEqual(len(plan["batch_ack"]), 1)
        self.assertEqual(len(plan["consider_fix"]), 0)
        # must_fix/consider_fix/batch_ack/escalate must all be present.
        self.assertEqual(set(plan.keys()), {"must_fix", "consider_fix", "batch_ack", "escalate"})

    def test_action_plan_routes_single_medium_to_consider_fix(self) -> None:
        medium = self._f("cursor", "x.py", 1, "medium", "Missing error handling path")
        out = bramble_ops.triage([medium], prior_fixed_keys=set())
        self.assertEqual(len(out["action_plan"]["consider_fix"]), 1)
        self.assertEqual(len(out["action_plan"]["must_fix"]), 0)

    def test_total_counts_all_sources_not_just_bramble(self) -> None:
        # `total` reports the full triaged finding set including PR comments
        # and CI failures, so it stays >= `unique` (which dedupes by key).
        bramble_finding = self._f("codex", "a.py", 10, "high", "Bramble issue")
        pr_comment = {
            "id": 1,
            "source": "github-inline",
            "path": "b.py",
            "line": 5,
            "author": "human",
            "body": "Please add a comment here.",
        }
        out = bramble_ops.triage(
            [bramble_finding],
            prior_fixed_keys=set(),
            pr_comments=[pr_comment],
        )
        self.assertEqual(out["total"], 2)
        self.assertGreaterEqual(out["total"], out["unique"])


class TestTriageWithPRComments(unittest.TestCase):
    def test_pr_comment_routes_to_single_medium_by_default(self) -> None:
        comment = {
            "id": 100,
            "source": "github-inline",
            "path": "a.py",
            "line": 20,
            "body": "please rename this variable",
            "is_bot": True,
            "author": "cursor[bot]",
        }
        out = bramble_ops.triage([], prior_fixed_keys=set(), pr_comments=[comment])
        self.assertEqual(len(out["single_medium"]), 1)
        self.assertEqual(out["single_medium"][0]["finding"]["source"], "github-inline")
        self.assertEqual(out["single_critical"], [])

    def test_pr_comment_with_security_keyword_routes_to_critical(self) -> None:
        comment = {
            "id": 101,
            "source": "github-inline",
            "path": "auth.py",
            "line": 40,
            "body": "this is a critical security issue — must fix before merge",
            "is_bot": False,
            "author": "reviewer",
        }
        out = bramble_ops.triage([], prior_fixed_keys=set(), pr_comments=[comment])
        self.assertEqual(len(out["single_critical"]), 1)

    def test_pr_comment_dedupes_against_bramble_finding_into_consensus(self) -> None:
        # A bramble finding and a PR comment on the same (path, line, topic)
        # should collapse into a consensus entry so the orchestrator treats
        # them as a single must-fix item.
        message = "null check missing for user input"
        bramble_finding = {
            "source": "codex",
            "severity": "high",
            "file": "a.py",
            "line": 10,
            "message": message,
            "suggestion": None,
            "topic": _common.topic_of(message),
        }
        comment = {
            "id": 200,
            "source": "github-inline",
            "path": "a.py",
            "line": 10,
            "body": message,
            "is_bot": True,
            "author": "cursor[bot]",
        }
        out = bramble_ops.triage([bramble_finding], prior_fixed_keys=set(), pr_comments=[comment])
        self.assertEqual(len(out["consensus"]), 1)
        self.assertEqual(
            out["consensus"][0]["sources"],
            ["codex", "github-inline"],
        )


class TestTriageWithCIFailures(unittest.TestCase):
    def test_genuine_ci_failure_routes_to_single_critical(self) -> None:
        ci = {
            "job_id": 42,
            "job_name": "build",
            "failed_tests": ["TestFoo"],
            "is_flake": False,
            "flake_reason": None,
            "assertion_snippet": "expected 1 got 2",
        }
        out = bramble_ops.triage([], prior_fixed_keys=set(), ci_failures=[ci])
        self.assertEqual(len(out["single_critical"]), 1)
        self.assertEqual(out["single_critical"][0]["finding"]["source"], "ci")

    def test_flake_routes_to_low_acks(self) -> None:
        ci = {
            "job_id": 43,
            "job_name": "build",
            "failed_tests": [],
            "is_flake": True,
            "flake_reason": "etxtbsy",
            "assertion_snippet": "text file busy",
        }
        out = bramble_ops.triage([], prior_fixed_keys=set(), ci_failures=[ci])
        self.assertEqual(len(out["low_acks"]), 1)
        self.assertEqual(out["single_critical"], [])


class TestPriorFixedKeys(unittest.TestCase):
    def test_collects_only_fixed_actions(self) -> None:
        state = {
            "rounds": [
                {
                    "n": 1,
                    "comment_actions": [
                        {"action": "fixed", "path": "a.py", "line": 10, "topic": "null check"},
                        {"action": "false_positive", "path": "b.py", "line": 5, "topic": "race"},
                        {"action": "ack", "path": "c.py", "line": 1, "topic": "rename"},
                    ],
                },
                {
                    "n": 2,
                    "comment_actions": [
                        {"action": "fixed", "path": "d.py", "line": 99, "topic": "oops"},
                    ],
                },
            ]
        }
        keys = bramble_ops.prior_fixed_keys(state)
        self.assertEqual(
            keys,
            {("a.py", 10, "null check"), ("d.py", 99, "oops")},
        )

    def test_none_state_returns_empty(self) -> None:
        self.assertEqual(bramble_ops.prior_fixed_keys(None), set())


class TestParseRoundWithStreams(unittest.TestCase):
    def test_uses_stream_when_provided(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            cx = Path(d) / "codex.log"
            cx.write_text(
                '{"event":"progress","kind":"session-started"}\n'
                '{"schema_version":1,"status":"ok","backend":"codex",'
                '"review":{"verdict":"accepted","issues":[]}}\n'
            )
            # cursor intentionally omitted from streams: should fall back to
            # legacy envelope path and, finding none, contribute 0 findings.
            out = bramble_ops.parse_round(
                1,
                streams={"codex": cx},
                repo="kernel",
                pr=42,
                backends=["codex"],
            )
            self.assertEqual(out, [])  # no issues in the happy envelope


class TestTriageCLIShapeCompat(unittest.TestCase):
    """The triage CLI's --pr-comments must accept both:

    - legacy bare list of classify_comments rows, and
    - pr_ops.fetch_comments' new wrapped shape
      {"comments": [...], "noise_filtered": N, "noise_samples": [...]}

    Tested via main(argv=[...]) so the argparse dispatch path runs.
    """

    def _run(self, pr_comments_path: Path) -> dict:
        import io
        from contextlib import redirect_stdout

        # Minimal bramble findings file: empty list of findings is fine.
        with tempfile.TemporaryDirectory() as d:
            findings_stub = Path(d) / "prior.json"
            # No prior state -> spiral matches disabled.
            findings_stub.write_text(json.dumps({"rounds": []}))
            buf = io.StringIO()
            with redirect_stdout(buf):
                rc = bramble_ops.main(
                    [
                        "triage",
                        "1",
                        str(findings_stub),
                        "--pr-comments",
                        str(pr_comments_path),
                    ]
                )
            self.assertEqual(rc, 0, f"main exited non-zero; stdout={buf.getvalue()}")
            return json.loads(buf.getvalue())

    def _sample_pr_comment(self) -> dict:
        return {
            "id": 1,
            "source": "github-inline",
            "path": "a.py",
            "line": 10,
            "body": "rename this var",
            "is_bot": False,
            "author": "alice",
        }

    def test_bare_list_shape_accepted(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "bare.json"
            p.write_text(json.dumps([self._sample_pr_comment()]))
            got = self._run(p)
        # Single non-keyword PR comment routes to single_medium.
        self.assertEqual(len(got["single_medium"]), 1)

    def test_wrapped_shape_accepted(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "wrapped.json"
            p.write_text(
                json.dumps(
                    {
                        "comments": [self._sample_pr_comment()],
                        "noise_filtered": 2,
                        "noise_samples": [
                            {"id": 99, "author": "linear[bot]", "pattern": "linear-linkback"}
                        ],
                    }
                )
            )
            got = self._run(p)
        # Same routing — adapter unwrapped comments before triage saw them.
        self.assertEqual(len(got["single_medium"]), 1)

    def test_wrapped_empty_comments_key_accepted(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "wrapped.json"
            p.write_text(json.dumps({"comments": [], "noise_filtered": 5, "noise_samples": []}))
            got = self._run(p)
        self.assertEqual(got["single_medium"], [])


if __name__ == "__main__":
    unittest.main()
