#!/usr/bin/env python3
"""Run one reviewer and forward its stdout push stream.

Progress is the child's stdout (phase lines, heartbeats, a terminal
done/error event). This process does not write a log to tail. After the
child has emitted ``analyzing`` or a heartbeat, silence for two heartbeat
intervals is a hang: the child is killed and an error envelope plus a
terminal event are published so the join can finish.

``reading_diff`` does not arm the hang clock. Backend start sits in that
phase and can outlast two intervals without being stuck.

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

# Matches reviewer.heartbeatInterval (20s) until a heartbeat event says
# otherwise. Hang threshold is always 2× the interval in force.
_DEFAULT_INTERVAL_MS = 20_000
_LIVENESS_PHASES = frozenset({"analyzing", "writing_envelope"})
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


def _emit_terminal(message: str, envelope: Path) -> None:
    line = json.dumps(
        {
            "event": "error",
            "verdict": "",
            "envelope": str(envelope),
            "status": "error",
            "issues": 0,
            "error": message,
        }
    )
    sys.stdout.write(line + "\n")
    sys.stdout.flush()


def _drain(stream: IO[str] | None, dest: IO[str] | None, out: queue.Queue[str | None]) -> None:
    if stream is None:
        if dest is None:
            out.put(None)
        return
    for line in stream:
        if dest is None:
            out.put(line)
        else:
            dest.write(line)
            dest.flush()
    if dest is None:
        out.put(None)


def _kill_and_collect(
    proc: subprocess.Popen[str],
    lines: queue.Queue[str | None],
    *,
    grace_s: float,
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
        sys.stdout.write(line)
        sys.stdout.flush()
        ev = _parse_event(line)
        if ev and ev.get("event") in _TERMINAL_EVENTS:
            saw = True
    if proc.poll() is None:
        _signal_group(proc, signal.SIGKILL)
        try:
            proc.wait(timeout=grace_s)
        except subprocess.TimeoutExpired:
            pass
    return saw


def supervise(
    cmd: list[str],
    *,
    envelope: Path,
    interval_ms: int = _DEFAULT_INTERVAL_MS,
    backend: str = "",
    sigterm_grace_s: float = 3.0,
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
        target=_drain, args=(proc.stderr, sys.stderr, lines), daemon=True
    )
    err_thread.start()

    hang_after = 2 * (interval_ms / 1000.0)
    deadline: float | None = None
    saw_terminal = False

    while True:
        timeout: float | None = None
        if deadline is not None:
            timeout = max(0.0, deadline - time.monotonic())
        try:
            line = lines.get(timeout=timeout)
        except queue.Empty:
            message = f"hang: no heartbeat for {_format_wait(hang_after)} (2x the liveness interval)"
            if not _kill_and_collect(proc, lines, grace_s=sigterm_grace_s):
                _write_error_envelope(envelope, message, backend)
                _emit_terminal(message, envelope)
            err_thread.join(timeout=1)
            _close_pipes(proc)
            return 1
        if line is None:
            break
        sys.stdout.write(line)
        sys.stdout.flush()
        ev = _parse_event(line)
        if ev is None:
            continue
        if ev.get("event") in _TERMINAL_EVENTS:
            saw_terminal = True
            deadline = None
            continue
        if not _is_liveness(ev):
            continue
        if ev.get("event") == "heartbeat":
            hang_after = 2 * _interval_s(ev, hang_after / 2)
        deadline = time.monotonic() + hang_after

    code = proc.wait()
    err_thread.join(timeout=1)
    _close_pipes(proc)
    if not saw_terminal:
        message = "review exited without a terminal event"
        _write_error_envelope(envelope, message, backend)
        _emit_terminal(message, envelope)
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
