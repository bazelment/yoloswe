#!/usr/bin/env python3
"""Atomic local ledger for a Conductor swarm run; never stores credentials."""

import argparse
import fcntl
import json
import os
import sys
import tempfile
from contextlib import contextmanager


STATUSES = {"planned", "running", "done", "blocked", "failed"}
PRIORITIES = {"p0": 0, "p1": 1, "p2": 2}


@contextmanager
def locked(run):
    os.makedirs(run, exist_ok=True)
    with open(os.path.join(run, "lanes.lock"), "a+") as lock:
        fcntl.flock(lock, fcntl.LOCK_EX)
        yield


def path(run):
    return os.path.join(run, "lanes.json")


def load(run):
    with open(path(run), encoding="utf-8") as file:
        return json.load(file)


def save(run, state):
    fd, temporary = tempfile.mkstemp(prefix=".lanes-", dir=run)
    try:
        with os.fdopen(fd, "w", encoding="utf-8") as file:
            json.dump(state, file, indent=2)
            file.write("\n")
            file.flush()
            os.fsync(file.fileno())
        os.replace(temporary, path(run))
    finally:
        if os.path.exists(temporary):
            os.unlink(temporary)


def lane(state, lane_id):
    try:
        return state["lanes"][lane_id]
    except KeyError:
        raise ValueError(f"unknown lane: {lane_id}") from None


def run(args):
    with locked(args.run):
        if args.command == "init":
            if os.path.exists(path(args.run)):
                raise ValueError("run already initialized")
            phases = [phase.strip() for phase in args.phases.split(",")]
            if not all(phases) or len(set(phases)) != len(phases):
                raise ValueError("phases must be nonempty and unique")
            save(args.run, {"goal": args.goal, "proof": args.proof, "phases": phases,
                            "concurrency": args.concurrency, "lanes": {}})
            return

        state = load(args.run)
        if args.command == "add":
            if args.id in state["lanes"]:
                raise ValueError(f"duplicate lane: {args.id}")
            dependencies = [part.strip() for part in args.depends_on.split(",") if part.strip()]
            if args.id in dependencies:
                raise ValueError("lane cannot depend on itself")
            for dependency in dependencies:
                lane(state, dependency)
            state["lanes"][args.id] = {
                "title": args.title, "project_id": args.project_id,
                "source_branch": args.branch, "workspace_branch": "",
                "priority": args.priority,
                "depends_on": dependencies, "phase": state["phases"][0],
                "status": "planned", "workspace_id": "", "deep_link": "",
                "sessions": {}, "cursors": {}, "observed_working": {},
                "artifact_url": "", "verified_sha": "", "reviewed_sha": "",
                "verification": "", "initial_message_ids": {}, "notes": [],
            }
            save(args.run, state)
            return
        if args.command == "set":
            item = lane(state, args.id)
            if args.phase and args.phase not in state["phases"]:
                raise ValueError(f"unknown phase: {args.phase}")
            if args.phase:
                item["phase"] = args.phase
            if args.status:
                item["status"] = args.status
            for option, field in (("workspace_id", "workspace_id"),
                                  ("workspace_branch", "workspace_branch"),
                                  ("deep_link", "deep_link"),
                                  ("artifact_url", "artifact_url"),
                                  ("verified_sha", "verified_sha"),
                                  ("reviewed_sha", "reviewed_sha"),
                                  ("verification", "verification")):
                value = getattr(args, option)
                if value is not None:
                    item[field] = value
            current = item["phase"]
            if args.session_id is not None:
                sessions = item["sessions"].setdefault(current, [])
                if args.session_id not in sessions:
                    sessions.append(args.session_id)
            sessions = item["sessions"].get(current, [])
            active_session = sessions[-1] if sessions else ""
            if (args.cursor is not None or args.observed_working or
                    args.initial_message_id is not None) and not active_session:
                raise ValueError("record a session before updating its message state")
            if args.cursor is not None:
                item["cursors"][active_session] = args.cursor
            if args.observed_working:
                item["observed_working"][active_session] = True
            if args.initial_message_id is not None:
                item.setdefault("initial_message_ids", {})[active_session] = args.initial_message_id
            if args.note:
                item["notes"].append(args.note)
            save(args.run, state)
            return
        if args.command == "ready":
            ready = []
            for lane_id, item in state["lanes"].items():
                if item["status"] == "planned" and all(
                    state["lanes"][dependency]["status"] == "done"
                    for dependency in item["depends_on"]
                ):
                    ready.append((PRIORITIES[item["priority"]], lane_id))
            for _, lane_id in sorted(ready):
                print(lane_id)
            return
        if args.command == "show":
            print(json.dumps(state, indent=2))
            return


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    init = commands.add_parser("init")
    init.add_argument("run")
    init.add_argument("--goal", required=True)
    init.add_argument("--proof", required=True)
    init.add_argument("--phases", required=True)
    init.add_argument("--concurrency", type=int, required=True)
    add = commands.add_parser("add")
    add.add_argument("run")
    add.add_argument("--id", required=True)
    add.add_argument("--title", required=True)
    add.add_argument("--project-id", required=True)
    add.add_argument("--branch", required=True)
    add.add_argument("--priority", choices=PRIORITIES, default="p2")
    add.add_argument("--depends-on", default="")
    update = commands.add_parser("set")
    update.add_argument("run")
    update.add_argument("--id", required=True)
    update.add_argument("--phase")
    update.add_argument("--status", choices=STATUSES)
    for option in ("workspace-id", "workspace-branch", "session-id", "deep-link",
                   "cursor", "artifact-url", "verified-sha", "reviewed-sha",
                   "verification", "initial-message-id", "note"):
        update.add_argument(f"--{option}")
    update.add_argument("--observed-working", action="store_true")
    for name in ("ready", "show"):
        command = commands.add_parser(name)
        command.add_argument("run")
    args = parser.parse_args()
    try:
        if args.command == "init" and args.concurrency < 1:
            raise ValueError("concurrency must be positive")
        run(args)
    except (OSError, ValueError, json.JSONDecodeError) as error:
        parser.exit(1, f"{error}\n")


if __name__ == "__main__":
    main()
