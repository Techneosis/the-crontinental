package main

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTimeoutSec = 3600

	// A run that fires within this many seconds of its due time is "on time".
	// The tick is 1/min, so an on-time fire is always under 60s late; anything
	// past this means the machine was asleep, off, or wedged -- which is exactly
	// the case --catchup decides.
	graceSec = 120

	// The exit status a command uses to say "my work is finished, stop running
	// me". Not 100: apt-get exits 100 on error, and a failing job must never
	// look done.
	doneExit = 99

	// How long a finished job stays around for show/log before the tick
	// deletes it.
	retainSec = 3 * 86400
	// messageRetainSec: how long the message book keeps a notification.
	messageRetainSec = 30 * 86400
)

// isoLayout is what the ledger stores: Python's datetime.isoformat() for a
// whole-second UTC time ("2026-10-23T08:25:57+00:00"). The tick compares these
// as strings, so every writer must produce exactly this shape.
const isoLayout = "2006-01-02T15:04:05-07:00"

func now() time.Time { return time.Now().UTC().Truncate(time.Second) }

func iso(t time.Time) string { return t.UTC().Format(isoLayout) }

func isoOrNil(t *time.Time) any {
	if t == nil {
		return nil
	}
	return iso(*t)
}

func parseISO(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		die("ledger holds an unreadable time %q", s)
	}
	return t
}

func die(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "crontinental: "+format+"\n", args...)
	os.Exit(1)
}

func homeDir() string {
	h, err := os.UserHomeDir()
	if err != nil {
		die("cannot find the home directory: %v", err)
	}
	return h
}

func humanize(t *time.Time) string {
	if t == nil {
		return "-"
	}
	local := t.Local().Format("Mon 01-02 15:04")
	delta := t.Sub(now()).Seconds()
	if math.Abs(delta) < 5 {
		return local + " (just now)"
	}
	future := delta >= 0
	if !future {
		delta = -delta
	}
	var n string
	switch {
	case delta < 90:
		n = fmt.Sprintf("%ds", int(delta))
	case delta < 5400:
		n = fmt.Sprintf("%dm", int(delta/60))
	case delta < 172800:
		n = fmt.Sprintf("%.1fh", delta/3600)
	default:
		n = fmt.Sprintf("%dd", int(delta/86400))
	}
	if future {
		return fmt.Sprintf("%s (in %s)", local, n)
	}
	return fmt.Sprintf("%s (%s ago)", local, n)
}

func timePtr(s string) *time.Time {
	t := parseISO(s)
	return &t
}

func describeEnd(j *Job) string {
	if j.Kind == "once" {
		return "when it fires"
	}
	if j.Forever != 0 {
		return "never (--forever)"
	}
	var parts []string
	if j.Until.Valid {
		parts = append(parts, "at "+humanize(timePtr(j.Until.String)))
	}
	if j.UntilDone != 0 {
		parts = append(parts, fmt.Sprintf("when the command exits %d", doneExit))
	}
	if len(parts) == 0 {
		return "never (scheduled before ends were required)"
	}
	return strings.Join(parts, ", or ")
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,48}$`)

func validName(name string) string {
	if !nameRe.MatchString(name) {
		die("bad job name %q: lowercase letters, digits, - and _ only "+
			"(it names the job's runs and its log)", name)
	}
	return name
}

var unsafeNameChars = regexp.MustCompile(`[^a-z0-9_-]+`)

func defaultName(command string) string {
	base := "job"
	if f := strings.Fields(command); len(f) > 0 {
		base = filepath.Base(strings.Trim(f[0], `'"`))
	}
	base = strings.Trim(unsafeNameChars.ReplaceAllString(strings.ToLower(base), "-"), "-")
	if base == "" {
		base = "job"
	}
	return base + "-" + now().Format("0102150405") // UTC, as the Python version did
}

func provenance() string {
	if sid := os.Getenv("CLAUDE_CODE_SESSION_ID"); sid != "" {
		if len(sid) > 8 {
			sid = sid[:8]
		}
		return "claude:" + sid
	}
	if u := os.Getenv("USER"); u != "" {
		return u
	}
	return "unknown"
}

// guardOthers: inside a Claude Code session, a verb that would stop or change a
// job refuses one this session didn't book, unless --force says the user asked
// for it. Keyed on the session rather than "booked by an agent", because a
// person's permanent jobs are mostly booked through an agent. A guard against a
// careless agent, not a boundary: --force and --by both get past it.
func guardOthers(j *Job, verb string, force bool) {
	if os.Getenv("CLAUDE_CODE_SESSION_ID") == "" || force || j.CreatedBy.String == provenance() {
		return
	}
	by := j.CreatedBy.String
	if by == "" {
		by = "someone unrecorded"
	}
	die("%s was booked by %s, %s, not by this session; not going to %s it.\n"+
		"  If the user asked for this job by name, repeat with --force.",
		j.Name, by, humanize(timePtr(j.CreatedAt)), verb)
}

