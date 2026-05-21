"""Unit + integration tests for the eval-dataset harvester."""

from __future__ import annotations

import json
import os
import sys
import tempfile
import unittest
from pathlib import Path

TEST_DIR = Path(__file__).resolve().parent
SCRIPT_DIR = TEST_DIR.parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

import harvest_lib as hl  # noqa: E402

KERNEL_3945_DIR = Path.home() / ".bramble" / "projects" / "kernel-3945"
BRAMBLE_OPS_PATH = (
    SCRIPT_DIR.parents[1] / "pr-polish" / "scripts" / "bramble_ops.py"
)


class ParseProjectDirNameTests(unittest.TestCase):
    def test_pr_numbered(self):
        self.assertEqual(hl.parse_project_dir_name("kernel-3945"), ("kernel", "3945"))
        self.assertEqual(hl.parse_project_dir_name("yoloswe-236"), ("yoloswe", "236"))
        self.assertEqual(hl.parse_project_dir_name("nebula-81"), ("nebula", "81"))

    def test_skips_doc_and_branch(self):
        self.assertIsNone(
            hl.parse_project_dir_name("kernel-doc-naming-rethink-cb9650558e82")
        )
        self.assertIsNone(
            hl.parse_project_dir_name("yoloswe-branch-feature-meeting-bot")
        )
        self.assertIsNone(
            hl.parse_project_dir_name("yoloswe-doc-meetingbot-architecture-e09ea41ac75e")
        )


class NormalizePathTests(unittest.TestCase):
    def test_leading_dot_slash(self):
        self.assertEqual(hl.normalize_path("./services/x.py"), "services/x.py")

    def test_none_and_empty(self):
        self.assertIsNone(hl.normalize_path(None))
        self.assertIsNone(hl.normalize_path(""))
        self.assertIsNone(hl.normalize_path("   "))

    def test_backslashes(self):
        self.assertEqual(hl.normalize_path("a\\b\\c.py"), "a/b/c.py")


class TopicTokenOverlapTests(unittest.TestCase):
    def test_zero_when_empty(self):
        self.assertEqual(hl.topic_token_overlap("", "anything"), 0.0)
        self.assertEqual(hl.topic_token_overlap("anything", ""), 0.0)

    def test_overlap(self):
        # Topic tokens are a subset of message tokens -> overlap > 0.5.
        topic = "deadline cache keyed by project"
        message = "deadline cache keyed by project identifier"
        self.assertGreater(hl.topic_token_overlap(topic, message), 0.5)

    def test_no_overlap(self):
        self.assertLess(
            hl.topic_token_overlap("flag toggle bool", "completely unrelated text"),
            0.3,
        )


class TopicTokenContainmentTests(unittest.TestCase):
    def test_zero_when_empty(self):
        self.assertEqual(hl.topic_token_containment("", "anything"), 0.0)
        self.assertEqual(hl.topic_token_containment("anything", ""), 0.0)

    def test_containment_ignores_extra_body_tokens(self):
        # The kernel-4189 shape: topic is a summary; the body has a verbatim
        # heading plus severity/description boilerplate. Symmetric Jaccard
        # drops below 0.5 here, but containment stays high.
        topic = "concurrent recreate leaves partial drops if one drop fails"
        body = (
            "### Concurrent recreate leaves partial drops\n\n"
            "**Medium Severity**\n\n<!-- DESCRIPTION START -->"
        )
        self.assertGreater(hl.topic_token_containment(topic, body), 0.6)
        self.assertLess(hl.topic_token_overlap(topic, body), 0.5)

    def test_full_containment(self):
        self.assertEqual(
            hl.topic_token_containment("alpha beta", "alpha beta gamma delta"),
            1.0,
        )


