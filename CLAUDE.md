# Working on Crontinental

This file is for working **on** Crontinental. The instructions for **using** it
(what an agent should do when asked to schedule something) are the skill in
`agents/skills/crontinental/SKILL.md`, which `install.sh` links into a user's
Claude Code setup.

## Layout

| path | what |
|---|---|
| `cmd/crontinental/` | the whole tool, in Go: one static binary (`CGO_ENABLED=0`) |
| `cmd/crontinental/platform_{linux,darwin}.go` | everything that differs: how a run starts, stops and logs |
| `systemd/` | the tick on Linux (`crontinental.{timer,service}`) |
| `launchd/` | the tick on macOS (a LaunchAgent; `install.sh` fills in its paths) |
| `completions/` | bash completion |
| `notify/` | notify hooks: `desktop` (the one `install.sh` seeds) and `examples/` |
| `agents/` | what ships to users' Claude Code: the skill, and the CLAUDE.md snippet |
| `install.sh` | one script, three modes: piped/downloaded fetches a release, a checkout builds, an unpacked release installs its binary; `--claude-only` touches only Claude Code |
| `scripts/package.sh` | builds and packages every release target, plus `SHA256SUMS` |
| `docs/releases/` | release notes, one file per version (optional; otherwise generated) |
| `tests/` | `python3 -m unittest discover tests` — run before every commit (it builds the binary) |
| `docs/design.md` | the building plan: why each piece is the way it is |

## Rules

- **Run the tests.** They drive the real CLI against a throwaway ledger
  (`CRONTINENTAL_DB`) and a throwaway Claude config (`CLAUDE_CONFIG_DIR`), so
  they never touch the machine's own jobs. A behaviour change comes with a test
  that fails without it.
- **One static binary, no runtime.** Go's standard library plus
  `modernc.org/sqlite` (pure Go, so cross-compiling needs no C toolchain). A
  new dependency has to keep `CGO_ENABLED=0` builds working for every target.
- **The ledger schema is a contract.** Existing ledgers must keep working: a
  schema change bumps `schemaVersion` (`ledger.go`) and ships a migration that is tested
  against the previous version's schema (see `Migration` in the tests).
- **Keep the invariants in the tick:** advance a job's schedule *before*
  dispatching it (a failed launch must not retry every minute), and never reap a
  job that has a live run.
- **The README stays in the hotel frame.** The Ledger, the Concierge (tick), the
  Room Steward (`exec`), the Cleaners (the 3-day reap), the Rules. Every
  metaphor has to map to real behaviour; if the behaviour changes, so does the
  README. `docs/design.md` is the plain-language version.
- **`agents/` is for everyone.** No machine names, people or private paths in
  the skill or the snippet. Change the snippet only in
  `agents/claude-md-snippet.md`; installed copies are rewritten from it.
- **Commit messages carry no AI attribution**: no `Co-Authored-By`,
  `Claude-Session` or "Generated with" lines.

## The test suite is the spec

The tests are Python on purpose and black-box: they drive the binary through
its CLI (`CRONTINENTAL_BIN` picks which one; unset, they build it). That made
them the conformance suite when the tool was ported from Python to Go
(2026-10-23), and it keeps behaviour pinned to the CLI rather than to internals.

## Two platforms

`platform_linux.go` leans on systemd (transient units, the journal,
RuntimeMaxSec); `platform_darwin.go` does the same jobs itself, in exec: a
session per run, an flock as the overlap guard (its file names exec's pid and
the command's process group, for `rm`), the timeout on the process group, a log
file per job. Behaviour must match: `tests/test_runs.py` starts real runs on
whichever platform runs it (it skips on Linux without a user manager, i.e. CI),
and CI runs the whole suite on macOS. Cross-compile for a Mac with
`GOOS=darwin GOARCH=arm64 CGO_ENABLED=0 go build`.

## The grammars are held to the real tools

`calendar.go`, `timespan.go` and `when.go` are native, and `go test ./...`
checks them against `systemd-analyze` and GNU `date` wherever those exist
(they skip elsewhere; fixed-answer tests run everywhere). A grammar change keeps
those differential tests passing. `CRONTINENTAL_FUZZ=100000 go test -run Random
./cmd/crontinental/` runs the random calendar comparison at full size (CI runs
300); it tells systemd's own repeat bugs apart from ours by re-asking with the
repeats written out as lists. Deliberate differences from the tools are listed
at the top of each file; anything else that disagrees is a bug here.

## Cutting a release

1. Optionally write `docs/releases/vX.Y.Z.md` (otherwise notes are generated
   from the commits). Commit it.
2. `git tag vX.Y.Z && git push origin vX.Y.Z`. A tag with a `-` (`v0.2.0-rc1`)
   publishes as a prerelease, which the installer's "latest" skips.
3. `.github/workflows/release.yml` packages all four targets, verifies them on
   Linux amd64, Linux arm64 and macOS (checksums, the version string, the full
   conformance suite against the packaged binary, and on macOS the one-liner
   end to end), and only then publishes. A failed check publishes nothing.

To rehearse without publishing: run the workflow by hand (workflow_dispatch),
or locally: `scripts/package.sh v0.0.0-test /tmp/rel/latest/download` and
`CRONTINENTAL_RELEASES=file:///tmp/rel bash -s -- --no-claude < install.sh`
(with a throwaway `HOME` unless you mean to install it).
