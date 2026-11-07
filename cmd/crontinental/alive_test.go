package main

import "testing"

func TestIsExecOf(t *testing.T) {
	for _, c := range []struct {
		args, name string
		want       bool
	}{
		{"/Users/a/.local/bin/crontinental exec backup --due 2026-01-01T00:00:00+00:00 --late 0", "backup", true},
		{"crontinental exec backup", "backup", true},
		{"/opt/x y/crontinental exec backup --late 0", "backup", true},
		{"/usr/bin/vim notes-exec backup", "backup", false},           // the words, not the binary
		{"/bin/sh -c echo crontinental-exec backup", "backup", false}, // not crontinental
		{"/x/crontinental exec backup2 --late 0", "backup", false},    // another job
		{"/x/notcrontinental exec backup", "backup", false},
		{"/x/crontinental exec a.b", "axb", false}, // the name is literal
		{"", "backup", false},
	} {
		if got := isExecOf(c.args, c.name); got != c.want {
			t.Errorf("isExecOf(%q, %q) = %v, want %v", c.args, c.name, got, c.want)
		}
	}
}
