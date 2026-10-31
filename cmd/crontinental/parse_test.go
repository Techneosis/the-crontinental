package main

// The grammars are checked two ways. Differential tests run every spec through
// the real tool (systemd-analyze, GNU date) and fail on any disagreement; they
// skip where the tool is missing (macOS). Fixed-answer tests pin the cases
// that matter most so they run everywhere.

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

var zones = []string{"UTC", "America/New_York", "Europe/Berlin", "Asia/Kolkata", "Australia/Lord_Howe"}

// Base times include both daylight-saving changes in New York and Berlin.
var bases = []string{
	"2026-10-23 09:13:32 UTC", // an ordinary Friday morning
	"2026-10-25 00:30:00 UTC", // Berlin falls back at 01:00 UTC
	"2026-11-01 05:20:00 UTC", // New York: 01:20 EDT, just before falling back
	"2026-11-01 06:58:30 UTC", // New York: 01:58:30 EST, inside the repeated hour
	"2027-03-14 06:59:00 UTC", // New York: 01:59 EST, just before springing forward
	"2027-03-28 00:30:00 UTC", // Berlin springs forward at 01:00 UTC
	"2027-12-31 23:59:59 UTC", // the year rolls over
	"2028-02-28 12:00:00 UTC", // a leap year
}

var calendarCorpus = []string{
	// every live job's spec on the machine this was written on
	"*-*-* 06:30", "Mon 08:00", "*-*-02 08:00",
	// the docs' examples
	"03:00", "Mon..Fri 07:30", "*-*-01 09:00", "hourly", "Mon..Fri 02:00",
	"minutely", "daily", "weekly", "monthly", "yearly", "annually", "quarterly",
	"semiannually", "semi-annually", "Daily", "HOURLY",
	"*:0/15", "*:30", "*-*-1/2", "Mon *-05~07/1", "*-02~01", "*-*~01", "*-*~03..05",
	"26-10-30 10:00", "0-10-30", "Sat,Sun", "mon..wed,fri 9:5", "mon-fri 08:00",
	"2026-02-30", "2020-01-01", "2026-*-* 00:00", "Mon,Mon", "1..3:00", "1..3/2:00",
	"*-1..3/2-01", "Mon..Sun", "*:*:5/10", "*-*-31", "*-*-* *:*:*", "*-*-* *:*",
	"1:2:3", "00:00/5", "4/4:00", "02/2:00", "2026-1-1", "Mon  08:00", "*-*-*  08:00",
	"Mon 08:00 UTC", "03:00 Europe/Berlin", "daily UTC", "*:00 utc", "02:30", "01:30",
	"*:15", "*:*:00", "Fri 23:59:59", "*-12-31 23:59:59", "*-02-29 12:00",
	"2027,2028-02-29", "*-*-* 0,12:00", "Tue,Thu *-*-8..14 10:00", "*-*-29..31 00:00",
	"*-01..12-01..07 Mon 00:00", "Wed..Sat,Tue 12-10-15 1:2:3", "*-*-* 24:00",
	"@1800000000", "@1700000000",
	// refusals
	"*:*/15", "*/2:00", "Fri..Mon", "9", "*-*-* 7", "Mon *", "* *:*", " 03:00 ",
	"03:00 03:00", "Mon Tue", "Mon, Tue 08:00", "Mon..Wed..Fri", "*:*:60", "*-2/0-01",
	"01:02:03.", "daily 03:00", "+1h", "tomorrow", "30m", "", "*", "Funday",
	"03:00 Nowhere/Land", "25:00", "*-13-01", "*-*-32", "1969-12-31",
}

func haveTool(t *testing.T, name string, args ...string) {
	t.Helper()
	if err := exec.Command(name, args...).Run(); err != nil {
		t.Skipf("%s not available here; the fixed-answer tests still run", name)
	}
}

var (
	oracleUTCRe     = regexp.MustCompile(`\(in UTC\):\s*\w{3} (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}) UTC`)
	oracleElapseRe  = regexp.MustCompile(`Next elapse:\s*\w{3} (\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}) UTC`)
	oracleNeverRe   = regexp.MustCompile(`Next elapse:\s*never`)
	oracleNormRe    = regexp.MustCompile(`Normalized form: (.*)`)
	oracleTimespanR = regexp.MustCompile(`(?:μs|us):\s*(\d+)`)
)

