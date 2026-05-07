"""Focused tests for compare-r1-r2.load_envelope.

The hyphenated filename means we import via importlib rather than a regular
``import`` statement. Tests cover the four shapes the loader must handle:
plain dict envelope, NDJSON with trailing envelope, malformed middle lines
followed by a good last line, and lines lacking ``schema_version`` (which
must be rejected to avoid returning a progress event by mistake).
"""

from __future__ import annotations

import importlib.util
import json
import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
SCRIPT = HERE.parent / "compare-r1-r2.py"


def _load_module():
    spec = importlib.util.spec_from_file_location("compare_r1_r2", SCRIPT)
    mod = importlib.util.module_from_spec(spec)
    sys.modules["compare_r1_r2"] = mod
    assert spec.loader is not None
    spec.loader.exec_module(mod)
    return mod


class TestLoadEnvelope(unittest.TestCase):
    def setUp(self) -> None:
        self.mod = _load_module()

    def _write(self, text: str) -> Path:
        f = tempfile.NamedTemporaryFile(mode="w", delete=False, suffix=".json")
        f.write(text)
        f.close()
        self.addCleanup(lambda: Path(f.name).unlink(missing_ok=True))
        return Path(f.name)

    def test_plain_dict_envelope(self) -> None:
        env = {"schema_version": 1, "status": "ok", "review": {"issues": []}}
        path = self._write(json.dumps(env))
        self.assertEqual(self.mod.load_envelope(path), env)

    def test_ndjson_trailing_envelope(self) -> None:
        progress = {"event": "progress", "msg": "starting"}
        env = {"schema_version": 1, "status": "ok", "review": {"issues": [{"x": 1}]}}
        text = json.dumps(progress) + "\n" + json.dumps(env) + "\n"
        path = self._write(text)
        self.assertEqual(self.mod.load_envelope(path), env)

    def test_malformed_middle_with_good_last(self) -> None:
        env = {"schema_version": 2, "status": "ok"}
        text = "not-json\n{also bad\n" + json.dumps(env) + "\n"
        path = self._write(text)
        self.assertEqual(self.mod.load_envelope(path), env)

    def test_lines_without_schema_version_rejected(self) -> None:
        # A stream of pure progress lines with no envelope must not be
        # accepted as a fallback envelope — that's the whole reason we
        # gate on the schema_version sentinel.
        progress_only = (
            json.dumps({"event": "progress", "msg": "a"}) + "\n"
            + json.dumps({"event": "progress", "msg": "b"}) + "\n"
        )
        path = self._write(progress_only)
        self.assertIsNone(self.mod.load_envelope(path))

    def test_missing_file_returns_none(self) -> None:
        self.assertIsNone(self.mod.load_envelope(Path("/nonexistent/path.json")))

    def test_schema_version_without_status_rejected(self) -> None:
        # An envelope must carry both schema_version and status — bramble_ops'
        # extract_terminal_envelope requires both, and accepting a schema_version-
        # only object here would skew compare metrics by counting partial events
        # that downstream parsers ignore.
        partial = json.dumps({"schema_version": 1, "review": {"issues": []}})
        path = self._write(partial)
        self.assertIsNone(self.mod.load_envelope(path))


class TestEnvelopeIssues(unittest.TestCase):
    """envelope_issues tolerates partial/null envelopes without crashing."""

    def setUp(self) -> None:
        self.mod = _load_module()

    def test_none_envelope_returns_empty(self) -> None:
        self.assertEqual(self.mod.envelope_issues(None), [])

    def test_review_issues_null_returns_empty(self) -> None:
        # Pre-fix: review["issues"] = null was returned verbatim and
        # would crash compare_pr when concatenated with a list.
        self.assertEqual(
            self.mod.envelope_issues({"review": {"issues": None}}),
            [],
        )

    def test_review_missing_falls_through_to_top_level_issues(self) -> None:
        env = {"issues": [{"file": "a.py", "line": 1}]}
        self.assertEqual(self.mod.envelope_issues(env), [{"file": "a.py", "line": 1}])

    def test_canonical_review_issues_returned(self) -> None:
        env = {"review": {"issues": [{"file": "a.py"}]}}
        self.assertEqual(self.mod.envelope_issues(env), [{"file": "a.py"}])

    def test_review_as_list_does_not_crash(self) -> None:
        # Corrupt JSON could put a list or string here; the previous
        # code used .get() unconditionally, which AttributeError'd.
        self.assertEqual(self.mod.envelope_issues({"review": []}), [])
        self.assertEqual(self.mod.envelope_issues({"review": "broken"}), [])
        self.assertEqual(self.mod.envelope_issues({"review": None}), [])


