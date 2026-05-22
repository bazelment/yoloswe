"""Unit tests for collection mode (collect_lib + collect)."""

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

import collect  # noqa: E402
import collect_lib as cl  # noqa: E402
import harvest_lib as hl  # noqa: E402


def _fv(file, line, verdict, *, severity="high", topic="t", reason="r",
        surfaced_by=None):
    return {
        "file": file, "line": line, "severity": severity, "topic": topic,
        "verdict": verdict, "reason": reason,
        "surfaced_by": surfaced_by or [],
    }


def _census(file, line, **kw):
    return {"file": file, "line": line, **kw}


class NormalizeFindingPathTests(unittest.TestCase):
    """Worktree-checkout prefixes must be stripped to repo-relative paths.

    Regression test: bramble emits absolute paths prefixed with its WORK_DIR
    worktree; the harvested dataset stores repo-relative paths. Unstripped,
    the same file looked like two files and dedup / GT-matching failed.
    """

    def test_strips_replay_worktree_prefix(self):
        self.assertEqual(
            cl.normalize_finding_path("/tmp/replay-kernel-3945-r1-xy/src/a.py"),
            "src/a.py",
        )

    def test_strips_legacy_crr_prefix(self):
        self.assertEqual(
            cl.normalize_finding_path("/tmp/crr-kernel-4013-r2-wt/src/a.py"),
            "src/a.py",
        )

    def test_strips_session_review_worktree_prefix(self):
        self.assertEqual(
            cl.normalize_finding_path(
                "/home/u/.bramble/code-review-eval/collect/"
                "kernel-1-20260521/review-worktree/src/a.py"
            ),
            "src/a.py",
        )

    def test_strips_session_judge_worktree_prefix(self):
        self.assertEqual(
            cl.normalize_finding_path(
                "/x/session/judge-worktree/services/api/main.py"
            ),
            "services/api/main.py",
        )

    def test_already_relative_passes_through(self):
        self.assertEqual(
            cl.normalize_finding_path("services/api/main.py"),
            "services/api/main.py",
        )

    def test_none_and_empty(self):
        self.assertIsNone(cl.normalize_finding_path(None))
        self.assertIsNone(cl.normalize_finding_path(""))


class SameDefectTests(unittest.TestCase):
    def test_same_path_line_within_slack(self):
        self.assertTrue(cl.same_defect("a.py", 10, "a.py", 12))
        self.assertTrue(cl.same_defect("a.py", 10, "a.py", 13))

    def test_line_outside_slack(self):
        self.assertFalse(cl.same_defect("a.py", 10, "a.py", 14))

    def test_different_path(self):
        self.assertFalse(cl.same_defect("a.py", 10, "b.py", 10))

    def test_path_normalization(self):
        self.assertTrue(cl.same_defect("./a.py", 10, "a.py", 11))

    def test_worktree_prefix_vs_relative_match(self):
        # The exact bug found in the kernel-4158 verification run: a
        # harvested repo-relative path and a bramble absolute worktree path
        # for the SAME file must compare equal.
        self.assertTrue(cl.same_defect(
            "services/api/main.py", 350,
            "/tmp/crr-kernel-4013-r2-wt/services/api/main.py", 350,
        ))

    def test_missing_line_is_file_level_match(self):
        self.assertTrue(cl.same_defect("a.py", None, "a.py", 99))
        self.assertTrue(cl.same_defect("a.py", 10, "a.py", None))


class ValidateJudgeVerdictTests(unittest.TestCase):
    def test_valid(self):
        v = {"finding_verdicts": [_fv("a.py", 1, "true_positive")],
             "census": []}
        self.assertIsNone(cl.validate_judge_verdict(v))

    def test_not_an_object(self):
        self.assertIsNotNone(cl.validate_judge_verdict([1, 2]))

    def test_missing_finding_verdicts(self):
        self.assertIsNotNone(cl.validate_judge_verdict({}))

    def test_bad_verdict_value(self):
        v = {"finding_verdicts": [_fv("a.py", 1, "maybe")]}
        self.assertIsNotNone(cl.validate_judge_verdict(v))

    def test_census_must_be_list(self):
        v = {"finding_verdicts": [], "census": {}}
        self.assertIsNotNone(cl.validate_judge_verdict(v))

    def test_census_merges_optional(self):
        v = {"finding_verdicts": [], "census": []}
        self.assertIsNone(cl.validate_judge_verdict(v))

    def test_census_merges_must_be_list(self):
        v = {"finding_verdicts": [], "census_merges": {}}
        self.assertIsNotNone(cl.validate_judge_verdict(v))

    def test_census_merge_needs_two_members(self):
        v = {"finding_verdicts": [],
             "census_merges": [{"members": [{"file": "a.py", "line": 1}]}]}
        self.assertIsNotNone(cl.validate_judge_verdict(v))

    def test_valid_census_merge(self):
        v = {"finding_verdicts": [], "census": [],
             "census_merges": [{"members": [
                 {"file": "a.py", "line": 8},
                 {"file": "a.py", "line": 46}]}]}
        self.assertIsNone(cl.validate_judge_verdict(v))


