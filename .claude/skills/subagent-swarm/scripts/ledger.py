#!/usr/bin/env python3
"""Durable lane ledger for subagent-swarm runs.

A LANE is one unit of work moving through an ordered list of PHASES. A phase is
one type of session that does the work. The lane's `status` is where it is in its
life; its `phase` is which phase-session is current. Those are different axes and
are stored separately.

State lives in <run>/state.json; <run>/ledger.md is re-rendered after every write
so the markdown is always current without anyone hand-editing tables.

    ledger.py init  <run> --goal G --phases "implement:opus,cleanup:gpt-5.6-luna"
                          [--base B --target B]
    ledger.py add   <run> --id ID --title T --branch B [--priority p0|p1|p2]
                          [--depends-on "a,b"] [--brief ...]
    ledger.py set   <run> --id ID [--status S] [--phase P] [--session S]
                          [--priority p0|p1|p2] [--worktree P] [--window-id W]
                          [--merge-sha SHA] [--note N]
    ledger.py advance <run> --id ID  # -> next phase, or status=done when exhausted
    ledger.py show  <run>            # print ledger.md
    ledger.py ready <run>            # dependency-ready ids, highest priority first
    ledger.py inflight <run>         # count of lanes with status running
    ledger.py lanes <run> [--status running] [--phase a,b] [--need-worktree]
                          [--config KEY]

`--session` records the session id for the lane's CURRENT phase.
"""

import argparse
import fcntl
import json
import os
import sys
import tempfile
import time

STATUSES = ["planned", "running", "done", "blocked", "failed"]
PRIORITIES = ["p0", "p1", "p2"]
CHECKS = ["pending", "passing", "failing", "unknown"]
PRIORITY_RANK = {p: i for i, p in enumerate(PRIORITIES)}
MARK = {"planned": "·", "running": "▶", "done": "✓", "blocked": "⏸", "failed": "✗"}


def paths(run):
    return os.path.join(run, "state.json"), os.path.join(run, "ledger.md")


def lock_path(run):
    # A separate inode from state.json, created on demand and NEVER deleted --
    # unlinking a lock file is itself a race (two processes can hold locks on two
    # different inodes with the same name and both believe they are exclusive).
    return os.path.join(run, "state.json.lock")


def acquire(run, exclusive, timeout=10.0):
    """Hold a flock across the WHOLE read-modify-write, not just the write.

    Every subcommand here is read-modify-write of the entire file, so a lock taken
    only around the write still lets two processes interleave between load and save
    and silently lose an update. Writers take LOCK_EX up front rather than upgrading
    from shared -- the upgrade window is exactly where lost updates hide.

    Readers take LOCK_SH: watch_lanes.sh calls `lanes` inside its polling loop, so
    exclusive reads would block the writer every INTERVAL seconds for no reason.

    Returns the held file object; the caller keeps it alive until after the replace.
    """
    os.makedirs(run, exist_ok=True)
    f = open(lock_path(run), "a+")
    mode = fcntl.LOCK_EX if exclusive else fcntl.LOCK_SH
    deadline = time.monotonic() + timeout
    while True:
        try:
            fcntl.flock(f.fileno(), mode | fcntl.LOCK_NB)
            return f
        except OSError:
            if time.monotonic() >= deadline:
                f.close()
                # Fail loudly. A tick that cannot get the lock must abort, never
                # proceed on a read it knows may be stale.
                sys.exit(f"ledger: could not lock {lock_path(run)} after {timeout:g}s "
                         f"-- another writer is holding it")
            time.sleep(0.05)


def write_atomic(path, text):
    """Write via a temp file in the SAME directory, then os.replace().

    os.replace is atomic on POSIX, so a concurrent reader sees either the old file
    or the new one -- never the truncated middle that open(path, "w") exposes.
    """
    d = os.path.dirname(path) or "."
    fd, tmp = tempfile.mkstemp(dir=d, prefix=".{}.".format(os.path.basename(path)))
    try:
        with os.fdopen(fd, "w") as f:
            f.write(text)
            f.flush()
            os.fsync(f.fileno())
        os.replace(tmp, path)
    except BaseException:
        try:
            os.unlink(tmp)
        except OSError:
            pass
        raise


