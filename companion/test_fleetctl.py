"""Black-box tests for the approved backlog and durable capacity contracts."""
import json
from concurrent.futures import ThreadPoolExecutor
import os
from pathlib import Path
import sqlite3
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).with_name("fleetctl.py")


class FleetTests(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.addCleanup(self.tmp.cleanup)
        self.root = Path(self.tmp.name)
        self.db = self.root / "private" / "inbox.sqlite"
        self.main = self.root / "repo"
        self.main.mkdir()
        self.git("init", "-q", str(self.main))
        self.git("-C", str(self.main), "-c", "user.name=Test", "-c", "user.email=test@example.invalid", "commit", "--allow-empty", "-qm", "base")
        self.tree = self.root / "task"
        self.git("-C", str(self.main), "worktree", "add", "-qb", "task", str(self.tree))

    def git(self, *args):
        subprocess.run(["git", *args], check=True, capture_output=True)

    def call(self, *args, ok=True):
        result = subprocess.run([sys.executable, str(SCRIPT), "--db", str(self.db), *args], capture_output=True, text=True)
        if ok:
            self.assertEqual(result.returncode, 0, result.stderr)
            return json.loads(result.stdout)
        self.assertNotEqual(result.returncode, 0, result.stdout)
        return json.loads(result.stderr)

    def worker(self, name="local", capacity=1, builds=1):
        return self.call("worker", name, "--capacity", str(capacity), "--build-capacity", str(builds))

    def task(self, title="Implement routing", build=False, argv=None, cwd=None):
        args = ["add", "--title", title, "--cwd", str(cwd or self.tree), "--argv", json.dumps(argv or [sys.executable, "-c", "pass"])]
        if build:
            args.append("--build")
        return self.call(*args)["id"]

    def test_unapproved_tasks_never_execute(self):
        self.worker()
        ident = self.task(argv=[sys.executable, "-c", "raise SystemExit(17)"])
        result = self.call("run", "local", "--max-tasks", "2")
        self.assertEqual(result["executed"], [])
        self.assertEqual(self.call("list")[0]["state"], "needs_input")
        self.call("approve", ident)
        result = self.call("run", "local", "--max-tasks", "1")
        self.assertEqual(result["executed"][0]["exit_code"], 17)
        self.assertEqual(self.call("list")[0]["state"], "failed")

    def test_claim_serializes_capacity_and_preserves_restart_state(self):
        self.worker()
        first, second = self.task(), self.task()
        self.call("approve", first)
        self.call("approve", second)
        self.call("claim", first, "local")
        error = self.call("claim", second, "local", ok=False)
        self.assertEqual(error["error"]["code"], "capacity")
        self.assertEqual(self.call("list")[0]["state"], "working")
        self.call("state", first, "needs_input")
        self.call("claim", second, "local")
        self.assertEqual(self.call("list")[1]["state"], "working")

    def test_build_capacity_and_worktree_isolation_are_independent(self):
        self.worker(capacity=3)
        first, second = self.task(build=True), self.task(build=True)
        self.call("approve", first)
        self.call("approve", second)
        self.call("claim", first, "local")
        self.assertEqual(self.call("claim", second, "local", ok=False)["error"]["code"], "capacity")
        self.call("state", first, "review_ready")
        self.call("claim", second, "local")
        third = self.task()
        self.call("approve", third)
        self.assertEqual(self.call("claim", third, "local", ok=False)["error"]["code"], "worktree_busy")
        self.assertEqual(self.call("add", "--title", "unsafe", "--cwd", str(self.main), "--argv", '["true"]', ok=False)["error"]["code"], "worktree_required")

    def test_picker_excludes_stale_full_and_disabled_workers(self):
        self.worker("offline")
        self.worker("ready")
        with sqlite3.connect(self.db) as conn:
            conn.execute("UPDATE workers SET seen=0 WHERE id='offline'")
        self.assertEqual(self.call("pick")["id"], "ready")
        self.call("worker", "ready", "--capacity", "1", "--build-capacity", "1", "--disabled")
        self.assertEqual(self.call("pick", ok=False)["error"]["code"], "no_worker")

    def test_archive_snooze_search_and_safe_message_boundary(self):
        self.worker()
        ident = self.task(title="Repair Unicode café CI")
        self.call("message", ident, "Review expiry ownership")
        self.call("approve", ident)
        self.call("claim", ident, "local")
        self.assertEqual(self.call("boundary", ident, ok=False)["error"]["code"], "unsafe_boundary")
        self.call("state", ident, "needs_input")
        self.assertEqual(self.call("boundary", ident)["messages"][0]["text"], "Review expiry ownership")
        self.assertEqual(self.call("boundary", ident)["messages"], [])
        self.call("snooze", ident, "2100-01-01T00:00:00Z")
        self.assertEqual(self.call("list", "--search", "café"), [])
        self.assertEqual(len(self.call("list", "--search", "café", "--all")), 1)
        self.call("archive", ident)
        self.assertTrue(self.call("list", "--all")[0]["archived"])

    def test_runner_is_bounded_and_success_requires_review(self):
        self.worker()
        for _ in range(3):
            ident = self.task()
            self.call("approve", ident)
        result = self.call("run", "local", "--max-tasks", "2")
        self.assertEqual(len(result["executed"]), 2)
        self.assertEqual([t["state"] for t in self.call("list")], ["review_ready", "review_ready", "queued"])
        self.assertEqual(os.stat(self.db).st_mode & 0o777, 0o600)
        self.assertEqual(os.stat(self.db.parent).st_mode & 0o777, 0o700)
        self.assertEqual(self.call("run", "local", "--max-tasks", "0", ok=False)["error"]["code"], "invalid_input")

    def test_concurrent_claims_cannot_overbook_worker(self):
        self.worker()
        other = self.root / "other"
        self.git("-C", str(self.main), "worktree", "add", "-qb", "other", str(other))
        tasks = [self.task(), self.task(cwd=other)]
        for ident in tasks:
            self.call("approve", ident)

        def claim(ident):
            return subprocess.run([sys.executable, str(SCRIPT), "--db", str(self.db), "claim", ident, "local"], capture_output=True, text=True)

        with ThreadPoolExecutor(max_workers=2) as pool:
            results = list(pool.map(claim, tasks))
        self.assertEqual(sorted(result.returncode for result in results), [0, 1])
        failed = next(result for result in results if result.returncode)
        self.assertEqual(json.loads(failed.stderr)["error"]["code"], "capacity")
        self.assertEqual(sum(task["state"] == "working" for task in self.call("list")), 1)

    def test_invalid_commands_and_naive_snooze_do_not_mutate_task(self):
        ident = self.task()
        for argv in ('{}', '[]', '[1]', '["true", null]', 'not JSON'):
            self.assertEqual(self.call("add", "--title", "bad", "--cwd", str(self.tree), "--argv", argv, ok=False)["error"]["code"], "invalid_input")
        self.assertEqual(self.call("snooze", ident, "2030-10-04T12:00:00", ok=False)["error"]["code"], "invalid_input")
        self.assertEqual(len(self.call("list")), 1)
        self.assertEqual(self.call("list")[0]["snoozed"], 0)

    def test_working_job_cannot_be_hidden_reapproved_or_settled(self):
        self.worker()
        ident = self.task()
        self.call("approve", ident)
        self.call("claim", ident, "local")
        for args in (("archive", ident), ("approve", ident), ("snooze", ident, "2100-01-01T00:00:00Z"), ("state", ident, "settled")):
            self.assertEqual(self.call(*args, ok=False)["error"]["code"], "invalid_state")
        self.assertEqual(self.call("list")[0]["state"], "working")


if __name__ == "__main__":
    unittest.main()