class MergeJudgeRoundTests(unittest.TestCase):
    def test_routes_tp_and_fp_drops_unsure(self):
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [
                _fv("a.py", 10, "true_positive"),
                _fv("b.py", 5, "false_positive"),
                _fv("c.py", 1, "unsure"),
            ],
            "census": [],
        })
        self.assertEqual(len(c.true_positives), 1)
        self.assertEqual(len(c.false_positives), 1)

    def test_dedupes_same_defect_unions_surfaced_by(self):
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [
                _fv("a.py", 10, "true_positive", surfaced_by=["codex"]),
            ],
            "census": [],
        })
        cl.merge_judge_round(c, 2, {
            "finding_verdicts": [
                _fv("a.py", 12, "true_positive", surfaced_by=["cursor"]),
            ],
            "census": [],
        })
        self.assertEqual(len(c.true_positives), 1)
        self.assertEqual(
            sorted(c.true_positives[0].surfaced_by), ["codex", "cursor"]
        )
        self.assertEqual(c.true_positives[0].first_seen_round, 1)

    def test_census_unioned_and_deduped(self):
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [],
            "census": [_census("a.py", 10), _census("c.py", 99)],
        })
        cl.merge_judge_round(c, 2, {
            "finding_verdicts": [],
            "census": [_census("a.py", 10), _census("d.py", 1)],
        })
        # a.py:10 not double-counted; d.py:1 added.
        self.assertEqual(len(c.census), 3)


class CensusConvergenceTests(unittest.TestCase):
    def test_needs_at_least_two_rounds(self):
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [_fv("a.py", 10, "true_positive")],
            "census": [_census("a.py", 10)],
        })
        # Round 1 alone: covered, but cannot be "unchanged vs prior".
        self.assertFalse(cl.census_converged(c))

    def test_converges_when_stable_and_covered(self):
        c = cl.CumulativeGT()
        for r in (1, 2):
            cl.merge_judge_round(c, r, {
                "finding_verdicts": [_fv("a.py", 10, "true_positive")],
                "census": [_census("a.py", 10)],
            })
        self.assertTrue(cl.census_converged(c))

    def test_no_converge_when_census_uncovered(self):
        c = cl.CumulativeGT()
        for r in (1, 2):
            cl.merge_judge_round(c, r, {
                "finding_verdicts": [_fv("a.py", 10, "true_positive")],
                # c.py:99 is a real bug NO finding caught -> uncovered.
                "census": [_census("a.py", 10), _census("c.py", 99)],
            })
        self.assertFalse(cl.census_converged(c))
        self.assertEqual(len(cl.census_uncovered(c)), 1)

    def test_no_converge_when_census_grew(self):
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [_fv("a.py", 10, "true_positive")],
            "census": [_census("a.py", 10)],
        })
        cl.merge_judge_round(c, 2, {
            "finding_verdicts": [
                _fv("a.py", 10, "true_positive"),
                _fv("c.py", 99, "true_positive"),
            ],
            # New census item in round 2 -> not unchanged.
            "census": [_census("a.py", 10), _census("c.py", 99)],
        })
        self.assertFalse(cl.census_converged(c))

    def test_empty_census_converges_after_two_quiet_rounds(self):
        c = cl.CumulativeGT()
        for r in (1, 2):
            cl.merge_judge_round(c, r, {"finding_verdicts": [], "census": []})
        self.assertTrue(cl.census_converged(c))