def load(run):
    state_path, _ = paths(run)
    if not os.path.exists(state_path):
        sys.exit(f"no ledger at {run} — run `ledger.py init` first")
    with open(state_path) as f:
        state = json.load(f)
    for task in state.get("tasks", []):
        task.setdefault("priority", "p2")
        # Every field added after a run started is absent-by-default and backfilled.
        # Never test presence as a signal: a lane's contents would otherwise depend on
        # when it was created, and swarm-queen reads the same file.
        for key, default in (("pr", 0), ("pr_head", ""), ("approval_sha", ""),
                             ("checks", "unknown"), ("fork_sha", ""),
                             ("phase_start_sha", ""), ("round", 0),
                             ("last_verified_at", "")):
            task.setdefault(key, default)
    return state


def save(run, state):
    state_path, md_path = paths(run)
    os.makedirs(run, exist_ok=True)
    write_atomic(state_path, json.dumps(state, indent=2) + "\n")
    # ledger.md is rendered from the same snapshot and written the same way, so
    # `ledger.py show` can never read a half-rendered table.
    write_atomic(md_path, render(state, run))
    return md_path


def phase_names(state):
    return [p["name"] for p in state["config"]["phases"]]


def parse_phases(spec):
    """"implement:opus,cleanup:gpt-5.6-luna" -> [{name, model}, ...]"""
    phases = []
    for item in spec.split(","):
        item = item.strip()
        if not item:
            continue
        name, _, model = item.partition(":")
        name = name.strip()
        if not name:
            sys.exit(f"phase spec `{item}` has no name")
        if name in [p["name"] for p in phases]:
            sys.exit(f"duplicate phase name `{name}`")
        phases.append({"name": name, "model": model.strip()})
    if not phases:
        sys.exit("--phases must name at least one phase")
    return phases


def render(state, run):
    cfg = state["config"]
    tasks = state["tasks"]
    names = phase_names(state)
    counts = {s: sum(1 for t in tasks if t["status"] == s) for s in STATUSES}
    tally = " · ".join(f"{s} {counts[s]}" for s in STATUSES if counts[s])

    out = [f"# subagent-swarm — {cfg['goal']}", ""]
    out.append(f"- **run**: `{run}`")
    out.append(f"- **merging into**: `{cfg['target']}`  (base `{cfg['base']}`)")
    out.append("- **phases**: " + " → ".join(
        f"{p['name']} `{p['model']}`" if p["model"] else p["name"] for p in cfg["phases"]))
    out.append(f"- **status**: {tally or 'no lanes yet'}")
    out.append("")
    out.append("| | priority | lane | status | phase | branch | "
               + " | ".join(names) + " | merge |")
    out.append("|---|---|---|---|---|---|" + "---|" * (len(names) + 1))
    for t in tasks:
        cells = " | ".join(code(t["sessions"].get(n, "")) for n in names)
        out.append("| {m} | {priority} | **{id}**<br>{title} | {status} | {phase} | `{branch}` | "
                   "{cells} | {merge} |".format(
                       m=MARK.get(t["status"], "?"), id=t["id"], title=t["title"],
                       priority=t.get("priority", "p2"),
                       status=t["status"], phase=t["phase"] or "—", branch=t["branch"],
                       cells=cells, merge=code(t["merge_sha"])))
    out.append("")

    for t in tasks:
        detail = []
        if t["depends_on"]:
            detail.append(f"- depends on: {', '.join('`%s`' % d for d in t['depends_on'])}")
        if t["worktree"]:
            detail.append(f"- worktree: `{t['worktree']}`")
        if t["brief"]:
            detail.append(f"- brief: {t['brief']}")
        for n in t["notes"]:
            detail.append(f"- note: {n}")
        if detail:
            out.append(f"## {t['id']}")
            out.extend(detail)
            out.append("")
    return "\n".join(out)


