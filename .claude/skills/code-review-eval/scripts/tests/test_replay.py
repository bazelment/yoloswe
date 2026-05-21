"""Unit tests for the code-review replay scorer (replay_lib + replay)."""

from __future__ import annotations

import json
import sys
import tempfile
import unittest
from pathlib import Path

TEST_DIR = Path(__file__).resolve().parent
SCRIPT_DIR = TEST_DIR.parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

import replay  # noqa: E402
import replay_lib as rl  # noqa: E402


# A realistic klogfmt run-log fragment (cursor backend logs every tool call).
SAMPLE_RUNLOG = """\
I0507 01:11:19.931402  228289 codereview.go:134] code-review run start run_tag=code-review-replay:kernel-3834:r1:cursor-composer2 pid=228289 cwd=/x backend=cursor model=composer-2 timeout=10m0s
I0507 01:11:21.446877  228289 backend.go:180] reviewer session started run_tag=code-review-replay:kernel-3834:r1:cursor-composer2 session_id=abc model="Composer 2 Fast"
D0507 01:11:40.329692  228289 backend.go:193] tool call start run_tag=code-review-replay:kernel-3834:r1:cursor-composer2 tool="read .../scripts/deploy.py" call_id=tool_001 input_summary=path=x
D0507 01:11:40.427429  228289 backend.go:209] tool call end run_tag=code-review-replay:kernel-3834:r1:cursor-composer2 tool=readToolCall call_id=tool_001 is_error=false result_len=0
D0507 01:11:41.000000  228289 backend.go:193] tool call start run_tag=code-review-replay:kernel-3834:r1:cursor-composer2 tool="grep deploy_rollout" call_id=tool_002 input_summary=pattern=x
D0507 01:11:41.500000  228289 backend.go:209] tool call end run_tag=code-review-replay:kernel-3834:r1:cursor-composer2 tool=grepToolCall call_id=tool_002 is_error=true result_len=0
I0507 01:12:45.717080  228289 backend.go:218] reviewer turn complete run_tag=code-review-replay:kernel-3834:r1:cursor-composer2 success=true duration_ms=84271
I0507 01:12:45.717234  228289 codereview.go:198] code-review run exit run_tag=code-review-replay:kernel-3834:r1:cursor-composer2 status=ok verdict=rejected issue_count=1 max_severity=high total_duration_ms=85785
"""


class ParseRunlogTests(unittest.TestCase):
    def test_parses_metadata_and_calls(self):
        tr = rl.parse_runlog(SAMPLE_RUNLOG)
        self.assertTrue(tr.parsed)
        self.assertEqual(tr.backend, "cursor")
        self.assertEqual(tr.model, "composer-2")
        self.assertTrue(tr.session_started)
        self.assertEqual(tr.total_duration_ms, 85785)
        self.assertEqual(tr.n_tool_calls, 2)
        self.assertEqual(tr.tool_kind_counts, {"read": 1, "grep": 1})

    def test_tool_error_and_durations(self):
        tr = rl.parse_runlog(SAMPLE_RUNLOG)
        self.assertEqual(tr.n_tool_errors, 1)
        read_call = tr.tool_calls[0]
        self.assertEqual(read_call.kind, "read")
        self.assertEqual(read_call.target, "scripts/deploy.py")
        self.assertFalse(read_call.is_error)
        # 01:11:40.427 - 01:11:40.329 = 98ms
        self.assertEqual(read_call.duration_ms, 98)
        grep_call = tr.tool_calls[1]
        self.assertTrue(grep_call.is_error)

    def test_first_tool_latency(self):
        tr = rl.parse_runlog(SAMPLE_RUNLOG)
        # session start 01:11:21.446 -> first tool 01:11:40.329 ≈ 18883ms
        self.assertIsNotNone(tr.first_tool_latency_ms)
        self.assertAlmostEqual(tr.first_tool_latency_ms, 18883, delta=2)

    def test_empty_log_not_parsed(self):
        tr = rl.parse_runlog("")
        self.assertFalse(tr.parsed)
        self.assertEqual(tr.n_tool_calls, 0)

    def test_garbage_lines_ignored(self):
        tr = rl.parse_runlog("not a klog line\nanother\n" + SAMPLE_RUNLOG)
        self.assertTrue(tr.parsed)
        self.assertEqual(tr.n_tool_calls, 2)


