#!/usr/bin/env python3
"""Tests for verdict.py — the computed, persisted pr-polish verdict."""

from __future__ import annotations

import json
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

SCRIPTS = Path(__file__).resolve().parents[1]
if str(SCRIPTS) not in sys.path:
    sys.path.insert(0, str(SCRIPTS))

import verdict as v  # noqa: E402


def _state(**kw):
    base = {
        "pr_number": 1,
        "completed": True,
        "exit_reason": "converged",
        "completed_at": "2026-07-30T00:00:00Z",
        "rounds": [],
    }
    base.update(kw)
    return base


class ReasonSubstanceTests(unittest.TestCase):
    """The wont_fix escape hatch: any non-empty string used to clear it."""

    def test_placeholder_reasons_rejected(self):
        for text in ("", " ", "n/a", "by design", "wontfix", "later", "no"):
            self.assertFalse(v.reason_is_substantive(text), text)

    def test_short_reason_rejected(self):
        self.assertFalse(v.reason_is_substantive("too short to argue"))

    def test_real_rationale_accepted(self):
        self.assertTrue(v.reason_is_substantive(
            "This path is unreachable because the caller validates the "
            "artifact id before dispatch, so the branch cannot execute."
        ))

    def test_long_but_contentless_still_rejected(self):
        # Padding a stock phrase to length must not buy a pass.
        self.assertFalse(v.reason_is_substantive("out of scope"))


class FilePathHeuristicTests(unittest.TestCase):
    """`ci` rows put a workflow run id in `path`; it is not a file."""

    def test_workflow_run_id_is_not_a_path(self):
        self.assertFalse(v._is_file_path("90720812688"))

    def test_real_paths_accepted(self):
        self.assertTrue(v._is_file_path("services/api/routes.py"))
        self.assertTrue(v._is_file_path("main.go"))

    def test_empty_is_not_a_path(self):
        self.assertFalse(v._is_file_path(""))
        self.assertFalse(v._is_file_path(None))


class OpenHighDeferralTests(unittest.TestCase):
    def test_bare_ack_on_high_blocks(self):
        st = _state(rounds=[{"n": 1, "comment_actions": [
            {"action": "ack", "severity": "high", "path": "a.py", "line": 1},
        ]}])
        self.assertEqual(len(v.open_high_deferrals(st)), 1)

    def test_wont_fix_with_real_reason_resolves(self):
        st = _state(rounds=[{"n": 1, "comment_actions": [
            {"action": "wont_fix", "severity": "high", "path": "a.py",
             "line": 1,
             "reason": "The value is validated upstream in the dispatcher, "
                       "so this branch cannot be reached at runtime."},
        ]}])
        self.assertEqual(v.open_high_deferrals(st), [])

    def test_wont_fix_rationale_under_notes_resolves(self):
        # SKILL.md documents `notes`/`reason` as interchangeable on an action
        # row, but this check read `reason` alone — so a real rationale filed
        # under `notes` was invisible and the high stayed "open" forever. Seen
        # in the wild: five substantive deferrals had to be hand-backfilled
        # into `reason` before the checker would see them.
        st = _state(rounds=[{"n": 1, "comment_actions": [
            {"action": "wont_fix", "severity": "high", "path": "a.py",
             "line": 1,
             "notes": "The value is validated upstream in the dispatcher, "
                      "so this branch cannot be reached at runtime."},
        ]}])
        self.assertEqual(v.open_high_deferrals(st), [])

    def test_placeholder_notes_still_block(self):
        # The fallback widens which field is read, not what counts as an
        # argument: a placeholder under `notes` is no better than under
        # `reason`.
        st = _state(rounds=[{"n": 1, "comment_actions": [
            {"action": "wont_fix", "severity": "high", "path": "a.py",
             "line": 1, "notes": "by design"},
        ]}])
        self.assertEqual(len(v.open_high_deferrals(st)), 1)

    def test_bare_ack_with_notes_still_blocks(self):
        # `ack` is a deferral whatever it says: acknowledged is not resolved.
        st = _state(rounds=[{"n": 1, "comment_actions": [
            {"action": "ack", "severity": "high", "path": "a.py", "line": 1,
             "notes": "Acknowledged; the caller validates this upstream so "
                      "the branch is unreachable in practice today."},
        ]}])
        self.assertEqual(len(v.open_high_deferrals(st)), 1)

    def test_wont_fix_with_placeholder_reason_blocks(self):
        st = _state(rounds=[{"n": 1, "comment_actions": [
            {"action": "wont_fix", "severity": "high", "path": "a.py",
             "line": 1, "reason": "n/a"},
        ]}])
        self.assertEqual(len(v.open_high_deferrals(st)), 1)

    def test_low_severity_deferral_does_not_block(self):
        st = _state(rounds=[{"n": 1, "comment_actions": [
            {"action": "ack", "severity": "low", "path": "a.py", "line": 1},
        ]}])
        self.assertEqual(v.open_high_deferrals(st), [])

    def test_later_fixed_supersedes_earlier_ack(self):
        row = {"severity": "high", "path": "a.py", "line": 1, "topic": "t",
               "source": "codex"}
        st = _state(rounds=[
            {"n": 1, "comment_actions": [dict(row, action="ack")]},
            {"n": 2, "comment_actions": [dict(row, action="fixed")]},
        ])
        self.assertEqual(v.open_high_deferrals(st), [])