class CensusMergeTests(unittest.TestCase):
    """The judge can declare two census items to be one defect.

    Regression test: an early round split a file-scoped test-coverage gap
    into two line numbers; no reviewer finding ever cites the lower line, so
    the line-precise coverage check kept it permanently uncovered and
    convergence could never be reached.
    """

    def test_merge_collapses_census_items(self):
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [],
            "census": [_census("t.py", 8), _census("t.py", 46)],
        })
        self.assertEqual(len(c.census), 2)
        # Round 2 declares the two t.py entries one defect.
        cl.merge_judge_round(c, 2, {
            "finding_verdicts": [],
            "census": [_census("t.py", 8), _census("t.py", 46)],
            "census_merges": [{"members": [
                {"file": "t.py", "line": 8},
                {"file": "t.py", "line": 46}]}],
        })
        # Collapsed to one — the first member (line 8) is canonical.
        self.assertEqual(len(c.census), 1)
        self.assertEqual(c.census[0]["line"], 8)

    def test_merge_lets_one_finding_cover_a_split_defect(self):
        c = cl.CumulativeGT()
        # Round 1: defect censused at two lines; a finding only at line 46.
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [_fv("t.py", 46, "true_positive")],
            "census": [_census("t.py", 8), _census("t.py", 46)],
        })
        # t.py:8 has no finding within +/-3 -> uncovered, no convergence.
        self.assertEqual(len(cl.census_uncovered(c)), 1)
        # Round 2: the judge merges the two census items into one defect.
        cl.merge_judge_round(c, 2, {
            "finding_verdicts": [_fv("t.py", 46, "true_positive")],
            "census": [_census("t.py", 8), _census("t.py", 46)],
            "census_merges": [{"members": [
                {"file": "t.py", "line": 8},
                {"file": "t.py", "line": 46}]}],
        })
        # The merged item is now covered by the line-46 finding. A merge is
        # a reinterpretation, not new information — the merged item still
        # answers to both member keys, so the key set is unchanged and
        # round 2 converges.
        self.assertEqual(len(cl.census_uncovered(c)), 0)
        self.assertTrue(cl.census_converged(c))

    def test_merge_of_absent_items_is_noop(self):
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [],
            "census": [_census("a.py", 1)],
            # members reference census entries that don't exist
            "census_merges": [{"members": [
                {"file": "z.py", "line": 1},
                {"file": "z.py", "line": 2}]}],
        })
        self.assertEqual(len(c.census), 1)

    def test_merge_with_finding_location_member_covers_census(self):
        # The kernel-4050 verification case: the judge declares a census
        # item and a FINDING location (not itself censused, >LINE_SLACK
        # away) to be one defect. The finding location must land in the
        # census entry's merged_locations so a TP finding there covers it.
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            # A TP finding 20 lines from the censused line — no auto-match.
            "finding_verdicts": [_fv("hooks.ts", 1358, "true_positive")],
            "census": [_census("hooks.ts", 1338)],
        })
        # Line-precise: 1358 vs 1338 is outside +/-3 -> uncovered.
        self.assertEqual(len(cl.census_uncovered(c)), 1)
        # Round 2: the judge declares 1338 and 1358 one defect. 1358 is a
        # finding location, not a census item — only 1338 is in the census.
        cl.merge_judge_round(c, 2, {
            "finding_verdicts": [_fv("hooks.ts", 1358, "true_positive")],
            "census": [_census("hooks.ts", 1338)],
            "census_merges": [{"members": [
                {"file": "hooks.ts", "line": 1338},
                {"file": "hooks.ts", "line": 1358}]}],
        })
        # The census still has one entry (the finding location is NOT
        # added as a census item) — but it now carries 1358 in
        # merged_locations, so the TP finding there covers it.
        self.assertEqual(len(c.census), 1)
        self.assertEqual(len(cl.census_uncovered(c)), 0)
        # The merge added 1358 to the key set, so round 2 changed the
        # census — a quiet round 3 is needed to converge.
        self.assertFalse(cl.census_converged(c))
        cl.merge_judge_round(c, 3, {
            "finding_verdicts": [_fv("hooks.ts", 1358, "true_positive")],
            "census": [_census("hooks.ts", 1338)],
            "census_merges": [{"members": [
                {"file": "hooks.ts", "line": 1338},
                {"file": "hooks.ts", "line": 1358}]}],
        })
        self.assertEqual(len(cl.census_uncovered(c)), 0)
        self.assertTrue(cl.census_converged(c))


