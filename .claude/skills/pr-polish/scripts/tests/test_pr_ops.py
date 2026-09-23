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
import bramble_ops  # noqa: E402
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


class TestLoadActions(unittest.TestCase):
    """_load_actions accepts the bare-array form AND the {comment_actions:[...]}
    form (the shape the SKILL.md State-tracking section implies — a model that
    builds either must not hit a wasted-turn parse error), and validates the
    closed-enum fields with a loud, indexed error."""

    def _write(self, obj: object) -> Path:
        fd, name = tempfile.mkstemp(suffix=".json")
        os.close(fd)
        p = Path(name)
        self.addCleanup(p.unlink)
        p.write_text(json.dumps(obj))
        return p

    def test_accepts_bare_array(self) -> None:
        got = pr_ops._load_actions(
            self._write([{"action": "fixed", "severity": "high"}])
        )
        self.assertEqual(got, [{"action": "fixed", "severity": "high"}])

    def test_accepts_comment_actions_object(self) -> None:
        got = pr_ops._load_actions(
            self._write({"comment_actions": [{"action": "ack", "severity": None}]})
        )
        self.assertEqual(got, [{"action": "ack", "severity": None}])

    def test_allows_unknown_optional_keys(self) -> None:
        # Forward-compat: v2 fields like invariant/spiral_refix must pass through.
        entry = {"action": "fixed", "invariant": "rule-x", "spiral_refix": True}
        self.assertEqual(pr_ops._load_actions(self._write([entry])), [entry])

    def test_rejects_scalar_with_clear_message(self) -> None:
        with self.assertRaises(ValueError) as cm:
            pr_ops._load_actions(self._write(42))
        self.assertIn("JSON array", str(cm.exception))
        self.assertIn("comment_actions", str(cm.exception))

    def test_rejects_object_without_comment_actions(self) -> None:
        # A dict that ISN'T the comment_actions wrapper is not a valid actions file.
        with self.assertRaises(ValueError):
            pr_ops._load_actions(self._write({"rounds": []}))

    def test_rejects_unknown_action_naming_index(self) -> None:
        with self.assertRaises(ValueError) as cm:
            pr_ops._load_actions(
                self._write([{"action": "fixed"}, {"action": "bogus"}])
            )
        msg = str(cm.exception)
        self.assertIn("actions[1].action", msg)
        self.assertIn("bogus", msg)

    def test_rejects_unknown_severity_naming_index(self) -> None:
        with self.assertRaises(ValueError) as cm:
            pr_ops._load_actions(self._write([{"severity": "critical"}]))
        self.assertIn("actions[0].severity", str(cm.exception))

    def test_rejects_non_dict_entry(self) -> None:
        with self.assertRaises(ValueError) as cm:
            pr_ops._load_actions(self._write(["not-an-object"]))
        self.assertIn("actions[0]", str(cm.exception))

    def test_known_actions_matches_buckets(self) -> None:
        # KNOWN_ACTIONS must stay the union of the classification buckets so the
        # validator can't drift from what _top_severity / counting recognize.
        self.assertEqual(
            pr_ops.KNOWN_ACTIONS, pr_ops.FIXED_ACTIONS | pr_ops.SKIPPED_ACTIONS
        )


