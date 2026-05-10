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

    def test_is_new_series_returns_pr_summary_at_round_n(self) -> None:
        """Re-invocation after a converged series with the orchestrator's
        IS_NEW_SERIES=1 must return PR_SUMMARY, not walk the prior round's
        head_after to build a 200-file 'Files changed' line."""
        state = {
            "completed": True,  # prior series finished
            "rounds": [
                {
                    "n": 10,
                    "head_after": "deadbeef",  # may be unreachable after rebase
                    "comment_actions": [
                        {"action": "fixed", "path": "a.go", "line": 5, "source": "codex"},
                    ],
                }
            ],
        }
        out = bramble_ops.goal_for_round(
            11, "PR #42: refactor", state, head_before="cafef00d", is_new_series=True
        )
        self.assertEqual(out, "PR #42: refactor")

    def test_is_new_series_false_still_walks_history(self) -> None:
        """Continuation rounds (is_new_series=False) keep the prior behavior."""
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
        out = bramble_ops.goal_for_round(2, "PR_SUMMARY", state, is_new_series=False)
        self.assertIn("Round 2", out)
        self.assertIn("a.go:5", out)


class TestFilesChangedBetweenWarnings(unittest.TestCase):
    """Stderr warning when git diff rejects the cited range — the noisy
    new-series goal blob symptom comes from the unreachable SHA being
    silently swallowed."""

    def test_unreachable_sha_warns_to_stderr(self) -> None:
        import io
        from contextlib import redirect_stderr  # noqa: PLC0415

        buf = io.StringIO()
        with patch("_common.run") as run_mock, redirect_stderr(buf):
            run_mock.return_value = type(
                "R",
                (),
                {
                    "returncode": 128,
                    "stdout": "",
                    "stderr": "fatal: bad revision 'deadbeef..cafef00d'",
                },
            )()
            result = bramble_ops._files_changed_between("deadbeef0000", "cafef00d0000")
        self.assertEqual(result, [])
        stderr = buf.getvalue()
        self.assertIn("git diff", stderr)
        self.assertIn("deadbee", stderr)  # short sha included
        self.assertIn("cafef00", stderr)
        self.assertIn("128", stderr)

    def test_success_does_not_warn(self) -> None:
        import io
        from contextlib import redirect_stderr  # noqa: PLC0415

        buf = io.StringIO()
        with patch("_common.run") as run_mock, redirect_stderr(buf):
            run_mock.return_value = type(
                "R", (), {"returncode": 0, "stdout": "a.go\nb.py\n", "stderr": ""}
            )()
            result = bramble_ops._files_changed_between("aaa", "bbb")
        self.assertEqual(result, ["a.go", "b.py"])
        self.assertEqual(buf.getvalue(), "")


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

    def test_ci_skip_actions_included_in_skipped(self) -> None:
        # ``pre_existing`` and ``flake`` are CI-source skip verbs. A
        # round that handled only CI failures must still propagate that
        # context into the next round's goal — otherwise the resumed
        # model loses the "this failure was already exempted" signal
        # and the helper falls back to PR_SUMMARY.
        state = self._state([
            {"n": 1, "comment_actions": [
                {"action": "pre_existing", "path": "222", "line": None,
                 "topic": "TestFoo", "reason": "ci-compare-base: also fails on main"},
                {"action": "flake", "path": "333", "line": None,
                 "topic": "TestBar", "reason": "etxtbsy"},
            ]},
        ])
        out = bramble_ops.action_history_goal(state, 2)
        self.assertIn("Skipped:", out)
        self.assertIn("222 pre_existing:", out)
        self.assertIn("333 flake:", out)

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

    def test_emits_files_changed_line_with_empty_prior_actions(self) -> None:
        # Pins the invariant that the files-changed sentence is
        # independent of the fixed/skipped buckets. Prior tests all
        # include at least one comment_action, so a refactor that
        # accidentally tied the line emission to fixed/skipped content
        # being non-empty would still pass. With empty prior actions
        # and a non-empty diff, we still want the files line — and we
        # want the early-return guard ("only the Round N. stub") to
        # treat the files line as meaningful content, not skip it.
        from unittest.mock import patch  # noqa: PLC0415

        state = self._state([
            {"n": 1, "comment_actions": [],
             "head_after": "sha-prev"},
        ])
        with patch("_common.run") as run_mock:
            run_mock.return_value = type(
                "R", (), {"returncode": 0, "stdout": "a.go\nb.py\n"},
            )()
            out = bramble_ops.action_history_goal(state, 2, head_before="sha-cur")
        self.assertIn("Round 2.", out)
        self.assertIn("Files changed since round 1: a.go, b.py.", out)
        self.assertNotIn("Prior round fixed", out)
        self.assertNotIn("Skipped", out)

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
        # When lint and codex agree on (file, line) — regardless of how
        # each one phrased the topic — the finding gets consensus
        # routing. The topics here deliberately differ to guard the
        # location-only consensus contract; identical topics would
        # consensus-pair under either rule and wouldn't catch a
        # regression to topic-gated grouping.
        codex = {
            "file": "a.py", "line": 5, "topic": "unused import F401",
            "message": "F401 unused import os", "severity": "low", "source": "codex",
        }
        lint = {
            "file": "a.py", "line": 5, "topic": "ruff f401 unused",
            "message": "[ruff F401] unused import", "severity": "low", "source": "lint",
        }
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

    def test_error_envelope_yields_high_severity_finding(self) -> None:
        # The whole point of synthesizing this finding is to surface a
        # failed bramble run loudly. severity must be "high" so triage
        # routes it through single_critical → must_fix; severity:None
        # would land it in low_acks (low-severity catch-all) and let
        # Monitor failures masquerade as batch-ackable nits.
        env = {"status": "error", "error": "backend crashed"}
        got = bramble_ops.parse_envelope(env, source="cursor")
        self.assertEqual(len(got), 1)
        self.assertEqual(got[0]["source"], "cursor")
        self.assertEqual(got[0]["status"], "error")
        self.assertEqual(got[0]["severity"], "high")
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
        # must_fix/consider_fix/batch_ack/batch_stale/escalate/cluster_hint
        # must all be present.
        self.assertEqual(
            set(plan.keys()),
            {"must_fix", "consider_fix", "batch_ack", "batch_stale",
             "escalate", "cluster_hint"},
        )

    def test_cluster_hint_groups_actionable_findings_by_file(self) -> None:
        # cluster_hint surfaces files concentrating >=2 actionable
        # findings so the fixer plans co-located edits as one task.
        # Without it, defensive cascades arise from minimal patches at
        # cited lines while sibling sites in the same file get found
        # one round at a time.
        a1 = self._f("codex", "a.py", 10, "high", "null body in foo")
        a2 = self._f("cursor", "a.py", 10, "high", "null body in foo")  # consensus
        a3 = self._f("codex", "a.py", 50, "medium", "null body in bar")
        b1 = self._f("cursor", "b.py", 1, "medium", "unrelated single-site issue")
        out = bramble_ops.triage([a1, a2, a3, b1], prior_fixed_keys=set())
        clusters = out["action_plan"]["cluster_hint"]
        # a.py has 2 actionable entries (one consensus pair + one single
        # medium); b.py has 1 → only a.py becomes a cluster candidate.
        self.assertEqual(len(clusters), 1)
        self.assertEqual(clusters[0]["file"], "a.py")
        self.assertEqual(clusters[0]["count"], 2)
        self.assertIn("null body in foo", clusters[0]["topics"])
        self.assertIn(50, clusters[0]["lines"])

    def test_cluster_hint_excludes_spirals(self) -> None:
        # Spiral matches go to ``escalate`` (not auto-fixed); they
        # shouldn't drive the cluster_hint either, since the fixer
        # isn't supposed to act on them without human input.
        spiral = self._f("codex", "a.py", 10, "high", "regression at foo")
        spiral_key = (spiral["file"], spiral["line"], spiral["topic"])
        sibling = self._f("codex", "a.py", 50, "medium", "different issue")
        out = bramble_ops.triage([spiral, sibling], prior_fixed_keys={spiral_key})
        # Only one actionable finding on a.py → no cluster.
        self.assertEqual(out["action_plan"]["cluster_hint"], [])

    def test_cluster_hint_unit_orders_by_count_desc(self) -> None:
        findings = [
            {"file": "a.py", "line": 10, "topic": "t1"},
            {"file": "a.py", "line": 50, "topic": "t2"},
            {"file": "a.py", "line": 90, "topic": "t3"},
            {"file": "b.py", "line": 1, "topic": "u1"},
            {"file": "b.py", "line": 2, "topic": "u2"},
            {"file": "c.py", "line": 1, "topic": "v1"},  # singleton, dropped
            {"file": None, "line": 1, "topic": "no-path"},  # dropped
        ]
        clusters = bramble_ops._cluster_hint(findings)
        self.assertEqual([c["file"] for c in clusters], ["a.py", "b.py"])
        self.assertEqual(clusters[0]["count"], 3)
        self.assertEqual(clusters[1]["count"], 2)

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


