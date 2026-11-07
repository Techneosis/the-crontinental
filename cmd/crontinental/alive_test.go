package main

import (
	"testing"
	"time"
)

func TestIsExecOf(t *testing.T) {
	for _, c := range []struct {
		args, name string
		want       bool
	}{
		{"/Users/a/.local/bin/crontinental exec backup --due 2026-01-01T00:00:00+00:00 --late 0", "backup", true},
		{"/Users/a/.local/share/crontinental/dist/v0.11.0/build/crontinental exec backup --due x --late 0", "backup", true},
		{"/opt/x y/cronto exec backup --due x --late 0", "backup", true}, // renamed, path with a space
		{"/usr/bin/vim exec-backup --due", "backup", false},
		{"/bin/sh -c echo exec backup", "backup", false},          // no --due: not dispatch's
		{"/x/crontinental exec backup2 --due x", "backup", false}, // another job
		{"/x/crontinental exec a.b --due x", "axb", false},        // the name is literal
		{"/x/crontinental exec backup", "backup", false},          // by hand, not a dispatch
		{"", "backup", false},
	} {
		if got := isExecOf(c.args, c.name); got != c.want {
			t.Errorf("isExecOf(%q, %q) = %v, want %v", c.args, c.name, got, c.want)
		}
	}
}

func TestParseLock(t *testing.T) {
	for _, c := range []struct {
		in        string
		pid, pgid int
	}{
		{"123\n", 123, 0},
		{"123 456\n", 123, 456},
		{"99 7\n45 678\n", 99, 7}, // the tail of an older, longer line
		{"99\n45 678\n", 99, 0},
		{"", 0, 0},
		{"garbage\n", 0, 0},
		{"-5 -9\n", 0, 0},
	} {
		if pid, pgid := parseLock([]byte(c.in)); pid != c.pid || pgid != c.pgid {
			t.Errorf("parseLock(%q) = %d %d, want %d %d", c.in, pid, pgid, c.pid, c.pgid)
		}
	}
}

func TestParseEtime(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"00:05": 5 * time.Second, "12:34": 754 * time.Second, "01:00:00": time.Hour,
		"2-03:04:05": 2*24*time.Hour + 3*time.Hour + 4*time.Minute + 5*time.Second, "  00:07 ": 7 * time.Second,
	} {
		if got, ok := parseEtime(in); !ok || got != want {
			t.Errorf("parseEtime(%q) = %v %v, want %v", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "5", "a:b", "1:2:3:4", "x-01:00"} {
		if _, ok := parseEtime(bad); ok {
			t.Errorf("parseEtime(%q) accepted", bad)
		}
	}
}

func TestPsSaysAlive(t *testing.T) {
	at := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	started := at.Add(-10 * time.Minute)
	line := " 10:02 /x/crontinental exec j --due x --late 0"
	if !psSaysAlive(line, "j", started, at) {
		t.Error("the run's own exec read as dead")
	}
	if psSaysAlive(" 00:30 /x/crontinental exec j --due x --late 0", "j", started, at) {
		t.Error("a later run's exec, begun after this run, read as alive")
	}
	if psSaysAlive(" 10:02 /usr/bin/python3 server.py", "j", started, at) {
		t.Error("a process that reused the pid read as alive")
	}
}

func TestUnitSaysAlive(t *testing.T) {
	started := time.Unix(1_800_000_000, 0)
	for _, c := range []struct {
		out  string
		want bool
	}{
		{"ActiveState=active\nActiveEnterTimestamp=@1799999999\n", true},
		{"ActiveState=active\nActiveEnterTimestamp=@1800000004\n", true},  // inside the slack
		{"ActiveState=active\nActiveEnterTimestamp=@1800000600\n", false}, // a later run's unit
		{"ActiveState=inactive\nActiveEnterTimestamp=\n", false},
		{"ActiveState=failed\nActiveEnterTimestamp=@1799999999\n", false},
		{"ActiveState=active\nActiveEnterTimestamp=Sat 2026-11-07 12:00:00 UTC\n", true}, // can't compare
	} {
		if got := unitSaysAlive(c.out, started); got != c.want {
			t.Errorf("unitSaysAlive(%q) = %v, want %v", c.out, got, c.want)
		}
	}
}
