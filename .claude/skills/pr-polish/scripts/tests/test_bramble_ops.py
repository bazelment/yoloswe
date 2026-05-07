"""Unit tests for bramble_ops. Hermetic: no bramble, no subprocess."""

from __future__ import annotations

import json
import os
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


class TestBrambleBin(unittest.TestCase):
    def test_defaults_to_path_lookup(self) -> None:
        with patch.dict("os.environ", {}, clear=False):
            os.environ.pop("BRAMBLE_BIN", None)
            self.assertEqual(bramble_ops.bramble_bin(), "bramble")

    def test_honors_env(self) -> None:
        with patch.dict("os.environ", {"BRAMBLE_BIN": "/tmp/dev/bramble"}):
            self.assertEqual(bramble_ops.bramble_bin(), "/tmp/dev/bramble")


class TestGoalForRound(unittest.TestCase):
    """Round 1 carries PR_SUMMARY; round 2+ carries action history (or
    falls back to PR_SUMMARY when state has no actions yet)."""

    def test_round_one_returns_pr_summary(self) -> None:
        self.assertEqual(
            bramble_ops.goal_for_round(1, "PR #42: refactor", state=None),
            "PR #42: refactor",
        )

    def test_round_two_with_actions_uses_history(self) -> None:
        state = {
            "rounds": [
                {
                    "n": 1,
                    "comment_actions": [
                        {"action": "fixed", "path": "a.go", "line": 5, "source": "codex"},
                    ],
                }
            ]
        }
        goal = bramble_ops.goal_for_round(2, "PR_SUMMARY", state)
        self.assertIn("Round 2", goal)
        self.assertIn("a.go:5", goal)
        self.assertNotIn("PR_SUMMARY", goal)

    def test_round_two_with_empty_state_falls_back(self) -> None:
        # Round 1 produced no actions; reanchor on PR_SUMMARY rather than send empty.
        self.assertEqual(
            bramble_ops.goal_for_round(2, "PR_SUMMARY", state={"rounds": []}),
            "PR_SUMMARY",
        )

    def test_round_two_with_no_state_falls_back(self) -> None:
        self.assertEqual(
            bramble_ops.goal_for_round(2, "PR_SUMMARY", state=None),
            "PR_SUMMARY",
        )