def code(v):
    return f"`{v}`" if v else "—"


def doctor(state, run, sessions_path=""):
    """Report where the ledger disagrees with reality. Pure read; changes nothing.

    Every finding here is a drift shape observed in a real run: lanes left `running`
    over merged work, `done` lanes still holding a worktree, and live sessions the
    ledger never recorded. The tick is supposed to catch these by hand every 20
    minutes, which is exactly the kind of step that decays under load.
    """
    findings = []
    live = {}
    if sessions_path:
        try:
            with open(sessions_path) as f:
                payload = json.load(f)
            # list-sessions returns {"sessions": [...]}, not a bare list.
            rows = payload.get("sessions", payload) if isinstance(payload, dict) else payload
            for row in rows or []:
                if row.get("id"):
                    live[row["id"]] = row
        except (OSError, ValueError) as exc:
            findings.append(f"sessions file unreadable ({exc}) -- session checks SKIPPED, "
                            f"not passed")

    recorded = set()
    for t in state["tasks"]:
        tid, status = t["id"], t.get("status", "")
        for sid in t.get("sessions", {}).values():
            if sid:
                recorded.add(sid)

        wt = t.get("worktree") or ""
        if status == "done" and wt and os.path.isdir(wt):
            findings.append(f"{tid}: status=done but worktree still exists ({wt})")
        if status == "running" and wt and not os.path.isdir(wt):
            findings.append(f"{tid}: status=running but worktree is gone ({wt})")
        if status == "running" and t.get("merge_sha"):
            findings.append(f"{tid}: status=running but merge_sha is set "
                            f"({t['merge_sha']}) -- merged work hiding as in-flight")
        if status in ("running", "done") and not wt:
            findings.append(f"{tid}: status={status} with no worktree recorded -- "
                            f"invisible to the watcher and to snapshot_at_risk")
        if status == "running" and t.get("phase") and not t["sessions"].get(t["phase"]):
            findings.append(f"{tid}: phase={t['phase']} has no session id recorded")

        # Absence is never evidence of approval: an unknown head or approval sha is
        # treated as stale, exactly as swarm-queen's ApprovalStale() does.
        if t.get("pr"):
            head, appr = t.get("pr_head", ""), t.get("approval_sha", "")
            if not head or not appr:
                findings.append(f"{tid}: PR #{t['pr']} approval unverifiable "
                                f"(pr_head={head or '?'} approval_sha={appr or '?'})")
            elif head != appr:
                findings.append(f"{tid}: PR #{t['pr']} approval is STALE "
                                f"(approved {appr}, head {head}) -- would merge "
                                f"unapproved bytes")

    # Only sessions sitting on THIS run's worktrees are ours to account for; the box
    # runs unrelated sessions and flagging those is noise that trains you to ignore
    # the report. Match on worktree_name -- list-sessions carries no worktree_path.
    ours = {os.path.basename((t.get("worktree") or "").rstrip("/"))
            for t in state["tasks"] if t.get("worktree")}
    ours.discard("")
    for sid, row in sorted(live.items()):
        if sid in recorded:
            continue
        name = row.get("worktree_name", "")
        if name not in ours:
            continue
        findings.append(f"live session on a run worktree but not in the ledger: {sid} "
                        f"(status={row.get('status', '?')} worktree={name}) -- "
                        f"the orchestrator has lost the handle on it")
        # A session with no tmux_target has no pane: it is gone, not merely idle.
        if not row.get("tmux_target"):
            findings.append(f"  ^ {sid} has no tmux_target -- window is gone, "
                            f"decide now rather than waiting out a stall timeout")

    if not findings:
        print(f"doctor: {len(state['tasks'])} lane(s), no drift detected")
        return 0
    for line in findings:
        print(f"DRIFT {line}")
    print(f"doctor: {len(findings)} finding(s) across {len(state['tasks'])} lane(s)")
    return 1