def _codex_line(direction: str, ts: str, message: dict) -> str:
    return json.dumps(
        {"timestamp": ts, "direction": direction, "message": message}
    )


# A codex protocol JSONL fragment: header, turn lifecycle, and two
# commandExecution items (one read, one failed search).
SAMPLE_CODEX_PROTOCOL = "\n".join(
    [
        json.dumps({"format": "codex", "version": "1.0"}),
        _codex_line(
            "received",
            "2026-05-21T05:24:42.944Z",
            {"method": "turn/started", "params": {}},
        ),
        _codex_line(
            "received",
            "2026-05-21T05:24:47.150Z",
            {
                "method": "item/started",
                "params": {
                    "item": {
                        "id": "call_A",
                        "type": "commandExecution",
                        "command": '/bin/bash -lc "sed -n 1,80p deploy.py"',
                        "commandActions": [
                            {
                                "type": "read",
                                "name": "deploy.py",
                                "path": "/x/scripts/deploy.py",
                            }
                        ],
                    }
                },
            },
        ),
        _codex_line(
            "received",
            "2026-05-21T05:24:47.350Z",
            {
                "method": "item/completed",
                "params": {
                    "item": {
                        "id": "call_A",
                        "type": "commandExecution",
                        "durationMs": 200,
                        "exitCode": 0,
                        "status": "completed",
                    }
                },
            },
        ),
        _codex_line(
            "received",
            "2026-05-21T05:24:48.000Z",
            {
                "method": "item/started",
                "params": {
                    "item": {
                        "id": "call_B",
                        "type": "commandExecution",
                        "command": '/bin/bash -lc "rg missing_pattern"',
                        "commandActions": [{"type": "search"}],
                    }
                },
            },
        ),
        _codex_line(
            "received",
            "2026-05-21T05:24:48.500Z",
            {
                "method": "item/completed",
                "params": {
                    "item": {
                        "id": "call_B",
                        "type": "commandExecution",
                        "exitCode": 1,
                        "status": "failed",
                    }
                },
            },
        ),
        _codex_line(
            "received",
            "2026-05-21T05:27:53.424Z",
            {"method": "turn/completed", "params": {}},
        ),
    ]
)


class ParseCodexProtocolTests(unittest.TestCase):
    def test_parses_tool_calls(self):
        tr = rl.parse_codex_protocol(SAMPLE_CODEX_PROTOCOL)
        self.assertTrue(tr.parsed)
        self.assertEqual(tr.backend, "codex")
        self.assertTrue(tr.session_started)
        self.assertEqual(tr.n_tool_calls, 2)
        self.assertEqual(tr.tool_kind_counts, {"read": 1, "grep": 1})

    def test_read_target_and_duration(self):
        tr = rl.parse_codex_protocol(SAMPLE_CODEX_PROTOCOL)
        read_call = tr.tool_calls[0]
        self.assertEqual(read_call.kind, "read")
        self.assertEqual(read_call.target, "/x/scripts/deploy.py")
        self.assertEqual(read_call.duration_ms, 200)
        self.assertFalse(read_call.is_error)

    def test_failed_command_is_error(self):
        tr = rl.parse_codex_protocol(SAMPLE_CODEX_PROTOCOL)
        self.assertEqual(tr.n_tool_errors, 1)
        self.assertTrue(tr.tool_calls[1].is_error)

    def test_turn_duration_and_latency(self):
        tr = rl.parse_codex_protocol(SAMPLE_CODEX_PROTOCOL)
        # turn 05:24:42.944 -> 05:27:53.424 ≈ 190480ms
        self.assertAlmostEqual(tr.total_duration_ms, 190480, delta=2)
        # first tool 05:24:47.150 - turn start 05:24:42.944 ≈ 4206ms
        self.assertAlmostEqual(tr.first_tool_latency_ms, 4206, delta=2)

    def test_files_coverage_from_codex_trace(self):
        tr = rl.parse_codex_protocol(SAMPLE_CODEX_PROTOCOL)
        rl.annotate_files_coverage(
            tr, ["scripts/deploy.py", "scripts/other.py"]
        )
        # files_read is the full distinct read set, not a files_changed subset.
        self.assertEqual(tr.files_read, ["/x/scripts/deploy.py"])
        # scripts/other.py was never read; scripts/deploy.py matches by basename.
        self.assertEqual(tr.files_changed_not_read, ["scripts/other.py"])

    def test_empty_protocol_not_parsed(self):
        tr = rl.parse_codex_protocol("")
        self.assertFalse(tr.parsed)
        self.assertEqual(tr.n_tool_calls, 0)

    def test_garbage_lines_ignored(self):
        tr = rl.parse_codex_protocol(
            "not json\n{bad}\n" + SAMPLE_CODEX_PROTOCOL
        )
        self.assertTrue(tr.parsed)
        self.assertEqual(tr.n_tool_calls, 2)

    def test_header_alone_marks_parsed(self):
        tr = rl.parse_codex_protocol(
            json.dumps({"format": "codex", "version": "1.0"})
        )
        self.assertTrue(tr.parsed)
        self.assertEqual(tr.n_tool_calls, 0)