// systemdNext asks systemd-analyze for the next elapse: ok=false if it refuses
// the spec, never=true if there is none.
func systemdNext(spec, zone, base string) (next time.Time, norm string, never, ok bool) {
	cmd := exec.Command("systemd-analyze", "calendar", "--iterations=1", "--base-time", base, spec)
	cmd.Env = append(os.Environ(), "TZ="+zone)
	out, err := cmd.CombinedOutput()
	if err != nil {
		// systemd accepted the spec but gave up finding its next elapse. It does
		// that both for specs that never elapse ("*-06/5-31") and, through a
		// bug, for some that do ("*-01/10-* 16:23" reports "Infinite loop in
		// calendar calculation", though "*-01,11-* 16:23" answers fine). No
		// answer to compare against, so the caller skips it.
		if strings.Contains(string(out), "Failed to determine next elapse") {
			return time.Time{}, "", false, true
		}
		return time.Time{}, "", false, false
	}
	if m := oracleNormRe.FindSubmatch(out); m != nil {
		norm = string(m[1])
	}
	if oracleNeverRe.Match(out) {
		return time.Time{}, norm, true, true
	}
	m := oracleUTCRe.FindSubmatch(out)
	if m == nil {
		m = oracleElapseRe.FindSubmatch(out)
	}
	if m == nil {
		panic("unreadable systemd-analyze output: " + string(out))
	}
	t, _ := time.Parse("2006-01-02 15:04:05", string(m[1]))
	return t, norm, false, true
}

func TestCalendarAgainstSystemd(t *testing.T) {
	haveTool(t, "systemd-analyze", "calendar", "daily")
	for _, zone := range zones {
		loc, err := time.LoadLocation(zone)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range calendarCorpus {
			for _, base := range bases {
				want, norm, never, ok := systemdNext(spec, zone, base)
				c, perr := parseCalendar(spec)
				label := fmt.Sprintf("%q TZ=%s base=%s", spec, zone, base)
				if !ok {
					if perr == nil && !strings.Contains(spec, ".") {
						t.Errorf("%s: systemd refuses it, we accept it", label)
					}
					break // a refusal doesn't depend on the base
				}
				if perr != nil {
					t.Errorf("%s: systemd accepts it, we refuse: %v", label, perr)
					break
				}
				if c.loc == nil { // no zone in the spec: the machine's, here the test's
					c.loc = loc
				}
				if !never && want.IsZero() {
					continue // systemd gave up; see systemdNext
				}
				b, _ := time.Parse("2006-01-02 15:04:05 MST", base)
				got := c.next(b)
				switch {
				case never && got != nil:
					t.Errorf("%s: systemd says never, we say %s", label, got)
				case !never && got == nil:
					t.Errorf("%s: systemd says %s, we say never", label, want)
				case !never && !got.Equal(want):
					t.Errorf("%s: systemd says %s, we say %s", label, want, got)
				}
				if norm != "" && c.normalized() != norm {
					t.Errorf("%s: normalized as %q, systemd %q", label, c.normalized(), norm)
				}
			}
		}
	}
}

// Successive elapses, not just the first: the daylight-saving walk is where
// one wrong step hides behind a right first answer.
func TestCalendarSeriesAgainstSystemd(t *testing.T) {
	haveTool(t, "systemd-analyze", "calendar", "daily")
	series := []string{"*:0/15", "*:30", "*:*:00", "01:30", "02:30", "*-*-* 01,02:15", "hourly"}
	for _, zone := range []string{"America/New_York", "Europe/Berlin", "Australia/Lord_Howe"} {
		loc, _ := time.LoadLocation(zone)
		for _, spec := range series {
			for _, base := range bases {
				cmd := exec.Command("systemd-analyze", "calendar", "--iterations=6", "--base-time", base, spec)
				cmd.Env = append(os.Environ(), "TZ="+zone)
				out, err := cmd.Output()
				if err != nil {
					t.Fatalf("%q: %v", spec, err)
				}
				var want []time.Time
				for _, line := range strings.Split(string(out), "\n") {
					if m := oracleUTCRe.FindStringSubmatch(line); m != nil {
						w, _ := time.Parse("2006-01-02 15:04:05", m[1])
						want = append(want, w)
					}
				}
				if len(want) == 0 { // a UTC zone prints no "(in UTC)" lines
					continue
				}
				c, err := parseCalendar(spec)
				if err != nil {
					t.Fatal(err)
				}
				c.loc = loc
				b, _ := time.Parse("2006-01-02 15:04:05 MST", base)
				for i, w := range want {
					got := c.next(b)
					if got == nil || !got.Equal(w) {
						t.Errorf("%q TZ=%s base=%s step %d: systemd %s, we %v", spec, zone, base, i, w, got)
						break
					}
					b = *got
				}
			}
		}
	}
}