class TestCommentCaughtInEnvelope(unittest.TestCase):
    """Direct coverage for the matcher heuristic. Without it, regressions in
    file/line/keyword matching would only surface via the higher-level
    compare_pr tests, which mock most of the work away.
    """

    def setUp(self) -> None:
        self.mod = _load_module()

    def test_same_file_line_within_window(self) -> None:
        comment = {"path": "pkg/foo.py", "line": 42, "body": "off-by-one in foo"}
        issues = [{"file": "pkg/foo.py", "line": 38, "message": "off-by-one"}]
        caught, evidence = self.mod.comment_caught_in_envelope(comment, issues)
        self.assertTrue(caught)
        self.assertIn("foo.py", evidence)

    def test_same_file_keyword_overlap_no_line(self) -> None:
        # Cross-package match where the issue lacks a line — must rely on
        # keyword overlap to bridge the comment to the finding.
        comment = {
            "path": "pkg/foo.py", "line": 5,
            "body": "session resume context not threaded through review wiring",
        }
        issues = [{
            "file": "pkg/foo.py", "line": None,
            "message": "session resume context wiring threaded review",
        }]
        caught, _ = self.mod.comment_caught_in_envelope(comment, issues)
        self.assertTrue(caught)

    def test_no_match_when_unrelated(self) -> None:
        comment = {"path": "pkg/foo.py", "line": 5, "body": "foo handler returns wrong type"}
        issues = [{"file": "pkg/bar.py", "line": 5, "message": "race in cache invalidation"}]
        caught, _ = self.mod.comment_caught_in_envelope(comment, issues)
        self.assertFalse(caught)

    def test_same_basename_different_paths_does_not_match(self) -> None:
        # Round 13 fix: monorepos with duplicate filenames
        # (pkg/foo.py vs other/foo.py) used to falsely match because
        # the matcher compared basenames only. Now requires full
        # path equality when both sides have a path.
        comment = {"path": "pkg/foo.py", "line": 5, "body": "null check missing"}
        issues = [{"file": "other/foo.py", "line": 5, "message": "null check missing"}]
        caught, _ = self.mod.comment_caught_in_envelope(comment, issues)
        self.assertFalse(caught)

    def test_kw_overlap_when_issue_has_no_file(self) -> None:
        # Round-14 symmetric branch: a reviewer's cross-file finding
        # (no file on the envelope issue) should still match a path-
        # bearing comment via keyword overlap. Before the fix only
        # the comment-with-no-file direction was covered.
        comment = {"path": "pkg/foo.py", "line": 5,
                   "body": "session resume threading missing for cursor backend"}
        issues = [{"file": "", "line": None,
                   "message": "session resume threading missing for cursor backend wiring"}]
        caught, _ = self.mod.comment_caught_in_envelope(comment, issues)
        self.assertTrue(caught)

    def test_confidence_none_does_not_crash(self) -> None:
        # Pre-fix bug: issue.get("confidence", 1.0) returns None when the
        # key is present-but-null, then `None < 1.0` raised TypeError and
        # aborted the batch. Lock in defensive coercion.
        comment = {"path": "pkg/foo.py", "line": 1, "body": "x"}
        issues = [{"file": "pkg/foo.py", "line": 1, "message": "x", "confidence": None}]
        caught, _ = self.mod.comment_caught_in_envelope(comment, issues)
        self.assertTrue(caught)

    def test_confidence_string_does_not_crash(self) -> None:
        comment = {"path": "pkg/foo.py", "line": 1, "body": "x"}
        issues = [{"file": "pkg/foo.py", "line": 1, "message": "x", "confidence": "high"}]
        caught, _ = self.mod.comment_caught_in_envelope(comment, issues)
        self.assertTrue(caught)


