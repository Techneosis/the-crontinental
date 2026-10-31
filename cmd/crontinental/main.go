// Crontinental runs a command later, once or on a repeat, without creating a
// systemd unit for it.
//
// Design, and why it is split this way:
//
// DURABILITY lives in the ledger (SQLite), not in systemd. `systemd-run
// --on-calendar` can schedule a command with no files at all, but the transient
// timer it creates dies with the user manager -- so "run this tomorrow at
// 09:00" silently evaporates at the next reboot. A row survives.
//
// THE HEARTBEAT is one timer (crontinental.timer, 1/min) shared by every job,
// so adding work never means adding a unit, a daemon-reload, or an enable.
// That is the entire point: agents schedule things, and agent-written unit
// files would otherwise pile up in ~/.config/systemd/user, unowned.
//
// EXECUTION is a transient unit per run, via systemd-run. Each job therefore
// gets its own cgroup, its own journal stream (`journalctl --user -u
// crontinental-job-<name>`), and its own RuntimeMaxSec -- none of which a tick
// that ran jobs inline could give it, and any one of which a wedged job would
// otherwise take from every other job. The tick only ever spawns and returns,
// so it stays sub-second no matter what it launched.
//
// EVERY JOB ENDS, and then it goes away. A one-off ends when it fires. A repeat
// has to say how it ends when it is created -- `--until <time>`,
// `--until-done` (the command exits 99 when its work is finished), or
// `--forever` -- and is refused otherwise, so a job that runs forever is a
// choice someone made rather than an agent forgetting to clean up. A finished
// job leaves `list` at once and the tick deletes it retainSec later; until then
// `show` and `log` still work.
package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

type opts struct {
	verb    string
	when    string
	name    string
	command []string

	workdir, timeout, catchup, until, notify, note, by string
	untilDone, forever, now, force                     bool
	tags, noTags                                       []string // --tag, --no-tag: repeatable

	// edit
	every, at, in        string
	noUntil, noUntilDone bool
	notifySet, noteSet   bool

	all, json         bool
	lines             int
	linesSet          bool
	follow            bool
	run               int64
	enabled, disabled bool
	count             int
	days              int
	due               string
	late              int64

	// notify
	text, title string
	urgent      bool

	// upgrade
	check bool
}

const usage = `usage: crontinental <verb> ...

Run a command later, once or on a repeat, without creating a systemd unit for it.

  crontinental in 90m               --name pr-recheck -- gh pr checks 412
  crontinental at "tomorrow 09:00"  --name thaw -- ./scripts/thaw-staging
  crontinental every "Mon..Fri 02:00" --name backup --forever -- restic backup ~/work
  crontinental every 30m --name watch --until "2 hours" -- 'curl -sf https://x/health || exit 1'

verbs:
  in <delay>          run once, after a delay (90m, 2h, 1d; 0 is now)
  at <time>           run once, at a time ("2026-09-15 09:00", "tomorrow 07:00", "now")
  every <spec>        repeat: a calendar spec ("Mon..Fri 07:30", "hourly") or an interval ("30m")
  list [--all] [--json]   what is scheduled (--json: each with its last_run)
  show <name> [--json]   one job, in full, with its recent runs
  log <name> [-n N] [-f]  the job's output, from the journal
  log <name> --run ID     one run's output (the id is in show --json)
  run <name>          fire it now, without moving its schedule
  rm <name>           remove a job
  enable <name>       resume a disabled job
  disable <name>      stop a job without removing it
  edit <name> ...     change a job in place, keeping its history (see below)
  prune [--days N]    drop finished jobs now (the tick does it after 3 days)
  check <spec> [-n N] how a spec reads (calendar, interval or time) and when it fires
  notify [--urgent] [--title T] [--tag K=V] <text>   tell the notify hooks
                      something, now; inside a job it is sent as that job's,
                      with its tags (a --tag here adds to them, for this message)
  messages [-n N] [--json]   every notification sent, newest first (default 20;
                      kept 30 days), hooks or none
  upgrade [--check]   install the latest release, through its installer;
                      --check only says whether there is a newer one (exit 3
                      if there is, 0 if not)
  path                print the ledger path
  version             which build this is

options for in/at/every:
  --name NAME         job name; also its systemd unit name
  --workdir DIR       run here (default: $HOME)
  --timeout SPAN      kill the run after this long (default 1h)
  --catchup run|skip  a fire more than 120s late: run it anyway, or skip to the
                      next one (default: run for one-offs, skip for repeats)
  --until TIME        a repeat ends at this time ("tomorrow 18:00", "3 days")
  --until-done        a repeat ends when the command exits 99
  --forever           a repeat that never ends; required when no end is given
  --now               a repeat also runs once right away, then keeps its schedule
  --notify never|fail|always   (default: fail)
  --note TEXT         why this exists
  --by WHO            who scheduled it (default: the Claude session, or $USER)
  --tag KEY=VALUE     a word for the notify hooks, as CRONTINENTAL_TAG_<KEY>
                      (repeatable; e.g. --tag topic=ops)

edit <name> takes any of the options above except --name, --by and --now, plus:
  --every SPEC        a repeat's new schedule
  --at TIME, --in SPAN   a one-off's new time
  --no-until, --no-until-done   drop that end (a repeat must keep one)
  --tag KEY=VALUE     add or change a tag;  --no-tag KEY   remove one
  -- COMMAND          a new command
Naming an end drops --forever; --forever drops both ends.

Inside a Claude Code session, rm, disable and edit refuse a job that session
didn't book unless given --force (an edit of only --note/--notify is let
through). It keeps a tidying agent off jobs it doesn't own; it is not a lock.

A repeat must end: --until <time>, --until-done (the command exits
99 when finished), or --forever. Finished jobs are deleted after 3 days.

Everything after ` + "`--`" + ` is the command. One argument is taken as a
shell line (pipes and && work); several are taken as an argv.
A job can send its own word with "crontinental notify" at any point: the
command runs with CRONTINENTAL_JOB set, so the message carries the job. That
goes out whatever --notify says, which only governs the end-of-run reports.

Every job runs in a login shell (bash -lc on Linux, your own login shell on
macOS), so PATH is the one your terminal has.
`