func TestTimespanAgainstSystemd(t *testing.T) {
	haveTool(t, "systemd-analyze", "timespan", "1s")
	corpus := []string{"90m", "2h 30min", "1.5h", "2 h", "1h30m", "5", "0", "-5m", "1M",
		"1y", "1w", "100ms", "1us", "1µs", "1 day", "2days", "1h  30m", " 5m", "5m ", "1H",
		"1D", "1Min", "30sec", "30secs", "1hr", "1hrs", "2weeks", "1month", "3months",
		"1.5", ".5h", "1.h", "1e3s", "1m1", "", "1fortnight", "1minutes", "1msec", "1usec",
		"1second", "1seconds", "m", "5 m 5", "1.25d", "0.001s", "2.999s", "3 weeks 2 days",
		"15m", "5m", "1h", "4h", "30m", "20m", "3s", "1d", "7d 1s", "1y 1M 1w 1d 1h 1m 1s"}
	for _, s := range corpus {
		out, err := exec.Command("systemd-analyze", "timespan", s).Output()
		got, perr := parseTimespan(s)
		if err != nil {
			if perr == nil {
				t.Errorf("%q: systemd refuses it, we read %ds", s, got)
			}
			continue
		}
		m := oracleTimespanR.FindSubmatch(out)
		us, _ := strconv.ParseInt(string(m[1]), 10, 64)
		if perr != nil {
			t.Errorf("%q: systemd reads %dus, we refuse: %v", s, us, perr)
		} else if got != us/1_000_000 {
			t.Errorf("%q: systemd %ds, we %ds", s, us/1_000_000, got)
		}
	}
}

// whenBases are the moments the GNU comparison runs at: the calendar bases,
// plus the mornings before each New York change, from which "1 day" lands on
// the other side of it.
var whenBases = append([]string{
	"2026-10-31 13:00:00 UTC", // New York: 09:00 EDT, the day before falling back
	"2026-10-31 06:30:00 UTC", // New York: 02:30 EDT; "1 day 2 hours" passes the repeated hour
	"2027-03-13 14:00:00 UTC", // New York: 09:00 EST, the day before springing forward
	"2027-03-13 07:30:00 UTC", // New York: 02:30 EST; "1 day" lands in the skipped hour
}, bases...)

// fakeNowSource is an LD_PRELOAD library that tells GNU date that now is
// $FAKE_NOW (epoch seconds). date has no option for its base time, and a
// comparison run against the real clock only meets a daylight-saving change on
// the two days a year it happens to run near one.
const fakeNowSource = `#define _GNU_SOURCE
#include <stdlib.h>
#include <time.h>
#include <sys/time.h>
static time_t fake(void) { const char *s = getenv("FAKE_NOW"); return s ? (time_t)atoll(s) : 0; }
int clock_gettime(clockid_t c, struct timespec *ts) { (void)c; ts->tv_sec = fake(); ts->tv_nsec = 0; return 0; }
int gettimeofday(struct timeval *tv, void *tz) { (void)tz; tv->tv_sec = fake(); tv->tv_usec = 0; return 0; }
time_t time(time_t *t) { if (t) *t = fake(); return fake(); }
`

