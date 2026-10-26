# Crontinental: how it works

*The house rules are in the [README](../README.md); this is the building
plan.*

Run a command later — once, or on a repeat — without creating a systemd unit
for it. Built so an agent can schedule work mid-session and have it survive the
session, the logout, and the reboot.

```
crontinental in 90m  --name pr-recheck -- gh pr checks 412
crontinental at "tomorrow 07:00" --name thaw -- ~/bin/thaw-staging
crontinental every "Mon..Fri 07:30" --name backup --forever -- restic backup ~/work
crontinental every 30m --name health --until "2 hours" -- 'curl -sf https://x/health || exit 1'
crontinental every 10m --name deploy-wait --until-done --until "tomorrow" -- 'deploy-is-live && exit 99'
```

## Why not just `systemd-run --on-calendar`?

Because it forgets. `systemd-run` creates a *transient* timer, and transient
units die with the user manager — so a job scheduled for tomorrow morning
evaporates at the next reboot, silently, with nothing to show that it was ever
scheduled. On a rolling-release distro that is not a rare event.

An agent's own scheduling forgets the same way, sooner. `/loop`, Claude Code's
cron and monitor tools, and any poll an agent runs itself live inside the
session: when it ends, so do they, and a promised check-in an hour out just
never happens. They suit work that only matters while the session is open;
anything that has to outlast it goes in the ledger.

Claude's cloud scheduler (the `schedule` skill's routines) does persist, but
it runs on Anthropic's servers, not this machine: no local files, tools,
devices or `localhost` servers. It suits work that lives entirely in the
cloud; anything that touches this machine is Crontinental's.

So the three concerns are split deliberately:

| concern | owner |
|---|---|
| durability | the job table (`~/.local/share/crontinental/ledger.db`) |
| heartbeat | one shared timer, `crontinental.timer`, 1/min |
| execution | a transient unit per run, via `systemd-run` |

The tick never *runs* a job. It reads the table, finds what is due, and launches
each one as `crontinental-job-<name>.service`, then returns — measured at
~44ms. That is what buys per-job cgroups, per-job journal streams, and per-job
timeouts without a single per-job file, and what guarantees one wedged job can
neither delay the tick nor starve another job.

## The command

Everything after `--` is the command.

- **One argument** is taken as a shell line, verbatim — pipes, `&&`, redirects
  and `$(…)` all work: `-- 'make build 2>&1 | tail -40'`
- **Several arguments** are taken as an argv and quoted safely:
  `-- gh pr checks 412`

Every job runs under `bash -lc`, so its PATH is the login shell's: nvm, mise,
`~/.local/bin`, whatever your profile adds. This is not cosmetic. systemd's own
PATH has none of those, which is the trap that makes a node-touching command
work by hand and fail only when scheduled.

One version of that trap survives the login shell. If your profile puts mise's
shims ahead of nvm and mise has no global node, `node`/`npx` resolve to a shim
that refuses to run ("No version is set for shim"). Fix it inside the job (put
the node you want first on PATH), not with a global default.

## Schedules

`crontinental check "<spec>"` says how any spec reads (calendar, interval or
time) and when it would fire next.

`in` takes a systemd time span (`90m`, `2h 30min`, `1.5h`, `1d`; case matters,
`M` is a month and `m` a minute).