def find(state, task_id):
    for t in state["tasks"]:
        if t["id"] == task_id:
            return t
    sys.exit(f"no lane `{task_id}` in this run")


def main():
    ap = argparse.ArgumentParser()
    sub = ap.add_subparsers(dest="cmd", required=True)

    p = sub.add_parser("init")
    p.add_argument("run")
    p.add_argument("--goal", required=True)
    p.add_argument("--phases", required=True,
                   help='ordered, e.g. "implement:opus,cleanup:gpt-5.6-luna"')
    p.add_argument("--base", default="main")
    p.add_argument("--target", default="")

    p = sub.add_parser("add")
    p.add_argument("run")
    p.add_argument("--id", required=True)
    p.add_argument("--title", required=True)
    p.add_argument("--branch", required=True)
    p.add_argument("--priority", choices=PRIORITIES, default="p2")
    p.add_argument("--depends-on", default="")
    p.add_argument("--brief", default="")

    p = sub.add_parser("set")
    p.add_argument("run")
    p.add_argument("--id", required=True)
    p.add_argument("--status", choices=STATUSES)
    p.add_argument("--phase")
    p.add_argument("--session", help="session id for the lane's current phase")
    p.add_argument("--priority", choices=PRIORITIES)
    p.add_argument("--worktree")
    p.add_argument("--window-id")
    p.add_argument("--merge-sha")
    p.add_argument("--note")
    p.add_argument("--pr", type=int, help="PR number (0 = none)")
    p.add_argument("--pr-head", help="SHA the PR currently points at")
    p.add_argument("--approval-sha", help="SHA the approval is pinned to")
    p.add_argument("--checks", choices=CHECKS, help="CI rollup for the PR head")
    p.add_argument("--fork-sha")
    p.add_argument("--phase-start-sha", help="HEAD when the current phase began")
    p.add_argument("--round", type=int, help="rework round for the current phase")
    p.add_argument("--verified-at", help="RFC3339 timestamp of the last reconcile")

    p = sub.add_parser("advance")
    p.add_argument("run")
    p.add_argument("--id", required=True)

    p = sub.add_parser("lanes")
    p.add_argument("run")
    p.add_argument("--status", default="running",
                   help="comma-separated statuses to include (default: running)")
    p.add_argument("--phase", default="", help="comma-separated phase allowlist")
    p.add_argument("--need-worktree", action="store_true",
                   help="skip lanes with no recorded worktree (warns on stderr)")
    p.add_argument("--config", metavar="KEY", help="print one config value and exit")

    p = sub.add_parser("doctor")
    p.add_argument("run")
    p.add_argument("--sessions", default="",
                   help="path to `bramble list-sessions` JSON; omit to skip session checks")

    for name in ("show", "ready", "inflight"):
        sub.add_parser(name).add_argument("run")

    a = ap.parse_args()

    # Mutating subcommands take LOCK_EX up front and hold it across load..save;
    # read-only ones take LOCK_SH so the 20s watcher poll never blocks a writer.
    writers = {"init", "add", "set", "advance"}
    _lock = acquire(a.run, exclusive=a.cmd in writers)

    if a.cmd == "init":
        state = {"config": {"goal": a.goal, "phases": parse_phases(a.phases),
                            "base": a.base, "target": a.target or a.base}, "tasks": []}
        print(save(a.run, state))
        return

    state = load(a.run)

    if a.cmd == "add":
        if any(t["id"] == a.id for t in state["tasks"]):
            sys.exit(f"lane `{a.id}` already exists")
        state["tasks"].append({
            "id": a.id, "title": a.title, "branch": a.branch, "brief": a.brief,
            "depends_on": [d.strip() for d in a.depends_on.split(",") if d.strip()],
            "priority": a.priority, "status": "planned", "phase": "",
            "sessions": {}, "worktree": "",
            "window_id": "", "merge_sha": "", "notes": [],
            # PR state cached here so a tick never re-derives it from `gh` after a
            # compaction. approval_sha vs pr_head IS the staleness check: an approval
            # pinned to an older head is not an approval for what would merge.
            "pr": 0, "pr_head": "", "approval_sha": "", "checks": "unknown",
            # fork_sha/phase_start_sha pin what "this phase changed" means. Measuring a
            # phase against a MOVING target is how an empty branch passes a .done.
            "fork_sha": "", "phase_start_sha": "", "round": 0, "last_verified_at": "",
        })
        print(save(a.run, state))

    elif a.cmd == "set":
        t = find(state, a.id)
        if a.phase is not None and a.phase not in phase_names(state):
            sys.exit(f"`{a.phase}` is not a phase of this run "
                     f"({', '.join(phase_names(state))})")
        for field, value in (("status", a.status), ("phase", a.phase),
                             ("priority", a.priority),
                             ("worktree", a.worktree), ("merge_sha", a.merge_sha),
                             ("window_id", a.window_id),
                             ("pr", a.pr), ("pr_head", a.pr_head),
                             ("approval_sha", a.approval_sha), ("checks", a.checks),
                             ("fork_sha", a.fork_sha),
                             ("phase_start_sha", a.phase_start_sha),
                             ("round", a.round),
                             ("last_verified_at", a.verified_at)):
            if value is not None:
                t[field] = value
        if a.session is not None:
            if not t["phase"]:
                sys.exit(f"lane `{a.id}` has no current phase — set --phase first")
            t["sessions"][t["phase"]] = a.session
        if a.note:
            t["notes"].append(a.note)
        print(save(a.run, state))

    elif a.cmd == "advance":
        t = find(state, a.id)
        names = phase_names(state)
        if not t["phase"]:
            t["phase"], t["status"] = names[0], "running"
        else:
            i = names.index(t["phase"]) if t["phase"] in names else -1
            if i < 0:
                sys.exit(f"lane `{a.id}` is on unknown phase `{t['phase']}`")
            if i + 1 < len(names):
                t["phase"], t["status"] = names[i + 1], "running"
            else:
                t["status"] = "done"
        print(f"{a.id}: status={t['status']} phase={t['phase'] or '—'}", file=sys.stderr)
        print(save(a.run, state))

    elif a.cmd == "show":
        print(render(state, a.run))

    elif a.cmd == "ready":
        done = {t["id"] for t in state["tasks"] if t["status"] == "done"}
        tasks = sorted(state["tasks"],
                       key=lambda t: PRIORITY_RANK.get(t.get("priority", "p2"), 2))
        for t in tasks:
            if t["status"] == "planned" and all(d in done for d in t["depends_on"]):
                print(t["id"])

    elif a.cmd == "inflight":
        print(sum(1 for t in state["tasks"] if t["status"] == "running"))

    elif a.cmd == "doctor":
        sys.exit(doctor(state, a.run, a.sessions))

    elif a.cmd == "lanes":
        if a.config:
            print(state["config"].get(a.config, ""))
            return
        want_status = {s.strip() for s in a.status.split(",") if s.strip()}
        want_phase = {p.strip() for p in a.phase.split(",") if p.strip()}
        for t in state["tasks"]:
            if want_status and t.get("status") not in want_status:
                continue
            if want_phase and t.get("phase") not in want_phase:
                continue
            wt = t.get("worktree") or ""
            if a.need_worktree and not wt:
                print(f"warn: {t['id']} has no worktree recorded in the ledger",
                      file=sys.stderr)
                continue
            print("\t".join([t["id"], t.get("phase", ""), t.get("branch", ""), wt,
                             t.get("window_id", "")]))


if __name__ == "__main__":
    main()
