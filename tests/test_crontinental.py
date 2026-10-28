"""End-to-end tests for crontinental, run against a throwaway ledger.

Nothing here touches systemd: runs are driven through `exec` directly (what a
job's own unit would call), and `tick` is only exercised on paths that finish or
reap jobs rather than dispatching them. So the suite runs anywhere Python, Go and
`systemd-analyze` do.

    python3 -m unittest discover tests
"""

import json
import os
import shlex
import sqlite3
import subprocess
import tempfile
import time
import unittest
from datetime import datetime, timedelta, timezone
from pathlib import Path

from _binary import BIN

V1_SCHEMA = """
CREATE TABLE job (
  id INTEGER PRIMARY KEY, name TEXT NOT NULL UNIQUE, command TEXT NOT NULL,
  workdir TEXT, kind TEXT NOT NULL, spec TEXT, interval_sec INTEGER, next_run TEXT,
  catchup TEXT NOT NULL DEFAULT 'run', until TEXT, timeout_sec INTEGER NOT NULL,
  notify TEXT NOT NULL DEFAULT 'fail', enabled INTEGER NOT NULL DEFAULT 1,
  note TEXT, created_at TEXT NOT NULL, created_by TEXT);
CREATE TABLE run (
  id INTEGER PRIMARY KEY, job_id INTEGER NOT NULL REFERENCES job(id) ON DELETE CASCADE,
  unit TEXT, due_at TEXT, started_at TEXT, finished_at TEXT, status INTEGER,
  outcome TEXT NOT NULL, late_sec INTEGER, detail TEXT);
PRAGMA user_version=1;
"""


def iso(dt):
    return dt.replace(microsecond=0).isoformat()


def ago(**kw):
    return iso(datetime.now(timezone.utc) - timedelta(**kw))


class Ledger(unittest.TestCase):
    def setUp(self):
        self.tmp = tempfile.TemporaryDirectory()
        self.db = os.path.join(self.tmp.name, "ledger.db")
        # an empty hook directory of its own, so a failing test job never runs
        # the machine's real notify hooks
        self.hooks = Path(self.tmp.name) / "notify.d"
        self.hooks.mkdir()
        self.env = {**os.environ, "CRONTINENTAL_DB": self.db,
                    "CRONTINENTAL_NOTIFY_DIR": str(self.hooks)}
        # the suite may itself run inside a Claude Code session; booking as one
        # is opted into per test (the Guard tests)
        self.env.pop("CLAUDE_CODE_SESSION_ID", None)

    def tearDown(self):
        self.tmp.cleanup()

    def cli(self, *args, ok=True):
        p = subprocess.run([BIN, *args], env=self.env, capture_output=True, text=True)
        if ok:
            self.assertEqual(p.returncode, 0, p.stderr)
        return p

    def sql(self, q, *params):
        with sqlite3.connect(self.db) as c:
            return c.execute(q, params).fetchall()

    def job(self, name):
        rows = self.sql("SELECT enabled, next_run, finished_at, forever, until_done"
                        " FROM job WHERE name=?", name)
        return rows[0] if rows else None


