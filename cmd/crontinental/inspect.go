package main

import (
	"bytes"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

type lastRun struct {
	outcome string
	ok      bool
}

func lastOutcome(db *sql.DB, jobID int64) lastRun {
	var o string
	err := db.QueryRow("SELECT outcome FROM run WHERE job_id=? ORDER BY id DESC LIMIT 1", jobID).Scan(&o)
	return lastRun{o, err == nil}
}

func cmdList(o *opts) {
	db := connect()
	where := ""
	if !o.all {
		where = " WHERE enabled=1"
	}
	order := " ORDER BY enabled DESC, next_run IS NULL, next_run"
	if o.json {
		listJSON(db, "SELECT * FROM job"+where+order)
		return
	}
	rows, err := db.Query("SELECT " + jobCols + " FROM job" + where + order)
	if err != nil {
		die("ledger: %v", err)
	}
	var jobs []*Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			die("ledger: %v", err)
		}
		jobs = append(jobs, j)
	}
	rows.Close()
	if len(jobs) == 0 {
		if o.all {
			fmt.Println("nothing scheduled")
		} else {
			fmt.Println("nothing scheduled (--all also shows finished and disabled jobs)")
		}
		return
	}

	w := 0
	for _, j := range jobs {
		w = max(w, len([]rune(j.Name)))
	}
	marks := map[string]string{"ok": "ok", "done": "done", "fail": "FAIL", "killed": "KILLED",
		"running": "running", "skipped": "skipped", "overlap": "overlap", "error": "ERROR"}
	for _, j := range jobs {
		lr := lastOutcome(db, j.ID)
		state := ""
		if j.Enabled == 0 {
			state = "off"
			if j.FinishedAt.Valid {
				state = "done"
			}
		}
		when := state
		if j.NextRun.Valid {
			when = humanize(timePtr(j.NextRun.String))
		} else if when == "" {
			when = "-"
		}
		mark := ""
		if lr.ok {
			mark = marks[lr.outcome]
		}
		kind := "once"
		if j.Kind == "repeat" {
			kind = "every " + j.Spec.String
		}
		cmdtxt := j.Command
		if len([]rune(cmdtxt)) > 44 {
			cmdtxt = truncate(cmdtxt, 41) + "..."
		}
		fmt.Printf("%s  %s %s %s %s\n", pad(j.Name, w), pad(when, 26), pad(kind, 22), pad(mark, 8), cmdtxt)
	}
}

// pad left-aligns to n characters, like Python's f"{s:<n}".
func pad(s string, n int) string {
	if r := len([]rune(s)); r < n {
		return s + strings.Repeat(" ", n-r)
	}
	return s
}

// listJSON prints every column in table order, as the Python version's
// json.dumps([dict(row) ...]) did: integers as numbers, NULL as null.
func listJSON(db *sql.DB, q string) {
	rows, err := db.Query(q)
	if err != nil {
		die("ledger: %v", err)
	}
	defer rows.Close()
	cols, _ := rows.Columns()
	var out []json.RawMessage
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			die("ledger: %v", err)
		}
		var b bytes.Buffer
		b.WriteString("{")
		for i, c := range cols {
			if i > 0 {
				b.WriteString(",")
			}
			k, _ := json.Marshal(c)
			v := vals[i]
			if bs, ok := v.([]byte); ok {
				v = string(bs)
			}
			jv, _ := json.Marshal(v)
			b.Write(k)
			b.WriteString(":")
			b.Write(jv)
		}
		b.WriteString("}")
		out = append(out, b.Bytes())
	}
	if out == nil {
		out = []json.RawMessage{}
	}
	enc, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(enc))
}

