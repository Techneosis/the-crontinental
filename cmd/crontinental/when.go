package main

// Absolute times for `at` and `--until`, as a person types them. This is a
// documented subset of what GNU `date -d` reads, and on Linux the tests check
// it against `date -d` with GNU's clock pinned. The deliberate differences:
//
//   - Days, weeks, months and years keep the wall-clock time across a daylight
//     saving change ("3 days" from 09:00 is 09:00), the way systemd's calendar
//     does. Hours, minutes and seconds are elapsed time, added after the
//     calendar part: "1 day 2 hours" from 09:00 is 11:00 tomorrow, whatever
//     the clocks did overnight. GNU keeps now's UTC offset for a phrase with
//     no clock, date or weekday in it, so its "1 day" is 24 hours (08:00 the
//     morning after clocks go back) while its "tomorrow 09:00" is 09:00; we
//     read both as 09:00. A delay that must be exact is `in 24h`.
//   - A clock time the spring change skips is refused ("2027-03-14 02:30" in
//     New York; systemd's calendar never fires it). GNU refuses it too, but
//     checks the clock against today before adding days, so its
//     "tomorrow 02:30" is refused on the day of the change and moved to 03:30
//     the day before. A calendar unit with no clock that lands in the gap
//     moves past it, as in GNU.
//   - A clock time the autumn change repeats is the first of the two, as in
//     systemd's calendar; GNU's pick depends on the zone. A clock taken from
//     now ("now", "2 hours") stays on now's side of the change.
//   - A date and a weekday together ("2026-09-15 friday") is refused; GNU
//     silently ignores the weekday. And "+2" after a time is two of something,
//     never a UTC offset.
//
// Supported: now, today, tomorrow, yesterday; 2026-09-15[ 09:00[:30]] and
// 2026-09-15T09:00; 9/15[/2026]; "Oct 24 [2026]" and "24 Oct [2026]"; 07:00,
// 7:30pm, 7 pm, 9; weekday names with next/last/this; [+|-]N unit [ago] and
// next/last unit and a bare unit ("fortnight"), for seconds through years;
// UTC; @<epoch>. An empty time is refused (GNU reads it as midnight).
// Items combine in any order: "next friday 18:00", "07:00 tomorrow".

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var monthNames = []string{"january", "february", "march", "april", "may", "june", "july",
	"august", "september", "october", "november", "december"}

func monthIndex(s string) (time.Month, bool) {
	for i, n := range monthNames {
		if s == n || s == n[:3] || (s == "sept" && i == 8) {
			return time.Month(i + 1), true
		}
	}
	return 0, false
}

var relUnits = map[string]string{
	"sec": "s", "secs": "s", "second": "s", "seconds": "s",
	"min": "min", "mins": "min", "minute": "min", "minutes": "min",
	"hour": "h", "hours": "h",
	"day": "d", "days": "d", "week": "w", "weeks": "w", "fortnight": "f", "fortnights": "f",
	"month": "mo", "months": "mo", "year": "y", "years": "y",
}

var (
	isoDateRe = regexp.MustCompile(`^(\d{4})-(\d{1,2})-(\d{1,2})(?:t(\d{1,2}):(\d{2})(?::(\d{2}))?)?$`)
	usDateRe  = regexp.MustCompile(`^(\d{1,2})/(\d{1,2})(?:/(\d{4}))?$`)
	clockRe   = regexp.MustCompile(`^(\d{1,2}):(\d{2})(?::(\d{2}))?(am|pm)?$`)
	hourAmRe  = regexp.MustCompile(`^(\d{1,2})(am|pm)$`)
	intRe     = regexp.MustCompile(`^[+-]?\d+$`)
)

type whenParts struct {
	hasDate, hasTime, hasDow bool
	y                        int
	m                        time.Month
	d                        int
	h, mi, s                 int
	dow, ord                 int // dow: Go weekday; ord: 0 this/plain, 1 next, -1 last
	relY, relM, relD         int
	relSec                   int64
	lastRel                  func()         // undoes and re-applies the last relative item negated ("ago")
	loc                      *time.Location // the zone the given fields are read in ("UTC" sets it)
	local                    *time.Location // the zone the fields not given come from
	epoch                    *time.Time
}

func whenErr(spec string) error {
	return fmt.Errorf("could not understand the time %q (try \"2026-09-15 09:00\" or \"tomorrow 07:00\")", spec)
}