class Booking(Ledger):
    def test_repeat_without_an_end_is_refused(self):
        p = self.cli("every", "30m", "--name", "x", "--", "true", ok=False)
        self.assertNotEqual(p.returncode, 0)
        self.assertIn("has to say how it ends", p.stderr)

    def test_forever_contradicts_an_end(self):
        p = self.cli("every", "30m", "--name", "x", "--forever", "--until", "2 hours",
                     "--", "true", ok=False)
        self.assertIn("contradicts", p.stderr)

    def test_one_off_takes_no_end_flags(self):
        p = self.cli("in", "5m", "--name", "x", "--forever", "--", "true", ok=False)
        self.assertIn("for repeats", p.stderr)

    def test_until_in_the_past_is_refused(self):
        p = self.cli("every", "30m", "--name", "x", "--until", "1 hour ago",
                     "--", "true", ok=False)
        self.assertIn("in the past", p.stderr)

    def test_a_one_off_in_the_past_is_still_refused(self):
        # "now" is allowed; anything before the moment of booking is not
        p = self.cli("at", "1 hour ago", "--name", "x", "--", "true", ok=False)
        self.assertIn("in the past", p.stderr)

    def test_now_is_only_for_repeats(self):
        p = self.cli("in", "2h", "--now", "--name", "x", "--", "true", ok=False)
        self.assertIn("in 0", p.stderr)
        self.assertIsNone(self.job("x"))

    def test_edit_takes_no_now(self):
        self.cli("every", "30m", "--name", "x", "--forever", "--", "true")
        p = self.cli("edit", "x", "--now", ok=False)
        self.assertEqual(p.returncode, 2)

    def test_each_end_is_accepted_and_recorded(self):
        self.cli("every", "30m", "--name", "a", "--until", "2 hours", "--", "true")
        self.cli("every", "30m", "--name", "b", "--until-done", "--", "true")
        self.cli("every", "30m", "--name", "c", "--forever", "--", "true")
        self.assertEqual(self.job("b")[4], 1)
        self.assertEqual(self.job("c")[3], 1)

    def test_calendar_specs_work_in_and_out_of_utc(self):
        # systemd words its answer differently when local time is UTC; a parser
        # that only read one wording reported every spec as "never elapses".
        for tz in ("UTC", "America/New_York", "Asia/Kolkata"):
            with self.subTest(tz=tz):
                self.env["TZ"] = tz
                name = "cal-" + tz.lower().replace("/", "-")
                self.cli("every", "Mon..Fri 07:30", "--name", name, "--forever", "--", "true")
                (nxt,), = self.sql("SELECT next_run FROM job WHERE name=?", name)
                gap = datetime.fromisoformat(nxt) - datetime.now(timezone.utc)
                self.assertTrue(timedelta(0) < gap <= timedelta(days=4), (tz, nxt))

    def test_daily_with_a_time_explains_both_grammars(self):
        p = self.cli("every", "daily 03:00", "--name", "x", "--forever", "--", "true", ok=False)
        self.assertIn("as a calendar", p.stderr)
        self.assertIn("as an interval", p.stderr)


class Stays(Ledger):
    def exec_(self, name):
        return subprocess.run([BIN, "exec", name], env=self.env, capture_output=True, text=True)

    def outcomes(self, name):
        return self.sql("SELECT r.outcome, r.status FROM run r JOIN job j ON j.id=r.job_id"
                        " WHERE j.name=? ORDER BY r.id", name)

    def test_exit_99_finishes_the_job_as_a_success(self):
        self.cli("every", "1m", "--name", "poll", "--until-done", "--notify", "never",
                 "--", "exit 99")
        p = self.exec_("poll")
        self.assertEqual(p.returncode, 0, "done must not make the unit fail")
        self.assertEqual(self.outcomes("poll"), [("done", 99)])
        enabled, next_run, finished_at, *_ = self.job("poll")
        self.assertEqual((enabled, next_run), (0, None))
        self.assertIsNotNone(finished_at)
        self.assertNotIn("poll", self.cli("list").stdout)
        self.assertIn("done", self.cli("list", "--all").stdout)

    def test_ordinary_exits_leave_the_job_scheduled(self):
        self.cli("every", "1h", "--name", "ok", "--forever", "--", "true")
        self.cli("every", "1h", "--name", "bad", "--forever", "--notify", "never", "--", "exit 3")
        self.assertEqual(self.exec_("ok").returncode, 0)
        self.assertEqual(self.exec_("bad").returncode, 3)
        self.assertEqual(self.outcomes("ok"), [("ok", 0)])
        self.assertEqual(self.outcomes("bad"), [("fail", 3)])
        self.assertEqual(self.job("ok")[0], 1)
        self.assertEqual(self.job("bad")[0], 1)


