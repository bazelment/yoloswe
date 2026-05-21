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
        )
        self.assertIsNotNone(record)
        self.assertEqual(record.schema_version, 1)
        self.assertEqual(record.pr["repo_name"], "kernel")
        self.assertEqual(record.pr["pr_number"], "3945")
        # kernel-3945 had 5 rounds, completed=True → R1 + R5
        self.assertEqual(len(record.harvested_rounds), 2)
        r1, r_final = record.harvested_rounds
        self.assertEqual(r1.round, 1)
        self.assertEqual(r1.signal_tier, "r1")
        self.assertEqual(r_final.round, 5)
        self.assertEqual(r_final.signal_tier, "final")

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
        )
        with tempfile.TemporaryDirectory() as td:
            out = Path(td)
            path = hl.write_pr_record(out, record)
            self.assertTrue(path.exists())
            loaded = json.loads(path.read_text())
            self.assertEqual(loaded["pr"]["pr_number"], "3945")
            self.assertEqual(len(loaded["harvested_rounds"]), 2)
            self.assertEqual(loaded["schema_version"], 1)


class BuildIndexTests(unittest.TestCase):
    def test_index_shape(self):
        # Minimal record to feed the index builder.
        rec = hl.PRRecord(
            schema_version=1,
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
            harvested_rounds=[],
        )
        index = hl.build_index(
            [rec], generated_at="2026-05-20T00:00:00Z", harvester_sha="abc"
        )
        self.assertEqual(index["schema_version"], 1)
        self.assertEqual(len(index["prs"]), 1)
        self.assertEqual(index["prs"][0]["pr_number"], "3945")
        self.assertEqual(index["prs"][0]["file"], "kernel-3945.json")
        self.assertEqual(index["prs"][0]["harvested_rounds"], 0)


if __name__ == "__main__":
    unittest.main()