class CommentActionXrefTests(unittest.TestCase):
    def _harvested(self):
        return [{
            "review_runs": [{
                "backend": "codex",
                "findings": [
                    {"file": "a.py", "line": 10,
                     "ground_truth": {"is_real_issue": True}},
                    {"file": "b.py", "line": 5,
                     "ground_truth": {"is_real_issue": False}},
                ],
            }],
        }]

    def test_agreement_and_disagreement(self):
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [
                _fv("a.py", 10, "true_positive"),   # agrees with harvest
                _fv("b.py", 5, "true_positive"),    # disagrees (harvest=False)
            ],
            "census": [],
        })
        xref = cl.comment_action_xref(c, self._harvested())
        self.assertEqual(xref["comparisons"], 2)
        self.assertEqual(xref["agreements"], 1)
        self.assertEqual(xref["comment_action_agreement_rate"], 0.5)
        self.assertEqual(len(xref["disagreements"]), 1)

    def test_null_harvest_label_skipped(self):
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [_fv("z.py", 1, "true_positive")],
            "census": [],
        })
        xref = cl.comment_action_xref(c, self._harvested())
        # No harvested finding at z.py:1 -> nothing to compare.
        self.assertEqual(xref["comparisons"], 0)
        self.assertIsNone(xref["comment_action_agreement_rate"])


class FreezeTests(unittest.TestCase):
    def test_freeze_writes_block_in_place(self):
        with tempfile.TemporaryDirectory() as d:
            ds_path = Path(d) / "demo-1.json"
            ds_path.write_text(json.dumps({
                "schema_version": 2,
                "pr": {"repo_name": "demo", "pr_number": "1"},
                "harvested_rounds": [{"round": 1}],
            }))
            c = cl.CumulativeGT()
            cl.merge_judge_round(c, 1, {
                "finding_verdicts": [_fv("a.py", 10, "true_positive")],
                "census": [_census("a.py", 10)],
            })
            gt = cl.build_ground_truth(
                c, collector_git_sha="abc", harvested_rounds=[]
            )
            cl.freeze(ds_path, gt)
            reloaded = json.loads(ds_path.read_text())
            # Harvested fields untouched; block added.
            self.assertEqual(reloaded["schema_version"], 2)
            self.assertIn("ground_truth_v3", reloaded)
            block = reloaded["ground_truth_v3"]
            self.assertEqual(
                block["schema_version"], cl.GROUND_TRUTH_SCHEMA_VERSION
            )
            self.assertEqual(len(block["true_positives"]), 1)
            self.assertIn("contested", block)

    def test_load_ground_truth(self):
        self.assertIsNone(cl.load_ground_truth({"pr": {}}))
        self.assertIsNotNone(
            cl.load_ground_truth({"ground_truth_v3": {"schema_version": 3}})
        )


class SessionRoundTripTests(unittest.TestCase):
    """collect.py's CumulativeGT serialize/restore across rounds."""

    def test_cumulative_survives_save_load(self):
        with tempfile.TemporaryDirectory() as d:
            session = Path(d)
            c = cl.CumulativeGT()
            cl.merge_judge_round(c, 1, {
                "finding_verdicts": [
                    _fv("a.py", 10, "true_positive", surfaced_by=["codex"]),
                    _fv("b.py", 5, "false_positive"),
                ],
                "census": [_census("a.py", 10), _census("c.py", 99)],
            })
            collect.save_cumulative(session, c)
            restored = collect.load_cumulative(session)
            self.assertEqual(len(restored.true_positives), 1)
            self.assertEqual(len(restored.false_positives), 1)
            self.assertEqual(len(restored.census), 2)
            self.assertEqual(restored.rounds_run, 1)
            # last_round_census_keys must round-trip as a set of tuples so
            # the next round's convergence check still works.
            self.assertIsInstance(restored.last_round_census_keys, set)
            cl.merge_judge_round(restored, 2, {
                "finding_verdicts": [
                    _fv("a.py", 10, "true_positive"),
                    _fv("c.py", 99, "true_positive"),
                ],
                "census": [_census("a.py", 10), _census("c.py", 99)],
            })
            self.assertTrue(cl.census_converged(restored))

    def test_fresh_session_is_empty(self):
        with tempfile.TemporaryDirectory() as d:
            c = collect.load_cumulative(Path(d))
            self.assertEqual(c.rounds_run, 0)
            self.assertEqual(c.true_positives, [])


class CanonicalRoundTests(unittest.TestCase):
    def test_prefers_r1(self):
        ds = {"harvested_rounds": [
            {"round": 1, "signal_tier": "r1"},
            {"round": 5, "signal_tier": "final"},
        ]}
        self.assertEqual(collect._canonical_round(ds)["round"], 1)

    def test_r1_only(self):
        ds = {"harvested_rounds": [{"round": 1, "signal_tier": "r1_only"}]}
        self.assertEqual(collect._canonical_round(ds)["round"], 1)