**Now** is a time like any other: `in 0` and `at now` are accepted (anything
earlier than the moment of booking is refused). A one-off due now does not
wait up to a minute for the tick: it is written to the ledger already finished
(no `next_run`, the same state the tick's advance leaves it in) and dispatched
on the spot, so the tick has nothing to fire a second time. `edit --in 0` on a
waiting one-off does the same. `every … --now` is booking plus `run`: one
off-schedule run straight away, with the first scheduled run left where the
spec put it.

`at` and `--until` take a time as a person types it: `2026-09-15 09:00`,
`2026-09-15T09:00`, `9/15/2026`, `Oct 24 2026 10:00`, `24 Oct`, `07:00`,
`7:30pm`, `7 pm`, `today`, `tomorrow 07:00`, `friday 18:00`, `next friday`,
`last monday`, `3 days`, `2 hours ago`, `1 day 2 hours`, `fortnight`,
`10:00 UTC`, `@<epoch>`, in any order. It is a subset of GNU `date -d`, with two
deliberate differences: days, weeks, months and years keep the wall-clock time
across a daylight-saving change (GNU keeps the UTC offset), and a date with a
weekday ("2026-09-15 friday") is refused rather than silently dropping the
weekday.

`every` takes systemd's own calendar grammar, so it is the same grammar as
every systemd timer. Crontinental parses it itself (one implementation on every
platform, macOS included), and the tests hold that parser to `systemd-analyze`:
a hand-written corpus across five time zones and both daylight-saving changes,
and a hundred thousand random specs. Where the two disagreed, every remaining
case turned out to be a systemd bug in repeats ("01/13:00" skips 01:00 after a
day boundary, though "01,14:00" doesn't), which the tests detect and log. One
omission: fractional seconds (`12:30:15.5`) are refused; the tick runs once a
minute.

```
03:00               daily at 3am
Mon..Fri 07:30      weekday mornings
*-*-01 09:00        the 1st of every month
hourly              every hour on the hour
```

**`daily 03:00` is not valid** — `daily` already means midnight and takes no
time. Write `03:00`.

`every` also accepts a plain interval (`30m`, `4h`), which repeats that long
after each run rather than landing on wall-clock boundaries. Anything under one
minute is refused: the tick is the resolution.

## Every job ends

A one-off ends when it fires. **A repeat has to say how it ends** when it is
created, or it is refused:

| flag | ends |
|---|---|
| `--until <time>` | at a deadline (`"tomorrow 18:00"`, `"3 days"`; see Schedules) |
| `--until-done` | when the command exits **99**: the job itself decides its work is finished |
| `--forever` | never — the only way to get a permanent job, so it is always a choice |

`--until` and `--until-done` combine (stop at whichever comes first), which is
the right shape for polling: "until it's live, but give up tomorrow".
`--forever` contradicts both.

Exit 99 is honoured on every job, declared or not, and counts as success: the
run is recorded as `done`, nothing notifies (except `--notify always`), and the
unit exits 0. Not 100 — apt-get exits 100 on error, and a failing job must
never look finished.

This exists because agents schedule things and then their session ends. Before
it, the way to make a repeat stop was a second one-off whose command was
`crontinental rm <first>`; now the end is part of the job.

Jobs that existed before ends were required were marked `--forever` by the
schema migration, so nothing that was running stopped.

## What happens when the box was asleep

Due times live in the table, so a tick missed while suspended is not a lost job
— the first tick after resume finds everything that came due. Each job then
applies its own `--catchup`:

- **`run`** (default for one-offs) — fire it anyway, late. What you want from
  "remind me at 09:00" when the lid opened at 09:40.
- **`skip`** (default for repeats) — let it go, wait for the next slot. What you
  want from a nightly job when the machine wakes at noon. This matches systemd's
  own default for a missed timer (`Persistent=false`).

A fire is "late" past 120s; under that it is just an on-time fire landing inside
its tick. Either way a repeat reschedules forward from **now**, never from the
slot it missed — so a day asleep costs at most one run of each job, never a
backlog queued up behind itself.

## Overlap, timeouts, failure

**Overlap is impossible by construction.** A run's transient unit is named after
the job, so systemd itself refuses a second copy while the first is running.
There is no lockfile to go stale. The refusal is recorded as an `overlap` run.

**Every job has a timeout**, 1h by default, `--timeout 20m` to change it. It is
enforced as `RuntimeMaxSec` on the transient unit, and the kill is caught and
written down (`killed`, exit 143) rather than leaving a run row open forever.

**Failures notify**, with the job name, its exit code, and the command to read
its log. `--notify always` also announces successes; `--notify never` stays
quiet. That is all the binary decides: *when*. *Where* is hooks, the
executables in `~/.config/crontinental/notify.d/` (`CRONTINENTAL_NOTIFY_DIR`
overrides it; the tests use that). Each gets the event as `CRONTINENTAL_*`
variables and as JSON on stdin, they run side by side, and each has 10s. A hook
that fails, hangs or is missing never changes a run's outcome; its output and
its failure go to the run's log. The setting is a directory, not an environment
variable, because the tick and the runs start from the service manager, which
never reads a shell profile.

Nothing about notification is built in. `install.sh` copies `notify/desktop`
(notify-send, or Omarchy's sender where it exists; osascript on macOS) into the
directory on a first install and never touches it again, so one emptied on
purpose stays empty; `show` says when a job would notify and nobody is wired to
hear it. `notify/examples/` has ntfy and a webhook.

A job can also speak for itself: `crontinental notify [--urgent] [--title T]
<text>` sends a `message` event to the same hooks. exec runs every command with
`CRONTINENTAL_JOB` set, so a message from inside a run carries the job (its
note, its log command); from anywhere else the job is empty. A message goes out
whatever `--notify` says, since `--notify` governs only the end-of-run reports,
and it never touches the run's outcome. No hooks is said on stderr but exits 0,
so a script under `set -e` doesn't die of it.

A run has exactly one ending. On Linux a stop (`RuntimeMaxSec`, `rm`) signals
`exec` and the command together, so the signal handler and the command's exit
race to record it; a lock lets the first one write it down and notify, and a
command that died of SIGTERM in a stopped unit counts as `killed` either way.

## Reading what happened

```
crontinental list          what is scheduled, next run, last outcome
crontinental list --all    including finished and disabled jobs
crontinental show <name>   one job in full, with its last 10 runs
crontinental log <name>    its output, from the journal (-f to follow)
crontinental run <name>    fire it now WITHOUT moving its schedule
crontinental edit <name>   change it in place (see below)
```

A job's output is never captured into the database — it streams to the journal
under the job's own unit, so `log` has exactly one source of truth and a chatty
job doesn't bloat the table.

`show` also prints who scheduled it and why: `created_by` is filled in
automatically from `CLAUDE_CODE_SESSION_ID` when an agent schedules something,
so a job nobody remembers can be traced back to the session that wanted it.
That matters here — this is a persistent execution channel that agents can write
to, and `list` is the audit of it.

## macOS

The same design, with launchd for the heartbeat and exec doing what transient
units do on Linux:

- **The tick** is a LaunchAgent (`launchd/com.techneosis.crontinental.plist`,
  written by `install.sh` with absolute paths, since launchd takes no `~`):
  `StartInterval` 60, `RunAtLoad`. A tick missed while asleep isn't replayed;
  the first one after waking finds what came due, as on Linux.
- **A run** is `crontinental exec`, started by the tick in a session of its own
  (`setsid`), so launchd's cleanup of the tick's process group never reaches
  it; `AbandonProcessGroup` in the plist is the backstop.
- **Overlap** is an `flock` on `<ledger dir>/locks/<name>.lock`, held for the
  whole run. The kernel releases it when exec exits however it exits, so it
  cannot go stale. The file records exec's pid and the command's process group.
- **The timeout** is exec's: SIGTERM to the command's process group, SIGKILL
  ten seconds later, recorded as `killed` (143) exactly as on Linux.
- **`rm` mid-run** sends exec SIGTERM (it stops the group and records the
  kill), waits for the lock, and kills the group itself if exec doesn't go.
- **Output** goes to `~/Library/Logs/crontinental/<name>.log`, which
  Console.app shows, with a line marking the start and end of each run. A log
  past 1MiB is rotated to `.1` before the next run; `log` reads both.
- **The shell** is the user's login shell as Directory Services records it
  (`dscl`): a LaunchAgent's environment has no `SHELL`, and on macOS the shell
  is usually zsh, whose profile is where Homebrew lands on PATH.
- **Notifications** are the same hooks; the default `desktop` hook posts to
  Notification Center through `osascript`, and critical ones play a sound.

## Changing a job

`edit <name>` changes a job in place, so its run history and `created_by`
survive. It takes the booking options (not `--name`, which is the unit name,
`--by`, or `--now`, which is what `run` is for), plus `--every` for a repeat's schedule, `--at`/`--in` for a one-off's
time, `--no-until`/`--no-until-done`, and a new command after `--`.

Ends compose: naming an end drops `--forever`, `--forever` drops both ends, and
the result must still satisfy the booking rule, so a repeat can't be edited
into having no end. A refused edit changes nothing. A new schedule recomputes
the next run from now. Enabling and disabling stay with `enable`/`disable`; an
edit to a finished job says so and leaves it finished.

## Housekeeping

A job tears itself down. When it can never run again — a one-off fired, a
deadline passed, or the command exited 99 — it leaves `list` at once (`list
--all` shows it as `done`, `show` says when it will go) and the tick deletes it,
with its run history, **3 days later**. Its output stays in the journal after
that. A run still in progress holds its job back.

`enable` on a finished repeat takes it off that clock. `disable` is different
from finishing: a disabled job is kept until someone removes it.
`prune [--days N]` deletes finished jobs now instead of waiting.
`rm <name>` stops a running instance and deletes the job.

## Files

- `cmd/crontinental/`: the whole thing, in Go; one static binary
- `systemd/crontinental.{timer,service}`: the tick
- `completions/crontinental.bash`: tab completion for verbs and job names
- `notify/`: the default desktop hook, and `examples/` for other places
- `install.sh`: links the three into `~/.local/bin`, `~/.config/systemd/user`
  and bash-completion's user directory, then starts the timer
- `tests/`: the suite (`python3 -m unittest discover tests`), black-box over the CLI
- `~/.local/share/crontinental/ledger.db`: jobs and run history
  (`CRONTINENTAL_DB` overrides it, which is how the tests run without touching
  the real one)
