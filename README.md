# The Crontinental

*A hotel for commands. Every guest has a reservation, every reservation has a
checkout, and the house keeps the books.*

The Crontinental runs a command later: once, on a schedule, or until its
business is finished. Guests survive power cuts and reboots, never double-book
a room, and are shown out when their stay is over. It was built for a clientele
of AI agents, who book rooms in the middle of a conversation and leave before
anyone thinks to ask who is still upstairs.

```
crontinental in 90m                --name pr-recheck  -- gh pr checks 412
crontinental at "tomorrow 07:00"   --name thaw        -- ./scripts/thaw-staging
crontinental every "Mon..Fri 02:00" --name backup --forever -- restic backup ~/work
crontinental every 10m --name deploy-wait --until-done --until "4 hours" \
                                         -- 'curl -sf https://example.com/healthz && exit 99'
```

## Checking In

**Reservations:** Linux with a systemd user manager, or macOS (Apple silicon
or Intel). One static binary, no runtime, nothing else to install.

```
curl -fsSL https://raw.githubusercontent.com/Techneosis/crontinental/main/install.sh | bash
```

That fetches the latest release for your machine, checks it against the
release's SHA-256 sums, and installs it: the binary in `~/.local/bin`, tab
completion, and the tick (a systemd user timer, or a LaunchAgent on macOS).
Run it again to upgrade; your bookings carry over. Options go after
`bash -s --`:

```
curl -fsSL …/install.sh | bash -s -- --claude          # also brief Claude Code, without asking
curl -fsSL …/install.sh | bash -s -- --version v0.1.0  # a particular release
curl -fsSL …/install.sh | bash -s -- --uninstall       # check out (the ledger is kept)
```