// joinCommand: one argument after `--` is a shell line, verbatim, so pipes and
// && survive. Several are an argv, quoted safely. Both end up as a string for
// the login shell.
func joinCommand(parts []string) string {
	if len(parts) == 1 {
		return parts[0]
	}
	q := make([]string, len(parts))
	for i, p := range parts {
		q[i] = shellQuote(p)
	}
	return strings.Join(q, " ")
}

var shellSafe = regexp.MustCompile(`^[A-Za-z0-9_@%+=:,./-]+$`)

// shellQuote matches Python's shlex.quote, so a command stored by either
// implementation reads the same.
func shellQuote(s string) string {
	if s == "" {
		return "''"
	}
	if shellSafe.MatchString(s) {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'"'"'`) + "'"
}

// truncate cuts to n characters, not bytes, so a multi-byte character is never split.
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// The process checks, kept here (not in platform_*.go) so their parsing is
// tested on every platform.

// startSlack is how much later than its run's start a process may have begun
// and still be that run's: exec is started, then writes the run. Anything that
// began later is a later run's.
const startSlack = 5 * time.Second

// isExecOf says whether a command line is the exec of a run of this job, as
// dispatch starts it on either platform (`<binary> exec <name> --due ...`).
// The binary's own name isn't relied on: a release runs from a versioned path,
// and anyone may rename it.
func isExecOf(args, name string) bool {
	return regexp.MustCompile(`(^|\s)exec ` + regexp.QuoteMeta(name) + ` --due `).MatchString(args)
}

// parseLock reads a run's lock file: exec's pid, then the command's process
// group once it has started (0 until then). Only the first line counts: the
// file is rewritten in place, so for a moment it can hold the end of an older,
// longer line after the newline.
func parseLock(data []byte) (pid, pgid int) {
	line, _, _ := strings.Cut(string(data), "\n")
	f := strings.Fields(line)
	if len(f) > 0 {
		pid, _ = strconv.Atoi(f[0])
	}
	if len(f) > 1 {
		pgid, _ = strconv.Atoi(f[1])
	}
	return max(pid, 0), max(pgid, 0)
}

// parseEtime reads ps's elapsed time, [[dd-]hh:]mm:ss.
func parseEtime(s string) (time.Duration, bool) {
	s = strings.TrimSpace(s)
	days := 0
	if d, rest, ok := strings.Cut(s, "-"); ok {
		n, err := strconv.Atoi(d)
		if err != nil {
			return 0, false
		}
		days, s = n, rest
	}
	parts := strings.Split(s, ":")
	if len(parts) < 2 || len(parts) > 3 {
		return 0, false
	}
	total := 0
	for _, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 {
			return 0, false
		}
		total = total*60 + n
	}
	return time.Duration(days*86400+total) * time.Second, true
}

// psSaysAlive reads `ps -o etime=,args= -p <pid>` for a run that started at
// started: alive only while it is still this job's exec, begun no later than
// the run.
func psSaysAlive(out, name string, started, at time.Time) bool {
	out = strings.TrimSpace(out)
	et, args, _ := strings.Cut(out, " ")
	el, ok := parseEtime(et)
	if !ok || !isExecOf(strings.TrimSpace(args), name) {
		return false
	}
	return !at.Add(-el).After(started.Add(startSlack))
}

// unitSaysAlive reads `systemctl show -p ActiveState -p ActiveEnterTimestamp
// --timestamp=unix` for a run that started at started: alive while the unit is
// up and became active for this run, not a later one (the unit's name is the
// job's, shared by all its runs).
func unitSaysAlive(out string, started time.Time) bool {
	state, enter := "", ""
	for _, l := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(l), "=")
		switch k {
		case "ActiveState":
			state = v
		case "ActiveEnterTimestamp":
			enter = v
		}
	}
	switch state {
	case "active", "activating", "deactivating", "reloading", "refreshing":
	default:
		return false
	}
	if n, err := strconv.ParseInt(strings.TrimPrefix(enter, "@"), 10, 64); err == nil && strings.HasPrefix(enter, "@") {
		return !time.Unix(n, 0).After(started.Add(startSlack))
	}
	return true // no time to compare: the unit is up
}
