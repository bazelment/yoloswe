"""Resume contract tests for /pr-polish.

These tests cover the seam where round N's bramble envelope ``session_id``
flows through ``prior_session_id`` into round N+1's
``--resume-session-id`` flag. The agent-cli-wrapper repo has its own
generic four-backend resume integration test
(``agent-cli-wrapper/integration/resume_test.go``) that validates the SDK
contract. That test passing tells you nothing about whether
``/pr-polish`` actually threads the captured session id through to
bramble — that's a separate seam, sitting on the other side of
``bramble_ops.format_monitor_command``.

The pure-Python tests here run on every invocation of the pr-polish unit
suite. The end-to-end test at the bottom is gated on a real ``bramble``
binary plus the codex CLI being present; it skips otherwise.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

HERE = Path(__file__).resolve().parent
PARENT = HERE.parent
for p in (str(PARENT), str(HERE)):
    if p not in sys.path:
        sys.path.insert(0, p)

import bramble_ops  # noqa: E402


class TestResumeContract(unittest.TestCase):
    """Verify the round-N envelope -> round-N+1 --resume-session-id wiring."""

    def _state_file_with_session(
        self,
        d: Path,
        *,
        backend: str = "codex",
        sid: str = "round-1-session-abc",
        shape: str = "session_ids",
    ) -> Path:
        """Write a state file matching one of the historical shapes pr-polish
        has supported.

        ``shape`` selects which on-disk layout to emit so we cover all three
        branches of ``prior_session_id``:

          - ``session_ids``: ``rounds[n].session_ids[backend]`` (current shape).
          - ``per_backend``: legacy ``rounds[n].<backend>_session_id`` field.
          - ``reviews_envelope``: ``rounds[n].reviews[backend]`` with the raw
            envelope inlined (used when the orchestrator persisted the entire
            envelope rather than just the id).
        """
        state: dict = {
            "pr_number": 9999,
            "branch": "feature/test",
            "current_round": 1,
            "rounds": [],
        }
        rnd: dict = {"n": 1, "head_after": "abc1234"}
        if shape == "session_ids":
            rnd["session_ids"] = {backend: sid}
        elif shape == "per_backend":
            rnd[f"{backend}_session_id"] = sid
        elif shape == "reviews_envelope":
            rnd["reviews"] = {backend: {"session_id": sid, "schema_version": 1, "status": "ok"}}
        else:
            raise ValueError(f"unknown shape {shape!r}")
        state["rounds"].append(rnd)

        path = d / "pr-polish-state.json"
        path.write_text(json.dumps(state))
        return path

    def test_round_two_threads_session_id_from_session_ids_field(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            state_file = self._state_file_with_session(Path(tmp), shape="session_ids")
            cmd = bramble_ops.format_monitor_command(
                "codex",
                "gpt-5.4-mini",
                2,
                "review the round-1 fixes",
                repo="kernel",
                pr=9999,
                work_dir="/tmp/worktree",
                state_file=str(state_file),
            )
        # The wired session id must reach the CLI as
        # `--resume-session-id 'round-1-session-abc'` (shlex-quoted).
        # shlex.quote leaves shell-safe identifiers bare. Match the bare form.
        self.assertIn("--resume-session-id round-1-session-abc", cmd)
        # Also assert the CLI flag we removed earlier didn't sneak back.
        self.assertNotIn(" --json ", " " + cmd + " ")

    def test_round_two_threads_session_id_from_legacy_per_backend_field(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            state_file = self._state_file_with_session(Path(tmp), shape="per_backend")
            cmd = bramble_ops.format_monitor_command(
                "codex",
                "gpt-5.4-mini",
                2,
                "review",
                repo="kernel",
                pr=9999,
                work_dir="/tmp/worktree",
                state_file=str(state_file),
            )
        # shlex.quote leaves shell-safe identifiers bare. Match the bare form.
        self.assertIn("--resume-session-id round-1-session-abc", cmd)

    def test_round_two_threads_session_id_from_inlined_reviews_envelope(self) -> None:
        # If the orchestrator persisted the entire round-1 envelope under
        # rounds[n].reviews[backend], `prior_session_id` should still pull
        # session_id out of it. This shape exists because some pr-polish
        # versions copied the envelope inline for post-loop audit access.
        with tempfile.TemporaryDirectory() as tmp:
            state_file = self._state_file_with_session(Path(tmp), shape="reviews_envelope")
            cmd = bramble_ops.format_monitor_command(
                "codex",
                "gpt-5.4-mini",
                2,
                "review",
                repo="kernel",
                pr=9999,
                work_dir="/tmp/worktree",
                state_file=str(state_file),
            )
        # shlex.quote leaves shell-safe identifiers bare. Match the bare form.
        self.assertIn("--resume-session-id round-1-session-abc", cmd)

    def test_round_one_omits_resume_flag_even_with_state(self) -> None:
        # Round 1 starts a fresh session — even if a state file exists from a
        # prior abandoned run, the orchestrator's round-1 invocation must NOT
        # carry --resume-session-id. format_monitor_command guards this with a
        # `round_ >= 2` check.
        with tempfile.TemporaryDirectory() as tmp:
            state_file = self._state_file_with_session(Path(tmp), shape="session_ids")
            cmd = bramble_ops.format_monitor_command(
                "codex",
                "gpt-5.4-mini",
                1,
                "first round",
                repo="kernel",
                pr=9999,
                work_dir="/tmp/worktree",
                state_file=str(state_file),
            )
        self.assertNotIn("--resume-session-id", cmd)

    def test_round_two_with_no_state_omits_resume_flag(self) -> None:
        cmd = bramble_ops.format_monitor_command(
            "codex",
            "gpt-5.4-mini",
            2,
            "review",
            repo="kernel",
            pr=9999,
            work_dir="/tmp/worktree",
            state_file=None,
        )
        self.assertNotIn("--resume-session-id", cmd)

    def test_round_two_with_state_file_for_different_backend_omits_resume(self) -> None:
        # If round 1 only ran codex but round 2 invokes cursor, there's no
        # cursor session id to resume. The flag must be omitted, not faked
        # with the codex id (which would silently send a cursor request to
        # the wrong session class).
        with tempfile.TemporaryDirectory() as tmp:
            state_file = self._state_file_with_session(Path(tmp), backend="codex")
            cmd = bramble_ops.format_monitor_command(
                "cursor",
                "composer-2",
                2,
                "review",
                repo="kernel",
                pr=9999,
                work_dir="/tmp/worktree",
                state_file=str(state_file),
            )
        self.assertNotIn("--resume-session-id", cmd)

    def test_round_two_resume_via_real_state_finalize_round(self) -> None:
        # Belt-and-braces: rather than fabricate ``rounds[n].session_ids``
        # by hand, drive a full append/finalize cycle via pr_ops with a
        # real envelope on disk and assert format_monitor_command picks
        # up the persisted id. A regression in _persist_round_findings
        # (e.g. dropping session_id capture) would let the hand-built
        # tests above keep passing while the live skill does cold starts.
        import pr_ops  # noqa: PLC0415 — import locally so unit tests don't pay the cost.

        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            envelope_dir = tmp_path / "r1"
            envelope_dir.mkdir()
            codex_env = envelope_dir / "codex-envelope.json"
            codex_env.write_text(
                json.dumps(
                    {
                        "schema_version": 1,
                        "status": "ok",
                        "backend": "codex",
                        "session_id": "real-codex-session-finalize-1",
                        "resume_status": "ok",
                        "review": {"verdict": "rejected", "issues": []},
                    }
                )
            )

            from unittest.mock import patch  # noqa: PLC0415

            fake_home = tmp_path / "home"
            fake_home.mkdir()
            with (
                patch.object(pr_ops, "repo_slug", return_value="kernel"),
                patch("_common.repo_slug", return_value="kernel"),
                patch.object(Path, "home", return_value=fake_home),
            ):
                pr_ops.state_append_round(9999, 1, "sha-before", verify_head=False)
                pr_ops.state_finalize_round(
                    9999,
                    1,
                    "sha-after",
                    [],
                    envelope_overrides={"codex": codex_env},
                )
                _, state_file = pr_ops.state_paths(9999)

                # Sanity: session id landed in the state file.
                state = json.loads(state_file.read_text())
                self.assertEqual(
                    state["rounds"][0]["session_ids"]["codex"],
                    "real-codex-session-finalize-1",
                )

                # The contract under test: format_monitor_command threads
                # the persisted id into round 2's launch string.
                cmd = bramble_ops.format_monitor_command(
                    "codex",
                    "gpt-5.4-mini",
                    2,
                    "review",
                    repo="kernel",
                    pr=9999,
                    work_dir=str(tmp_path),
                    state_file=str(state_file),
                )
            self.assertTrue(
                "real-codex-session-finalize-1" in cmd
                and "--resume-session-id" in cmd,
                f"persisted id not threaded into round 2 cmd:\n{cmd}",
            )


def _bramble_binary_with_resume() -> str | None:
    """Locate a bramble binary that actually understands --resume-session-id.

    Prefers $BRAMBLE_BIN over PATH lookup so a developer worktree's freshly
    built bramble takes precedence over an older copy in ~/bin. Probes the
    candidate's `code-review --help` output for the resume flag and returns
    None when neither candidate supports it — the end-to-end test is skipped
    in that case rather than silently testing against a stale binary that
    can't exercise the contract under test.
    """
    candidates: list[str] = []
    explicit = os.environ.get("BRAMBLE_BIN")
    if explicit and Path(explicit).is_file():
        candidates.append(explicit)
    on_path = shutil.which("bramble")
    if on_path and on_path not in candidates:
        candidates.append(on_path)
    for cand in candidates:
        try:
            help_out = subprocess.run(
                [cand, "code-review", "--help"],
                capture_output=True,
                text=True,
                timeout=10,
            )
        except (subprocess.SubprocessError, OSError):
            continue
        if "--resume-session-id" in (help_out.stdout + help_out.stderr):
            return cand
    return None


@unittest.skipUnless(
    _bramble_binary_with_resume() and shutil.which("codex"),
    "end-to-end resume test requires bramble (via $BRAMBLE_BIN or PATH) supporting "
    "--resume-session-id, plus the codex CLI",
)
class TestResumeEndToEnd(unittest.TestCase):
    """Run two real bramble code-review turns against the codex backend.

    Round 1 runs fresh and captures the envelope's ``session_id``. Round 2
    runs with ``--resume-session-id <captured>`` and asserts the resulting
    envelope reports ``resume_status: "ok"``. This is the same secret-recall
    pattern the agent-cli-wrapper integration test uses, but driven through
    the bramble CLI surface so it covers the pr-polish path end-to-end.

    Skipped unless the CLIs and credentials are available; the unit-level
    tests above are the always-on safety net.
    """

    def _run_review(
        self,
        *,
        work_dir: Path,
        envelope_path: Path,
        resume_session_id: str | None,
    ) -> dict:
        bramble = _bramble_binary_with_resume()
        assert bramble is not None  # gated by skip decorator
        cmd = [
            bramble,
            "code-review",
            "--backend",
            "codex",
            "--model",
            "gpt-5.4-mini",
            "--effort",
            "medium",
            "--skip-test-execution",
            "--verbose",
            "--timeout",
            "5m",
            "--envelope-file",
            str(envelope_path),
            "--goal",
            "Reply 'acknowledged' and remember the phrase ECHO-LOBSTER-91.",
        ]
        if resume_session_id:
            cmd += ["--resume-session-id", resume_session_id]
        env = os.environ.copy()
        env["WORK_DIR"] = str(work_dir)
        proc = subprocess.run(
            cmd,
            cwd=str(work_dir),
            env=env,
            capture_output=True,
            text=True,
            timeout=600,
        )
        if proc.returncode != 0:
            self.fail(
                "bramble code-review exited %d\nstdout:\n%s\nstderr:\n%s"
                % (proc.returncode, proc.stdout, proc.stderr)
            )
        return json.loads(envelope_path.read_text())

    def test_round_two_resumes_round_one_session(self) -> None:
        with tempfile.TemporaryDirectory() as tmp:
            tmp_path = Path(tmp)
            r1_envelope = tmp_path / "r1.json"
            r2_envelope = tmp_path / "r2.json"

            # Round 1 — fresh.
            env1 = self._run_review(
                work_dir=tmp_path, envelope_path=r1_envelope, resume_session_id=None
            )
            self.assertEqual(env1.get("status"), "ok")
            sid = env1.get("session_id")
            self.assertTrue(sid, "round 1 envelope must include session_id")

            # Round 2 — resumed. Build a state file the way pr-polish would
            # at end-of-round-1 and verify format_monitor_command would also
            # produce a resume flag — that's the contract this whole test is
            # protecting.
            state_file = tmp_path / "state.json"
            state_file.write_text(
                json.dumps(
                    {
                        "pr_number": 9999,
                        "branch": "feature/e2e",
                        "rounds": [{"n": 1, "session_ids": {"codex": sid}}],
                    }
                )
            )
            wired_cmd = bramble_ops.format_monitor_command(
                "codex",
                "gpt-5.4-mini",
                2,
                "review",
                repo="kernel",
                pr=9999,
                work_dir=str(tmp_path),
                state_file=str(state_file),
            )
            # shlex.quote may or may not wrap the id in quotes depending on
            # its characters; match the unquoted-or-quoted form.
            self.assertTrue(
                f"--resume-session-id {sid}" in wired_cmd
                or f"--resume-session-id '{sid}'" in wired_cmd,
                f"resume id {sid!r} not threaded into command:\n{wired_cmd}",
            )

            # Now run round 2 directly with the captured id and confirm the
            # CLI reports resume_status=ok in the envelope.
            env2 = self._run_review(
                work_dir=tmp_path, envelope_path=r2_envelope, resume_session_id=sid
            )
            self.assertEqual(env2.get("status"), "ok")
            self.assertEqual(
                env2.get("resume_status"),
                "ok",
                f"round 2 should report resume_status=ok; got envelope:\n{env2}",
            )


if __name__ == "__main__":
    unittest.main()