class Checkout(Ledger):
    def test_deadline_finishes_a_due_job_without_running_it(self):
        self.cli("every", "30m", "--name", "w", "--until", "2 hours", "--", "true")
        self.sql("UPDATE job SET next_run=?, until=? WHERE name='w'", ago(minutes=5), ago(minutes=1))
        self.cli("tick")
        enabled, _, finished_at, *_ = self.job("w")
        self.assertEqual(enabled, 0)
        self.assertIsNotNone(finished_at)
        self.assertEqual(self.sql("SELECT count(*) FROM run"), [(0,)])

    def test_cleaners_come_three_days_after_checkout(self):
        self.cli("every", "1h", "--name", "old", "--forever", "--", "true")
        self.cli("every", "1h", "--name", "recent", "--forever", "--", "true")
        self.sql("UPDATE job SET enabled=0, next_run=NULL, finished_at=? WHERE name='old'", ago(days=4))
        self.sql("UPDATE job SET enabled=0, next_run=NULL, finished_at=? WHERE name='recent'", ago(days=1))
        self.cli("tick")
        self.assertIsNone(self.job("old"))
        self.assertIsNotNone(self.job("recent"))

    def test_a_guest_still_in_the_room_holds_off_the_cleaners(self):
        self.cli("every", "1h", "--name", "busy", "--forever", "--", "true")
        self.sql("UPDATE job SET enabled=0, next_run=NULL, finished_at=? WHERE name='busy'", ago(days=4))
        self.sql("INSERT INTO run (job_id, started_at, outcome)"
                 " SELECT id, ?, 'running' FROM job WHERE name='busy'", ago(hours=2))
        self.cli("tick")
        self.assertIsNotNone(self.job("busy"))
        # ...unless that "running" row is older than retention: an exec that died too hard to say so
        self.sql("UPDATE run SET started_at=?", ago(days=5))
        self.cli("tick")
        self.assertIsNone(self.job("busy"))

    def test_runs_go_with_their_job(self):
        self.cli("every", "1h", "--name", "gone", "--forever", "--", "true")
        subprocess.run([BIN, "exec", "gone"], env=self.env, capture_output=True)
        self.sql("UPDATE job SET enabled=0, next_run=NULL, finished_at=? WHERE name='gone'", ago(days=4))
        self.cli("tick")
        self.assertEqual(self.sql("SELECT count(*) FROM run"), [(0,)])

    def test_enable_takes_a_finished_repeat_off_the_clock(self):
        self.cli("every", "1h", "--name", "back", "--until-done", "--", "true")
        self.sql("UPDATE job SET enabled=0, next_run=NULL, finished_at=? WHERE name='back'", ago(days=1))
        self.cli("enable", "back")
        enabled, next_run, finished_at, *_ = self.job("back")
        self.assertEqual(enabled, 1)
        self.assertIsNotNone(next_run)
        self.assertIsNone(finished_at)

    def test_disabled_is_not_finished(self):
        self.cli("every", "1h", "--name", "held", "--forever", "--", "true")
        self.cli("disable", "held")
        self.sql("UPDATE job SET created_at=? WHERE name='held'", ago(days=30))
        self.cli("tick")
        self.cli("prune")
        self.assertIsNotNone(self.job("held"))