// gnuDateAt returns a function that runs `date -d spec` in zone with now
// pinned to base, or why it can't be arranged here (no GNU date, no C
// compiler, not Linux, or a date that ignores the library).
func gnuDateAt(t *testing.T) (func(spec, zone string, base time.Time) (time.Time, error), string) {
	t.Helper()
	if out, err := exec.Command("date", "--version").Output(); err != nil || !strings.Contains(string(out), "GNU") {
		return nil, "GNU date not available here"
	}
	if runtime.GOOS != "linux" {
		return nil, "pinning GNU date's clock needs LD_PRELOAD"
	}
	dir := t.TempDir()
	src, lib := filepath.Join(dir, "fakenow.c"), filepath.Join(dir, "fakenow.so")
	if err := os.WriteFile(src, []byte(fakeNowSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("cc", "-shared", "-fPIC", "-o", lib, src).CombinedOutput(); err != nil {
		return nil, fmt.Sprintf("no C compiler to pin GNU date's clock (%v: %s)", err, out)
	}
	run := func(spec, zone string, base time.Time) (time.Time, error) {
		cmd := exec.Command("date", "-d", spec, "+%s")
		cmd.Env = append(os.Environ(), "TZ="+zone, "LC_ALL=C", "LD_PRELOAD="+lib,
			"FAKE_NOW="+strconv.FormatInt(base.Unix(), 10))
		out, err := cmd.Output()
		if err != nil {
			return time.Time{}, err
		}
		secs, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
		return time.Unix(secs, 0), err
	}
	probe := time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC)
	if got, err := run("now", "UTC", probe); err != nil || !got.Equal(probe) {
		return nil, fmt.Sprintf("GNU date ignored the pinned clock (got %v, %v)", got, err)
	}
	return run, ""
}

// The subset of `date -d` that `at` reads, against GNU date with its clock
// pinned to each of whenBases, so the answer doesn't depend on today's date.
// The one deliberate difference that shows up here, a calendar unit across a
// daylight-saving change, is allowed only where GNU's answer is exactly its
// own rule (gnuKeepsOffset); TestWhenAcrossDST pins our answers for it.
func TestWhenAgainstGNUDate(t *testing.T) {
	gnu, why := gnuDateAt(t)
	if gnu == nil && runtime.GOOS == "linux" && os.Getenv("CI") != "" {
		t.Fatal(why + ": Linux CI is where this comparison runs, so it must not skip there")
	}
	if gnu == nil {
		t.Skip(why + "; the fixed-answer tests still run")
	}
	corpus := []string{"now", "today", "tomorrow", "yesterday", "07:00", "7:00", "7am", "7pm",
		"7:30pm", "12am", "12pm", "19:00:30", "tomorrow 07:00", "07:00 tomorrow", "friday",
		"next friday", "last friday", "this friday", "fri", "next fri 18:00", "friday 18:00",
		"monday", "next monday", "last sunday", "sat 9am", "3 days", "2 hours", "90 minutes",
		"1 hour ago", "2 hours ago", "+90 minutes", "+2 days", "1 day 2 hours", "3 days ago",
		"1 day 2 hours ago", "2 hours 1 day ago", "2026-09-15", "2026-09-15 09:00",
		"2026-09-15T09:00", "2026-09-15 9:00", "next week", "last week", "next day", "next hour",
		"tomorrow tomorrow", "7:30 pm", "7 pm", "0:00", "9", "2026-09-15 9", "friday 9",
		"1 days", "1 mins", "2 secs", "+1 day", "-1 day", "Friday", "FRIDAY", "next Friday 6pm",
		"today 23:59", "mon 10:00", "10/24/2026", "10/24", "Oct 24", "Oct 24 2026 10:00",
		"24 Oct", "24 october 2027", "@1800000000", "2026-10-24 10:00 UTC", "10:00 UTC",
		"UTC", "now UTC", "tomorrow 10:00 UTC", "friday UTC", "Oct 24 UTC", "3 days UTC",
		"thursday next week", "2026-11-01 01:30", "2026-10-31 07:00 1 day", "2027-01-31 1 month",
		"2028-02-29 1 year", "fortnight", "2 fortnights", "1 week ago", "week", "day", "hour",
		"month", "year", "minute", "second", "next fortnight", "week ago", "tomorrow week",
		// refusals
		"noon", "midnight", "in 2 hours", "13pm", "24:00", "2026-10-23 25:00", "1 hr",
		"1.5 hours", "ago", "friday friday", "2027-03-14 02:30", "garbage"}
	// +14 and -11 keep a zone on a different date from UTC at every hour, so a
	// date taken from the wrong zone fails here whenever the test runs.
	// Berlin and New York change on different dates, both inside whenBases.
	zones := []string{"America/New_York", "Europe/Berlin", "UTC", "Asia/Kolkata", "Pacific/Kiritimati", "Pacific/Pago_Pago"}
	for _, zone := range zones {
		loc, _ := time.LoadLocation(zone)
		for _, bs := range whenBases {
			base, _ := time.Parse("2006-01-02 15:04:05 MST", bs)
			for _, s := range corpus {
				want, err := gnu(s, zone, base)
				got, perr := parseWhenAt(s, base, loc)
				label := fmt.Sprintf("%q TZ=%s now=%s", s, zone, base.In(loc).Format("2006-01-02 15:04:05 MST"))
				switch {
				case err != nil && perr == nil:
					t.Errorf("%s: GNU refuses it, we read %s", label, got.In(loc))
				case err != nil:
				case perr != nil:
					t.Errorf("%s: GNU reads %s, we refuse: %v", label, want.In(loc), perr)
				case !got.Equal(want):
					if alt, ok := gnuKeepsOffset(s, base, loc); !ok || !alt.Equal(want) {
						t.Errorf("%s: GNU %s, we %s", label, want.In(loc), got.In(loc))
					}
				}
			}
		}
	}
}

// gnuKeepsOffset is GNU's reading where it differs from ours on purpose: a
// phrase made only of relative items, at least one of them a day or longer
// ("1 day", "tomorrow", "1 day 2 hours ago"). GNU adds the days keeping the
// UTC offset now has, which is our reading in a zone fixed at that offset. A
// clock, a date or a weekday in the phrase make GNU keep the wall clock, as we
// always do, so those get no allowance. With UTC named ("3 days UTC") GNU
// still moves the answer by the local change, which UTC itself doesn't have.
func gnuKeepsOffset(s string, base time.Time, loc *time.Location) (time.Time, bool) {
	calendar, utc := false, false
	for _, f := range strings.Fields(strings.ToLower(s)) {
		switch u, unit := relUnits[f]; {
		case f == "tomorrow" || f == "yesterday":
			calendar = true
		case unit:
			calendar = calendar || (u != "s" && u != "min" && u != "h")
		case f == "utc":
			utc = true
		case f == "now" || f == "today" || f == "ago" || f == "next" || f == "last" || f == "this" ||
			intRe.MatchString(f):
		default:
			return time.Time{}, false
		}
	}
	if !calendar {
		return time.Time{}, false
	}
	_, off := base.In(loc).Zone()
	t, err := parseWhenAt(s, base, time.FixedZone("now", off))
	if utc && err == nil {
		_, then := t.In(loc).Zone()
		t = t.Add(time.Duration(then-off) * time.Second)
	}
	return t, err == nil
}

func mustLoc(t *testing.T, name string) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func at(t *testing.T, loc *time.Location, s string) time.Time {
	v, err := time.ParseInLocation("2006-01-02 15:04:05", s, loc)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

// Fixed answers: these run on every platform, macOS included.
func TestCalendarFixed(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	cases := []struct{ spec, base, want string }{
		{"Mon..Fri 07:30", "2026-10-23 05:00:00", "2026-10-23 07:30:00"},
		{"Mon..Fri 07:30", "2026-10-23 07:30:00", "2026-10-26 07:30:00"}, // strictly after
		{"*:0/15", "2026-10-23 05:16:00", "2026-10-23 05:30:00"},
		{"Mon *-05~07/1", "2026-10-23 00:00:00", "2027-05-31 00:00:00"},
		{"*-02~01", "2027-03-01 00:00:00", "2028-02-29 00:00:00"},
		{"02:30", "2027-03-13 12:00:00", "2027-03-15 02:30:00"}, // skipped hour
		{"01:30", "2026-10-31 12:00:00", "2026-11-01 01:30:00"}, // repeated hour, first only
		{"weekly", "2026-10-23 12:00:00", "2026-10-26 00:00:00"},
	}
	for _, c := range cases {
		cal, err := parseCalendar(c.spec)
		if err != nil {
			t.Fatalf("%q: %v", c.spec, err)
		}
		cal.loc = ny
		got := cal.next(at(t, ny, c.base))
		if want := at(t, ny, c.want); got == nil || !got.Equal(want) {
			t.Errorf("%q after %s: want %s, got %v", c.spec, c.base, want, got)
		}
	}
	// the repeated hour runs once: 01:15 EDT, then 02:15 EST
	cal, _ := parseCalendar("*:15")
	cal.loc = ny
	first := cal.next(at(t, ny, "2026-11-01 00:20:00"))
	second := cal.next(*first)
	if first.Sub(at(t, ny, "2026-11-01 00:20:00")) != 55*time.Minute || second.Sub(*first) != 2*time.Hour {
		t.Errorf("repeated hour: got %s then %s", first.In(ny), second.In(ny))
	}
	// systemd itself can't answer these (see systemdNext), so pin them here.
	berlin := mustLoc(t, "Europe/Berlin")
	cal, _ = parseCalendar("*-01/10-* 16..21:23,44")
	cal.loc = berlin
	if got, want := cal.next(at(t, berlin, "2026-12-15 05:57:37")), at(t, berlin, "2027-01-01 16:23:00"); got == nil || !got.Equal(want) {
		t.Errorf("month repeat: want %s, got %v", want, got)
	}
	cal, _ = parseCalendar("*-06/5-31")
	if got := cal.next(at(t, ny, "2026-10-23 00:00:00")); got != nil {
		t.Errorf("the 31st of June or November: want never, got %s", got)
	}
	for _, bad := range []string{"daily 03:00", "*:*/15", "Fri..Mon", "12:30:15.5", " 03:00",
		"*-*~29", "*-*~01,26", "*:42/60", "*:*:05..05"} {
		if _, err := parseCalendar(bad); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
}

func TestTimespanFixed(t *testing.T) {
	for s, want := range map[string]int64{"90m": 5400, "2h 30min": 9000, "1.5h": 5400,
		"1m1": 61, "5": 5, "1M": 2629800, "1y": 31557600, "100ms": 0} {
		if got, err := parseTimespan(s); err != nil || got != want {
			t.Errorf("%q: want %d, got %d (%v)", s, want, got, err)
		}
	}
	for _, bad := range []string{"", "1H", "-5m", "1.h", "1hrs", "m"} {
		if _, err := parseTimespan(bad); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
}

func TestWhenFixed(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	base := at(t, ny, "2026-10-23 05:13:32") // a Friday
	cases := map[string]string{
		"tomorrow 07:00":    "2026-10-24 07:00:00",
		"friday":            "2026-10-23 00:00:00",
		"next friday 18:00": "2026-10-30 18:00:00",
		"last friday":       "2026-10-16 00:00:00",
		"thursday":          "2026-10-29 00:00:00",
		"next thursday":     "2026-10-29 00:00:00",
		"1 day 2 hours ago": "2026-10-24 03:13:32",
		"7:30 pm":           "2026-10-23 19:30:00",
		"24 oct 2027":       "2027-10-24 00:00:00",
		"3 days":            "2026-10-26 05:13:32",
		// deliberate: the wall clock holds across the November change
		"2 weeks": "2026-11-06 05:13:32",
	}
	for s, w := range cases {
		got, err := parseWhenAt(s, base, ny)
		if want := at(t, ny, w); err != nil || !got.Equal(want) {
			t.Errorf("%q: want %s, got %s (%v)", s, want, got.In(ny), err)
		}
	}
	for _, bad := range []string{"2026-09-15 friday", "1h", "noon", "2027-03-14 02:30", "friday friday"} {
		if got, err := parseWhenAt(bad, base, ny); err == nil {
			t.Errorf("%q: accepted as %s", bad, got.In(ny))
		}
	}
}

// A named zone reads the given fields in that zone, but the fields it does not
// give (the date, the weekday, a clock with no time) are the local ones, as in
// GNU. These answers are GNU's, taken at a moment when the two dates differ,
// which the differential test only sees for part of each day.
func TestWhenZoneKeepsTheLocalDate(t *testing.T) {
	ist := mustLoc(t, "Asia/Kolkata")
	base := at(t, ist, "2026-10-27 03:01:48") // a Tuesday; still Monday in UTC
	cases := map[string]string{
		"10:00 UTC":           "2026-10-27 10:00:00",
		"UTC":                 "2026-10-27 00:00:00",
		"now UTC":             "2026-10-27 03:01:48",
		"tomorrow UTC":        "2026-10-28 03:01:48",
		"tomorrow 10:00 UTC":  "2026-10-28 10:00:00",
		"tuesday UTC":         "2026-10-27 00:00:00",
		"wednesday 10:00 UTC": "2026-10-28 10:00:00",
		"2026-10-24 UTC":      "2026-10-24 00:00:00",
		"3 days UTC":          "2026-10-30 03:01:48",
		"1 hour UTC":          "2026-10-27 04:01:48",
		"10:00 UTC 1 day":     "2026-10-28 10:00:00",
	}
	for s, w := range cases {
		got, err := parseWhenAt(s, base, ist)
		if want := at(t, time.UTC, w); err != nil || !got.Equal(want) {
			t.Errorf("%q: want %s UTC, got %s (%v)", s, w, got.UTC(), err)
		}
	}
}

func TestWhenDeliberateDifferences(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	base := at(t, ny, "2026-10-31 05:00:00")
	// GNU reads an empty time as midnight; here it is a mistake.
	if _, err := parseWhenAt("", base, ny); err == nil {
		t.Error("an empty time was accepted")
	}
	// GNU reads "+2" after a time as a UTC offset; here it is two days.
	got, err := parseWhenAt("2026-10-31 05:00 +2 days", base, ny)
	if want := at(t, ny, "2026-11-02 05:00:00"); err != nil || !got.Equal(want) {
		t.Errorf("want %s, got %s (%v)", want, got.In(ny), err)
	}
}

// Daylight-saving changes, pinned. These run everywhere and don't depend on
// today's date. want is our answer ("" refuses); gnu is GNU date 9.11's
// answer at the same moment where it differs ("refused" if it refuses), recorded for comparison and
// re-checked against GNU itself wherever TestWhenAgainstGNUDate can run.
//
// The rules: a calendar unit (day, week, month, year, "tomorrow") keeps the
// wall clock; hours, minutes and seconds are elapsed time, added after the
// calendar part. A clock time the spring change skips is refused; one the
// autumn change repeats is the first of the two, as in systemd's calendar,
// unless the clock comes from now, which stays on now's side of the change.
func TestWhenAcrossDST(t *testing.T) {
	cases := []struct{ zone, base, spec, want, gnu string }{
		// New York falls back at 02:00 EDT on 2026-11-01.
		{"America/New_York", "2026-10-31 09:00:00 EDT", "1 day", "2026-11-01 09:00:00 EST", "2026-11-01 08:00:00 EST"},
		{"America/New_York", "2026-10-31 09:00:00 EDT", "tomorrow", "2026-11-01 09:00:00 EST", "2026-11-01 08:00:00 EST"},
		{"America/New_York", "2026-10-31 09:00:00 EDT", "1 week", "2026-11-07 09:00:00 EST", "2026-11-07 08:00:00 EST"},
		{"America/New_York", "2026-10-31 09:00:00 EDT", "24 hours", "2026-11-01 08:00:00 EST", ""},
		{"America/New_York", "2026-10-31 09:00:00 EDT", "1 day 2 hours", "2026-11-01 11:00:00 EST", "2026-11-01 10:00:00 EST"},
		{"America/New_York", "2026-10-31 09:00:00 EDT", "1 day 2 hours ago", "2026-11-01 07:00:00 EST", "2026-11-01 06:00:00 EST"},
		{"America/New_York", "2026-10-31 09:00:00 EDT", "tomorrow 09:00", "2026-11-01 09:00:00 EST", ""},
		{"America/New_York", "2026-10-31 09:00:00 EDT", "sunday", "2026-11-01 00:00:00 EDT", ""},
		{"America/New_York", "2026-10-31 09:00:00 EDT", "3 days UTC", "2026-11-03 04:00:00 EST", "2026-11-03 03:00:00 EST"},
		// the shape CI met: the day lands past the repeated hour
		{"America/New_York", "2026-10-31 02:30:00 EDT", "1 day 2 hours", "2026-11-01 04:30:00 EST", "2026-11-01 03:30:00 EST"},
		{"America/New_York", "2026-10-31 02:30:00 EDT", "1 day 2 hours ago", "2026-11-01 01:30:00 EDT", "2026-11-01 00:30:00 EDT"},
		// a day landing in the repeated hour takes the first pass
		{"America/New_York", "2026-10-31 01:30:00 EDT", "1 day", "2026-11-01 01:30:00 EDT", ""},
		// the repeated hour's second pass: now's clock stays on its side
		{"America/New_York", "2026-11-01 01:30:00 EST", "now", "2026-11-01 01:30:00 EST", ""},
		{"America/New_York", "2026-11-01 01:30:00 EST", "2 hours", "2026-11-01 03:30:00 EST", ""},
		{"America/New_York", "2026-11-01 01:30:00 EST", "next hour", "2026-11-01 02:30:00 EST", ""},
		{"America/New_York", "2026-11-01 01:30:00 EST", "1 hour ago", "2026-11-01 01:30:00 EDT", ""},
		{"America/New_York", "2026-11-01 01:30:00 EST", "1 day ago", "2026-10-31 01:30:00 EDT", "2026-10-31 02:30:00 EDT"},
		// a clock that is given, in the repeated hour: the first pass
		{"America/New_York", "2026-11-01 01:50:00 EST", "01:30", "2026-11-01 01:30:00 EDT", ""},
		{"America/New_York", "2026-10-31 12:00:00 EDT", "2026-11-01 01:30", "2026-11-01 01:30:00 EDT", ""},
		{"America/New_York", "2026-10-31 12:00:00 EDT", "tomorrow 01:30", "2026-11-01 01:30:00 EDT", ""},

		// New York springs forward at 02:00 EST on 2027-03-14.
		{"America/New_York", "2027-03-13 09:00:00 EST", "1 day", "2027-03-14 09:00:00 EDT", "2027-03-14 10:00:00 EDT"},
		{"America/New_York", "2027-03-13 09:00:00 EST", "24 hours", "2027-03-14 10:00:00 EDT", ""},
		{"America/New_York", "2027-03-13 09:00:00 EST", "1 day 2 hours", "2027-03-14 11:00:00 EDT", "2027-03-14 12:00:00 EDT"},
		{"America/New_York", "2027-03-13 09:00:00 EST", "1 day 2 hours ago", "2027-03-14 07:00:00 EDT", "2027-03-14 08:00:00 EDT"},
		{"America/New_York", "2027-03-13 09:00:00 EST", "tomorrow 09:00", "2027-03-14 09:00:00 EDT", ""},
		// a day landing in the skipped hour moves past it
		{"America/New_York", "2027-03-13 02:30:00 EST", "1 day", "2027-03-14 03:30:00 EDT", ""},
		{"America/New_York", "2027-03-13 02:30:00 EST", "1 day 2 hours", "2027-03-14 05:30:00 EDT", ""},
		// a clock that is given, in the skipped hour: refused. GNU checks the
		// clock against today before it adds the day, so it refuses
		// "tomorrow 02:30" on the day of the change and moves it on the day
		// before.
		{"America/New_York", "2027-03-13 12:00:00 EST", "2027-03-14 02:30", "", ""},
		{"America/New_York", "2027-03-14 00:10:00 EST", "02:30", "", ""},
		{"America/New_York", "2027-03-13 12:00:00 EST", "tomorrow 02:30", "", "2027-03-14 03:30:00 EDT"},
		{"America/New_York", "2027-03-13 12:00:00 EST", "02:30 tomorrow", "", "2027-03-14 03:30:00 EDT"},
		{"America/New_York", "2027-03-14 00:10:00 EST", "tomorrow 02:30", "2027-03-15 02:30:00 EDT", "refused"},

		// Berlin falls back at 03:00 CEST on 2026-10-25. GNU takes the
		// second pass of this repeated hour, though the first of New York's.
		{"Europe/Berlin", "2026-10-24 09:00:00 CEST", "1 day 2 hours", "2026-10-25 11:00:00 CET", "2026-10-25 10:00:00 CET"},
		{"Europe/Berlin", "2026-10-24 09:00:00 CEST", "2026-10-25 02:30", "2026-10-25 02:30:00 CEST", "2026-10-25 02:30:00 CET"},

		// Lord Howe springs forward half an hour at 02:00 on 2026-10-04.
		{"Australia/Lord_Howe", "2026-10-03 02:15:00 +1030", "1 day", "2026-10-04 02:45:00 +1100", ""},
		{"Australia/Lord_Howe", "2026-10-03 09:00:00 +1030", "1 day", "2026-10-04 09:00:00 +1100", "2026-10-04 09:30:00 +1100"},

		// no daylight saving: a day is 24 hours
		{"Asia/Kolkata", "2026-10-31 09:00:00 IST", "1 day", "2026-11-01 09:00:00 IST", ""},
		{"Asia/Kolkata", "2026-10-31 09:00:00 IST", "1 day 2 hours", "2026-11-01 11:00:00 IST", ""},
		{"Asia/Kolkata", "2026-10-31 09:00:00 IST", "1 day 2 hours ago", "2026-11-01 07:00:00 IST", ""},
	}
	gnu, _ := gnuDateAt(t)
	read := func(loc *time.Location, s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04:05 MST", s, loc)
		if err != nil { // a zone with no abbreviation ("+1030")
			v, err = time.ParseInLocation("2006-01-02 15:04:05 -0700", s, loc)
		}
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	for _, c := range cases {
		loc := mustLoc(t, c.zone)
		base := read(loc, c.base)
		label := fmt.Sprintf("%q TZ=%s now=%s", c.spec, c.zone, c.base)
		got, err := parseWhenAt(c.spec, base, loc)
		switch {
		case c.want == "" && err == nil:
			t.Errorf("%s: want refused, got %s", label, got.In(loc).Format(time.DateTime+" MST"))
		case c.want != "" && err != nil:
			t.Errorf("%s: want %s, refused: %v", label, c.want, err)
		case c.want != "" && !got.Equal(read(loc, c.want)):
			t.Errorf("%s: want %s, got %s", label, c.want, got.In(loc).Format(time.DateTime+" MST"))
		}
		gnuWant := c.want
		if c.gnu == "refused" {
			gnuWant = ""
		} else if c.gnu != "" {
			gnuWant = c.gnu
			// where GNU differs by keeping now's offset, that rule alone accounts for it
			if alt, ok := gnuKeepsOffset(c.spec, base, loc); ok && !alt.Equal(read(loc, c.gnu)) {
				t.Errorf("%s: GNU's %s is not its offset rule, which gives %s", label, c.gnu, alt.In(loc).Format(time.DateTime+" MST"))
			}
		}
		if gnu != nil {
			g, gerr := gnu(c.spec, c.zone, base)
			switch {
			case gnuWant == "" && gerr == nil:
				t.Errorf("%s: recorded GNU refusing, GNU reads %s", label, g.In(loc).Format(time.DateTime+" MST"))
			case gnuWant != "" && (gerr != nil || !g.Equal(read(loc, gnuWant))):
				t.Errorf("%s: recorded GNU %s, GNU gives %s (%v)", label, gnuWant, g.In(loc).Format(time.DateTime+" MST"), gerr)
			}
		}
	}
}
