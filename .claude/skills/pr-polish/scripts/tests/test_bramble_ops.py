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
    """Per-turn metadata for the resumed model: what the immediately-prior
    round actioned, plus the diff since that round closed.

    Bramble's BuildFollowUpJSONPromptWithScope embeds non-empty goal as
    "Context for this turn:" so the model reads this as orchestrator-
    supplied state, not as a re-statement of the session goal.
    """

    def _state(self, rounds: list[dict], **extra) -> dict:
        out = {"pr_number": 42, "rounds": rounds}
        out.update(extra)
        return out

    def test_round_one_returns_empty(self) -> None:
        state = self._state([{"n": 1, "comment_actions": [{"action": "fixed", "path": "a.go", "line": 5}]}])
        self.assertEqual(bramble_ops.action_history_goal(state, 1), "")

    def test_no_state_returns_empty(self) -> None:
        self.assertEqual(bramble_ops.action_history_goal(None, 2), "")

    def test_no_prior_rounds_returns_empty(self) -> None:
        self.assertEqual(bramble_ops.action_history_goal(self._state([]), 2), "")

    def test_no_prior_actions_returns_empty(self) -> None:
        state = self._state([{"n": 1, "comment_actions": []}])
        self.assertEqual(bramble_ops.action_history_goal(state, 2), "")

    def test_summarizes_fixed_and_skipped(self) -> None:
        state = self._state([
            {"n": 1, "comment_actions": [
                {"action": "fixed", "path": "a.go", "line": 10, "topic": "null check missing"},
                {"action": "wont_fix", "path": "b.py", "line": 42, "reason": "design tradeoff", "topic": "unused param"},
                {"action": "stale", "path": "c.go", "line": 5, "topic": "old null guard"},
                {"action": "ack", "path": "d.go", "line": 8, "topic": "rename helper"},
            ]},
        ])
        out = bramble_ops.action_history_goal(state, 2)
        self.assertIn("Round 2.", out)
        self.assertIn("Prior round fixed:", out)
        self.assertIn("a.go:10 — null check missing", out)
        self.assertIn("Skipped:", out)
        # Reason wins when present (the orchestrator's decision is what
        # the model needs to know to avoid re-arguing the skip).
        self.assertIn("b.py:42 wont_fix: design tradeoff", out)
        # Topic-only fallback when no reason was recorded.
        self.assertIn("d.go:8 ack: rename helper", out)
        # Stale entries are deliberately excluded from the goal channel.
        self.assertNotIn("c.go:5", out)
        self.assertNotIn("stale", out)

    def test_stale_actions_excluded_from_goal(self) -> None:
        # Even a round that's *only* stale acks should not bloat the
        # goal — the model has nothing actionable from superseded
        # bot comments. Should fall through to "no skipped" (and if no
        # fixed either, return "" so caller falls back to PR_SUMMARY).
        state = self._state([
            {"n": 1, "comment_actions": [
                {"action": "stale", "path": f"x{i}.py", "line": i,
                 "reason": "Superseded by abc; pre-series",
                 "topic": "### bot body **Medium**"} for i in range(10)
            ]},
        ])
        out = bramble_ops.action_history_goal(state, 2)
        self.assertEqual(out, "")  # no fixed, no non-stale skipped

    def test_drops_source_label_from_entries(self) -> None:
        # Source is triage's concern; the resumed model treats every
        # finding the same way. Don't burn tokens on (codex)/(cursor).
        state = self._state([
            {"n": 1, "comment_actions": [
                {"action": "fixed", "path": "a.go", "line": 10, "source": "codex", "topic": "x"},
                {"action": "wont_fix", "path": "b.py", "line": 42, "source": "cursor", "topic": "y", "reason": "z"},
            ]},
        ])
        out = bramble_ops.action_history_goal(state, 2)
        self.assertNotIn("(codex)", out)
        self.assertNotIn("(cursor)", out)

    def test_includes_topic_when_present(self) -> None:
        state = self._state([
            {"n": 1, "comment_actions": [
                {"action": "fixed", "path": "a.go", "line": 10, "topic": "null check missing on builder lite"},
            ]},
        ])
        out = bramble_ops.action_history_goal(state, 2)
        self.assertIn("a.go:10 — null check missing on builder lite", out)

    def test_truncates_long_topics(self) -> None:
        long_topic = "x" * 200
        state = self._state([
            {"n": 1, "comment_actions": [
                {"action": "fixed", "path": "a.go", "line": 10, "topic": long_topic},
            ]},
        ])
        out = bramble_ops.action_history_goal(state, 2)
        self.assertIn("…", out)
        # After truncation each entry stays well under the 200-char raw topic.
        # _TOPIC_CHAR_CAP is 80 so the rendered entry is <120 chars total.
        # Find the entry slice and bound it.
        entry = out.split("Prior round fixed: ")[1].split(".")[0]
        self.assertLess(len(entry), 120)

    def test_handles_action_without_topic(self) -> None:
        # No em dash when topic is missing — bare path:line.
        state = self._state([
            {"n": 1, "comment_actions": [
                {"action": "fixed", "path": "a.go", "line": 10},
            ]},
        ])
        out = bramble_ops.action_history_goal(state, 2)
        self.assertIn("a.go:10.", out)  # period is the line terminator
        self.assertNotIn("a.go:10 —", out)

    def test_only_includes_immediately_prior_round(self) -> None:
        # State has rounds 1, 2, 3. Goal at round 3 references round 2 only;
        # earlier turns are in the model's session conversation already.
        state = self._state([
            {"n": 1, "comment_actions": [{"action": "fixed", "path": "round1.go", "line": 1}]},
            {"n": 2, "comment_actions": [{"action": "fixed", "path": "round2.go", "line": 2}]},
        ])
        out = bramble_ops.action_history_goal(state, 3)
        self.assertIn("round2.go:2", out)
        self.assertNotIn("round1.go:1", out)

    def test_handles_actions_without_path(self) -> None:
        state = self._state([
            {"n": 1, "comment_actions": [
                {"action": "fixed", "path": "a.go", "line": 10, "topic": "x"},
                {"action": "ack", "path": None, "line": None, "source": "github-review"},
            ]},
        ])
        out = bramble_ops.action_history_goal(state, 2)
        self.assertIn("a.go:10", out)
        self.assertNotIn("None", out)

    def test_caps_long_lists(self) -> None:
        actions = [
            {"action": "fixed", "path": f"f{i}.go", "line": i, "topic": f"t{i}"}
            for i in range(30)
        ]
        state = self._state([{"n": 1, "comment_actions": actions}])
        out = bramble_ops.action_history_goal(state, 2)
        self.assertIn("more)", out)
        # Cap is 20; suffix mentions the rest.
        self.assertIn("(10 more)", out)

    def test_emits_files_changed_line(self) -> None:
        from unittest.mock import patch  # noqa: PLC0415

        state = self._state([
            {"n": 1, "comment_actions": [{"action": "fixed", "path": "a.go", "line": 1, "topic": "x"}],
             "head_after": "sha-prev"},
        ])
        with patch("_common.run") as run_mock:
            run_mock.return_value = type("R", (), {"returncode": 0, "stdout": "a.go\nb.py\n"})()
            out = bramble_ops.action_history_goal(state, 2, head_before="sha-cur")
        self.assertIn("Files changed since round 1: a.go, b.py.", out)

    def test_omits_files_changed_line_when_diff_empty(self) -> None:
        from unittest.mock import patch  # noqa: PLC0415

        state = self._state([
            {"n": 1, "comment_actions": [{"action": "fixed", "path": "a.go", "line": 1}],
             "head_after": "sha-prev"},
        ])
        with patch("_common.run") as run_mock:
            run_mock.return_value = type("R", (), {"returncode": 0, "stdout": ""})()
            out = bramble_ops.action_history_goal(state, 2, head_before="sha-cur")
        self.assertNotIn("Files changed", out)

    def test_omits_files_changed_line_when_no_head_before(self) -> None:
        # Caller didn't pass head_before — we don't shell out to git
        # speculatively; just skip the line.
        state = self._state([
            {"n": 1, "comment_actions": [{"action": "fixed", "path": "a.go", "line": 1}],
             "head_after": "sha-prev"},
        ])
        out = bramble_ops.action_history_goal(state, 2)
        self.assertNotIn("Files changed", out)


