#!/usr/bin/env python3
"""Local companion inbox. Executes only explicitly approved, bounded argv jobs.

Use one SQLite database per coordinator, on a local disk. Remote execution is
an operator-approved SSH argv job, not a distributed SQLite filesystem.
"""
import argparse
from contextlib import contextmanager
from datetime import datetime
import json
import os
from pathlib import Path
import sqlite3
import subprocess
import sys
import time
import uuid


class FleetError(Exception):
    def __init__(self, code, message):
        self.code, self.message = code, message


def require(condition, code, message):
    if not condition:
        raise FleetError(code, message)


@contextmanager
def transaction(conn):
    conn.execute("BEGIN IMMEDIATE")
    try:
        yield
        conn.commit()
    except BaseException:
        conn.rollback()
        raise


def connect(path):
    path = Path(path).absolute()
    require(not path.is_symlink(), "unsafe_path", "The inbox must not be a symlink")
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    require(not path.parent.is_symlink(), "unsafe_path", "The state directory must not be a symlink")
    require(path.parent.stat().st_uid == os.getuid(), "unsafe_path", "The state directory must belong to this user")
    # Fail rather than changing permissions on an operator's existing directory.
    require(path.parent.stat().st_mode & 0o077 == 0, "unsafe_path", "Use a private state directory with mode 0700")
    os.umask(0o077)
    conn = sqlite3.connect(path, timeout=10, isolation_level=None)
    os.chmod(path, 0o600)
    conn.row_factory = sqlite3.Row
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA synchronous=FULL")
    conn.executescript("""
        CREATE TABLE IF NOT EXISTS workers (
          id TEXT PRIMARY KEY, capacity INTEGER NOT NULL, builds INTEGER NOT NULL,
          enabled INTEGER NOT NULL, seen REAL NOT NULL);
        CREATE TABLE IF NOT EXISTS tasks (
          id TEXT PRIMARY KEY, title TEXT NOT NULL, cwd TEXT NOT NULL,
          argv TEXT NOT NULL, build INTEGER NOT NULL, state TEXT NOT NULL,
          approved INTEGER NOT NULL DEFAULT 0, worker TEXT,
          archived INTEGER NOT NULL DEFAULT 0, snoozed REAL NOT NULL DEFAULT 0,
          created REAL NOT NULL, exit_code INTEGER, log TEXT);
        CREATE TABLE IF NOT EXISTS messages (
          id TEXT PRIMARY KEY, task TEXT NOT NULL, text TEXT NOT NULL,
          created REAL NOT NULL, delivered REAL);
    """)
    return conn, path


def task_row(conn, ident):
    row = conn.execute("SELECT * FROM tasks WHERE id=?", (ident,)).fetchone()
    require(row is not None, "not_found", "Unknown task")
    return row


def task_dict(row):
    data = dict(row)
    data["argv"] = json.loads(data["argv"])
    for name in ("build", "approved", "archived"):
        data[name] = bool(data[name])
    return data


def isolated_worktree(path):
    path = Path(path).resolve()
    result = subprocess.run(["git", "-C", str(path), "rev-parse", "--show-toplevel"], capture_output=True, text=True)
    require(result.returncode == 0, "worktree_required", "Task cwd must be an existing linked Git worktree")
    top = Path(result.stdout.strip()).resolve()
    require(path == top and (top / ".git").is_file(), "worktree_required", "Use the root of a linked Git worktree, not the primary checkout")
    return str(top)


def available(conn, worker, build, now):
    require(worker["enabled"] and worker["seen"] >= now - 120, "unavailable", "Worker disabled or heartbeat older than 120 seconds")
    counts = conn.execute("SELECT COUNT(*),COALESCE(SUM(build),0) FROM tasks WHERE worker=? AND state='working'", (worker["id"],)).fetchone()
    require(counts[0] < worker["capacity"] and (not build or counts[1] < worker["builds"]), "capacity", "Worker task or build capacity is exhausted")
    return counts[0] / worker["capacity"]


def claim(conn, ident, worker_id):
    with transaction(conn):
        task = task_row(conn, ident)
        require(task["approved"] and task["state"] == "queued" and not task["archived"] and task["snoozed"] <= time.time(), "not_approved", "Task is not an approved, awake queued job")
        worker = conn.execute("SELECT * FROM workers WHERE id=?", (worker_id,)).fetchone()
        require(worker is not None, "not_found", "Unknown worker")
        available(conn, worker, task["build"], time.time())
        isolated_worktree(task["cwd"])
        require(conn.execute("SELECT 1 FROM tasks WHERE cwd=? AND state='working'", (task["cwd"],)).fetchone() is None, "worktree_busy", "Another task owns this worktree")
        conn.execute("UPDATE tasks SET state='working',worker=? WHERE id=?", (worker_id, ident))
        return task_dict(task_row(conn, ident))