class TestSpiralEvidencePresent(unittest.TestCase):
    """The spiral auto-demote heuristic depends on this helper. Round 13 of
    pr-polish saw codex re-flag a finding on a line whose code was rewritten
    in round 12 — the resumed model was reading stale context, but the
    spiral guard couldn't tell. We grep ±10 lines around the cited line for
    distinctive tokens from the finding's message; absent → not a spiral.
    """

    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)

    def _write(self, rel: str, body: str) -> None:
        p = self.root / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(body)

    def test_returns_true_when_quoted_phrase_at_cited_line(self) -> None:
        self._write(
            "a.go",
            "\n".join([f"line {i}" for i in range(1, 30)] + ["return ctx.Err()"]),
        )
        finding = {
            "file": "a.go",
            "line": 30,
            "message": "still returns `ctx.Err()` instead of context-aware error",
            "suggestion": None,
        }
        self.assertTrue(bramble_ops._spiral_evidence_present(finding, head=self.root))

    def test_returns_false_when_token_absent(self) -> None:
        self._write(
            "a.go",
            "\n".join(
                [f"line {i}" for i in range(1, 30)]
                + ["return fmt.Errorf(\"waitForObject timed out\")"]
            ),
        )
        finding = {
            "file": "a.go",
            "line": 30,
            "message": "still returns `ctx.Err()` instead of a typed error",
            "suggestion": None,
        }
        self.assertFalse(bramble_ops._spiral_evidence_present(finding, head=self.root))

    def test_window_extends_above_and_below(self) -> None:
        self._write(
            "a.go",
            "\n".join([f"line {i}" for i in range(1, 50)] + ["return ctx.Err()"] + [f"line {i}" for i in range(51, 70)]),
        )
        # cited line is 60, evidence is at line 50 — within ±10.
        finding = {
            "file": "a.go",
            "line": 60,
            "message": "still returns `ctx.Err()`",
        }
        self.assertTrue(bramble_ops._spiral_evidence_present(finding, head=self.root))

    def test_token_outside_window_is_absent(self) -> None:
        self._write(
            "a.go",
            "\n".join(["return ctx.Err()"] + [f"line {i}" for i in range(2, 200)]),
        )
        finding = {
            "file": "a.go",
            "line": 150,  # window 140..160; evidence at line 1
            "message": "still returns `ctx.Err()`",
        }
        self.assertFalse(bramble_ops._spiral_evidence_present(finding, head=self.root))

    def test_unreadable_file_returns_true_conservatively(self) -> None:
        finding = {
            "file": "missing.go",
            "line": 1,
            "message": "still returns `ctx.Err()` instead of a typed error",
        }
        self.assertTrue(bramble_ops._spiral_evidence_present(finding, head=self.root))

    def test_no_extractable_tokens_returns_true(self) -> None:
        self._write("a.go", "return\n")
        finding = {
            "file": "a.go",
            "line": 1,
            "message": "this code is bad",  # all words below the 8-char floor
        }
        self.assertTrue(bramble_ops._spiral_evidence_present(finding, head=self.root))

    def test_no_address_returns_true(self) -> None:
        # PR-level / sourceless findings can't have evidence checked.
        self.assertTrue(
            bramble_ops._spiral_evidence_present(
                {"file": None, "line": None, "message": "topical foo"}, head=self.root
            )
        )

    def test_identifier_token_picks_up(self) -> None:
        # A bare identifier ≥ _SPIRAL_EVIDENCE_MIN_LEN chars must match
        # even when the message has no quotes/backticks.
        self._write(
            "h.py",
            "\n".join(
                [f"line {i}" for i in range(1, 10)]
                + ["def waitForObjectReady(client):"]
                + [f"line {i}" for i in range(11, 20)]
            ),
        )
        finding = {
            "file": "h.py",
            "line": 10,
            "message": "waitForObjectReady should respect timeout",
        }
        self.assertTrue(bramble_ops._spiral_evidence_present(finding, head=self.root))