// usageErr is argparse's behaviour for a malformed command line: exit 2.
func usageErr(verb, format string, args ...any) {
	prog := "crontinental"
	if verb != "" {
		prog += " " + verb
	}
	fmt.Fprintf(os.Stderr, "usage: %s ... (crontinental --help)\n%s: error: %s\n", prog, prog, fmt.Sprintf(format, args...))
	os.Exit(2)
}

type flagSpec struct {
	takesValue bool
	choices    []string
}

var addFlags = map[string]flagSpec{
	"--name": {true, nil}, "--workdir": {true, nil}, "--timeout": {true, nil},
	"--catchup": {true, []string{"run", "skip"}}, "--until": {true, nil},
	"--until-done": {false, nil}, "--forever": {false, nil},
	"--notify": {true, []string{"never", "fail", "always"}},
	"--note":   {true, nil}, "--by": {true, nil},
	"--now": {false, nil}, "--tag": {true, nil},
}

var verbFlags = map[string]map[string]flagSpec{
	"in": addFlags, "at": addFlags, "every": addFlags,
	"list": {"--all": {false, nil}, "--json": {false, nil}},
	"show": {"--json": {false, nil}}, "run": {}, "rm": {"--force": {false, nil}}, "enable": {},
	"disable": {"--force": {false, nil}},
	"log":     {"--lines": {true, nil}, "--follow": {false, nil}, "--run": {true, nil}},
	"prune":   {"--days": {true, nil}},
	"tick":    {}, "path": {},
	"edit":     editFlags,
	"check":    {"--count": {true, nil}, "--lines": {true, nil}},
	"exec":     {"--due": {true, nil}, "--late": {true, nil}},
	"names":    {"--enabled": {false, nil}, "--disabled": {false, nil}},
	"notify":   {"--urgent": {false, nil}, "--title": {true, nil}, "--tag": {true, nil}},
	"messages": {"--lines": {true, nil}, "--json": {false, nil}},
	"upgrade":  {"--check": {false, nil}},
}

var editFlags = func() map[string]flagSpec {
	m := map[string]flagSpec{
		"--every": {true, nil}, "--at": {true, nil}, "--in": {true, nil},
		"--no-until": {false, nil}, "--no-until-done": {false, nil},
		"--force": {false, nil}, "--no-tag": {true, nil},
	}
	for k, v := range addFlags {
		if k != "--name" && k != "--by" && k != "--now" {
			m[k] = v
		}
	}
	return m
}()

var shortFlags = map[string]string{"-n": "--lines", "-f": "--follow"}

// repeatable flags collect every value given, rather than the last one
var repeatable = map[string]bool{"--tag": true, "--no-tag": true}

// positionals each verb needs, by name
var verbArgs = map[string][]string{
	"in": {"when"}, "at": {"when"}, "every": {"when"}, "check": {"when"},
	"show": {"name"}, "log": {"name"}, "run": {"name"}, "rm": {"name"},
	"enable": {"name"}, "disable": {"name"}, "exec": {"name"}, "edit": {"name"},
	"notify": {"text"},
}