class TestIsFirstRoundOfSeriesAgreement(unittest.TestCase):
    """The helper is duplicated in pr_ops and bramble_ops to avoid an
    import cycle (pr_ops already imports bramble_ops via
    _persist_round_findings). This test asserts the two copies agree on
    a shared fixture table so they can't drift apart silently.
    """

    def test_helpers_agree(self) -> None:
        import pr_ops  # noqa: PLC0415

        cases: list[tuple[dict | None, int]] = [
            (None, 1),
            ({"rounds": []}, 1),
            ({"rounds": []}, 5),
            ({"rounds": [{"n": 1}], "completed": True}, 2),
            ({"rounds": [{"n": 5}], "completed": True}, 6),
            ({"rounds": [{"n": 1}], "completed": False}, 2),
            ({"rounds": [{"n": 1}, {"n": 2}], "completed": False}, 3),
        ]
        for state, n in cases:
            self.assertEqual(
                pr_ops._is_first_round_of_series(state, n),
                bramble_ops._is_first_round_of_series(state, n),
                f"helpers disagree on (state={state}, n={n})",
            )


class TestPriorSessionIdSeriesBoundary(unittest.TestCase):
    """At a series boundary (prior loop completed), prior_session_id must
    return empty so the new audit gets a fresh bramble session."""

    def test_returns_empty_at_series_start(self) -> None:
        state = {
            "completed": True,
            "rounds": [
                {"n": 5, "session_ids": {"codex": "abc-123"}},
            ],
        }
        self.assertEqual(bramble_ops.prior_session_id(state, "codex", 6), "")

    def test_returns_id_within_same_series(self) -> None:
        state = {
            "rounds": [
                {"n": 1, "session_ids": {"codex": "abc-123"}},
            ],
        }
        self.assertEqual(bramble_ops.prior_session_id(state, "codex", 2), "abc-123")

    def test_explicit_is_new_series_true_returns_empty_even_when_state_says_otherwise(self) -> None:
        # The SKILL captures IS_NEW_SERIES at Step 0.5, before
        # state_append_round mutates state["completed"] to false. The
        # captured value must remain authoritative for the rest of the
        # round, even though the state file by then looks "in-progress".
        state = {
            "completed": False,  # state_append_round already cleared it
            "rounds": [{"n": 5, "session_ids": {"codex": "old-abc"}}],
        }
        self.assertEqual(
            bramble_ops.prior_session_id(state, "codex", 6, is_new_series=True),
            "",
        )

    def test_explicit_is_new_series_false_overrides_completed_flag(self) -> None:
        # Symmetric belt-and-braces: caller can also force resume even
        # when the state says completed=true (e.g., a manual replay).
        state = {
            "completed": True,
            "rounds": [{"n": 5, "session_ids": {"codex": "abc"}}],
        }
        self.assertEqual(
            bramble_ops.prior_session_id(state, "codex", 6, is_new_series=False),
            "abc",
        )



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

    def test_spiral_match_via_location_only_key(self) -> None:
        # Rewording-resilient escalation: prior_fixed_keys carries
        # (path, line, None) alongside the strict (path, line, topic) key
        # so a fix-then-reword regression at the same site still
        # escalates even when the new finding's topic differs from what
        # the orchestrator persisted.
        f = self._f("codex", "a.py", 10, "high", "Different wording for the same bug")
        # Prior round had topic "null check missing"; we only keep the
        # location-only companion in prior_fixed_keys.
        location_key = (f["file"], f["line"], None)
        out = bramble_ops.triage([f], prior_fixed_keys={location_key})
        self.assertEqual(len(out["spiral_matches"]), 1)
        # The reported key reflects the new finding's full triage key.
        self.assertEqual(
            out["spiral_matches"][0]["key"],
            [f["file"], f["line"], f["topic"]],
        )

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
    def test_total_includes_pr_comments_and_ci_failures(self) -> None:
        # Regression guard: round 5 fixed `total` to count all merged
        # findings (bramble + pr_comments + ci_failures), not just bramble.
        # Without this test, a refactor could regress to len(findings)
        # and triage's `total` would silently under-count comment-only
        # or CI-only triage runs.
        bramble = [{
            "source": "codex", "severity": "high", "file": "a.py", "line": 1,
            "message": "boom", "topic": "boom",
        }]
        pr_comments = [{
            "id": 99, "source": "github-inline", "path": "b.py", "line": 5,
            "body": "rename", "is_bot": True, "author": "cursor[bot]",
        }]
        ci_failures = [{
            "job_id": 222, "job_name": "build", "is_flake": False,
            "failed_tests": ["TestFoo"], "assertion_snippet": "expected 1 got 2",
        }]
        out = bramble_ops.triage(
            bramble, prior_fixed_keys=set(),
            pr_comments=pr_comments, ci_failures=ci_failures,
        )
        self.assertEqual(out["total"], 3)

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

    def test_typo_stream_path_surfaces_synthetic_finding(self) -> None:
        # If the orchestrator passes --stream cursor=/typo/path, parse_round
        # used to silently drop it. Now it emits a high-severity placeholder
        # so the missing source is visible in triage.
        bogus = Path("/nonexistent/cursor.log")
        out = bramble_ops.parse_round({"cursor": bogus}, backends=["cursor"])
        self.assertEqual(len(out), 1)
        self.assertEqual(out[0]["severity"], "high")
        self.assertEqual(out[0]["topic"], "stream-missing")


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