class TestTriageSpiralAutoDemote(unittest.TestCase):
    """Single-source spirals whose cited evidence isn't at HEAD are auto-
    demoted to batch_stale; multi-source spirals always escalate; the
    behavior gates on resolved review_mode (design-doc spirals don't have
    addressable file/line, so the heuristic short-circuits to "escalate")."""

    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)

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

    def _write_file(self, rel: str, body: str) -> None:
        p = self.root / rel
        p.parent.mkdir(parents=True, exist_ok=True)
        p.write_text(body)

    def test_single_source_spiral_with_evidence_absent_demotes_to_stale(self) -> None:
        # File at HEAD has the *fix*, not the prior buggy code.
        self._write_file(
            "a.go",
            "\n".join(
                [f"line {i}" for i in range(1, 50)]
                + ["return fmt.Errorf(\"waitForObject timed out\")"]
                + [f"line {i}" for i in range(51, 100)]
            ),
        )
        # codex re-flags ctx.Err(), but it isn't at HEAD anymore.
        f = self._f("codex", "a.go", 50, "high", "still returns `ctx.Err()` instead of a typed error")
        prior = {(f["file"], f["line"], f["topic"]), (f["file"], f["line"], None)}
        out = bramble_ops.triage([f], prior_fixed_keys=prior, head_path=self.root)
        self.assertEqual(out["spiral_matches"], [])
        self.assertEqual(len(out["stale_prior_commit"]), 1)
        demoted = out["stale_prior_commit"][0]["finding"]
        self.assertIn("auto-demoted", demoted.get("stale_reason", ""))
        # The auto-demoted finding must not also surface in single_critical.
        self.assertEqual(out["single_critical"], [])
        # action_plan reflects the demote: nothing in escalate, the finding
        # in batch_stale.
        self.assertEqual(out["action_plan"]["escalate"], [])
        self.assertEqual(len(out["action_plan"]["batch_stale"]), 1)

    def test_single_source_spiral_with_evidence_present_still_escalates(self) -> None:
        # File at HEAD still has the cited code — keep the spiral.
        self._write_file(
            "a.go",
            "\n".join(
                [f"line {i}" for i in range(1, 50)]
                + ["return ctx.Err()"]
                + [f"line {i}" for i in range(51, 100)]
            ),
        )
        f = self._f("codex", "a.go", 50, "high", "still returns `ctx.Err()` instead of a typed error")
        prior = {(f["file"], f["line"], f["topic"])}
        out = bramble_ops.triage([f], prior_fixed_keys=prior, head_path=self.root)
        self.assertEqual(len(out["spiral_matches"]), 1)
        self.assertEqual(out["stale_prior_commit"], [])

    def test_multi_source_spiral_always_escalates_even_with_evidence_absent(self) -> None:
        # Even with a "real" file that doesn't have ctx.Err(), two
        # backends agreeing on the spiral is a stronger signal than the
        # heuristic. Don't demote.
        self._write_file("a.go", "return fmt.Errorf(\"timed out\")\n")
        f1 = self._f("codex", "a.go", 1, "high", "still returns `ctx.Err()`")
        f2 = self._f("cursor", "a.go", 1, "high", "still returns `ctx.Err()`")
        prior = {(f1["file"], f1["line"], None)}
        out = bramble_ops.triage([f1, f2], prior_fixed_keys=prior, head_path=self.root)
        self.assertEqual(len(out["spiral_matches"]), 1)
        self.assertEqual(out["stale_prior_commit"], [])
        # Multi-source spiral must NOT also land in batch_stale.
        self.assertEqual(out["action_plan"]["batch_stale"], [])
        self.assertEqual(len(out["action_plan"]["escalate"]), 1)

    def test_demoted_spiral_does_not_double_appear_in_severity_buckets(self) -> None:
        """An auto-demoted spiral lives only in batch_stale — never in
        single_critical/single_medium/low_acks (those would re-route it
        to a fix and undo the demote)."""
        self._write_file("a.go", "return fmt.Errorf(\"timed out\")\n")
        f = self._f("codex", "a.go", 1, "high", "still returns `ctx.Err()`")
        prior = {(f["file"], f["line"], None)}
        out = bramble_ops.triage([f], prior_fixed_keys=prior, head_path=self.root)
        # Not in any of the active severity buckets.
        self.assertEqual(out["single_critical"], [])
        self.assertEqual(out["single_medium"], [])
        self.assertEqual(out["low_acks"], [])
        # Only in batch_stale.
        self.assertEqual(len(out["stale_prior_commit"]), 1)


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

    def test_is_new_series_flag_returns_pr_summary(self) -> None:
        """--is-new-series 1 must short-circuit to PR_SUMMARY even when the
        state file has a converged prior round whose head_after is set."""
        with tempfile.TemporaryDirectory() as d:
            sf = Path(d) / "state.json"
            sf.write_text(
                json.dumps(
                    {
                        "completed": True,
                        "rounds": [
                            {
                                "n": 10,
                                "head_after": "deadbeef",
                                "comment_actions": [
                                    {"action": "fixed", "path": "a.go", "line": 5},
                                ],
                            }
                        ],
                    }
                )
            )
            out = self._run(
                "goal", "11",
                "--pr-summary", "PR #99: refactor",
                "--state-file", str(sf),
                "--head-before", "cafef00d",
                "--is-new-series", "1",
            )
        self.assertEqual(out, "PR #99: refactor")

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





