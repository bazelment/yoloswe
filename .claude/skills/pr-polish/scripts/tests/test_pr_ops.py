"""Unit tests for pr_ops. Hermetic: no gh/git/network.

Run with:
    python3 -m unittest discover -v   # from ~/.claude/skills/pr-polish/scripts
"""

from __future__ import annotations

import json
import os
import subprocess
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
import pr_ops  # noqa: E402


class TestClassifyComments(unittest.TestCase):
    """Pure function: no mocks needed."""

    def _user(self, login: str, is_bot: bool = False) -> dict:
        return {"login": login, "type": "Bot" if is_bot else "User"}

    def test_inline_with_reply_is_tagged_but_reply_count_nonzero(self) -> None:
        parent = {
            "id": 100,
            "user": self._user("botx", is_bot=True),
            "path": "a.py",
            "line": 5,
            "body": "fix this",
            "in_reply_to_id": None,
            "created_at": "2026-04-19T00:00:00Z",
        }
        reply = {
            "id": 101,
            "user": self._user("mzhaom"),
            "path": "a.py",
            "line": 5,
            "body": "fixed in abc",
            "in_reply_to_id": 100,
            "created_at": "2026-04-19T00:05:00Z",
        }
        kept, noise = pr_ops.classify_comments([parent, reply], [], [])
        self.assertEqual(len(kept), 1)
        self.assertEqual(kept[0]["id"], 100)
        self.assertEqual(kept[0]["source"], pr_ops.SOURCE_INLINE)
        self.assertEqual(kept[0]["reply_count"], 1)
        self.assertTrue(kept[0]["is_bot"])
        self.assertEqual(noise, [])

    def test_inline_comment_carries_original_commit_id(self) -> None:
        # bramble_ops.triage routes is_stale_prior_commit comments to a
        # dedicated bucket. classify_comments must surface the original SHA
        # so fetch_comments can compute the flag against pr["head_sha"].
        inline = {
            "id": 100,
            "user": self._user("cursor[bot]", is_bot=True),
            "path": "a.py",
            "line": 5,
            "body": "fix this",
            "in_reply_to_id": None,
            "created_at": "t1",
            "original_commit_id": "deadbeefcafebabe1234567890abcdef00000000",
        }
        kept, _ = pr_ops.classify_comments([inline], [], [])
        self.assertEqual(len(kept), 1)
        self.assertEqual(
            kept[0]["original_commit_id"],
            "deadbeefcafebabe1234567890abcdef00000000",
        )

    def test_issue_and_review_tagging(self) -> None:
        issue = [
            {
                "id": 1,
                "user": self._user("claude", is_bot=True),
                "body": "LGTM-ish",
                "created_at": "t1",
            }
        ]
        review_kept = {
            "id": 2,
            "user": self._user("alice"),
            "state": "CHANGES_REQUESTED",
            "body": "please change X",
            "submitted_at": "t2",
        }
        review_dropped_state = {
            "id": 3,
            "user": self._user("alice"),
            "state": "APPROVED",
            "body": "ok",
            "submitted_at": "t3",
        }
        review_dropped_body = {
            "id": 4,
            "user": self._user("alice"),
            "state": "COMMENTED",
            "body": "",
            "submitted_at": "t4",
        }
        kept, _ = pr_ops.classify_comments(
            [], issue, [review_kept, review_dropped_state, review_dropped_body]
        )
        kinds = [(c["id"], c["source"]) for c in kept]
        self.assertIn((1, pr_ops.SOURCE_ISSUE), kinds)
        self.assertIn((2, pr_ops.SOURCE_REVIEW), kinds)
        self.assertNotIn(3, [c["id"] for c in kept])
        self.assertNotIn(4, [c["id"] for c in kept])


class TestBotProcessNoiseFilter(unittest.TestCase):
    """Bot linkbacks and progress posts are noise, not findings.

    They're dropped into the second return value of classify_comments so
    the orchestrator can log a count + samples without polluting
    comment_actions with bogus false_positive entries.
    """

    def _user(self, login: str, is_bot: bool = False) -> dict:
        return {"login": login, "type": "Bot" if is_bot else "User"}

    def test_linear_linkback_issue_comment_is_filtered(self) -> None:
        issue = [
            {
                "id": 4300306871,
                "user": self._user("linear[bot]", is_bot=True),
                "body": (
                    "<!-- linear-linkback -->\n<details>\n<summary>"
                    "<a href='https://linear.app/...'>INF-448</a></summary>\n"
                ),
                "created_at": "t1",
            }
        ]
        kept, noise = pr_ops.classify_comments([], issue, [])
        self.assertEqual(kept, [])
        self.assertEqual(len(noise), 1)
        self.assertEqual(noise[0]["id"], 4300306871)
        self.assertEqual(noise[0]["author"], "linear[bot]")
        self.assertEqual(noise[0]["pattern"], "linear-linkback")

    def test_claude_progress_issue_comment_is_filtered(self) -> None:
        issue = [
            {
                "id": 4300307985,
                "user": self._user("claude[bot]", is_bot=True),
                "body": (
                    "Reviewing PR...\n\n- [ ] Gather diff\n- [ ] Review\n\n"
                    "[View job run](https://github.com/...)"
                ),
                "created_at": "t1",
            }
        ]
        kept, noise = pr_ops.classify_comments([], issue, [])
        self.assertEqual(kept, [])
        self.assertEqual(len(noise), 1)
        self.assertEqual(noise[0]["pattern"], "claude-progress")

    def test_human_quoting_noise_strings_is_kept(self) -> None:
        # A human explaining the noise patterns MUST NOT be dropped.
        issue = [
            {
                "id": 777,
                "user": self._user("mzhaom", is_bot=False),
                "body": (
                    "fyi the linear-linkback HTML comment marker means the "
                    "bot will auto-link this PR. Reviewing PR... is the other noisy one."
                ),
                "created_at": "t1",
            }
        ]
        kept, noise = pr_ops.classify_comments([], issue, [])
        self.assertEqual(len(kept), 1)
        self.assertEqual(kept[0]["id"], 777)
        self.assertEqual(noise, [])

    def test_fetch_comments_returns_wrapped_shape(self) -> None:
        issue = [
            {
                "id": 1,
                "user": self._user("linear[bot]", is_bot=True),
                "body": "<!-- linear-linkback --> stuff",
                "created_at": "t",
            },
            {
                "id": 2,
                "user": self._user("alice", is_bot=False),
                "body": "real comment",
                "created_at": "t",
            },
        ]
        with (
            patch.object(pr_ops, "_fetch_inline_comments", return_value=[]),
            patch.object(pr_ops, "_fetch_issue_comments", return_value=issue),
            patch.object(pr_ops, "_fetch_reviews", return_value=[]),
        ):
            got = pr_ops.fetch_comments({"owner_repo": "o/r", "pr_number": 1})
        self.assertEqual(len(got["comments"]), 1)
        self.assertEqual(got["comments"][0]["id"], 2)
        self.assertEqual(got["noise_filtered"], 1)
        self.assertEqual(len(got["noise_samples"]), 1)

    def test_noise_samples_capped(self) -> None:
        # Cap defense-in-depth: shouldn't emit more than _NOISE_SAMPLE_CAP (5).
        issues = [
            {
                "id": i,
                "user": self._user("linear[bot]", is_bot=True),
                "body": "<!-- linear-linkback --> stuff",
                "created_at": "t",
            }
            for i in range(10)
        ]
        with (
            patch.object(pr_ops, "_fetch_inline_comments", return_value=[]),
            patch.object(pr_ops, "_fetch_issue_comments", return_value=issues),
            patch.object(pr_ops, "_fetch_reviews", return_value=[]),
        ):
            got = pr_ops.fetch_comments({"owner_repo": "o/r", "pr_number": 1})
        self.assertEqual(got["noise_filtered"], 10)
        self.assertEqual(len(got["noise_samples"]), 5)


class TestStateLifecycle(unittest.TestCase):
    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self._patch_paths()

    def _patch_paths(self) -> None:
        tmp_root = Path(self.tmp.name)

        def fake_state_paths(pr, branch=None):
            key = pr if pr is not None else f"branch-{branch}"
            d = tmp_root / f"proj-{key}"
            d.mkdir(parents=True, exist_ok=True)
            return d, d / "pr-polish-state.json"

        patcher = patch.object(pr_ops, "state_paths", side_effect=fake_state_paths)
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_append_round_creates_file(self) -> None:
        state = pr_ops.state_append_round(42, 1, "abc123", verify_head=False)
        self.assertEqual(state["pr_number"], 42)
        self.assertEqual(state["current_round"], 1)
        self.assertEqual(state["rounds"][0]["head_before"], "abc123")
        # New round defaults: noise fields present, zeroed.
        self.assertEqual(state["rounds"][0]["noise_filtered"], 0)
        self.assertEqual(state["rounds"][0]["noise_samples"], [])

    def test_append_round_persists_noise_counter(self) -> None:
        samples = [{"id": 1, "author": "linear[bot]", "pattern": "linear-linkback"}]
        state = pr_ops.state_append_round(
            42, 1, "abc123", verify_head=False, noise_filtered=2, noise_samples=samples
        )
        rnd = state["rounds"][0]
        self.assertEqual(rnd["noise_filtered"], 2)
        self.assertEqual(rnd["noise_samples"], samples)

    def test_append_same_round_keeps_max_noise_counter(self) -> None:
        # Re-invocation after compaction should not zero out an earlier non-zero count.
        pr_ops.state_append_round(42, 1, "a", verify_head=False, noise_filtered=3)
        state = pr_ops.state_append_round(42, 1, "a", verify_head=False, noise_filtered=0)
        self.assertEqual(state["rounds"][0]["noise_filtered"], 3)

    def test_append_round_accepts_branch_ctx(self) -> None:
        state = pr_ops.state_append_round("branch:foo-bar", 1, "abc", verify_head=False)
        self.assertIsNone(state["pr_number"])
        self.assertEqual(state["branch"], "foo-bar")

    def test_append_same_round_refreshes_head_before(self) -> None:
        pr_ops.state_append_round(42, 1, "abc123", verify_head=False)
        state = pr_ops.state_append_round(42, 1, "def456", verify_head=False)
        self.assertEqual(state["rounds"][0]["head_before"], "def456")
        self.assertEqual(len(state["rounds"]), 1)

    def test_finalize_recomputes_counts_and_top_severity(self) -> None:
        pr_ops.state_append_round(42, 1, "abc123", verify_head=False)
        actions = [
            {
                "comment_id": 1,
                "source": "codex",
                "severity": "medium",
                "action": "fixed",
                "commit_sha": "x",
            },
            {
                "comment_id": 2,
                "source": "cursor",
                "severity": "high",
                "action": "fixed",
                "commit_sha": "x",
            },
            {
                "comment_id": 3,
                "source": "cursor",
                "severity": "low",
                "action": "false_positive",
                "reason": "r",
            },
            {"comment_id": 4, "source": "cursor", "severity": "low", "action": "ack"},
        ]
        state = pr_ops.state_finalize_round(42, 1, "def456", actions)
        rnd = state["rounds"][0]
        self.assertEqual(rnd["head_after"], "def456")
        self.assertEqual(rnd["fixed_count"], 2)
        # ``ack`` joins ``false_positive`` in SKIPPED_ACTIONS; both count.
        self.assertEqual(rnd["skipped_count"], 2)
        self.assertEqual(rnd["top_severity"], "high")
        self.assertEqual(len(rnd["comment_actions"]), 4)

    def test_finalize_dedupes_by_comment_id(self) -> None:
        pr_ops.state_append_round(42, 1, "abc", verify_head=False)
        pr_ops.state_finalize_round(
            42, 1, "def", [{"comment_id": 1, "action": "fixed", "severity": "low"}]
        )
        state = pr_ops.state_finalize_round(
            42, 1, "def", [{"comment_id": 1, "action": "fixed", "severity": "high"}]
        )
        rnd = state["rounds"][0]
        self.assertEqual(len(rnd["comment_actions"]), 1)
        self.assertEqual(rnd["comment_actions"][0]["severity"], "high")  # new wins

    def test_mark_complete_preserves_file(self) -> None:
        pr_ops.state_append_round(42, 1, "abc", verify_head=False)
        pr_ops.state_mark_complete(42, "both-accepted")
        _, path = pr_ops.state_paths(42)
        self.assertTrue(path.exists(), "state file must NOT be deleted on mark-complete")
        with path.open() as f:
            state = json.load(f)
        self.assertTrue(state["completed"])
        self.assertEqual(state["exit_reason"], "both-accepted")
        self.assertIn("completed_at", state)

    def test_finalize_without_append_raises(self) -> None:
        with self.assertRaises(RuntimeError):
            pr_ops.state_finalize_round(99, 1, "x", [])

    def test_append_new_round_after_completion_resets_completed_flag(self) -> None:
        # When pr-polish re-runs on a state file from a prior converged
        # loop, the new state-append-round call must clear completed/
        # exit_reason/completed_at — otherwise the mid-loop state file
        # is "current_round=2 AND completed: converged at <prior ts>",
        # which is contradictory and confused this session's run logs.
        # state-mark-complete will set them again at the new loop's exit.
        pr_ops.state_append_round(42, 1, "sha1", verify_head=False)
        pr_ops.state_mark_complete(42, "converged")
        _, path = pr_ops.state_paths(42)
        with path.open() as f:
            state_before = json.load(f)
        self.assertTrue(state_before["completed"])
        self.assertEqual(state_before["exit_reason"], "converged")
        self.assertIsNotNone(state_before.get("completed_at"))

        state = pr_ops.state_append_round(42, 2, "sha2", verify_head=False)
        self.assertFalse(state["completed"], "new round must clear stale completed flag")
        self.assertIsNone(state.get("exit_reason"), "new round must clear stale exit_reason")
        self.assertIsNone(state.get("completed_at"), "new round must clear stale completed_at")
        # Old round entry preserved; new one appended.
        self.assertEqual(len(state["rounds"]), 2)
        self.assertEqual(state["current_round"], 2)