class TestActionHistoryGoal(unittest.TestCase):
    """Per-turn metadata for the resumed model: what prior rounds actioned.

    Bramble's BuildFollowUpJSONPromptWithScope embeds non-empty goal as
    "Context for this turn:" so the model reads action history as
    orchestrator-supplied state, not as a re-statement of the session goal.
    """

    def _state(self, rounds: list[dict]) -> dict:
        return {"pr_number": 42, "rounds": rounds}

    def test_round_one_returns_empty(self) -> None:
        # Round 1 uses PR_SUMMARY; action history doesn't apply.
        state = self._state([{"n": 1, "comment_actions": [{"action": "fixed", "path": "a.go", "line": 5, "source": "codex"}]}])
        self.assertEqual(bramble_ops.action_history_goal(state, 1), "")

    def test_no_state_returns_empty(self) -> None:
        self.assertEqual(bramble_ops.action_history_goal(None, 2), "")

    def test_no_prior_rounds_returns_empty(self) -> None:
        # State exists but no rounds; round 2 has nothing to summarize.
        self.assertEqual(bramble_ops.action_history_goal(self._state([]), 2), "")

    def test_no_prior_actions_returns_empty(self) -> None:
        # Round 1 ran but produced no actions (e.g. zero findings, all stale).
        state = self._state([{"n": 1, "comment_actions": []}])
        self.assertEqual(bramble_ops.action_history_goal(state, 2), "")

    def test_summarizes_fixed_and_skipped(self) -> None:
        state = self._state([
            {"n": 1, "comment_actions": [
                {"action": "fixed", "path": "a.go", "line": 10, "source": "codex"},
                {"action": "wont_fix", "path": "b.py", "line": 42, "source": "cursor", "reason": "design tradeoff"},
                {"action": "stale", "path": "c.go", "line": 5, "source": "codex"},
                {"action": "ack", "path": "d.go", "line": 8, "source": "cursor"},
            ]},
        ])
        out = bramble_ops.action_history_goal(state, 2)
        self.assertIn("Round 2.", out)
        self.assertIn("Prior rounds fixed:", out)
        self.assertIn("a.go:10 (codex)", out)
        self.assertIn("Skipped:", out)
        self.assertIn("b.py:42 (cursor) (wont_fix)", out)
        self.assertIn("c.go:5 (codex) (stale)", out)
        self.assertIn("d.go:8 (cursor) (ack)", out)

    def test_only_includes_actions_from_prior_rounds(self) -> None:
        # An entry under round 2 must NOT show up in the round-2 goal —
        # we summarize what was actioned BEFORE the current round.
        state = self._state([
            {"n": 1, "comment_actions": [{"action": "fixed", "path": "a.go", "line": 10, "source": "codex"}]},
            {"n": 2, "comment_actions": [{"action": "fixed", "path": "ZZZ.go", "line": 99, "source": "codex"}]},
        ])
        out = bramble_ops.action_history_goal(state, 2)
        self.assertIn("a.go:10", out)
        self.assertNotIn("ZZZ.go:99", out)

    def test_handles_actions_without_path(self) -> None:
        # PR-level / review-level comments have no path/line. Drop them
        # from the summary rather than emitting "None:None".
        state = self._state([
            {"n": 1, "comment_actions": [
                {"action": "fixed", "path": "a.go", "line": 10, "source": "codex"},
                {"action": "ack", "path": None, "line": None, "source": "github-review"},
            ]},
        ])
        out = bramble_ops.action_history_goal(state, 2)
        self.assertIn("a.go:10", out)
        self.assertNotIn("None", out)

    def test_caps_long_lists(self) -> None:
        # Don't blow up the prompt on a verbose round. Cap is _ACTION_HISTORY_CAP.
        actions = [
            {"action": "fixed", "path": f"f{i}.go", "line": i, "source": "codex"}
            for i in range(30)
        ]
        state = self._state([{"n": 1, "comment_actions": actions}])
        out = bramble_ops.action_history_goal(state, 2)
        # Should mention the truncation count.
        self.assertIn("more)", out)



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

    def test_multiline_envelope_parses_as_lint_path(self) -> None:
        # lint_gate.py writes its envelope through atomic_write_json with
        # indent=2 — a single multi-line JSON object, not NDJSON. The
        # NDJSON line-scanner can't see that as an envelope, so parse_stream
        # must try whole-file json.loads first. Otherwise every round
        # synthesizes a phantom 'bramble-empty-envelope' high-severity finding.
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "lint-envelope.json"
            p.write_text(
                "{\n"
                '  "schema_version": 1,\n'
                '  "status": "ok",\n'
                '  "backend": "lint",\n'
                '  "review": {\n'
                '    "verdict": "advisory",\n'
                '    "issues": [\n'
                "      {\n"
                '        "severity": "medium",\n'
                '        "file": "a.py",\n'
                '        "line": 7,\n'
                '        "message": "Empty except clause",\n'
                '        "suggestion": "specify exception type"\n'
                "      }\n"
                "    ]\n"
                "  }\n"
                "}\n"
            )
            got = bramble_ops.parse_stream(p, source="lint")
            self.assertEqual(len(got), 1)
            self.assertEqual(got[0]["severity"], "medium")
            self.assertEqual(got[0]["source"], "lint")
            self.assertNotEqual(got[0].get("topic"), "bramble-empty-envelope")

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

    def test_accepts_gemini_and_lint(self) -> None:
        # Regression guard for two related shifts:
        #  * gemini envelopes exist on disk for old runs (e.g.
        #    ~/.bramble/projects/kernel-2755/r1/gemini-envelope.json) and the
        #    SKILL.md --gemini flag depends on this --stream path.
        #  * lint is a new source (deterministic linter findings via
        #    lint_gate.py) flowing through the same triage pipeline.
        # Both must round-trip through _parse_stream_args without rejection.
        out = bramble_ops._parse_stream_args(
            ["codex=/a.log", "cursor=/b.log", "gemini=/c.log", "lint=/d.json"]
        )
        self.assertEqual(out["gemini"], Path("/c.log"))
        self.assertEqual(out["lint"], Path("/d.json"))