def state(conn, ident, new):
    task = task_row(conn, ident)
    require(new in ("needs_input", "review_ready", "failed", "settled"), "invalid_input", "Invalid result state")
    require(not (new == "review_ready" and task["state"] != "working"), "invalid_state", "Only completed working tasks can become review_ready")
    require(not (new == "settled" and task["state"] == "working"), "invalid_state", "Complete the task and review before settling")
    conn.execute("UPDATE tasks SET state=?,approved=0 WHERE id=?", (new, ident))
    return task_dict(task_row(conn, ident))


def run_jobs(conn, dbpath, args):
    require(1 <= args.max_tasks <= 32, "invalid_input", "max-tasks must be between 1 and 32")
    worker = conn.execute("SELECT * FROM workers WHERE id=?", (args.worker,)).fetchone()
    require(worker is not None and worker["enabled"], "unavailable", "Register and enable this worker first")
    results = []
    for _ in range(args.max_tasks):
        # Runner presence supplies a heartbeat. Separate heartbeat calls can keep
        # a worker available while its long job is running.
        with transaction(conn):
            conn.execute("UPDATE workers SET seen=? WHERE id=?", (time.time(), args.worker))
        candidates = conn.execute("SELECT id FROM tasks WHERE state='queued' AND approved=1 AND archived=0 AND snoozed<=? ORDER BY created,id", (time.time(),)).fetchall()
        task = None
        for candidate in candidates:
            try:
                task = claim(conn, candidate["id"], args.worker)
                break
            except FleetError as err:
                if err.code not in ("capacity", "worktree_busy", "not_approved"):
                    raise
        if task is None:
            break
        logdir = dbpath.parent / "logs"
        logdir.mkdir(mode=0o700, exist_ok=True)
        logfile = logdir / (task["id"] + ".log")
        # Neither prompts nor subprocess output are written to shared stdout.
        # Append retains earlier attempts for operator recovery.
        with logfile.open("ab") as output:
            with transaction(conn):
                conn.execute("UPDATE tasks SET log=? WHERE id=?", (str(logfile), task["id"]))
            try:
                result = subprocess.run(task["argv"], cwd=task["cwd"], stdin=subprocess.DEVNULL, stdout=output, stderr=subprocess.STDOUT)
                exit_code = result.returncode
            except OSError:
                exit_code = 127
        with transaction(conn):
            state(conn, task["id"], "review_ready" if exit_code == 0 else "failed")
            conn.execute("UPDATE tasks SET exit_code=? WHERE id=?", (exit_code, task["id"]))
        results.append({"id": task["id"], "exit_code": exit_code, "log": str(logfile)})
    return {"executed": results}


