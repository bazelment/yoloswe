#!/usr/bin/env python3
"""Run one reviewer and forward its stdout push stream.

Progress is the child's stdout (phase lines, heartbeats, a terminal
done/error event). This process does not write a log to tail. The hang
clock starts when the child spawns. Silence for two heartbeat intervals,
counted from spawn or from the last phase or heartbeat, is a hang: the
child is killed and an error envelope plus a terminal event are published
so the join can finish.

Heartbeats come from a timer in the bramble process that runs from
``reading_diff`` until the envelope is written. They do not depend on the
backend's event stream, so backend start and resume fallback are covered
and nothing is at risk of being mistaken for a hang. A hang therefore means
the bramble process itself stopped writing. A backend that is up but stalled
is bramble's ``--idle-timeout``'s job, and that path still ends in a terminal
event.

Several reviewers share one job stdout, so ``--backend`` is stamped onto
every forwarded JSON event, and each forwarded stderr line gets a
``[backend]`` prefix. Stdout lines stay valid JSON, and every line says
which reviewer wrote it.

Exit status is the child's, except a hang or a child that exits with no
terminal event exits 1. The envelope, not the status, is what triage reads.
"""

from __future__ import annotations

import argparse
import json
import os
import queue
import signal
import subprocess
import sys
import threading
import time
from pathlib import Path
from typing import Any, IO

SCRIPT_DIR = Path(__file__).resolve().parent
if str(SCRIPT_DIR) not in sys.path:
    sys.path.insert(0, str(SCRIPT_DIR))

from _common import atomic_write_json  # noqa: E402

# Fallback for bramble's codereview heartbeatInterval (progress.go). Every
# phase and heartbeat event carries interval_ms, and that value replaces
# this one. The hang threshold is always 2x the interval in force.
_DEFAULT_INTERVAL_MS = 20_000
# reading_diff arms the clock because bramble starts its heartbeat timer in
# the same step. Every phase after it is covered by heartbeats.
_LIVENESS_PHASES = frozenset({"reading_diff", "analyzing", "writing_envelope"})
_TERMINAL_EVENTS = frozenset({"done", "error"})


def _parse_event(line: str) -> dict[str, Any] | None:
    text = line.strip()
    if not text.startswith("{"):
        return None
    try:
        obj = json.loads(text)
    except json.JSONDecodeError:
        return None
    if isinstance(obj, dict):
        return obj
    return None


def _is_liveness(ev: dict[str, Any]) -> bool:
    if ev.get("event") == "heartbeat":
        return True
    return ev.get("event") == "phase" and ev.get("phase") in _LIVENESS_PHASES


def _interval_s(ev: dict[str, Any], current: float) -> float:
    ms = ev.get("interval_ms")
    if isinstance(ms, (int, float)) and ms > 0:
        return float(ms) / 1000.0
    return current


def _signal_group(proc: subprocess.Popen[str], sig: int) -> None:
    try:
        os.killpg(proc.pid, sig)
    except (ProcessLookupError, PermissionError):
        try:
            proc.send_signal(sig)
        except ProcessLookupError:
            return


def _envelope_ready(path: Path) -> bool:
    try:
        if not path.exists() or path.stat().st_size == 0:
            return False
        obj = json.loads(path.read_text())
    except (OSError, json.JSONDecodeError):
        return False
    return isinstance(obj, dict) and "schema_version" in obj and "status" in obj


def _write_error_envelope(path: Path, message: str, backend: str) -> None:
    if _envelope_ready(path):
        return
    atomic_write_json(
        path,
        {
            "schema_version": 2,
            "status": "error",
            "backend": backend,
            "model": "",
            "review_mode": "code",
            "error": message,
            "review": {"verdict": "", "issues": []},
            "duration_ms": 0,
            "input_tokens": 0,
            "output_tokens": 0,
        },
    )


def _format_wait(seconds: float) -> str:
    if seconds >= 10:
        return f"{seconds:.0f}s"
    if seconds >= 1:
        return f"{seconds:.1f}s"
    return f"{seconds * 1000:.0f}ms"


def _close_pipes(proc: subprocess.Popen[str]) -> None:
    for stream in (proc.stdout, proc.stderr):
        if stream is not None and not stream.closed:
            stream.close()


def _forward(line: str, backend: str) -> dict[str, Any] | None:
    """Write one child stdout line with ``backend`` stamped on JSON events.

    Returns the parsed event, or None for a non-JSON line (forwarded as-is).
    """
    ev = _parse_event(line)
    if ev is not None and backend:
        ev.setdefault("backend", backend)
        line = json.dumps(ev) + "\n"
    sys.stdout.write(line)
    sys.stdout.flush()
    return ev


def _emit_terminal(message: str, envelope: Path, backend: str) -> None:
    ev: dict[str, Any] = {
        "event": "error",
        "verdict": "",
        "envelope": str(envelope),
        "status": "error",
        "issues": 0,
        "error": message,
    }
    if backend:
        ev["backend"] = backend
    sys.stdout.write(json.dumps(ev) + "\n")
    sys.stdout.flush()


def _drain(
    stream: IO[str] | None,
    dest: IO[str] | None,
    out: queue.Queue[str | None],
    prefix: str = "",
) -> None:
    if stream is None:
        if dest is None:
            out.put(None)
        return
    for line in stream:
        if dest is None:
            out.put(line)
        else:
            dest.write(prefix + line)
            dest.flush()
    if dest is None:
        out.put(None)