class TestLintSource(unittest.TestCase):
    """Pin the contract that lint findings flow through the same parse →
    triage → action_plan pipeline as the LLM backends, with no special-casing.
    """

    def test_lint_envelope_parses_like_a_normal_backend(self) -> None:
        # lint_gate.py emits an envelope whose shape matches the bramble
        # ResultEnvelope (status=ok, review.issues=[…]). parse_envelope
        # should treat ``source="lint"`` as just another backend.
        env = {
            "status": "ok",
            "schema_version": 1,
            "backend": "lint",
            "review": {
                "verdict": "advisory",
                "issues": [
                    {
                        "file": "a.py",
                        "line": 12,
                        "severity": "low",
                        "message": "[ruff F401] unused import",
                    },
                ],
            },
        }
        findings = bramble_ops.parse_envelope(env, source="lint")
        self.assertEqual(len(findings), 1)
        self.assertEqual(findings[0]["source"], "lint")
        self.assertEqual(findings[0]["severity"], "low")

    def test_lint_finding_routes_to_low_acks_in_triage(self) -> None:
        # A single low-severity lint finding lands in low_acks/batch_ack just
        # like a single low-severity codex finding. No new bucket needed.
        finding = {
            "source": "lint",
            "severity": "low",
            "file": "a.py",
            "line": 1,
            "message": "[ruff W291] trailing whitespace",
            "topic": "ruff w291 trailing whitespace",
        }
        out = bramble_ops.triage([finding], prior_fixed_keys=set())
        self.assertEqual(len(out["low_acks"]), 1)
        self.assertEqual(out["action_plan"]["batch_ack"][0]["finding"]["source"], "lint")

    def test_lint_plus_codex_at_same_key_is_consensus(self) -> None:
        # When lint and codex agree on (file, line, topic) the finding gets
        # consensus routing — that's the whole point of treating lint as a
        # peer source. Pinning this so a future tweak doesn't accidentally
        # exclude lint from consensus eligibility.
        common = {
            "file": "a.py",
            "line": 5,
            "topic": "unused import",
            "message": "unused import",
            "severity": "low",
        }
        codex = {**common, "source": "codex"}
        lint = {**common, "source": "lint"}
        out = bramble_ops.triage([codex, lint], prior_fixed_keys=set())
        self.assertEqual(len(out["consensus"]), 1)
        self.assertEqual(
            sorted(out["consensus"][0]["sources"]),
            ["codex", "lint"],
        )


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

    def test_consensus_collapses_same_location_different_topics(self) -> None:
        # Round 7 of pr-polish observed two reviewers flagging the same line
        # with different phrasings — codex said "TestEmitEarlyFailure does
        # not assert resume_status=unverified", cursor said
        # "TestEmitEarlyFailure does not set resumeSessionID". Same finding,
        # same line, different topic. Pre-fix consensus key was
        # (file, line, topic) so each routed to single_medium and the
        # consensus signal was lost; the new key drops topic so
        # location-level agreement is enough.
        findings = [
            self._f("codex", "a.py", 42, "medium", "first phrasing of the same issue"),
            self._f("cursor", "a.py", 42, "medium", "different phrasing of the same issue"),
        ]
        out = bramble_ops.triage(findings, prior_fixed_keys=set())
        self.assertEqual(
            len(out["consensus"]), 1,
            f"expected location-based consensus, got {out['consensus']!r}",
        )
        self.assertEqual(out["consensus"][0]["sources"], ["codex", "cursor"])
        self.assertEqual(len(out["single_medium"]), 0)
        # action_plan must route consensus to must_fix, not consider_fix.
        self.assertEqual(len(out["action_plan"]["must_fix"]), 1)
        self.assertEqual(len(out["action_plan"]["consider_fix"]), 0)

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
        # must_fix/consider_fix/batch_ack/batch_stale/escalate must all be present.
        self.assertEqual(
            set(plan.keys()),
            {"must_fix", "consider_fix", "batch_ack", "batch_stale", "escalate"},
        )

    def test_action_plan_routes_single_medium_to_consider_fix(self) -> None:
        medium = self._f("cursor", "x.py", 1, "medium", "Missing error handling path")
        out = bramble_ops.triage([medium], prior_fixed_keys=set())
        self.assertEqual(len(out["action_plan"]["consider_fix"]), 1)
        self.assertEqual(len(out["action_plan"]["must_fix"]), 0)

    def test_stale_prior_commit_routed_to_dedicated_bucket(self) -> None:
        # A bot comment posted against a superseded SHA must not be re-fixed.
        # It goes into stale_prior_commit and skips the severity buckets so
        # the orchestrator auto-acks it with action="stale".
        stale = self._f("github-inline", "a.py", 10, "medium", "Old comment about foo")
        stale["is_stale_prior_commit"] = True
        fresh = self._f("codex", "b.py", 5, "medium", "Real new finding bar")
        out = bramble_ops.triage([stale, fresh], prior_fixed_keys=set())
        self.assertEqual(len(out["stale_prior_commit"]), 1)
        self.assertEqual(out["stale_prior_commit"][0]["finding"]["file"], "a.py")
        self.assertEqual(len(out["single_medium"]), 1)
        self.assertEqual(out["single_medium"][0]["finding"]["file"], "b.py")
        self.assertEqual(len(out["action_plan"]["batch_stale"]), 1)
        self.assertEqual(len(out["action_plan"]["consider_fix"]), 1)
        # Stale finding must not appear in any of the act-on-it buckets.
        for bucket in ("must_fix", "consider_fix", "batch_ack", "escalate"):
            files = [
                (item.get("finding") or item.get("findings", [{}])[0]).get("file")
                for item in out["action_plan"][bucket]
            ]
            self.assertNotIn("a.py", files, f"stale leaked into {bucket}")

    def test_stale_does_not_form_consensus_with_fresh_same_key(self) -> None:
        # If a stale bot comment shares (file, line, topic) with a fresh
        # codex finding, they MUST NOT pair into spurious consensus —
        # that would re-fix code the stale comment was complaining about
        # before the user already addressed it.
        stale = self._f("github-inline", "a.py", 10, "medium", "Same exact topic here")
        stale["is_stale_prior_commit"] = True
        fresh = self._f("codex", "a.py", 10, "medium", "Same exact topic here")
        out = bramble_ops.triage([stale, fresh], prior_fixed_keys=set())
        self.assertEqual(len(out["consensus"]), 0)
        self.assertEqual(len(out["stale_prior_commit"]), 1)
        # Fresh one still routes by its own severity — single_medium.
        self.assertEqual(len(out["single_medium"]), 1)


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
        # Strict (path, line, topic) plus location-only fallback for
        # rewording-resilient spiral detection.
        self.assertEqual(
            keys,
            {
                ("a.py", 10, "null check"),
                ("a.py", 10, None),
                ("d.py", 99, "oops"),
                ("d.py", 99, None),
            },
        )

    def test_none_state_returns_empty(self) -> None:
        self.assertEqual(bramble_ops.prior_fixed_keys(None), set())