class TestTriageDesignDocMode(unittest.TestCase):
    """Mode-aware consensus, spiral, and bucketing for design-doc envelopes.

    Code-mode triage is exercised by the existing TestTriage class above; we
    only need to assert that (a) design-doc mode keys on the right
    addressing fields, (b) cross-source consensus collapses on
    ``(section, dimension)``, and (c) the spiral guard reads
    ``(section, dimension)`` from prior comment_actions rows.
    """

    def _finding(self, *, section, dimension, severity, source, message):
        # Mirror what parse_envelope produces for ReviewModeDesignDoc:
        # no file/line, section+dimension, review_mode tag.
        return {
            "source": source,
            "severity": severity,
            "message": message,
            "suggestion": None,
            "topic": _common.topic_of(message),
            "review_mode": "design-doc",
            "section": section,
            "dimension": dimension,
        }

    def test_consensus_keys_on_section_dimension(self):
        # Two backends flag the same section+dimension; consensus must
        # collapse them into one must_fix entry. If the triage layer
        # had defaulted to (file, line) keying, both would route to
        # single_critical because (None, None) doesn't form an address.
        findings = [
            self._finding(
                section="Milestone 2",
                dimension="q4",
                severity="high",
                source="codex",
                message="doesn't frontload risk",
            ),
            self._finding(
                section="Milestone 2",
                dimension="q4",
                severity="high",
                source="cursor",
                message="risk is back-loaded into the final milestone",
            ),
        ]
        result = bramble_ops.triage(findings, set(), mode="design-doc")
        self.assertEqual(result["review_mode"], "design-doc")
        self.assertEqual(len(result["consensus"]), 1, result)
        self.assertEqual(
            tuple(result["consensus"][0]["key"]),
            ("Milestone 2", "q4"),
        )
        self.assertEqual(
            sorted(result["consensus"][0]["sources"]),
            ["codex", "cursor"],
        )
        # Routed to must_fix (consensus + high severity).
        self.assertEqual(len(result["action_plan"]["must_fix"]), 1)

    def test_different_dimensions_at_same_section_do_not_consensus(self):
        # The dimension component of the consensus key partitions
        # systemic issues by rubric question. Two findings that share
        # a section but differ on dimension are distinct issues, NOT
        # consensus; both should land in single_critical.
        findings = [
            self._finding(
                section="Intro",
                dimension="q1",
                severity="high",
                source="codex",
                message="long-term fit unclear",
            ),
            self._finding(
                section="Intro",
                dimension="q2",
                severity="high",
                source="cursor",
                message="could be simpler",
            ),
        ]
        result = bramble_ops.triage(findings, set(), mode="design-doc")
        self.assertEqual(len(result["consensus"]), 0)
        self.assertEqual(len(result["single_critical"]), 2)

    def test_spiral_match_reads_section_dimension_from_prior(self):
        # Construct a state with one prior-round fixed action against
        # (section=Milestone 2, dimension=q4). A new finding at the
        # same key must trigger the spiral guard and land in
        # action_plan.escalate, NOT must_fix.
        prior_state = {
            "rounds": [
                {
                    "n": 1,
                    "comment_actions": [
                        {
                            "source": "codex",
                            "action": "fixed",
                            "section": "Milestone 2",
                            "dimension": "q4",
                            "topic": "risk frontloading",
                        }
                    ],
                }
            ]
        }
        keys = bramble_ops.prior_fixed_keys(prior_state, "design-doc")
        # Strict + location-only fallback both present.
        self.assertIn(("Milestone 2", "q4", "risk frontloading"), keys)
        self.assertIn(("Milestone 2", "q4", None), keys)

        new = [
            self._finding(
                section="Milestone 2",
                dimension="q4",
                severity="high",
                source="codex",
                message="this still doesn't frontload risk",
            )
        ]
        result = bramble_ops.triage(new, keys, mode="design-doc")
        self.assertEqual(len(result["spiral_matches"]), 1)
        self.assertEqual(len(result["action_plan"]["escalate"]), 1)
        self.assertEqual(len(result["action_plan"]["must_fix"]), 0)

    def test_cluster_hint_buckets_by_section(self):
        # cluster_hint must group by section (not file) in design-doc
        # mode. Two findings at "Milestone 2" but different dimensions
        # both count toward the bucket — the sweep candidate is the
        # section the model keeps revisiting, not the rubric question.
        findings = [
            self._finding(
                section="Milestone 2",
                dimension="q1",
                severity="medium",
                source="codex",
                message="long-term fit shaky",
            ),
            self._finding(
                section="Milestone 2",
                dimension="q4",
                severity="medium",
                source="cursor",
                message="risk back-loaded",
            ),
            self._finding(
                section="Intro",
                dimension="q2",
                severity="medium",
                source="codex",
                message="simpler shape exists",
            ),
        ]
        result = bramble_ops.triage(findings, set(), mode="design-doc")
        clusters = result["action_plan"]["cluster_hint"]
        # Only Milestone 2 has 2+ findings; Intro is single and dropped.
        self.assertEqual(len(clusters), 1)
        self.assertEqual(clusters[0]["section"], "Milestone 2")
        self.assertEqual(clusters[0]["count"], 2)
        # In design-doc mode "lines" actually carries the dimensions
        # for the bucket; populated with the rubric-question signals.
        self.assertEqual(sorted(clusters[0]["lines"]), ["q1", "q4"])

    def test_pr_comments_rejected_in_design_doc_mode(self):
        # Design-doc reviews don't have PR comments — the only way they
        # arrive is a misrouted call. The triage layer must reject it
        # rather than silently key bramble's design-doc findings against
        # PR-comment code-shaped findings.
        with self.assertRaises(ValueError) as ctx:
            bramble_ops.triage(
                [],
                set(),
                pr_comments=[{"path": "x.go", "line": 1, "body": "x"}],
                mode="design-doc",
            )
        self.assertIn("design-doc mode", str(ctx.exception))

    def test_explicit_mode_must_match_envelope_mode(self):
        # If the operator passes mode=code but the findings carry
        # review_mode=design-doc, fail loud. Silent dispatch on the
        # wrong mode would key on (file=None, line=None) and produce
        # nonsense consensus.
        finding = self._finding(
            section="Intro", dimension="q1", severity="high",
            source="codex", message="x",
        )
        with self.assertRaises(ValueError) as ctx:
            bramble_ops.triage([finding], set(), mode="code")
        self.assertIn("doesn't match", str(ctx.exception))

    def test_mixed_envelope_modes_rejected(self):
        # Findings from a code-mode envelope and a design-doc-mode
        # envelope must not be triaged together. A defensive ValueError
        # surfaces the misroute; auto-defaulting to one mode would
        # silently corrupt the other half.
        code_finding = {
            "source": "cursor",
            "severity": "high",
            "file": "x.go",
            "line": 1,
            "message": "x",
            "topic": "x",
            "review_mode": "code",
        }
        doc_finding = self._finding(
            section="Intro", dimension="q1", severity="high",
            source="codex", message="y",
        )
        with self.assertRaises(ValueError) as ctx:
            bramble_ops.triage([code_finding, doc_finding], set())
        self.assertIn("mixed review_mode", str(ctx.exception))

    def test_design_doc_mode_default_via_findings(self):
        # When mode is omitted, the resolved mode comes from the
        # findings' review_mode tag. A list of design-doc-tagged
        # findings with no explicit mode argument must triage as
        # design-doc.
        finding = self._finding(
            section="Intro", dimension="q1", severity="high",
            source="codex", message="x",
        )
        result = bramble_ops.triage([finding], set())
        self.assertEqual(result["review_mode"], "design-doc")
        self.assertEqual(len(result["single_critical"]), 1)