class TestLowOnlyStreak(unittest.TestCase):
    """`low_only_streak` powers the streak-based convergence rule and the
    reviewer-pressure goal sentence (B1). Test the increment/reset shape
    directly through the unit helper plus the live finalize path so a
    state-shape regression surfaces here instead of leaking into a real run.
    """

    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        tmp_root = Path(self.tmp.name)

        def fake_state_paths(pr, branch=None):
            key = pr if pr is not None else f"branch-{branch}"
            d = tmp_root / f"proj-{key}"
            d.mkdir(parents=True, exist_ok=True)
            return d, d / "pr-polish-state.json"

        patcher = patch.object(pr_ops, "state_paths", side_effect=fake_state_paths)
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_unit_increments_when_top_severity_low(self) -> None:
        prior = [{"n": 1, "low_only_streak": 1, "top_severity": "low"}]
        self.assertEqual(pr_ops._compute_low_only_streak(prior, "low"), 2)
        self.assertEqual(pr_ops._compute_low_only_streak(prior, "nit"), 2)
        # ``None`` top_severity (zero findings) counts as low-only too.
        self.assertEqual(pr_ops._compute_low_only_streak(prior, None), 2)

    def test_unit_resets_when_medium_or_higher(self) -> None:
        prior = [{"n": 1, "low_only_streak": 5, "top_severity": "low"}]
        self.assertEqual(pr_ops._compute_low_only_streak(prior, "medium"), 0)
        self.assertEqual(pr_ops._compute_low_only_streak(prior, "high"), 0)
        self.assertEqual(pr_ops._compute_low_only_streak(prior, "critical"), 0)

    def test_unit_round_one_low_only_starts_at_one(self) -> None:
        self.assertEqual(pr_ops._compute_low_only_streak([], "low"), 1)
        self.assertEqual(pr_ops._compute_low_only_streak([], None), 1)

    def test_unit_round_one_high_starts_at_zero(self) -> None:
        self.assertEqual(pr_ops._compute_low_only_streak([], "high"), 0)

    def test_finalize_persists_low_only_streak(self) -> None:
        pr_ops.state_append_round(42, 1, "abc", verify_head=False)
        state = pr_ops.state_finalize_round(
            42,
            1,
            "def",
            [{"comment_id": 1, "action": "ack", "severity": "low"}],
        )
        self.assertEqual(state["rounds"][0]["low_only_streak"], 1)

    def test_finalize_increments_across_consecutive_low_rounds(self) -> None:
        pr_ops.state_append_round(42, 1, "sha1", verify_head=False)
        pr_ops.state_finalize_round(
            42, 1, "sha1f",
            [{"comment_id": 1, "action": "ack", "severity": "low"}],
        )
        pr_ops.state_append_round(42, 2, "sha1f", verify_head=False)
        state = pr_ops.state_finalize_round(
            42, 2, "sha2f",
            [{"comment_id": 2, "action": "ack", "severity": "nit"}],
        )
        self.assertEqual(state["rounds"][0]["low_only_streak"], 1)
        self.assertEqual(state["rounds"][1]["low_only_streak"], 2)

    def test_finalize_resets_when_medium_lands(self) -> None:
        pr_ops.state_append_round(42, 1, "sha1", verify_head=False)
        pr_ops.state_finalize_round(
            42, 1, "sha1f",
            [{"comment_id": 1, "action": "ack", "severity": "low"}],
        )
        pr_ops.state_append_round(42, 2, "sha1f", verify_head=False)
        state = pr_ops.state_finalize_round(
            42, 2, "sha2f",
            [{"comment_id": 2, "action": "fixed", "severity": "medium",
              "commit_sha": "sha2f"}],
        )
        self.assertEqual(state["rounds"][1]["low_only_streak"], 0)

    def test_backfills_streak_from_top_severity_when_field_missing(self) -> None:
        """An in-progress state file written by a pre-streak orchestrator
        won't have ``low_only_streak`` on its rounds. The next finalize
        must reconstruct the streak from ``top_severity`` history so the
        new convergence shortcut and pressure note trigger correctly
        instead of waiting for two fresh low rounds to accumulate.
        """
        # Two prior low-only rounds with no streak field — simulates state
        # written by the pre-this-feature orchestrator.
        prior = [
            {"n": 1, "top_severity": "low"},
            {"n": 2, "top_severity": "nit"},
        ]
        # This round is also low — streak should be 3 (2 + 1), not 1.
        self.assertEqual(pr_ops._compute_low_only_streak(prior, "low"), 3)

    def test_backfill_resets_at_first_non_low_walking_back(self) -> None:
        """Backfill walks the top_severity ladder backwards and stops at
        the first medium/high. A history of [high, low, low] ending in
        low-only continues at streak=2, not 3.
        """
        prior = [
            {"n": 1, "top_severity": "high"},
            {"n": 2, "top_severity": "low"},
            {"n": 3, "top_severity": "low"},
        ]
        # Most recent is low; walking back: low (n=3), low (n=2), high
        # (n=1 — stop). prev_streak from backfill = 2; this round adds 1.
        self.assertEqual(pr_ops._compute_low_only_streak(prior, "low"), 3)

    def test_backfill_unit(self) -> None:
        # Empty -> 0
        self.assertEqual(pr_ops._backfill_low_only_streak([]), 0)
        # All low -> count all
        self.assertEqual(
            pr_ops._backfill_low_only_streak(
                [{"n": 1, "top_severity": "low"},
                 {"n": 2, "top_severity": "nit"},
                 {"n": 3, "top_severity": None}],
            ),
            3,
        )
        # Most recent is medium -> 0 (the streak ended at the most recent round)
        self.assertEqual(
            pr_ops._backfill_low_only_streak(
                [{"n": 1, "top_severity": "low"},
                 {"n": 2, "top_severity": "medium"}],
            ),
            0,
        )

    def test_finalize_zero_findings_counts_as_low_only(self) -> None:
        # Zero findings -> top_severity is None -> still counts as low-only,
        # so the streak increments. A single zero-finding round is what
        # "converged" feels like; the convergence rule treats two of these
        # in a row as definite.
        pr_ops.state_append_round(42, 1, "sha1", verify_head=False)
        state = pr_ops.state_finalize_round(42, 1, "sha1f", [])
        self.assertIsNone(state["rounds"][0]["top_severity"])
        self.assertEqual(state["rounds"][0]["low_only_streak"], 1)


class TestStateFirstRoundOfSeries(unittest.TestCase):
    """state_load decorates state with is_first_round_of_series.

    A new "series" starts when there's no state, the prior loop set
    completed=true (any exit_reason), or this is round 1. The orchestrator
    uses the field to decide whether to re-fetch PR comments + CI failures
    and to skip bramble session resume on a fresh audit.
    """

    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        tmp_root = Path(self.tmp.name)

        def fake_state_paths(pr, branch=None):
            key = pr if pr is not None else f"branch-{branch}"
            d = tmp_root / f"proj-{key}"
            d.mkdir(parents=True, exist_ok=True)
            return d, d / "pr-polish-state.json"

        patcher = patch.object(pr_ops, "state_paths", side_effect=fake_state_paths)
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_no_state_emits_true(self) -> None:
        out = pr_ops.state_load(42)
        # Empty state → no derived field at all (state_load returns {} so
        # the orchestrator's `state-is-new-series` CLI is the canonical
        # query). Helper directly:
        self.assertTrue(pr_ops._is_first_round_of_series(None, 1))

    def test_completed_state_emits_true(self) -> None:
        # Prior loop converged; round 6 is a new series.
        pr_ops.state_append_round(42, 1, "sha", verify_head=False)
        pr_ops.state_mark_complete(42, "converged")
        loaded = pr_ops.state_load(42)
        self.assertTrue(loaded["is_first_round_of_series"])

    def test_in_progress_state_emits_false(self) -> None:
        # Mid-series round 2: completed is false, prior round exists.
        pr_ops.state_append_round(42, 1, "sha1", verify_head=False)
        pr_ops.state_append_round(42, 2, "sha2", verify_head=False)
        loaded = pr_ops.state_load(42)
        self.assertFalse(loaded["is_first_round_of_series"])

    def test_state_is_new_series_helper_three_cases(self) -> None:
        # Direct unit coverage of the helper, decoupled from state_paths.
        self.assertTrue(pr_ops._is_first_round_of_series(None, 1))
        self.assertTrue(pr_ops._is_first_round_of_series({"rounds": []}, 1))
        self.assertTrue(
            pr_ops._is_first_round_of_series(
                {"rounds": [{"n": 5}], "completed": True}, 6
            )
        )
        self.assertFalse(
            pr_ops._is_first_round_of_series(
                {"rounds": [{"n": 1}], "completed": False}, 2
            )
        )

    def test_state_is_new_series_cli(self) -> None:
        # SKILL Step 0.5 invokes this CLI directly. Cover the argparse +
        # dispatch path so a typo in the parser regresses loudly.
        import io  # noqa: PLC0415
        from contextlib import redirect_stdout  # noqa: PLC0415

        # Three-case fixture across two PRs to exercise the dispatch.
        # Mid-series state for PR 42:
        pr_ops.state_append_round(42, 1, "sha", verify_head=False)
        pr_ops.state_append_round(42, 2, "sha2", verify_head=False)

        # Completed state for PR 99:
        pr_ops.state_append_round(99, 1, "sha", verify_head=False)
        pr_ops.state_mark_complete(99, "converged")

        def _run(*argv) -> str:
            buf = io.StringIO()
            with redirect_stdout(buf):
                rc = pr_ops.main(list(argv))
            self.assertEqual(rc, 0, f"main exited non-zero; stdout={buf.getvalue()!r}")
            return buf.getvalue().rstrip("\n")

        # Brand new PR (no state) → 1
        self.assertEqual(_run("state-is-new-series", "1234", "1"), "1")
        # Completed prior series → 1
        self.assertEqual(_run("state-is-new-series", "99", "2"), "1")
        # Mid-series → 0
        self.assertEqual(_run("state-is-new-series", "42", "3"), "0")