class DeriveIsRealIssueTests(unittest.TestCase):
    def test_table(self):
        self.assertIs(hl.derive_is_real_issue("fixed"), True)
        self.assertIs(hl.derive_is_real_issue("wont_fix"), True)
        self.assertIs(hl.derive_is_real_issue("false_positive"), False)
        self.assertIs(hl.derive_is_real_issue("stale"), False)
        self.assertIsNone(hl.derive_is_real_issue("ack"))
        self.assertIsNone(hl.derive_is_real_issue("flake"))
        self.assertIsNone(hl.derive_is_real_issue("pre_existing"))
        self.assertIsNone(hl.derive_is_real_issue(None))
        self.assertIsNone(hl.derive_is_real_issue("garbage"))


class MatchFindingTests(unittest.TestCase):
    def test_exact_match(self):
        finding = {
            "file": "services/x.py",
            "line": 94,
            "severity": "high",
            "message": "the preview restore deadline cache is still keyed by project",
        }
        actions = [
            {
                "source": "codex",
                "path": "services/x.py",
                "line": 94,
                "severity": "high",
                "topic": "deadline cache is still keyed",
                "action": "fixed",
            },
        ]
        match, strategy = hl.match_finding_to_action(finding, "codex", actions)
        self.assertEqual(strategy, "exact")
        self.assertIsNotNone(match)
        self.assertEqual(match["action"], "fixed")

    def test_sweep_source_wildcard_backend(self):
        finding = {
            "file": "x.py",
            "line": 10,
            "severity": "medium",
            "message": "msg",
        }
        actions = [
            {
                "source": "sweep",
                "path": "x.py",
                "line": 10,
                "severity": "medium",
                "topic": "...",
                "action": "fixed",
            },
        ]
        match, strategy = hl.match_finding_to_action(finding, "cursor", actions)
        self.assertEqual(strategy, "exact")
        self.assertEqual(match["source"], "sweep")

    def test_topic_path_line_fallback(self):
        finding = {
            "file": "./x.py",
            "line": 12,  # ±3 of action.line=10
            "severity": "high",  # severity mismatch -> Tier 1 fails
            "message": "the preview restore deadline cache is still keyed",
        }
        actions = [
            {
                "source": "codex",
                "path": "x.py",
                "line": 10,
                "severity": "medium",
                "topic": "deadline cache is still keyed",
                "action": "fixed",
            },
        ]
        match, strategy = hl.match_finding_to_action(finding, "codex", actions)
        self.assertEqual(strategy, "topic_path_line")
        self.assertEqual(match["action"], "fixed")

    def test_topic_only_when_no_path(self):
        finding = {
            "file": "x.py",
            "line": 99,
            "severity": "low",
            "message": "the preview restore deadline cache is still keyed by project",
        }
        actions = [
            {
                "source": "codex",
                "path": None,
                "line": None,
                "severity": None,
                "topic": "deadline cache is still keyed",
                "action": "wont_fix",
                "reason": "by design",
            },
        ]
        match, strategy = hl.match_finding_to_action(finding, "codex", actions)
        self.assertEqual(strategy, "topic_only")
        self.assertEqual(match["action"], "wont_fix")

    def test_no_match(self):
        finding = {
            "file": "x.py",
            "line": 1,
            "severity": "low",
            "message": "totally unrelated content here",
        }
        actions = [
            {
                "source": "codex",
                "path": "y.py",
                "line": 50,
                "severity": "high",
                "topic": "something else entirely about flags",
                "action": "fixed",
            },
        ]
        match, strategy = hl.match_finding_to_action(finding, "codex", actions)
        self.assertIsNone(match)
        self.assertEqual(strategy, "none")

    def test_fixed_preferred_over_false_positive_on_tie(self):
        finding = {
            "file": "x.py",
            "line": 10,
            "severity": "high",
            "message": "the preview restore deadline cache is still keyed",
        }
        actions = [
            {
                "source": "codex",
                "path": "x.py",
                "line": 10,
                "severity": "high",
                "topic": "deadline cache is still keyed",
                "action": "false_positive",
                "reason": "not actually",
            },
            {
                "source": "codex",
                "path": "x.py",
                "line": 10,
                "severity": "high",
                "topic": "deadline cache is still keyed",
                "action": "fixed",
            },
        ]
        match, _ = hl.match_finding_to_action(finding, "codex", actions)
        self.assertEqual(match["action"], "fixed")


