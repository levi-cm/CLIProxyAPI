#!/usr/bin/env python3
"""Exercise the build limiter with a harmless Go-command probe."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


class GoLimitedTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory(prefix="cliproxy-cpu-limit-test-")
        self.addCleanup(self.temp.cleanup)
        self.probe = Path(self.temp.name) / "go-probe"
        self.probe.write_text(
            "#!" + sys.executable + "\n"
            "import json, os, sys\n"
            "print(json.dumps({'workers': os.environ['GOMAXPROCS'], "
            "'flags': os.environ['GOFLAGS'], 'args': sys.argv[1:], "
            "'omp': os.environ.get('OMP_NUM_THREADS'), "
            "'cpus': sorted(os.sched_getaffinity(0)) if hasattr(os, 'sched_getaffinity') else None}))\n"
        )
        self.probe.chmod(0o755)
        self.wrapper = Path(__file__).resolve().with_name("go-limited.sh")
        self.env = os.environ.copy()
        for name in ("OMP_NUM_THREADS", "OMP_THREAD_LIMIT"):
            self.env.pop(name, None)
        self.env.update(GO_BINARY=str(self.probe), GOFLAGS="-trimpath")

    def call(self, args, overrides=None, prefix=()):
        env = dict(self.env, **(overrides or {}))
        return subprocess.run(
            [*prefix, str(self.wrapper), *args], env=env,
            capture_output=True, text=True, check=False,
        )

    @unittest.skipUnless(shutil.which("nproc"), "nproc is unavailable")
    def test_openmp_does_not_change_cpu_budget(self):
        available = int(subprocess.check_output(["nproc"], env=self.env, text=True))
        expected = max(1, available // 2)
        for settings in ({}, {"OMP_NUM_THREADS": "128"}, {"OMP_THREAD_LIMIT": "1"},
                         {"OMP_NUM_THREADS": "128", "OMP_THREAD_LIMIT": "128"}):
            with self.subTest(settings=settings):
                result = self.call(["build", "./cmd/server"], settings)
                self.assertEqual(result.returncode, 0, result.stderr)
                probe = json.loads(result.stdout)
                self.assertEqual(int(probe["workers"]), expected)
                self.assertEqual(probe["omp"], settings.get("OMP_NUM_THREADS"))
                self.assertEqual(probe["flags"], "-trimpath -p=1")
                if probe["cpus"] is not None and shutil.which("taskset"):
                    self.assertEqual(len(probe["cpus"]), expected)

    @unittest.skipUnless(hasattr(os, "sched_getaffinity") and shutil.which("taskset"),
                         "CPU affinity requires Linux taskset")
    def test_affinity_rounds_down_and_keeps_one_cpu_minimum(self):
        available = sorted(os.sched_getaffinity(0))
        for count in (1, min(3, len(available))):
            prefix = ("taskset", "-c", ",".join(map(str, available[:count])))
            result = self.call(["build", "./cmd/server"], prefix=prefix)
            self.assertEqual(result.returncode, 0, result.stderr)
            probe = json.loads(result.stdout)
            self.assertEqual(len(probe["cpus"]), max(1, count // 2))

    def test_explicit_parallelism_cannot_bypass_serial_packages(self):
        for args in (["build", "-p=8", "./cmd/server"],
                     ["test", "-p", "8", "./sdk/cliproxy/auth"]):
            result = self.call(args)
            self.assertEqual(result.returncode, 2)
            self.assertEqual(result.stdout, "")

    def test_explicit_serial_flag_is_accepted(self):
        for args in (["build", "-p=1", "./cmd/server"], ["build", "-p", "1", "./cmd/server"]):
            result = self.call(args)
            self.assertEqual(result.returncode, 0, result.stderr)
            self.assertEqual(json.loads(result.stdout)["args"], args)

    def test_test_binary_arguments_are_forwarded(self):
        args = ["test", "./example", "-args", "-p=8"]
        result = self.call(args)
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertEqual(json.loads(result.stdout)["args"], args)

    def test_server_runtime_commands_are_rejected(self):
        result = self.call(["run", "./cmd/server"])
        self.assertEqual(result.returncode, 2)
        self.assertEqual(result.stdout, "")


if __name__ == "__main__":
    unittest.main()
