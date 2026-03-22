#!/usr/bin/env python3
"""Multi-turn delegator eval driver.

Spawns `bramble delegator` in interactive mode via a PTY, feeds questions from
a text file one at a time, and captures stdout (answers) and stderr (metadata)
separately.

Coordination uses --status-fd: the delegator writes "idle" to a pipe when
ready for input and "done" on exit. No regex parsing of terminal output.

Usage:
    python3 scripts/delegator-eval.py \
        --questions-file scripts/delegator-eval-questions.txt \
        --work-dir /path/to/repo \
        --log-dir /tmp/eval-logs \
        [--model sonnet] [--child-model gemini-3-flash-preview] \
        [--timeout 900]
"""
import argparse
import os
import pty
import select
import subprocess
import sys
import time


def load_questions(path: str) -> list[str]:
    questions = []
    with open(path) as f:
        for line in f:
            line = line.strip()
            if line and not line.startswith("#"):
                questions.append(line)
    return questions


def run_eval(args: argparse.Namespace) -> None:
    questions = load_questions(args.questions_file)
    if not questions:
        print("No questions found", file=sys.stderr)
        sys.exit(1)

    # Status pipe: delegator writes "idle\n" / "done\n" to the write end.
    status_r, status_w = os.pipe()

    # PTY for stdin so the CLI sees a terminal and enters interactive mode.
    master_fd, slave_fd = pty.openpty()

    cmd = [
        "bazel-bin/bramble/bramble_/bramble",
        "delegator",
        "--mode", "real",
        "--verbose",
        "--work-dir", args.work_dir,
        "--model", args.model,
        "--timeout", f"{args.timeout}s",
        "--status-fd", str(status_w),
    ]
    if args.child_model:
        cmd += ["--child-model", args.child_model]
    if args.log_dir:
        cmd += ["--log-dir", args.log_dir]

    proc = subprocess.Popen(
        cmd,
        stdin=slave_fd,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        pass_fds=(status_w,),
    )
    os.close(slave_fd)
    os.close(status_w)  # Parent doesn't write to status pipe.

    child_model = args.child_model or args.model
    print(
        f"Multi-turn Delegator Eval | Model: {args.model}"
        f" | Child: {child_model} | Questions: {len(questions)}",
        file=sys.stderr,
    )

    question_idx = 0
    all_questions_sent = False
    # After all questions are sent, the next idle means the delegator has
    # finished processing. The child-notification priority fix in
    # manager.go ensures child completions are drained before idle, so a
    # single post-questions idle is sufficient.
    post_questions_idle = False
    start = time.monotonic()
    deadline = start + args.timeout
    stdout_path = os.path.join(args.log_dir, "stdout.txt") if args.log_dir else None
    stderr_path = os.path.join(args.log_dir, "stderr.txt") if args.log_dir else None
    stdout_file = open(stdout_path, "w") if stdout_path else None
    stderr_file = open(stderr_path, "w") if stderr_path else None
    status_buf = b""

    try:
        while proc.poll() is None and time.monotonic() < deadline:
            remaining = min(deadline - time.monotonic(), 2.0)
            fds = [proc.stdout, proc.stderr, status_r]
            readable, _, _ = select.select(fds, [], [], remaining)

            for fd in readable:
                if fd == status_r:
                    data = os.read(status_r, 256)
                    if not data:
                        continue
                    status_buf += data
                    # Process complete lines from status pipe.
                    while b"\n" in status_buf:
                        line, status_buf = status_buf.split(b"\n", 1)
                        msg = line.decode().strip()
                        if msg in ("idle", "idle-children-active"):
                            children_active = msg == "idle-children-active"
                            if question_idx < len(questions):
                                q = questions[question_idx]
                                question_idx += 1
                                print(
                                    f"\n--- Q{question_idx}/{len(questions)}:"
                                    f" {q[:80]} ---",
                                    file=sys.stderr,
                                )
                                os.write(master_fd, (q + "\n").encode())
                                all_questions_sent = (
                                    question_idx >= len(questions)
                                )
                            elif all_questions_sent and not children_active:
                                # No more questions and no active children
                                # — the delegator has finished everything.
                                post_questions_idle = True
                                print(
                                    "\nAll questions answered,"
                                    " sending quit.",
                                    file=sys.stderr,
                                )
                                os.write(master_fd, b"quit\n")
                        elif msg == "done":
                            pass  # Process will exit shortly.
                else:
                    data = os.read(fd.fileno(), 8192)
                    if not data:
                        continue
                    text = data.decode("utf-8", errors="replace")
                    if fd is proc.stdout:
                        sys.stdout.write(text)
                        sys.stdout.flush()
                        if stdout_file:
                            stdout_file.write(text)
                            stdout_file.flush()
                    elif fd is proc.stderr:
                        sys.stderr.write(text)
                        sys.stderr.flush()
                        if stderr_file:
                            stderr_file.write(text)
                            stderr_file.flush()

        if proc.poll() is None:
            elapsed = time.monotonic() - start
            print(
                f"\nTimeout after {elapsed:.0f}s"
                f" ({question_idx}/{len(questions)} questions)",
                file=sys.stderr,
            )
            proc.terminate()
            proc.wait(timeout=5)
    finally:
        os.close(master_fd)
        os.close(status_r)

    elapsed = time.monotonic() - start
    summary = (
        f"\n=== EVAL COMPLETE ==="
        f"\nQuestions: {question_idx}/{len(questions)}"
        f"\nElapsed: {elapsed:.0f}s"
        f"\nCost: (see Total: line above)\n"
    )
    print(summary, file=sys.stderr)
    if stdout_file:
        stdout_file.close()
    if stderr_file:
        stderr_file.write(summary)
        stderr_file.close()


def main():
    parser = argparse.ArgumentParser(description="Multi-turn delegator eval")
    parser.add_argument(
        "--questions-file", required=True, help="Path to questions file"
    )
    parser.add_argument(
        "--work-dir", required=True, help="Working directory for the eval"
    )
    parser.add_argument(
        "--model", default="sonnet", help="Delegator model (default: sonnet)"
    )
    parser.add_argument(
        "--child-model", default="", help="Child session model"
    )
    parser.add_argument(
        "--log-dir", default="", help="Directory for logs and recordings"
    )
    parser.add_argument(
        "--timeout",
        type=int,
        default=900,
        help="Timeout in seconds (default: 900 = 15min)",
    )
    args = parser.parse_args()
    run_eval(args)


if __name__ == "__main__":
    main()