class DedupeFindingsTests(unittest.TestCase):
    def test_unions_surfaced_by_across_backends(self):
        merged = collect._dedupe_findings([
            {"file": "a.py", "line": 10, "surfaced_by": ["codex"]},
            {"file": "a.py", "line": 11, "surfaced_by": ["cursor"]},
            {"file": "b.py", "line": 1, "surfaced_by": ["gemini"]},
        ])
        self.assertEqual(len(merged), 2)
        a = next(m for m in merged if m["file"] == "a.py")
        self.assertEqual(sorted(a["surfaced_by"]), ["codex", "cursor"])


def _git(repo: Path, *args: str) -> str:
    import subprocess
    r = subprocess.run(["git", "-C", str(repo), *args],
                       capture_output=True, text=True, check=True)
    return r.stdout.strip()


def _make_repo_two_commits(repo: Path) -> tuple[str, str]:
    """A throwaway git repo: bug.py has a defect at HEAD~1, fixed at HEAD.

    Returns (head_before, head_after). The judge worktree must be pinned to
    head_before so it sees the BUGGY version, not the fixed one.
    """
    import subprocess
    repo.mkdir(parents=True, exist_ok=True)
    subprocess.run(["git", "-C", str(repo), "init", "-q"], check=True)
    _git(repo, "config", "user.email", "t@t.t")
    _git(repo, "config", "user.name", "t")
    (repo / "bug.py").write_text("x = 1 / 0  # BUG: div by zero\n")
    _git(repo, "add", "bug.py")
    _git(repo, "commit", "-q", "-m", "buggy")
    head_before = _git(repo, "rev-parse", "HEAD")
    (repo / "bug.py").write_text("x = 1 / 1  # fixed\n")
    _git(repo, "add", "bug.py")
    _git(repo, "commit", "-q", "-m", "fix")
    head_after = _git(repo, "rev-parse", "HEAD")
    return head_before, head_after


def _demo_dataset(head_before: str, merge_base: str) -> dict:
    return {
        "schema_version": 2,
        "pr": {"repo_name": "demo", "pr_number": "1"},
        "harvested_rounds": [{
            "round": 1, "signal_tier": "r1",
            "head_before": head_before, "head_after": head_before,
            "merge_base_sha": merge_base, "base_branch": "main",
            "files_changed": ["bug.py"],
            "goal_text": "fix the bug",
            "raw_comment_actions": [],
            "review_runs": [],
        }],
    }


class WorktreeTests(unittest.TestCase):
    """The session worktree must be pinned at head_before, never live HEAD.

    Regression test: an earlier design pointed the judge at the live repo
    checkout, so it inspected already-fixed code and wrongly verdicted real
    findings as false positives.
    """

    def test_worktree_pinned_to_head_before(self):
        with tempfile.TemporaryDirectory() as d:
            repo = Path(d) / "repo"
            head_before, head_after = _make_repo_two_commits(repo)
            session = Path(d) / "session"
            session.mkdir()
            try:
                wt = collect.ensure_worktree(session, repo, head_before)
                # The working tree must show the BUGGY version — the repo's
                # own HEAD is head_after, but the worktree is pinned.
                self.assertIn("BUG", (wt / "bug.py").read_text())
                self.assertEqual(
                    _git(wt, "rev-parse", "HEAD"), head_before
                )
                self.assertEqual(wt, collect._worktree_path(session))
            finally:
                collect.remove_worktree(session, repo)
            self.assertFalse((session / "worktree").exists())

    def test_ensure_is_idempotent(self):
        with tempfile.TemporaryDirectory() as d:
            repo = Path(d) / "repo"
            head_before, _ = _make_repo_two_commits(repo)
            session = Path(d) / "session"
            session.mkdir()
            try:
                w1 = collect.ensure_worktree(session, repo, head_before)
                w2 = collect.ensure_worktree(session, repo, head_before)
                self.assertEqual(w1, w2)
                self.assertEqual(_git(w2, "rev-parse", "HEAD"), head_before)
            finally:
                collect.remove_worktree(session, repo)

    def test_setup_pins_worktree_and_records_meta(self):
        with tempfile.TemporaryDirectory() as d:
            repo = Path(d) / "repo"
            head_before, _ = _make_repo_two_commits(repo)
            merge_base = _git(repo, "rev-list", "--max-parents=0", "HEAD")
            ds_dir = Path(d) / "ds"
            ds_dir.mkdir()
            (ds_dir / "demo-1.json").write_text(
                json.dumps(_demo_dataset(head_before, merge_base)))
            repo_map = hl.RepoMap(mapping={"demo": repo})
            result = collect.setup(
                target="demo-1", dataset_dir=ds_dir,
                session_root=Path(d) / "sessions", repo_map=repo_map)
            session = Path(result["session"])
            # The worktree is pinned at head_before (shows the buggy code).
            wt = Path(result["worktree"])
            self.assertIn("BUG", (wt / "bug.py").read_text())
            self.assertEqual(_git(wt, "rev-parse", "HEAD"), head_before)
            # Session meta records target + source repo.
            meta = collect.load_session_meta(session)
            self.assertEqual(meta["target"], "demo-1")
            self.assertEqual(meta["source_repo"], str(repo))
            collect.remove_worktree(session, repo)

    def test_build_prompt_points_repo_path_at_pinned_worktree(self):
        with tempfile.TemporaryDirectory() as d:
            repo = Path(d) / "repo"
            head_before, _ = _make_repo_two_commits(repo)
            merge_base = _git(repo, "rev-list", "--max-parents=0", "HEAD")
            ds_dir = Path(d) / "ds"
            ds_dir.mkdir()
            (ds_dir / "demo-1.json").write_text(
                json.dumps(_demo_dataset(head_before, merge_base)))
            repo_map = hl.RepoMap(mapping={"demo": repo})
            result = collect.setup(
                target="demo-1", dataset_dir=ds_dir,
                session_root=Path(d) / "sessions", repo_map=repo_map)
            session = Path(result["session"])
            try:
                prompt_path = collect.build_prompt(
                    session=session, round_n=1, envelopes=[],
                    include_harvested=False, dataset_dir=ds_dir)
                prompt = json.loads(prompt_path.read_text())
                # repo_path is the single pinned session worktree.
                self.assertEqual(
                    prompt["repo_path"],
                    str(collect._worktree_path(session)))
                self.assertIn(
                    str(collect._worktree_path(session)),
                    prompt["diff_ref"]["diff_command"])
            finally:
                collect.remove_worktree(session, repo)