class TestPreflightBrambleWarning(unittest.TestCase):
    """preflight emits a non-fatal warning when the branch reviews bramble's own
    code but bramble_bin fell back to the PATH binary (likely a stale build from
    another worktree)."""

    def test_reviewing_bramble_detects_self_prefixes(self) -> None:
        with patch.object(
            pr_ops,
            "changed_files",
            return_value=["yoloswe/reviewer/backend.go", "README.md"],
        ):
            self.assertTrue(pr_ops._reviewing_bramble_itself("main"))
        with patch.object(
            pr_ops, "changed_files", return_value=["bramble/cmd/codereview/x.go"]
        ):
            self.assertTrue(pr_ops._reviewing_bramble_itself("main"))
        with patch.object(pr_ops, "changed_files", return_value=["docs/x.md"]):
            self.assertFalse(pr_ops._reviewing_bramble_itself("main"))
        with patch.object(pr_ops, "changed_files", return_value=[]):
            self.assertFalse(pr_ops._reviewing_bramble_itself("main"))

    def _run_preflight(self, *, cwd: Path, changed: list[str]) -> dict:
        # Force the PATH fallback by running preflight from a cwd with no
        # bazel-bin build, and make the resume-support + git-sync probes cheap.
        with (
            patch.object(pr_ops.Path, "cwd", return_value=cwd),
            patch.object(pr_ops, "changed_files", return_value=changed),
            patch.object(pr_ops, "detect_base_branch", return_value="main"),
            patch.object(
                pr_ops.subprocess,
                "run",
                return_value=subprocess.CompletedProcess(
                    args=[], returncode=0, stdout="--resume-session-id\n", stderr=""
                ),
            ),
        ):
            return pr_ops.preflight()

    def test_warns_when_reviewing_bramble_on_path_fallback(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            out = self._run_preflight(
                cwd=Path(d), changed=["yoloswe/reviewer/backend.go"]
            )
        self.assertEqual(out["bramble_bin"], "bramble")
        self.assertTrue(
            any("stale code" in w for w in out["warnings"]),
            f"expected a stale-bramble warning, got {out['warnings']}",
        )

    def test_no_warning_when_diff_untouched_by_bramble(self) -> None:
        with tempfile.TemporaryDirectory() as d:
            out = self._run_preflight(cwd=Path(d), changed=["docs/readme.md"])
        self.assertEqual(out["warnings"], [])


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
        # `notes` is the other documented spelling of the same field, so a
        # rationale filed there reaches the PR thread too.
        body_notes = pr_ops._reply_body(
            {"action": "wont_fix", "notes": "design tradeoff"}, "abc123fdeadbeef"
        )
        self.assertIn("Won't fix: design tradeoff", body_notes)

    def test_open_deferral_reads_either_reason_field(self) -> None:
        # SKILL.md documents `notes`/`reason` as interchangeable; reading only
        # `reason` left a reasoned wont_fix looking like a bare deferral, which
        # suppresses the convergence hint for the rest of the run.
        self.assertFalse(
            pr_ops._action_is_open_deferral(
                {"action": "wont_fix", "notes": "validated upstream"}
            )
        )
        self.assertTrue(
            pr_ops._action_is_open_deferral({"action": "wont_fix", "notes": "  "})
        )
        self.assertTrue(
            pr_ops._action_is_open_deferral({"action": "ack", "notes": "seen"})
        )


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
        for backend in bramble_ops.BACKENDS:
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
        for backend in bramble_ops.BACKENDS:
            self.assertTrue(
                out2["envelope_paths"][backend].endswith(
                    f"/r1/a2/{backend}-envelope.json"
                )
            )

    def test_next_attempt_empty_round_is_one(self) -> None:
        self.assertEqual(pr_ops._next_attempt(self.tmp, 1), 1)

    def test_next_attempt_only_counts_numbered_attempt_dirs(self) -> None:
        # A non-attempt dir whose name merely starts with `a` must not
        # bump the index — otherwise the orchestrator skips an attempt
        # number and a fresh review could land in a dir name that an
        # unrelated dir already pushed past.
        round_dir = self.tmp / "r1"
        (round_dir / "a1").mkdir(parents=True)
        (round_dir / "archive").mkdir()
        (round_dir / "aux").mkdir()
        self.assertEqual(pr_ops._next_attempt(self.tmp, 1), 2)

    def test_next_attempt_uses_max_not_count(self) -> None:
        # With a gap (a1 deleted, a2 kept) the next index must be a free
        # one (3), not count+1 (2) which would collide with the live a2.
        round_dir = self.tmp / "r1"
        (round_dir / "a2").mkdir(parents=True)
        self.assertEqual(pr_ops._next_attempt(self.tmp, 1), 3)

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

    def test_restarted_series_gets_fresh_goal_and_resume_ids(self) -> None:
        # A converged loop that is re-invoked starts a NEW series, and a new
        # series must not inherit the prior one's goal or bramble sessions.
        # The boundary signal is only readable BEFORE state_append_round
        # clears `completed`, so round_bundle cannot re-derive it from the
        # post-append state — by then every new series looks like a
        # continuation. Symptom in production: round N's goal opens with
        # "Files changed since round N-1" computed across the prior series'
        # head, dragging unrelated files into the review scope.
        pr_ops.state_append_round(99, 1, "sha1", verify_head=False,
                                  pr_summary="PR #99: the frozen purpose")
        pr_ops.state_finalize_round(
            99, 1, "sha1f",
            [{"comment_id": 1, "action": "fixed", "severity": "high",
              "path": "a.go", "line": 5, "source": "codex", "topic": "bug"}],
            auto_reply=False,
        )
        # Seed the prior series' bramble session so a leaked resume is visible.
        _, state_file = pr_ops.state_paths(99)
        seeded = _common.read_json(state_file, default={})
        seeded["rounds"][0]["session_ids"] = {"codex": "prior-series-session"}
        _common.atomic_write_json(state_file, seeded)
        pr_ops.state_mark_complete(99, "converged")

        # New loop, new series: round 2 is the first round after completion.
        pr_ops.state_append_round(99, 2, "sha1f", verify_head=False)

        def fake_run(cmd, **kwargs):
            if cmd[:2] == ["git", "rev-parse"]:
                return _common.RunResult(stdout="sha1f\n", stderr="", returncode=0)
            return _common.RunResult(stdout="", stderr="", returncode=1)
        with patch.object(pr_ops, "run", side_effect=fake_run):
            out = pr_ops.round_bundle(99, 2)

        # Fresh session, not the prior series' conversation.
        self.assertEqual(out["resume_ids"].get("codex", ""), "")
        # The goal is the PR's frozen purpose, not an inter-round delta
        # measured against the prior series' head.
        self.assertIn("the frozen purpose", out["goal_text"])
        self.assertNotIn("Files changed since round 1", out["goal_text"])


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

    def test_prior_round_bare_ack_high_blocks_converged_signal(self) -> None:
        # SKILL.md Step 3.g: a high/critical finding bare-ack'd in a prior
        # round is still open and must keep the loop running, even when the
        # current + subsequent rounds are low-only and low_only_streak >= 2
        # would otherwise fire. Without the deferred-high guard the
        # orchestrator would follow a False converged hint and exit early.
        pr_ops.state_append_round(99, 1, "sha", verify_head=False)
        pr_ops.state_finalize_round(
            99, 1, "sha1f",
            [{"comment_id": 1, "action": "ack", "severity": "high"}],
        )
        pr_ops.state_append_round(99, 2, "sha1f", verify_head=False)
        pr_ops.state_finalize_round(
            99, 2, "sha2f",
            [{"comment_id": 2, "action": "ack", "severity": "low"}],
        )
        pr_ops.state_append_round(99, 3, "sha2f", verify_head=False)
        out = pr_ops.finalize_and_report(
            99, 3, "sha3f",
            [{"comment_id": 3, "action": "ack", "severity": "low"}],
        )
        # low_only_streak reached 2, but the round-1 deferred high suppresses it.
        self.assertEqual(out["low_only_streak"], 2)
        self.assertIsNone(out["converged_signal"])
        self.assertIsNone(out["exit_reason_hint"])

    def test_wont_fix_high_with_reason_does_not_block_converged(self) -> None:
        # A wont_fix carrying a real rationale is a resolution, not a
        # deferral — it must NOT keep the loop open. Contrast with a bare
        # ack, which does (test above). The high wont_fix lands in round 1
        # (so it isn't the current round's top severity), then two low-only
        # rounds bring low_only_streak to 2; convergence must fire.
        pr_ops.state_append_round(99, 1, "sha", verify_head=False)
        pr_ops.state_finalize_round(
            99, 1, "sha1f",
            [{"comment_id": 1, "action": "wont_fix", "severity": "high",
              "reason": "intentional: perf tradeoff documented in ADR-7"}],
        )
        pr_ops.state_append_round(99, 2, "sha1f", verify_head=False)
        pr_ops.state_finalize_round(
            99, 2, "sha2f",
            [{"comment_id": 2, "action": "ack", "severity": "low"}],
        )
        pr_ops.state_append_round(99, 3, "sha2f", verify_head=False)
        out = pr_ops.finalize_and_report(
            99, 3, "sha3f",
            [{"comment_id": 3, "action": "ack", "severity": "low"}],
        )
        self.assertEqual(out["low_only_streak"], 2)
        self.assertEqual(out["converged_signal"], True)
        self.assertEqual(out["exit_reason_hint"], "converged")

    def test_bare_ack_high_then_fixed_allows_converged(self) -> None:
        # Recovery path: a high finding bare-ack'd in round 1 but FIXED on the
        # same comment_id in round 2 is resolved — the deferral guard resolves
        # each finding to its latest action, so it must NOT block convergence
        # forever. Round 2's top severity is still high (the fix action), so the
        # low-only streak starts after it: rounds 3-4 low-only reach streak 2.
        pr_ops.state_append_round(99, 1, "sha", verify_head=False)
        pr_ops.state_finalize_round(
            99, 1, "sha1f",
            [{"comment_id": 7, "action": "ack", "severity": "high"}],
        )
        pr_ops.state_append_round(99, 2, "sha1f", verify_head=False)
        pr_ops.state_finalize_round(
            99, 2, "sha2f",
            [{"comment_id": 7, "action": "fixed", "severity": "high",
              "commit_sha": "sha2f"}],
        )
        pr_ops.state_append_round(99, 3, "sha2f", verify_head=False)
        pr_ops.state_finalize_round(
            99, 3, "sha3f",
            [{"comment_id": 8, "action": "ack", "severity": "low"}],
        )
        pr_ops.state_append_round(99, 4, "sha3f", verify_head=False)
        out = pr_ops.finalize_and_report(
            99, 4, "sha4f",
            [{"comment_id": 9, "action": "ack", "severity": "low"}],
        )
        self.assertEqual(out["low_only_streak"], 2)
        self.assertEqual(out["converged_signal"], True)
        self.assertEqual(out["exit_reason_hint"], "converged")

    def test_bare_ack_high_then_fixed_via_kpl_key_allows_converged(self) -> None:
        # Same recovery path as above but for findings WITHOUT a comment_id:
        # _action_key falls back to (source, path, line, topic). Round-1 acks
        # and round-2 fixes the SAME kpl identity, so latest-action resolution
        # must clear the deferral and let convergence fire.
        kpl = {"source": "cursor", "path": "a.py", "line": 5, "topic": "leak"}
        pr_ops.state_append_round(99, 1, "sha", verify_head=False)
        pr_ops.state_finalize_round(
            99, 1, "sha1f", [{**kpl, "action": "ack", "severity": "high"}],
        )
        pr_ops.state_append_round(99, 2, "sha1f", verify_head=False)
        pr_ops.state_finalize_round(
            99, 2, "sha2f",
            [{**kpl, "action": "fixed", "severity": "high", "commit_sha": "sha2f"}],
        )
        pr_ops.state_append_round(99, 3, "sha2f", verify_head=False)
        pr_ops.state_finalize_round(
            99, 3, "sha3f",
            [{"source": "cursor", "path": "b.py", "line": 1, "topic": "nit",
              "action": "ack", "severity": "low"}],
        )
        pr_ops.state_append_round(99, 4, "sha3f", verify_head=False)
        out = pr_ops.finalize_and_report(
            99, 4, "sha4f",
            [{"source": "cursor", "path": "c.py", "line": 1, "topic": "nit2",
              "action": "ack", "severity": "low"}],
        )
        self.assertEqual(out["low_only_streak"], 2)
        self.assertEqual(out["converged_signal"], True)
        self.assertEqual(out["exit_reason_hint"], "converged")

    def test_bare_ack_high_via_kpl_key_blocks_converged(self) -> None:
        # Negative kpl counterpart: a high finding bare-ack'd via the kpl key
        # and never fixed keeps the loop open across a low-only streak.
        kpl = {"source": "cursor", "path": "a.py", "line": 5, "topic": "leak"}
        pr_ops.state_append_round(99, 1, "sha", verify_head=False)
        pr_ops.state_finalize_round(
            99, 1, "sha1f", [{**kpl, "action": "ack", "severity": "high"}],
        )
        pr_ops.state_append_round(99, 2, "sha1f", verify_head=False)
        pr_ops.state_finalize_round(
            99, 2, "sha2f",
            [{"source": "cursor", "path": "b.py", "line": 1, "topic": "nit",
              "action": "ack", "severity": "low"}],
        )
        pr_ops.state_append_round(99, 3, "sha2f", verify_head=False)
        out = pr_ops.finalize_and_report(
            99, 3, "sha3f",
            [{"source": "cursor", "path": "c.py", "line": 1, "topic": "nit2",
              "action": "ack", "severity": "low"}],
        )
        self.assertEqual(out["low_only_streak"], 2)
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
        self.assertIn("review=", out["round_summary"])
        self.assertIn("orchestrator=", out["round_summary"])
        self.assertEqual(out["next_round_n"], 2)

    def _quiet(self, n: int, root: str, *, severity: str = "medium") -> dict:
        return {
            "comment_id": n,
            "action": "ack",
            "severity": severity,
            "topic": root,
        }

    def test_early_convergence_stops_before_cap_with_justification(self) -> None:
        # Medium findings reset low_only_streak, so the older rules would
        # run this to the cap. Two quiet rounds that only restate a root
        # named in round 1 are not new signal.
        root = "parser-root"
        pr_ops.state_append_round(99, 1, "sha", verify_head=False)
        pr_ops.state_finalize_round(
            99, 1, "sha1",
            [{"comment_id": 1, "action": "fixed", "severity": "high",
              "topic": root, "commit_sha": "sha1"}],
        )
        pr_ops.state_append_round(99, 2, "sha1", verify_head=False)
        pr_ops.state_finalize_round(99, 2, "sha2", [self._quiet(2, root)])
        pr_ops.state_append_round(99, 3, "sha2", verify_head=False)
        out = pr_ops.finalize_and_report(99, 3, "sha3", [self._quiet(3, root)])
        self.assertEqual(out["exit_reason_hint"], "early-convergence")
        self.assertTrue(out["converged_signal"])
        justification = out["convergence_justification"]
        self.assertIn("parser-root", justification)
        self.assertIn("round 1", justification)
        self.assertLess(out["next_round_n"] - 1, 5)  # stopped before the default cap
        done = pr_ops.state_mark_complete(
            99, "early-convergence", justification=justification,
        )
        self.assertEqual(done["exit_reason"], "early-convergence")
        self.assertEqual(done["exit_justification"], justification)
        self.assertEqual(len(done["rounds"]), 3)

    def test_early_convergence_requires_two_quiet_rounds_and_one_root(self) -> None:
        root = "parser-root"
        pr_ops.state_append_round(99, 1, "sha", verify_head=False)
        pr_ops.state_finalize_round(
            99, 1, "sha1",
            [{"comment_id": 1, "action": "fixed", "severity": "high",
              "topic": root, "commit_sha": "sha1"}],
        )
        pr_ops.state_append_round(99, 2, "sha1", verify_head=False)
        one = pr_ops.finalize_and_report(99, 2, "sha2", [self._quiet(2, root)])
        self.assertIsNone(one["converged_signal"])

        pr_ops.state_append_round(99, 3, "sha2", verify_head=False)
        split = pr_ops.finalize_and_report(
            99, 3, "sha3",
            [self._quiet(3, root), self._quiet(4, "other-root")],
        )
        self.assertIsNone(split["converged_signal"])

    def test_early_convergence_blocked_by_high_or_regression(self) -> None:
        root = "parser-root"
        pr_ops.state_append_round(99, 1, "sha", verify_head=False)
        pr_ops.state_finalize_round(
            99, 1, "sha1",
            [{"comment_id": 1, "action": "ack", "severity": "medium", "topic": root}],
        )
        pr_ops.state_append_round(99, 2, "sha1", verify_head=False)
        pr_ops.state_finalize_round(99, 2, "sha2", [self._quiet(2, root)])
        pr_ops.state_append_round(99, 3, "sha2", verify_head=False)
        high = pr_ops.finalize_and_report(
            99, 3, "sha3", [self._quiet(3, root, severity="high")],
        )
        self.assertIsNone(high["converged_signal"])

        pr_ops.state_append_round(99, 4, "sha3", verify_head=False)
        pr_ops.state_finalize_round(99, 4, "sha4", [self._quiet(4, root)])
        pr_ops.state_append_round(99, 5, "sha4", verify_head=False)
        regressed = dict(self._quiet(5, root), spiral_refix=True)
        out = pr_ops.finalize_and_report(99, 5, "sha5", [regressed])
        self.assertIsNone(out["converged_signal"])

    def test_round_summary_splits_review_and_orchestrator_time(self) -> None:
        pr_ops.state_append_round(99, 1, "sha", verify_head=False)
        _, path = pr_ops.state_paths(99)
        state = json.loads(path.read_text())
        state["rounds"][0]["opened_at_ms"] = pr_ops._epoch_ms() - 7_200_000
        path.write_text(json.dumps(state))
        out = pr_ops.finalize_and_report(
            99, 1, "sha1f",
            [{"comment_id": 1, "action": "ack", "severity": "low"}],
            review_wall_ms=420_000,
        )
        self.assertEqual(out["review_wall_ms"], 420_000)
        self.assertGreater(out["orchestrator_wall_ms"], 6_000_000)
        self.assertIn("review=7m0s", out["round_summary"])
        self.assertIn("orchestrator=", out["round_summary"])
        self.assertLess(
            out["review_wall_ms"],
            out["orchestrator_wall_ms"],
        )


class TestPRSummaryReachesStateViaCLI(unittest.TestCase):
    """The frozen PR_SUMMARY must survive the CLI, not just the Python call.

    SKILL.md drives state-append-round as a subprocess, so a parameter that
    exists on the function but has no argparse flag is dead code in production.
    That is exactly what shipped: `state_append_round(pr_summary=...)` froze the
    value, `round_bundle` read it back, and 407 unit tests passed — while the
    CLI never declared `--pr-summary` and never passed it, so `state` never held
    one and the goal was as purposeless as before the fix.

    Tests the boundary the orchestrator actually crosses.
    """

    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        os.environ["HOME"] = self.tmp.name

    def test_cli_flag_persists_pr_summary(self):
        rc = pr_ops.main([
            "state-append-round", "branch:x", "1", "deadbeef",
            "--no-verify-head", "--pr-summary", "PR GOAL TEXT",
        ])
        self.assertEqual(rc, 0)
        _, path = pr_ops.state_paths(None, branch="x")
        state = json.loads(Path(path).read_text())
        self.assertEqual(state.get("pr_summary"), "PR GOAL TEXT",
                         "the CLI must persist it, or round_bundle reads '' forever")

    def test_frozen_not_overwritten_by_later_rounds(self):
        pr_ops.main(["state-append-round", "branch:x", "1", "aaa",
                     "--no-verify-head", "--pr-summary", "ORIGINAL"])
        pr_ops.main(["state-append-round", "branch:x", "2", "bbb",
                     "--no-verify-head", "--pr-summary", "DRIFTED"])
        _, path = pr_ops.state_paths(None, branch="x")
        state = json.loads(Path(path).read_text())
        self.assertEqual(state.get("pr_summary"), "ORIGINAL",
                         "frozen means round 1 wins; re-deriving would let a "
                         "ratcheting PR re-authorize its own growth")

    def test_new_series_re_anchors_the_frozen_summary(self):
        """A completed series is a fresh look; the old summary may be stale.

        Observed on this PR: after a squash + force-push, round 2 opened a new
        series but kept a summary describing half the PR and citing a commit
        that no longer existed, so reviewers judged scope against stale intent.
        Re-anchoring is safe exactly here — the prior series' action history is
        already unreachable, so there is no ratchet to re-authorize.
        """
        pr_ops.main(["state-append-round", "branch:x", "1", "aaa",
                     "--no-verify-head", "--pr-summary", "ORIGINAL"])
        pr_ops.main(["state-finalize-round", "branch:x", "1", "aaa",
                     self._actions_file()])
        pr_ops.main(["state-mark-complete", "branch:x", "converged"])
        # New series: prior loop completed.
        pr_ops.main(["state-append-round", "branch:x", "2", "bbb",
                     "--no-verify-head", "--pr-summary", "REWRITTEN"])
        _, path = pr_ops.state_paths(None, branch="x")
        state = json.loads(Path(path).read_text())
        self.assertEqual(state.get("pr_summary"), "REWRITTEN",
                         "a new series must re-anchor; the old summary can "
                         "describe a branch that no longer exists")

    def test_new_series_re_anchors_base_branch(self):
        """Same boundary: a PR retargeted between series must re-measure."""
        pr_ops.main(["state-append-round", "branch:x", "1", "aaa",
                     "--no-verify-head", "--base-branch", "release/v1"])
        pr_ops.main(["state-finalize-round", "branch:x", "1", "aaa",
                     self._actions_file()])
        pr_ops.main(["state-mark-complete", "branch:x", "converged"])
        pr_ops.main(["state-append-round", "branch:x", "2", "bbb",
                     "--no-verify-head", "--base-branch", "main"])
        _, path = pr_ops.state_paths(None, branch="x")
        state = json.loads(Path(path).read_text())
        self.assertEqual(state.get("base_branch"), "main")

    def test_mid_series_still_frozen_after_new_series_change(self):
        """The anti-ratchet freeze must survive the re-anchoring change.

        Guard against 'fix staleness' quietly becoming 'refresh every round',
        which is the drift the scope contract exists to prevent.
        """
        pr_ops.main(["state-append-round", "branch:x", "1", "aaa",
                     "--no-verify-head", "--pr-summary", "ORIGINAL",
                     "--base-branch", "release/v1"])
        # No mark-complete: rounds 2 and 3 are the SAME series.
        for n, head in (("2", "bbb"), ("3", "ccc")):
            pr_ops.main(["state-append-round", "branch:x", n, head,
                         "--no-verify-head", "--pr-summary", "DRIFTED",
                         "--base-branch", "main"])
        _, path = pr_ops.state_paths(None, branch="x")
        state = json.loads(Path(path).read_text())
        self.assertEqual(state.get("pr_summary"), "ORIGINAL")
        self.assertEqual(state.get("base_branch"), "release/v1")

    def test_frozen_summary_reaches_round_bundle_goal_text(self):
        """The consumer hop, not just the write.

        Persistence tests pass even if `round_bundle` goes back to passing
        `pr_summary=""` — that is precisely the regression this PR fixes, and
        it lived through 407 green tests. Assert on the value the orchestrator
        actually hands bramble: `round_bundle(...)["goal_text"]`.
        """
        pr_ops.main(["state-append-round", "branch:x", "1", "aaa",
                     "--no-verify-head", "--pr-summary", "FROZEN-PURPOSE"])
        pr_ops.main(["state-finalize-round", "branch:x", "1", "aaa",
                     self._actions_file()])
        out = pr_ops.round_bundle("branch:x", 2)
        self.assertTrue(
            out["goal_text"].startswith("FROZEN-PURPOSE"),
            "round 2's goal must LEAD with the frozen summary; got: "
            f"{out['goal_text'][:120]!r}",
        )

    def test_cli_flag_persists_base_branch(self):
        """The file-set anchor must be the PR's base, not the repo default.

        Without this the 'Files in this PR' range falls back to origin/HEAD,
        so a PR stacked on a non-default branch is measured against a
        different ancestor than its own PR_SUMMARY diffstat.
        """
        rc = pr_ops.main([
            "state-append-round", "branch:x", "1", "deadbeef",
            "--no-verify-head", "--base-branch", "release/v2",
        ])
        self.assertEqual(rc, 0)
        _, path = pr_ops.state_paths(None, branch="x")
        state = json.loads(Path(path).read_text())
        self.assertEqual(state.get("base_branch"), "release/v2",
                         "the CLI must persist the base, or the file-set range "
                         "silently re-anchors to the repo default")

    def test_base_branch_frozen_not_overwritten(self):
        pr_ops.main(["state-append-round", "branch:x", "1", "aaa",
                     "--no-verify-head", "--base-branch", "release/v2"])
        pr_ops.main(["state-append-round", "branch:x", "2", "bbb",
                     "--no-verify-head", "--base-branch", "main"])
        _, path = pr_ops.state_paths(None, branch="x")
        state = json.loads(Path(path).read_text())
        self.assertEqual(state.get("base_branch"), "release/v2",
                         "the remit's base is fixed at round 1 like pr_summary")

    def test_frozen_base_branch_drives_the_file_set_range(self):
        """The consumer hop for base_branch, not just the write.

        Persistence coverage alone leaves the same hole `--pr-summary` had:
        severing `base_branch` from round_bundle -> goal_for_round ->
        action_history_goal -> _files_changed_in_pr keeps every test green
        while "Files in this PR" silently re-anchors to the repo default.
        Assert on the git range actually executed.
        """
        pr_ops.main(["state-append-round", "branch:x", "1", "aaa",
                     "--no-verify-head", "--pr-summary", "SUMMARY",
                     "--base-branch", "release/v2"])
        pr_ops.main(["state-finalize-round", "branch:x", "1", "aaa",
                     self._actions_file()])

        seen: list[list[str]] = []
        real_run = _common.run

        def spy(cmd, *a, **kw):
            if cmd[:3] == ["git", "diff", "--name-only"]:
                seen.append(cmd)
            return real_run(cmd, *a, **kw)

        with patch.object(_common, "run", spy):
            pr_ops.round_bundle("branch:x", 2)

        ranges = [c[3] for c in seen if len(c) > 3]
        self.assertTrue(
            any(r.startswith("origin/release/v2...") for r in ranges),
            "the file-set range must use the FROZEN base (origin/release/v2...), "
            f"not the repo default; ranges executed: {ranges}",
        )

    def _actions_file(self):
        p = Path(self.tmp.name) / "actions.json"
        p.write_text(json.dumps([]))
        return str(p)


# Shape of a real PR body: authored prose, then a bot block after a rule.
_REAL_BODY = """## Core

Adds `claude` as a fourth `bramble code-review` backend (default model opus).

## Verification

- Resume: `[resume=ok]` against a live session id.

<!-- CURSOR_SUMMARY -->
---

> [!NOTE]
> **Medium Risk**
> New review backend and pr-polish orchestration affect automation.
>
> **Overview**
> Adds **`claude`** as a fourth backend, wired through `backend_claude.go`.
>
> <sup>Reviewed by [Cursor Bugbot](https://cursor.com/bugbot) for commit 608d8a6.</sup>
<!-- /CURSOR_SUMMARY -->"""


class TestStripPRBody(unittest.TestCase):
    """Pure function: no gh/git."""

    def test_strips_cursor_summary_block(self) -> None:
        cleaned, dropped = pr_ops.strip_pr_body(_REAL_BODY)
        self.assertIn("cursor-summary", dropped)
        self.assertNotIn("CURSOR_SUMMARY", cleaned)
        self.assertNotIn("Cursor Bugbot", cleaned)
        self.assertNotIn("Medium Risk", cleaned)

    def test_preserves_author_intent_verbatim(self) -> None:
        cleaned, _ = pr_ops.strip_pr_body(_REAL_BODY)
        self.assertIn("## Core", cleaned)
        self.assertIn("Adds `claude` as a fourth", cleaned)
        self.assertIn("## Verification", cleaned)
        self.assertIn("[resume=ok]", cleaned)

    def test_revert_to_prove_block_pattern_is_load_bearing(self) -> None:
        """Without the block pattern the bot text survives — proves reachability."""
        with patch.object(pr_ops, "_BODY_BLOCK_PATTERNS", ()):
            cleaned, dropped = pr_ops.strip_pr_body(_REAL_BODY)
        self.assertIn("Cursor Bugbot", cleaned)
        self.assertNotIn("cursor-summary", dropped)

    def test_strips_coderabbit_release_notes(self) -> None:
        """A separate marker shape from the Cursor block, which misses it."""
        body = (
            "## What\n\nReal intent.\n\n"
            "<!-- This is an auto-generated comment: release notes by coderabbit.ai -->\n"
            "## Summary by CodeRabbit\n\n* **New Features**\n"
            "  * Added APIs to list, create, update, and delete bindings.\n"
            "<!-- end of auto-generated comment: release notes by coderabbit.ai -->\n"
        )
        cleaned, dropped = pr_ops.strip_pr_body(body)
        self.assertIn("coderabbit-release-notes", dropped)
        self.assertNotIn("Summary by CodeRabbit", cleaned)
        self.assertNotIn("New Features", cleaned)
        self.assertIn("Real intent.", cleaned)

    def test_author_prose_mentioning_coderabbit_survives(self) -> None:
        """An author explaining bot behavior is review input, not bot output."""
        body = (
            "## What\n\n**A stacked PR cannot pass this gate.** CodeRabbit skips "
            "non-default bases, so neither gating bot produces a verdict.\n"
        )
        cleaned, dropped = pr_ops.strip_pr_body(body)
        self.assertEqual(dropped, [])
        self.assertIn("CodeRabbit skips", cleaned)

    def test_unclosed_block_strips_to_end(self) -> None:
        body = "Real intent here.\n\n<!-- CURSOR_SUMMARY -->\nbot text\nmore bot"
        cleaned, dropped = pr_ops.strip_pr_body(body)
        self.assertIn("cursor-summary", dropped)
        self.assertNotIn("bot text", cleaned)
        self.assertIn("Real intent here.", cleaned)

    def test_strips_generated_with_and_coauthor_lines(self) -> None:
        body = (
            "Fixes the parser.\n\n"
            "🤖 Generated with [Claude Code](https://claude.com/claude-code)\n"
            "Co-Authored-By: Claude <noreply@anthropic.com>\n"
        )
        cleaned, dropped = pr_ops.strip_pr_body(body)
        self.assertIn("generated-with", dropped)
        self.assertIn("co-authored-by", dropped)
        self.assertEqual(cleaned, "Fixes the parser.")

    def test_strips_generated_with_without_emoji(self) -> None:
        """The trailer ships both with and without the robot emoji."""
        for trailer in (
            "Generated with [Claude Code](https://claude.com/claude-code)",
            "Generated with Claude Code",
        ):
            with self.subTest(trailer=trailer):
                cleaned, dropped = pr_ops.strip_pr_body(f"Fixes the parser.\n\n{trailer}\n")
                self.assertIn("generated-with", dropped)
                self.assertEqual(cleaned, "Fixes the parser.")

    def test_generated_with_mid_sentence_survives(self) -> None:
        """Anchored to line start: prose mentioning the phrase is not a trailer."""
        body = "## What\n\nThe fixture was generated with the old script, so it drifted.\n"
        cleaned, dropped = pr_ops.strip_pr_body(body)
        self.assertEqual(dropped, [])
        self.assertIn("generated with the old script", cleaned)

    def test_prose_opening_with_trailer_words_survives(self) -> None:
        """Regression: line-start anchoring alone deleted author prose.

        A sentence may legitimately OPEN with these words. Stripping the whole
        line then eats review context — the same failure that retired the
        "## Deployment Notes" rule, so the patterns are pinned to the
        trailer's machine-emitted shape (markdown link / bare tool name;
        ``Name <email>``) rather than its opening words.
        """
        for prose in (
            "Generated with care by the platform team, this migration backfills rows.",
            "Co-Authored-By: design review, the retry limit was raised to 5.",
            "Generated with the old script, so the fixture drifted and needs a rebuild.",
        ):
            with self.subTest(prose=prose):
                cleaned, dropped = pr_ops.strip_pr_body(f"## Notes\n\n{prose}\n")
                self.assertEqual(dropped, [])
                self.assertIn(prose, cleaned)

    def test_unpunctuated_prose_opening_with_trailer_words_survives(self) -> None:
        """Regression: punctuation is not what separates prose from a trailer.

        A first pass allowed any punctuation-free tail after "Generated with",
        which still ate short unpunctuated author lines. A tool name is 1-3
        Capitalized words; connectives like "by"/"from" mark prose.
        """
        for prose in (
            "Generated with care by the platform team",
            "Generated with extensive manual testing before release",
            "Generated with input from the security review",
        ):
            with self.subTest(prose=prose):
                cleaned, dropped = pr_ops.strip_pr_body(f"## Notes\n\n{prose}\n")
                self.assertEqual(dropped, [])
                self.assertIn(prose, cleaned)

    def test_bare_tool_name_trailer_still_strips(self) -> None:
        """Narrowing the bare form must not cost the real trailers."""
        for trailer in ("Generated with Claude Code", "Generated with Claude", "Generated with Cursor"):
            with self.subTest(trailer=trailer):
                cleaned, dropped = pr_ops.strip_pr_body(f"Fixes the parser.\n\n{trailer}\n")
                self.assertIn("generated-with", dropped)
                self.assertEqual(cleaned, "Fixes the parser.")

    def test_linkback_strip_keeps_author_sections_after_it(self) -> None:
        """Regression: the linkback strip ran to EOF and ate trailing prose.

        The marker says where bot output starts, not that the author wrote
        nothing below it. Bound the strip to the `<details>` payload.
        """
        body = (
            "## Problem\n\nBroken.\n\n"
            "<!-- linear-linkback -->\n"
            "<details>\n<summary><a href=\"https://linear.app/x\">INF-437</a></summary>\n"
            "bot detail\n</details>\n\n"
            "## Verification\n\nTests pass.\n"
        )
        cleaned, dropped = pr_ops.strip_pr_body(body)
        self.assertIn("linear-linkback", dropped)
        self.assertNotIn("bot detail", cleaned)
        self.assertIn("## Verification", cleaned)
        self.assertIn("Tests pass.", cleaned)

    def test_coauthor_trailer_requires_an_address(self) -> None:
        """The real trailer carries ``Name <email>``; bare prose does not."""
        cleaned, dropped = pr_ops.strip_pr_body(
            "Fixes the parser.\n\n"
            "Co-authored-by: jiradozer-builder[bot] "
            "<283316645+jiradozer-builder[bot]@users.noreply.github.com>\n"
        )
        self.assertIn("co-authored-by", dropped)
        self.assertEqual(cleaned, "Fixes the parser.")

    def test_author_written_sections_are_never_stripped(self) -> None:
        """Regression: a heading name must not decide what a reviewer sees.

        A "## Deployment Notes" stripper was tried and removed — it had no
        true positives and ate real review input filed under that heading.
        The three kinds below are the ones it was measured destroying.
        """
        body = (
            "## What\n\nReal change.\n\n"
            "## Deployment Notes\n\n"
            "**Posture change worth its own review:** the job now reads live "
            "credentials *and* checks out PR code.\n\n"
            "Stacked on #123; review/merge that first.\n\n"
            "Migration 227 creates one tenant-scoped table; no backfill.\n\n"
            "## Verification\n\nTests pass.\n"
        )
        cleaned, dropped = pr_ops.strip_pr_body(body)
        self.assertEqual(dropped, [])
        self.assertIn("Posture change worth its own review", cleaned)
        self.assertIn("Stacked on #123", cleaned)
        self.assertIn("Migration 227", cleaned)
        self.assertIn("Real change.", cleaned)
        self.assertIn("Tests pass.", cleaned)

    def test_mentioning_deploy_in_prose_is_not_stripped(self) -> None:
        body = "## What\n\nThis changes how we deploy the binary, but is not a rollout.\n"
        cleaned, dropped = pr_ops.strip_pr_body(body)
        self.assertEqual(dropped, [])
        self.assertIn("deploy the binary", cleaned)

    def test_empty_body(self) -> None:
        self.assertEqual(pr_ops.strip_pr_body(""), ("", []))
        self.assertEqual(pr_ops.strip_pr_body(None), ("", []))


class TestBuildPRSummary(unittest.TestCase):
    def _with_state(self, body):
        with tempfile.TemporaryDirectory() as d:
            tmp_root = Path(d)

            def fake_state_paths(pr, branch=None):
                key = pr if pr is not None else f"branch-{branch}"
                pr_dir = tmp_root / f"proj-{key}"
                return pr_dir, pr_dir / "pr-polish-state.json"

            with patch.object(pr_ops, "state_paths", side_effect=fake_state_paths):
                return body()

    def _pr(self, **kw):
        base = {
            "pr_number": 311,
            "title": "fix(x): do the thing",
            "body": "",
            "base": "main",
        }
        base.update(kw)
        return base

    def test_uses_pr_body_when_usable(self) -> None:
        with patch.object(pr_ops, "_commit_diffstat_summary", return_value="COMMITS"):
            out = pr_ops.build_pr_summary(self._pr(body=_REAL_BODY))
        self.assertEqual(out["source"], "pr-body")
        self.assertIn("cursor-summary", out["dropped"])
        self.assertIn("Adds `claude` as a fourth", out["pr_summary"])
        self.assertNotIn("Cursor Bugbot", out["pr_summary"])
        # Title leads so the reviewer sees the one-line intent first.
        self.assertTrue(out["pr_summary"].startswith("fix(x): do the thing"))
        # A usable body does not drag the commit list along.
        self.assertNotIn("COMMITS", out["pr_summary"])

    def test_usable_body_does_not_shell_out_for_fallback(self) -> None:
        """The git fallback is lazy — two subprocesses per call, discarded."""
        calls: list[str] = []

        def spy(base, **kw):
            calls.append(base)
            return "COMMITS"

        with patch.object(pr_ops, "_commit_diffstat_summary", side_effect=spy):
            out = pr_ops.build_pr_summary(self._pr(body=_REAL_BODY))
        self.assertEqual(out["source"], "pr-body")
        self.assertEqual(calls, [], "fallback must not run when the body is usable")

        # ...but it still runs on the paths that need it.
        with patch.object(pr_ops, "_commit_diffstat_summary", side_effect=spy):
            pr_ops.build_pr_summary(self._pr(body=""))
        self.assertEqual(len(calls), 1)

    def test_falls_back_to_commits_when_no_body(self) -> None:
        with patch.object(pr_ops, "_commit_diffstat_summary", return_value="COMMITS"):
            out = pr_ops.build_pr_summary(self._pr(body=""))
        self.assertEqual(out["source"], "commits-diffstat")
        self.assertIn("COMMITS", out["pr_summary"])

    def test_body_that_strips_to_nothing_falls_back(self) -> None:
        """A body that is ONLY a bot block must not yield an empty goal."""
        body = "<!-- CURSOR_SUMMARY -->\nall bot, no author\n<!-- /CURSOR_SUMMARY -->"
        with patch.object(pr_ops, "_commit_diffstat_summary", return_value="COMMITS"):
            out = pr_ops.build_pr_summary(self._pr(body=body))
        self.assertEqual(out["source"], "commits-diffstat")
        self.assertIn("COMMITS", out["pr_summary"])
        self.assertNotIn("all bot", out["pr_summary"])

    def test_thin_body_keeps_intent_and_appends_diffstat(self) -> None:
        with patch.object(pr_ops, "_commit_diffstat_summary", return_value="COMMITS"):
            out = pr_ops.build_pr_summary(self._pr(body="fix typo"))
        self.assertEqual(out["source"], "pr-body+diffstat")
        self.assertIn("fix typo", out["pr_summary"])
        self.assertIn("COMMITS", out["pr_summary"])

    def test_gh_body_reaches_summary_end_to_end(self) -> None:
        """Boundary test: gh pr view's body must survive identify -> summary.

        The unit tests above build the pr dict by hand, so a regression in
        identify's --json/--jq projection (dropping `body`) would silently
        fall back to commits with every other test still green.
        """
        body = (
            "## What\n\nThis PR rewires the widget cache so stale entries "
            "cannot outlive a deploy, which is the actual bug.\n"
        )
        pr_json = json.dumps(
            {
                "pr_number": 77,
                "title": "fix(widget): expire stale cache",
                "url": "https://example.invalid/pull/77",
                "body": body,
                "base": "main",
                "head": "fix/widget",
                "head_sha": "deadbeef",
            }
        )
        seen_args: list[list[str]] = []

        def fake_run(cmd, **kwargs):
            if cmd[:2] == ["git", "rev-parse"]:
                return _common.RunResult(stdout="fix/widget\n", stderr="", returncode=0)
            if cmd[:3] == ["gh", "pr", "view"]:
                seen_args.append(cmd)
                return _common.RunResult(stdout=pr_json, stderr="", returncode=0)
            if cmd[:3] == ["gh", "repo", "view"]:
                return _common.RunResult(stdout='"acme/widgets"', stderr="", returncode=0)
            if cmd[:2] == ["git", "log"] or cmd[:2] == ["git", "diff"]:
                return _common.RunResult(stdout="FALLBACK", stderr="", returncode=0)
            raise AssertionError(f"unexpected cmd: {cmd}")

        with (
            patch.object(pr_ops, "run", side_effect=fake_run),
            patch.object(_common, "run", side_effect=fake_run),
        ):
            pr = self._with_state(lambda: pr_ops.identify_pr())
            out = pr_ops.build_pr_summary(pr)

        # The projection must actually request and keep `body`.
        joined = " ".join(seen_args[0])
        self.assertIn("body", joined)
        self.assertEqual(pr["body"], body)
        # ...and the summary must use it rather than the commit fallback.
        self.assertEqual(out["source"], "pr-body")
        self.assertIn("rewires the widget cache", out["pr_summary"])
        self.assertNotIn("FALLBACK", out["pr_summary"])

    def _run_cli(self, argv: list[str]) -> str:
        """Invoke pr_ops.main and capture stdout — the SKILL.md entry point."""
        import contextlib
        import io

        buf = io.StringIO()
        with contextlib.redirect_stdout(buf):
            rc = pr_ops.main(argv)
        self.assertEqual(rc, 0, f"{argv} exited {rc}")
        return buf.getvalue()

    def test_cli_pr_summary_emits_json(self) -> None:
        """CLI boundary: argparse registration + JSON dispatch path."""
        fake = {
            "pr_summary": "T\n\nbody",
            "source": "pr-body",
            "dropped": ["cursor-summary"],
            "pr_number": 9,
            "base": "main",
        }
        with patch.object(pr_ops, "build_pr_summary", return_value=fake):
            out = json.loads(self._run_cli(["pr-summary"]))
        self.assertEqual(out["source"], "pr-body")
        self.assertEqual(out["dropped"], ["cursor-summary"])
        self.assertEqual(out["pr_summary"], "T\n\nbody")

    def test_cli_pr_summary_text_only_emits_plain_text(self) -> None:
        """--text-only must print the summary alone, parseable as $(...)."""
        fake = {
            "pr_summary": "T\n\nbody",
            "source": "pr-body",
            "dropped": [],
            "pr_number": 9,
            "base": "main",
        }
        with patch.object(pr_ops, "build_pr_summary", return_value=fake):
            out = self._run_cli(["pr-summary", "--text-only"])
        self.assertEqual(out.strip(), "T\n\nbody")
        # Must not be JSON — SKILL.md feeds this straight to --goal.
        with self.assertRaises(json.JSONDecodeError):
            json.loads(out)

    def test_branch_only_mode_uses_commits(self) -> None:
        with patch.object(pr_ops, "_commit_diffstat_summary", return_value="COMMITS"):
            out = pr_ops.build_pr_summary(
                {"pr_number": None, "title": None, "body": None, "base": "main"}
            )
        self.assertEqual(out["source"], "commits-diffstat")
        self.assertEqual(out["pr_summary"], "COMMITS")
        self.assertIsNone(out["pr_number"])


def _envelope(status="ok", issues=None, session_id="sess-1"):
    """A minimal bramble review envelope."""
    return {
        "status": status,
        "backend": "codex",
        "session_id": session_id,
        "review_mode": "code",
        "review": {"verdict": "rejected", "issues": issues or []},
    }


class OrphanEnvelopeGuardTests(unittest.TestCase):
    """finalize must never silently ignore an envelope sitting in the log dir.

    kernel#8682 r1: the orchestrator decided codex produced nothing, wrote
    `ack ... no envelope`, and finalized without `--envelope codex=...`. The
    envelope landed 2m26s later with status=ok and three findings, one at 0.98
    confidence that then took four more rounds to rediscover.
    """

    def _round_dir(self, sd: Path, n: int) -> Path:
        d = sd / f"r{n}" / "a1"
        d.mkdir(parents=True, exist_ok=True)
        return d

    def test_rejects_an_envelope_that_was_not_passed(self):
        with tempfile.TemporaryDirectory() as td:
            sd = Path(td)
            d = self._round_dir(sd, 1)
            (d / "codex-envelope.json").write_text(json.dumps(_envelope()))
            (d / "cursor-envelope.json").write_text(json.dumps(_envelope()))
            with self.assertRaises(ValueError) as ctx:
                pr_ops._reject_orphan_envelopes(
                    sd, 1, {"cursor": d / "cursor-envelope.json"}
                )
            self.assertIn("codex-envelope.json", str(ctx.exception))

    def test_accepts_when_every_envelope_is_passed(self):
        with tempfile.TemporaryDirectory() as td:
            sd = Path(td)
            d = self._round_dir(sd, 1)
            (d / "codex-envelope.json").write_text(json.dumps(_envelope()))
            (d / "cursor-envelope.json").write_text(json.dumps(_envelope()))
            pr_ops._reject_orphan_envelopes(
                sd,
                1,
                {
                    "codex": d / "codex-envelope.json",
                    "cursor": d / "cursor-envelope.json",
                },
            )

    def test_ignores_a_zero_byte_envelope(self):
        """A reviewer that died mid-write left no findings to lose.

        That is the genuine stream-missing case, and it must still be
        finalizable as `ack` — the guard exists to protect real review work,
        not to block on an empty file.
        """
        with tempfile.TemporaryDirectory() as td:
            sd = Path(td)
            d = self._round_dir(sd, 1)
            (d / "codex-envelope.json").write_text("")
            pr_ops._reject_orphan_envelopes(sd, 1, {})

    def test_a_prior_attempts_envelope_is_not_an_orphan(self):
        """Resume must stay possible: round_bundle keeps prior attempt dirs.

        `_next_attempt` allocates a FRESH a<n> on resume and deliberately
        preserves earlier attempts, so a retried round legitimately has valid
        envelopes under a1 while finalizing a2. Scanning all of r<n> flagged
        every one of them and made resume impossible — a guard against losing
        findings must not break the retry path that exists to recover them.
        """
        with tempfile.TemporaryDirectory() as td:
            sd = Path(td)
            a1 = sd / "r1" / "a1"
            a1.mkdir(parents=True)
            a2 = sd / "r1" / "a2"
            a2.mkdir(parents=True)
            (a1 / "codex-envelope.json").write_text(json.dumps(_envelope()))
            (a2 / "codex-envelope.json").write_text(json.dumps(_envelope()))
            # Finalizing a2 must not trip on a1's leftover.
            pr_ops._reject_orphan_envelopes(
                sd, 1, {"codex": a2 / "codex-envelope.json"}
            )

    def test_still_catches_an_orphan_inside_the_active_attempt(self):
        """Scoping to the active attempt must not blunt the actual guard."""
        with tempfile.TemporaryDirectory() as td:
            sd = Path(td)
            a1 = sd / "r1" / "a1"
            a1.mkdir(parents=True)
            a2 = sd / "r1" / "a2"
            a2.mkdir(parents=True)
            (a1 / "codex-envelope.json").write_text(json.dumps(_envelope()))
            # a2 holds BOTH; only cursor is passed, so codex is a real orphan.
            (a2 / "codex-envelope.json").write_text(json.dumps(_envelope()))
            (a2 / "cursor-envelope.json").write_text(json.dumps(_envelope()))
            with self.assertRaises(ValueError) as ctx:
                pr_ops._reject_orphan_envelopes(
                    sd, 1, {"cursor": a2 / "cursor-envelope.json"}
                )
            msg = str(ctx.exception)
            self.assertIn("a2", msg)
            self.assertNotIn("a1", msg)

    def test_a_recovered_envelope_covers_its_original(self):
        """The documented recovery flow must not trip this guard.

        SKILL Step 3.b runs recover-envelope, which writes a sibling
        <backend>-envelope-recovered.json and returns THAT path — so finalize
        legitimately gets the sibling while the original stays on disk. Left
        alone, a check meant to stop findings being dropped instead blocked the
        mechanism that rescues them.
        """
        with tempfile.TemporaryDirectory() as td:
            sd = Path(td)
            d = sd / "r1" / "a1"
            d.mkdir(parents=True)
            (d / "codex-envelope.json").write_text(json.dumps(_envelope()))
            rec = d / "codex-envelope-recovered.json"
            rec.write_text(json.dumps(_envelope()))
            pr_ops._reject_orphan_envelopes(sd, 1, {"codex": rec})

    def test_a_missing_override_cannot_silence_the_guard(self):
        """The recovered-envelope exemption must require a real file.

        Matching on filename alone let a stale, missing, or misspelled override
        suppress detection for that backend entirely — a wider hole than the one
        the exemption closes, since the guard would then say nothing while a
        real envelope went unread.
        """
        with tempfile.TemporaryDirectory() as td:
            sd = Path(td)
            d = sd / "r1" / "a1"
            d.mkdir(parents=True)
            (d / "codex-envelope.json").write_text(json.dumps(_envelope()))
            with self.assertRaises(ValueError):
                pr_ops._reject_orphan_envelopes(
                    sd, 1, {"codex": d / "codex-envelope-recovered.json"},
                )

    def test_ignores_other_rounds(self):
        with tempfile.TemporaryDirectory() as td:
            sd = Path(td)
            d = self._round_dir(sd, 1)
            (d / "codex-envelope.json").write_text(json.dumps(_envelope()))
            pr_ops._reject_orphan_envelopes(sd, 2, {})

    def test_finds_an_envelope_in_a_later_attempt_dir(self):
        """The guard works in any attempt dir, not just a1."""
        with tempfile.TemporaryDirectory() as td:
            sd = Path(td)
            d2 = sd / "r1" / "a2"
            d2.mkdir(parents=True)
            (d2 / "codex-envelope.json").write_text(json.dumps(_envelope()))
            (d2 / "cursor-envelope.json").write_text(json.dumps(_envelope()))
            with self.assertRaises(ValueError) as ctx:
                pr_ops._reject_orphan_envelopes(
                    sd, 1, {"cursor": d2 / "cursor-envelope.json"}
                )
            self.assertIn("codex-envelope.json", str(ctx.exception))

    def test_a_zero_envelope_finalize_is_still_checked(self):
        """The motivating case must not slip through the scoping fix.

        "The orchestrator decided the reviewers produced nothing and finalized
        without them" IS kernel#8682 r1. Deriving scope only from passed paths
        would skip exactly that, and upstream merely warns on a zero-envelope
        finalize rather than rejecting it — so this is the only thing in front
        of it. Falls back to the latest attempt.
        """
        with tempfile.TemporaryDirectory() as td:
            sd = Path(td)
            d1 = sd / "r1" / "a1"
            d1.mkdir(parents=True)
            (d1 / "codex-envelope.json").write_text(json.dumps(_envelope()))
            with self.assertRaises(ValueError) as ctx:
                pr_ops._reject_orphan_envelopes(sd, 1, {})
            self.assertIn("codex-envelope.json", str(ctx.exception))

    def test_a_zero_envelope_finalize_checks_only_the_latest_attempt(self):
        """Still must not resurrect the resume breakage."""
        with tempfile.TemporaryDirectory() as td:
            sd = Path(td)
            (sd / "r1" / "a1").mkdir(parents=True)
            (sd / "r1" / "a2").mkdir(parents=True)
            # Only the OLD attempt has an envelope; a2 is the live one.
            (sd / "r1" / "a1" / "codex-envelope.json").write_text(
                json.dumps(_envelope())
            )
            pr_ops._reject_orphan_envelopes(sd, 1, {})

    def test_no_round_dir_is_not_an_error(self):
        with tempfile.TemporaryDirectory() as td:
            pr_ops._reject_orphan_envelopes(Path(td), 1, {})


class LowOnlyStreakLivenessTests(unittest.TestCase):
    """A dead round must not advance the low-only streak.

    top_severity is None for a round no reviewer returned, and None ranks
    below "low" — so a dead round used to INCREMENT. That is the same
    "nobody looked" == "nothing to find" conflation, at the writer instead
    of the reader. Downstream it is unsuppressed: the streak feeds
    convergence-pressure text into the next round's reviewer goal, telling a
    healthy reviewer its dead peers found the diff clean.
    """

    def test_a_dead_round_holds_the_streak(self):
        prior = [{"n": 1, "low_only_streak": 1, "top_severity": "low"}]
        got = pr_ops._compute_low_only_streak(prior, None, had_live_reviewer=False)
        self.assertEqual(got, 1, "a dead round must neither advance nor reset")

    def test_a_dead_round_used_to_increment(self):
        # The un-fixed behaviour, pinned so the regression is legible.
        prior = [{"n": 1, "low_only_streak": 1, "top_severity": "low"}]
        self.assertEqual(pr_ops._compute_low_only_streak(prior, None), 2)

    def test_a_live_low_round_still_advances(self):
        prior = [{"n": 1, "low_only_streak": 1, "top_severity": "low"}]
        self.assertEqual(
            pr_ops._compute_low_only_streak(prior, "low", had_live_reviewer=True), 2
        )

    def test_a_live_high_round_still_resets(self):
        prior = [{"n": 1, "low_only_streak": 2, "top_severity": "low"}]
        self.assertEqual(
            pr_ops._compute_low_only_streak(prior, "high", had_live_reviewer=True), 0
        )

    def test_the_backfill_path_applies_the_same_dead_round_rule(self):
        """Both writers of low_only_streak must answer one question the same way.

        _backfill_low_only_streak reconstructs the streak from top_severity
        history. Left alone, it re-derived the inflated number the forward path
        had just been fixed to stop producing — the same defect at the second
        writer, which is why both now route through _round_counts_as_low_only.
        """
        prior = [
            {"n": 1, "top_severity": "low", "stream_status": {"codex": "ok"}},
            # Dead: top_severity None only because nobody reported.
            {"n": 2, "top_severity": None,
             "stream_status": {"codex": "error", "cursor": "absent"}},
        ]
        # Only round 1 is evidence, so the streak is 1 — not 2.
        self.assertEqual(pr_ops._backfill_low_only_streak(prior), 1)

    def test_the_backfill_path_still_resets_on_a_live_high_round(self):
        prior = [
            {"n": 1, "top_severity": "low", "stream_status": {"codex": "ok"}},
            {"n": 2, "top_severity": "high", "stream_status": {"codex": "ok"}},
        ]
        self.assertEqual(pr_ops._backfill_low_only_streak(prior), 0)

    def test_the_backfill_path_is_unchanged_for_pre_stream_status_rounds(self):
        # No stream_status at all reads as live, so historical reconstruction
        # behaves exactly as before the field existed.
        prior = [{"n": 1, "top_severity": "low"}, {"n": 2, "top_severity": "low"}]
        self.assertEqual(pr_ops._backfill_low_only_streak(prior), 2)

    def test_liveness_is_read_from_stream_status(self):
        self.assertTrue(pr_ops._round_had_live_reviewer({"stream_status": {"codex": "ok"}}))
        self.assertTrue(pr_ops._round_had_live_reviewer({"stream_status": {"codex": "partial"}}))
        self.assertFalse(
            pr_ops._round_had_live_reviewer(
                {"stream_status": {"codex": "error", "cursor": "absent", "lint": "ok"}}
            )
        )
        # Statuses beyond the four named ones must read as not-live.
        self.assertFalse(
            pr_ops._round_had_live_reviewer({"stream_status": {"codex": "exited-empty"}})
        )
        # No data is not "nobody looked".
        self.assertTrue(pr_ops._round_had_live_reviewer({}))


class ConvergedSignalLivenessTests(unittest.TestCase):
    """The in-loop guard, exercised through finalize_and_report itself.

    _round_had_live_reviewer and verdict.py were each covered alone, so
    deleting the production guard left both green (codex, r7). This asserts
    the wiring.
    """

    def _finalize(self, tmp, stream_files, n=1, prior=None):
        sd = Path(tmp)
        d = sd / f"r{n}" / "a1"
        d.mkdir(parents=True)
        overrides = {}
        for backend, status in stream_files.items():
            f = d / f"{backend}-envelope.json"
            if status is not None:
                f.write_text(json.dumps({"status": status, "backend": backend,
                                         "review": {"verdict": "accepted", "issues": []}}))
            overrides[backend] = f
        rounds = list(prior or [])
        rounds.append({"n": n, "comment_actions": []})
        (sd / "pr-polish-state.json").write_text(
            json.dumps({"pr_number": None, "branch": "br", "rounds": rounds})
        )
        import unittest.mock as _mock
        with _mock.patch.object(
            pr_ops, "state_paths",
            return_value=(sd, sd / "pr-polish-state.json"),
        ):
            return pr_ops.finalize_and_report(
                "branch:br", n, "deadbeef", [], envelope_overrides=overrides,
            )

    def test_a_dead_round_cannot_report_converged(self):
        with tempfile.TemporaryDirectory() as td:
            out = self._finalize(td, {"codex": "error", "cursor": None, "lint": "ok"})
            self.assertIsNone(out["converged_signal"])
            self.assertIsNone(out["exit_reason_hint"])

    def test_a_live_clean_round_may_report_converged(self):
        with tempfile.TemporaryDirectory() as td:
            out = self._finalize(td, {"codex": "ok", "lint": "ok"})
            self.assertTrue(out["converged_signal"])

    def test_a_recovered_round_clears_an_earlier_dead_round(self):
        """The in-loop gate is per-round, not series-wide.

        A series-wide read meant one dead round blocked convergence for every
        round after it, so a recovered run could never signal done and would
        burn its budget to the cap.
        """
        prior = [{"n": 1, "comment_actions": [],
                  "stream_status": {"codex": "error", "cursor": "absent"}}]
        with tempfile.TemporaryDirectory() as td:
            out = self._finalize(td, {"codex": "ok", "lint": "ok"}, n=2, prior=prior)
            self.assertTrue(
                out["converged_signal"],
                "a healthy round must be able to converge after an earlier dead one",
            )


class StreamStatusPersistenceTests(unittest.TestCase):
    """`<backend>_findings: []` cannot distinguish "clean" from "never ran".

    On #8682 an empty array meant a DROPPED envelope in r1 and a real backend
    error in r2/r3. Persist the envelope's own status so convergence and
    escape_rate can tell them apart.
    """

    def test_persists_the_envelope_status_per_backend(self):
        with tempfile.TemporaryDirectory() as td:
            sd = Path(td)
            d = sd / "r1" / "a1"
            d.mkdir(parents=True)
            ok = d / "codex-envelope.json"
            ok.write_text(json.dumps(_envelope(status="ok")))
            bad = d / "cursor-envelope.json"
            bad.write_text(
                json.dumps({"status": "error", "error": "stalled", "backend": "cursor"})
            )
            entry: dict = {}
            pr_ops._persist_round_findings(
                sd, entry, None, "br", 1,
                {"codex": ok, "cursor": bad},
            )
            self.assertEqual(entry["stream_status"]["codex"], "ok")
            self.assertEqual(entry["stream_status"]["cursor"], "error")

    def test_a_launched_backend_that_wrote_nothing_records_absent(self):
        """The 'absent' half of the live-reviewer rule needs a recorded value.

        A backend that was launched and crashed writes no envelope. Skipping it
        left no key at all, so the gate had nothing to judge and "nobody
        looked" kept reading as "nothing to find".
        """
        with tempfile.TemporaryDirectory() as td:
            sd = Path(td)
            d = sd / "r1" / "a1"
            d.mkdir(parents=True)
            ok = d / "codex-envelope.json"
            ok.write_text(json.dumps(_envelope(status="ok")))
            entry: dict = {}
            pr_ops._persist_round_findings(
                sd, entry, None, "br", 1,
                {"codex": ok, "cursor": d / "cursor-envelope.json"},
            )
            self.assertEqual(entry["stream_status"]["cursor"], "absent")
            self.assertEqual(entry["stream_status"]["codex"], "ok")

    def test_an_unreadable_envelope_records_a_status(self):
        """A file that exists but does not parse is a failed stream.

        Leaving no key at all is the same "nobody looked" reads as "nothing to
        find" hole the absent branch closes — and this is the shape a reviewer
        that died mid-write actually produces.
        """
        with tempfile.TemporaryDirectory() as td:
            sd = Path(td)
            d = sd / "r1" / "a1"
            d.mkdir(parents=True)
            bad = d / "codex-envelope.json"
            bad.write_text("{ truncated mid-writ")
            entry: dict = {}
            pr_ops._persist_round_findings(sd, entry, None, "br", 1, {"codex": bad})
            self.assertEqual(entry["stream_status"]["codex"], "unreadable")

    def test_a_backend_never_launched_records_nothing(self):
        with tempfile.TemporaryDirectory() as td:
            sd = Path(td)
            d = sd / "r1" / "a1"
            d.mkdir(parents=True)
            ok = d / "codex-envelope.json"
            ok.write_text(json.dumps(_envelope(status="ok")))
            entry: dict = {}
            pr_ops._persist_round_findings(sd, entry, None, "br", 1, {"codex": ok})
            self.assertNotIn("cursor", entry.get("stream_status", {}))

    def test_refinalize_drops_stale_status_for_omitted_backends(self):
        with tempfile.TemporaryDirectory() as td:
            sd = Path(td)
            d = sd / "r1" / "a1"
            d.mkdir(parents=True)
            cur = d / "cursor-envelope.json"
            cur.write_text(json.dumps(_envelope(status="ok")))
            entry: dict = {"stream_status": {"codex": "ok", "cursor": "error"}}
            pr_ops._persist_round_findings(sd, entry, None, "br", 1, {"cursor": cur})
            # codex was not passed this time: its stale "ok" must not survive.
            self.assertNotIn("codex", entry.get("stream_status", {}))
            self.assertEqual(entry["stream_status"]["cursor"], "ok")


if __name__ == "__main__":
    unittest.main()