class VerdictTests(unittest.TestCase):
    def test_summary_surfaces_review_vs_orchestrator_wall_time(self):
        st = _state(rounds=[{
            "n": 1,
            "review_wall_ms": 420_000,
            "orchestrator_wall_ms": 10_800_000,
        }])
        row = v.compute_verdict(st)["evidence"]["round_timing"][0]
        self.assertEqual(row["review_wall_ms"], 420_000)
        self.assertEqual(row["orchestrator_wall_ms"], 10_800_000)
        self.assertIn("review=7m0s", row["summary"])
        self.assertIn("orchestrator=3h0m0s", row["summary"])

    def test_capped_at_max_can_never_be_ready(self):
        # The headline conflation: budget exhaustion previously reached the
        # same summary as genuine convergence.
        out = v.compute_verdict(_state(exit_reason="capped-at-max"))
        self.assertEqual(out["verdict"], "not_ready")
        self.assertIn("abnormal_exit", [b["code"] for b in out["blockers"]])

    def test_reviewers_unavailable_can_never_be_ready(self):
        # A batch where every reviewer failed to return a verdict never held
        # the local bar. The diff may be fine, but nothing here checked it —
        # and under the batch protocol that hands the first real review to the
        # external reviewers. kernel#8682 b1 pushed exactly this way.
        out = v.compute_verdict(_state(exit_reason="reviewers-unavailable"))
        self.assertEqual(out["verdict"], "not_ready")
        self.assertIn("abnormal_exit", [b["code"] for b in out["blockers"]])

    def test_a_round_with_no_live_stream_blocks_ready(self):
        # The automated half of the stream_status contract. Without it the
        # field is written and never read, and a completed+converged run whose
        # every stream errored still reports ready — "nobody looked" reading as
        # "nothing to find".
        st = _state(rounds=[{
            "n": 1,
            "comment_actions": [],
            "stream_status": {"codex": "error", "cursor": "error"},
        }])
        out = v.compute_verdict(st)
        self.assertEqual(out["verdict"], "not_ready")
        self.assertIn("no_live_reviewer", [b["code"] for b in out["blockers"]])

    def test_a_partial_stream_counts_as_a_live_reviewer(self):
        # partial carries real findings alongside the failure that ended the
        # run, so it is review coverage — not a dead round.
        st = _state(rounds=[{
            "n": 1,
            "comment_actions": [],
            "stream_status": {"codex": "partial", "cursor": "error"},
        }])
        out = v.compute_verdict(st)
        self.assertNotIn("no_live_reviewer", [b["code"] for b in out["blockers"]])

    def test_rounds_predating_stream_status_are_not_judged(self):
        # No recorded status is "no data", not "nobody looked". Assuming the
        # latter is the same absent-vs-empty conflation this field ends.
        st = _state(rounds=[{"n": 1, "comment_actions": []}])
        self.assertEqual(v.rounds_without_a_live_stream(st), [])
        self.assertEqual(v.compute_verdict(st)["verdict"], "ready")

    def test_silent_reviewer_advisory_names_the_cause_when_known(self):
        st = _state(rounds=[{
            "n": 1,
            "comment_actions": [],
            "codex_findings": [],
            "cursor_findings": [],
            "stream_status": {"codex": "error", "cursor": "ok"},
        }])
        out = v.compute_verdict(st)
        details = [a["detail"] for a in out["advisories"] if a["code"] == "silent_reviewer"]
        self.assertTrue(
            any("never returned a verdict" in d for d in details),
            f"expected codex's silence attributed to its error status, got {details}",
        )

    def test_lint_alone_is_not_a_live_reviewer(self):
        # lint is in BACKENDS, lint_gate hardcodes status "ok", and SKILL always
        # passes its envelope — so counting it as coverage made this gate
        # unsatisfiable. Consensus finding (codex + claude), PR #314 r2.
        st = _state(rounds=[{
            "n": 1,
            "comment_actions": [],
            "stream_status": {"codex": "error", "cursor": "error", "lint": "ok"},
        }])
        out = v.compute_verdict(st)
        self.assertEqual(out["verdict"], "not_ready")
        self.assertIn("no_live_reviewer", [b["code"] for b in out["blockers"]])

    def test_a_lint_only_round_is_not_judged(self):
        # No model reviewer ran at all in the record: nothing to conclude from.
        st = _state(rounds=[{
            "n": 1, "comment_actions": [], "stream_status": {"lint": "ok"},
        }])
        self.assertEqual(v.rounds_without_a_live_stream(st), [])

    def test_clean_converged_run_is_ready(self):
        self.assertEqual(v.compute_verdict(_state())["verdict"], "ready")

    def test_incomplete_run_is_inconclusive_not_not_ready(self):
        # "Couldn't check" must stay distinct from "it's broken".
        out = v.compute_verdict(_state(completed=False, exit_reason=None))
        self.assertEqual(out["verdict"], "inconclusive")

    def test_sufficiency_dissent_is_advisory_never_blocking(self):
        st = _state(rounds=[{
            "n": 1,
            "sufficiency_claims": {"codex": {"is_confident_complete": False}},
        }])
        out = v.compute_verdict(st)
        self.assertEqual(out["verdict"], "ready")
        self.assertIn("sufficiency_dissent",
                      [a["code"] for a in out["advisories"]])

    def test_silent_reviewer_is_advisory(self):
        st = _state(rounds=[{"n": 1, "codex_findings": [],
                             "cursor_findings": [{"x": 1}]}])
        out = v.compute_verdict(st)
        self.assertIn("silent_reviewer",
                      [a["code"] for a in out["advisories"]])
        self.assertEqual(out["verdict"], "ready")


