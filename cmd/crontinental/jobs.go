package main

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// ---------------------------------------------------------------------- add

func cmdAdd(o *opts, kind string) {
	command := joinCommand(o.command)
	if command == "" {
		die("no command given -- put it after `--`")
	}
	name := defaultName(command)
	if o.name != "" {
		name = validName(o.name)
	}

	db := connect()
	var exists int
	if db.QueryRow("SELECT 1 FROM job WHERE name=?", name).Scan(&exists) == nil {
		die("a job named %q already exists (rm it, or pass a different --name)", name)
	}

	var (
		when           time.Time
		interval       *int64
		spec           *string
		defaultCatchup string
	)
	if o.now && kind != "repeat" {
		die("--now is for repeats; a one-off that should run now is `in 0` (or `at now`)")
	}
	booked := now() // "in 0" and "at now" land exactly here, so they count as now, not past
	if kind == "once" {
		if o.verb == "at" {
			when = parseWhen(o.when)
		} else {
			secs, err := parseTimespan(o.when)
			if err != nil {
				die("%q is not a delay: %v (try 90m, 2h, 1d)", o.when, err)
			}
			when = now().Add(time.Duration(secs) * time.Second)
		}
		if when.Before(booked) {
			die("that time is in the past: %s", when.Local().Format("2006-01-02 15:04"))
		}
		defaultCatchup = "run"
	} else {
		s := o.when
		spec = &s
		next, iv := parseRepeat(s)
		when = next
		if secs, ok := iv.(int64); ok {
			interval = &secs
		}
		// systemd's own default for a missed timer run is to skip it, and the
		// same reasoning holds here: a nightly job that wakes at noon usually
		// wants tonight, not a surprise run in the middle of the day.
		defaultCatchup = "skip"
	}

	var until *time.Time
	if o.until != "" {
		u := parseWhen(o.until)
		until = &u
		if !u.After(now()) {
			die("--until is in the past: %s", u.Local().Format("2006-01-02 15:04"))
		}
	}
	if kind == "repeat" {
		if o.forever && (until != nil || o.untilDone) {
			die("--forever contradicts --until/--until-done; pick one")
		}
		if until == nil && !o.untilDone && !o.forever {
			die("a repeat has to say how it ends:\n"+
				"  --until <time>   stop after a deadline (\"tomorrow 18:00\", \"3 days\")\n"+
				"  --until-done     stop when the command exits %d (polling for something)\n"+
				"  --forever        never stop -- only for jobs meant to be permanent", doneExit)
		}
	} else if o.forever || o.untilDone {
		die("--forever/--until-done are for repeats; a one-off ends when it fires")
	}

	var workdir *string
	if o.workdir != "" {
		w := resolveDir(o.workdir)
		if st, err := os.Stat(w); err != nil || !st.IsDir() {
			die("--workdir is not a directory: %s", w)
		}
		workdir = &w
	}

	timeout := int64(defaultTimeoutSec)
	if o.timeout != "" {
		secs, err := parseTimespan(o.timeout)
		if err != nil {
			die("%q is not a timeout: %v", o.timeout, err)
		}
		timeout = secs
	}
	catchup := o.catchup
	if catchup == "" {
		catchup = defaultCatchup
	}
	by := o.by
	if by == "" {
		by = provenance()
	}

	// A one-off due now is booked already fired: no next_run for the tick to find,
	// so the run below is its only one. (Same order as the tick: advance, then
	// dispatch.)
	dueNow := kind == "once" && !when.After(now())
	nextRun, enabled, finished := any(iso(when)), 1, any(nil)
	if dueNow {
		nextRun, enabled, finished = nil, 0, iso(now())
	}
	mustExec(db, `INSERT INTO job (name, command, workdir, kind, spec, interval_sec, next_run,
		catchup, until, until_done, forever, timeout_sec, notify, note, created_at, created_by,
		enabled, finished_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		name, command, workdir, kind, spec, interval, nextRun,
		catchup, isoOrNil(until), b2i(o.untilDone), b2i(o.forever), timeout,
		o.notify, nullIfEmpty(o.note), iso(now()), by, enabled, finished)

	if dueNow || o.now {
		// --now is `run` straight after booking: off-schedule, so the first
		// scheduled run stays where the spec put it.
		dispatch(db, getJob(db, name), now(), 0)
	}
	if dueNow {
		fmt.Printf("%s: started now — crontinental log %s\n", name, name)
		fmt.Printf("  command: %s\n", command)
		return
	}
	fmt.Printf("%s: next run %s\n", name, humanize(&when))
	if o.now {
		fmt.Printf("  started now — crontinental log %s\n", name)
	}
	if kind == "repeat" {
		if interval != nil {
			fmt.Printf("  repeating: every %s\n", *spec)
		} else {
			fmt.Printf("  repeating: %s\n", *spec)
		}
		fmt.Printf("  ends: %s\n", describeEnd(getJob(db, name)))
	}
	fmt.Printf("  command: %s\n", command)
}

// resolveDir is ~-expansion plus an absolute, symlink-free path, like
// Python's Path.expanduser().resolve() (which tolerates a missing path).
func resolveDir(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		p = filepath.Join(homeDir(), p[1:])
	}
	abs, err := filepath.Abs(p)
	if err != nil {
		return p
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real
	}
	return abs
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// --------------------------------------------------------------------- tick

func cmdTick() {
	db := connect()
	t := now()
	reap(db, t)

	rows, err := db.Query("SELECT "+jobCols+" FROM job WHERE enabled=1 AND next_run IS NOT NULL"+
		" AND next_run<=? ORDER BY next_run", iso(t))
	if err != nil {
		die("ledger: %v", err)
	}
	var due []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			die("ledger: %v", err)
		}
		due = append(due, j)
	}
	rows.Close()

	for _, j := range due {
		dueAt := parseISO(j.NextRun.String)
		late := int64(t.Sub(dueAt).Seconds())

		if j.Until.Valid && !t.Before(parseISO(j.Until.String)) {
			finishJob(db, j.ID)
			continue
		}

		// Advance BEFORE dispatching. If anything below fails, the job has
		// already moved on -- the alternative is a job that failed to launch
		// being retried every 60 seconds forever.
		if err := advance(db, j, t); err != nil {
			fmt.Fprintf(os.Stderr, "crontinental: %s: cannot compute its next run: %v\n", j.Name, err)
			continue
		}

		if j.Catchup == "skip" && late > graceSec {
			record(db, j, dueAt, late, "skipped", fmt.Sprintf("%ds late; catchup=skip", late), "")
			continue
		}
		dispatch(db, j, dueAt, late)
	}
}

func advance(db *sql.DB, j *Job, t time.Time) error {
	var next *time.Time
	if j.Kind == "repeat" {
		n, err := nextFire(j, t)
		if err != nil {
			return err
		}
		next = n
		if next != nil && j.Until.Valid && !next.Before(parseISO(j.Until.String)) {
			next = nil
		}
	}
	if next == nil {
		finishJob(db, j.ID)
	} else {
		mustExec(db, "UPDATE job SET next_run=? WHERE id=?", iso(*next), j.ID)
	}
	return nil
}

// reap deletes jobs finished more than retainSec ago (their runs cascade). A
// run still marked running holds its job back, so a long last run is never
// pulled out from under its own `exec` -- unless that row is older than
// retainSec, which only an `exec` killed too hard to write down its ending
// leaves behind.
func reap(db *sql.DB, t time.Time) {
	cutoff := iso(t.Add(-retainSec * time.Second))
	mustExec(db, `DELETE FROM job WHERE finished_at IS NOT NULL AND finished_at<? AND NOT EXISTS
		(SELECT 1 FROM run WHERE run.job_id=job.id AND outcome='running' AND started_at>=?)`,
		cutoff, cutoff)
}

func record(db *sql.DB, j *Job, dueAt time.Time, late int64, outcome, detail, unit string) {
	n := iso(now())
	mustExec(db, `INSERT INTO run (job_id, unit, due_at, started_at, finished_at, outcome, late_sec, detail)
		VALUES (?,?,?,?,?,?,?,?)`,
		j.ID, nullIfEmpty(unit), iso(dueAt), n, n, outcome, late, nullIfEmpty(detail))
}

func self() string {
	p, err := os.Executable()
	if err != nil {
		die("cannot find my own binary: %v", err)
	}
	if real, err := filepath.EvalSymlinks(p); err == nil {
		return real
	}
	return p
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// --------------------------------------------------------------------- exec

// cmdExec is what each run is: the process that starts the command and
// outlives it to write down what it did. On Linux it is the run's transient
// unit; on macOS the tick starts it in a session of its own. The command's
// output goes straight to the run's log (no capture), so `log` has one source
// of truth.
func cmdExec(o *opts) {
	db := connect()
	j := getJob(db, o.name)
	ref := runRef(j.Name)
	dueAt := now()
	if o.due != "" {
		dueAt = parseISO(o.due)
	}
	started := now()

	// The overlap guard, where the platform has no unit to refuse a second
	// copy (macOS): a lock only one run of a job can hold.
	g, ok := guardRun(j.Name)
	if !ok {
		record(db, j, dueAt, o.late, "overlap", "a run of this job is still going", ref)
		return
	}

	res := mustExec(db, `INSERT INTO run (job_id, unit, due_at, started_at, outcome, late_sec)
		VALUES (?,?,?,?, 'running', ?)`, j.ID, ref, iso(dueAt), iso(started), o.late)
	runID, _ := res.LastInsertId()

	finish := func(status int, outcome, detail string) {
		mustExec(db, "UPDATE run SET finished_at=?, status=?, outcome=?, detail=? WHERE id=?",
			iso(now()), status, outcome, nullIfEmpty(detail), runID)
	}
	killed := func(sig syscall.Signal, detail string) {
		finish(128+int(sig), "killed", detail)
		if j.Notify != "never" {
			notify("Scheduled job killed", fmt.Sprintf("%s hit its %ds limit", j.Name, j.TimeoutSec), true)
		}
		banner("killed: " + detail)
		os.Exit(128 + int(sig))
	}

	workdir := homeDir()
	if j.Workdir.Valid {
		workdir = j.Workdir.String
	}
	banner(fmt.Sprintf("run started (due %s, late %ds)", dueAt.Local().Format("2006-01-02 15:04:05"), o.late))
	cmd := exec.Command(shellArgv(j.Command)[0], shellArgv(j.Command)[1:]...)
	cmd.Dir = workdir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, os.Stdout, os.Stderr
	// Its own process group, so a stop or a timeout reaches everything the
	// command started, not just the shell.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	var stopReason atomic.Value // set when this process ends the command itself
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM)

	err := cmd.Start()
	if err == nil {
		g.started(cmd.Process.Pid)
		pgid := cmd.Process.Pid
		stopGroup := func(reason string) {
			stopReason.Store(reason)
			_ = syscall.Kill(-pgid, syscall.SIGTERM)
			time.AfterFunc(10*time.Second, func() { _ = syscall.Kill(-pgid, syscall.SIGKILL) })
		}
		go func() {
			sig := (<-sigs).(syscall.Signal)
			if !selfManaged {
				// RuntimeMaxSec expired, or someone stopped the unit. systemd
				// kills the whole cgroup, so the command is already going; all
				// that is left is to say so before this process goes too.
				killed(sig, fmt.Sprintf("stopped by signal %d", int(sig)))
			}
			stopGroup(fmt.Sprintf("stopped by signal %d", int(sig)))
		}()
		if selfManaged {
			// No RuntimeMaxSec here: the timeout is this process's to enforce.
			time.AfterFunc(time.Duration(j.TimeoutSec)*time.Second, func() {
				stopGroup(fmt.Sprintf("hit its %ds limit", j.TimeoutSec))
			})
		}
		err = cmd.Wait()
	}
	if r, ok := stopReason.Load().(string); ok {
		killed(syscall.SIGTERM, r)
	}

	status := 0
	var exitErr *exec.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exitErr):
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			status = -int(ws.Signal()) // as Python reported a command killed by a signal
		} else {
			status = exitErr.ExitCode()
		}
	default: // the command could not be started at all; the run must still record why
		finish(-1, "error", truncate(err.Error(), 500))
		if j.Notify != "never" {
			notify("Scheduled job errored", truncate(j.Name+": "+err.Error(), 160), true)
		}
		banner("could not start: " + err.Error())
		os.Exit(1)
	}

	elapsed := int(now().Sub(started).Seconds())
	if status == doneExit {
		// The job's own verdict that there is nothing left to do. Honoured on
		// every job, declared or not: a command that says it is finished and
		// keeps getting run is worse than one that stops a little early.
		finish(status, "done", "")
		finishJob(db, j.ID)
		if j.Notify == "always" {
			notify("Scheduled job finished", fmt.Sprintf("%s reported done after %ds", j.Name, elapsed), false)
		}
		banner(fmt.Sprintf("done (exit %d) after %ds", doneExit, elapsed))
		os.Exit(0) // done is success; the run should not show as failed
	}

	outcome := "ok"
	if status != 0 {
		outcome = "fail"
	}
	finish(status, outcome, "")
	if status != 0 && (j.Notify == "fail" || j.Notify == "always") {
		notify("Scheduled job failed", fmt.Sprintf("%s exited %d after %ds — crontinental log %s",
			j.Name, status, elapsed, j.Name), true)
	} else if status == 0 && j.Notify == "always" {
		notify("Scheduled job done", fmt.Sprintf("%s finished in %ds", j.Name, elapsed), false)
	}
	banner(fmt.Sprintf("exit %d after %ds", status, elapsed))
	os.Exit(status & 0xff) // the run's own exit should mirror the job's
}
