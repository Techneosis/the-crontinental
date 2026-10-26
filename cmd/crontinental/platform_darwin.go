//go:build darwin

package main

// macOS: launchd runs the tick (a LaunchAgent, every 60s) and nothing else.
// There are no transient units, so exec carries what systemd carries on
// Linux: it starts each run in a session of its own, holds a lock that is the
// overlap guard, enforces the timeout on the command's process group, and
// writes the run's output to a log file of its own.

import (
	"bufio"
	"bytes"
	"database/sql"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	// selfManaged: exec enforces the timeout and stops the command itself.
	selfManaged = true

	logRotateBytes = 1 << 20 // a log past 1MiB is rotated to .1 before the next run
)

// logDir holds one log per job: ~/Library/Logs, so Console.app shows them.
// Beside a CRONTINENTAL_DB ledger instead, so a test ledger keeps its own.
func logDir() string {
	if os.Getenv("CRONTINENTAL_DB") != "" {
		return filepath.Join(filepath.Dir(dbPath()), "logs")
	}
	return filepath.Join(homeDir(), "Library", "Logs", "crontinental")
}

func logPath(name string) string { return filepath.Join(logDir(), name+".log") }

func lockPath(name string) string {
	return filepath.Join(filepath.Dir(dbPath()), "locks", name+".lock")
}

func runRef(name string) string { return logPath(name) }

func runnerInfo(name string) (label, where string) { return "log", logPath(name) }

var (
	shellOnce sync.Once
	shellPath string
)

// loginShell is the user's own shell, as Directory Services records it. A
// LaunchAgent's environment carries no SHELL to go by, and on macOS the shell
// is usually zsh, whose profile is where Homebrew puts itself on PATH.
func loginShell() string {
	shellOnce.Do(func() {
		if u := os.Getenv("USER"); u != "" {
			if out, err := exec.Command("/usr/bin/dscl", ".", "-read", "/Users/"+u, "UserShell").Output(); err == nil {
				if f := strings.Fields(string(out)); len(f) == 2 && f[0] == "UserShell:" {
					shellPath = f[1]
				}
			}
		}
		if shellPath == "" {
			shellPath = os.Getenv("SHELL")
		}
		if shellPath == "" {
			shellPath = "/bin/zsh"
		}
	})
	return shellPath
}

func shellArgv(command string) []string { return []string{loginShell(), "-l", "-c", command} }

func shellDesc() string { return "login shell, " + loginShell() + " -l" }

// banner marks where each run starts and ends in the job's log. A log file has
// no journal to stamp it.
func banner(msg string) {
	fmt.Printf("── %s %s\n", time.Now().Format("2006-01-02 15:04:05"), msg)
}

// runGuard is an exclusive flock on the job's lock file, held for the whole
// run. The kernel drops it when exec exits however it exits, so it can never
// go stale. The file also says who holds it, for `rm`.
type runGuard struct{ f *os.File }

var heldGuard *os.File // kept reachable: closing the file would drop the lock

func guardRun(name string) (runGuard, bool) {
	p := lockPath(name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		die("cannot create %s: %v", filepath.Dir(p), err)
	}
	f, err := os.OpenFile(p, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		die("cannot open %s: %v", p, err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return runGuard{}, false
	}
	heldGuard = f
	g := runGuard{f}
	g.write(fmt.Sprintf("%d", os.Getpid()))
	return g, true
}

func (g runGuard) write(s string) {
	if g.f == nil {
		return
	}
	_ = g.f.Truncate(0)
	_, _ = g.f.WriteAt([]byte(s+"\n"), 0)
}

// started records the command's process group beside exec's own pid.
func (g runGuard) started(pgid int) { g.write(fmt.Sprintf("%d %d", os.Getpid(), pgid)) }

// dispatch starts one run: exec, in a session of its own (so launchd's cleanup
// of the tick's process group never reaches it), writing to the job's log. It
// returns at once; exec does the rest, the overlap check included.
func dispatch(db *sql.DB, j *Job, dueAt time.Time, late int64) {
	p := logPath(j.Name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err == nil {
		if st, err := os.Stat(p); err == nil && st.Size() > logRotateBytes {
			_ = os.Rename(p, p+".1")
		}
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		startFailed(db, j, dueAt, late, err)
		return
	}
	defer f.Close()
	cmd := exec.Command(self(), "exec", j.Name, "--due", iso(dueAt), "--late", strconv.FormatInt(late, 10))
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		startFailed(db, j, dueAt, late, err)
		return
	}
	_ = cmd.Process.Release()
}

func startFailed(db *sql.DB, j *Job, dueAt time.Time, late int64, err error) {
	msg := err.Error()
	record(db, j, dueAt, late, "error", truncate(msg, 500), logPath(j.Name))
	if j.Notify != "never" {
		notify(j, "unstartable", "Scheduled job could not start", j.Name+": "+truncate(msg, 160), true, nil, nil)
	}
}

// stopRun ends a run in progress, if there is one: SIGTERM to its exec, which
// stops the command's process group and records the run as killed. It waits
// for the lock to come free, and makes sure of the group if exec doesn't.
func stopRun(name string) {
	f, err := os.OpenFile(lockPath(name), os.O_RDWR, 0)
	if err != nil {
		return // never run here
	}
	defer f.Close()
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
		return // nothing holds it: no run in progress
	}
	data, _ := io.ReadAll(f)
	ids := strings.Fields(string(data))
	if len(ids) == 0 {
		return
	}
	execPid, _ := strconv.Atoi(ids[0])
	if execPid > 0 {
		_ = syscall.Kill(execPid, syscall.SIGTERM)
	}
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) == nil {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	if len(ids) > 1 {
		if pgid, _ := strconv.Atoi(ids[1]); pgid > 0 {
			_ = syscall.Kill(-pgid, syscall.SIGKILL)
		}
	}
	if execPid > 0 {
		_ = syscall.Kill(execPid, syscall.SIGKILL)
	}
}

// showLog prints the last lines of the job's log (the rotated half first), and
// with follow keeps printing what arrives, across a rotation.
func showLog(name string, lines int, follow bool) {
	p := logPath(name)
	var all []byte
	for _, f := range []string{p + ".1", p} {
		if b, err := os.ReadFile(f); err == nil {
			all = append(all, b...)
		}
	}
	if len(all) == 0 && !follow {
		fmt.Printf("no output logged yet (%s)\n", p)
		return
	}
	var tail []string
	sc := bufio.NewScanner(bytes.NewReader(all))
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		tail = append(tail, sc.Text())
		if len(tail) > lines {
			tail = tail[1:]
		}
	}
	for _, l := range tail {
		fmt.Println(l)
	}
	if !follow {
		return
	}
	var off int64
	if st, err := os.Stat(p); err == nil {
		off = st.Size()
	}
	for {
		time.Sleep(500 * time.Millisecond)
		st, err := os.Stat(p)
		if err != nil {
			continue
		}
		if st.Size() < off {
			off = 0 // rotated: read the new file from its start
		}
		if st.Size() == off {
			continue
		}
		f, err := os.Open(p)
		if err != nil {
			continue
		}
		_, _ = f.Seek(off, io.SeekStart)
		n, _ := io.Copy(os.Stdout, f)
		f.Close()
		off += n
	}
}