class TestParseEnvelopeReviewMode(unittest.TestCase):
    """parse_envelope must propagate review_mode and emit the right
    addressing fields per mode."""

    def test_design_doc_envelope_emits_section_dimension(self):
        env = {
            "schema_version": 1,
            "status": "ok",
            "review_mode": "design-doc",
            "review": {
                "verdict": "revise",
                "issues": [
                    {
                        "severity": "high",
                        "section": "Milestone 2",
                        "dimension": "q4",
                        "message": "doesn't frontload risk",
                        "suggestion": "swap M1 and M2",
                    }
                ],
            },
        }
        findings = bramble_ops.parse_envelope(env, source="codex")
        self.assertEqual(len(findings), 1)
        f = findings[0]
        self.assertEqual(f["review_mode"], "design-doc")
        self.assertEqual(f["section"], "Milestone 2")
        self.assertEqual(f["dimension"], "q4")
        self.assertNotIn("file", f)
        self.assertNotIn("line", f)

    def test_code_envelope_unchanged(self):
        # Backward-compat: a pre-mode envelope (no review_mode field)
        # must produce code-mode findings byte-for-byte equivalent to
        # the prior behaviour.
        env = {
            "schema_version": 1,
            "status": "ok",
            "review": {
                "verdict": "rejected",
                "issues": [
                    {
                        "severity": "high",
                        "file": "x.go",
                        "line": 42,
                        "message": "bug",
                    }
                ],
            },
        }
        findings = bramble_ops.parse_envelope(env, source="codex")
        self.assertEqual(len(findings), 1)
        f = findings[0]
        self.assertEqual(f["review_mode"], "code")
        self.assertEqual(f["file"], "x.go")
        self.assertEqual(f["line"], 42)

    def test_design_doc_error_envelope_carries_mode_and_addressless_fields(self):
        # Error envelope in design-doc mode must surface as a high
        # finding with addressless section/dimension (so the consensus
        # key is (None, None) and triage routes through the
        # triage_key pipeline rather than the location-based pass).
        env = {
            "schema_version": 1,
            "status": "error",
            "error": "rubric file unreadable",
            "review_mode": "design-doc",
        }
        findings = bramble_ops.parse_envelope(env, source="codex")
        self.assertEqual(len(findings), 1)
        f = findings[0]
        self.assertEqual(f["severity"], "high")
        self.assertEqual(f["review_mode"], "design-doc")
        self.assertIsNone(f.get("section"))
        self.assertIsNone(f.get("dimension"))
        self.assertNotIn("file", f)


class TestSyntheticFindingFallbackMode(unittest.TestCase):
    """parse_stream's empty-envelope fallback and parse_round's
    stream-missing fallback must adopt the caller's fallback_mode so a
    crashed design-doc backend produces a design-doc-tagged synthetic
    finding (otherwise triage rejects the batch as 'mixed review_mode')."""

    def test_parse_stream_empty_envelope_uses_fallback_mode(self):
        with tempfile.TemporaryDirectory() as d:
            empty = Path(d) / "empty.txt"
            empty.write_text("[Status] Running review with codex (model: x)...\n")
            findings = bramble_ops.parse_stream(
                empty, source="codex", fallback_mode="design-doc"
            )
        self.assertEqual(len(findings), 1)
        f = findings[0]
        self.assertEqual(f["review_mode"], "design-doc")
        self.assertEqual(f["topic"], "bramble-empty-envelope")
        # Design-doc shape: section/dimension, no file/line.
        self.assertIsNone(f["section"])
        self.assertIsNone(f["dimension"])
        self.assertNotIn("file", f)
        self.assertNotIn("line", f)

    def test_parse_stream_default_fallback_is_code(self):
        # Backward-compat: callers that don't pass fallback_mode still
        # get code-mode synthetics (same shape as before this change).
        with tempfile.TemporaryDirectory() as d:
            empty = Path(d) / "empty.txt"
            empty.write_text("garbage no envelope\n")
            findings = bramble_ops.parse_stream(empty, source="codex")
        self.assertEqual(findings[0]["review_mode"], "code")
        self.assertIsNone(findings[0]["file"])
        self.assertIsNone(findings[0]["line"])

    def test_parse_round_missing_stream_uses_fallback_mode(self):
        findings = bramble_ops.parse_round(
            {"codex": Path("/nonexistent/path.json")},
            backends=["codex"],
            fallback_mode="design-doc",
        )
        self.assertEqual(len(findings), 1)
        f = findings[0]
        self.assertEqual(f["review_mode"], "design-doc")
        self.assertEqual(f["topic"], "stream-missing")
        self.assertIsNone(f["section"])
        self.assertIsNone(f["dimension"])
        self.assertNotIn("file", f)

    def test_triage_cli_explicit_mode_overrides_all_synthetic_fallback(self):
        # When EVERY backend fails before producing an envelope, the
        # auto-detect path has no real envelopes to inspect and would
        # default to code mode — contradicting a design-doc orchestrator's
        # contract. The /design-doc-polish skill works around this by
        # always passing --mode design-doc; this test pins that the
        # explicit mode survives even when the only findings are
        # synthetic stream-missing rows.
        with tempfile.TemporaryDirectory() as d:
            d = Path(d)
            missing_a = d / "missing-a.json"  # never written
            missing_b = d / "missing-b.json"

            import contextlib
            import io

            buf = io.StringIO()
            with contextlib.redirect_stdout(buf):
                rc = bramble_ops.main([
                    "triage",
                    "--mode", "design-doc",
                    "--stream", f"codex={missing_a}",
                    "--stream", f"cursor={missing_b}",
                ])
            self.assertEqual(rc, 0)
            result = json.loads(buf.getvalue())
        # Mode honours the explicit flag even though no real envelope
        # was available to vote on it. The two synthetic findings share
        # the same (None, None, "stream-missing") triage key so they
        # collapse into one consensus entry routed to must_fix.
        self.assertEqual(result["review_mode"], "design-doc")
        self.assertEqual(len(result["consensus"]), 1)
        consensus_entry = result["consensus"][0]
        self.assertEqual(sorted(consensus_entry["sources"]), ["codex", "cursor"])
        for f in consensus_entry["findings"]:
            self.assertEqual(f["review_mode"], "design-doc")
            self.assertEqual(f["topic"], "stream-missing")
        self.assertEqual(len(result["action_plan"]["must_fix"]), 1)

    def test_triage_cli_auto_detects_design_doc_from_real_envelopes(self):
        # End-to-end: a design-doc envelope from cursor + a missing
        # codex stream must triage cleanly when --mode is omitted. The
        # triage CLI does an initial parse to detect the mode from the
        # real envelopes (skipping synthetic stream-missing findings),
        # then re-parses with the resolved mode. Without this, the
        # synthetic codex finding would default to code-mode and triage
        # would reject the batch as mixed review_mode.
        with tempfile.TemporaryDirectory() as d:
            d = Path(d)
            cursor_env = d / "cursor.json"
            cursor_env.write_text(json.dumps({
                "schema_version": 1,
                "status": "ok",
                "review_mode": "design-doc",
                "review": {
                    "verdict": "revise",
                    "confidence": 0.7,
                    "issues": [{
                        "severity": "high",
                        "section": "Milestone 2",
                        "dimension": "q4",
                        "message": "doesn't frontload risk",
                    }],
                },
            }))
            # codex envelope path that doesn't exist on disk —
            # parse_round emits a stream-missing synthetic.
            codex_env = d / "codex.json"

            # Drive the CLI in-process via main(argv) so we exercise
            # the same code path the orchestrator hits.
            import contextlib
            import io

            buf = io.StringIO()
            with contextlib.redirect_stdout(buf):
                rc = bramble_ops.main([
                    "triage",
                    "--stream", f"cursor={cursor_env}",
                    "--stream", f"codex={codex_env}",
                ])
            self.assertEqual(rc, 0)
            result = json.loads(buf.getvalue())
        # Mode auto-detected as design-doc; the synthetic codex
        # stream-missing finding is tagged design-doc and routes to
        # single_critical (it has severity=high). The cursor finding
        # also routes to single_critical at (Milestone 2, q4, ...).
        self.assertEqual(result["review_mode"], "design-doc")
        self.assertGreaterEqual(len(result["single_critical"]), 1)