class Edit(Ledger):
    def row(self, name, *cols):
        return self.sql(f"SELECT {', '.join(cols)} FROM job WHERE name=?", name)[0]

    def test_naming_an_end_drops_forever(self):
        self.cli("every", "1h", "--name", "w", "--forever", "--", "true")
        self.cli("edit", "w", "--until-done")
        self.assertEqual(self.row("w", "forever", "until_done"), (0, 1))

    def test_forever_drops_both_ends(self):
        self.cli("every", "1h", "--name", "w", "--until-done", "--until", "2 hours", "--", "true")
        self.cli("edit", "w", "--forever")
        self.assertEqual(self.row("w", "forever", "until_done", "until"), (1, 0, None))

    def test_a_repeat_cannot_be_left_without_an_end(self):
        self.cli("every", "1h", "--name", "w", "--until-done", "--", "true")
        p = self.cli("edit", "w", "--no-until-done", ok=False)
        self.assertIn("without an end", p.stderr)
        self.assertEqual(self.row("w", "until_done"), (1,), "a refused edit changes nothing")

    def test_dropping_one_of_two_ends_keeps_the_other(self):
        self.cli("every", "1h", "--name", "w", "--until-done", "--until", "2 hours", "--", "true")
        self.cli("edit", "w", "--no-until")
        self.assertEqual(self.row("w", "until", "until_done"), (None, 1))

    def test_new_schedule_recomputes_the_next_run(self):
        self.cli("every", "1h", "--name", "w", "--forever", "--", "true")
        before = self.row("w", "next_run")[0]
        self.cli("edit", "w", "--every", "2h")
        spec, interval, after = self.row("w", "spec", "interval_sec", "next_run")
        self.assertEqual((spec, interval), ("2h", 7200))
        self.assertGreater(after, before)
        self.cli("edit", "w", "--every", "Mon..Fri 07:30")
        self.assertEqual(self.row("w", "spec", "interval_sec"), ("Mon..Fri 07:30", None))

    def test_schedule_flags_must_match_the_kind(self):
        self.cli("every", "1h", "--name", "r", "--forever", "--", "true")
        self.cli("in", "1h", "--name", "o", "--", "true")
        self.assertIn("one-off", self.cli("edit", "o", "--every", "2h", ok=False).stderr)
        self.assertIn("is a repeat", self.cli("edit", "r", "--in", "2h", ok=False).stderr)
        self.assertIn("ends when it fires", self.cli("edit", "o", "--forever", ok=False).stderr)

    def test_command_and_settings_change_and_history_stays(self):
        self.cli("every", "1h", "--name", "w", "--forever", "--notify", "never", "--", "true")
        subprocess.run([BIN, "exec", "w"], env=self.env, capture_output=True)
        created = self.row("w", "created_at", "created_by")
        self.cli("edit", "w", "--timeout", "5m", "--notify", "always", "--note", "why",
                 "--", "echo", "new command")
        self.assertEqual(self.row("w", "command", "timeout_sec", "notify", "note"),
                         ("echo 'new command'", 300, "always", "why"))
        self.assertEqual(self.row("w", "created_at", "created_by"), created)
        self.assertEqual(self.sql("SELECT count(*) FROM run"), [(1,)])

    def test_edit_without_changes_or_job_is_refused(self):
        self.cli("every", "1h", "--name", "w", "--forever", "--", "true")
        self.assertIn("nothing to change", self.cli("edit", "w", ok=False).stderr)
        self.assertIn("no such job", self.cli("edit", "nope", "--forever", ok=False).stderr)


class Guard(Ledger):
    """Inside a Claude Code session, rm/disable/edit keep off jobs that session
    didn't book unless --force; outside one, nothing changes."""

    def as_session(self, sid):
        self.env["CLAUDE_CODE_SESSION_ID"] = sid

    def book_as_person(self, name):
        self.env.pop("CLAUDE_CODE_SESSION_ID", None)
        self.env["USER"] = "pat"
        self.cli("every", "1d", "--name", name, "--forever", "--", "true")

    def test_another_sessions_job_is_refused_without_force(self):
        self.as_session("aaaaaaaa-1111")
        self.cli("every", "1d", "--name", "w", "--forever", "--", "true")
        self.as_session("bbbbbbbb-2222")
        for args in (["rm", "w"], ["disable", "w"], ["edit", "w", "--until", "2 days"]):
            p = self.cli(*args, ok=False)
            self.assertEqual(p.returncode, 1, args)
            self.assertIn("booked by claude:aaaaaaaa", p.stderr)
            self.assertIn("--force", p.stderr)
        self.assertEqual(self.job("w")[0], 1, "refused verbs change nothing")
        self.assertEqual(self.job("w")[3], 1)

    def test_a_persons_job_is_refused_too(self):
        self.book_as_person("w")
        self.as_session("bbbbbbbb-2222")
        self.assertIn("booked by pat", self.cli("rm", "w", ok=False).stderr)
        self.assertIsNotNone(self.job("w"))

    def test_force_gets_past_it(self):
        self.book_as_person("w")
        self.as_session("bbbbbbbb-2222")
        self.cli("disable", "w", "--force")
        self.cli("edit", "w", "--until", "2 days", "--force")
        self.cli("rm", "w", "--force")
        self.assertIsNone(self.job("w"))

    def test_a_session_handles_its_own_jobs_freely(self):
        self.as_session("aaaaaaaa-1111")
        self.cli("every", "1d", "--name", "w", "--forever", "--", "true")
        self.cli("edit", "w", "--until-done")
        self.cli("disable", "w")
        self.cli("rm", "w")

    def test_outside_a_session_nothing_is_guarded(self):
        self.as_session("aaaaaaaa-1111")
        self.cli("every", "1d", "--name", "w", "--forever", "--", "true")
        self.env.pop("CLAUDE_CODE_SESSION_ID")
        self.cli("disable", "w")
        self.cli("rm", "w")

    def test_a_note_or_notify_edit_is_let_through(self):
        self.book_as_person("w")
        self.as_session("bbbbbbbb-2222")
        self.cli("edit", "w", "--note", "nightly backup", "--notify", "always")
        p = self.cli("edit", "w", "--note", "x", "--timeout", "5m", ok=False)
        self.assertIn("not going to edit", p.stderr)

    def test_enable_is_not_guarded(self):
        self.book_as_person("w")
        self.cli("disable", "w")
        self.as_session("bbbbbbbb-2222")
        self.cli("enable", "w")

    def test_an_unrecorded_booker_is_guarded(self):
        self.book_as_person("w")
        self.sql("UPDATE job SET created_by=NULL WHERE name='w'")
        self.as_session("bbbbbbbb-2222")
        self.assertIn("someone unrecorded", self.cli("rm", "w", ok=False).stderr)


