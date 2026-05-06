"""Unit tests for pr_ops. Hermetic: no gh/git/network.

Run with:
    python3 -m unittest discover -v   # from .claude/skills/pr-polish/scripts
"""

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
            }
        )

        def fake_run(cmd, **kwargs):
            if cmd[:2] == ["git", "rev-parse"]:
                return _common.RunResult(stdout="feature/PLA-287\n", stderr="", returncode=0)
            if cmd[:3] == ["gh", "pr", "view"]:
                return _common.RunResult(stdout=pr_json, stderr="", returncode=0)
            if cmd[:3] == ["gh", "repo", "view"]:
                return _common.RunResult(
                    stdout=json.dumps({"owner": {"login": "sycamore-labs"}, "name": "kernel"}),
                    stderr="",
                    returncode=0,
                )
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
        self.assertTrue(out["state_file"].endswith("pr-polish-state.json"))

    def test_identify_without_pr_returns_branch_only(self) -> None:
        def fake_run(cmd, **kwargs):
            if cmd[:2] == ["git", "rev-parse"]:
                return _common.RunResult(stdout="feature/new-idea\n", stderr="", returncode=0)
            if cmd[:3] == ["gh", "pr", "view"]:
                # gh exits non-zero when the branch has no PR.
                return _common.RunResult(stdout="", stderr="no pull requests found", returncode=1)
            if cmd[:3] == ["gh", "repo", "view"]:
                return _common.RunResult(
                    stdout=json.dumps({"owner": {"login": "sycamore-labs"}, "name": "kernel"}),
                    stderr="",
                    returncode=0,
                )
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
    """state_finalize_round must hydrate {backend}_findings and copy envelopes."""

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

        import bramble_ops  # noqa: PLC0415

        self.bramble_ops = bramble_ops

        def fake_envelope_path(repo: str, pr, backend: str, round_: int) -> Path:
            return self.envelope_dir / f"{backend}-r{round_}.json"

        p2 = patch.object(bramble_ops, "envelope_path", side_effect=fake_envelope_path)
        p2.start()
        self.addCleanup(p2.stop)

        p3 = patch.object(pr_ops, "repo_slug", return_value="demo-repo")
        p3.start()
        self.addCleanup(p3.stop)

    def _write_envelope(self, backend: str, issues: list[dict]) -> Path:
        obj = {
            "schema_version": 1,
            "status": "ok",
            "backend": backend,
            "review": {"verdict": "rejected", "issues": issues},
        }
        path = self.envelope_dir / f"{backend}-r1.json"
        path.write_text(json.dumps(obj))
        return path

    def test_finalize_hydrates_findings_and_copies_envelopes(self) -> None:
        self._write_envelope(
            "codex",
            [{"severity": "high", "file": "a.go", "line": 5, "message": "oops", "topic": "t1"}],
        )
        self._write_envelope(
            "cursor",
            [{"severity": "medium", "file": "b.go", "line": 8, "message": "meh", "topic": "t2"}],
        )

        pr_ops.state_append_round(77, 1, "sha", verify_head=False)
        state = pr_ops.state_finalize_round(77, 1, "sha2", [])
        rnd = state["rounds"][0]

        self.assertEqual(len(rnd["codex_findings"]), 1)
        self.assertEqual(rnd["codex_findings"][0]["file"], "a.go")
        self.assertEqual(rnd["codex_findings"][0]["source"], "codex")
        self.assertEqual(len(rnd["cursor_findings"]), 1)
        self.assertEqual(rnd["cursor_findings"][0]["source"], "cursor")

        reviews = self.state_dir / "reviews"
        self.assertTrue((reviews / "r1-codex.json").exists())
        self.assertTrue((reviews / "r1-cursor.json").exists())

    def test_finalize_tolerates_missing_envelope(self) -> None:
        self._write_envelope(
            "codex",
            [{"severity": "low", "file": "c.go", "line": 3, "message": "ok", "topic": "t3"}],
        )
        pr_ops.state_append_round(77, 1, "sha", verify_head=False)
        state = pr_ops.state_finalize_round(77, 1, "sha2", [])
        rnd = state["rounds"][0]
        self.assertEqual(len(rnd["codex_findings"]), 1)
        self.assertEqual(rnd["cursor_findings"], [])

    def test_finalize_tolerates_malformed_envelope(self) -> None:
        (self.envelope_dir / "codex-r1.json").write_text("not json {{{")
        pr_ops.state_append_round(77, 1, "sha", verify_head=False)
        state = pr_ops.state_finalize_round(77, 1, "sha2", [])
        self.assertEqual(state["rounds"][0]["codex_findings"], [])


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
        base_list = {"workflow_runs": [{"id": 555, "head_sha": "basesha"}]}
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
                patch.object(pr_ops, "repo_slug", return_value="demo"),
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
                patch.object(pr_ops, "repo_slug", return_value="demo"),
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
                patch.object(pr_ops, "repo_slug", return_value="demo"),
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


if __name__ == "__main__":
    unittest.main()