class NormalizeRemoteUrlTests(unittest.TestCase):
    def test_ssh_to_https(self):
        self.assertEqual(
            hl.normalize_remote_url("git@github.com:anthropics/kernel.git"),
            "https://github.com/anthropics/kernel",
        )

    def test_https_unchanged(self):
        self.assertEqual(
            hl.normalize_remote_url("https://github.com/x/y.git"),
            "https://github.com/x/y",
        )

    def test_ssh_url_form(self):
        self.assertEqual(
            hl.normalize_remote_url("ssh://git@github.com/x/y.git"),
            "https://github.com/x/y",
        )


class SelectRoundsTests(unittest.TestCase):
    def test_completed_multi_round(self):
        state = {
            "completed": True,
            "rounds": [{"n": 1}, {"n": 2}, {"n": 3}],
        }
        self.assertEqual(
            hl.select_rounds_to_harvest(state), [(1, "r1"), (3, "final")]
        )

    def test_incomplete_multi_round(self):
        state = {
            "completed": False,
            "rounds": [{"n": 1}, {"n": 2}],
        }
        self.assertEqual(
            hl.select_rounds_to_harvest(state), [(1, "r1"), (2, "final_incomplete")]
        )

    def test_single_round(self):
        state = {"completed": True, "rounds": [{"n": 1}]}
        self.assertEqual(hl.select_rounds_to_harvest(state), [(1, "r1_only")])

    def test_empty(self):
        self.assertEqual(hl.select_rounds_to_harvest({"rounds": []}), [])
        self.assertEqual(hl.select_rounds_to_harvest({}), [])


class ComputeMergeBaseTests(unittest.TestCase):
    def test_no_repo_mapping(self):
        sha, resolved, err = hl.compute_merge_base(None, "abc123")
        self.assertIsNone(sha)
        self.assertFalse(resolved)
        self.assertIn("no repo mapping", err)

    def test_nonexistent_repo_path(self):
        sha, resolved, err = hl.compute_merge_base(
            Path("/nonexistent/path"), "abc123"
        )
        self.assertIsNone(sha)
        self.assertFalse(resolved)

    def test_bogus_commit_in_real_repo(self):
        # Use the yoloswe worktree itself as a real repo.
        repo = Path(__file__).resolve().parents[4]
        sha, resolved, err = hl.compute_merge_base(
            repo, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef"
        )
        self.assertIsNone(sha)
        self.assertFalse(resolved)


class ReconstructGoalTextTests(unittest.TestCase):
    def test_r1_with_pr_summary(self):
        text, ok = hl.reconstruct_goal_text(
            state={"rounds": []},
            round_n=1,
            head_before=None,
            pr_summary="some PR summary",
            bramble_ops_path=BRAMBLE_OPS_PATH,
            repo_path=None,
        )
        self.assertEqual(text, "some PR summary")
        self.assertTrue(ok)

    def test_r1_without_pr_summary(self):
        text, ok = hl.reconstruct_goal_text(
            state={"rounds": []},
            round_n=1,
            head_before=None,
            pr_summary=None,
            bramble_ops_path=BRAMBLE_OPS_PATH,
            repo_path=None,
        )
        self.assertIsNone(text)
        self.assertFalse(ok)