class IsoMsTests(unittest.TestCase):
    def test_z_suffix(self):
        self.assertIsNotNone(rl._iso_ms("2026-05-21T05:24:42.944Z"))

    def test_nanosecond_fraction_truncated(self):
        # codex emits 9-digit fractions; fromisoformat only takes 6.
        self.assertIsNotNone(rl._iso_ms("2026-05-21T05:24:42.944245137Z"))

    def test_offset_form(self):
        self.assertIsNotNone(rl._iso_ms("2026-05-21T05:24:42.944+00:00"))

    def test_none_and_garbage(self):
        self.assertIsNone(rl._iso_ms(None))
        self.assertIsNone(rl._iso_ms("not a timestamp"))


class FilesCoverageTests(unittest.TestCase):
    def test_split_read_vs_not_read(self):
        tr = rl.parse_runlog(SAMPLE_RUNLOG)
        rl.annotate_files_coverage(
            tr, ["scripts/deploy.py", "tests/scripts/test_deploy_rollout.py"]
        )
        self.assertEqual(tr.files_read, ["scripts/deploy.py"])
        self.assertEqual(
            tr.files_changed_not_read, ["tests/scripts/test_deploy_rollout.py"]
        )

    def test_note_when_no_tool_calls(self):
        tr = rl.parse_runlog(
            "I0507 01:11:19.931402  1 codereview.go:134] "
            "code-review run start run_tag=x backend=codex model=gpt\n"
        )
        rl.annotate_files_coverage(tr, ["a.py"])
        self.assertTrue(any("no tool-call records" in n for n in tr.notes))

    def test_note_when_unparsed(self):
        tr = rl.parse_runlog("")
        rl.annotate_files_coverage(tr, ["a.py"])
        self.assertTrue(
            any("no usable execution log" in n for n in tr.notes)
        )

    def test_files_read_is_full_set_not_diff_subset(self):
        # A reviewer that read files OUTSIDE the diff must have them all in
        # files_read — the earlier bug intersected files_read with
        # files_changed, hiding the reviewer's true investigation breadth.
        proto = "\n".join(
            [
                json.dumps({"format": "codex", "version": "1.0"}),
                _codex_line(
                    "received",
                    "2026-05-21T05:24:42.944Z",
                    {"method": "turn/started", "params": {}},
                ),
                _codex_line(
                    "received",
                    "2026-05-21T05:24:43.000Z",
                    {
                        "method": "item/started",
                        "params": {
                            "item": {
                                "id": "c1",
                                "type": "commandExecution",
                                "command": "sed -n 1,80p Dockerfile",
                                "commandActions": [
                                    {
                                        "type": "read",
                                        "path": "/tmp/replay-kernel-4024-r1-xx/Dockerfile",
                                    }
                                ],
                            }
                        },
                    },
                ),
                _codex_line(
                    "received",
                    "2026-05-21T05:24:43.500Z",
                    {
                        "method": "item/started",
                        "params": {
                            "item": {
                                "id": "c2",
                                "type": "commandExecution",
                                "command": "sed -n 1,80p docs/testing.md",
                                "commandActions": [
                                    {
                                        "type": "read",
                                        "path": "/tmp/replay-kernel-4024-r1-xx/docs/testing.md",
                                    }
                                ],
                            }
                        },
                    },
                ),
            ]
        )
        tr = rl.parse_codex_protocol(proto)
        rl.annotate_files_coverage(tr, ["Dockerfile", "nitro.config.ts"])
        # Both reads surface — including docs/testing.md, NOT in the diff.
        self.assertEqual(
            tr.files_read, ["Dockerfile", "docs/testing.md"]
        )
        # The replay-checkout prefix is stripped to repo-relative paths.
        self.assertNotIn(
            "/tmp/replay", "".join(tr.files_read)
        )
        # Coverage diagnostic still flags the unread changed file.
        self.assertEqual(tr.files_changed_not_read, ["nitro.config.ts"])

    def test_files_read_dedups(self):
        # Same file read twice -> one entry in files_read.
        proto = "\n".join(
            [
                json.dumps({"format": "codex", "version": "1.0"}),
                _codex_line(
                    "received",
                    "2026-05-21T05:24:42.944Z",
                    {"method": "turn/started", "params": {}},
                ),
            ]
            + [
                _codex_line(
                    "received",
                    f"2026-05-21T05:24:4{i}.000Z",
                    {
                        "method": "item/started",
                        "params": {
                            "item": {
                                "id": f"c{i}",
                                "type": "commandExecution",
                                "command": "sed -n p a.py",
                                "commandActions": [
                                    {"type": "read", "path": "/x/a.py"}
                                ],
                            }
                        },
                    },
                )
                for i in (3, 4)
            ]
        )
        tr = rl.parse_codex_protocol(proto)
        rl.annotate_files_coverage(tr, [])
        self.assertEqual(tr.files_read, ["/x/a.py"])


