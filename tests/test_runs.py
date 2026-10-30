"""Real runs: the platform's dispatch, end to end.

Each test starts actual runs with `run` (exactly what the tick does for a due
job) against a throwaway ledger, and waits for what they record. That needs
the platform's machinery: on macOS nothing beyond the binary; on Linux a
systemd user manager, so these skip where there is none (CI's Linux runners).

    python3 -m unittest discover tests
"""

import os
import shutil
import sqlite3
import subprocess
import sys
import tempfile
import time
import unittest
import uuid
from datetime import datetime, timedelta, timezone

from _binary import BIN


def can_dispatch():
    if sys.platform == "darwin":
        return True
    if sys.platform.startswith("linux") and shutil.which("systemd-run"):
        return subprocess.run(["systemctl", "--user", "show-environment"],
                              capture_output=True).returncode == 0
    return False


@unittest.skipUnless(can_dispatch(), "no way to start real runs here")
class Runs(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.db = os.path.join(self.tmp.name, "ledger.db")
        os.mkdir(os.path.join(self.tmp.name, "notify.d"))  # empty: never the real hooks
        self.env = {**os.environ, "CRONTINENTAL_DB": self.db,
                    "CRONTINENTAL_NOTIFY_DIR": os.path.join(self.tmp.name, "notify.d")}
        self.env.pop("CLAUDE_CODE_SESSION_ID", None)  # rm in teardown is unguarded
        # unit names are machine-wide on Linux: never collide with real jobs
        self.prefix = "t" + uuid.uuid4().hex[:8] + "-"
        self.names = []

    def tearDown(self):
        for n in self.names:
            subprocess.run([BIN, "rm", n], env=self.env, capture_output=True)
        self.tmp.cleanup()

    def cli(self, *args):
        p = subprocess.run([BIN, *args], env=self.env, capture_output=True, text=True)
        self.assertEqual(p.returncode, 0, p.stderr)
        return p.stdout

    def book(self, short, command, *flags):
        name = self.prefix + short
        self.names.append(name)
        self.cli("every", "1h", "--name", name, "--notify", "never", *flags, "--", command)
        return name

    def runs(self, name):
        with sqlite3.connect(self.db) as c:
            return c.execute("SELECT r.outcome, r.status, coalesce(r.detail, '') FROM run r"
                             " JOIN job j ON j.id=r.job_id WHERE j.name=? ORDER BY r.id",
                             (name,)).fetchall()

    def wait_for(self, name, count=1, timeout=20):
        deadline = time.time() + timeout
        while time.time() < deadline:
            done = [r for r in self.runs(name) if r[0] != "running"]
            if len(done) >= count:
                return done
            time.sleep(0.2)
        self.fail(f"{name}: no finished run after {timeout}s: {self.runs(name)}")

    def wait_running(self, name, timeout=10):
        deadline = time.time() + timeout
        while time.time() < deadline:
            if any(r[0] == "running" for r in self.runs(name)):
                return
            time.sleep(0.1)
        self.fail(f"{name}: never started")

    def test_done_fail_and_output(self):
        done = self.book("done", "echo instant-line; exit 99", "--until-done")
        fail = self.book("fail", "exit 3", "--forever")
        self.cli("run", done)
        self.cli("run", fail)
        self.assertEqual(self.wait_for(done)[0][:2], ("done", 99))
        self.assertEqual(self.wait_for(fail)[0][:2], ("fail", 3))
        # a line printed by a command that exits at once still reaches its log
        deadline = time.time() + 5
        while "instant-line" not in self.cli("log", done, "-n", "20") and time.time() < deadline:
            time.sleep(0.2)
        self.assertIn("instant-line", self.cli("log", done, "-n", "20"))

    def test_log_run_shows_that_run_alone(self):
        name = self.book("perrun", "echo line-$(date +%s%N); sleep 1", "--forever")
        self.cli("run", name)
        self.wait_for(name, 1)
        time.sleep(1.2)  # the ledger keeps whole seconds; runs further apart never share one
        self.cli("run", name)
        self.wait_for(name, 2)
        with sqlite3.connect(self.db) as c:
            ids = [r[0] for r in c.execute("SELECT r.id FROM run r JOIN job j ON j.id=r.job_id"
                                           " WHERE j.name=? ORDER BY r.id", (name,))]
        logs = []
        for run_id in ids:
            deadline = time.time() + 5
            out = ""
            while "line-" not in out and time.time() < deadline:
                out = self.cli("log", name, "--run", str(run_id))
                time.sleep(0.2)
            logs.append([l for l in out.splitlines() if "line-" in l])
        self.assertEqual([len(l) for l in logs], [1, 1], logs)
        self.assertNotEqual(logs[0][0].split("line-")[1], logs[1][0].split("line-")[1])

    def name(self, short):
        name = self.prefix + short
        self.names.append(name)
        return name

    def job(self, name):
        with sqlite3.connect(self.db) as c:
            return c.execute("SELECT next_run, finished_at FROM job WHERE name=?",
                             (name,)).fetchone()

    def test_a_one_off_due_now_runs_at_once_and_only_once(self):
        for verb, when in (("in", "0"), ("at", "now")):
            with self.subTest(verb=verb):
                name = self.name(verb)
                out = self.cli(verb, when, "--name", name, "--notify", "never", "--", "true")
                self.assertIn("started now", out)
                self.assertEqual(self.wait_for(name)[0][:2], ("ok", 0))
                next_run, finished = self.job(name)
                self.assertIsNone(next_run)
                self.assertIsNotNone(finished)
                # booked already fired: the tick finds nothing left to run
                self.cli("tick")
                time.sleep(1)
                self.assertEqual(len(self.runs(name)), 1)

    def test_a_repeat_with_now_runs_at_once_and_keeps_its_schedule(self):
        name = self.book("now", "true", "--forever", "--now")
        self.assertEqual(self.wait_for(name)[0][:2], ("ok", 0))
        next_run, finished = self.job(name)
        self.assertIsNone(finished)
        # the first scheduled run is still an hour out, not moved by the extra one
        due = datetime.fromisoformat(next_run.replace("Z", "+00:00"))
        self.assertGreater(due - datetime.now(timezone.utc), timedelta(minutes=55))

    def test_editing_a_one_off_to_now_runs_it(self):
        name = self.name("edit")
        self.cli("in", "1h", "--name", name, "--notify", "never", "--", "true")
        self.assertIn("started now", self.cli("edit", name, "--in", "0"))
        self.assertEqual(self.wait_for(name)[0][:2], ("ok", 0))
        self.assertIsNotNone(self.job(name)[1])

    def test_timeout_kills_the_whole_command(self):
        name = self.book("kill", "sleep 30 & sleep 30; wait", "--forever", "--timeout", "2s")
        self.cli("run", name)
        outcome, status, _ = self.wait_for(name, timeout=30)[0]
        self.assertEqual((outcome, status), ("killed", 143))

    def test_a_killed_run_tells_the_hooks(self):
        hooks = self.env["CRONTINENTAL_NOTIFY_DIR"]
        told = os.path.join(self.tmp.name, "told")
        hook = os.path.join(hooks, "rec")
        with open(hook, "w") as f:
            f.write(f'#!/bin/sh\necho "$CRONTINENTAL_EVENT $CRONTINENTAL_STATUS" > "{told}"\n')
        os.chmod(hook, 0o755)
        name = self.book("killhook", "sleep 30", "--forever", "--timeout", "2s", "--notify", "fail")
        self.cli("run", name)
        self.wait_for(name, timeout=30)
        deadline = time.time() + 10
        while not os.path.exists(told) and time.time() < deadline:
            time.sleep(0.2)
        with open(told) as f:
            self.assertEqual(f.read().strip(), "killed 143")

    def test_a_second_run_while_one_is_going_is_an_overlap(self):
        name = self.book("long", "sleep 20", "--forever")
        self.cli("run", name)
        self.wait_running(name)
        self.cli("run", name)
        outcomes = [r[0] for r in self.wait_for(name)]
        self.assertIn("overlap", outcomes)

    def test_rm_stops_a_run_in_progress(self):
        name = self.book("rm", "sleep 25 & sleep 25; wait", "--forever")
        self.cli("run", name)
        self.wait_running(name)
        start = time.time()
        self.cli("rm", name)
        self.names.remove(name)
        self.assertLess(time.time() - start, 15, "rm waited out the command instead of stopping it")
        self.assertNotIn(name, self.cli("names"))


if __name__ == "__main__":
    unittest.main()