@unittest.skipUnless(
    KERNEL_3945_DIR.exists() and BRAMBLE_OPS_PATH.exists(),
    "kernel-3945 fixture or bramble_ops.py not present",
)
class BuildPRRecordKernel3945Snapshot(unittest.TestCase):
    """Integration test against the real ~/.bramble/projects/kernel-3945."""

    def test_snapshot(self):
        record = hl.build_pr_record(
            KERNEL_3945_DIR,
            "kernel",
            "3945",
            repo_map=hl.RepoMap(),  # no repo mapping → merge_base unresolved
            pr_summary=None,
            harvester_sha="testsha",
            harvested_at="2026-05-20T00:00:00Z",
            bramble_ops_path=BRAMBLE_OPS_PATH,
            fetch_attempted=False,  # offline integration test
        )
        self.assertIsNotNone(record)
        self.assertEqual(record.schema_version, 2)
        self.assertEqual(record.pr["repo_name"], "kernel")
        self.assertEqual(record.pr["pr_number"], "3945")
        # kernel-3945 had 5 rounds, completed=True → R1 + R5
        self.assertEqual(len(record.harvested_rounds), 2)
        r1, r_final = record.harvested_rounds
        self.assertEqual(r1.round, 1)
        self.assertEqual(r1.signal_tier, "r1")
        self.assertEqual(r_final.round, 5)
        self.assertEqual(r_final.signal_tier, "final")

        # Fetch was skipped → github comments come from the state fallback.
        self.assertEqual(record.pr_comments_attribution_basis, "no_timestamp")

        # R1 should have at least codex + cursor review_runs.
        backends_r1 = {rr.backend for rr in r1.review_runs}
        self.assertIn("codex", backends_r1)

        # Cursor R1 envelope was status=error in the real fixture.
        cursor = next((rr for rr in r1.review_runs if rr.backend == "cursor"), None)
        if cursor is not None:
            self.assertIn(cursor.envelope_status, {"ok", "error"})
            if cursor.envelope_status == "error":
                self.assertEqual(cursor.findings, [])

        # At least one finding ground-truthed as 'fixed' across the run.
        all_actions = [
            f.ground_truth.action
            for rr in r1.review_runs
            for f in rr.findings
        ]
        self.assertIn("fixed", all_actions)

        # No repo mapping → merge_base must be unresolved for both rounds.
        for hr in record.harvested_rounds:
            self.assertFalse(hr.merge_base_resolved)
            self.assertIsNone(hr.merge_base_sha)

    def test_write_round_trip(self):
        record = hl.build_pr_record(
            KERNEL_3945_DIR,
            "kernel",
            "3945",
            repo_map=hl.RepoMap(),
            pr_summary=None,
            harvester_sha="testsha",
            harvested_at="2026-05-20T00:00:00Z",
            bramble_ops_path=BRAMBLE_OPS_PATH,
            fetch_attempted=False,
        )
        with tempfile.TemporaryDirectory() as td:
            out = Path(td)
            path = hl.write_pr_record(out, record)
            self.assertTrue(path.exists())
            loaded = json.loads(path.read_text())
            self.assertEqual(loaded["pr"]["pr_number"], "3945")
            self.assertEqual(len(loaded["harvested_rounds"]), 2)
            self.assertEqual(loaded["schema_version"], 2)
            self.assertEqual(
                loaded["pr_comments_attribution_basis"], "no_timestamp"
            )


class IndexCommentVerdictsTests(unittest.TestCase):
    def test_dedup_keeps_earliest_round(self):
        # comment id 999 recurs in rounds 8, 10, 12 (the yoloswe-236 shape).
        # The round-8 verdict is the real one; later rows are re-fetched echoes.
        state = {
            "rounds": [
                {
                    "n": 8,
                    "comment_actions": [
                        {
                            "source": "github-inline",
                            "comment_id": 999,
                            "action": "fixed",
                            "reason": None,
                        }
                    ],
                },
                {
                    "n": 10,
                    "comment_actions": [
                        {
                            "source": "github-inline",
                            "comment_id": 999,
                            "action": "ack",
                            "reason": "echo",
                        }
                    ],
                },
                {
                    "n": 12,
                    "comment_actions": [
                        {
                            "source": "github-inline",
                            "comment_id": 999,
                            "action": "ack",
                            "reason": "echo",
                        }
                    ],
                },
            ]
        }
        idx = hl.index_comment_verdicts(state)
        self.assertEqual(set(idx.by_id.keys()), {999})
        self.assertEqual(idx.by_id[999]["action"], "fixed")
        self.assertEqual(idx.by_id[999]["triaged_in_round"], 8)
        self.assertEqual(idx.by_topic, [])

    def test_null_id_goes_to_topic_index(self):
        state = {
            "rounds": [
                {
                    "n": 1,
                    "comment_actions": [
                        {"source": "codex", "comment_id": None, "action": "fixed"},
                        {
                            "source": "github-inline",
                            "comment_id": None,
                            "action": "fixed",
                            "topic": "concurrent recreate leaves partial drops",
                        },
                        {
                            "source": "github-review",
                            "comment_id": 5,
                            "action": "wont_fix",
                        },
                    ],
                }
            ]
        }
        idx = hl.index_comment_verdicts(state)
        # Keyed index: only the github comment with a real id.
        self.assertEqual(set(idx.by_id.keys()), {5})
        self.assertEqual(idx.by_id[5]["action"], "wont_fix")
        # Topic index: the null-id github row (codex non-github row excluded).
        self.assertEqual(len(idx.by_topic), 1)
        self.assertEqual(idx.by_topic[0]["action"], "fixed")
        self.assertEqual(idx.by_topic[0]["source"], "github-inline")