class ContestedVerdictTests(unittest.TestCase):
    """A finding judged inconsistently across rounds must be quarantined,
    never silently kept in TP or FP.

    Regression intent: the accumulator used to keep whichever round came
    first, so a converged dataset could contain a finding the judges never
    actually agreed on.
    """

    def test_flip_tp_to_fp_lands_in_contested(self):
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [_fv("a.py", 10, "true_positive")],
            "census": [],
        })
        self.assertEqual(len(c.true_positives), 1)
        cl.merge_judge_round(c, 2, {
            "finding_verdicts": [_fv("a.py", 10, "false_positive")],
            "census": [],
        })
        # The flip empties both buckets and quarantines the defect.
        self.assertEqual(len(c.true_positives), 0)
        self.assertEqual(len(c.false_positives), 0)
        self.assertEqual(len(c.contested), 1)
        self.assertFalse(c.contested[0].resolved)
        # Both verdicts are in the history.
        kinds = [h["verdict"] for h in c.contested[0].verdict_history]
        self.assertEqual(kinds, ["true_positive", "false_positive"])

    def test_unresolved_contested_blocks_convergence(self):
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [_fv("a.py", 10, "true_positive")],
            "census": [],
        })
        cl.merge_judge_round(c, 2, {
            "finding_verdicts": [_fv("a.py", 10, "false_positive")],
            "census": [],
        })
        # Census is empty + stable, but a contested entry is unresolved.
        self.assertFalse(cl.census_converged(c))
        self.assertEqual(len(cl.unresolved_contested(c)), 1)

    def test_round_verdict_resolves_contested(self):
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [_fv("a.py", 10, "true_positive")],
            "census": [],
        })
        cl.merge_judge_round(c, 2, {
            "finding_verdicts": [_fv("a.py", 10, "false_positive")],
            "census": [],
        })
        # Round 3's judge re-rules it false_positive — binding.
        cl.merge_judge_round(c, 3, {
            "finding_verdicts": [_fv("a.py", 10, "false_positive")],
            "census": [],
        })
        self.assertEqual(len(c.contested), 0)
        self.assertEqual(len(c.false_positives), 1)
        self.assertTrue(c.false_positives[0].resolved)
        # All three rounds are in the history.
        self.assertEqual(len(c.false_positives[0].verdict_history), 3)
        # Census empty+stable, no unresolved contested -> converged.
        self.assertTrue(cl.census_converged(c))

    def test_agreement_across_rounds_is_not_contested(self):
        c = cl.CumulativeGT()
        for r in (1, 2):
            cl.merge_judge_round(c, r, {
                "finding_verdicts": [_fv("a.py", 10, "true_positive")],
                "census": [],
            })
        # Same verdict twice — accumulates, never contested.
        self.assertEqual(len(c.contested), 0)
        self.assertEqual(len(c.true_positives), 1)
        self.assertEqual(len(c.true_positives[0].verdict_history), 2)

    def test_unsure_does_not_flip_or_resolve(self):
        c = cl.CumulativeGT()
        cl.merge_judge_round(c, 1, {
            "finding_verdicts": [_fv("a.py", 10, "true_positive")],
            "census": [],
        })
        # An `unsure` on the same defect carries no ground truth.
        cl.merge_judge_round(c, 2, {
            "finding_verdicts": [_fv("a.py", 10, "unsure")],
            "census": [],
        })
        self.assertEqual(len(c.true_positives), 1)
        self.assertEqual(len(c.contested), 0)