class FixClaimVerificationTests(unittest.TestCase):
    def _repo(self, td):
        repo = Path(td)
        run = lambda *a: subprocess.run(
            ["git", "-C", str(repo), *a], capture_output=True, text=True)
        run("init", "-q")
        run("config", "user.email", "t@t")
        run("config", "user.name", "t")
        (repo / "a.py").write_text("one\n")
        (repo / "b.py").write_text("one\n")
        run("add", "-A")
        run("commit", "-qm", "one")
        before = run("rev-parse", "HEAD").stdout.strip()
        (repo / "a.py").write_text("two\n")
        run("add", "-A")
        run("commit", "-qm", "two")
        after = run("rev-parse", "HEAD").stdout.strip()
        return repo, before, after

    def test_claim_on_touched_file_verifies(self):
        with tempfile.TemporaryDirectory() as td:
            repo, before, after = self._repo(td)
            st = _state(rounds=[{
                "n": 1, "head_before": before, "head_after": after,
                "comment_actions": [
                    {"action": "fixed", "severity": "high", "path": "a.py"},
                ]}])
            res = v.verify_fix_claims(st, repo)
            self.assertEqual((res["total"], res["verified"]), (1, 1))

    def test_claim_on_untouched_file_is_unverified_and_blocks(self):
        with tempfile.TemporaryDirectory() as td:
            repo, before, after = self._repo(td)
            st = _state(rounds=[{
                "n": 1, "head_before": before, "head_after": after,
                "comment_actions": [
                    {"action": "fixed", "severity": "high", "path": "b.py"},
                ]}])
            self.assertEqual(v.verify_fix_claims(st, repo)["unverified"], 1)
            out = v.compute_verdict(st, repo_root=repo)
            self.assertEqual(out["verdict"], "not_ready")
            self.assertIn("unverified_fix_claim",
                          [b["code"] for b in out["blockers"]])

    def test_round_that_committed_nothing_but_claims_fixed(self):
        # The sharpest fabrication signal.
        with tempfile.TemporaryDirectory() as td:
            repo, before, _ = self._repo(td)
            st = _state(rounds=[{
                "n": 1, "head_before": before, "head_after": before,
                "comment_actions": [
                    {"action": "fixed", "severity": "high", "path": "a.py"},
                ]}])
            res = v.verify_fix_claims(st, repo)
            self.assertEqual(res["unverified"], 1)
            self.assertTrue(res["unverified_rows"][0]["no_commit"])

    def test_absent_commits_are_unknown_not_unverified(self):
        # An old PR whose branch was deleted: we cannot check, and saying
        # "unverified" would be indistinguishable from real fabrication.
        with tempfile.TemporaryDirectory() as td:
            repo, _, _ = self._repo(td)
            st = _state(rounds=[{
                "n": 1, "head_before": "0" * 40, "head_after": "1" * 40,
                "comment_actions": [
                    {"action": "fixed", "severity": "high", "path": "a.py"},
                ]}])
            res = v.verify_fix_claims(st, repo)
            self.assertEqual(res["unverified"], 0)
            self.assertEqual(res["unknown"], 1)
            self.assertEqual(v.compute_verdict(st, repo_root=repo)["verdict"],
                             "ready")

    def test_ci_workflow_id_is_not_counted(self):
        with tempfile.TemporaryDirectory() as td:
            repo, before, after = self._repo(td)
            st = _state(rounds=[{
                "n": 1, "head_before": before, "head_after": after,
                "comment_actions": [
                    {"action": "fixed", "severity": "high",
                     "source": "ci", "path": "90720812688"},
                ]}])
            res = v.verify_fix_claims(st, repo)
            self.assertEqual((res["total"], res["unverified"]), (0, 0))

    def test_fix_landing_in_a_later_round_is_not_a_false_positive(self):
        with tempfile.TemporaryDirectory() as td:
            repo, before, after = self._repo(td)
            st = _state(rounds=[
                {"n": 1, "head_before": before, "head_after": before,
                 "comment_actions": [
                     {"action": "fixed", "severity": "high", "path": "a.py"}]},
                {"n": 2, "head_before": before, "head_after": after,
                 "comment_actions": []},
            ])
            self.assertEqual(v.verify_fix_claims(st, repo)["unverified"], 0)