class TestPriorSessionID(unittest.TestCase):
    def test_walks_back_to_latest_prior_round(self) -> None:
        state = {
            "rounds": [
                {"n": 1, "session_ids": {"codex": "r1"}},
                {"n": 2, "session_ids": {"codex": "r2"}},
            ]
        }
        self.assertEqual(bramble_ops.prior_session_id(state, "codex", 3), "r2")

    def test_ignores_current_round_and_supports_legacy_key(self) -> None:
        state = {
            "rounds": [
                {"n": 1, "cursor_session_id": "c1"},
                {"n": 2, "cursor_session_id": "c2"},
            ]
        }
        self.assertEqual(bramble_ops.prior_session_id(state, "cursor", 2), "c1")

    def test_none_or_round_one_returns_empty(self) -> None:
        self.assertEqual(bramble_ops.prior_session_id(None, "codex", 2), "")
        self.assertEqual(
            bramble_ops.prior_session_id({"rounds": [{"n": 1, "session_ids": {"codex": "r1"}}]}, "codex", 1),
            "",
        )


class TestParseRoundWithStreams(unittest.TestCase):
    def test_uses_stream_when_provided(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            cx = Path(d) / "codex.log"
            cx.write_text(
                '{"event":"progress","kind":"session-started"}\n'
                '{"schema_version":1,"status":"ok","backend":"codex",'
                '"review":{"verdict":"accepted","issues":[]}}\n'
            )
            out = bramble_ops.parse_round({"codex": cx}, backends=["codex"])
            self.assertEqual(out, [])  # no issues in the happy envelope

    def test_missing_backend_in_streams_yields_nothing(self) -> None:
        # Backends not present in the streams mapping contribute 0 findings.
        with tempfile.TemporaryDirectory() as d:
            cx = Path(d) / "codex.log"
            cx.write_text(
                '{"schema_version":1,"status":"ok","backend":"codex",'
                '"review":{"verdict":"accepted","issues":[]}}\n'
            )
            out = bramble_ops.parse_round({"codex": cx}, backends=["codex", "cursor"])
            self.assertEqual(out, [])


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