class TestActionLabelsModeAware(unittest.TestCase):
    """_action_label and _skipped_label format addresses for both code-
    mode (path:line) and design-doc-mode (section (dimension)) actions.
    Without mode-awareness, design-doc round-2+ goal text would be
    truncated to topic-only (no address) — losing the load-bearing
    'fixed at the same place last round' signal that prevents the
    resumed model from re-flagging its own prior fixes."""

    def test_action_label_code_mode(self):
        action = {
            "path": "x.go", "line": 42, "topic": "missing nil check",
        }
        got = bramble_ops._action_label(action)
        self.assertIn("x.go:42", got)
        self.assertIn("missing nil check", got)

    def test_action_label_design_doc_mode(self):
        action = {
            "section": "Milestone 2",
            "dimension": "q4",
            "topic": "milestone 2 doesn't frontload risk",
        }
        got = bramble_ops._action_label(action)
        self.assertIn("Milestone 2", got)
        self.assertIn("q4", got)
        self.assertIn("doesn't frontload risk", got)

    def test_action_label_returns_empty_when_no_address(self):
        # Top-level / addressless actions still produce no label so
        # the goal-text builder can elide them cleanly.
        self.assertEqual(bramble_ops._action_label({"topic": "x"}), "")

    def test_skipped_label_design_doc_mode(self):
        action = {
            "section": "Intro",
            "dimension": "q1",
            "reason": "design tradeoff: one-pager not arch doc",
        }
        got = bramble_ops._skipped_label(action, "wont_fix")
        self.assertIn("Intro", got)
        self.assertIn("q1", got)
        self.assertIn("wont_fix", got)
        self.assertIn("design tradeoff", got)

    def test_skipped_label_section_only_no_dimension(self):
        # Sections alone (no dimension) still produce a usable label.
        action = {
            "section": "(whole document)",
            "reason": "needs author input",
        }
        got = bramble_ops._skipped_label(action, "wont_fix")
        self.assertIn("(whole document)", got)
        self.assertIn("needs author input", got)


class TestRecoverEnvelope(unittest.TestCase):
    """Mechanical recovery for the ``approve_with_notes`` family. Cursor
    occasionally returns verdicts the wrapper doesn't recognize; if the
    inner ``review.issues`` is populated, salvage the envelope so the
    round's actual signal isn't lost.
    """

    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)

    def _write(self, name: str, obj: dict) -> Path:
        p = self.root / name
        p.write_text(json.dumps(obj))
        return p

    def test_approve_with_notes_remaps_to_accepted(self) -> None:
        env = {
            "schema_version": 1,
            "status": "error",
            "error": "unrecognized verdict 'approve_with_notes'",
            "review": {
                "issues": [
                    {"severity": "low", "file": "a.go", "line": 3,
                     "message": "trailing whitespace"},
                ],
            },
        }
        path = self._write("cursor.json", env)
        out = bramble_ops.recover_envelope(path)
        self.assertNotEqual(out, path, "expected a recovered sibling path")
        recovered = json.loads(out.read_text())
        self.assertEqual(recovered["status"], "ok")
        self.assertEqual(recovered["review"]["verdict"], "accepted")
        self.assertEqual(len(recovered["review"]["issues"]), 1)

    def test_request_changes_remaps_to_rejected(self) -> None:
        env = {
            "schema_version": 1,
            "status": "error",
            "error": "verdict 'request_changes' not in allowed set",
            "review": {
                "issues": [{"severity": "high", "file": "x.go", "line": 1,
                            "message": "missing nil check"}],
            },
        }
        path = self._write("cursor.json", env)
        out = bramble_ops.recover_envelope(path)
        recovered = json.loads(out.read_text())
        self.assertEqual(recovered["status"], "ok")
        self.assertEqual(recovered["review"]["verdict"], "rejected")

    def test_status_ok_envelope_returned_unchanged(self) -> None:
        env = {"schema_version": 1, "status": "ok", "review": {"issues": []}}
        path = self._write("codex.json", env)
        out = bramble_ops.recover_envelope(path)
        self.assertEqual(out, path)

    def test_error_envelope_with_no_verdict_returns_unchanged(self) -> None:
        env = {
            "schema_version": 1,
            "status": "error",
            "error": "bramble timed out after 10m",
            "review": {"issues": []},
        }
        path = self._write("codex.json", env)
        out = bramble_ops.recover_envelope(path)
        self.assertEqual(out, path)

    def test_verdict_error_with_empty_issues_returns_unchanged(self) -> None:
        # Vocabulary problem + no issues = nothing to salvage.
        env = {
            "schema_version": 1,
            "status": "error",
            "error": "unrecognized verdict 'approve_with_notes'",
            "review": {"issues": []},
        }
        path = self._write("cursor.json", env)
        out = bramble_ops.recover_envelope(path)
        self.assertEqual(out, path)

    def test_idempotent_on_recovered_envelope(self) -> None:
        env = {
            "schema_version": 1,
            "status": "error",
            "error": "unrecognized verdict 'approve_with_notes'",
            "review": {"issues": [{"severity": "low", "file": "a", "line": 1,
                                   "message": "x"}]},
        }
        path = self._write("cursor.json", env)
        first = bramble_ops.recover_envelope(path)
        # second pass on the recovered file (status ok) should no-op.
        second = bramble_ops.recover_envelope(first)
        self.assertEqual(first, second)

    def test_missing_file_returned_unchanged(self) -> None:
        path = self.root / "does-not-exist.json"
        out = bramble_ops.recover_envelope(path)
        self.assertEqual(out, path)