class ClaimPathMatchingTests(unittest.TestCase):
    """Fix-claim paths must not collapse to basenames.

    In a monorepo the basenames repeat constantly — BUILD.bazel, SKILL.md,
    __init__.py, tests/test_*.py — so a basename comparison verified a
    claim on one directory's file against an edit to a completely
    different one. That silently understates ``unverified``, which is the
    number the fabrication blocker gates on.
    """

    def test_same_basename_different_directory_does_not_verify(self):
        self.assertFalse(
            v._claim_matches("pkg/a/util.go", {"other/util.go"}))

    def test_exact_path_verifies(self):
        self.assertTrue(
            v._claim_matches("pkg/a/util.go", {"pkg/a/util.go", "x/y.go"}))

    def test_partial_path_suffix_verifies(self):
        # Reviewers cite varying amounts of the path; a component-aligned
        # suffix is still the same file.
        self.assertTrue(v._claim_matches("a/util.go", {"pkg/a/util.go"}))
        self.assertTrue(v._claim_matches("pkg/a/util.go", {"a/util.go"}))

    def test_partial_component_is_not_a_suffix(self):
        # "autil.go" must not match "pkg/a/util.go" via string suffix.
        self.assertFalse(v._claim_matches("autil.go", {"pkg/a/util.go"}))

    def test_end_to_end_claim_on_untouched_same_basename_file(self):
        with tempfile.TemporaryDirectory() as td:
            repo = Path(td)
            run = lambda *a: subprocess.run(
                ["git", "-C", str(repo), *a], capture_output=True, text=True)
            run("init", "-q")
            run("config", "user.email", "t@t")
            run("config", "user.name", "t")
            for d in ("pkg", "other"):
                (repo / d).mkdir()
                (repo / d / "util.go").write_text("package x\n")
            run("add", "-A")
            run("commit", "-qm", "base")
            before = run("rev-parse", "HEAD").stdout.strip()
            (repo / "other" / "util.go").write_text("package x // edited\n")
            run("add", "-A")
            run("commit", "-qm", "touch other only")
            after = run("rev-parse", "HEAD").stdout.strip()

            st = _state(rounds=[{
                "n": 1, "head_before": before, "head_after": after,
                "comment_actions": [
                    {"action": "fixed", "severity": "high",
                     "path": "pkg/util.go"}],
            }])
            out = v.verify_fix_claims(st, repo)
            self.assertEqual(
                out["unverified"], 1,
                "claim on pkg/util.go verified against an edit to "
                "other/util.go — basename collision",
            )