class JudgeSeverityValidationTests(unittest.TestCase):
    def test_tp_verdict_needs_valid_severity(self):
        v = {"finding_verdicts": [
            {"file": "a.py", "line": 1, "verdict": "true_positive",
             "severity": "critical"}]}  # not in vocabulary
        self.assertIsNotNone(cl.validate_judge_verdict(v))

    def test_tp_verdict_missing_severity_rejected(self):
        v = {"finding_verdicts": [
            {"file": "a.py", "line": 1, "verdict": "true_positive"}]}
        self.assertIsNotNone(cl.validate_judge_verdict(v))

    def test_valid_severities_accepted(self):
        for sev in ("high", "medium", "low", "nit"):
            v = {"finding_verdicts": [
                {"file": "a.py", "line": 1, "verdict": "false_positive",
                 "severity": sev}]}
            self.assertIsNone(cl.validate_judge_verdict(v), sev)

    def test_unsure_verdict_needs_no_severity(self):
        v = {"finding_verdicts": [
            {"file": "a.py", "line": 1, "verdict": "unsure"}]}
        self.assertIsNone(cl.validate_judge_verdict(v))


class ValidateDatasetTests(unittest.TestCase):
    def _gt_block(self, **over):
        block = {
            "schema_version": cl.GROUND_TRUTH_SCHEMA_VERSION,
            "census_converged": True,
            "rounds_run": 2,
            "true_positives": [
                {"file": "a.py", "line": 10, "severity": "high"}],
            "false_positives": [],
            "contested": [],
            "per_round_diff": [],
            "dataset_xref": {"comment_action_agreement_rate": 1.0},
        }
        block.update(over)
        return {"schema_version": 2, "harvested_rounds": [], **(
            {"ground_truth_v3": block})}

    def test_clean_dataset_no_errors_no_warnings(self):
        errs, warns = cl.validate_dataset(self._gt_block())
        self.assertEqual(errs, [])
        self.assertEqual(warns, [])

    def test_uncollected_dataset_warns(self):
        errs, warns = cl.validate_dataset(
            {"schema_version": 2, "harvested_rounds": []})
        self.assertEqual(errs, [])
        self.assertTrue(any("not collected" in w for w in warns))

    def test_missing_severity_is_error(self):
        ds = self._gt_block()
        del ds["ground_truth_v3"]["true_positives"][0]["severity"]
        errs, _ = cl.validate_dataset(ds)
        self.assertTrue(any("severity" in e for e in errs))

    def test_not_converged_warns(self):
        errs, warns = cl.validate_dataset(
            self._gt_block(census_converged=False))
        self.assertEqual(errs, [])
        self.assertTrue(any("did not converge" in w for w in warns))

    def test_unresolved_contested_warns(self):
        ds = self._gt_block(contested=[
            {"file": "x.py", "line": 1, "severity": "high",
             "resolved": False}])
        errs, warns = cl.validate_dataset(ds)
        self.assertTrue(any("contested" in w for w in warns))

    def test_low_agreement_warns(self):
        ds = self._gt_block(
            dataset_xref={"comment_action_agreement_rate": 0.3})
        _, warns = cl.validate_dataset(ds)
        self.assertTrue(any("agreement" in w for w in warns))

    def test_budget_exhausted_warns(self):
        _, warns = cl.validate_dataset(
            self._gt_block(rounds_run=4), round_budget=4)
        self.assertTrue(any("budget" in w for w in warns))

    def test_schema_3_block_warns(self):
        _, warns = cl.validate_dataset(self._gt_block(schema_version=3))
        self.assertTrue(any("schema" in w for w in warns))