class AttributeCommentToRoundTests(unittest.TestCase):
    ROUND_TIMES = [
        (1, "2026-05-07T00:00:00Z"),
        (4, "2026-05-07T06:00:00Z"),
        (6, "2026-05-07T12:00:00Z"),
    ]

    def test_inside_middle_window(self):
        self.assertEqual(
            hl.attribute_comment_to_round(
                "2026-05-07T07:30:00Z", self.ROUND_TIMES
            ),
            4,
        )

    def test_before_first_round(self):
        self.assertEqual(
            hl.attribute_comment_to_round(
                "2026-05-06T23:00:00Z", self.ROUND_TIMES
            ),
            1,
        )

    def test_after_last_round(self):
        self.assertEqual(
            hl.attribute_comment_to_round(
                "2026-05-09T00:00:00Z", self.ROUND_TIMES
            ),
            6,
        )

    def test_empty_created_at_goes_last(self):
        self.assertEqual(
            hl.attribute_comment_to_round(None, self.ROUND_TIMES), 6
        )

    def test_no_round_times(self):
        self.assertIsNone(hl.attribute_comment_to_round("2026-05-07T00:00:00Z", []))

    def test_unresolved_times_attribute_last(self):
        # All boundary times None -> attribute everything to the last round.
        rt = [(1, None), (2, None), (3, None)]
        self.assertEqual(hl.attribute_comment_to_round("2026-05-07Z", rt), 3)


class FoldCommentToHarvestedRoundTests(unittest.TestCase):
    def test_middle_round_folds_to_final(self):
        # attributed round 4, harvested rounds {1, 7} -> emit on final (7).
        self.assertEqual(hl.fold_comment_to_harvested_round(4, [1, 7]), 7)

    def test_round_one_folds_to_r1(self):
        self.assertEqual(hl.fold_comment_to_harvested_round(1, [1, 7]), 1)

    def test_before_r1_folds_to_r1(self):
        self.assertEqual(hl.fold_comment_to_harvested_round(1, [1, 7]), 1)

    def test_none_attribution_folds_to_final(self):
        self.assertEqual(hl.fold_comment_to_harvested_round(None, [1, 7]), 7)

    def test_no_harvested_rounds(self):
        self.assertIsNone(hl.fold_comment_to_harvested_round(4, []))


