"""Unit tests for doc_ops. Hermetic — no bramble, no subprocess."""

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
PR_POLISH_SCRIPTS = PARENT.parent.parent / "pr-polish" / "scripts"
for p in (str(PARENT), str(PR_POLISH_SCRIPTS)):
    if p not in sys.path:
        sys.path.insert(0, p)

import doc_ops  # noqa: E402


class TestDocSlug(unittest.TestCase):
    def test_stable_for_same_path(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "milestone.md"
            p.write_text("x")
            self.assertEqual(doc_ops.doc_slug(p), doc_ops.doc_slug(p))
            slug = doc_ops.doc_slug(p)
            self.assertTrue(slug.startswith("milestone-"))
            self.assertEqual(len(slug.split("-")[-1]), 12)

    def test_different_dirs_same_basename_get_different_slugs(self):
        with tempfile.TemporaryDirectory() as d1, tempfile.TemporaryDirectory() as d2:
            (Path(d1) / "design.md").write_text("a")
            (Path(d2) / "design.md").write_text("b")
            self.assertNotEqual(
                doc_ops.doc_slug(Path(d1) / "design.md"),
                doc_ops.doc_slug(Path(d2) / "design.md"),
            )

    def test_filesystem_unsafe_basename_sanitized(self):
        self.assertEqual(doc_ops._sanitize_basename("foo bar baz"), "foo-bar-baz")
        self.assertEqual(doc_ops._sanitize_basename(""), "doc")
        self.assertEqual(doc_ops._sanitize_basename("---"), "doc")


class TestIdentify(unittest.TestCase):
    def test_returns_canonical_record(self):
        with tempfile.TemporaryDirectory() as repo:
            doc_dir = Path(repo) / "docs"
            doc_dir.mkdir()
            doc = doc_dir / "x.md"
            doc.write_text("x")
            rec = doc_ops.identify(doc, repo_root=Path(repo))
            self.assertEqual(rec["doc_path"], "docs/x.md")
            self.assertEqual(rec["doc_path_abs"], str(doc.resolve()))
            self.assertTrue(rec["doc_slug"].startswith("x-"))
            self.assertEqual(rec["ctx"], f"doc:{rec['doc_slug']}")

    def test_missing_file_rejected(self):
        with tempfile.TemporaryDirectory() as repo:
            with self.assertRaises(FileNotFoundError):
                doc_ops.identify(Path(repo) / "nope.md", repo_root=Path(repo))

    def test_directory_rejected(self):
        with tempfile.TemporaryDirectory() as repo:
            with self.assertRaises(ValueError):
                doc_ops.identify(Path(repo), repo_root=Path(repo))

    def test_doc_outside_repo_rejected(self):
        with tempfile.TemporaryDirectory() as repo, tempfile.TemporaryDirectory() as elsewhere:
            stray = Path(elsewhere) / "x.md"
            stray.write_text("x")
            with self.assertRaises(ValueError) as ctx:
                doc_ops.identify(stray, repo_root=Path(repo))
            self.assertIn("not under", str(ctx.exception))


class TestParseCtx(unittest.TestCase):
    def test_doc_prefix_required(self):
        with self.assertRaises(ValueError):
            doc_ops._parse_ctx("12345")  # PR number — wrong skill
        with self.assertRaises(ValueError):
            doc_ops._parse_ctx("branch:foo")  # wrong skill
        with self.assertRaises(ValueError):
            doc_ops._parse_ctx("doc:")
        self.assertEqual(doc_ops._parse_ctx("doc:abc123"), "abc123")


class TestStateFinalize(unittest.TestCase):
    def setUp(self):
        self._td = tempfile.TemporaryDirectory()
        self._tmp = Path(self._td.name)

        def fake_paths(slug):
            d = self._tmp / f"yoloswe-doc-{slug}"
            return d, d / "design-doc-polish-state.json"

        self._patch = patch.object(doc_ops, "state_paths_for_doc", side_effect=fake_paths)
        self._patch.start()

    def tearDown(self):
        self._patch.stop()
        self._td.cleanup()

    def _make_doc(self):
        repo = self._tmp / "repo"
        repo.mkdir()
        doc = repo / "design.md"
        doc.write_text("x")
        return repo, doc

    def test_finalize_seeds_state_on_round_1(self):
        repo, doc = self._make_doc()
        rec = doc_ops.identify(doc, repo_root=repo)
        actions = [{
            "source": "codex", "section": "Milestone 2", "dimension": "q1",
            "severity": "high", "topic": "x", "action": "fixed",
        }]
        state = doc_ops.state_finalize_round(
            rec["ctx"], 1, actions,
            doc_path=rec["doc_path"], doc_path_abs=rec["doc_path_abs"],
            rubric=["q1?", "q2?"], rubric_source="default-4-questions",
        )
        self.assertEqual(state["doc_path"], "design.md")
        self.assertEqual(state["doc_slug"], rec["doc_slug"])
        self.assertEqual(state["rubric"], ["q1?", "q2?"])
        self.assertEqual(state["rubric_source"], "default-4-questions")
        self.assertFalse(state["completed"])
        self.assertEqual(len(state["rounds"]), 1)
        rnd = state["rounds"][0]
        self.assertEqual(rnd["fixed_count"], 1)
        self.assertEqual(rnd["skipped_count"], 0)
        self.assertEqual(rnd["top_severity"], "high")

    def test_finalize_overwrites_round_on_re_call(self):
        # Re-finalizing an existing round replaces its actions and counts
        # — useful when the orchestrator amends actions before exiting.
        repo, doc = self._make_doc()
        rec = doc_ops.identify(doc, repo_root=repo)
        for actions in [
            [{"source": "codex", "section": "S", "dimension": "q1",
              "severity": "high", "topic": "x", "action": "fixed"}],
            [{"source": "codex", "section": "S", "dimension": "q1",
              "severity": "low", "topic": "x", "action": "wont_fix"},
             {"source": "cursor", "section": "T", "dimension": "q2",
              "severity": "medium", "topic": "y", "action": "fixed"}],
        ]:
            state = doc_ops.state_finalize_round(
                rec["ctx"], 1, actions,
                doc_path=rec["doc_path"], doc_path_abs=rec["doc_path_abs"],
                rubric=["q1?"], rubric_source="default-4-questions",
            )
        rnd = state["rounds"][0]
        self.assertEqual(len(rnd["comment_actions"]), 2)
        self.assertEqual(rnd["fixed_count"], 1)
        self.assertEqual(rnd["skipped_count"], 1)

    def test_finalize_rejects_doc_path_mismatch(self):
        # Slug collision (or relative-path resolved under a different
        # worktree) → refuse rather than overwrite.
        repo, doc = self._make_doc()
        rec = doc_ops.identify(doc, repo_root=repo)
        doc_ops.state_finalize_round(
            rec["ctx"], 1, [],
            doc_path=rec["doc_path"], doc_path_abs=rec["doc_path_abs"],
            rubric=["q1?"], rubric_source="default-4-questions",
        )
        with self.assertRaises(RuntimeError) as ctx:
            doc_ops.state_finalize_round(
                rec["ctx"], 2, [],
                doc_path=rec["doc_path"], doc_path_abs="/some/other/abs/path.md",
                rubric=["q1?"], rubric_source="default-4-questions",
            )
        self.assertIn("slug collision", str(ctx.exception))

    def test_state_load_returns_empty_when_absent(self):
        self.assertEqual(doc_ops.state_load("doc:never-written-abc123"), {})

    def test_mark_complete(self):
        repo, doc = self._make_doc()
        rec = doc_ops.identify(doc, repo_root=repo)
        doc_ops.state_finalize_round(
            rec["ctx"], 1, [],
            doc_path=rec["doc_path"], doc_path_abs=rec["doc_path_abs"],
            rubric=["q1?"], rubric_source="default-4-questions",
        )
        state = doc_ops.state_mark_complete(rec["ctx"], "converged")
        self.assertTrue(state["completed"])
        self.assertEqual(state["exit_reason"], "converged")
        self.assertIn("completed_at", state)

    def test_finalize_clears_completed_on_re_run(self):
        # A new round on a previously-completed state file un-completes
        # it so mid-loop reads aren't contradictory.
        repo, doc = self._make_doc()
        rec = doc_ops.identify(doc, repo_root=repo)
        doc_ops.state_finalize_round(
            rec["ctx"], 1, [],
            doc_path=rec["doc_path"], doc_path_abs=rec["doc_path_abs"],
            rubric=["q1?"], rubric_source="default-4-questions",
        )
        doc_ops.state_mark_complete(rec["ctx"], "converged")
        state = doc_ops.state_finalize_round(
            rec["ctx"], 2, [],
            doc_path=rec["doc_path"], doc_path_abs=rec["doc_path_abs"],
            rubric=["q1?"], rubric_source="default-4-questions",
        )
        self.assertFalse(state["completed"])
        self.assertIsNone(state["exit_reason"])


class TestPersistRoundFindings(unittest.TestCase):
    def setUp(self):
        self._td = tempfile.TemporaryDirectory()
        self._tmp = Path(self._td.name)

        def fake_paths(slug):
            d = self._tmp / f"yoloswe-doc-{slug}"
            return d, d / "design-doc-polish-state.json"

        self._patch = patch.object(doc_ops, "state_paths_for_doc", side_effect=fake_paths)
        self._patch.start()

    def tearDown(self):
        self._patch.stop()
        self._td.cleanup()

    def test_omitted_backend_findings_cleared(self):
        # Re-finalize that omits a backend must clear its findings —
        # otherwise next round's audit shows stale data.
        repo = self._tmp / "repo"
        repo.mkdir()
        doc = repo / "design.md"
        doc.write_text("x")
        rec = doc_ops.identify(doc, repo_root=repo)
        codex_env = self._tmp / "codex.json"
        cursor_env = self._tmp / "cursor.json"
        payload = {
            "schema_version": 1, "status": "ok", "review_mode": "design-doc",
            "review": {"verdict": "ready", "confidence": 0.9, "issues": []},
        }
        codex_env.write_text(json.dumps(payload))
        cursor_env.write_text(json.dumps(payload))
        doc_ops.state_finalize_round(
            rec["ctx"], 1, [],
            doc_path=rec["doc_path"], doc_path_abs=rec["doc_path_abs"],
            rubric=["q1?"], rubric_source="default-4-questions",
            envelope_overrides={"codex": codex_env, "cursor": cursor_env},
        )
        # Re-finalize with only cursor.
        state = doc_ops.state_finalize_round(
            rec["ctx"], 1, [],
            doc_path=rec["doc_path"], doc_path_abs=rec["doc_path_abs"],
            rubric=["q1?"], rubric_source="default-4-questions",
            envelope_overrides={"cursor": cursor_env},
        )
        rnd = state["rounds"][0]
        self.assertEqual(rnd["codex_findings"], [])
        self.assertEqual(rnd["cursor_findings"], [])  # ready+0 issues = empty


class TestRecomputeCounts(unittest.TestCase):
    def test_action_verb_dispatch(self):
        actions = [
            {"action": "fixed", "severity": "high"},
            {"action": "fixed", "severity": "medium"},
            {"action": "false_positive", "severity": "low"},
            {"action": "wont_fix", "severity": "medium"},
            {"action": "stale", "severity": "low"},
        ]
        counts = doc_ops.recompute_counts(actions)
        self.assertEqual(counts["fixed_count"], 2)
        self.assertEqual(counts["skipped_count"], 3)
        self.assertEqual(counts["top_severity"], "high")


class TestReadRubricFile(unittest.TestCase):
    def test_skips_blanks_and_comments(self):
        with tempfile.NamedTemporaryFile("w", suffix=".txt", delete=False) as f:
            f.write("\n# preamble\nq1?\n  q2?  \n# inline\nq3?\n\n")
            path = f.name
        try:
            self.assertEqual(doc_ops.read_rubric_file(path), ["q1?", "q2?", "q3?"])
        finally:
            os.unlink(path)

    def test_empty_rubric_rejected(self):
        with tempfile.NamedTemporaryFile("w", suffix=".txt", delete=False) as f:
            f.write("# only comments\n")
            path = f.name
        try:
            with self.assertRaises(ValueError):
                doc_ops.read_rubric_file(path)
        finally:
            os.unlink(path)

    def test_overlong_line_rejected_by_utf8_bytes(self):
        # Mirrors bramble's loadRubricFile cap (500 bytes UTF-8). A line
        # under the codepoint cap but over the byte cap (multi-byte
        # chars) must still be rejected.
        with tempfile.NamedTemporaryFile("w", suffix=".txt", encoding="utf-8", delete=False) as f:
            line = "—" * 200  # 200 codepoints, 600 UTF-8 bytes
            f.write(line + "\n")
            path = f.name
        try:
            self.assertEqual(len(line), 200)
            self.assertEqual(len(line.encode("utf-8")), 600)
            with self.assertRaises(ValueError) as ctx:
                doc_ops.read_rubric_file(path)
            self.assertIn("exceeds", str(ctx.exception))
        finally:
            os.unlink(path)

    def test_too_many_entries_rejected(self):
        with tempfile.NamedTemporaryFile("w", suffix=".txt", delete=False) as f:
            for i in range(doc_ops._RUBRIC_MAX_ENTRIES + 5):
                f.write(f"Q{i+1}?\n")
            path = f.name
        try:
            with self.assertRaises(ValueError) as ctx:
                doc_ops.read_rubric_file(path)
            self.assertIn("cap is", str(ctx.exception))
        finally:
            os.unlink(path)

    def test_markdown_control_prefix_rejected(self):
        for content in ("- bullet?", "* asterisk?", "> blockquote?",
                        "1. ordered?", "42) closing paren?"):
            with self.subTest(content=content):
                with tempfile.NamedTemporaryFile("w", suffix=".txt", delete=False) as f:
                    f.write("Valid first line?\n")
                    f.write(content + "\n")
                    path = f.name
                try:
                    with self.assertRaises(ValueError) as ctx:
                        doc_ops.read_rubric_file(path)
                    self.assertIn("sanitization", str(ctx.exception))
                finally:
                    os.unlink(path)

    def test_sanitize_prompt_hint_unit(self):
        # Direct unit-test of the helper — the safety net for the
        # hand-mirrored Go SanitizePromptHint port.
        self.assertTrue(doc_ops._sanitize_prompt_hint("Is this clear?"))
        self.assertTrue(doc_ops._sanitize_prompt_hint("_underscore_starts_ok"))
        self.assertFalse(doc_ops._sanitize_prompt_hint(""))
        self.assertFalse(doc_ops._sanitize_prompt_hint("- dash"))
        self.assertFalse(doc_ops._sanitize_prompt_hint("# hash"))
        self.assertFalse(doc_ops._sanitize_prompt_hint("> blockquote"))
        self.assertFalse(doc_ops._sanitize_prompt_hint("1. ordered"))
        self.assertFalse(doc_ops._sanitize_prompt_hint("text\nwith newline"))
        self.assertFalse(doc_ops._sanitize_prompt_hint("  leading whitespace"))


if __name__ == "__main__":
    unittest.main()