class Notify(Ledger):
    """Hooks: --notify says when, the executables in the notify dir say where."""

    def hook(self, name, body):
        p = self.hooks / name
        p.write_text("#!/bin/sh\n" + body)
        p.chmod(0o755)
        return p

    def recorder(self, name="rec"):
        # writes what it was told, env and stdin, one file per call
        out = Path(self.tmp.name) / f"{name}.calls"
        out.mkdir()
        self.hook(name, f'f="{out}/$$"; env | grep ^CRONTINENTAL_ > "$f.env"; cat > "$f.json"\n')
        return out

    def calls(self, out):
        result = []
        for envf in sorted(out.glob("*.env")):
            env = dict(l.split("=", 1) for l in envf.read_text().splitlines())
            result.append((env, json.loads(envf.with_suffix(".json").read_text())))
        return result

    def run_job(self, command, *flags):
        self.cli("every", "1h", "--name", "j", "--forever", *flags, "--", command)
        return subprocess.run([BIN, "exec", "j"], env=self.env, capture_output=True, text=True)

    def test_a_failure_tells_every_hook_in_env_and_json(self):
        a, b = self.recorder("a"), self.recorder("b")
        self.run_job("exit 3", "--note", "nightly sync")
        for out in (a, b):
            [(env, ev)] = self.calls(out)
            self.assertEqual(env["CRONTINENTAL_EVENT"], "fail")
            self.assertEqual(env["CRONTINENTAL_JOB"], "j")
            self.assertEqual(env["CRONTINENTAL_STATUS"], "3")
            self.assertEqual(env["CRONTINENTAL_URGENCY"], "critical")
            self.assertEqual(env["CRONTINENTAL_LOG"], "crontinental log j")
            self.assertEqual((ev["event"], ev["job"], ev["status"], ev["note"]),
                             ("fail", "j", 3, "nightly sync"))
            self.assertIn("elapsed", ev)

    def test_when_is_the_jobs_notify(self):
        out = self.recorder()
        self.run_job("true")                                   # fail: quiet on success
        self.assertEqual(self.calls(out), [])
        self.cli("rm", "j")
        self.run_job("exit 1", "--notify", "never")
        self.assertEqual(self.calls(out), [])
        self.cli("rm", "j")
        self.run_job("true", "--notify", "always")
        [(env, ev)] = self.calls(out)
        self.assertEqual((ev["event"], ev["status"], ev["urgency"]), ("ok", 0, "low"))

    def test_done_is_its_own_event(self):
        out = self.recorder()
        self.run_job("exit 99", "--notify", "always")
        [(env, ev)] = self.calls(out)
        self.assertEqual((ev["event"], ev["status"]), ("done", 99))

    def test_a_broken_hook_changes_nothing_and_the_others_still_run(self):
        self.hook("a-broken", "echo hook-said-this; exit 7\n")
        (self.hooks / "b-not-executable").write_text("#!/bin/sh\nexit 1\n")
        (self.hooks / ".hidden").write_text("")
        out = self.recorder("c")
        p = self.run_job("exit 3")
        self.assertEqual(p.returncode, 3, "the run's exit is the job's, not a hook's")
        self.assertEqual(len(self.calls(out)), 1)
        # what a hook says, and how it failed, lands in the run's own output
        self.assertIn("hook-said-this", p.stderr)
        self.assertIn("notify hook a-broken", p.stderr)
        self.assertEqual(self.sql("SELECT outcome, status FROM run"), [("fail", 3)])

    def test_a_hung_hook_is_given_up_on(self):
        self.hook("slow", "sleep 60\n")
        start = time.time()
        p = self.run_job("exit 3")
        self.assertLess(time.time() - start, 20)
        self.assertIn("gave up", p.stderr)
        self.assertEqual(self.sql("SELECT outcome FROM run"), [("fail",)])

    def test_a_message_from_anywhere(self):
        out = self.recorder()
        self.cli("notify", "disk is 91% full")
        [(env, ev)] = self.calls(out)
        self.assertEqual((ev["event"], ev["job"], ev["urgency"]), ("message", "", "low"))
        self.assertEqual((ev["headline"], ev["body"]), ("Crontinental", "disk is 91% full"))
        self.assertNotIn("log", ev)
        self.assertNotIn("status", ev)

    def test_urgent_and_title(self):
        out = self.recorder()
        self.cli("notify", "--urgent", "--title", "Deploy", "v1.4.2 is live")
        [(env, ev)] = self.calls(out)
        self.assertEqual((ev["urgency"], ev["headline"], env["CRONTINENTAL_URGENCY"]),
                         ("critical", "Deploy", "critical"))

    def test_a_message_from_inside_a_job_is_the_jobs_even_with_notify_never(self):
        out = self.recorder()
        p = self.run_job(f"{shlex.quote(BIN)} notify 'backup took 3h'; exit 0",
                         "--notify", "never", "--note", "nightly")
        self.assertEqual(p.returncode, 0, p.stderr)
        [(env, ev)] = self.calls(out)  # the message, and no end-of-run report
        self.assertEqual((ev["event"], ev["job"], ev["headline"], ev["note"], ev["log"]),
                         ("message", "j", "j", "nightly", "crontinental log j"))
        self.assertEqual(self.sql("SELECT outcome FROM run"), [("ok",)])

    def test_nobody_to_tell_is_said_but_not_an_error(self):
        p = self.cli("notify", "hello")
        self.assertIn("nobody was told", p.stderr)

    def test_a_message_needs_words(self):
        self.assertNotEqual(self.cli("notify", " ", ok=False).returncode, 0)
        self.assertEqual(self.cli("notify", ok=False).returncode, 2)

    def test_show_says_where_notifications_go(self):
        self.cli("every", "1h", "--name", "j", "--forever", "--", "true")
        self.assertIn("nobody is told", self.cli("show", "j").stdout)
        self.recorder("phone")
        self.assertIn("fail → phone", self.cli("show", "j").stdout)