class BuildPRCommentsTests(unittest.TestCase):
    def _state(self):
        return {
            "rounds": [
                {
                    "n": 1,
                    "head_before": "sha1",
                    "comment_actions": [
                        {
                            "source": "github-inline",
                            "comment_id": 100,
                            "action": "fixed",
                            "reason": None,
                        }
                    ],
                },
                {"n": 4, "head_before": "sha4", "comment_actions": []},
            ]
        }

    def test_fetch_skipped_uses_state_fallback(self):
        rows, basis = hl.build_pr_comments(
            self._state(), [], None, fetch_attempted=False
        )
        self.assertEqual(basis, "no_timestamp")
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["comment_id"], 100)
        self.assertEqual(rows[0]["action"], "fixed")
        self.assertEqual(rows[0]["attributed_round"], 1)
        self.assertIsNone(rows[0]["body"])  # no fetched body in fallback

    def test_unmapped_repo_fallback(self):
        # Fetch produced comments but no repo -> can't resolve round times.
        fetched = [
            {
                "id": 100,
                "source": "github-inline",
                "author": "cursor[bot]",
                "is_bot": True,
                "path": "a.py",
                "line": 5,
                "body": "issue here",
                "created_at": "2026-05-07T07:00:00Z",
                "original_commit_id": "abc",
            },
            {
                "id": 200,  # GitHub comment pr-polish never triaged
                "source": "github-issue",
                "author": "human",
                "is_bot": False,
                "path": None,
                "line": None,
                "body": "looks good",
                "created_at": "2026-05-07T08:00:00Z",
                "original_commit_id": None,
            },
        ]
        rows, basis = hl.build_pr_comments(
            self._state(), fetched, None, fetch_attempted=True
        )
        self.assertEqual(basis, "unmapped_repo_fallback")
        self.assertEqual(len(rows), 2)
        by_id = {r["comment_id"]: r for r in rows}
        # Verdict joined for the triaged comment.
        self.assertEqual(by_id[100]["action"], "fixed")
        self.assertEqual(by_id[100]["body"], "issue here")
        # Untriaged comment is still emitted, action null (complete census).
        self.assertIsNone(by_id[200]["action"])
        # Unmapped repo -> everything attributes to round 1.
        self.assertEqual(by_id[100]["attributed_round"], 1)
        self.assertEqual(by_id[200]["attributed_round"], 1)

    def test_topic_substring_fallback_join(self):
        # pr-polish recorded the verdict with comment_id=null (older run);
        # the fetched comment carries a real id but the join must still
        # succeed by matching the recorded topic as a substring of the body.
        state = {
            "rounds": [
                {
                    "n": 1,
                    "head_before": "sha1",
                    "comment_actions": [
                        {
                            "source": "github-inline",
                            "comment_id": None,
                            "action": "wont_fix",
                            "reason": "by design",
                            "topic": "concurrent recreate leaves partial drops",
                        }
                    ],
                }
            ]
        }
        fetched = [
            {
                "id": 555,
                "source": "github-inline",
                "author": "cursor[bot]",
                "is_bot": True,
                "path": "deploy.py",
                "line": None,
                "body": "### Concurrent recreate leaves partial drops\n\nMedium",
                "created_at": "2026-05-07T07:00:00Z",
                "original_commit_id": "abc",
            }
        ]
        rows, _ = hl.build_pr_comments(
            state, fetched, None, fetch_attempted=True
        )
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["action"], "wont_fix")
        self.assertEqual(rows[0]["reason"], "by design")

    def test_topic_containment_fallback_join(self):
        # Recorded topic is a summary phrase LONGER than the body's verbatim
        # heading -> no substring match, but token containment recovers it.
        state = {
            "rounds": [
                {
                    "n": 1,
                    "head_before": "sha1",
                    "comment_actions": [
                        {
                            "source": "github-inline",
                            "comment_id": None,
                            "action": "wont_fix",
                            "reason": "by design",
                            "topic": (
                                "concurrent recreate leaves partial drops "
                                "if one drop fails"
                            ),
                        }
                    ],
                }
            ]
        }
        fetched = [
            {
                "id": 555,
                "source": "github-inline",
                "author": "cursor[bot]",
                "is_bot": True,
                "path": "deploy.py",
                "line": None,
                "body": (
                    "### Concurrent recreate leaves partial drops\n\n"
                    "**Medium Severity**\n\n<!-- DESCRIPTION START -->"
                ),
                "created_at": "2026-05-07T07:00:00Z",
                "original_commit_id": "abc",
            }
        ]
        rows, _ = hl.build_pr_comments(
            state, fetched, None, fetch_attempted=True
        )
        self.assertEqual(rows[0]["action"], "wont_fix")

    def test_topic_fallback_consumed_once(self):
        # One null-id verdict must not join to two fetched comments.
        state = {
            "rounds": [
                {
                    "n": 1,
                    "head_before": "sha1",
                    "comment_actions": [
                        {
                            "source": "github-inline",
                            "comment_id": None,
                            "action": "fixed",
                            "topic": "shared topic phrase",
                        }
                    ],
                }
            ]
        }
        fetched = [
            {
                "id": 1,
                "source": "github-inline",
                "author": "a",
                "is_bot": True,
                "path": "p",
                "line": 1,
                "body": "shared topic phrase here",
                "created_at": "2026-05-07T07:00:00Z",
                "original_commit_id": None,
            },
            {
                "id": 2,
                "source": "github-inline",
                "author": "a",
                "is_bot": True,
                "path": "p",
                "line": 2,
                "body": "shared topic phrase also here",
                "created_at": "2026-05-07T08:00:00Z",
                "original_commit_id": None,
            },
        ]
        rows, _ = hl.build_pr_comments(
            state, fetched, None, fetch_attempted=True
        )
        joined = [r for r in rows if r["action"] == "fixed"]
        self.assertEqual(len(joined), 1)