// cmdNames prints bare names, one per line -- what tab completion reads. Every
// job by default: show/log/rm are as useful on a finished one-off as on a live
// one.
func cmdNames(o *opts) {
	q := "SELECT name FROM job"
	if o.enabled {
		q += " WHERE enabled=1"
	} else if o.disabled {
		q += " WHERE enabled=0"
	}
	rows, err := connect().Query(q + " ORDER BY name")
	if err != nil {
		die("ledger: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var n string
		rows.Scan(&n)
		fmt.Println(n)
	}
}

func cmdShow(o *opts) {
	db := connect()
	j := getJob(db, o.name)
	fmt.Printf("name        %s\n", j.Name)
	fmt.Printf("command     %s\n", j.Command)
	if j.Workdir.Valid {
		fmt.Printf("workdir     %s\n", j.Workdir.String)
	} else {
		fmt.Printf("workdir     ~ (%s)\n", shellDesc())
	}
	kind := j.Kind
	if j.Kind == "repeat" {
		kind += " — "
		if j.Spec.Valid && j.Spec.String != "" {
			kind += "every " + j.Spec.String
		}
	}
	fmt.Printf("kind        %s\n", kind)
	if j.NextRun.Valid {
		fmt.Printf("next run    %s\n", humanize(timePtr(j.NextRun.String)))
	} else {
		fmt.Printf("next run    -\n")
	}
	verdict := "skipped"
	if j.Catchup == "run" {
		verdict = "run anyway"
	}
	fmt.Printf("catchup     %s  (a fire >%ds late is %s)\n", j.Catchup, graceSec, verdict)
	fmt.Printf("timeout     %ds\n", j.TimeoutSec)
	fmt.Printf("notify      %s\n", describeNotify(j.Notify))
	if j.Enabled != 0 {
		fmt.Printf("enabled     yes\n")
	} else {
		fmt.Printf("enabled     no\n")
	}
	fmt.Printf("ends        %s\n", describeEnd(j))
	if j.FinishedAt.Valid {
		f := parseISO(j.FinishedAt.String)
		gone := f.Add(retainSec * time.Second)
		fmt.Printf("finished    %s; deleted %s\n", humanize(&f), humanize(&gone))
	}
	fmt.Printf("created     %s by %s\n", humanize(timePtr(j.CreatedAt)), j.CreatedBy.String)
	if j.Note.Valid && j.Note.String != "" {
		fmt.Printf("note        %s\n", j.Note.String)
	}
	label, where := runnerInfo(j.Name)
	fmt.Printf("%-11s %s\n", label, where)

	rows, err := db.Query(`SELECT started_at, finished_at, status, outcome, late_sec, detail
		FROM run WHERE job_id=? ORDER BY id DESC LIMIT 10`, j.ID)
	if err != nil {
		die("ledger: %v", err)
	}
	defer rows.Close()
	first := true
	for rows.Next() {
		var started, finished, detail sql.NullString
		var status, late sql.NullInt64
		var outcome string
		if err := rows.Scan(&started, &finished, &status, &outcome, &late, &detail); err != nil {
			die("ledger: %v", err)
		}
		if first {
			fmt.Println("\nrecent runs")
			first = false
		}
		st := ""
		if started.Valid {
			st = parseISO(started.String).Local().Format("01-02 15:04")
		}
		exit := ""
		if status.Valid {
			exit = "exit " + strconv.FormatInt(status.Int64, 10)
		}
		dur := ""
		if started.Valid && finished.Valid {
			dur = fmt.Sprintf("%ds", int(parseISO(finished.String).Sub(parseISO(started.String)).Seconds()))
		}
		lateTxt := ""
		if late.Valid && late.Int64 > graceSec {
			lateTxt = fmt.Sprintf("late %ds", late.Int64)
		}
		fmt.Printf("  %s  %s %s%s %s %s\n", st, pad(outcome, 9), pad(exit, 9), pad(dur, 7), lateTxt, detail.String)
	}
}

func cmdLog(o *opts) {
	j := getJob(connect(), o.name)
	showLog(j.Name, o.lines, o.follow)
}

func cmdRun(o *opts) {
	db := connect()
	j := getJob(db, o.name)
	// Off-schedule, so next_run is deliberately untouched: testing a job must
	// not move the schedule it is being tested against.
	dispatch(db, j, now(), 0)
	fmt.Printf("%s: started — crontinental log %s\n", j.Name, j.Name)
}

func cmdRm(o *opts) {
	db := connect()
	j := getJob(db, o.name)
	stopRun(j.Name)
	mustExec(db, "DELETE FROM job WHERE id=?", j.ID)
	fmt.Printf("removed %s\n", j.Name)
}

func cmdToggle(o *opts, on bool) {
	db := connect()
	j := getJob(db, o.name)
	var next *time.Time
	if j.NextRun.Valid {
		next = timePtr(j.NextRun.String)
	}
	if on && (next == nil || !next.After(now())) {
		if j.Kind == "once" {
			die("%s is a one-off that has already run; schedule a new one", j.Name)
		}
		n, err := nextFire(j, now())
		if err != nil {
			die("%s: cannot compute its next run: %v", j.Name, err)
		}
		if n == nil || (j.Until.Valid && !n.Before(parseISO(j.Until.String))) {
			var u *time.Time
			if j.Until.Valid {
				u = timePtr(j.Until.String)
			}
			die("%s has passed its --until (%s); schedule a new one", j.Name, humanize(u))
		}
		next = n
	}
	if on {
		// Re-enabling a finished repeat takes it back off the reaper's clock.
		mustExec(db, "UPDATE job SET enabled=1, next_run=?, finished_at=NULL WHERE id=?", isoOrNil(next), j.ID)
		fmt.Printf("%s: enabled, next run %s\n", j.Name, humanize(next))
	} else {
		mustExec(db, "UPDATE job SET enabled=0 WHERE id=?", j.ID)
		fmt.Printf("%s: disabled\n", j.Name)
	}
}

func cmdPrune(o *opts) {
	db := connect()
	cutoff := iso(now().Add(-time.Duration(o.days) * 24 * time.Hour))
	res := mustExec(db, "DELETE FROM job WHERE finished_at IS NOT NULL AND finished_at<?", cutoff)
	n, _ := res.RowsAffected()
	mustExec(db, "VACUUM")
	fmt.Printf("pruned %d finished job(s) older than %dd\n", n, o.days)
}

// describeNotify says who hears about a job: --notify is when, the hooks are
// where. A job that should notify with no hooks installed is said out loud,
// since nothing else would ever show it.
func describeNotify(when string) string {
	if when == "never" {
		return "never"
	}
	hs := hooks()
	if len(hs) == 0 {
		return fmt.Sprintf("%s, but nobody is told: no hooks in %s", when, notifyDir())
	}
	var names []string
	for _, h := range hs {
		names = append(names, filepath.Base(h))
	}
	return fmt.Sprintf("%s → %s", when, strings.Join(names, ", "))
}