class TestRoundDiff(unittest.TestCase):
    """`round_diff` shells out to ``git diff prior_head_after..head_before``
    and truncates. Test with a real tmp git repo so the helper exercises
    the same plumbing the orchestrator runs through.
    """

    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self._git("init", "-q")
        self._git("config", "user.email", "t@t")
        self._git("config", "user.name", "t")
        self._git("config", "commit.gpgsign", "false")
        # Run the helper as if the orchestrator launched it inside the
        # tmp repo. ``_common.run`` honours `cwd`, so we just cd. Saves
        # us monkey-patching subprocess plumbing per call.
        self._old_cwd = os.getcwd()
        os.chdir(self.root)
        self.addCleanup(os.chdir, self._old_cwd)

    def _git(self, *args: str) -> str:
        import subprocess

        res = subprocess.run(
            ["git", *args], cwd=str(self.root),
            capture_output=True, text=True, check=False,
        )
        if res.returncode != 0:
            raise RuntimeError(f"git {args}: {res.stderr}")
        return res.stdout

    def _commit(self, name: str, body: str) -> str:
        (self.root / name).write_text(body)
        self._git("add", name)
        self._git("commit", "-q", "-m", f"add {name}")
        return self._git("rev-parse", "HEAD").strip()

    def test_returns_diff_between_prior_head_after_and_head_before(self) -> None:
        sha1 = self._commit("a.txt", "hello\n")
        sha2 = self._commit("a.txt", "hello\nworld\n")
        state = {"rounds": [{"n": 1, "head_before": "x", "head_after": sha1}]}
        text = bramble_ops.round_diff(state, 2, head_before=sha2)
        self.assertIn("+world", text)
        self.assertIn("a.txt", text)

    def test_empty_when_round_one(self) -> None:
        self.assertEqual(bramble_ops.round_diff({}, 1, head_before="abc"), "")

    def test_empty_when_no_prior_round(self) -> None:
        self.assertEqual(bramble_ops.round_diff({"rounds": []}, 2, head_before="abc"), "")

    def test_empty_when_prior_round_never_finalized(self) -> None:
        state = {"rounds": [{"n": 1, "head_before": "x", "head_after": None}]}
        self.assertEqual(bramble_ops.round_diff(state, 2, head_before="abc"), "")

    def test_empty_when_head_before_missing(self) -> None:
        state = {"rounds": [{"n": 1, "head_before": "x", "head_after": "y"}]}
        self.assertEqual(bramble_ops.round_diff(state, 2, head_before=None), "")

    def test_empty_when_shas_equal(self) -> None:
        state = {"rounds": [{"n": 1, "head_before": "x", "head_after": "abc"}]}
        self.assertEqual(bramble_ops.round_diff(state, 2, head_before="abc"), "")

    def test_truncates_long_diffs_with_elision_footer(self) -> None:
        sha1 = self._commit("a.txt", "x\n")
        # 50 lines added to a.txt
        big = "\n".join(f"line {i}" for i in range(50)) + "\n"
        (self.root / "a.txt").write_text(big)
        self._git("add", "a.txt")
        self._git("commit", "-q", "-m", "big")
        sha2 = self._git("rev-parse", "HEAD").strip()
        state = {"rounds": [{"n": 1, "head_after": sha1}]}
        text = bramble_ops.round_diff(state, 2, head_before=sha2, max_lines=10)
        self.assertIn("...elided", text)
        # Exactly 11 lines: 10 + footer
        self.assertEqual(len(text.splitlines()), 11)

    def test_unreachable_sha_returns_empty(self) -> None:
        state = {
            "rounds": [{"n": 1, "head_after": "0" * 40}],
        }
        text = bramble_ops.round_diff(state, 2, head_before="1" * 40)
        self.assertEqual(text, "")


class TestPriorRoundModifiedHunks(unittest.TestCase):
    """E1: spiral demote when cited line falls inside a hunk a prior round
    modified. Validates hunk-header parsing and the boundary semantics.
    """

    def test_parse_hunk_ranges_basic(self) -> None:
        diff = """@@ -10,3 +12,5 @@
 ctx
-old
+new1
+new2
@@ -100 +150,2 @@
-old
+new
+new
"""
        ranges = bramble_ops._parse_hunk_ranges(diff)
        self.assertEqual(ranges, [(12, 16), (150, 151)])

    def test_parse_hunk_default_count_one(self) -> None:
        diff = "@@ -1 +5 @@\n+x\n"
        self.assertEqual(bramble_ops._parse_hunk_ranges(diff), [(5, 5)])

    def test_parse_hunk_pure_deletion_skipped(self) -> None:
        # +c,0 — no lines on the + side
        diff = "@@ -10,3 +10,0 @@\n-a\n-b\n-c\n"
        self.assertEqual(bramble_ops._parse_hunk_ranges(diff), [])

    def test_line_in_modified_hunk(self) -> None:
        hunks = {"a.go": [(10, 15), (50, 55)]}
        # Inside both ranges
        self.assertTrue(bramble_ops._line_in_modified_hunk(
            {"file": "a.go", "line": 10}, hunks))
        self.assertTrue(bramble_ops._line_in_modified_hunk(
            {"file": "a.go", "line": 15}, hunks))
        self.assertTrue(bramble_ops._line_in_modified_hunk(
            {"file": "a.go", "line": 52}, hunks))
        # Outside
        self.assertFalse(bramble_ops._line_in_modified_hunk(
            {"file": "a.go", "line": 9}, hunks))
        self.assertFalse(bramble_ops._line_in_modified_hunk(
            {"file": "a.go", "line": 100}, hunks))
        # Different file
        self.assertFalse(bramble_ops._line_in_modified_hunk(
            {"file": "b.go", "line": 12}, hunks))
        # No address
        self.assertFalse(bramble_ops._line_in_modified_hunk(
            {"file": None, "line": None}, hunks))