class TestGoalCLI(unittest.TestCase):
    """SKILL invokes `bramble_ops.py goal {ROUND} ...` every round."""

    def _run(self, *argv) -> str:
        import io
        from contextlib import redirect_stdout
        buf = io.StringIO()
        with redirect_stdout(buf):
            rc = bramble_ops.main(list(argv))
        assert rc == 0, buf.getvalue()
        return buf.getvalue().rstrip("\n")

    def test_round_one_prints_pr_summary(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            sf = Path(d) / "state.json"
            sf.write_text(json.dumps({"rounds": []}))
            out = self._run("goal", "1", "--pr-summary", "PR #99: refactor", "--state-file", str(sf))
        self.assertEqual(out, "PR #99: refactor")

    def test_round_two_with_actions_prints_history(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            sf = Path(d) / "state.json"
            sf.write_text(
                json.dumps(
                    {
                        "rounds": [
                            {
                                "n": 1,
                                "comment_actions": [
                                    {"action": "fixed", "path": "a.go", "line": 5, "source": "codex"},
                                ],
                            }
                        ]
                    }
                )
            )
            out = self._run("goal", "2", "--pr-summary", "PR_SUM", "--state-file", str(sf))
        self.assertIn("Round 2", out)
        self.assertIn("a.go:5", out)

    def test_head_before_flag_emits_files_changed_line(self) -> None:
        # SKILL passes --head-before "$(git rev-parse HEAD)"; the CLI must
        # accept it and thread it into action_history_goal so the model
        # sees "Files changed since round N-1: ...".
        from unittest.mock import patch  # noqa: PLC0415

        with tempfile.TemporaryDirectory() as d:
            sf = Path(d) / "state.json"
            sf.write_text(
                json.dumps(
                    {
                        "rounds": [
                            {
                                "n": 1,
                                "comment_actions": [
                                    {"action": "fixed", "path": "a.go", "line": 5},
                                ],
                                "head_after": "sha-prev",
                            }
                        ]
                    }
                )
            )
            with patch("_common.run") as run_mock:
                run_mock.return_value = type("R", (), {"returncode": 0, "stdout": "a.go\nb.py\n"})()
                out = self._run(
                    "goal", "2",
                    "--pr-summary", "PR_SUM",
                    "--state-file", str(sf),
                    "--head-before", "sha-cur",
                )
        self.assertIn("Files changed since round 1: a.go, b.py.", out)


class TestPriorSessionIDCLI(unittest.TestCase):
    """SKILL shells out to `bramble_ops.py prior-session-id <backend> N` per round."""

    def _run(self, *argv) -> str:
        import io
        from contextlib import redirect_stdout
        buf = io.StringIO()
        with redirect_stdout(buf):
            rc = bramble_ops.main(list(argv))
        assert rc == 0, buf.getvalue()
        return buf.getvalue().rstrip("\n")

    def test_returns_session_id_when_present(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            sf = Path(d) / "state.json"
            sf.write_text(
                json.dumps({"rounds": [{"n": 1, "session_ids": {"codex": "abc-123"}}]})
            )
            out = self._run("prior-session-id", "codex", "2", "--state-file", str(sf))
        self.assertEqual(out, "abc-123")

    def test_returns_empty_when_absent(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            sf = Path(d) / "state.json"
            sf.write_text(json.dumps({"rounds": []}))
            out = self._run("prior-session-id", "codex", "2", "--state-file", str(sf))
        self.assertEqual(out, "")

    def test_is_new_series_flag_forces_empty(self) -> None:
        # By the time the SKILL calls this, state_append_round has cleared
        # `completed: true` to false. The captured IS_NEW_SERIES from
        # Step 0.5 must override that mutation.
        with tempfile.TemporaryDirectory() as d:
            sf = Path(d) / "state.json"
            sf.write_text(
                json.dumps(
                    {
                        "completed": False,  # already cleared by state_append_round
                        "rounds": [{"n": 5, "session_ids": {"codex": "abc"}}],
                    }
                )
            )
            out = self._run(
                "prior-session-id", "codex", "6",
                "--state-file", str(sf),
                "--is-new-series", "1",
            )
        self.assertEqual(out, "")

    def test_is_new_series_zero_resumes_normally(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            sf = Path(d) / "state.json"
            sf.write_text(
                json.dumps(
                    {
                        "rounds": [{"n": 1, "session_ids": {"codex": "abc"}}],
                    }
                )
            )
            out = self._run(
                "prior-session-id", "codex", "2",
                "--state-file", str(sf),
                "--is-new-series", "0",
            )
        self.assertEqual(out, "abc")





if __name__ == "__main__":
    unittest.main()