class TestIsSubstantive(unittest.TestCase):
    """is_substantive must keep substantive bot findings on
    github-issue / github-review while dropping summary-only noise.
    """

    def setUp(self) -> None:
        self.mod = _load_module()

    def test_inline_bot_finding_kept(self) -> None:
        c = {
            "author": "cursor[bot]", "is_bot": True,
            "source": "github-inline",
            "body": "null check missing on the BUILDER_LITE branch",
        }
        self.assertTrue(self.mod.is_substantive(c))

    def test_substantive_finding_on_issue_channel_kept(self) -> None:
        # Round 21 fix: bots post substantive findings as top-level
        # issue/review comments too. Pre-fix, blanket-dropping the
        # source kicked these out — the comparison script then
        # underreported r2 hits on those channels.
        c = {
            "author": "coderabbitai[bot]", "is_bot": True,
            "source": "github-issue",
            "body": "There's a race condition in the cache invalidation path "
                    "around line 42 of pkg/foo.py — the lock is released "
                    "before the write completes.",
        }
        self.assertTrue(self.mod.is_substantive(c))

    def test_review_summary_on_issue_channel_dropped(self) -> None:
        # The actual noise: BUGBOT_REVIEW summary as a top-level
        # issue comment.
        c = {
            "author": "cursor[bot]", "is_bot": True,
            "source": "github-issue",
            "body": "Cursor Bugbot has reviewed your changes and found 3 potential issues.",
        }
        self.assertFalse(self.mod.is_substantive(c))

    def test_human_comment_dropped(self) -> None:
        c = {"author": "alice", "is_bot": False, "source": "github-inline", "body": "lgtm"}
        self.assertFalse(self.mod.is_substantive(c))

    def test_summary_phrase_with_concrete_finding_kept(self) -> None:
        # Round 24 fix: a long bot comment that opens with the summary
        # phrase but then lists concrete findings is substantive, not
        # boilerplate. The naive "regex matches anywhere" check
        # excluded it.
        body = (
            "Cursor Bugbot has reviewed your changes and found 3 potential issues.\n\n"
            "1. **services/python/agent/handlers/provision.py:142** — missing error "
            "handling for the BUILDER_LITE branch; if the builder is unset, the "
            "request will silently 500 with no telemetry. Suggested fix: wrap the "
            "client construction in a try/except and emit a structured log line.\n"
            "2. **bramble/cmd/codereview/codereview.go:88** — race in deferred "
            "envelope guard; the goroutine reading the channel may fire after "
            "the deferred close runs.\n"
            "3. **scripts/lint.sh:23** — unused arg on the bazel invocation; "
            "the --keep_going flag was removed in bazel 7 and now triggers a "
            "deprecation warning that fails strict-warnings-as-errors CI runs.\n"
        )
        c = {"author": "cursor[bot]", "is_bot": True, "source": "github-issue", "body": body}
        self.assertTrue(self.mod.is_substantive(c))


    def test_null_body_does_not_crash(self) -> None:
        # r37 finding: some PR-comment exports record body: null for
        # empty-body comments. Without normalization, the regex search
        # in _is_review_summary_only / NOISE_BODY_PATTERNS would
        # TypeError and abort the whole comparison batch.
        for body in (None, ""):
            c = {"author": "cursor[bot]", "is_bot": True,
                 "source": "github-inline", "body": body}
            # Just ensure no exception; semantics for empty body are
            # "kept as bot finding" (NOISE_BODY_PATTERNS need text to
            # filter on, so empty falls through).
            self.assertEqual(self.mod.is_substantive(c), True)