@unittest.skipUnless(
    KERNEL_3945_DIR.exists() and BRAMBLE_OPS_PATH.exists(),
    "kernel-3945 fixture or bramble_ops.py not present",
)
class BuildPRRecordMiddleRoundFoldTests(unittest.TestCase):
    """A github comment authored during a middle round must not be dropped."""

    def test_middle_round_comment_folds_onto_final(self):
        # kernel-3945 had 5 rounds -> harvested rounds are {1, 5}. A fetched
        # comment with no matching round time (unmapped repo) folds onto r1;
        # to exercise the *final*-fold path we attribute via no_timestamp
        # fallback is not enough -> use a synthetic comment whose verdict was
        # recorded in a middle round so the state fallback attributes it there.
        # Simplest deterministic check: unmapped fetch -> all fold onto r1,
        # and the comment is present (never dropped).
        fetched = [
            {
                "id": 77777,
                "source": "github-inline",
                "author": "cursor[bot]",
                "is_bot": True,
                "path": "x.py",
                "line": 1,
                "body": "synthetic middle-round comment",
                "created_at": "2026-05-07T07:00:00Z",
                "original_commit_id": "abc",
            }
        ]
        record = hl.build_pr_record(
            KERNEL_3945_DIR,
            "kernel",
            "3945",
            repo_map=hl.RepoMap(),  # unmapped -> unmapped_repo_fallback
            pr_summary=None,
            harvester_sha="t",
            harvested_at="2026-05-20T00:00:00Z",
            bramble_ops_path=BRAMBLE_OPS_PATH,
            fetched_pr_comments=fetched,
            fetch_attempted=True,
        )
        self.assertIsNotNone(record)
        self.assertEqual(
            record.pr_comments_attribution_basis, "unmapped_repo_fallback"
        )
        # The synthetic comment must appear exactly once across harvested rounds.
        all_ids = [
            c.get("comment_id")
            for hr in record.harvested_rounds
            for c in hr.raw_comment_actions
        ]
        self.assertEqual(all_ids.count(77777), 1)
        # Non-github comment_actions still present on their own rounds.
        for hr in record.harvested_rounds:
            for c in hr.raw_comment_actions:
                if c.get("source") not in hl.GITHUB_SOURCES:
                    # Reviewer findings keep their original schema (no
                    # attributed_round key).
                    self.assertNotIn("attributed_round", c)