class ReviewerStreamRosterTests(unittest.TestCase):
    """``reviewer_stream_health`` must know every backend in the roster.

    Its stated job is to distinguish "backend ran and found nothing" from
    "backend never ran". A backend missing from its key map produces no
    entry at all — which is the *same* output as never having run, so the
    function silently reintroduces the ambiguity it exists to remove.
    """

    def test_reports_every_backend_in_the_roster(self):
        import bramble_ops

        st = _state(rounds=[{
            "n": 1,
            **{f"{b}_findings": [] for b in bramble_ops.BACKENDS},
        }])
        self.assertEqual(
            set(v.reviewer_stream_health(st)), set(bramble_ops.BACKENDS),
            "reviewer_stream_health omits a backend pr_ops writes; a run "
            "where it found nothing is indistinguishable from one where it "
            "never ran",
        )

    def test_claude_stream_is_counted(self):
        # The concrete regression: claude joined the roster after this map
        # was written, so claude rounds reported no claude stream at all.
        st = _state(rounds=[{"n": 1, "claude_findings": [{"x": 1}, {"y": 2}]}])
        self.assertEqual(v.reviewer_stream_health(st).get("claude"), 2)


def _fix(path, invariant=None, **kw):
    a = {"action": "fixed", "path": path, "severity": "medium"}
    if invariant:
        a["invariant"] = invariant
    a.update(kw)
    return a


class RefutedClassClaimTests(unittest.TestCase):
    """A class claim is refuted by a later round fixing the same file.

    The point of the check is that the refuting evidence comes from a
    *later round's* behaviour, so it cannot be satisfied by prose in the
    claiming round — the failure mode that made ``sites_found`` useless.
    """

    def test_later_round_touching_the_file_refutes_the_claim(self):
        st = _state(rounds=[
            {"n": 1, "comment_actions": [_fix("a.go", "every path drains")]},
            {"n": 2, "comment_actions": [_fix("a.go")]},
        ])
        rows = v.refuted_class_claims(st)
        self.assertEqual(len(rows), 1)
        self.assertEqual(rows[0]["round"], 1)
        self.assertEqual(rows[0]["refuted_by_round"], 2)
        self.assertEqual(rows[0]["invariant"], "every path drains")

    def test_claim_that_holds_is_not_refuted(self):
        st = _state(rounds=[
            {"n": 1, "comment_actions": [_fix("a.go", "every path drains")]},
            {"n": 2, "comment_actions": [_fix("b.go")]},
        ])
        self.assertEqual(v.refuted_class_claims(st), [])

    def test_several_sites_in_the_claiming_round_do_not_self_refute(self):
        # A class fixed at three sites in one round is the GOOD case —
        # counting it as a refutation would penalise exactly the
        # behaviour the skill asks for.
        st = _state(rounds=[{"n": 1, "comment_actions": [
            _fix("a.go", "every path drains"),
            _fix("a.go"),
            _fix("a.go"),
        ]}])
        self.assertEqual(v.refuted_class_claims(st), [])

    def test_fix_without_an_invariant_makes_no_class_claim(self):
        st = _state(rounds=[
            {"n": 1, "comment_actions": [_fix("a.go")]},
            {"n": 2, "comment_actions": [_fix("a.go")]},
        ])
        self.assertEqual(v.refuted_class_claims(st), [])

    def test_two_invariants_on_one_file_are_separate_claims(self):
        # Keying by path alone would hide the second refutation behind
        # the first claim.
        st = _state(rounds=[
            {"n": 1, "comment_actions": [
                _fix("a.go", "drains"), _fix("a.go", "fails closed"),
            ]},
            {"n": 2, "comment_actions": [_fix("a.go")]},
        ])
        self.assertEqual(
            sorted(r["invariant"] for r in v.refuted_class_claims(st)),
            ["drains", "fails closed"],
        )

    def test_a_claim_is_reported_once_not_every_later_round(self):
        st = _state(rounds=[
            {"n": 1, "comment_actions": [_fix("a.go", "drains")]},
            {"n": 2, "comment_actions": [_fix("a.go")]},
            {"n": 3, "comment_actions": [_fix("a.go")]},
        ])
        self.assertEqual(len(v.refuted_class_claims(st)), 1)

    def test_surfaces_as_an_advisory_and_never_blocks(self):
        st = _state(rounds=[
            {"n": 1, "comment_actions": [_fix("a.go", "drains")]},
            {"n": 2, "comment_actions": [_fix("a.go")]},
        ])
        out = v.compute_verdict(st)
        codes = [a["code"] for a in out["advisories"]]
        self.assertIn("class_claim_refuted", codes)
        self.assertNotIn(
            "class_claim_refuted", [b["code"] for b in out["blockers"]],
            "made a blocker, the check would push the loop to stop "
            "labelling invariants — losing the signal it runs on",
        )
        self.assertEqual(out["verdict"], "ready")


