package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

const schemaVersion = 4

const schema = `
CREATE TABLE IF NOT EXISTS job (
  id           INTEGER PRIMARY KEY,
  name         TEXT NOT NULL UNIQUE,
  command      TEXT NOT NULL,
  workdir      TEXT,
  kind         TEXT NOT NULL,              -- 'once' | 'repeat'
  spec         TEXT,                       -- calendar spec, or a timespan for fixed intervals
  interval_sec INTEGER,                    -- set when spec is a timespan rather than a calendar
  next_run     TEXT,                       -- ISO-8601 UTC; NULL once a one-off has fired
  catchup      TEXT NOT NULL DEFAULT 'run',-- 'run' | 'skip' a fire that is more than graceSec late
  until        TEXT,                       -- repeats stop here
  until_done   INTEGER NOT NULL DEFAULT 0, -- repeat ends when the command exits doneExit
  forever      INTEGER NOT NULL DEFAULT 0, -- repeat with no end, chosen explicitly
  finished_at  TEXT,                       -- set when the job can never run again; reaped retainSec later
  timeout_sec  INTEGER NOT NULL,
  notify       TEXT NOT NULL DEFAULT 'fail', -- 'never' | 'fail' | 'always'
  enabled      INTEGER NOT NULL DEFAULT 1,
  note         TEXT,
  created_at   TEXT NOT NULL,
  created_by   TEXT,
  tags         TEXT                        -- JSON object of the job's --tag key=value pairs; NULL for none
);
CREATE INDEX IF NOT EXISTS job_due ON job(enabled, next_run);

-- History is per RUN, not per job, so "it failed" and "it failed every night
-- this week" are different-looking facts rather than the same overwritten row.
CREATE TABLE IF NOT EXISTS run (
  id          INTEGER PRIMARY KEY,
  job_id      INTEGER NOT NULL REFERENCES job(id) ON DELETE CASCADE,
  unit        TEXT,
  due_at      TEXT,
  started_at  TEXT,
  finished_at TEXT,
  status      INTEGER,                     -- process exit code; NULL while running
  outcome     TEXT NOT NULL,               -- 'running'|'ok'|'done'|'fail'|'killed'|'skipped'|'overlap'|'error'
  late_sec    INTEGER,
  detail      TEXT
);
CREATE INDEX IF NOT EXISTS run_job ON run(job_id, id DESC);
` + messageSchema

// messageSchema is the message book: every notification, written down before
// the hooks hear it. Its own constant so notify can make sure of it on a
// ledger nothing else has opened yet. A v3 ledger gains it the same way (v4 is
// only this table).
const messageSchema = `
CREATE TABLE IF NOT EXISTS message (
  id        INTEGER PRIMARY KEY,
  at        TEXT NOT NULL,                 -- ISO-8601 UTC
  event     TEXT NOT NULL,                 -- fail|killed|error|unstartable|done|ok|message
  job       TEXT,                          -- NULL for a message from outside any job
  run_id    INTEGER,                       -- the run it is about, or was sent from
  urgency   TEXT NOT NULL,                 -- critical|low
  headline  TEXT NOT NULL,
  body      TEXT NOT NULL,
  tags      TEXT                           -- JSON object, as the hooks got it
);
`

// Always name the columns. A ledger migrated from v1 has the lifecycle columns
// at the END of the table (ALTER TABLE appends), a fresh one has them in the
// middle, so anything positional over SELECT * reads one of them wrong.
const jobCols = `id, name, command, workdir, kind, spec, interval_sec, next_run, catchup,
  until, until_done, forever, finished_at, timeout_sec, notify, enabled, note,
  created_at, created_by, tags`

type Job struct {
	ID          int64
	Name        string
	Command     string
	Workdir     sql.NullString
	Kind        string
	Spec        sql.NullString
	IntervalSec sql.NullInt64
	NextRun     sql.NullString
	Catchup     string
	Until       sql.NullString
	UntilDone   int64
	Forever     int64
	FinishedAt  sql.NullString
	TimeoutSec  int64
	Notify      string
	Enabled     int64
	Note        sql.NullString
	CreatedAt   string
	CreatedBy   sql.NullString
	Tags        sql.NullString
}

type scanner interface{ Scan(...any) error }