class StripReplayCwdTests(unittest.TestCase):
    def test_strips_replay_checkout_prefix(self):
        self.assertEqual(
            rl._strip_replay_cwd("/tmp/replay-kernel-4024-r1-abc/services/x.ts"),
            "services/x.ts",
        )

    def test_leaves_non_replay_paths(self):
        self.assertEqual(
            rl._strip_replay_cwd("/home/ubuntu/.claude/skills/review/SKILL.md"),
            "/home/ubuntu/.claude/skills/review/SKILL.md",
        )
        self.assertEqual(rl._strip_replay_cwd("scripts/deploy.py"), "scripts/deploy.py")


class CodexActionKindTests(unittest.TestCase):
    def _one_item_proto(self, command_actions: list[dict]) -> str:
        return "\n".join(
            [
                json.dumps({"format": "codex", "version": "1.0"}),
                _codex_line(
                    "received",
                    "2026-05-21T05:24:42.944Z",
                    {"method": "turn/started", "params": {}},
                ),
                _codex_line(
                    "received",
                    "2026-05-21T05:24:43.000Z",
                    {
                        "method": "item/started",
                        "params": {
                            "item": {
                                "id": "c1",
                                "type": "commandExecution",
                                "command": "cmd",
                                "commandActions": command_actions,
                            }
                        },
                    },
                ),
            ]
        )

    def test_listfiles_maps_to_glob(self):
        # The codex protocol emits `listFiles` (not `list`) for directory
        # enumeration; it must map to the glob kind, not fall through to shell.
        tr = rl.parse_codex_protocol(
            self._one_item_proto([{"type": "listFiles", "name": "libs/"}])
        )
        self.assertEqual(tr.tool_calls[0].kind, "glob")

    def test_read_action_preferred_over_unknown(self):
        # An item with an `unknown` action listed before a `read` action must
        # still be classified `read` so the file target is recovered.
        tr = rl.parse_codex_protocol(
            self._one_item_proto(
                [
                    {"type": "unknown", "path": None},
                    {"type": "read", "path": "/x/found.py"},
                ]
            )
        )
        self.assertEqual(tr.tool_calls[0].kind, "read")
        self.assertEqual(tr.tool_calls[0].target, "/x/found.py")

    def test_unknown_only_stays_shell(self):
        tr = rl.parse_codex_protocol(
            self._one_item_proto([{"type": "unknown", "path": None}])
        )
        self.assertEqual(tr.tool_calls[0].kind, "shell")
        self.assertIsNone(tr.tool_calls[0].target)


