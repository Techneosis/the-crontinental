package main

// Notifications are hooks. Crontinental decides WHEN to tell someone (a job's
// --notify), and the executables in the notify directory decide WHERE: the
// desktop, a phone, a webhook. The binary has no notifier of its own; the
// installer seeds a desktop hook, and an empty directory means nobody is told.
//
// Each hook gets the event as CRONTINENTAL_* environment variables and as one
// JSON object on stdin, runs alongside the others, and has hookTimeout to
// finish. A hook that fails, hangs or is missing never changes a run's outcome;
// what it printed, and how it failed, goes to whatever log the caller writes to
// (a run's own, when exec is the caller).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

const hookTimeout = 10 * time.Second

// notifyDir is where the hooks live. A file, not an environment variable a
// shell profile could set: the tick and the runs start from the service
// manager, which never reads one.
func notifyDir() string {
	if d := os.Getenv("CRONTINENTAL_NOTIFY_DIR"); d != "" {
		return d
	}
	cfg := os.Getenv("XDG_CONFIG_HOME")
	if cfg == "" {
		cfg = filepath.Join(homeDir(), ".config")
	}
	return filepath.Join(cfg, "crontinental", "notify.d")
}

// hooks lists the executables in the notify directory, in name order. Dotfiles
// and editor backups are skipped, and so is anything not executable, which is
// how a hook is switched off without deleting it.
func hooks() []string {
	dir := notifyDir()
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []string
	for _, e := range ents {
		n := e.Name()
		if strings.HasPrefix(n, ".") || strings.HasSuffix(n, "~") {
			continue
		}
		p := filepath.Join(dir, n)
		st, err := os.Stat(p) // follows links: a hook may be a symlink
		if err != nil || st.IsDir() || st.Mode()&0o111 == 0 {
			continue
		}
		out = append(out, p)
	}
	sort.Strings(out)
	return out
}

// event is what a hook is told. Status and Elapsed are absent when there is
// none (a run that never started has no exit status).
type event struct {
	Event    string `json:"event"` // fail, killed, error, unstartable, done, ok
	Job      string `json:"job"`
	Status   *int   `json:"status,omitempty"`
	Elapsed  *int   `json:"elapsed,omitempty"`
	Urgency  string `json:"urgency"` // critical or low
	Headline string `json:"headline"`
	Body     string `json:"body"`
	Log      string `json:"log"` // the command that shows the job's output
	Note     string `json:"note,omitempty"`
	Host     string `json:"host"`
}

// notify tells every hook about ev. It returns when they have all finished or
// been given up on.
func notify(j *Job, kind, headline, body string, critical bool, status, elapsed *int) {
	hs := hooks()
	if len(hs) == 0 {
		return
	}
	ev := event{Event: kind, Job: j.Name, Status: status, Elapsed: elapsed, Urgency: "low",
		Headline: headline, Body: body, Log: "crontinental log " + j.Name}
	if critical {
		ev.Urgency = "critical"
	}
	if j.Note.Valid {
		ev.Note = j.Note.String
	}
	ev.Host, _ = os.Hostname()

	payload, _ := json.Marshal(ev)
	env := append(os.Environ(),
		"CRONTINENTAL_EVENT="+ev.Event, "CRONTINENTAL_JOB="+ev.Job,
		"CRONTINENTAL_URGENCY="+ev.Urgency, "CRONTINENTAL_HEADLINE="+ev.Headline,
		"CRONTINENTAL_BODY="+ev.Body, "CRONTINENTAL_LOG="+ev.Log,
		"CRONTINENTAL_NOTE="+ev.Note, "CRONTINENTAL_HOST="+ev.Host)
	if status != nil {
		env = append(env, "CRONTINENTAL_STATUS="+strconv.Itoa(*status))
	}
	if elapsed != nil {
		env = append(env, "CRONTINENTAL_ELAPSED="+strconv.Itoa(*elapsed))
	}

	var wg sync.WaitGroup
	for _, h := range hs {
		wg.Add(1)
		go func(h string) {
			defer wg.Done()
			runHook(h, env, payload)
		}(h)
	}
	wg.Wait()
}

func runHook(path string, env []string, payload []byte) {
	ctx, cancel := context.WithTimeout(context.Background(), hookTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, path)
	cmd.Env, cmd.Stdin = env, bytes.NewReader(payload)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	// Its own process group, so a timeout takes whatever the hook started too
	// (a curl left behind would hold the run's log open).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	name := filepath.Base(path)
	switch {
	case ctx.Err() != nil:
		fmt.Fprintf(os.Stderr, "crontinental: notify hook %s: gave up after %s\n", name, hookTimeout)
	case err != nil:
		fmt.Fprintf(os.Stderr, "crontinental: notify hook %s: %v\n", name, err)
	}
}

// intp is for the optional fields of an event.
func intp(n int) *int { return &n }