// parseWhenAt reads spec relative to base, in loc.
func parseWhenAt(spec string, base time.Time, loc *time.Location) (time.Time, error) {
	toks := strings.Fields(strings.ToLower(spec))
	if len(toks) == 0 {
		return time.Time{}, whenErr(spec)
	}
	p := &whenParts{loc: loc, local: loc}
	bad := func() (time.Time, error) { return time.Time{}, whenErr(spec) }

	for i := 0; i < len(toks); i++ {
		t := toks[i]
		peek := func(k int) string {
			if i+k < len(toks) {
				return toks[i+k]
			}
			return ""
		}
		switch {
		case t == "now" || t == "today":
		case t == "tomorrow":
			p.addRel("d", 1)
		case t == "yesterday":
			p.addRel("d", -1)
		case t == "utc":
			p.loc = time.UTC
		case t == "ago":
			if p.lastRel == nil {
				return bad()
			}
			p.lastRel()
			p.lastRel = nil
		case t == "next" || t == "last" || t == "this":
			ord := map[string]int{"next": 1, "last": -1, "this": 0}[t]
			if wd, ok := weekdayGo(peek(1)); ok {
				if !p.setDow(wd, ord) {
					return bad()
				}
				i++
			} else if u, ok := relUnits[peek(1)]; ok {
				p.addRel(u, ord)
				i++
			} else {
				return bad()
			}
		case strings.HasPrefix(t, "@") && len(toks) == 1:
			secs, err := strconv.ParseInt(t[1:], 10, 64)
			if err != nil {
				return bad()
			}
			e := time.Unix(secs, 0).UTC()
			p.epoch = &e
		default:
			if wd, ok := weekdayGo(t); ok {
				if !p.setDow(wd, 0) {
					return bad()
				}
				continue
			}
			if u, ok := relUnits[t]; ok { // a bare unit is one of it: "fortnight", "week ago"
				p.addRel(u, 1)
				continue
			}
			if mo, ok := monthIndex(t); ok { // "oct 24 [2026]"
				d, err := strconv.Atoi(peek(1))
				if err != nil || p.hasDate {
					return bad()
				}
				i++
				y := base.In(loc).Year()
				if yy := peek(1); len(yy) == 4 && allDigits(yy) {
					y, _ = strconv.Atoi(yy)
					i++
				}
				p.setDate(y, mo, d)
				continue
			}
			if m := isoDateRe.FindStringSubmatch(t); m != nil {
				if p.hasDate {
					return bad()
				}
				p.setDate(atoi(m[1]), time.Month(atoi(m[2])), atoi(m[3]))
				if m[4] != "" {
					if !p.setTime(atoi(m[4]), atoi(m[5]), atoi(m[6])) {
						return bad()
					}
				}
				continue
			}
			if m := usDateRe.FindStringSubmatch(t); m != nil {
				if p.hasDate {
					return bad()
				}
				y := base.In(loc).Year()
				if m[3] != "" {
					y = atoi(m[3])
				}
				p.setDate(y, time.Month(atoi(m[1])), atoi(m[2]))
				continue
			}
			if m := clockRe.FindStringSubmatch(t); m != nil {
				h := atoi(m[1])
				mer := m[4]
				if mer == "" && (peek(1) == "am" || peek(1) == "pm") {
					mer = peek(1)
					i++
				}
				if h, ok := meridiem(h, mer); !ok || !p.setTime(h, atoi(m[2]), atoi(m[3])) {
					return bad()
				}
				continue
			}
			if m := hourAmRe.FindStringSubmatch(t); m != nil {
				if h, ok := meridiem(atoi(m[1]), m[2]); !ok || !p.setTime(h, 0, 0) {
					return bad()
				}
				continue
			}
			if intRe.MatchString(t) {
				n, _ := strconv.Atoi(t)
				if u, ok := relUnits[peek(1)]; ok { // "3 days", "+90 minutes"
					p.addRel(u, n)
					i++
					continue
				}
				if mo, ok := monthIndex(peek(1)); ok && !strings.ContainsAny(t, "+-") { // "24 oct [2026]"
					if p.hasDate {
						return bad()
					}
					i++
					y := base.In(loc).Year()
					if yy := peek(1); len(yy) == 4 && allDigits(yy) {
						y, _ = strconv.Atoi(yy)
						i++
					}
					p.setDate(y, mo, n)
					continue
				}
				if len(t) <= 2 && !strings.ContainsAny(t, "+-") { // "9", "friday 9", "7 pm"
					mer := ""
					if peek(1) == "am" || peek(1) == "pm" {
						mer = peek(1)
						i++
					}
					if h, ok := meridiem(n, mer); !ok || !p.setTime(h, 0, 0) {
						return bad()
					}
					continue
				}
			}
			return bad()
		}
	}
	// A zone and nothing else is midnight in it, as in GNU (where "now" and
	// "today" are relative items, so "now UTC" keeps the clock).
	zoneOnly := true
	for _, t := range toks {
		zoneOnly = zoneOnly && t == "utc"
	}
	if zoneOnly {
		p.hasTime = true
	}
	return p.resolve(spec, base)
}