class TestFormatResultsAllPass(unittest.TestCase):
    """``all_pass`` is the gate that ``main`` returns as exit code.

    Each branch must yield the right value: kernel-2755 keys on
    regressions, other PRs require full recall (caught == total), and
    total==0 is treated as skipped (neither passes nor fails the run).
    """

    def setUp(self) -> None:
        self.mod = _load_module()

    def _result(self, **overrides) -> dict:
        base = {
            "pr": "1234",
            "total_substantive": 0,
            "caught": 0,
            "findings": [],
            "r1_issue_count": 0,
            "r2_issue_count": 0,
            "r1_only_count": 0,
            "r1_cursor_loaded": True,
            "r1_codex_loaded": True,
            "r2_cursor_loaded": True,
            "r2_codex_loaded": True,
        }
        base.update(overrides)
        return base

    def test_2755_zero_regressions_with_full_recall_passes(self) -> None:
        # Round 25 tightens this: prior version left total/caught at 0/0
        # which never actually exercised the caught==total bar added
        # in r24. Use 5/5 to prove the success path.
        r = self._result(
            pr="2755", r1_only_count=0, r1_issue_count=3, r2_issue_count=3,
            total_substantive=5, caught=5,
        )
        _, all_pass = self.mod.format_results([r])
        self.assertTrue(all_pass)

    def test_2755_with_regressions_fails(self) -> None:
        r = self._result(pr="2755", r1_only_count=2, r1_issue_count=5, r2_issue_count=3)
        _, all_pass = self.mod.format_results([r])
        self.assertFalse(all_pass)

    def test_2755_partial_recall_fails(self) -> None:
        # Round 24 fix: 2755 verdict path used to consult only
        # r1_only_count, ignoring caught/total. A run with zero
        # regressions but partial substantive-comment recall would
        # still pass. Now both must be clean.
        r = self._result(
            pr="2755", r1_only_count=0, r1_issue_count=3, r2_issue_count=3,
            total_substantive=5, caught=2,
        )
        _, all_pass = self.mod.format_results([r])
        self.assertFalse(all_pass)

    def test_unloaded_r2_envelopes_force_failure(self) -> None:
        # Round 23 fix: if both r2 envelopes failed to load, the
        # comparison is structurally invalid — recall metrics from
        # no-data look clean. Forced failure with explicit verdict.
        r = self._result(
            pr="2978", r2_cursor_loaded=False, r2_codex_loaded=False,
        )
        out, all_pass = self.mod.format_results([r])
        self.assertFalse(all_pass)
        self.assertIn("envelopes unloaded", out)

    def test_unloaded_r1_envelopes_force_failure_only_for_2755(self) -> None:
        # Round 25 fix, refined in r26: an r1 baseline that didn't load
        # means r1_only_count=0 vacuously, which would let kernel-2755
        # pass even though we never compared anything. Restricted to
        # 2755 because non-2755 kernels verify recall against r2 alone
        # (r1 is informational there, not a hard requirement).
        r_2755 = self._result(
            pr="2755", r1_only_count=0,
            r1_cursor_loaded=False, r1_codex_loaded=False,
        )
        out, all_pass = self.mod.format_results([r_2755])
        self.assertFalse(all_pass)
        self.assertIn("r1 envelopes unloaded", out)

        # A non-2755 kernel without r1 envelopes but with full r2
        # recall must still pass — r1 is informational for those.
        r_other = self._result(
            pr="2978", total_substantive=3, caught=3,
            r1_cursor_loaded=False, r1_codex_loaded=False,
        )
        _, all_pass = self.mod.format_results([r_other])
        self.assertTrue(all_pass)

    def test_partial_recall_fails(self) -> None:
        # Pre-fix bug: caught=1/3 used to read as a pass. Lock in full-recall.
        r = self._result(pr="2978", total_substantive=3, caught=1)
        _, all_pass = self.mod.format_results([r])
        self.assertFalse(all_pass)

    def test_full_recall_passes(self) -> None:
        r = self._result(pr="2978", total_substantive=3, caught=3)
        _, all_pass = self.mod.format_results([r])
        self.assertTrue(all_pass)

    def test_no_substantive_comments_neither_passes_nor_fails(self) -> None:
        # total==0 used to read as a pass via caught>=1 → False but
        # all_pass was only cleared on caught==0; really there's nothing
        # to verify. Treat as skipped: doesn't flip all_pass either way.
        r = self._result(pr="2978", total_substantive=0, caught=0)
        _, all_pass = self.mod.format_results([r])
        self.assertTrue(all_pass)