def dispatch(conn, dbpath, args):
    if args.command == "run":
        return run_jobs(conn, dbpath, args)
    if args.command == "claim":
        return claim(conn, args.id, args.worker)
    with transaction(conn):
        now = time.time()
        if args.command == "worker":
            require(1 <= args.capacity <= 32 and 0 <= args.build_capacity <= args.capacity, "invalid_input", "Capacity must be 1..32 and build-capacity 0..capacity")
            conn.execute("INSERT INTO workers VALUES (?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET capacity=excluded.capacity,builds=excluded.builds,enabled=excluded.enabled,seen=excluded.seen", (args.id, args.capacity, args.build_capacity, not args.disabled, now))
            return dict(conn.execute("SELECT * FROM workers WHERE id=?", (args.id,)).fetchone())
        if args.command == "heartbeat":
            require(conn.execute("UPDATE workers SET seen=? WHERE id=?", (now, args.id)).rowcount == 1, "not_found", "Unknown worker")
            return {"heartbeat": args.id}
        if args.command == "pick":
            choices = []
            for worker in conn.execute("SELECT * FROM workers ORDER BY id"):
                try:
                    choices.append((available(conn, worker, args.build, now), worker["id"], dict(worker)))
                except FleetError:
                    pass
            require(choices, "no_worker", "No fresh enabled worker has capacity")
            return min(choices)[2]
        if args.command == "add":
            argv = json.loads(args.argv)
            require(isinstance(argv, list) and argv and all(isinstance(item, str) and item and "\x00" not in item for item in argv), "invalid_input", "argv must be a nonempty JSON array of strings")
            require(args.title.strip(), "invalid_input", "Task title is required")
            ident = str(uuid.uuid4())
            conn.execute("INSERT INTO tasks (id,title,cwd,argv,build,state,created) VALUES (?,?,?,?,?,'needs_input',?)", (ident, args.title, isolated_worktree(args.cwd), json.dumps(argv), args.build, now))
            return task_dict(task_row(conn, ident))
        if args.command == "list":
            rows = conn.execute("SELECT * FROM tasks ORDER BY created,id").fetchall()
            return [task_dict(row) for row in rows if (args.all or (not row["archived"] and row["snoozed"] <= now)) and args.search.casefold() in row["title"].casefold()]
        task = task_row(conn, args.id)
        if args.command == "approve":
            require(task["state"] in ("needs_input", "failed", "queued"), "invalid_state", "Only pending or failed tasks can be approved")
            require(not task["archived"], "invalid_state", "Unarchive before approval")
            conn.execute("UPDATE tasks SET approved=1,state='queued',exit_code=NULL WHERE id=?", (args.id,))
            return task_dict(task_row(conn, args.id))
        if args.command == "state":
            return state(conn, args.id, args.state)
        if args.command in ("archive", "unarchive", "snooze"):
            require(task["state"] != "working", "invalid_state", "Do not hide or snooze a running task")
            if args.command == "snooze":
                when = datetime.fromisoformat(args.until.replace("Z", "+00:00"))
                require(when.tzinfo is not None, "invalid_input", "Snooze needs an explicit timezone")
                conn.execute("UPDATE tasks SET snoozed=? WHERE id=?", (when.timestamp(), args.id))
            else:
                conn.execute("UPDATE tasks SET archived=? WHERE id=?", (args.command == "archive", args.id))
            return task_dict(task_row(conn, args.id))
        if args.command == "message":
            require(args.text.strip(), "invalid_input", "Message is empty")
            ident = str(uuid.uuid4())
            conn.execute("INSERT INTO messages VALUES (?,?,?,?,NULL)", (ident, args.id, args.text, now))
            return {"id": ident, "state": "pending"}
        if args.command == "messages":
            return [dict(row) for row in conn.execute("SELECT * FROM messages WHERE task=? ORDER BY created,id", (args.id,))]
        if args.command == "boundary":
            require(task["state"] != "working", "unsafe_boundary", "A running exec job has no verified safe boundary; wait for turn completion or use native app-server steering")
            rows = [dict(row) for row in conn.execute("SELECT * FROM messages WHERE task=? AND delivered IS NULL ORDER BY created,id", (args.id,))]
            conn.execute("UPDATE messages SET delivered=? WHERE task=? AND delivered IS NULL", (now, args.id))
            return {"task": args.id, "messages": rows}
    raise FleetError("invalid_input", "Unknown command")


def parser():
    p = argparse.ArgumentParser(description=__doc__)
    p.add_argument("--db", required=True, help="SQLite inbox in a private 0700 directory")
    sub = p.add_subparsers(dest="command", required=True)
    worker = sub.add_parser("worker", help="Register or update worker capacity and heartbeat")
    worker.add_argument("id")
    worker.add_argument("--capacity", type=int, required=True)
    worker.add_argument("--build-capacity", type=int, required=True)
    worker.add_argument("--disabled", action="store_true")
    sub.add_parser("heartbeat").add_argument("id")
    sub.add_parser("pick").add_argument("--build", action="store_true")
    add = sub.add_parser("add")
    add.add_argument("--title", required=True)
    add.add_argument("--cwd", required=True)
    add.add_argument("--argv", required=True)
    add.add_argument("--build", action="store_true")
    listing = sub.add_parser("list")
    listing.add_argument("--all", action="store_true")
    listing.add_argument("--search", default="")
    for name in ("approve", "archive", "unarchive", "boundary", "messages"):
        sub.add_parser(name).add_argument("id")
    claimant = sub.add_parser("claim")
    claimant.add_argument("id")
    claimant.add_argument("worker")
    status = sub.add_parser("state")
    status.add_argument("id")
    status.add_argument("state", choices=["needs_input", "review_ready", "failed", "settled"])
    snooze = sub.add_parser("snooze")
    snooze.add_argument("id")
    snooze.add_argument("until")
    message = sub.add_parser("message")
    message.add_argument("id")
    message.add_argument("text")
    runner = sub.add_parser("run")
    runner.add_argument("worker")
    runner.add_argument("--max-tasks", type=int, required=True)
    return p


def main():
    conn = None
    try:
        args = parser().parse_args()
        conn, path = connect(args.db)
        print(json.dumps(dispatch(conn, path, args), ensure_ascii=False))
        return 0
    except (FleetError, ValueError, OSError, sqlite3.Error) as err:
        code = err.code if isinstance(err, FleetError) else "invalid_input"
        message = err.message if isinstance(err, FleetError) else "Invalid input or local storage/process failure"
        print(json.dumps({"error": {"code": code, "message": message}}), file=sys.stderr)
        return 1
    finally:
        if conn is not None:
            conn.close()


if __name__ == "__main__":
    sys.exit(main())