class TestTriageSpiralModifiedHunkDemote(unittest.TestCase):
    """When a single-source spiral lands on a line a prior round
    modified, demote to batch_stale even when the evidence-at-HEAD
    heuristic would have kept it. Multi-source spirals still escalate.
    """

    def test_single_source_in_modified_hunk_demoted(self) -> None:
        # Codex re-flagging a finding at a.go:42; prior round modified
        # lines 40-50 of a.go.
        finding = {
            "source": "codex",
            "severity": "high",
            "file": "a.go",
            "line": 42,
            "message": "still has `ctx.Err()` here",
            "topic": "ctx-err",
            "review_mode": "code",
        }
        prior_keys = {("a.go", 42, "ctx-err")}
        modified_hunks = {"a.go": [(40, 50)]}
        out = bramble_ops.triage(
            [finding],
            prior_keys,
            prior_modified_hunks=modified_hunks,
        )
        self.assertEqual(len(out["spiral_matches"]), 0)
        self.assertEqual(len(out["stale_prior_commit"]), 1)
        stale = out["stale_prior_commit"][0]["finding"]
        self.assertIn("modified by a prior round", stale["stale_reason"])

    def test_single_source_outside_modified_hunk_still_escalates(self) -> None:
        finding = {
            "source": "codex",
            "severity": "high",
            "file": "a.go",
            "line": 5,
            "message": "still has `ctx.Err()` here",
            "topic": "ctx-err",
            "review_mode": "code",
        }
        prior_keys = {("a.go", 5, "ctx-err")}
        # The cited line is outside the prior round's modified hunks; with
        # head_path unset the evidence-at-HEAD check returns conservative-
        # True (file unreadable), so spiral escalates.
        modified_hunks = {"a.go": [(40, 50)]}
        out = bramble_ops.triage(
            [finding],
            prior_keys,
            prior_modified_hunks=modified_hunks,
        )
        self.assertEqual(len(out["spiral_matches"]), 1)
        self.assertEqual(len(out["stale_prior_commit"]), 0)

    def test_multi_source_in_modified_hunk_still_escalates(self) -> None:
        # Two backends agreeing on a regression overrides the
        # modified-hunk demote — that's a stronger signal than file plumbing.
        f_codex = {
            "source": "codex",
            "severity": "high",
            "file": "a.go",
            "line": 42,
            "message": "still has ctxErrSentinel here",
            "topic": "ctx-err",
            "review_mode": "code",
        }
        f_cursor = {
            "source": "cursor",
            "severity": "high",
            "file": "a.go",
            "line": 42,
            "message": "ctxErrSentinel still returned",
            "topic": "ctx-err",
            "review_mode": "code",
        }
        prior_keys = {("a.go", 42, "ctx-err")}
        modified_hunks = {"a.go": [(40, 50)]}
        out = bramble_ops.triage(
            [f_codex, f_cursor],
            prior_keys,
            prior_modified_hunks=modified_hunks,
        )
        # Multi-source spirals escalate regardless of modified-hunk
        # signal. (They also collapse under consensus, which is fine —
        # what matters is that the spiral isn't silently demoted.)
        self.assertEqual(len(out["stale_prior_commit"]), 0)
        self.assertEqual(len(out["spiral_matches"]), 1)


class TestSessionResetEveryKRounds(unittest.TestCase):
    """E2: forcing a fresh bramble session every K rounds clears stale
    accumulated context. The reset gate triggers on round-distance, so
    an interrupted-and-resumed loop still gets the same cadence.
    """

    def _state_with_session_at(self, rounds: list[tuple[int, str | None]]) -> dict:
        return {
            "completed": False,
            "current_round": rounds[-1][0],
            "rounds": [
                {
                    "n": n,
                    "session_ids": ({"codex": sid} if sid else {}),
                }
                for n, sid in rounds
            ],
        }

    def test_returns_empty_when_session_k_rounds_back(self) -> None:
        # Last codex session id was at round 1; current is round 5; K=4.
        # 5 - 1 = 4 >= 4 -> reset.
        state = self._state_with_session_at([(1, "sid-1"), (2, None), (3, None), (4, None)])
        out = bramble_ops.prior_session_id(
            state, "codex", 5, is_new_series=False, session_reset_k=4
        )
        self.assertEqual(out, "")

    def test_returns_id_when_session_within_window(self) -> None:
        # Last id at round 2; current 4; 4-2 = 2 < 4 -> keep.
        state = self._state_with_session_at([(1, None), (2, "sid-2"), (3, None)])
        out = bramble_ops.prior_session_id(
            state, "codex", 4, is_new_series=False, session_reset_k=4
        )
        self.assertEqual(out, "sid-2")

    def test_zero_disables_reset(self) -> None:
        state = self._state_with_session_at([(1, "sid-1")])
        out = bramble_ops.prior_session_id(
            state, "codex", 99, is_new_series=False, session_reset_k=0
        )
        self.assertEqual(out, "sid-1")

    def test_default_k_is_four(self) -> None:
        # SESSION_RESET_K_DEFAULT = 4; ensure round 5 with id at round 1 resets.
        state = self._state_with_session_at([(1, "sid-1"), (2, None), (3, None), (4, None)])
        out = bramble_ops.prior_session_id(state, "codex", 5, is_new_series=False)
        self.assertEqual(out, "")


class TestGoalLowStreakSentence(unittest.TestCase):
    """B1: when the prior round's low_only_streak >= 2, append a single
    sentence to the goal text stating the fact and the cost frame. The
    sentence is appended; it does not replace the action-history
    briefing.
    """

    def _state(self, streak: int) -> dict:
        return {
            "rounds": [
                {
                    "n": 1,
                    "head_before": "a",
                    "head_after": "b",
                    "low_only_streak": streak,
                    "comment_actions": [
                        {"action": "fixed", "path": "a.go", "line": 5,
                         "topic": "stub fix"},
                    ],
                }
            ]
        }

    def test_streak_two_appends_sentence(self) -> None:
        state = self._state(2)
        out = bramble_ops.goal_for_round(
            2, "PR_SUMMARY", state, head_before="b", include_round_diff=False
        )
        self.assertIn("last 2 rounds returned only low-severity findings", out)
        self.assertIn("returning zero findings is the right call", out)
        # action-history still present
        self.assertIn("Round 2.", out)

    def test_streak_one_no_sentence(self) -> None:
        state = self._state(1)
        out = bramble_ops.goal_for_round(
            2, "PR_SUMMARY", state, head_before="b", include_round_diff=False
        )
        self.assertNotIn("low-severity findings", out)

    def test_streak_three_uses_actual_count(self) -> None:
        state = self._state(3)
        out = bramble_ops.goal_for_round(
            2, "PR_SUMMARY", state, head_before="b", include_round_diff=False
        )
        self.assertIn("last 3 rounds", out)

    def test_is_new_series_skips_sentence(self) -> None:
        state = self._state(2)
        out = bramble_ops.goal_for_round(
            2, "PR_SUMMARY", state, head_before="b",
            is_new_series=True, include_round_diff=False,
        )
        self.assertEqual(out, "PR_SUMMARY")
        self.assertNotIn("low-severity findings", out)

    def test_round_one_skips_sentence(self) -> None:
        # Round 1 always returns PR_SUMMARY; no streak logic applies.
        state = self._state(2)
        out = bramble_ops.goal_for_round(
            1, "PR_SUMMARY", state, head_before="b", include_round_diff=False
        )
        self.assertEqual(out, "PR_SUMMARY")


if __name__ == "__main__":
    unittest.main()