Or download a release archive by hand, unpack it, and run the `install.sh`
inside. On macOS, a browser download is quarantined; `xattr -dr
com.apple.quarantine <dir>` clears it (curl downloads aren't).

**From source** (needs Go): `git clone` this repository and run `./install.sh`
in it. It builds the binary and links everything into the checkout, so edits
are live.

If you use Claude Code, the installer also hands your agents the house
handbook. See [The Clientele's Handbook](#the-clienteles-handbook).

---

## The Rules

The house runs on rules, and none of them bend.

**1. No reservation without a checkout.** A one-night stay (`in`, `at`) checks
out when it is served. A standing reservation (`every`) must name its
departure when it is booked, or the front desk turns it away:

| at the desk | the guest leaves |
|---|---|
| `--until <time>` | on the date given: `"tomorrow 18:00"`, `"3 days"` |
| `--until-done` | when they settle the bill themselves, by exiting **99** |
| `--forever` | never. A permanent resident, on the Management's say-so only |

A guest may hold both of the first two: *"stay until the deploy is live, but
not past tonight."*

**2. One guest per room.** Each stay gets a room of its own, with its own
walls, its own logbook and its own time limit, and a guest who is still in
residence cannot check in a second time under the same name. The desk notes
the attempt as an `overlap` and sends them away. How the rooms are built
depends on the building: see [Two Buildings](#two-buildings).

**3. No guest overstays.** Every stay has a limit, one hour unless arranged
otherwise (`--timeout 20m`). At the limit the room is cleared, the whole party
leaves together, and the books record them as `killed`.

**4. The Management is told when there is trouble.** A guest who leaves badly,
with any exit but 0 or 99, is reported at once: name, exit code, and where to
read the logbook. `--notify always` reports good departures too.
`--notify never` is for regulars who come and go every few minutes.

**5. The books are kept.** The register records who made every booking: a
Claude Code session (from `CLAUDE_CODE_SESSION_ID`), or the user. `list` is a walk down the hall past every
occupied room.

---

## The Staff

**The Ledger** (`~/.local/share/crontinental/ledger.db`). Every
reservation and every stay is written here, and nothing else is trusted. The
building can lose power and the bookings are still on the page in the morning.

**The Concierge** (the tick, every minute). Makes one
round a minute, the only one on staff with a key to the Ledger's schedule. On
each round they:

1. send up the cleaners for anyone who left more than three days ago;
2. show out anyone whose checkout date has arrived;
3. move each due guest's next reservation along *before* showing them up, so
   a guest who can't be seated is never re-seated every minute forever;
4. hand each due guest a room, and return to the desk without waiting to see
   how the stay goes.

The round takes a few dozen milliseconds. No single guest can hold up the
Concierge, or the guest behind them.

**The Room Steward** (`crontinental exec`). Sits inside every room
for the length of the stay, because something must outlast the guest to write
down how they left: `ok`, `fail`, `killed`, or `done`. When a guest settles up
with a 99, it is the Steward who closes the reservation.

**The Cleaners.** Three days after a guest checks out, their entry and its
history are cleared from the Ledger. Until then, `show` and `log` still answer
questions about the stay. The logbook is kept on its own terms, after the room
itself is long gone.

---

## Two Buildings

The house rules are the same in both; the plumbing is local.

| | Linux | macOS |
|---|---|---|
| the Concierge's rounds | a systemd user timer, `crontinental.timer` | a LaunchAgent, `com.techneosis.crontinental` |
| a room | a transient systemd unit: its own cgroup | a session of its own, the command in its own process group |
| one guest per room | systemd refuses a second unit of the same name | the Steward holds a lock the kernel lets go of when they leave |
| the time limit | systemd's `RuntimeMaxSec` | the Steward, who clears the whole party out |
| the logbook | the journal | `~/Library/Logs/crontinental/<name>.log`, in Console.app |
| room service | `bash -lc` | the guest's own login shell |
| word to the Management | the desktop's notifications | Notification Center |

On macOS a guest who must leave early (`rm`) is asked by the Steward, and
shown out with their whole party if they don't go.

---

## Booking a Room

Everything after `--` is the guest's business. One argument is a full shell
line, so pipes, `&&` and `$(…)` all work. Several arguments are taken as an
argv, quoted properly.

**Wake-up calls.** `in` takes a span (`90m`, `2h 30min`, `1d`). `at` takes a
time the way a guest would say it: `"next friday 18:00"`, `"tomorrow 7am"`,
`"2026-11-02 09:00"`, `"Oct 24 10:00"`, `"3 days"`.

**Walk-ins.** `in 0` (or `at now`) seats the guest at once, straight from the
desk, without waiting for the Concierge's next round. The booking is checked
out as it is made, so the Concierge never seats them twice.

**Standing reservations.** `every` takes systemd's calendar grammar, the same
one every systemd timer uses (`03:00`, `Mon..Fri 07:30`, `*-*-01 09:00`,
`hourly`), or a plain interval (`30m`, `4h`). The house reads it with its own
clerk, so it means the same thing in every building, and the clerk is held to
systemd's answers on a hundred thousand bookings. A standing guest booked
with `--now` is also shown up straight away, and their first reservation stays
where the schedule put it: `every 30m --now` runs now, then half an hour from
now.

**Asking the desk.** `crontinental check "<spec>"` says how the house reads a
spec, and when it would next come round:

```
$ crontinental check "Mon..Fri 07:30"
calendar    Mon..Fri *-*-* 07:30:00
next        Mon 10-26 07:30 (in 3d)
            Tue 10-27 07:30 (in 4d)
            Wed 10-28 07:30 (in 5d)
```

The house does not accept `daily 03:00`: `daily` already means midnight and
takes no time. Write `03:00`. Nothing is booked more often than once a minute,
because that is how often the Concierge comes by.

**Room service.** Every guest is served by a login shell (`bash -lc` on
Linux, the user's own on macOS) and starts in `$HOME` unless `--workdir` says
otherwise, so the PATH is the one your terminal has, not the service manager's
bare one. One note on the kitchen: if your
profile puts mise's shims ahead of nvm and mise has no global node, `node` and
`npx` refuse to serve. Guests who need node should bring their own (put the one
they want first on PATH).

---

## Changing a Reservation

`edit` changes a booking in place: its stay and checkout, its schedule, its
business. The guest keeps their history and the register keeps who booked
them, which a cancellation and a fresh booking would both lose.

```
crontinental edit deploy-wait --until "tomorrow 18:00"   # extend the stay
crontinental edit health --until-done                    # leave when the business is done
crontinental edit backup --every "Mon..Fri 03:00"        # a new standing time
crontinental edit backup -- restic backup ~/work ~/notes # new business, same room
crontinental edit thaw --in 0                            # a wake-up call, brought forward to now
```

Checkout terms compose as a guest would mean them: naming a checkout
(`--until`, `--until-done`) ends permanent residence, and `--forever` makes a
resident of a guest who had a checkout. `--no-until` and `--no-until-done`
take one away, but Rule 1 still holds: a standing reservation left with no
checkout at all is turned away, and the booking stays as it was.

The name on the door does not change (that is the room's name; a new name is
a new booking), and neither does whether the room is held. That is `disable`
and `enable`.

---

## Late Arrivals

The building sometimes sleeps (a closed lid, a suspended box). Reservations
are written in the Ledger, so the first round after waking finds everyone who
came due, and each guest's `--catchup` decides what happens:

- **`run`**: seat them anyway, late. The default for wake-up calls: a 9:00
  reminder at 9:40 is still a reminder.
- **`skip`**: wait for the next reservation. The default for standing
  bookings: a nightly job that wakes at noon wants tonight, not lunchtime.

A standing guest is always rebooked from *now*. A day asleep costs at most one
visit, never a queue in the lobby.

---

## At the Front Desk

```
crontinental list              who is in residence, and when they're next due
crontinental list --all        including guests checked out and awaiting the cleaners
crontinental show <name>       one guest's reservation, checkout terms, last 10 stays
crontinental log <name>        their logbook (-f to keep reading)
crontinental run <name>        seat them now, without moving their reservation
crontinental edit <name> …     change a reservation, keeping the guest's history
crontinental disable <name>    hold the room; a held room is never cleaned
crontinental enable <name>     release it; a checked-out regular can be re-booked
crontinental rm <name>         show them out now, mid-stay if need be
crontinental prune             send the cleaners up early
```

---

## The Clientele's Handbook

Most bookings come from AI agents, so the house issues them a handbook: a
Claude Code skill (`agents/skills/crontinental/`) that teaches an agent when to
book a room here instead of somewhere that won't last (a session-scoped loop,
a cloud routine that can't reach this machine, a hand-written cron line), and
how to book one properly: always by name, with the narrowest checkout that fits
the request, and never permanent residence unless the guest's host asked for
it.

When `~/.claude` exists, `install.sh` links the handbook into its `skills/` and
offers to add a short notice to `CLAUDE.md` pointing agents to it. The notice
sits between marker comments, so a later install updates it in place and
`--uninstall` takes it out without disturbing the rest of the file.

```
… | bash -s -- --claude                    # add the CLAUDE.md notice without asking
… | bash -s -- --no-claude                 # leave Claude Code alone
… | bash -s -- --claude-only               # only the handbook and the notice
… | bash -s -- --uninstall --claude-only   # take both back
```

(From a checkout, the same options go to `./install.sh`.)

---

## Why Not Book Elsewhere

`systemd-run --on-calendar` will take a reservation on a napkin. The napkin
is a transient timer, and transient timers are thrown out with the user
manager, so "tomorrow at 9" vanishes at the next reboot without a trace. At
the Crontinental only the *stays* are transient. The reservations are in the
Ledger, and the Concierge is a permanent member of staff.

---

## The Premises

| | |
|---|---|
| `cmd/crontinental/` | the whole establishment: one static Go binary |
| `systemd/crontinental.{timer,service}` | the Concierge's rounds, on Linux |
| `launchd/com.techneosis.crontinental.plist` | the Concierge's rounds, on macOS |
| `completions/crontinental.bash` | the bell on the desk (tab completion) |
| `agents/` | the Clientele's Handbook (a Claude Code skill) and the `CLAUDE.md` notice |
| `tests/` | rehearsals: `python3 -m unittest discover tests` |
| `~/.local/share/crontinental/ledger.db` | the Ledger (`CRONTINENTAL_DB` points the house at another one) |

The building plan, with why each piece is the way it is:
[docs/design.md](docs/design.md).

MIT licensed.
