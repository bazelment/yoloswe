"""Unit tests for doc_ops. Hermetic: no bramble, no subprocess (except
git HEAD verification, which we disable via verify_head=False)."""

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
    """The slug must be (a) stable for the same path, (b) collision-safe
    across same-basename docs in different directories, and (c)
    filesystem-friendly."""

    def test_stable_for_same_path(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "milestone.md"
            p.write_text("x")
            a = doc_ops.doc_slug(p)
            b = doc_ops.doc_slug(p)
            self.assertEqual(a, b)
            self.assertTrue(a.startswith("milestone-"))
            # Hash component should be 12 hex chars.
            self.assertEqual(len(a.split("-")[-1]), 12)

    def test_different_dirs_same_basename_get_different_slugs(self):
        with tempfile.TemporaryDirectory() as d1, tempfile.TemporaryDirectory() as d2:
            p1 = Path(d1) / "design.md"
            p2 = Path(d2) / "design.md"
            p1.write_text("a")
            p2.write_text("b")
            self.assertNotEqual(doc_ops.doc_slug(p1), doc_ops.doc_slug(p2))

    def test_dotted_filename_uses_only_stem(self):
        with tempfile.TemporaryDirectory() as d:
            p = Path(d) / "v2.architecture.md"
            p.write_text("x")
            slug = doc_ops.doc_slug(p)
            # ``Path.stem`` strips only the final ``.md`` suffix; the
            # ``v2`` survives. Verify the human-readable prefix is
            # what we expect, not the full filename.
            self.assertTrue(slug.startswith("v2.architecture-"))

    def test_filesystem_unsafe_chars_sanitized(self):
        # Realistic docs can sit in oddly-named directories. The slug
        # itself uses the basename, so anything outside [A-Za-z0-9._-]
        # in the basename gets collapsed to '-'. We exercise the
        # sanitiser directly because Path can't carry a slash inside
        # the stem.
        self.assertTrue(doc_ops._sanitize_basename("foo bar baz") == "foo-bar-baz")
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
            self.assertIn("state_dir", rec)
            self.assertIn("state_file", rec)

    def test_missing_file_rejected(self):
        with tempfile.TemporaryDirectory() as repo:
            with self.assertRaises(FileNotFoundError):
                doc_ops.identify(Path(repo) / "nonexistent.md", repo_root=Path(repo))

    def test_directory_rejected(self):
        with tempfile.TemporaryDirectory() as repo:
            with self.assertRaises(ValueError):
                doc_ops.identify(Path(repo), repo_root=Path(repo))

    def test_doc_outside_repo_rejected(self):
        with tempfile.TemporaryDirectory() as repo, tempfile.TemporaryDirectory() as elsewhere:
            other = Path(elsewhere) / "stray.md"
            other.write_text("x")
            with self.assertRaises(ValueError) as ctx:
                doc_ops.identify(other, repo_root=Path(repo))
            self.assertIn("not under", str(ctx.exception))


class TestParseCtx(unittest.TestCase):
    def test_doc_prefix_required(self):
        with self.assertRaises(ValueError):
            doc_ops._parse_ctx("12345")  # PR number — wrong skill
        with self.assertRaises(ValueError):
            doc_ops._parse_ctx("branch:foo")  # branch — wrong skill
        with self.assertRaises(ValueError):
            doc_ops._parse_ctx("doc:")  # missing slug

    def test_strips_prefix(self):
        self.assertEqual(doc_ops._parse_ctx("doc:abc123"), "abc123")


class TestStateRoundTrip(unittest.TestCase):
    """Identify → append-round → finalize-round → load → mark-complete
    happy path. Patches state_paths_for_doc to a temp dir so tests don't
    pollute the real ~/.bramble/projects/."""

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
        repo = Path(self._td.name) / "repo"
        repo.mkdir()
        doc = repo / "design.md"
        doc.write_text("# Title\n")
        return repo, doc

    def test_append_round_seeds_state(self):
        repo, doc = self._make_doc()
        rec = doc_ops.identify(doc, repo_root=repo)
        state = doc_ops.state_append_round(
            rec["ctx"],
            1,
            "abc1234",
            doc_path=rec["doc_path"],
            doc_path_abs=rec["doc_path_abs"],
            rubric=["q1?", "q2?"],
            rubric_source="inferred",
            verify_head=False,
        )
        self.assertEqual(state["doc_path"], "design.md")
        self.assertEqual(state["doc_slug"], rec["doc_slug"])
        self.assertEqual(state["rubric"], ["q1?", "q2?"])
        self.assertEqual(state["rubric_source"], "inferred")
        self.assertEqual(state["current_round"], 1)
        self.assertEqual(len(state["rounds"]), 1)
        self.assertIsNotNone(state["last_heartbeat_at"])

    def test_append_round_rejects_doc_path_mismatch(self):
        # Same slug different absolute path = either a hash collision
        # or the user resolved a relative path under the wrong worktree.
        # Either way, refuse to write.
        repo, doc = self._make_doc()
        rec = doc_ops.identify(doc, repo_root=repo)
        doc_ops.state_append_round(
            rec["ctx"], 1, "sha1",
            doc_path=rec["doc_path"], doc_path_abs=rec["doc_path_abs"],
            rubric=["q1?"], rubric_source="inferred", verify_head=False,
        )
        with self.assertRaises(RuntimeError) as ctx:
            doc_ops.state_append_round(
                rec["ctx"], 2, "sha2",
                doc_path=rec["doc_path"],
                doc_path_abs="/some/other/abs/path.md",
                rubric=["q1?"], rubric_source="inferred", verify_head=False,
            )
        self.assertIn("slug collision", str(ctx.exception))

    def test_rubric_locked_mid_series(self):
        repo, doc = self._make_doc()
        rec = doc_ops.identify(doc, repo_root=repo)
        doc_ops.state_append_round(
            rec["ctx"], 1, "sha1",
            doc_path=rec["doc_path"], doc_path_abs=rec["doc_path_abs"],
            rubric=["original q1?"], rubric_source="inferred", verify_head=False,
        )
        # Round 2 mid-series passes a different rubric — must be ignored.
        state = doc_ops.state_append_round(
            rec["ctx"], 2, "sha2",
            doc_path=rec["doc_path"], doc_path_abs=rec["doc_path_abs"],
            rubric=["NEW q?"], rubric_source="inferred", verify_head=False,
        )
        self.assertEqual(state["rubric"], ["original q1?"])

    def test_rubric_re_pinned_on_new_series(self):
        repo, doc = self._make_doc()
        rec = doc_ops.identify(doc, repo_root=repo)
        doc_ops.state_append_round(
            rec["ctx"], 1, "sha1",
            doc_path=rec["doc_path"], doc_path_abs=rec["doc_path_abs"],
            rubric=["round1 q?"], rubric_source="inferred", verify_head=False,
        )
        doc_ops.state_mark_complete(rec["ctx"], "converged")
        # Now run a new series. The orchestrator may have re-inferred
        # the rubric; round 1 of the new series must adopt it.
        state = doc_ops.state_append_round(
            rec["ctx"], 1, "sha2",
            doc_path=rec["doc_path"], doc_path_abs=rec["doc_path_abs"],
            rubric=["new series q?"], rubric_source="inferred", verify_head=False,
        )
        self.assertEqual(state["rubric"], ["new series q?"])
        self.assertFalse(state.get("completed"))

    def test_finalize_recomputes_counts(self):
        repo, doc = self._make_doc()
        rec = doc_ops.identify(doc, repo_root=repo)
        doc_ops.state_append_round(
            rec["ctx"], 1, "sha1",
            doc_path=rec["doc_path"], doc_path_abs=rec["doc_path_abs"],
            rubric=["q1?"], rubric_source="inferred", verify_head=False,
        )
        actions = [
            {
                "source": "codex",
                "section": "Milestone 2",
                "dimension": "q1",
                "severity": "high",
                "topic": "x",
                "action": "fixed",
                "commit_sha": "abc",
            },
            {
                "source": "cursor",
                "section": "Intro",
                "dimension": "q1",
                "severity": "medium",
                "topic": "y",
                "action": "wont_fix",
                "reason": "design tradeoff",
            },
        ]
        state = doc_ops.state_finalize_round(
            rec["ctx"], 1, "sha2", actions,
        )
        rnd = state["rounds"][0]
        self.assertEqual(rnd["fixed_count"], 1)
        self.assertEqual(rnd["skipped_count"], 1)
        self.assertEqual(rnd["top_severity"], "high")
        self.assertEqual(rnd["head_after"], "sha2")
        self.assertEqual(len(rnd["comment_actions"]), 2)

    def test_state_load_decorates_signals(self):
        repo, doc = self._make_doc()
        rec = doc_ops.identify(doc, repo_root=repo)
        doc_ops.state_append_round(
            rec["ctx"], 1, "sha1",
            doc_path=rec["doc_path"], doc_path_abs=rec["doc_path_abs"],
            rubric=["q1?"], rubric_source="inferred", verify_head=False,
        )
        loaded = doc_ops.state_load(rec["ctx"])
        self.assertIn("is_heartbeat_stale", loaded)
        self.assertIn("is_first_round_of_series", loaded)
        # Just-written heartbeat is fresh.
        self.assertFalse(loaded["is_heartbeat_stale"])

    def test_state_is_new_series_after_complete(self):
        repo, doc = self._make_doc()
        rec = doc_ops.identify(doc, repo_root=repo)
        doc_ops.state_append_round(
            rec["ctx"], 1, "sha1",
            doc_path=rec["doc_path"], doc_path_abs=rec["doc_path_abs"],
            rubric=["q1?"], rubric_source="inferred", verify_head=False,
        )
        # Mid-series round 2 is NOT a new series.
        self.assertFalse(doc_ops.state_is_new_series(rec["ctx"], 2))
        doc_ops.state_mark_complete(rec["ctx"], "converged")
        # After complete, round 1 of the next loop IS a new series.
        self.assertTrue(doc_ops.state_is_new_series(rec["ctx"], 1))

    def test_mark_abandoned(self):
        repo, doc = self._make_doc()
        rec = doc_ops.identify(doc, repo_root=repo)
        doc_ops.state_append_round(
            rec["ctx"], 1, "sha1",
            doc_path=rec["doc_path"], doc_path_abs=rec["doc_path_abs"],
            rubric=["q1?"], rubric_source="inferred", verify_head=False,
        )
        state = doc_ops.state_mark_abandoned(rec["ctx"])
        self.assertTrue(state["completed"])
        self.assertEqual(state["exit_reason"], "abandoned")


class TestRecomputeCounts(unittest.TestCase):
    def test_action_verb_dispatch(self):
        # design-doc skill drops the pr-polish-only verbs (pre_existing,
        # flake, ack). Those rows are counted as neither fixed nor
        # skipped — they shouldn't appear here.
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


class TestMergeActions(unittest.TestCase):
    def test_dedupe_on_action_key(self):
        # Same (source, section, dimension, topic) → new wins on conflict.
        existing = [{
            "source": "codex", "section": "S", "dimension": "q1",
            "topic": "t", "severity": "medium", "action": "wont_fix",
            "reason": "old",
        }]
        new = [{
            "source": "codex", "section": "S", "dimension": "q1",
            "topic": "t", "severity": "high", "action": "fixed",
            "commit_sha": "abc",
        }]
        merged = doc_ops._merge_actions(existing, new)
        self.assertEqual(len(merged), 1)
        self.assertEqual(merged[0]["action"], "fixed")
        self.assertEqual(merged[0]["severity"], "high")

    def test_distinct_keys_kept(self):
        existing = [{
            "source": "codex", "section": "S", "dimension": "q1",
            "topic": "t", "action": "fixed",
        }]
        new = [{
            "source": "cursor", "section": "S", "dimension": "q1",
            "topic": "t", "action": "wont_fix",
        }]
        merged = doc_ops._merge_actions(existing, new)
        self.assertEqual(len(merged), 2)


class TestReadRubricFile(unittest.TestCase):
    def test_skips_blanks_and_comments(self):
        with tempfile.NamedTemporaryFile("w", suffix=".txt", delete=False) as f:
            f.write("\n# preamble\nq1?\n  q2?  \n# inline comment\nq3?\n\n")
            path = f.name
        try:
            self.assertEqual(doc_ops._read_rubric_file(path), ["q1?", "q2?", "q3?"])
        finally:
            os.unlink(path)

    def test_empty_rubric_rejected(self):
        with tempfile.NamedTemporaryFile("w", suffix=".txt", delete=False) as f:
            f.write("# only comments\n# nothing else\n")
            path = f.name
        try:
            with self.assertRaises(ValueError):
                doc_ops._read_rubric_file(path)
        finally:
            os.unlink(path)

    def test_overlong_line_rejected(self):
        # Mirrors bramble's loadRubricFile cap (500 BYTES per line in
        # UTF-8 — Go's len()). A rubric that doc_ops accepts but
        # bramble rejects every round would silently brick the loop's
        # first turn.
        with tempfile.NamedTemporaryFile("w", suffix=".txt", delete=False) as f:
            long = "x" * (doc_ops._RUBRIC_LINE_MAX_LEN + 1)
            f.write(long + "\n")
            path = f.name
        try:
            with self.assertRaises(ValueError) as ctx:
                doc_ops._read_rubric_file(path)
            self.assertIn("exceeds", str(ctx.exception))
        finally:
            os.unlink(path)

    def test_overlong_line_uses_utf8_bytes_not_codepoints(self):
        # A line just under the codepoint cap but over the byte cap (with
        # multi-byte characters) must still be rejected, mirroring Go's
        # byte-counting len(). Without this, a rubric like 250 em-dashes
        # (3 bytes each = 750 bytes) would pass Python (250 codepoints)
        # but bramble would reject it every round.
        with tempfile.NamedTemporaryFile("w", suffix=".txt", encoding="utf-8", delete=False) as f:
            line = "—" * 200  # 200 codepoints, 600 UTF-8 bytes (>500)
            f.write(line + "\n")
            path = f.name
        try:
            self.assertEqual(len(line), 200)  # codepoints
            self.assertEqual(len(line.encode("utf-8")), 600)
            with self.assertRaises(ValueError) as ctx:
                doc_ops._read_rubric_file(path)
            self.assertIn("exceeds", str(ctx.exception))
        finally:
            os.unlink(path)

    def test_too_many_entries_rejected(self):
        with tempfile.NamedTemporaryFile("w", suffix=".txt", delete=False) as f:
            for i in range(doc_ops._RUBRIC_MAX_ENTRIES + 5):
                f.write(f"Question {i+1}?\n")
            path = f.name
        try:
            with self.assertRaises(ValueError) as ctx:
                doc_ops._read_rubric_file(path)
            self.assertIn("cap is", str(ctx.exception))
        finally:
            os.unlink(path)

    def test_markdown_control_prefix_rejected(self):
        # Lines that would corrupt the surrounding numbered-list
        # rendering inside the prompt (leading '-', '*', '>', etc.)
        # are rejected at ingest. Same rule as
        # SanitizePromptHint on the Go side.
        cases = [
            ("- bullet?", "leading hyphen"),
            ("* asterisk?", "leading asterisk"),
            ("> blockquote?", "leading blockquote"),
            ("1. ordered list?", "leading ordered list"),
            ("42) closing paren?", "leading ordered list"),
        ]
        for content, label in cases:
            with self.subTest(label=label):
                with tempfile.NamedTemporaryFile("w", suffix=".txt", delete=False) as f:
                    f.write("Valid first line?\n")
                    f.write(content + "\n")
                    path = f.name
                try:
                    with self.assertRaises(ValueError) as ctx:
                        doc_ops._read_rubric_file(path)
                    self.assertIn("sanitization", str(ctx.exception))
                finally:
                    os.unlink(path)

    def test_sanitize_prompt_hint_unit(self):
        # Direct unit-test of the helper so the Python port stays
        # aligned with yoloswe/reviewer.SanitizePromptHint.
        self.assertTrue(doc_ops._sanitize_prompt_hint("Is this clear?"))
        self.assertTrue(doc_ops._sanitize_prompt_hint("_underscore_starts_ok"))
        self.assertFalse(doc_ops._sanitize_prompt_hint(""))
        self.assertFalse(doc_ops._sanitize_prompt_hint("- dash"))
        self.assertFalse(doc_ops._sanitize_prompt_hint("# hash"))
        self.assertFalse(doc_ops._sanitize_prompt_hint("> blockquote"))
        self.assertFalse(doc_ops._sanitize_prompt_hint("1. ordered"))
        self.assertFalse(doc_ops._sanitize_prompt_hint("text\nwith newline"))
        self.assertFalse(doc_ops._sanitize_prompt_hint("  leading whitespace"))


class TestPersistRoundFindingsCleansBackends(unittest.TestCase):
    """A re-finalize that omits a backend must clear its findings,
    session_ids, and resume_status (so next round's resume doesn't
    hit a stale session). Mirrors the same correctness rule as
    pr_ops._persist_round_findings."""

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
        # Seed state with both codex and cursor having prior findings,
        # then re-finalize with only cursor — codex must end up empty.
        repo = self._tmp / "repo"
        repo.mkdir()
        doc = repo / "design.md"
        doc.write_text("x")
        rec = doc_ops.identify(doc, repo_root=repo)
        doc_ops.state_append_round(
            rec["ctx"], 1, "sha1",
            doc_path=rec["doc_path"], doc_path_abs=rec["doc_path_abs"],
            rubric=["q1?"], rubric_source="inferred", verify_head=False,
        )
        # First finalize: both codex and cursor have envelopes.
        codex_env = self._tmp / "codex.json"
        cursor_env = self._tmp / "cursor.json"
        envelope_payload = lambda sid: {
            "schema_version": 1,
            "status": "ok",
            "review_mode": "design-doc",
            "session_id": sid,
            "resume_status": "ok",
            "review": {
                "verdict": "ready",
                "confidence": 0.9,
                "issues": [],
            },
        }
        codex_env.write_text(json.dumps(envelope_payload("codex-sess-1")))
        cursor_env.write_text(json.dumps(envelope_payload("cursor-sess-1")))
        doc_ops.state_finalize_round(
            rec["ctx"], 1, "sha2", [],
            envelope_overrides={"codex": codex_env, "cursor": cursor_env},
        )
        # Second finalize: only cursor. Codex must be cleared.
        doc_ops.state_finalize_round(
            rec["ctx"], 1, "sha3", [],
            envelope_overrides={"cursor": cursor_env},
        )
        loaded = doc_ops.state_load(rec["ctx"])
        rnd = loaded["rounds"][0]
        self.assertEqual(rnd.get("codex_findings"), [])
        # session_ids dict — cursor present, codex absent.
        self.assertIn("session_ids", rnd)
        self.assertIn("cursor", rnd["session_ids"])
        self.assertNotIn("codex", rnd["session_ids"])


if __name__ == "__main__":
    unittest.main()
