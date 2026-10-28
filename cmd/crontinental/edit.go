package main

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// cmdEdit changes a job in place, keeping its history and provenance --
// which delete-and-rebook would lose. It holds a job to the same rules as
// booking one: a repeat still has to end.
//
// Ends compose the way people mean them: naming an end (--until,
// --until-done) drops --forever, and --forever drops both ends. The name stays
// (it is the unit name; rename is rm plus a new booking), and so does whether
// the job is enabled (that is enable/disable).
func cmdEdit(o *opts) {
	db := connect()
	j := getJob(db, o.name)
	var changed []string
	fireNow := false
	sets := map[string]any{}
	set := func(col string, v any, what string) {
		sets[col] = v
		changed = append(changed, what)
	}

	// --- schedule
	if o.every != "" && j.Kind != "repeat" {
		die("%s is a one-off; --every only edits repeats (use --at or --in)", j.Name)
	}
	if (o.at != "" || o.in != "") && j.Kind != "once" {
		die("%s is a repeat; --at/--in only edit one-offs (use --every)", j.Name)
	}
	if o.at != "" && o.in != "" {
		die("--at and --in both say when; pick one")
	}
	switch {
	case o.every != "":
		next, interval := parseRepeat(o.every)
		set("spec", o.every, "schedule")
		set("interval_sec", interval, "")
		set("next_run", iso(next), "")
		j.Spec.String, j.Spec.Valid = o.every, true
	case o.at != "" || o.in != "":
		booked := now()
		var when time.Time
		if o.at != "" {
			when = parseWhen(o.at)
		} else {
			secs, err := parseTimespan(o.in)
			if err != nil {
				die("%q is not a delay: %v (try 90m, 2h, 1d)", o.in, err)
			}
			when = now().Add(time.Duration(secs) * time.Second)
		}
		if when.Before(booked) {
			die("that time is in the past: %s", when.Local().Format("2006-01-02 15:04"))
		}
		set("next_run", iso(when), "time")
		fireNow = !when.After(now())
	}

	// --- how it ends
	if o.until != "" && o.noUntil {
		die("--until and --no-until contradict each other")
	}
	if o.untilDone && o.noUntilDone {
		die("--until-done and --no-until-done contradict each other")
	}
	if o.forever && (o.until != "" || o.untilDone) {
		die("--forever contradicts --until/--until-done; pick one")
	}
	if j.Kind == "once" && (o.until != "" || o.noUntil || o.untilDone || o.noUntilDone || o.forever) {
		die("%s is a one-off: it ends when it fires", j.Name)
	}
	until, untilDone, forever := j.Until, j.UntilDone != 0, j.Forever != 0
	endChanged := false
	if o.until != "" {
		u := parseWhen(o.until)
		if !u.After(now()) {
			die("--until is in the past: %s", u.Local().Format("2006-01-02 15:04"))
		}
		until.String, until.Valid = iso(u), true
		forever, endChanged = false, true
	}
	if o.noUntil {
		until.Valid, endChanged = false, true
	}
	if o.untilDone {
		untilDone, forever, endChanged = true, false, true
	}
	if o.noUntilDone {
		untilDone, endChanged = false, true
	}
	if o.forever {
		until.Valid, untilDone, forever, endChanged = false, false, true, true
	}
	if endChanged {
		if j.Kind == "repeat" && !until.Valid && !untilDone && !forever {
			die("that leaves %s without an end. A repeat has to say how it ends:\n"+
				"  --until <time>   stop after a deadline (\"tomorrow 18:00\", \"3 days\")\n"+
				"  --until-done     stop when the command exits %d (polling for something)\n"+
				"  --forever        never stop -- only for jobs meant to be permanent", j.Name, doneExit)
		}
		if until.Valid {
			set("until", until.String, "end")
		} else {
			set("until", nil, "end")
		}
		set("until_done", b2i(untilDone), "")
		set("forever", b2i(forever), "")
		j.Until, j.UntilDone, j.Forever = until, int64(b2i(untilDone)), int64(b2i(forever))
	}

	// --- the rest
	if o.timeout != "" {
		secs, err := parseTimespan(o.timeout)
		if err != nil {
			die("%q is not a timeout: %v", o.timeout, err)
		}
		set("timeout_sec", secs, "timeout")
	}
	if o.notifySet {
		set("notify", o.notify, "notify")
	}
	if o.catchup != "" {
		set("catchup", o.catchup, "catchup")
	}
	if o.noteSet {
		set("note", nullIfEmpty(o.note), "note")
	}
	if o.workdir != "" {
		w := resolveDir(o.workdir)
		if st, err := os.Stat(w); err != nil || !st.IsDir() {
			die("--workdir is not a directory: %s", w)
		}
		set("workdir", w, "workdir")
	}
	if len(o.command) > 0 {
		c := joinCommand(o.command)
		if c == "" {
			die("the new command is empty")
		}
		set("command", c, "command")
	}

	if len(sets) == 0 {
		die("nothing to change (see crontinental --help for what edit takes)")
	}
	for c := range sets {
		if c != "note" && c != "notify" { // saying why, or how loudly, is harmless
			guardOthers(j, "edit", o.force)
			break
		}
	}
	var cols []string
	var args []any
	for c, v := range sets {
		cols = append(cols, c+"=?")
		args = append(args, v)
	}
	mustExec(db, "UPDATE job SET "+strings.Join(cols, ", ")+" WHERE id=?", append(args, j.ID)...)

	j = getJob(db, j.Name)
	// A one-off moved to now runs now, the way `in 0` does when it is booked:
	// finished first, so the tick has nothing left to fire. A disabled or
	// already-finished one-off only gets its new time (enable decides).
	started := false
	if fireNow && j.Enabled != 0 && !j.FinishedAt.Valid {
		finishJob(db, j.ID)
		dispatch(db, j, now(), 0)
		j, started = getJob(db, j.Name), true
	}
	var what []string
	for _, c := range changed {
		if c != "" {
			what = append(what, c)
		}
	}
	fmt.Printf("%s: changed %s\n", j.Name, strings.Join(what, ", "))
	if started {
		fmt.Printf("  started now — crontinental log %s\n", j.Name)
		return
	}
	if j.NextRun.Valid {
		fmt.Printf("  next run: %s\n", humanize(timePtr(j.NextRun.String)))
	}
	fmt.Printf("  ends: %s\n", describeEnd(j))
	if j.FinishedAt.Valid {
		fmt.Printf("  note: %s has finished; `crontinental enable %s` puts it back on the schedule\n", j.Name, j.Name)
	}
}

// parseRepeat reads an `every` spec: a calendar first, then an interval,
// refusing anything else with both readings explained.
func parseRepeat(s string) (time.Time, any) {
	next, calErr := calendarNext(s, nil)
	if calErr == nil {
		if next == nil {
			die("calendar spec %q never elapses", s)
		}
		return *next, nil
	}
	secs, spanErr := parseTimespan(s)
	if spanErr != nil {
		die("%q is neither a calendar spec nor an interval.\n"+
			"  as a calendar: %v\n"+
			"  as an interval: %v\n"+
			"  calendar examples: \"03:00\", \"Mon..Fri 07:30\", \"*-*-01 09:00\", \"hourly\"\n"+
			"  interval examples: 30m, 2h, 1d\n"+
			"  check one with: crontinental check %s", s, calErr, spanErr, shellQuote(s))
	}
	if secs < 60 {
		die("interval %q is shorter than the 1-minute tick", s)
	}
	return now().Add(time.Duration(secs) * time.Second), secs
}
