package main

// The grammars are checked two ways. Differential tests run every spec through
// the real tool (systemd-analyze, GNU date) and fail on any disagreement; they
// skip where the tool is missing (macOS). Fixed-answer tests pin the cases
// that matter most so they run everywhere.

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
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

// The subset of `date -d` that `at` reads. Phrases relative to now are run
// against the same second on both sides (retrying if the clock ticks in
// between); phrases that GNU reads differently on purpose are in
// TestWhenDeliberateDifferences instead.
func TestWhenAgainstGNUDate(t *testing.T) {
	if out, err := exec.Command("date", "--version").Output(); err != nil || !strings.Contains(string(out), "GNU") {
		t.Skip("GNU date not available here; the fixed-answer tests still run")
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
	zones := []string{"America/New_York", "UTC", "Asia/Kolkata", "Pacific/Kiritimati", "Pacific/Pago_Pago"}
	for _, zone := range zones {
		loc, _ := time.LoadLocation(zone)
		for _, s := range corpus {
			for attempt := 0; ; attempt++ {
				base := time.Now().Truncate(time.Second)
				cmd := exec.Command("date", "-d", s, "+%s")
				cmd.Env = append(os.Environ(), "TZ="+zone, "LC_ALL=C")
				out, err := cmd.Output()
				if time.Now().Truncate(time.Second) != base && attempt < 3 {
					continue // the second ticked over between the two readings
				}
				got, perr := parseWhenAt(s, base, loc)
				if err != nil {
					if perr == nil {
						t.Errorf("%q TZ=%s: GNU refuses it, we read %s", s, zone, got.In(loc))
					}
				} else {
					secs, _ := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
					want := time.Unix(secs, 0)
					if perr != nil {
						t.Errorf("%q TZ=%s: GNU reads %s, we refuse: %v", s, zone, want.In(loc), perr)
					} else if !got.Equal(want) && !(crossesDST(base, want, loc) && dayUnitsOnly(s)) {
						t.Errorf("%q TZ=%s: GNU %s, we %s", s, zone, want.In(loc), got.In(loc))
					}
				}
				break
			}
		}
	}
}

func crossesDST(a, b time.Time, loc *time.Location) bool {
	_, oa := a.In(loc).Zone()
	_, ob := b.In(loc).Zone()
	return oa != ob
}

// dayUnitsOnly: a bare relative phrase in days or longer, which keeps the wall
// clock here and the UTC offset in GNU.
func dayUnitsOnly(s string) bool {
	for _, f := range strings.Fields(strings.ToLower(s)) {
		if u, ok := relUnits[f]; ok && (u == "s" || u == "min" || u == "h") {
			return false
		}
		if clockRe.MatchString(f) || isoDateRe.MatchString(f) {
			return false
		}
	}
	return true
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