class FetchPRCommentsTests(unittest.TestCase):
    """fetch_pr_comments with gh stubbed via monkeypatched subprocess.run."""

    def _run_with_stub(self, responses):
        """responses: dict endpoint-substring -> JSON-serializable payload."""
        import subprocess as _sp

        real_run = hl.subprocess.run

        def fake_run(cmd, *a, **kw):
            endpoint = cmd[-1]  # repos/<slug>/<endpoint>
            for key, payload in responses.items():
                if key in endpoint:
                    return _sp.CompletedProcess(
                        cmd, 0, stdout=json.dumps(payload), stderr=""
                    )
            return _sp.CompletedProcess(cmd, 0, stdout="[]", stderr="")

        hl.subprocess.run = fake_run
        try:
            return hl.fetch_pr_comments("org/repo", "236")
        finally:
            hl.subprocess.run = real_run

    def test_classifies_three_sources(self):
        comments, err = self._run_with_stub(
            {
                "pulls/236/comments": [
                    {
                        "id": 1,
                        "user": {"login": "cursor[bot]", "type": "Bot"},
                        "path": "a.py",
                        "line": 9,
                        "body": "inline issue",
                        "created_at": "2026-05-07T00:00:00Z",
                        "original_commit_id": "c1",
                    },
                    {
                        "id": 2,
                        "in_reply_to_id": 1,  # reply -> dropped
                        "user": {"login": "human", "type": "User"},
                        "body": "reply",
                        "created_at": "2026-05-07T01:00:00Z",
                    },
                ],
                "issues/236/comments": [
                    {
                        "id": 3,
                        "user": {"login": "human", "type": "User"},
                        "body": "top-level note",
                        "created_at": "2026-05-07T02:00:00Z",
                    }
                ],
                "pulls/236/reviews": [
                    {
                        "id": 4,
                        "state": "COMMENTED",
                        "user": {"login": "codex[bot]", "type": "Bot"},
                        "body": "review body",
                        "submitted_at": "2026-05-07T03:00:00Z",
                    },
                    {
                        "id": 5,
                        "state": "APPROVED",  # dropped
                        "user": {"login": "human", "type": "User"},
                        "body": "lgtm",
                        "submitted_at": "2026-05-07T04:00:00Z",
                    },
                    {
                        "id": 6,
                        "state": "COMMENTED",
                        "user": {"login": "human", "type": "User"},
                        "body": "   ",  # empty -> dropped
                        "submitted_at": "2026-05-07T05:00:00Z",
                    },
                ],
            }
        )
        self.assertIsNone(err)
        ids = {c["id"]: c for c in comments}
        self.assertEqual(set(ids), {1, 3, 4})  # reply, APPROVED, empty dropped
        self.assertEqual(ids[1]["source"], "github-inline")
        self.assertEqual(ids[1]["original_commit_id"], "c1")
        self.assertEqual(ids[3]["source"], "github-issue")
        self.assertEqual(ids[4]["source"], "github-review")
        self.assertEqual(ids[4]["created_at"], "2026-05-07T03:00:00Z")

    def test_missing_slug(self):
        comments, err = hl.fetch_pr_comments("", "236")
        self.assertEqual(comments, [])
        self.assertIsNotNone(err)


class BuildIndexTests(unittest.TestCase):
    def test_index_shape(self):
        # Minimal record to feed the index builder.
        rec = hl.PRRecord(
            schema_version=2,
            harvested_at="2026-05-20T00:00:00Z",
            harvester_git_sha="abc",
            pr={
                "repo_name": "kernel",
                "repo_url": "https://github.com/anthropics/kernel",
                "pr_number": "3945",
                "pr_url": "https://github.com/anthropics/kernel/pull/3945",
                "branch": None,
                "started_at": "2026-05-16T00:34:34Z",
                "completed": True,
                "exit_reason": "converged",
                "total_rounds": 5,
            },
            pr_comments_attribution_basis="created_at",
            pr_comments_fetch_error=None,
            harvested_rounds=[],
        )
        index = hl.build_index(
            [rec], generated_at="2026-05-20T00:00:00Z", harvester_sha="abc"
        )
        self.assertEqual(index["schema_version"], 2)
        self.assertEqual(len(index["prs"]), 1)
        self.assertEqual(index["prs"][0]["pr_number"], "3945")
        self.assertEqual(index["prs"][0]["file"], "kernel-3945.json")
        self.assertEqual(index["prs"][0]["harvested_rounds"], 0)


if __name__ == "__main__":
    unittest.main()