func parse(argv []string) *opts {
	o := &opts{notify: "fail", lines: 50}
	// Split the command off BEFORE parsing. Collecting "the rest" from the first
	// positional onward would swallow --name and friends along with it;
	// splitting on the first literal `--` keeps the option/command boundary
	// exactly where the user typed it, and leaves any further `--` inside the
	// command untouched.
	for i, a := range argv {
		if a == "--" {
			o.command = argv[i+1:]
			argv = argv[:i]
			break
		}
	}
	if len(argv) == 0 {
		usageErr("", "the following arguments are required: verb")
	}
	if argv[0] == "--version" || argv[0] == "version" {
		fmt.Println(versionString())
		os.Exit(0)
	}
	if argv[0] == "-h" || argv[0] == "--help" || argv[0] == "help" {
		fmt.Print(usage)
		os.Exit(0)
	}
	o.verb = argv[0]
	flags, ok := verbFlags[o.verb]
	if !ok {
		usageErr("", "invalid choice: %q (choose from in, at, every, list, show, edit, log, run, rm, enable, disable, prune, check, notify, messages, upgrade, path)", o.verb)
	}

	var pos []string
	set := map[string]string{}
	lists := map[string][]string{}
	bools := map[string]bool{}
	rest := argv[1:]
	for i := 0; i < len(rest); i++ {
		a := rest[i]
		if a == "-h" || a == "--help" {
			fmt.Print(usage)
			os.Exit(0)
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		key, val, hasVal := strings.Cut(a, "=")
		if long, ok := shortFlags[key]; ok {
			key = long
		}
		spec, ok := flags[key]
		if !ok {
			usageErr(o.verb, "unrecognized arguments: %s", a)
		}
		if !spec.takesValue {
			if hasVal {
				usageErr(o.verb, "argument %s: ignored explicit argument %q", key, val)
			}
			bools[key] = true
			continue
		}
		if !hasVal {
			if i+1 >= len(rest) {
				usageErr(o.verb, "argument %s: expected one argument", key)
			}
			i++
			val = rest[i]
		}
		if spec.choices != nil && !contains(spec.choices, val) {
			usageErr(o.verb, "argument %s: invalid choice: %q (choose from %s)", key, val,
				"'"+strings.Join(spec.choices, "', '")+"'")
		}
		if repeatable[key] {
			lists[key] = append(lists[key], val)
		}
		set[key] = val
	}

	want := verbArgs[o.verb]
	if len(pos) < len(want) {
		usageErr(o.verb, "the following arguments are required: %s", strings.Join(want[len(pos):], ", "))
	}
	if len(pos) > len(want) {
		usageErr(o.verb, "unrecognized arguments: %s", strings.Join(pos[len(want):], " "))
	}
	for i, w := range want {
		switch w {
		case "when":
			o.when = pos[i]
		case "name":
			o.name = pos[i]
		case "text":
			o.text = pos[i]
		}
	}

	if v, ok := set["--name"]; ok {
		o.name = v
	}
	o.workdir, o.timeout, o.catchup = set["--workdir"], set["--timeout"], set["--catchup"]
	o.until, o.note, o.by, o.due = set["--until"], set["--note"], set["--by"], set["--due"]
	if v, ok := set["--notify"]; ok {
		o.notify, o.notifySet = v, true
	}
	_, o.noteSet = set["--note"]
	o.every, o.at, o.in = set["--every"], set["--at"], set["--in"]
	o.noUntil, o.noUntilDone = bools["--no-until"], bools["--no-until-done"]
	o.untilDone, o.forever, o.now = bools["--until-done"], bools["--forever"], bools["--now"]
	o.all, o.json, o.follow = bools["--all"], bools["--json"], bools["--follow"]
	o.enabled, o.disabled = bools["--enabled"], bools["--disabled"]
	o.force = bools["--force"]
	o.tags, o.noTags = lists["--tag"], lists["--no-tag"]
	o.urgent, o.title = bools["--urgent"], set["--title"]
	o.check = bools["--check"]
	if o.enabled && o.disabled {
		usageErr(o.verb, "argument --disabled: not allowed with argument --enabled")
	}
	o.lines = intFlag(o.verb, set, "--lines", 50)
	_, o.linesSet = set["--lines"]
	if o.verb == "messages" && !o.linesSet {
		o.lines = 20
	}
	o.run = int64(intFlag(o.verb, set, "--run", 0))
	o.days = intFlag(o.verb, set, "--days", 0)
	o.count = intFlag(o.verb, set, "--count", 3)
	if o.verb == "check" {
		if v, ok := set["--lines"]; ok { // -n reads naturally here too
			o.count = intFlag(o.verb, map[string]string{"--count": v}, "--count", 3)
		}
	}
	o.late = int64(intFlag(o.verb, set, "--late", 0))
	return o
}

func intFlag(verb string, set map[string]string, key string, dflt int) int {
	v, ok := set[key]
	if !ok {
		return dflt
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		usageErr(verb, "argument %s: invalid int value: %q", key, v)
	}
	return n
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func main() {
	o := parse(os.Args[1:])
	switch o.verb {
	case "in", "at":
		cmdAdd(o, "once")
	case "every":
		cmdAdd(o, "repeat")
	case "list":
		cmdList(o)
	case "show":
		cmdShow(o)
	case "log":
		cmdLog(o)
	case "run":
		cmdRun(o)
	case "rm":
		cmdRm(o)
	case "enable":
		cmdToggle(o, true)
	case "disable":
		cmdToggle(o, false)
	case "edit":
		cmdEdit(o)
	case "prune":
		cmdPrune(o)
	case "tick":
		cmdTick()
	case "exec":
		cmdExec(o)
	case "names":
		cmdNames(o)
	case "notify":
		cmdNotify(o)
	case "messages":
		cmdMessages(o)
	case "check":
		cmdCheck(o)
	case "upgrade":
		cmdUpgrade(o)
	case "path":
		fmt.Println(dbPath())
	}
}
