---
name: crontinental
description: Schedule a command to run later ON THIS MACHINE with Crontinental (`crontinental`) — once ("in 90 minutes", "at 7am tomorrow", "after the build") or recurring ("every weekday morning", "every 30 minutes", "nightly"). Use whenever work must happen after the current session ends, or on a repeat: "remind me to", "check back in an hour", "run this every morning", "retry that tonight", "keep polling until", "schedule a script", "set up a cron job", "crontab", "make this recurring", "run it on a timer". This is NOT the `schedule` skill (cloud routines, which cannot run anything on this machine), NOT `/loop` and NOT CronCreate (both last only as long as the session). Covers the CLI, how a job ends, schedule syntax, what happens when the machine sleeps, and reading a job's output.
---

# Scheduling a command on this machine

**Crontinental** (`crontinental`, on PATH) runs a command later, once or on a
repeat, and survives the session, a logout and a reboot. Every job ends, and a
finished job removes itself.

## Pick the right tool first

| the ask | use |
|---|---|
| run a command or script on **this machine**, later or on a repeat | **`crontinental`** (this skill) |
| run a **Claude session in the cloud** on a cron | the `schedule` skill (routines) |
| repeat a prompt **for the rest of this session** | `/loop`, or CronCreate |

If the thing to run is a shell command, a script, a CLI, or anything that
touches this machine's files, devices or local servers, it is this skill. A
cloud routine cannot reach any of that, and a session-scoped loop stops the
moment the session does.

## Usage

```
crontinental in 90m --name pr-recheck -- gh pr checks 412
crontinental at "tomorrow 07:00" --name thaw -- ./scripts/thaw-staging
crontinental every "Mon..Fri 02:00" --name backup --forever -- restic backup ~/work
crontinental every 30m --name health --until "tomorrow 18:00" -- 'curl -sf https://example.com/healthz'
crontinental every 10m --name deploy-wait --until-done --until "4 hours" -- 'deploy-is-live && exit 99'
crontinental in 0 --name rebuild -- make release           # now, detached, logged like any job
crontinental every 30m --now --name sync --forever -- ./sync   # once now, then every 30m

crontinental list            what is scheduled, next run, last outcome
crontinental show <name>     one job in full, with its last 10 runs
crontinental log <name>      what it printed
crontinental run <name>      fire it now, without moving its schedule
crontinental edit <name> …   change it in place: --until, --until-done, --forever,
                             --every, --timeout, --notify, or a new command after --
crontinental rm <name>       remove it
```

**Starting now.** `in 0` (or `at now`) runs a one-off straight away instead of
on the next tick. On `every`, `--now` adds one run straight away without moving
the schedule. Without it, an interval repeat's first run is one interval out.

**Always pass `--name`.** It is how the job is found later, and it becomes the
name of its runs and its log (lowercase letters, digits, `-`, `_`).

## How a job ends

A one-off ends when it fires. **A repeat must say how it ends, or it is
refused:**

| flag | ends | use it for |
|---|---|---|
| `--until <time>` | at a deadline (`"tomorrow 18:00"`, `"3 days"`) | watching something for a while |
| `--until-done` | when the command **exits 99** | polling until something is true |
| `--forever` | never | only what the user asked to be permanent |

Pick the narrowest end that matches the ask. "Check every 10 minutes until the
deploy is live" is `--until-done`, paired with `--until` as a backstop so a
condition that never comes true doesn't poll forever. Don't reach for
`--forever` just to get past the refusal: a permanent job is the user's call,
so use it only when they said "every morning", "always" or similar. Write the
command so it exits 99 at the moment its work is done (`check && exit 99`).
Exit 99 counts as success, not failure.

A finished job leaves `list` right away and is deleted 3 days later; until
then `show` and `log` still work. Never schedule a second job to `rm` the
first. That is what `--until` is for.

To change how an existing job ends, or anything else about it, use `edit`
rather than `rm` and a fresh booking: edit keeps its history and who created
it. Naming an end (`--until`, `--until-done`) drops `--forever`, and a repeat
can't be edited into having no end.

## The command

Everything after `--`. **One** argument is a shell line, verbatim: pipes, `&&`,
redirects and `$(…)` all work. **Several** are an argv, quoted safely.

Jobs run in a login shell (`bash -lc` on Linux, the user's own login shell on
macOS), so PATH is the one the user's terminal has, not the service manager's
bare one. Absolute paths are not needed, though
they don't hurt.

## Schedules

`in` takes a timespan (`90m`, `2h`, `1d`). `at` takes a date and time
(`"tomorrow 07:00"`, `"2026-09-15 09:00"`, `"next friday 18:00"`, `"3 days"`,
`"7pm"`, `"Oct 24 10:00"`).

`every` takes systemd's calendar grammar or a plain interval:

```
03:00            daily at 3am        30m    every 30 minutes
Mon..Fri 07:30   weekday mornings    4h     every 4 hours
*-*-01 09:00     the 1st of a month  hourly on the hour
```

**`daily 03:00` is invalid**: `daily` already means midnight and takes no time.
Write `03:00`. **Check any spec before booking it** with
`crontinental check "<spec>"`: it says whether it reads as a calendar, an
interval or a time, and when it next fires. Nothing runs more often than once a
minute; that is the tick.

## Things that will otherwise surprise you

- **A sleeping machine.** A missed run is decided per job by `--catchup`:
  one-offs run late by default (right for a reminder), repeats skip to the next
  slot (right for a nightly job when the lid opens at noon). Pass
  `--catchup run` on a repeat that must not miss a day. A repeat always
  reschedules from *now*, so a day asleep costs one run, never a backlog.
- **Node through mise shims can die unattended.** If the login shell puts
  mise's shims ahead of nvm and mise has no global node, a job calling `node`
  or `npx` fails with "No version is set for shim" yet works when tested by
  hand. Fix it inside the job (put the node it needs first on PATH), not by
  setting a global default the user didn't ask for.
- **Output goes to the job's log, not the database.** `crontinental log
  <name>` is how to read what a job printed. Don't add a redirect of your own.
- **Every job has a 1-hour timeout** unless `--timeout` says otherwise; the kill
  is recorded as `killed`.
- **Overlap cannot happen.** A second run of a job that is still running is
  refused and recorded as `overlap`.
- **Failures notify the user** (critical, with the job name and exit code).
  `--notify always` also announces successes and `--notify never` stays silent.
  Use `never` for anything frequent, or the desktop gets noisy. Where a
  notification goes (desktop, phone, webhook) is the user's hooks in
  `~/.config/crontinental/notify.d/`, not a job setting; `show <name>` names
  them, or says nobody is told. Don't add or change hooks unless asked.
- **A job can say something itself.** When the user wants to hear a result, not
  just a failure ("tell me what the backup found", "let me know when it's
  live"), put `crontinental notify "…"` in the command at that point. It goes to
  the same hooks as the job's own message, whatever `--notify` says (`--notify
  never` plus an explicit notify = quiet unless it matters). `--urgent` marks it
  critical, `--title` replaces the job name as the headline.

## Clean up after yourself

Jobs record the session that created them, so `list` is the audit of what agents
have left running on this machine. Check `list` before adding a near-duplicate
of something already scheduled. If a job's purpose goes away before its end
does, `rm` it; finished jobs take care of themselves.

Full reference: `docs/design.md` in the Crontinental repository
(github.com/Techneosis/the-crontinental).