class CollectExecutionTraceTests(unittest.TestCase):
    """The round dir is shared by both configs; a codex protocol JSONL must
    only be attributed to the codex run, never the cursor sibling."""

    def _round_dir(self, d: Path) -> Path:
        # A codex protocol JSONL lands in the shared round dir.
        (d / "reviewer-session-20260521-052441.jsonl").write_text(
            SAMPLE_CODEX_PROTOCOL
        )
        return d

    def test_cursor_run_ignores_codex_protocol(self):
        with tempfile.TemporaryDirectory() as d:
            rlog = self._round_dir(Path(d))
            tr = replay.collect_execution_trace(
                run_tag="code-review-replay:x-1:r1:cursor-composer2",
                started_at=0.0,
                round_log=rlog,
                config_name="cursor-composer2",
                backend="cursor",
                files_changed=["a.py"],
            )
            # No klogfmt log found + protocol ignored => empty cursor trace,
            # NOT the codex sibling's 2 tool calls.
            self.assertEqual(tr.n_tool_calls, 0)
            self.assertIsNone(tr.protocol_log_path)

    def test_codex_run_uses_protocol(self):
        with tempfile.TemporaryDirectory() as d:
            rlog = self._round_dir(Path(d))
            tr = replay.collect_execution_trace(
                run_tag="code-review-replay:x-1:r1:codex-5.4-mini",
                started_at=0.0,
                round_log=rlog,
                config_name="codex-5.4-mini",
                backend="codex",
                files_changed=["a.py"],
            )
            self.assertEqual(tr.n_tool_calls, 2)
            self.assertIsNotNone(tr.protocol_log_path)


class FindRunlogByTagTests(unittest.TestCase):
    def test_matches_tag_in_recent_log(self):
        with tempfile.TemporaryDirectory() as d:
            log_dir = Path(d)
            p = log_dir / "code-review-20260507-011119-228289.log"
            p.write_text(SAMPLE_RUNLOG)
            found = rl.find_runlog_by_tag(
                log_dir,
                "code-review-replay:kernel-3834:r1:cursor-composer2",
            )
            self.assertEqual(found, p)

    def test_no_match_returns_none(self):
        with tempfile.TemporaryDirectory() as d:
            log_dir = Path(d)
            (log_dir / "code-review-x.log").write_text(SAMPLE_RUNLOG)
            self.assertIsNone(
                rl.find_runlog_by_tag(log_dir, "nonexistent-tag")
            )

    def test_missing_dir_returns_none(self):
        self.assertIsNone(
            rl.find_runlog_by_tag(Path("/no/such/dir"), "tag")
        )


class GoalDivergenceTests(unittest.TestCase):
    def test_identical_after_whitespace_does_not_diverge(self):
        self.assertFalse(
            rl._materially_diverges("fix the bug", "fix   the\nbug")
        )

    def test_unrelated_goals_diverge(self):
        self.assertTrue(
            rl._materially_diverges(
                "refactor the authentication middleware layer",
                "update documentation for the deployment pipeline",
            )
        )

    def test_dataset_goal_source_skips_reconstruction(self):
        dataset_round = {"round": 1, "goal_text": "the recorded goal text"}
        # prefer=dataset must not touch the repo or gh — pass a bogus path.
        result = rl.build_goal(
            dataset_round,
            repo_path=Path("/no/such/repo"),
            pr_number="1",
            state=None,
            bramble_ops_path=Path("/no/such/bramble_ops.py"),
            prefer="dataset",
        )
        self.assertEqual(result.text, "the recorded goal text")
        self.assertEqual(result.source, "dataset_fallback")
        self.assertFalse(result.goal_divergence)


class ValidateVerdictTests(unittest.TestCase):
    def _ok_verdict(self) -> dict:
        return {
            "finding_verdicts": [
                {"index": 0, "verdict": "true_positive", "reason": "x"},
                {"index": 1, "verdict": "false_positive", "reason": "y"},
            ],
            "missed_real_issues": [],
            "execution_analysis": [],
        }

    def test_valid(self):
        self.assertIsNone(rl.validate_verdict(self._ok_verdict()))

    def test_not_an_object(self):
        self.assertIsNotNone(rl.validate_verdict([1, 2]))

    def test_missing_finding_verdicts(self):
        self.assertIsNotNone(rl.validate_verdict({}))

    def test_bad_verdict_value(self):
        v = self._ok_verdict()
        v["finding_verdicts"][0]["verdict"] = "maybe"
        self.assertIsNotNone(rl.validate_verdict(v))

    def test_non_int_index(self):
        v = self._ok_verdict()
        v["finding_verdicts"][0]["index"] = "0"
        self.assertIsNotNone(rl.validate_verdict(v))