func atoi(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

func weekdayGo(s string) (time.Weekday, bool) {
	if i, ok := weekdayIndex(s); ok {
		return time.Weekday((i + 1) % 7), true
	}
	return 0, false
}

// meridiem turns a 12-hour clock into 24; without am/pm the hour stands.
func meridiem(h int, mer string) (int, bool) {
	switch mer {
	case "":
		return h, h <= 23
	case "am":
		if h < 1 || h > 12 {
			return 0, false
		}
		return h % 12, true
	default:
		if h < 1 || h > 12 {
			return 0, false
		}
		return h%12 + 12, true
	}
}

func (p *whenParts) setDate(y int, m time.Month, d int) {
	p.hasDate, p.y, p.m, p.d = true, y, m, d
}

func (p *whenParts) setTime(h, mi, s int) bool {
	if p.hasTime || h > 23 || mi > 59 || s > 59 {
		return false
	}
	p.hasTime, p.h, p.mi, p.s = true, h, mi, s
	return true
}

func (p *whenParts) setDow(wd time.Weekday, ord int) bool {
	if p.hasDow {
		return false
	}
	p.hasDow, p.dow, p.ord = true, int(wd), ord
	return true
}

func (p *whenParts) addRel(unit string, n int) {
	apply := func(n int) {
		switch unit {
		case "s":
			p.relSec += int64(n)
		case "min":
			p.relSec += int64(n) * 60
		case "h":
			p.relSec += int64(n) * 3600
		case "d":
			p.relD += n
		case "w":
			p.relD += 7 * n
		case "f":
			p.relD += 14 * n
		case "mo":
			p.relM += n
		case "y":
			p.relY += n
		}
	}
	apply(n)
	p.lastRel = func() { apply(-2 * n) } // "ago": take it back, then the other way
}

func (p *whenParts) resolve(spec string, base time.Time) (time.Time, error) {
	if p.epoch != nil {
		return *p.epoch, nil
	}
	if p.hasDate && p.hasDow {
		return time.Time{}, fmt.Errorf("the time %q names both a date and a weekday; give one", spec)
	}
	// GNU's rule for a named zone: the fields the spec leaves out (the date,
	// the weekday, the clock) are the local ones, and only the result is read
	// in that zone. Taking them from the zone's own now picks the wrong day
	// whenever the two dates differ.
	b := base.In(p.local)
	y, m, d := b.Date()
	h, mi, s := b.Clock()
	if p.hasDate {
		y, m, d = p.y, p.m, p.d
		if p.d < 1 || p.d > daysIn(p.y, p.m) || p.m < 1 || p.m > 12 {
			return time.Time{}, whenErr(spec)
		}
	}
	if (p.hasDate || p.hasDow) && !p.hasTime {
		h, mi, s = 0, 0, 0
	}
	if p.hasTime {
		h, mi, s = p.h, p.mi, p.s
	}
	if p.hasDow {
		// GNU's rule: the named weekday on or after today; next/last move it a
		// week, except that "next friday" on a Thursday is tomorrow.
		wd := int(b.Weekday())
		off := (p.dow - wd + 7) % 7
		adj := p.ord
		if p.ord > 0 && wd != p.dow {
			adj--
		}
		d += off + 7*adj
	}
	// Calendar arithmetic on the date (time.Date normalizes Jan 31 + 1 month
	// the way mktime does), keeping the wall-clock time.
	n := time.Date(y+p.relY, m+time.Month(p.relM), d+p.relD, 12, 0, 0, 0, time.UTC)
	y, m, d = n.Date()
	t, ok := wallTime(y, m, d, h, mi, s, p.loc)
	if ok && !p.hasTime && !p.hasDate && !p.hasDow {
		// The clock came from now. In the repeated hour, stay on now's side of
		// the change: "now" at 01:30 EST is not 01:30 EDT, an hour ago.
		_, off := b.Zone()
		if c := time.Date(y, m, d, h, mi, s, 0, time.FixedZone("", off)); sameWall(c.In(p.loc), y, m, d, h, mi, s) {
			t = c
		}
	}
	if !ok {
		if p.hasTime {
			return time.Time{}, fmt.Errorf("%q doesn't exist: the clocks skip it", spec)
		}
		// A day landing in the skipped hour moves past it, by the size of the
		// gap: the clock read at the offset before the change. (time.Date
		// leaves the choice unspecified, and in practice goes back.)
		_, off := time.Date(y, m, d, h, mi, s, 0, p.loc).Add(-12 * time.Hour).Zone()
		t = time.Date(y, m, d, h, mi, s, 0, time.FixedZone("", off))
	}
	return t.Add(time.Duration(p.relSec) * time.Second).UTC(), nil
}