class RefreshIndexEntryTests(unittest.TestCase):
    def test_patches_one_entry_leaves_others(self):
        with tempfile.TemporaryDirectory() as d:
            out = Path(d)
            (out / "kernel-1.json").write_text(json.dumps({
                "schema_version": 2,
                "ground_truth_v3": {"schema_version": 4,
                                    "census_converged": True}}))
            (out / "kernel-2.json").write_text(json.dumps(
                {"schema_version": 2}))
            (out / "index.json").write_text(json.dumps({
                "schema_version": 2,
                "prs": [
                    {"file": "kernel-1.json", "ground_truth_collected": False,
                     "census_converged": None},
                    {"file": "kernel-2.json", "ground_truth_collected": False,
                     "census_converged": None},
                ]}))
            cl.refresh_index_entry(out, "kernel-1")
            index = json.loads((out / "index.json").read_text())
            e1 = next(e for e in index["prs"] if e["file"] == "kernel-1.json")
            e2 = next(e for e in index["prs"] if e["file"] == "kernel-2.json")
            self.assertTrue(e1["ground_truth_collected"])
            self.assertTrue(e1["census_converged"])
            # The untouched entry stays as it was.
            self.assertFalse(e2["ground_truth_collected"])

    def test_no_index_is_noop(self):
        with tempfile.TemporaryDirectory() as d:
            self.assertIsNone(cl.refresh_index_entry(Path(d), "kernel-1"))


class SelectTargetsTests(unittest.TestCase):
    """Collection's scan: which PRs `collect` processes by default."""

    def _fixture(self, d: Path) -> tuple[Path, Path]:
        """Build a projects dir (3 PRs) + a dataset dir (1 collected)."""
        src = d / "projects"
        ds = d / "dataset"
        src.mkdir()
        ds.mkdir()
        for pr in ("kernel-1", "kernel-2", "kernel-3"):
            (src / pr).mkdir()
            (src / pr / "pr-polish-state.json").write_text("{}")
        # kernel-2 already has a frozen ground truth.
        (ds / "kernel-2.json").write_text(json.dumps(
            {"schema_version": 2,
             "ground_truth_v3": {"schema_version": 4}}))
        # kernel-3 has a dataset file but no GT block (harvested, not collected).
        (ds / "kernel-3.json").write_text(json.dumps({"schema_version": 2}))
        return ds, src

    def test_default_scan_is_uncollected_only(self):
        with tempfile.TemporaryDirectory() as d:
            ds, src = self._fixture(Path(d))
            targets, collectable = collect.select_targets(
                dataset_dir=ds, source_dir=src, only=[], sample=None)
            self.assertEqual(len(collectable), 3)
            # kernel-2 is collected -> excluded; 1 and 3 remain.
            self.assertEqual(targets, ["kernel-1", "kernel-3"])

    def test_sample_caps_the_target_count(self):
        with tempfile.TemporaryDirectory() as d:
            ds, src = self._fixture(Path(d))
            targets, _ = collect.select_targets(
                dataset_dir=ds, source_dir=src, only=[], sample=1)
            self.assertEqual(len(targets), 1)
            self.assertIn(targets[0], ("kernel-1", "kernel-3"))

    def test_only_overrides_the_scan(self):
        with tempfile.TemporaryDirectory() as d:
            ds, src = self._fixture(Path(d))
            targets, _ = collect.select_targets(
                dataset_dir=ds, source_dir=src,
                only=["kernel-2"], sample=None)
            # --only is honored verbatim, even for an already-collected PR.
            self.assertEqual(targets, ["kernel-2"])

    def test_discover_collectable_flags_collected(self):
        with tempfile.TemporaryDirectory() as d:
            ds, src = self._fixture(Path(d))
            by_target = {
                c["target"]: c["collected"]
                for c in collect.discover_collectable(
                    dataset_dir=ds, source_dir=src)
            }
            self.assertFalse(by_target["kernel-1"])
            self.assertTrue(by_target["kernel-2"])
            self.assertFalse(by_target["kernel-3"])  # harvested != collected


class RepoRootDiscoveryTests(unittest.TestCase):
    def test_discover_keys_by_repo_name(self):
        # discover_repo_roots reads the real ~/worktrees + ~/g; assert it
        # returns a name->dir map and every value is a git checkout.
        roots = hl.discover_repo_roots()
        self.assertIsInstance(roots, dict)
        for name, path in roots.items():
            self.assertTrue((path / ".git").exists(), f"{name}: {path}")

    def test_repomap_discover_merges_overrides(self):
        rm = hl.RepoMap.discover(["zzz-custom=/tmp/zzz"])
        self.assertEqual(rm.lookup("zzz-custom"), Path("/tmp/zzz"))


if __name__ == "__main__":
    unittest.main()