class TestHeartbeatTelemetry(unittest.TestCase):
    """Distinguish abandoned runs from interrupted ones.

    The 50-state-file analysis showed 4/50 runs ended with
    ``completed: false, exit_reason: null`` — we couldn't tell user-paused
    from crashed. Heartbeat fixes that: every ``state_append_round`` stamps
    ``last_heartbeat_at``; ``state_load`` returns ``is_heartbeat_stale``;
    ``state_mark_abandoned`` writes the tombstone.
    """

    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        tmp_root = Path(self.tmp.name)

        def fake_state_paths(pr, branch=None):
            key = pr if pr is not None else f"branch-{branch}"
            d = tmp_root / f"proj-{key}"
            d.mkdir(parents=True, exist_ok=True)
            return d, d / "pr-polish-state.json"

        patcher = patch.object(pr_ops, "state_paths", side_effect=fake_state_paths)
        patcher.start()
        self.addCleanup(patcher.stop)

    def test_append_round_stamps_heartbeat(self) -> None:
        # Pre-heartbeat state files lacked the field entirely; we want every
        # new state file to carry a heartbeat from round 1, since Step 0.5
        # uses its presence as the only reliable liveness signal.
        state = pr_ops.state_append_round(42, 1, "abc", verify_head=False)
        self.assertIn("last_heartbeat_at", state)
        # Format must round-trip with state_load's parser (UTC ISO).
        self.assertRegex(
            state["last_heartbeat_at"], r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}Z$"
        )

    def test_state_load_marks_fresh_heartbeat_not_stale(self) -> None:
        pr_ops.state_append_round(42, 1, "abc", verify_head=False)
        loaded = pr_ops.state_load(42)
        self.assertFalse(loaded["is_heartbeat_stale"])

    def test_state_load_marks_old_heartbeat_stale(self) -> None:
        # Hand-edit the state file to backdate the heartbeat past the
        # threshold. This is exactly what an abandoned run looks like on
        # disk: completed=false, heartbeat from 3h ago.
        pr_ops.state_append_round(42, 1, "abc", verify_head=False)
        _, path = pr_ops.state_paths(42)
        with path.open() as f:
            state = json.load(f)
        # 3 hours ago is well past the 2-hour staleness threshold.
        from datetime import UTC as _UTC
        from datetime import datetime, timedelta

        old = (datetime.now(_UTC) - timedelta(hours=3)).strftime("%Y-%m-%dT%H:%M:%SZ")
        state["last_heartbeat_at"] = old
        with path.open("w") as f:
            json.dump(state, f)
        loaded = pr_ops.state_load(42)
        self.assertTrue(loaded["is_heartbeat_stale"])

    def test_state_load_completed_run_is_never_stale(self) -> None:
        # Even if heartbeat is ancient, a completed run is final. Don't
        # flag historical state files as stale (we'd churn the whole audit
        # trail directory).
        pr_ops.state_append_round(42, 1, "abc", verify_head=False)
        pr_ops.state_mark_complete(42, "converged")
        # Forcibly age the heartbeat to confirm completed wins.
        _, path = pr_ops.state_paths(42)
        with path.open() as f:
            state = json.load(f)
        state["last_heartbeat_at"] = "2020-01-01T00:00:00Z"
        with path.open("w") as f:
            json.dump(state, f)
        loaded = pr_ops.state_load(42)
        self.assertFalse(loaded["is_heartbeat_stale"])

    def test_state_load_treats_missing_heartbeat_as_stale(self) -> None:
        # Old state files (kernel-2755 etc.) predate the heartbeat field.
        # On resume we must not wedge — a missing heartbeat on an in-progress
        # run is treated as stale so the orchestrator falls through to a
        # fresh start instead of pretending to resume forever.
        _, path = pr_ops.state_paths(42)
        path.parent.mkdir(parents=True, exist_ok=True)
        with path.open("w") as f:
            json.dump({"pr_number": 42, "rounds": [], "current_round": 1}, f)
        loaded = pr_ops.state_load(42)
        self.assertTrue(loaded["is_heartbeat_stale"])

    def test_mark_abandoned_tombstones_with_exit_reason(self) -> None:
        # Records the run as completed with exit_reason="abandoned" so future
        # state-file analyses can distinguish user-paused from abandoned.
        pr_ops.state_append_round(42, 1, "abc", verify_head=False)
        state = pr_ops.state_mark_abandoned(42)
        self.assertTrue(state["completed"])
        self.assertEqual(state["exit_reason"], "abandoned")
        self.assertIn("completed_at", state)
        # The heartbeat-stale derivation flips to False once completed=true.
        loaded = pr_ops.state_load(42)
        self.assertFalse(loaded["is_heartbeat_stale"])