class Migration(Ledger):
    def test_v1_ledger_is_migrated_without_stopping_anything(self):
        with sqlite3.connect(self.db) as c:
            c.executescript(V1_SCHEMA)
            c.execute("INSERT INTO job (name, command, kind, spec, interval_sec, next_run,"
                      " timeout_sec, created_at) VALUES ('legacy','true','repeat','1h',3600,?,3600,?)",
                      (ago(minutes=-30), ago(days=10)))
            c.execute("INSERT INTO job (name, command, kind, spec, interval_sec, next_run, until,"
                      " timeout_sec, created_at) VALUES ('ending','true','repeat','1h',3600,?,?,3600,?)",
                      (ago(minutes=-30), ago(hours=-5), ago(days=10)))
            c.execute("INSERT INTO job (name, command, kind, next_run, enabled, timeout_sec, created_at)"
                      " VALUES ('fired','true','once',NULL,0,3600,?)", (ago(days=10),))
        self.cli("list")
        self.assertEqual(self.sql("PRAGMA user_version"), [(2,)])
        self.assertEqual(self.job("legacy")[3], 1, "open-ended repeats become --forever")
        self.assertEqual(self.job("ending")[3], 0, "a repeat with --until keeps its end")
        self.assertIsNotNone(self.job("fired")[2], "finished one-offs start the retention clock")
        self.cli("tick")
        self.assertIsNotNone(self.job("fired"), "...rather than being reaped on the first tick")


if __name__ == "__main__":
    unittest.main()