class TestMainExitCode(unittest.TestCase):
    """main() must return non-zero on regression in both Markdown and --json."""

    def setUp(self) -> None:
        self.mod = _load_module()

    def _patch_compare(self, stub_results):
        def _stub(pr, *_args, **_kw):
            return stub_results[pr]
        return _stub

    def test_markdown_failing_returns_one(self) -> None:
        # Single PR with partial recall — should return 1.
        results = {
            "1234": {
                "pr": "1234", "total_substantive": 3, "caught": 1, "findings": [],
                "r1_issue_count": 1, "r2_issue_count": 1, "r1_only_count": 0,
                "r1_cursor_loaded": True, "r1_codex_loaded": True, "r2_cursor_loaded": True, "r2_codex_loaded": True,
            }
        }
        from unittest.mock import patch  # noqa: PLC0415
        with patch.object(self.mod, "compare_pr", self._patch_compare(results)):
            rc = self.mod.main(["--prs", "1234"])
        self.assertEqual(rc, 1)

    def test_json_failing_returns_one(self) -> None:
        # Pre-fix bug: --json returned 0 even on regression. Lock the new
        # behavior: --json must share the exit-code contract with Markdown.
        results = {
            "1234": {
                "pr": "1234", "total_substantive": 3, "caught": 1, "findings": [],
                "r1_issue_count": 1, "r2_issue_count": 1, "r1_only_count": 0,
                "r1_cursor_loaded": True, "r1_codex_loaded": True, "r2_cursor_loaded": True, "r2_codex_loaded": True,
            }
        }
        from unittest.mock import patch  # noqa: PLC0415
        with patch.object(self.mod, "compare_pr", self._patch_compare(results)):
            rc = self.mod.main(["--prs", "1234", "--json"])
        self.assertEqual(rc, 1)

    def test_json_passing_returns_zero(self) -> None:
        results = {
            "1234": {
                "pr": "1234", "total_substantive": 3, "caught": 3, "findings": [],
                "r1_issue_count": 1, "r2_issue_count": 1, "r1_only_count": 0,
                "r1_cursor_loaded": True, "r1_codex_loaded": True, "r2_cursor_loaded": True, "r2_codex_loaded": True,
            }
        }
        from unittest.mock import patch  # noqa: PLC0415
        with patch.object(self.mod, "compare_pr", self._patch_compare(results)):
            rc = self.mod.main(["--prs", "1234", "--json"])
        self.assertEqual(rc, 0)

    def test_json_payload_includes_all_pass_and_results(self) -> None:
        # Round 30: prior --json tests asserted only the exit code.
        # main() could regress the payload shape (e.g. drop all_pass
        # or wrap results differently) without failing those tests.
        # Capture stdout and parse the emitted JSON.
        results = {
            "1234": {
                "pr": "1234", "total_substantive": 3, "caught": 3, "findings": [],
                "r1_issue_count": 1, "r2_issue_count": 1, "r1_only_count": 0,
                "r1_cursor_loaded": True, "r1_codex_loaded": True,
                "r2_cursor_loaded": True, "r2_codex_loaded": True,
            }
        }
        import io  # noqa: PLC0415
        from contextlib import redirect_stdout  # noqa: PLC0415
        from unittest.mock import patch  # noqa: PLC0415

        buf = io.StringIO()
        with patch.object(self.mod, "compare_pr", self._patch_compare(results)):
            with redirect_stdout(buf):
                rc = self.mod.main(["--prs", "1234", "--json"])
        self.assertEqual(rc, 0)
        import json as _json  # noqa: PLC0415
        payload = _json.loads(buf.getvalue())
        self.assertIn("all_pass", payload)
        self.assertTrue(payload["all_pass"])
        self.assertIn("results", payload)
        self.assertEqual(len(payload["results"]), 1)
        self.assertEqual(payload["results"][0]["pr"], "1234")

    def test_markdown_calls_format_results_exactly_once(self) -> None:
        # Round 28 fix: format_results was called twice (once for
        # all_pass, once for the table). Removing the cache and
        # re-calling would only fail this test.
        results = {
            "1234": {
                "pr": "1234", "total_substantive": 3, "caught": 3, "findings": [],
                "r1_issue_count": 1, "r2_issue_count": 1, "r1_only_count": 0,
                "r1_cursor_loaded": True, "r1_codex_loaded": True,
                "r2_cursor_loaded": True, "r2_codex_loaded": True,
            }
        }
        from unittest.mock import patch  # noqa: PLC0415

        original = self.mod.format_results
        call_count = [0]

        def counting_format_results(*args, **kwargs):
            call_count[0] += 1
            return original(*args, **kwargs)

        with patch.object(self.mod, "compare_pr", self._patch_compare(results)):
            with patch.object(
                self.mod, "format_results", side_effect=counting_format_results
            ):
                rc = self.mod.main(["--prs", "1234"])
        self.assertEqual(rc, 0)
        self.assertEqual(call_count[0], 1, "format_results was called more than once")


if __name__ == "__main__":
    unittest.main()