class TestAtomicWrite(unittest.TestCase):
    def test_write_then_read_roundtrip(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "out.json"
            _common.atomic_write_json(p, {"x": 1})
            self.assertEqual(json.loads(p.read_text()), {"x": 1})

    def test_crash_between_write_and_rename_leaves_old_file_intact(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "out.json"
            _common.atomic_write_json(p, {"v": 1})

            # Force os.replace to fail; old file should survive with v=1.
            original_replace = os.replace

            def boom(src: str, dst: str) -> None:
                raise OSError("simulated crash")

            with patch("os.replace", side_effect=boom), self.assertRaises(OSError):
                _common.atomic_write_json(p, {"v": 2})

            self.assertEqual(json.loads(p.read_text()), {"v": 1})
            leftovers = [
                x
                for x in Path(d).iterdir()
                if x.name.startswith(".out.json.") and x.name.endswith(".tmp")
            ]
            self.assertEqual(leftovers, [], f"expected no .tmp leftovers, got {leftovers}")
            self.assertIs(os.replace, original_replace)


class TestIdentifyPR(unittest.TestCase):
    def _with_state(self, body):
        with tempfile.TemporaryDirectory() as d:
            tmp_root = Path(d)

            def fake_state_paths(pr, branch=None):
                key = pr if pr is not None else f"branch-{branch}"
                pr_dir = tmp_root / f"proj-{key}"
                return pr_dir, pr_dir / "pr-polish-state.json"

            with patch.object(pr_ops, "state_paths", side_effect=fake_state_paths):
                return body()

    def test_identify_with_pr(self) -> None:
        pr_json = json.dumps(
            {
                "pr_number": 2443,
                "title": "perf(forge-full-coder): reuse planner sandbox",
                "url": "https://github.com/sycamore-labs/kernel/pull/2443",
                "base": "main",
                "head": "feature/PLA-287",
                "head_sha": "abc123def456",
            }
        )

        def fake_run(cmd, **kwargs):
            if cmd[:2] == ["git", "rev-parse"]:
                return _common.RunResult(stdout="feature/PLA-287\n", stderr="", returncode=0)
            if cmd[:3] == ["gh", "pr", "view"]:
                return _common.RunResult(stdout=pr_json, stderr="", returncode=0)
            if cmd[:3] == ["gh", "repo", "view"]:
                return _common.RunResult(stdout='"sycamore-labs/kernel"', stderr="", returncode=0)
            raise AssertionError(f"unexpected cmd: {cmd}")

        with (
            patch.object(pr_ops, "run", side_effect=fake_run),
            patch.object(_common, "run", side_effect=fake_run),
        ):
            out = self._with_state(lambda: pr_ops.identify_pr())
        self.assertEqual(out["pr_number"], 2443)
        self.assertEqual(out["owner"], "sycamore-labs")
        self.assertEqual(out["repo"], "kernel")
        self.assertEqual(out["branch"], "feature/PLA-287")
        self.assertEqual(out["head_sha"], "abc123def456")
        self.assertTrue(out["state_file"].endswith("pr-polish-state.json"))

    def test_identify_without_pr_returns_branch_only(self) -> None:
        def fake_run(cmd, **kwargs):
            if cmd[:2] == ["git", "rev-parse"]:
                return _common.RunResult(stdout="feature/new-idea\n", stderr="", returncode=0)
            if cmd[:3] == ["gh", "pr", "view"]:
                # gh exits non-zero when the branch has no PR.
                return _common.RunResult(stdout="", stderr="no pull requests found", returncode=1)
            if cmd[:3] == ["gh", "repo", "view"]:
                return _common.RunResult(stdout='"sycamore-labs/kernel"', stderr="", returncode=0)
            if cmd[:2] == ["git", "symbolic-ref"]:
                return _common.RunResult(
                    stdout="refs/remotes/origin/main\n", stderr="", returncode=0
                )
            raise AssertionError(f"unexpected cmd: {cmd}")

        with (
            patch.object(pr_ops, "run", side_effect=fake_run),
            patch.object(_common, "run", side_effect=fake_run),
        ):
            out = self._with_state(lambda: pr_ops.identify_pr())
        self.assertIsNone(out["pr_number"])
        self.assertEqual(out["branch"], "feature/new-idea")
        self.assertEqual(out["base"], "main")
        self.assertEqual(out["owner"], "sycamore-labs")
        self.assertEqual(out["repo"], "kernel")
        # Branch-only mode: no PR, so no head SHA — downstream consumers
        # treat None as "cannot prove staleness" and never flag a comment.
        self.assertIsNone(out["head_sha"])


class TestFetchComments(unittest.TestCase):
    def test_merges_three_endpoints(self) -> None:
        inline = [
            {
                "id": 10,
                "user": {"login": "bot", "type": "Bot"},
                "path": "f.py",
                "line": 1,
                "body": "fix",
                "in_reply_to_id": None,
                "created_at": "t1",
            }
        ]
        issue = [
            {
                "id": 20,
                "user": {"login": "claude", "type": "Bot"},
                "body": "top",
                "created_at": "t2",
            }
        ]
        review = [
            {
                "id": 30,
                "user": {"login": "alice", "type": "User"},
                "state": "COMMENTED",
                "body": "review body",
                "submitted_at": "t3",
            }
        ]

        def fake_run(cmd, **kwargs):
            url = cmd[-1] if cmd[:2] == ["gh", "api"] else ""
            if "/issues/" in url and url.endswith("/comments"):
                return _common.RunResult(stdout=json.dumps(issue), stderr="", returncode=0)
            if "/pulls/" in url and url.endswith("/comments"):
                return _common.RunResult(stdout=json.dumps(inline), stderr="", returncode=0)
            if url.endswith("/reviews"):
                return _common.RunResult(stdout=json.dumps(review), stderr="", returncode=0)
            raise AssertionError(f"unexpected cmd: {cmd}")

        with patch.object(pr_ops, "run", side_effect=fake_run):
            got = pr_ops.fetch_comments({"owner_repo": "x/y", "pr_number": 1})
        ids = sorted(c["id"] for c in got["comments"])
        self.assertEqual(ids, [10, 20, 30])
        self.assertEqual(got["noise_filtered"], 0)

    def _stale_tag_fixture(self, original_sha: str, head_sha: str | None) -> dict:
        inline = [
            {
                "id": 42,
                "user": {"login": "cursor[bot]", "type": "Bot"},
                "path": "a.py",
                "line": 5,
                "body": "fix this on the old commit",
                "in_reply_to_id": None,
                "created_at": "t1",
                "original_commit_id": original_sha,
            }
        ]

        def fake_run(cmd, **kwargs):
            url = cmd[-1] if cmd[:2] == ["gh", "api"] else ""
            if "/issues/" in url and url.endswith("/comments"):
                return _common.RunResult(stdout="[]", stderr="", returncode=0)
            if "/pulls/" in url and url.endswith("/comments"):
                return _common.RunResult(stdout=json.dumps(inline), stderr="", returncode=0)
            if url.endswith("/reviews"):
                return _common.RunResult(stdout="[]", stderr="", returncode=0)
            raise AssertionError(f"unexpected cmd: {cmd}")

        with patch.object(pr_ops, "run", side_effect=fake_run):
            return pr_ops.fetch_comments(
                {"owner_repo": "x/y", "pr_number": 1, "head_sha": head_sha}
            )

    def test_tags_stale_when_original_commit_id_differs_from_head(self) -> None:
        # The cursor[bot] regression: comments anchored to a superseded
        # commit (PR was force-pushed) must be flagged so triage routes them
        # to stale_prior_commit instead of forming a fresh finding.
        got = self._stale_tag_fixture("oldsha111", "newsha222")
        self.assertEqual(len(got["comments"]), 1)
        self.assertTrue(got["comments"][0]["is_stale_prior_commit"])
        self.assertEqual(got["head_sha"], "newsha222")

    def test_does_not_tag_stale_when_original_commit_matches_head(self) -> None:
        got = self._stale_tag_fixture("samesha", "samesha")
        self.assertEqual(len(got["comments"]), 1)
        self.assertFalse(got["comments"][0]["is_stale_prior_commit"])

    def test_does_not_tag_stale_when_head_sha_unknown(self) -> None:
        # Branch-only mode (no PR) leaves head_sha=None. A missing SHA
        # cannot prove staleness — preserve the bot comment as fresh.
        got = self._stale_tag_fixture("anysha", None)
        self.assertEqual(len(got["comments"]), 1)
        self.assertFalse(got["comments"][0]["is_stale_prior_commit"])

    def test_filters_top_level_bugbot_summary_as_noise(self) -> None:
        # BUGBOT_REVIEW summary often arrives as a top-level issue comment,
        # not a review-level one. Without filtering it here, triage would
        # surface it as a github-review finding and force a hand-classified
        # false_positive every round.
        bot_user = {"login": "cursor[bot]", "type": "Bot"}
        issues = [
            {
                "id": 4240504634,
                "user": bot_user,
                "body": "<!-- BUGBOT_REVIEW -->\nCursor Bugbot has reviewed your changes "
                        "and found 3 potential issues.\n\n<!-- BUGBOT_FIX_ALL -->",
                "created_at": "2026-05-07T00:00:00Z",
            }
        ]

        def fake_run(cmd, **kwargs):
            url = cmd[-1] if cmd[:2] == ["gh", "api"] else ""
            if "/issues/" in url and url.endswith("/comments"):
                return _common.RunResult(stdout=json.dumps(issues), stderr="", returncode=0)
            if "/pulls/" in url and url.endswith("/comments"):
                return _common.RunResult(stdout="[]", stderr="", returncode=0)
            if url.endswith("/reviews"):
                return _common.RunResult(stdout="[]", stderr="", returncode=0)
            raise AssertionError(f"unexpected cmd: {cmd}")

        with patch.object(pr_ops, "run", side_effect=fake_run):
            got = pr_ops.fetch_comments({"owner_repo": "x/y", "pr_number": 1})
        self.assertEqual(got["comments"], [])
        self.assertEqual(got["noise_filtered"], 1)
        self.assertEqual(got["noise_samples"][0]["pattern"], "review-summary")


class TestIsBotReviewSummary(unittest.TestCase):
    """The filter should drop short bugbot boilerplate but keep real reviews."""

    def _bot(self) -> dict:
        return {"login": "cursor[bot]", "type": "Bot"}

    def _human(self) -> dict:
        return {"login": "mzhaom", "type": "User"}

    def test_filters_short_bugbot_summary(self) -> None:
        body = "Cursor Bugbot has reviewed your changes and found 3 potential issues."
        self.assertTrue(pr_ops._is_bot_review_summary(self._bot(), "COMMENTED", body))

    def test_keeps_changes_requested_even_if_short(self) -> None:
        body = "Found 2 issues. Please fix."
        self.assertFalse(pr_ops._is_bot_review_summary(self._bot(), "CHANGES_REQUESTED", body))

    def test_keeps_long_prose_review(self) -> None:
        body = "We found 4 issues in this review. " + ("Detailed analysis follows. " * 40)
        self.assertFalse(pr_ops._is_bot_review_summary(self._bot(), "COMMENTED", body))

    def test_keeps_human_authored_comment(self) -> None:
        body = "Found 3 potential issues that worry me."
        self.assertFalse(pr_ops._is_bot_review_summary(self._human(), "COMMENTED", body))

    def test_keeps_body_without_summary_phrase(self) -> None:
        body = "Please add a test for the new branch."
        self.assertFalse(pr_ops._is_bot_review_summary(self._bot(), "COMMENTED", body))

    def test_strips_html_scaffolding_before_length_check(self) -> None:
        body = (
            "<!-- bugbot-id:xyz -->"
            "<p>Cursor Bugbot reviewed your changes and found 5 potential issues.</p>"
            + ("<span></span>" * 10)
        )
        self.assertTrue(pr_ops._is_bot_review_summary(self._bot(), "COMMENTED", body))


class TestClassifyFiltersReviewSummaries(unittest.TestCase):
    """End-to-end: review-summary entries must not appear in classify_comments output."""

    def test_bugbot_summary_review_is_filtered(self) -> None:
        reviews = [
            {
                "id": 999,
                "user": {"login": "cursor[bot]", "type": "Bot"},
                "body": "Cursor Bugbot has reviewed your changes and found 2 potential issues.",
                "state": "COMMENTED",
                "submitted_at": "2026-04-19T00:00:00Z",
                "html_url": "https://github.com/x/y/pull/1#pullrequestreview-999",
            },
            {
                "id": 1000,
                "user": {"login": "reviewer", "type": "User"},
                "body": "This needs more work across several files.",
                "state": "CHANGES_REQUESTED",
                "submitted_at": "2026-04-19T00:01:00Z",
                "html_url": "https://github.com/x/y/pull/1#pullrequestreview-1000",
            },
        ]
        kept, _ = pr_ops.classify_comments([], [], reviews)
        ids = [c["id"] for c in kept]
        self.assertNotIn(999, ids)
        self.assertIn(1000, ids)


class TestHeadVerification(unittest.TestCase):
    """state_append_round must reject a mismatched head_before by default."""

    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        tmp_root = Path(self.tmp.name)

        def fake_state_paths(pr, branch=None):
            key = pr if pr is not None else f"branch-{branch}"
            d = tmp_root / f"proj-{key}"
            d.mkdir(parents=True, exist_ok=True)
            return d, d / "pr-polish-state.json"

        p = patch.object(pr_ops, "state_paths", side_effect=fake_state_paths)
        p.start()
        self.addCleanup(p.stop)

    def test_mismatched_head_raises_and_does_not_write_state(self) -> None:
        with patch("subprocess.run") as run:
            run.return_value = type(
                "R", (), {"returncode": 0, "stdout": "realhead12345\n", "stderr": ""}
            )()
            with self.assertRaises(RuntimeError) as ctx:
                pr_ops.state_append_round(7, 1, "declaredSHA")
        self.assertIn("realhea", str(ctx.exception))
        _, path = pr_ops.state_paths(7)
        self.assertFalse(path.exists(), "state file must not be written on HEAD mismatch")

    def test_matching_head_writes_state(self) -> None:
        with patch("subprocess.run") as run:
            run.return_value = type(
                "R", (), {"returncode": 0, "stdout": "matchingSHA\n", "stderr": ""}
            )()
            state = pr_ops.state_append_round(7, 1, "matchingSHA")
        self.assertEqual(state["rounds"][0]["head_before"], "matchingSHA")

    def test_verify_head_false_skips_check(self) -> None:
        with patch("subprocess.run") as run:
            state = pr_ops.state_append_round(7, 1, "whatever", verify_head=False)
            run.assert_not_called()
        self.assertEqual(state["rounds"][0]["head_before"], "whatever")


class TestPersistRoundFindings(unittest.TestCase):
    """state_finalize_round must hydrate {backend}_findings, copy envelopes,
    and persist session ids — driven entirely by ``envelope_overrides``."""

    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.tmp_root = Path(self.tmp.name)
        self.state_dir = self.tmp_root / "proj-77"
        self.state_dir.mkdir(parents=True, exist_ok=True)

        def fake_state_paths(pr, branch=None):
            return self.state_dir, self.state_dir / "pr-polish-state.json"

        p = patch.object(pr_ops, "state_paths", side_effect=fake_state_paths)
        p.start()
        self.addCleanup(p.stop)

        self.envelope_dir = self.tmp_root / "envelopes"
        self.envelope_dir.mkdir()

    def _write_envelope(self, backend: str, **kw) -> Path:
        obj = {
            "schema_version": 1,
            "status": "ok",
            "backend": backend,
            "review": {"verdict": kw.get("verdict", "rejected"), "issues": kw.get("issues", [])},
        }
        if "session_id" in kw:
            obj["session_id"] = kw["session_id"]
        if "resume_status" in kw:
            obj["resume_status"] = kw["resume_status"]
        path = self.envelope_dir / f"{backend}-envelope.json"
        path.write_text(json.dumps(obj))
        return path

    def test_finalize_hydrates_findings_and_copies_envelopes(self) -> None:
        cx = self._write_envelope(
            "codex",
            issues=[{"severity": "high", "file": "a.go", "line": 5, "message": "oops", "topic": "t1"}],
        )
        cu = self._write_envelope(
            "cursor",
            issues=[{"severity": "medium", "file": "b.go", "line": 8, "message": "meh", "topic": "t2"}],
        )
        pr_ops.state_append_round(77, 1, "sha", verify_head=False)
        state = pr_ops.state_finalize_round(
            77, 1, "sha2", [], envelope_overrides={"codex": cx, "cursor": cu}
        )
        rnd = state["rounds"][0]

        self.assertEqual(len(rnd["codex_findings"]), 1)
        self.assertEqual(rnd["codex_findings"][0]["file"], "a.go")
        self.assertEqual(rnd["codex_findings"][0]["source"], "codex")
        self.assertEqual(len(rnd["cursor_findings"]), 1)
        self.assertEqual(rnd["cursor_findings"][0]["source"], "cursor")

        reviews = self.state_dir / "reviews"
        self.assertTrue((reviews / "r1-codex.json").exists())
        self.assertTrue((reviews / "r1-cursor.json").exists())

    def test_finalize_skips_backends_not_in_overrides(self) -> None:
        cx = self._write_envelope(
            "codex",
            issues=[{"severity": "low", "file": "c.go", "line": 3, "message": "ok", "topic": "t3"}],
        )
        pr_ops.state_append_round(77, 1, "sha", verify_head=False)
        state = pr_ops.state_finalize_round(
            77, 1, "sha2", [], envelope_overrides={"codex": cx}
        )
        rnd = state["rounds"][0]
        self.assertEqual(len(rnd["codex_findings"]), 1)
        self.assertEqual(rnd["cursor_findings"], [])

    def test_finalize_tolerates_malformed_envelope(self) -> None:
        bad = self.envelope_dir / "codex-envelope.json"
        bad.write_text("not json {{{")
        pr_ops.state_append_round(77, 1, "sha", verify_head=False)
        state = pr_ops.state_finalize_round(
            77, 1, "sha2", [], envelope_overrides={"codex": bad}
        )
        self.assertEqual(state["rounds"][0]["codex_findings"], [])

    def test_finalize_persists_session_ids(self) -> None:
        cx = self._write_envelope(
            "codex", session_id="codex-session-abc", resume_status="ok"
        )
        cu = self._write_envelope("cursor", session_id="cursor-session-xyz")
        pr_ops.state_append_round(77, 1, "sha", verify_head=False)
        state = pr_ops.state_finalize_round(
            77, 1, "sha2", [], envelope_overrides={"codex": cx, "cursor": cu}
        )
        rnd = state["rounds"][0]
        self.assertEqual(rnd.get("session_ids", {}).get("codex"), "codex-session-abc")
        self.assertEqual(rnd.get("session_ids", {}).get("cursor"), "cursor-session-xyz")
        self.assertEqual(rnd.get("resume_status", {}).get("codex"), "ok")
        self.assertTrue((self.state_dir / "reviews" / "r1-codex.json").exists())
        self.assertTrue((self.state_dir / "reviews" / "r1-cursor.json").exists())

    def test_finalize_hydrates_lint_findings(self) -> None:
        # lint is a first-class backend; its envelope must hydrate
        # rounds[n].lint_findings and copy into <state_dir>/reviews/.
        lint = self._write_envelope(
            "lint",
            issues=[{"severity": "low", "file": "x.py", "line": 1, "message": "F401", "topic": "unused-import"}],
        )
        pr_ops.state_append_round(77, 1, "sha", verify_head=False)
        state = pr_ops.state_finalize_round(
            77, 1, "sha2", [], envelope_overrides={"lint": lint}
        )
        rnd = state["rounds"][0]
        self.assertEqual(len(rnd["lint_findings"]), 1)
        self.assertEqual(rnd["lint_findings"][0]["source"], "lint")
        self.assertTrue((self.state_dir / "reviews" / "r1-lint.json").exists())

    def test_refinalize_drops_stale_backend_data(self) -> None:
        # r36 finding: re-finalizing a round with a narrower envelope
        # set previously left stale per-backend findings, session_ids,
        # and resume_status behind. The next round's prior_session_id
        # could then resume the wrong backend's session, breaking
        # continuous-conversation review. After the fix, omitted
        # backends get their entry data dropped so re-finalize is
        # genuinely idempotent.
        cx = self._write_envelope(
            "codex", session_id="codex-1", issues=[
                {"severity": "high", "file": "a.py", "line": 1, "message": "x", "topic": "t"},
            ],
        )
        cu = self._write_envelope(
            "cursor", session_id="cursor-1", resume_status="ok",
            issues=[
                {"severity": "low", "file": "b.py", "line": 2, "message": "y", "topic": "u"},
            ],
        )
        pr_ops.state_append_round(77, 1, "sha", verify_head=False)
        # First pass: both backends.
        state = pr_ops.state_finalize_round(
            77, 1, "sha2", [], envelope_overrides={"codex": cx, "cursor": cu}
        )
        rnd = state["rounds"][0]
        self.assertEqual(len(rnd.get("codex_findings") or []), 1)
        self.assertEqual(len(rnd.get("cursor_findings") or []), 1)
        self.assertEqual(rnd["session_ids"]["cursor"], "cursor-1")
        # Second pass: only codex. cursor's data must be dropped.
        cx2 = self._write_envelope(
            "codex", session_id="codex-2", issues=[
                {"severity": "high", "file": "a.py", "line": 1, "message": "x", "topic": "t"},
            ],
        )
        state = pr_ops.state_finalize_round(
            77, 1, "sha3", [], envelope_overrides={"codex": cx2}
        )
        rnd = state["rounds"][0]
        self.assertEqual(rnd["session_ids"].get("codex"), "codex-2")
        self.assertNotIn("cursor", rnd.get("session_ids") or {})
        # resume_status[cursor] should also be cleared, not just session_ids.
        self.assertNotIn("cursor", rnd.get("resume_status") or {})
        # Findings reset to empty (rather than popped) so callers
        # indexing rnd["cursor_findings"] still work.
        self.assertEqual(rnd.get("cursor_findings"), [])
        # Disk parity: archived envelope file for the dropped backend
        # is removed so post-loop audits don't see contradictions.
        self.assertFalse((self.state_dir / "reviews" / "r1-cursor.json").exists())
        # The retained backend's archive should still be there.
        self.assertTrue((self.state_dir / "reviews" / "r1-codex.json").exists())

    def test_refinalize_with_zero_envelopes_clears_all_backends(self) -> None:
        # r37 finding: the prior fix only ran cleanup when the new
        # envelope set was non-empty, so a re-finalize that passed no
        # envelopes (or only missing-on-disk paths) silently kept the
        # earlier round's session_ids/findings. The next round would
        # then resume a stale session.
        cx = self._write_envelope(
            "codex", session_id="codex-x", issues=[
                {"severity": "high", "file": "a.py", "line": 1, "message": "x", "topic": "t"},
            ],
        )
        pr_ops.state_append_round(77, 1, "sha", verify_head=False)
        pr_ops.state_finalize_round(
            77, 1, "sha2", [], envelope_overrides={"codex": cx}
        )
        # Re-finalize with no envelopes: must clear codex too.
        state = pr_ops.state_finalize_round(77, 1, "sha3", [], envelope_overrides={})
        rnd = state["rounds"][0]
        self.assertEqual(rnd.get("codex_findings"), [])
        self.assertNotIn("codex", rnd.get("session_ids") or {})
        self.assertFalse((self.state_dir / "reviews" / "r1-codex.json").exists())

    def test_refinalize_treats_missing_envelope_as_absent(self) -> None:
        # An override path that doesn't exist on disk must not protect
        # the prior round's per-backend state from cleanup. Otherwise a
        # caller that points at a stale path silently keeps stale data.
        cx = self._write_envelope("codex", session_id="codex-y")
        pr_ops.state_append_round(77, 1, "sha", verify_head=False)
        pr_ops.state_finalize_round(
            77, 1, "sha2", [], envelope_overrides={"codex": cx}
        )
        ghost = self.envelope_dir / "missing.json"  # never created
        state = pr_ops.state_finalize_round(
            77, 1, "sha3", [], envelope_overrides={"codex": ghost}
        )
        rnd = state["rounds"][0]
        self.assertEqual(rnd.get("codex_findings"), [])
        self.assertNotIn("codex", rnd.get("session_ids") or {})
        # Disk parity: archived envelope file unlinked even on the
        # missing-override path.
        self.assertFalse((self.state_dir / "reviews" / "r1-codex.json").exists())

    def test_refinalize_clears_session_when_envelope_lacks_id(self) -> None:
        # r38 finding: an envelope that exists on disk but parses to
        # non-dict or a dict without session_id/resume_status used to
        # leave the prior finalize's values in place. Same stale-resume
        # class. After the fix, processing a backend always clears its
        # session_ids/resume_status entry first, then re-applies only
        # what the new envelope provides.
        cx_with = self._write_envelope("codex", session_id="codex-old", resume_status="ok")
        pr_ops.state_append_round(77, 1, "sha", verify_head=False)
        pr_ops.state_finalize_round(
            77, 1, "sha2", [], envelope_overrides={"codex": cx_with}
        )
        # Second envelope: valid JSON dict, no session_id key.
        cx_no = self.envelope_dir / "codex-no-sid.json"
        cx_no.write_text(json.dumps({"backend": "codex", "review": {"issues": []}}))
        state = pr_ops.state_finalize_round(
            77, 1, "sha3", [], envelope_overrides={"codex": cx_no}
        )
        rnd = state["rounds"][0]
        self.assertNotIn("codex", rnd.get("session_ids") or {})
        self.assertNotIn("codex", rnd.get("resume_status") or {})

    def test_state_finalize_round_cli_rejects_unknown_backend(self) -> None:
        # Round 27 fix: --envelope curor=/tmp/x typos used to be
        # silently ignored later; now the CLI parser validates
        # against bramble_ops.BACKENDS at parse time.
        cx = self._write_envelope("codex")
        actions_file = self.state_dir / "actions.json"
        actions_file.write_text("[]")
        pr_ops.state_append_round(77, 1, "sha", verify_head=False)
        rc = pr_ops.main(
            [
                "state-finalize-round", "77", "1", "sha2", str(actions_file),
                "--envelope", f"codex={cx}",
                "--envelope", "curor=/tmp/typo.json",  # typo: curor not cursor
            ]
        )
        self.assertNotEqual(rc, 0)


    def test_state_finalize_round_cli_warns_when_no_envelopes(self) -> None:
        # r36 audit: orchestrator silently dropped --envelope across
        # several rounds, which lost session_ids and broke the next
        # round's resume continuity (prior_session_id walked past the
        # un-hydrated round and resumed a stale earlier session). CLI
        # warns loudly on stderr so pilot errors don't go silent.
        actions_file = self.state_dir / "actions.json"
        actions_file.write_text("[]")
        pr_ops.state_append_round(78, 1, "sha", verify_head=False)
        import io
        from contextlib import redirect_stderr
        buf = io.StringIO()
        with redirect_stderr(buf):
            rc = pr_ops.main(
                ["state-finalize-round", "78", "1", "sha2", str(actions_file)]
            )
        self.assertEqual(rc, 0)
        self.assertIn("without --envelope", buf.getvalue())

    def test_finalize_persists_sufficiency_claim_when_present(self) -> None:
        # v2 schema: a reviewer that emits a top-level sufficiency
        # object has its claim persisted at rounds[n].sufficiency_claims
        # under the backend's key. Absence stays absent — no synthesis.
        cx_path = self.envelope_dir / "codex-envelope.json"
        cx_path.write_text(json.dumps({
            "status": "ok",
            "backend": "codex",
            "session_id": "s1",
            "review": {
                "verdict": "accepted",
                "issues": [],
                "sufficiency": {
                    "is_confident_complete": True,
                    "evidence": "all named invariants addressed",
                },
            },
            "schema_version": 2,
        }))
        cu_path = self.envelope_dir / "cursor-envelope.json"
        cu_path.write_text(json.dumps({
            "status": "ok",
            "backend": "cursor",
            "session_id": "s2",
            "review": {"verdict": "accepted", "issues": []},
            "schema_version": 2,
        }))
        pr_ops.state_append_round(77, 1, "sha", verify_head=False)
        state = pr_ops.state_finalize_round(
            77, 1, "sha2", [],
            envelope_overrides={"codex": cx_path, "cursor": cu_path},
        )
        claims = state["rounds"][0].get("sufficiency_claims")
        self.assertIsNotNone(claims)
        self.assertEqual(claims["codex"]["is_confident_complete"], True)
        self.assertEqual(claims["codex"]["evidence"], "all named invariants addressed")
        # Cursor's envelope had no sufficiency — no entry.
        self.assertNotIn("cursor", claims)

    def test_finalize_clears_stale_sufficiency_on_re_finalize(self) -> None:
        # Re-finalize must not let the previous turn's sufficiency
        # claim survive when the new envelope omits the field. Same
        # cleanup pattern as session_ids/resume_status.
        env_path = self.envelope_dir / "codex-envelope.json"
        env_path.write_text(json.dumps({
            "status": "ok",
            "backend": "codex",
            "review": {
                "verdict": "accepted",
                "issues": [],
                "sufficiency": {"is_confident_complete": True},
            },
        }))
        pr_ops.state_append_round(77, 1, "sha", verify_head=False)
        pr_ops.state_finalize_round(
            77, 1, "sha2", [], envelope_overrides={"codex": env_path}
        )
        # Re-finalize with an envelope that has NO sufficiency.
        env_path.write_text(json.dumps({
            "status": "ok",
            "backend": "codex",
            "review": {"verdict": "accepted", "issues": []},
        }))
        state = pr_ops.state_finalize_round(
            77, 1, "sha3", [], envelope_overrides={"codex": env_path}
        )
        claims = state["rounds"][0].get("sufficiency_claims")
        # Either absent entirely or codex key removed — both are fine.
        if claims is not None:
            self.assertNotIn("codex", claims)


class TestCIFailedTests(unittest.TestCase):
    """Parses per-failed-job test details from gh check output + job logs."""

    def test_go_fail_extracted_with_assertion_snippet(self) -> None:
        log = (
            "=== RUN   TestFoo\n"
            "    foo_test.go:42: expected 1 got 2\n"
            "--- FAIL: TestFoo (0.01s)\n"
            "FAIL\n"
            "exit status 1\n"
        )
        checks = [
            {
                "name": "build",
                "state": "fail",
                "workflow": "ci",
                "link": "https://github.com/o/r/actions/runs/111/job/222",
            }
        ]
        with (
            patch.object(pr_ops, "identify_pr", return_value={"owner_repo": "o/r", "pr_number": 9}),
            patch.object(
                pr_ops, "run", return_value=type("R", (), {"stdout": json.dumps(checks)})()
            ),
            patch.object(pr_ops, "_fetch_job_log", return_value=log),
        ):
            out = pr_ops.ci_failed_tests(9)
        self.assertEqual(len(out), 1)
        entry = out[0]
        self.assertEqual(entry["job_id"], 222)
        self.assertEqual(entry["job_name"], "build")
        self.assertEqual(entry["failed_tests"], ["TestFoo"])
        self.assertFalse(entry["is_flake"])
        self.assertIsNone(entry["flake_reason"])
        self.assertIn("TestFoo", entry["assertion_snippet"])


class TestCIFlakeClassifier(unittest.TestCase):
    """classify_ci_log pure; flake markers win over --- FAIL:."""

    def test_etxtbsy_marks_flake(self) -> None:
        log = "fork/exec /path/to/bin: text file busy\n"
        out = pr_ops.classify_ci_log(log)
        self.assertTrue(out["is_flake"])
        self.assertEqual(out["flake_reason"], "etxtbsy")

    def test_real_fail_not_flake(self) -> None:
        log = "--- FAIL: TestBar (0.00s)\nFAIL\n"
        out = pr_ops.classify_ci_log(log)
        self.assertFalse(out["is_flake"])
        self.assertIsNone(out["flake_reason"])
        self.assertEqual(out["failed_tests"], ["TestBar"])

    def test_context_deadline_without_fail_is_flake(self) -> None:
        log = "bazel: context deadline exceeded after 15m\n"
        out = pr_ops.classify_ci_log(log)
        self.assertTrue(out["is_flake"])
        self.assertEqual(out["flake_reason"], "ci_deadline")

    def test_etxtbsy_wins_over_fail_marker(self) -> None:
        log = "--- FAIL: TestX\nexec: text file busy\n"
        out = pr_ops.classify_ci_log(log)
        self.assertTrue(out["is_flake"])
        self.assertEqual(out["flake_reason"], "etxtbsy")


class TestCICompareBase(unittest.TestCase):
    """Splits PR failures into pre_existing vs pr_caused by intersecting base run."""

    def _run_factory(self, responses: list[str]):
        it = iter(responses)

        def fake_run(argv, check=False):
            try:
                stdout = next(it)
            except StopIteration:
                stdout = ""
            return type("R", (), {"stdout": stdout})()

        return fake_run

    def test_pre_existing_when_base_fails_same_test(self) -> None:
        base_list = {"workflow_runs": [{"id": 555}]}
        base_meta = {"head_sha": "basesha"}
        base_jobs = {
            "head_sha": "basesha",
            "jobs": [
                {"id": 777, "conclusion": "failure"},
            ],
        }
        pr_checks = [
            {
                "name": "build",
                "state": "fail",
                "workflow": "ci",
                "link": "https://github.com/o/r/actions/runs/11/job/22",
            }
        ]
        base_job_log = "--- FAIL: TestFoo (0.00s)\nFAIL\n"
        pr_job_log = "--- FAIL: TestFoo (0.00s)\nFAIL\n"

        with tempfile.TemporaryDirectory() as d:
            state_dir = Path(d)

            def fake_state_paths(pr, branch=None):
                return state_dir, state_dir / "s.json"

            def fake_fetch_job_log(owner_repo, job_id):
                return base_job_log if job_id == 777 else pr_job_log

            with (
                patch.object(
                    pr_ops,
                    "identify_pr",
                    return_value={"owner_repo": "o/r", "pr_number": 9, "base": "main"},
                ),
                patch.object(pr_ops, "state_paths", side_effect=fake_state_paths),
                patch.object(pr_ops, "_fetch_job_log", side_effect=fake_fetch_job_log),
                patch.object(
                    pr_ops,
                    "run",
                    side_effect=self._run_factory(
                        [
                            json.dumps(base_list),
                            json.dumps(base_meta),
                            json.dumps(base_jobs),
                            json.dumps(pr_checks),
                        ]
                    ),
                ),
            ):
                out = pr_ops.ci_compare_base(9)
        self.assertEqual(out["pre_existing"], ["TestFoo"])
        self.assertEqual(out["pr_caused"], [])

    def test_pr_caused_when_only_pr_fails(self) -> None:
        base_list = {"workflow_runs": []}
        pr_checks = [
            {
                "name": "build",
                "state": "fail",
                "workflow": "ci",
                "link": "https://github.com/o/r/actions/runs/11/job/22",
            }
        ]
        pr_job_log = "--- FAIL: TestBar (0.00s)\nFAIL\n"

        with tempfile.TemporaryDirectory() as d:
            state_dir = Path(d)

            def fake_state_paths(pr, branch=None):
                return state_dir, state_dir / "s.json"

            with (
                patch.object(
                    pr_ops,
                    "identify_pr",
                    return_value={"owner_repo": "o/r", "pr_number": 9, "base": "main"},
                ),
                patch.object(pr_ops, "state_paths", side_effect=fake_state_paths),
                patch.object(pr_ops, "_fetch_job_log", return_value=pr_job_log),
                patch.object(
                    pr_ops,
                    "run",
                    side_effect=self._run_factory([json.dumps(base_list), json.dumps(pr_checks)]),
                ),
            ):
                out = pr_ops.ci_compare_base(9)
        self.assertEqual(out["pre_existing"], [])
        self.assertEqual(out["pr_caused"], ["TestBar"])


class TestStateFinalizeRecordsCIFindings(unittest.TestCase):
    """state_finalize_round populates rounds[n].ci_findings via ci_failed_tests."""

    def test_ci_findings_populated(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            state_dir = Path(d) / "proj-77"
            state_dir.mkdir()

            def fake_state_paths(pr, branch=None):
                return state_dir, state_dir / "pr-polish-state.json"

            fake_ci = [
                {
                    "job_id": 222,
                    "job_name": "build",
                    "workflow": "ci",
                    "url": "u",
                    "failed_tests": ["TestFoo"],
                    "is_flake": False,
                    "flake_reason": None,
                    "assertion_snippet": "",
                }
            ]

            with (
                patch.object(pr_ops, "state_paths", side_effect=fake_state_paths),
                patch.object(pr_ops, "ci_failed_tests", return_value=fake_ci),
            ):
                pr_ops.state_append_round(77, 1, "sha", verify_head=False)
                state = pr_ops.state_finalize_round(77, 1, "sha2", [])
        self.assertEqual(state["rounds"][0]["ci_findings"], fake_ci)

    def test_ci_findings_best_effort_on_error(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            state_dir = Path(d) / "proj-77"
            state_dir.mkdir()

            def fake_state_paths(pr, branch=None):
                return state_dir, state_dir / "pr-polish-state.json"

            with (
                patch.object(pr_ops, "state_paths", side_effect=fake_state_paths),
                patch.object(pr_ops, "ci_failed_tests", side_effect=RuntimeError("gh down")),
            ):
                pr_ops.state_append_round(77, 1, "sha", verify_head=False)
                state = pr_ops.state_finalize_round(77, 1, "sha2", [])
        self.assertEqual(state["rounds"][0]["ci_findings"], [])

    def test_ci_findings_skipped_for_branch_only_ctx(self) -> None:
        """Branch-only runs never call ci_failed_tests — there's no PR to query."""
        with tempfile.TemporaryDirectory() as d:
            state_dir = Path(d) / "proj-branch-foo"
            state_dir.mkdir()

            def fake_state_paths(pr, branch=None):
                return state_dir, state_dir / "pr-polish-state.json"

            sentinel = {"called": False}

            def boom(*args, **kwargs):
                sentinel["called"] = True
                return []

            with (
                patch.object(pr_ops, "state_paths", side_effect=fake_state_paths),
                patch.object(pr_ops, "ci_failed_tests", side_effect=boom),
            ):
                pr_ops.state_append_round("branch:foo", 1, "sha", verify_head=False)
                state = pr_ops.state_finalize_round("branch:foo", 1, "sha2", [])
        self.assertFalse(sentinel["called"])
        self.assertEqual(state["rounds"][0]["ci_findings"], [])


class TestRecomputeCountsTreatsPreExistingAsSkipped(unittest.TestCase):
    """pre_existing + flake count as skipped, not fixed, not ignored."""

    def test_pre_existing_increments_skipped(self) -> None:
        counts = pr_ops.recompute_counts(
            [{"source": "ci", "action": "pre_existing", "severity": None}]
        )
        self.assertEqual(counts["skipped_count"], 1)
        self.assertEqual(counts["fixed_count"], 0)

    def test_flake_increments_skipped(self) -> None:
        counts = pr_ops.recompute_counts([{"source": "ci", "action": "flake", "severity": None}])
        self.assertEqual(counts["skipped_count"], 1)
        self.assertEqual(counts["fixed_count"], 0)

    def test_mixed_actions(self) -> None:
        actions = [
            {"source": "ci", "action": "pre_existing", "severity": None},
            {"source": "ci", "action": "flake", "severity": None},
            {"source": "codex", "action": "fixed", "severity": "high"},
            {"source": "cursor", "action": "wont_fix", "severity": "low"},
        ]
        counts = pr_ops.recompute_counts(actions)
        self.assertEqual(counts["fixed_count"], 1)
        self.assertEqual(counts["skipped_count"], 3)


class TestSlugifyAndStatePaths(unittest.TestCase):
    def test_slugify_strips_slashes_and_lowercases(self) -> None:
        self.assertEqual(_common._slugify_branch("feature/Foo BAR"), "feature-foo-bar")

    def test_slugify_handles_empty_like_input(self) -> None:
        self.assertEqual(_common._slugify_branch("---"), "unnamed")

    def test_state_paths_branch_mode(self) -> None:
        with patch.object(_common, "repo_slug", return_value="myrepo"):
            sd, sf = _common.state_paths(None, branch="feature/foo")
        self.assertIn("myrepo-branch-feature-foo", str(sd))
        self.assertTrue(str(sf).endswith("pr-polish-state.json"))

    def test_state_paths_requires_branch_when_no_pr(self) -> None:
        with self.assertRaises(ValueError):
            _common.state_paths(None)


class TestReplyInlineSafeBody(unittest.TestCase):
    """Reply bodies must never be passed via `gh api -f body=...`: gh treats
    `@`-prefixed values as file references, so a body starting with `@` could
    read a local file or send the wrong payload. The fix pipes JSON via stdin.
    """

    def test_uses_stdin_input_with_json_payload(self) -> None:
        captured: dict = {}

        def fake_run(cmd, **kwargs):
            captured["cmd"] = list(cmd)
            captured["input_text"] = kwargs.get("input_text")
            return _common.RunResult(stdout='{"id": 1}', stderr="", returncode=0)

        body = "@malicious-looking but really just a literal reply"
        with patch.object(pr_ops, "run", side_effect=fake_run):
            pr_ops.reply_inline("owner/repo", 234, 99, body)
        self.assertIn("--input", captured["cmd"])
        self.assertIn("-", captured["cmd"])
        # Body must not be embedded in argv via -f / -F.
        self.assertNotIn("-f", captured["cmd"])
        self.assertNotIn("-F", captured["cmd"])
        payload = json.loads(captured["input_text"])
        self.assertEqual(payload, {"body": body})




class TestRemoteHead(unittest.TestCase):
    """Series-boundary detection prefers `git ls-remote refs/heads/<branch>`
    over `git rev-parse origin/<branch>` because the latter lags in
    worktrees (existing memory: feedback_force_with_lease_in_worktrees).
    Round 13 of pr-polish surfaced this when git-sync had pushed during
    the run but origin/<branch> still pointed at the pre-push SHA,
    confusing the operator about whether to push again."""

    def test_in_sync_when_remote_matches_local(self) -> None:
        def fake_run(cmd, **kwargs):
            if cmd[:2] == ["git", "rev-parse"]:
                return _common.RunResult(stdout="abc123\n", stderr="", returncode=0)
            if cmd[:2] == ["git", "ls-remote"]:
                return _common.RunResult(
                    stdout="abc123\trefs/heads/feature/foo\n",
                    stderr="",
                    returncode=0,
                )
            raise AssertionError(f"unexpected command: {cmd}")

        with patch.object(pr_ops, "run", side_effect=fake_run):
            out = pr_ops.remote_head("feature/foo")
        self.assertEqual(out["local_head"], "abc123")
        self.assertEqual(out["remote_head"], "abc123")
        self.assertTrue(out["in_sync"])
        self.assertTrue(out["remote_present"])

    def test_remote_absent_yields_remote_present_false(self) -> None:
        def fake_run(cmd, **kwargs):
            if cmd[:2] == ["git", "rev-parse"]:
                return _common.RunResult(stdout="abc123\n", stderr="", returncode=0)
            return _common.RunResult(stdout="", stderr="", returncode=0)

        with patch.object(pr_ops, "run", side_effect=fake_run):
            out = pr_ops.remote_head("feature/foo")
        self.assertFalse(out["remote_present"])
        self.assertFalse(out["in_sync"])
        self.assertEqual(out["remote_head"], "")

    def test_diverged_branch_is_not_in_sync(self) -> None:
        def fake_run(cmd, **kwargs):
            if cmd[:2] == ["git", "rev-parse"]:
                return _common.RunResult(stdout="local-sha\n", stderr="", returncode=0)
            return _common.RunResult(
                stdout="remote-sha\trefs/heads/feature/foo\n",
                stderr="",
                returncode=0,
            )

        with patch.object(pr_ops, "run", side_effect=fake_run):
            out = pr_ops.remote_head("feature/foo")
        self.assertEqual(out["local_head"], "local-sha")
        self.assertEqual(out["remote_head"], "remote-sha")
        self.assertFalse(out["in_sync"])
        self.assertTrue(out["remote_present"])

    def test_uses_ls_remote_not_rev_parse_origin(self) -> None:
        # Regression guard: rev-parse origin/<branch> would silently lag in
        # worktrees. The helper must call ls-remote.
        called_cmds = []

        def fake_run(cmd, **kwargs):
            called_cmds.append(list(cmd))
            if cmd[:2] == ["git", "rev-parse"]:
                return _common.RunResult(stdout="abc\n", stderr="", returncode=0)
            return _common.RunResult(
                stdout="abc\trefs/heads/main\n", stderr="", returncode=0
            )

        with patch.object(pr_ops, "run", side_effect=fake_run):
            pr_ops.remote_head("main")
        kinds = [tuple(c[:2]) for c in called_cmds]
        self.assertIn(("git", "ls-remote"), kinds)
        # Must NOT use rev-parse on origin/<branch> — that's the buggy path.
        for c in called_cmds:
            if c[:2] == ["git", "rev-parse"]:
                self.assertNotIn("origin/main", c)


class TestAutoReplyInFinalize(unittest.TestCase):
    """state-finalize-round must post auto-replies on github-inline rows
    whose action ∈ {fixed, stale, false_positive, wont_fix} that don't
    already carry a reply_url. Idempotent across replays. Failures are
    captured as reply_error and never block finalize.
    """

    def setUp(self) -> None:
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.tmp_root = Path(self.tmp.name)
        self.state_dir = self.tmp_root / "proj-77"
        self.state_dir.mkdir(parents=True, exist_ok=True)

        def fake_state_paths(pr, branch=None):
            return self.state_dir, self.state_dir / "pr-polish-state.json"

        p = patch.object(pr_ops, "state_paths", side_effect=fake_state_paths)
        p.start()
        self.addCleanup(p.stop)

        # Stub _owner_repo so finalize doesn't shell out to gh.
        p2 = patch.object(
            pr_ops, "_owner_repo", return_value=("owner", "repo", "owner/repo")
        )
        p2.start()
        self.addCleanup(p2.stop)

    def _action(self, **kw):
        base = {
            "comment_id": kw.get("comment_id", 1001),
            "source": kw.get("source", "github-inline"),
            "author": "coderabbitai[bot]",
            "path": "a.py",
            "line": 42,
            "severity": "high",
            "topic": "missing null check",
            "action": kw.get("action", "fixed"),
            "reason": kw.get("reason"),
            "commit_sha": "abc123f",
        }
        base.update(kw)
        return base

    def test_posts_replies_for_fixed_inline_rows(self) -> None:
        calls = []

        def fake_reply(owner_repo, pr, cid, body):
            calls.append((owner_repo, pr, cid, body))
            return {"html_url": f"https://github.com/owner/repo/pull/{pr}#discussion_r{cid}"}

        pr_ops.state_append_round(77, 1, "sha", verify_head=False)
        actions = [self._action(comment_id=2001, action="fixed")]
        with patch.object(pr_ops, "reply_inline", side_effect=fake_reply):
            state = pr_ops.state_finalize_round(77, 1, "abc123fdeadbeef", actions)
        self.assertEqual(len(calls), 1)
        self.assertEqual(calls[0][2], 2001)
        self.assertIn("Fixed in abc123f", calls[0][3])
        rnd = state["rounds"][0]
        row = rnd["comment_actions"][0]
        self.assertTrue(row.get("reply_url", "").startswith("https://github.com/"))

    def test_skips_rows_already_carrying_reply_url(self) -> None:
        """Replays must not double-post."""
        calls = []
        pr_ops.state_append_round(77, 1, "sha", verify_head=False)
        actions = [
            self._action(
                comment_id=2002,
                action="fixed",
                reply_url="https://github.com/owner/repo/pull/77#discussion_r2002",
            )
        ]
        with patch.object(pr_ops, "reply_inline", side_effect=lambda *a: calls.append(a) or {}):
            pr_ops.state_finalize_round(77, 1, "sha2", actions)
        self.assertEqual(calls, [])

    def test_replay_with_fresh_action_list_does_not_repost(self) -> None:
        """Re-finalize from a freshly recomputed action list (no reply_url
        carried forward in the caller) must still skip already-replied rows.

        This is the cross-process replay path: round 1 finalizes and persists
        reply_url; later, finalize is called again with `actions` rebuilt
        from comments — it does not carry reply_url. ``_merge_actions`` must
        preserve the persisted reply_url so ``_post_inline_replies`` skips
        the row.
        """
        calls = []

        def fake_reply(owner_repo, pr, cid, body):
            calls.append((owner_repo, pr, cid, body))
            return {"html_url": f"https://github.com/owner/repo/pull/{pr}#discussion_r{cid}"}

        pr_ops.state_append_round(77, 1, "sha", verify_head=False)
        with patch.object(pr_ops, "reply_inline", side_effect=fake_reply):
            pr_ops.state_finalize_round(
                77, 1, "abc123fdeadbeef", [self._action(comment_id=2099, action="fixed")]
            )
        self.assertEqual(len(calls), 1)

        with patch.object(pr_ops, "reply_inline", side_effect=fake_reply):
            state = pr_ops.state_finalize_round(
                77, 1, "abc123fdeadbeef", [self._action(comment_id=2099, action="fixed")]
            )
        self.assertEqual(len(calls), 1)
        row = state["rounds"][0]["comment_actions"][0]
        self.assertTrue(row.get("reply_url", "").startswith("https://"))

    def test_skips_ack_and_non_inline_rows(self) -> None:
        calls = []
        pr_ops.state_append_round(77, 1, "sha", verify_head=False)
        actions = [
            self._action(comment_id=2003, action="ack"),
            self._action(source="codex", comment_id=None, action="fixed"),
            self._action(source="github-issue", comment_id=2004, action="fixed"),
        ]
        with patch.object(pr_ops, "reply_inline", side_effect=lambda *a: calls.append(a) or {}):
            pr_ops.state_finalize_round(77, 1, "sha2", actions)
        # github-issue rows still skipped: only inline review threads
        # support the /comments/<id>/replies endpoint.
        self.assertEqual(calls, [])

    def test_records_reply_error_on_failure_without_blocking_finalize(self) -> None:
        pr_ops.state_append_round(77, 1, "sha", verify_head=False)
        actions = [
            self._action(comment_id=2005, action="fixed"),
            self._action(comment_id=2006, action="stale"),
        ]

        def fake_reply(owner_repo, pr, cid, body):
            if cid == 2005:
                raise RuntimeError("rate limit exceeded")
            return {"html_url": f"https://github.com/owner/repo/pull/{pr}#discussion_r{cid}"}

        with patch.object(pr_ops, "reply_inline", side_effect=fake_reply):
            state = pr_ops.state_finalize_round(77, 1, "sha2", actions)
        rows = {r["comment_id"]: r for r in state["rounds"][0]["comment_actions"]}
        self.assertIn("rate limit", rows[2005].get("reply_error", ""))
        self.assertNotIn("reply_url", rows[2005])  # failed row left without URL
        # Other row in same finalize call still posts.
        self.assertTrue(rows[2006].get("reply_url", "").startswith("https://"))

    def test_retry_on_next_finalize_clears_prior_error(self) -> None:
        pr_ops.state_append_round(77, 1, "sha", verify_head=False)
        actions_initial = [self._action(comment_id=2007, action="fixed")]

        def fake_fail(*a, **kw):
            raise RuntimeError("transient")

        with patch.object(pr_ops, "reply_inline", side_effect=fake_fail):
            pr_ops.state_finalize_round(77, 1, "sha2", actions_initial)

        def fake_ok(owner_repo, pr, cid, body):
            return {"html_url": f"https://github.com/owner/repo/pull/{pr}#discussion_r{cid}"}

        with patch.object(pr_ops, "reply_inline", side_effect=fake_ok):
            state = pr_ops.state_finalize_round(77, 1, "sha3", actions_initial)
        row = state["rounds"][0]["comment_actions"][0]
        self.assertTrue(row.get("reply_url"))
        self.assertNotIn("reply_error", row)

    def test_branch_mode_skips_auto_reply(self) -> None:
        # Branch-only mode (no PR number) has no inline-comment endpoint.
        # _owner_repo would still resolve, but there's no PR to post to.
        calls = []
        pr_ops.state_append_round("branch:foo", 1, "sha", verify_head=False)
        actions = [self._action(comment_id=2008, action="fixed")]
        with patch.object(pr_ops, "reply_inline", side_effect=lambda *a: calls.append(a) or {}):
            pr_ops.state_finalize_round("branch:foo", 1, "sha2", actions)
        self.assertEqual(calls, [])

    def test_reply_body_shapes(self) -> None:
        # Direct exercise of the body renderer — golden-shape contract
        # documented in SKILL.md Step 3.d.
        body_fixed = pr_ops._reply_body({"action": "fixed"}, "abc123fdeadbeef")
        self.assertEqual(body_fixed, "Fixed in abc123f.")
        body_stale = pr_ops._reply_body({"action": "stale"}, "abc123fdeadbeef")
        self.assertIn("Superseded by abc123f", body_stale)
        self.assertIn("/pr-polish.", body_stale)
        body_fp = pr_ops._reply_body(
            {"action": "false_positive", "reason": "see foo.py:10"}, "abc123fdeadbeef"
        )
        self.assertIn("Marked false positive: see foo.py:10", body_fp)
        body_wf = pr_ops._reply_body(
            {"action": "wont_fix", "reason": "design tradeoff"}, "abc123fdeadbeef"
        )
        self.assertIn("Won't fix: design tradeoff", body_wf)


class TestPreflight(unittest.TestCase):
    """preflight resolves the binaries + helper paths the round loop
    needs in one JSON dict. The errors[] list signals fail-fast cases
    (missing --resume-session-id support, git-sync not on disk)."""

    def test_returns_dict_with_required_keys(self) -> None:
        out = pr_ops.preflight()
        for key in (
            "bramble_bin",
            "bramble_resume_supported",
            "git_sync_path",
            "git_sync_supports_no_push",
            "skill_dir",
            "errors",
        ):
            self.assertIn(key, out)
        self.assertIsInstance(out["errors"], list)

    def test_reports_missing_resume_support_in_errors(self) -> None:
        def fake_subprocess_run(cmd, **kwargs):
            # Simulate an old bramble that doesn't print --resume-session-id.
            return subprocess.CompletedProcess(args=cmd, returncode=0,
                                               stdout="usage: bramble code-review", stderr="")
        with patch("subprocess.run", side_effect=fake_subprocess_run):
            out = pr_ops.preflight()
        self.assertFalse(out["bramble_resume_supported"])
        self.assertTrue(any("--resume-session-id" in e for e in out["errors"]))


class TestRoundBundle(unittest.TestCase):
    """round-bundle wraps state_load + goal_for_round + prior_session_id
    into one JSON dict the orchestrator reads with one jq call."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.tmp = Path(self._tmp.name)
        self._patch_home = patch.dict(os.environ, {"HOME": str(self.tmp)})
        self._patch_home.start()
        # Reset the cached state dir module-level path.
        import importlib
        importlib.reload(pr_ops)

    def tearDown(self) -> None:
        self._patch_home.stop()
        self._tmp.cleanup()
        import importlib
        importlib.reload(pr_ops)

    def test_emits_paths_and_resume_ids_for_fresh_run(self) -> None:
        def fake_run(cmd, **kwargs):
            if cmd[:2] == ["git", "rev-parse"]:
                return _common.RunResult(stdout="head-sha\n", stderr="", returncode=0)
            return _common.RunResult(stdout="", stderr="", returncode=1)
        with patch.object(pr_ops, "run", side_effect=fake_run):
            out = pr_ops.round_bundle(99, 1)
        self.assertIn("state_dir", out)
        self.assertIn("log_dir", out)
        # Log dir is attempt-scoped: first attempt of round 1 is r1/a1.
        self.assertTrue(out["log_dir"].endswith("/r1/a1"))
        self.assertEqual(out["head_before"], "head-sha")
        self.assertIn("envelope_paths", out)
        for backend in ("codex", "cursor", "gemini", "lint"):
            self.assertIn(backend, out["envelope_paths"])
            # Envelope paths inherit the attempt-scoped log dir.
            self.assertTrue(
                out["envelope_paths"][backend].endswith(
                    f"/r1/a1/{backend}-envelope.json"
                )
            )
            # All resume ids empty on a fresh run (no prior state).
            self.assertEqual(out["resume_ids"].get(backend, ""), "")

    def test_attempt_index_increments_on_resumed_round(self) -> None:
        # Simulate a prior attempt by creating the r1/a1 dir, then confirm
        # the next round-bundle for the same round lands on a2 (fresh dir,
        # so the Monitor barrier can't see the prior attempt's envelope).
        def fake_run(cmd, **kwargs):
            if cmd[:2] == ["git", "rev-parse"]:
                return _common.RunResult(stdout="head-sha\n", stderr="", returncode=0)
            return _common.RunResult(stdout="", stderr="", returncode=1)
        with patch.object(pr_ops, "run", side_effect=fake_run):
            out1 = pr_ops.round_bundle(99, 1)
            self.assertTrue(out1["log_dir"].endswith("/r1/a1"))
            # Materialize the first attempt dir as the orchestrator would.
            Path(out1["log_dir"]).mkdir(parents=True, exist_ok=True)
            out2 = pr_ops.round_bundle(99, 1)
        self.assertTrue(out2["log_dir"].endswith("/r1/a2"))
        for backend in ("codex", "cursor", "gemini", "lint"):
            self.assertTrue(
                out2["envelope_paths"][backend].endswith(
                    f"/r1/a2/{backend}-envelope.json"
                )
            )

    def test_returns_goal_text_on_round_two_with_prior_actions(self) -> None:
        pr_ops.state_append_round(99, 1, "sha1", verify_head=False)
        pr_ops.state_finalize_round(
            99, 1, "sha1f",
            [{"comment_id": 1, "action": "fixed", "severity": "high",
              "path": "a.go", "line": 5, "source": "codex", "topic": "bug"}],
        )
        pr_ops.state_append_round(99, 2, "sha1f", verify_head=False)

        def fake_run(cmd, **kwargs):
            if cmd[:2] == ["git", "rev-parse"]:
                return _common.RunResult(stdout="sha1f\n", stderr="", returncode=0)
            return _common.RunResult(stdout="", stderr="", returncode=1)
        with patch.object(pr_ops, "run", side_effect=fake_run):
            out = pr_ops.round_bundle(99, 2)
        # Round 2 with prior actions: goal_text references the prior round.
        self.assertIn("Round 2", out["goal_text"])
        self.assertIn("a.go:5", out["goal_text"])


class TestFinalizeAndReport(unittest.TestCase):
    """finalize-and-report wraps state_finalize_round and emits the
    one-shot audit-trail digest the orchestrator displays per-round."""

    def setUp(self) -> None:
        self._tmp = tempfile.TemporaryDirectory()
        self.tmp = Path(self._tmp.name)
        self._patch_home = patch.dict(os.environ, {"HOME": str(self.tmp)})
        self._patch_home.start()
        import importlib
        importlib.reload(pr_ops)
        self.envelope_dir = self.tmp / "envelopes"
        self.envelope_dir.mkdir()

    def tearDown(self) -> None:
        self._patch_home.stop()
        self._tmp.cleanup()
        import importlib
        importlib.reload(pr_ops)

    def _write_envelope(self, backend: str, **kw) -> Path:
        obj = {
            "status": "ok",
            "backend": backend,
            "review": {
                "verdict": kw.get("verdict", "rejected"),
                "issues": kw.get("issues", []),
            },
        }
        if "sufficiency" in kw:
            obj["review"]["sufficiency"] = kw["sufficiency"]
        path = self.envelope_dir / f"{backend}-envelope.json"
        path.write_text(json.dumps(obj))
        return path

    def test_low_only_streak_two_signals_converged(self) -> None:
        # Two consecutive low-only rounds: existing low_only_streak rule
        # fires. converged_signal == True so the orchestrator can mark
        # complete without grepping state.
        pr_ops.state_append_round(99, 1, "sha", verify_head=False)
        pr_ops.state_finalize_round(
            99, 1, "sha1f",
            [{"comment_id": 1, "action": "ack", "severity": "low"}],
        )
        pr_ops.state_append_round(99, 2, "sha1f", verify_head=False)
        out = pr_ops.finalize_and_report(
            99, 2, "sha2f",
            [{"comment_id": 2, "action": "ack", "severity": "low"}],
        )
        self.assertEqual(out["low_only_streak"], 2)
        self.assertEqual(out["converged_signal"], True)
        self.assertEqual(out["exit_reason_hint"], "converged")

    def test_high_severity_round_does_not_signal_converged(self) -> None:
        pr_ops.state_append_round(99, 1, "sha", verify_head=False)
        out = pr_ops.finalize_and_report(
            99, 1, "sha1f",
            [{"comment_id": 1, "action": "fixed", "severity": "high",
              "commit_sha": "sha1f"}],
        )
        self.assertIsNone(out["converged_signal"])
        self.assertIsNone(out["exit_reason_hint"])

    def test_sufficiency_consensus_when_both_backends_agree(self) -> None:
        cx = self._write_envelope(
            "codex",
            verdict="accepted",
            sufficiency={"is_confident_complete": True, "evidence": "ok"},
        )
        cu = self._write_envelope(
            "cursor",
            verdict="accepted",
            sufficiency={"is_confident_complete": True, "evidence": "lgtm"},
        )
        pr_ops.state_append_round(99, 1, "sha", verify_head=False)
        out = pr_ops.finalize_and_report(
            99, 1, "sha1f", [],
            envelope_overrides={"codex": cx, "cursor": cu},
        )
        self.assertEqual(out["sufficiency_consensus"], True)
        self.assertIn("both backends signalled sufficiency", out["round_summary"])

    def test_sufficiency_consensus_false_when_one_dissents(self) -> None:
        cx = self._write_envelope(
            "codex",
            verdict="accepted",
            sufficiency={"is_confident_complete": True},
        )
        cu = self._write_envelope(
            "cursor",
            verdict="rejected",
            issues=[{"severity": "medium", "file": "a.go", "line": 1, "message": "m"}],
            sufficiency={"is_confident_complete": False, "evidence": "more sites"},
        )
        pr_ops.state_append_round(99, 1, "sha", verify_head=False)
        out = pr_ops.finalize_and_report(
            99, 1, "sha1f", [],
            envelope_overrides={"codex": cx, "cursor": cu},
        )
        self.assertEqual(out["sufficiency_consensus"], False)
        self.assertIn("one backend signalled more sites remain", out["round_summary"])

    def test_round_summary_shape(self) -> None:
        pr_ops.state_append_round(99, 1, "sha", verify_head=False)
        out = pr_ops.finalize_and_report(
            99, 1, "sha1f",
            [{"comment_id": 1, "action": "fixed", "severity": "medium",
              "commit_sha": "sha1f"}],
        )
        self.assertIn("Round 1", out["round_summary"])
        self.assertIn("top=medium", out["round_summary"])
        self.assertIn("fixed 1", out["round_summary"])
        self.assertEqual(out["next_round_n"], 2)


if __name__ == "__main__":
    unittest.main()