def _kill_and_collect(
    proc: subprocess.Popen[str],
    lines: queue.Queue[str | None],
    *,
    grace_s: float,
    backend: str,
) -> bool:
    """SIGTERM, forward any terminal event the child emits, then SIGKILL.

    Returns whether a terminal event was forwarded.
    """
    _signal_group(proc, signal.SIGTERM)
    saw = False
    deadline = time.monotonic() + grace_s
    while time.monotonic() < deadline:
        try:
            line = lines.get(timeout=0.05)
        except queue.Empty:
            if proc.poll() is not None:
                break
            continue
        if line is None:
            break
        ev = _forward(line, backend)
        if ev and ev.get("event") in _TERMINAL_EVENTS:
            saw = True
    if proc.poll() is None:
        _signal_group(proc, signal.SIGKILL)
        try:
            proc.wait(timeout=grace_s)
        except subprocess.TimeoutExpired:
            pass
    return saw


def _wait_after_terminal(proc: subprocess.Popen[str], grace_s: float) -> int:
    try:
        return proc.wait(timeout=grace_s)
    except subprocess.TimeoutExpired:
        _signal_group(proc, signal.SIGTERM)
        try:
            proc.wait(timeout=grace_s)
            return 1
        except subprocess.TimeoutExpired:
            _signal_group(proc, signal.SIGKILL)
            try:
                proc.wait(timeout=grace_s)
            except subprocess.TimeoutExpired:
                pass
            return 1


def supervise(
    cmd: list[str],
    *,
    envelope: Path,
    interval_ms: int = _DEFAULT_INTERVAL_MS,
    backend: str = "",
    sigterm_grace_s: float = 3.0,
    terminal_grace_s: float = 3.0,
) -> int:
    """Run ``cmd``. Forward stdout. Hang-kill after 2× the heartbeat interval."""
    if not cmd:
        raise ValueError("review command is empty")
    proc = subprocess.Popen(
        cmd,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True,
        start_new_session=True,
    )
    lines: queue.Queue[str | None] = queue.Queue()
    threading.Thread(
        target=_drain, args=(proc.stdout, None, lines), daemon=True
    ).start()
    err_thread = threading.Thread(
        target=_drain,
        args=(proc.stderr, sys.stderr, lines, f"[{backend}] " if backend else ""),
        daemon=True,
    )
    err_thread.start()

    # The clock is armed from spawn and never unarmed, so no wait below is
    # unbounded. Before the first event the threshold comes from interval_ms
    # (the fallback). Pre-reading_diff setup is local and takes well under
    # one interval.
    hang_after = 2 * (interval_ms / 1000.0)
    deadline = time.monotonic() + hang_after
    saw_terminal = False

    while True:
        try:
            line = lines.get(timeout=max(0.0, deadline - time.monotonic()))
        except queue.Empty:
            message = f"hang: no heartbeat for {_format_wait(hang_after)} (2x the liveness interval)"
            if not _kill_and_collect(proc, lines, grace_s=sigterm_grace_s, backend=backend):
                _write_error_envelope(envelope, message, backend)
                _emit_terminal(message, envelope, backend)
            err_thread.join(timeout=1)
            _close_pipes(proc)
            return 1
        if line is None:
            break
        ev = _forward(line, backend)
        if ev is None:
            continue
        if ev.get("event") in _TERMINAL_EVENTS:
            saw_terminal = True
            code = _wait_after_terminal(proc, terminal_grace_s)
            err_thread.join(timeout=1)
            _close_pipes(proc)
            return code
        if not _is_liveness(ev):
            continue
        hang_after = 2 * _interval_s(ev, hang_after / 2)
        deadline = time.monotonic() + hang_after

    # stdout closed without a terminal event. No more events can arrive, but
    # the child may still be alive, so the wait for its exit is bounded by the
    # same hang deadline as the loop above.
    remaining = max(0.0, deadline - time.monotonic())
    try:
        code = proc.wait(timeout=remaining)
    except subprocess.TimeoutExpired:
        message = f"hang: stdout closed without a terminal event and the process did not exit within {_format_wait(remaining)}"
        _kill_and_collect(proc, lines, grace_s=sigterm_grace_s, backend=backend)
        _write_error_envelope(envelope, message, backend)
        _emit_terminal(message, envelope, backend)
        err_thread.join(timeout=1)
        _close_pipes(proc)
        return 1
    err_thread.join(timeout=1)
    _close_pipes(proc)
    if not saw_terminal:
        message = "review exited without a terminal event"
        _write_error_envelope(envelope, message, backend)
        _emit_terminal(message, envelope, backend)
        return code if code not in (0, None) else 1
    return code if code is not None else 1


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--envelope", required=True, type=Path)
    parser.add_argument("--backend", default="", help="Backend name stamped on a synthesized error envelope")
    parser.add_argument(
        "--interval-ms",
        type=int,
        default=_DEFAULT_INTERVAL_MS,
        help="Hang threshold is 2x this until a heartbeat reports its own interval_ms",
    )
    parser.add_argument("cmd", nargs=argparse.REMAINDER)
    args = parser.parse_args(argv)
    cmd = list(args.cmd)
    if cmd and cmd[0] == "--":
        cmd = cmd[1:]
    if not cmd:
        parser.error("command required after --")
    return supervise(
        cmd,
        envelope=args.envelope,
        interval_ms=args.interval_ms,
        backend=args.backend,
    )


if __name__ == "__main__":
    raise SystemExit(main())