class ScoreFromVerdictsTests(unittest.TestCase):
    def test_precision_recall_f1(self):
        replay_findings = [{"message": "a"}, {"message": "b"}, {"message": "c"}]
        # mechanical hints: finding 0 the dataset called real, 1 called FP.
        mech = [
            {"dataset_is_real_issue": True},
            {"dataset_is_real_issue": False},
            {"dataset_is_real_issue": None},
        ]
        judge = {
            "finding_verdicts": [
                {"index": 0, "verdict": "true_positive"},
                {"index": 1, "verdict": "false_positive"},
                {"index": 2, "verdict": "true_positive"},
            ],
            "missed_real_issues": [
                {"file": "x", "line": 1, "description": "missed"}
            ],
        }
        s = rl.score_from_verdicts(
            backend="codex",
            model="gpt",
            config="codex-5.4-mini",
            envelope_status="ok",
            verdict="rejected",
            duration_ms=1000,
            replay_findings=replay_findings,
            mechanical_match=mech,
            judge_verdict=judge,
        )
        self.assertEqual(s.judged_tp, 2)
        self.assertEqual(s.judged_fp, 1)
        self.assertEqual(s.n_missed_real, 1)
        # precision = 2/3, recall = 2/(2+1) = 2/3
        self.assertAlmostEqual(s.precision, 2 / 3)
        self.assertAlmostEqual(s.recall, 2 / 3)
        self.assertAlmostEqual(s.f1, 2 / 3)

    def test_dataset_agreement_rate(self):
        # Judge agrees with the dataset on finding 0 (both real), disagrees
        # on finding 1 (dataset says FP, judge says TP).
        replay_findings = [{"message": "a"}, {"message": "b"}]
        mech = [
            {"dataset_is_real_issue": True},
            {"dataset_is_real_issue": False},
        ]
        judge = {
            "finding_verdicts": [
                {"index": 0, "verdict": "true_positive"},
                {"index": 1, "verdict": "true_positive"},
            ]
        }
        s = rl.score_from_verdicts(
            backend="codex",
            model="gpt",
            config="c",
            envelope_status="ok",
            verdict=None,
            duration_ms=None,
            replay_findings=replay_findings,
            mechanical_match=mech,
            judge_verdict=judge,
        )
        self.assertEqual(s.dataset_comparisons, 2)
        self.assertEqual(s.dataset_agreements, 1)
        self.assertAlmostEqual(s.dataset_agreement_rate, 0.5)

    def test_skipped_finding_counts_unsure(self):
        # Judge only judged finding 0; finding 1 was skipped.
        s = rl.score_from_verdicts(
            backend="codex",
            model="gpt",
            config="c",
            envelope_status="ok",
            verdict=None,
            duration_ms=None,
            replay_findings=[{"message": "a"}, {"message": "b"}],
            mechanical_match=[],
            judge_verdict={
                "finding_verdicts": [
                    {"index": 0, "verdict": "true_positive"}
                ]
            },
        )
        self.assertEqual(s.judged_tp, 1)
        self.assertEqual(s.judged_unsure, 1)

    def test_no_findings_metrics_none(self):
        s = rl.score_from_verdicts(
            backend="codex",
            model="gpt",
            config="c",
            envelope_status="ok",
            verdict="accepted",
            duration_ms=None,
            replay_findings=[],
            mechanical_match=[],
            judge_verdict={"finding_verdicts": [], "missed_real_issues": []},
        )
        self.assertIsNone(s.precision)
        self.assertIsNone(s.recall)
        self.assertIsNone(s.f1)