func scanJob(s scanner) (*Job, error) {
	var j Job
	err := s.Scan(&j.ID, &j.Name, &j.Command, &j.Workdir, &j.Kind, &j.Spec, &j.IntervalSec,
		&j.NextRun, &j.Catchup, &j.Until, &j.UntilDone, &j.Forever, &j.FinishedAt,
		&j.TimeoutSec, &j.Notify, &j.Enabled, &j.Note, &j.CreatedAt, &j.CreatedBy, &j.Tags)
	return &j, err
}

func dbPath() string {
	if p := os.Getenv("CRONTINENTAL_DB"); p != "" {
		return p
	}
	return filepath.Join(dataHome(), "crontinental", "ledger.db")
}

func dataHome() string {
	if d := os.Getenv("XDG_DATA_HOME"); d != "" {
		return d
	}
	return filepath.Join(homeDir(), ".local", "share")
}

func connect() *sql.DB {
	path := dbPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		die("cannot create %s: %v", filepath.Dir(path), err)
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(10000)&_pragma=foreign_keys(1)")
	if err != nil {
		die("cannot open the ledger: %v", err)
	}
	// One connection: every pragma above then holds for every statement, and
	// the CLI never needs concurrency inside a single process.
	db.SetMaxOpenConns(1)
	// WAL so a job's own `exec` writing its result never blocks the tick that is
	// trying to dispatch the next one.
	mustExec(db, "PRAGMA journal_mode=WAL")
	mustExec(db, schema)
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		die("cannot read the ledger version: %v", err)
	}
	if version < 2 {
		migrateV2(db)
	}
	if version < 3 {
		migrateV3(db)
	}
	if version < schemaVersion {
		mustExec(db, fmt.Sprintf("PRAGMA user_version=%d", schemaVersion))
	}
	return db
}

// migrateV2 adds the lifecycle columns. Every repeat that already exists
// without an end was scheduled before an end was required, so it is marked
// --forever rather than left to be refused or expired; anything already
// finished starts its retention clock now instead of being deleted on the
// first tick.
func migrateV2(db *sql.DB) {
	have := jobColumns(db)
	for _, c := range [][2]string{
		{"until_done", "INTEGER NOT NULL DEFAULT 0"},
		{"forever", "INTEGER NOT NULL DEFAULT 0"},
		{"finished_at", "TEXT"},
	} {
		if !have[c[0]] {
			mustExec(db, "ALTER TABLE job ADD COLUMN "+c[0]+" "+c[1])
		}
	}
	t := iso(now())
	mustExec(db, "UPDATE job SET forever=1 WHERE kind='repeat' AND until IS NULL")
	mustExec(db, "UPDATE job SET finished_at=? WHERE finished_at IS NULL AND enabled=0"+
		" AND next_run IS NULL AND (kind='once' OR until<=?)", t, t)
}

// migrateV3 adds tags. Every existing job has none, which NULL already says.
func migrateV3(db *sql.DB) {
	if !jobColumns(db)["tags"] {
		mustExec(db, "ALTER TABLE job ADD COLUMN tags TEXT")
	}
}

// jobColumns: the job table's columns as the ledger has them, which the
// CREATE TABLE IF NOT EXISTS above leaves alone for an older ledger.
func jobColumns(db *sql.DB) map[string]bool {
	have := map[string]bool{}
	rows, err := db.Query("PRAGMA table_info(job)")
	if err != nil {
		die("cannot read the ledger schema: %v", err)
	}
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			die("cannot read the ledger schema: %v", err)
		}
		have[name] = true
	}
	rows.Close()
	return have
}

func mustExec(db *sql.DB, q string, args ...any) sql.Result {
	r, err := db.Exec(q, args...)
	if err != nil {
		die("ledger: %v", err)
	}
	return r
}

// lookupJob is getJob for a job that may not be there.
func lookupJob(db *sql.DB, name string) *Job {
	j, err := scanJob(db.QueryRow("SELECT "+jobCols+" FROM job WHERE name=?", name))
	if err == sql.ErrNoRows {
		return nil
	} else if err != nil {
		die("ledger: %v", err)
	}
	return j
}

func getJob(db *sql.DB, name string) *Job {
	j, err := scanJob(db.QueryRow("SELECT "+jobCols+" FROM job WHERE name=?", name))
	if err == sql.ErrNoRows {
		die("no such job: %s", name)
	} else if err != nil {
		die("ledger: %v", err)
	}
	return j
}

// finishJob: the job can never run again -- off the schedule, and onto the
// reaper's clock.
func finishJob(db *sql.DB, id int64) {
	mustExec(db, "UPDATE job SET enabled=0, next_run=NULL, finished_at=? WHERE id=?", iso(now()), id)
}