class WritePersistsToStateTests(unittest.TestCase):
    """``--write`` must update the state file, not only the sidecar.

    Consumers read pr-polish-state.json; a verdict written only to the
    sidecar is invisible to every one of them.
    """

    def setUp(self):
        tmp = tempfile.TemporaryDirectory()
        self.addCleanup(tmp.cleanup)
        self.d = Path(tmp.name)
        (self.d / "pr-polish-state.json").write_text(json.dumps(_state()))

    def _written_state(self):
        return json.loads((self.d / "pr-polish-state.json").read_text())

    def test_write_updates_state_file(self):
        v.main([str(self.d), "--write"])
        state = self._written_state()
        self.assertIn(state["verdict"]["verdict"], {"ready", "not_ready"})
        self.assertTrue((self.d / "verdict.json").is_file())

    def test_write_preserves_existing_state_keys(self):
        v.main([str(self.d), "--write"])
        state = self._written_state()
        self.assertEqual(state["pr_number"], 1)
        self.assertEqual(state["exit_reason"], "converged")

    def test_missing_state_file_reports_and_writes_nothing(self):
        (self.d / "pr-polish-state.json").unlink()
        self.assertEqual(v.main([str(self.d), "--write"]), 2)
        self.assertFalse((self.d / "verdict.json").exists())


class Step5InvocationContractTests(unittest.TestCase):
    """SKILL.md must actually invoke verdict.py at exit.

    This script shipped complete and tested while nothing called it — the
    whole defect this wiring fixes. Every other test here drives ``v.main``
    directly, so deleting the Step 5 invocation, or dropping ``--repo-root``
    (which silently disables fix-claim verification) or ``--write``, leaves
    the suite green and restores the original no-op.
    """

    @classmethod
    def setUpClass(cls):
        cls.skill_md = (SCRIPTS.parent / "SKILL.md").read_text()
        cls.invocation = next(
            (ln for ln in cls.skill_md.splitlines() if "scripts/verdict.py" in ln),
            None,
        )

    def test_skill_md_invokes_verdict_py(self):
        self.assertIsNotNone(
            self.invocation, "SKILL.md no longer runs verdict.py — the verdict "
            "is unreachable and the run reports prose again")

    def test_invocation_passes_repo_root_and_write(self):
        for flag in ("--repo-root", "--write"):
            self.assertIn(
                flag, self.invocation,
                f"Step 5 dropped {flag}; without --repo-root fix-claim "
                f"verification silently skips, without --write nothing persists")


if __name__ == "__main__":
    unittest.main()


class StatusVocabularySharingTests(unittest.TestCase):
    """One definition of "this envelope carries a body", not two.

    STATUSES_WITH_BODY says a future body-carrying status joins there and every
    consumer follows. A parallel literal in verdict.py made that false the round
    after it was written.
    """

    def test_live_statuses_derive_from_the_shared_constant(self):
        import bramble_ops
        self.assertEqual(
            set(v._LIVE_STREAM_STATUSES), set(bramble_ops.STATUSES_WITH_BODY)
        )

    def test_a_new_body_carrying_status_propagates(self):
        import bramble_ops
        import importlib
        orig = bramble_ops.STATUSES_WITH_BODY
        try:
            bramble_ops.STATUSES_WITH_BODY = ("ok", "partial", "truncated")
            importlib.reload(v)
            self.assertIn("truncated", v._LIVE_STREAM_STATUSES)
        finally:
            bramble_ops.STATUSES_WITH_BODY = orig
            importlib.reload(v)