class FoldVerdictsTests(unittest.TestCase):
    def _artifact(self) -> dict:
        return {
            "rounds": [
                {
                    "round": 1,
                    "signal_tier": "r1",
                    "runs": [
                        {
                            "backend": "codex",
                            "model": "gpt",
                            "config": "codex-5.4-mini",
                            "envelope_status": "ok",
                            "verdict": "rejected",
                            "duration_ms": 1000,
                            "replay_findings": [{"message": "a"}],
                            "mechanical_match": [
                                {"dataset_is_real_issue": True}
                            ],
                        }
                    ],
                }
            ]
        }

    def test_fold_with_verdict(self):
        with tempfile.TemporaryDirectory() as d:
            vdir = Path(d)
            (vdir / "r1-codex-5.4-mini-verdict.json").write_text(
                json.dumps(
                    {
                        "finding_verdicts": [
                            {"index": 0, "verdict": "true_positive"}
                        ],
                        "missed_real_issues": [],
                    }
                )
            )
            scored = rl.fold_verdicts(self._artifact(), vdir)
            self.assertEqual(len(scored), 1)
            self.assertEqual(scored[0].judged_tp, 1)
            self.assertIsNone(scored[0].fold_error)

    def test_fold_missing_verdict_file(self):
        with tempfile.TemporaryDirectory() as d:
            scored = rl.fold_verdicts(self._artifact(), Path(d))
            self.assertEqual(len(scored), 1)
            self.assertIsNotNone(scored[0].fold_error)
            self.assertEqual(scored[0].judged_unsure, 1)

    def test_fold_malformed_verdict(self):
        with tempfile.TemporaryDirectory() as d:
            vdir = Path(d)
            (vdir / "r1-codex-5.4-mini-verdict.json").write_text(
                json.dumps({"finding_verdicts": "not a list"})
            )
            scored = rl.fold_verdicts(self._artifact(), vdir)
            self.assertIsNotNone(scored[0].fold_error)


class MechanicalMatchTests(unittest.TestCase):
    def test_exact_match_hint(self):
        replay = [
            {
                "file": "scripts/deploy.py",
                "line": 679,
                "severity": "high",
                "message": "off-by-one in the rollout index calculation",
            }
        ]
        dataset = [
            {
                "file": "scripts/deploy.py",
                "line": 679,
                "severity": "high",
                "message": "off-by-one in the rollout index calculation",
                "ground_truth": {"is_real_issue": True, "action": "fixed"},
            }
        ]
        from replay import build_mechanical_match

        hints = build_mechanical_match(replay, dataset)
        self.assertEqual(hints[0]["match_strategy"], "exact")
        self.assertEqual(hints[0]["dataset_is_real_issue"], True)
        self.assertEqual(hints[0]["dataset_index"], 0)

    def test_no_match_hint(self):
        from replay import build_mechanical_match

        replay = [
            {
                "file": "totally/different.py",
                "line": 1,
                "severity": "low",
                "message": "an unrelated finding about something else",
            }
        ]
        dataset = [
            {
                "file": "scripts/deploy.py",
                "line": 679,
                "severity": "high",
                "message": "off-by-one in the rollout index calculation",
                "ground_truth": {"is_real_issue": True},
            }
        ]
        hints = build_mechanical_match(replay, dataset)
        self.assertEqual(hints[0]["match_strategy"], "none")
        self.assertIsNone(hints[0]["dataset_index"])


class SelectDatasetRoundsTests(unittest.TestCase):
    def _dataset(self) -> dict:
        return {
            "harvested_rounds": [
                {"round": 1, "signal_tier": "r1"},
                {"round": 2, "signal_tier": "final"},
            ]
        }

    def test_no_filter_returns_all(self):
        rounds = replay.select_dataset_rounds(self._dataset(), None)
        self.assertEqual(len(rounds), 2)

    def test_r1_filter(self):
        rounds = replay.select_dataset_rounds(self._dataset(), "r1")
        self.assertEqual([r["round"] for r in rounds], [1])

    def test_final_filter_includes_incomplete(self):
        ds = {
            "harvested_rounds": [
                {"round": 1, "signal_tier": "r1"},
                {"round": 3, "signal_tier": "final_incomplete"},
            ]
        }
        rounds = replay.select_dataset_rounds(ds, "final")
        self.assertEqual([r["round"] for r in rounds], [3])


if __name__ == "__main__":
    unittest.main()
