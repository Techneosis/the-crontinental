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
//
// Every notification is also written in the ledger's message book first, hooks
// or none, so it can be read back (`messages`) by whoever missed it: a phone in
// a pocket, a toast that timed out. Hooks get its id, which is how a device
// links its notification to the record.

import (
	"bytes"
	"context"
	"database/sql"
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
	ID       int64             `json:"id,omitempty"`  // in the message book; absent if it couldn't be written
	Event    string            `json:"event"`         // fail, killed, error, unstartable, done, ok, message
	Job      string            `json:"job"`           // empty for a message sent from outside any job
	Run      int64             `json:"run,omitempty"` // the run it is about, or was sent from
	Status   *int              `json:"status,omitempty"`
	Elapsed  *int              `json:"elapsed,omitempty"`
	Urgency  string            `json:"urgency"` // critical or low
	Headline string            `json:"headline"`
	Body     string            `json:"body"`
	Log      string            `json:"log,omitempty"` // the command that shows the job's output
	Note     string            `json:"note,omitempty"`
	Tags     map[string]string `json:"tags"` // the job's --tag pairs; {} for none
	Host     string            `json:"host"`
}

// notify tells every hook about ev, and returns how many it told (once they
// have all finished or been given up on). j may be nil: a message from outside
// any job.
func notify(j *Job, kind, headline, body string, critical bool, status, elapsed *int) int {
	return notifyTagged(j, nil, kind, headline, body, critical, status, elapsed)
}

// notifyTagged is notify with extra tags laid over the job's, for one message.
func notifyTagged(j *Job, extra map[string]string, kind, headline, body string, critical bool, status, elapsed *int) int {
	if j == nil {
		j = &Job{}
	}
	ev := event{Event: kind, Job: j.Name, Status: status, Elapsed: elapsed, Urgency: "low",
		Headline: headline, Body: body}
	if j.Name != "" {
		ev.Log = "crontinental log " + j.Name
	}
	if critical {
		ev.Urgency = "critical"
	}
	if j.Note.Valid {
		ev.Note = j.Note.String
	}
	ev.Tags = jobTags(j)
	for k, v := range extra {
		ev.Tags[k] = v
	}
	ev.Host, _ = os.Hostname()
	ev.Run = runOf()
	recordMessage(&ev)

	hs := hooks()
	if len(hs) == 0 {
		return 0
	}
	payload, _ := json.Marshal(ev)
	env := append(hookEnviron(),
		"CRONTINENTAL_EVENT="+ev.Event, "CRONTINENTAL_JOB="+ev.Job,
		"CRONTINENTAL_URGENCY="+ev.Urgency, "CRONTINENTAL_HEADLINE="+ev.Headline,
		"CRONTINENTAL_BODY="+ev.Body, "CRONTINENTAL_LOG="+ev.Log,
		"CRONTINENTAL_NOTE="+ev.Note, "CRONTINENTAL_HOST="+ev.Host)
	env = append(env, tagEnv(ev.Tags)...)
	if status != nil {
		env = append(env, "CRONTINENTAL_STATUS="+strconv.Itoa(*status))
	}
	if elapsed != nil {
		env = append(env, "CRONTINENTAL_ELAPSED="+strconv.Itoa(*elapsed))
	}
	if ev.ID != 0 {
		env = append(env, "CRONTINENTAL_MESSAGE="+strconv.FormatInt(ev.ID, 10))
	}
	if ev.Run != 0 {
		env = append(env, "CRONTINENTAL_RUN="+strconv.FormatInt(ev.Run, 10))
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
	return len(hs)
}

// cmdNotify is a message on request: from inside a job (exec sets
// CRONTINENTAL_JOB, so it goes out as that job's) or from anywhere else. It is
// sent whatever the job's --notify says; that only governs the reports at the
// end of a run. Nobody to tell is said on stderr but is not an error, so a
// script under `set -e` doesn't die of a quiet switchboard.
func cmdNotify(o *opts) {
	if strings.TrimSpace(o.text) == "" {
		die("nothing to say")
	}
	extra := parseTags(o.tags)
	var j *Job
	if name := os.Getenv("CRONTINENTAL_JOB"); name != "" {
		j = lookupJob(connect(), name)
		if j == nil {
			j = &Job{Name: name} // removed mid-run: still the job's word
		}
	}
	title := o.title
	if title == "" {
		title = "Crontinental"
		if j != nil {
			title = j.Name
		}
	}
	if notifyTagged(j, extra, "message", title, o.text, o.urgent, nil, nil) == 0 {
		fmt.Fprintf(os.Stderr, "crontinental: nobody was told: no hooks in %s\n", notifyDir())
	}
}

// currentRun is the run this process is, when it is exec.
var currentRun int64

// runOf is the run a notification belongs to: exec's own, or, for a
// `crontinental notify` from inside a run, the one exec named in its
// environment.
func runOf() int64 {
	if currentRun != 0 {
		return currentRun
	}
	n, _ := strconv.ParseInt(os.Getenv("CRONTINENTAL_RUN"), 10, 64)
	return n
}

// recordMessage writes ev in the message book and sets its id. A ledger that
// cannot be written costs the record, never the notification, so this
// complains and carries on rather than dying.
func recordMessage(ev *event) {
	db, err := sql.Open("sqlite", dbPath()+"?_pragma=busy_timeout(10000)")
	if err == nil {
		defer db.Close()
		_, err = db.Exec(messageSchema)
	}
	var res sql.Result
	if err == nil {
		tags, _ := json.Marshal(ev.Tags)
		var job, run any
		if ev.Job != "" {
			job = ev.Job
		}
		if ev.Run != 0 {
			run = ev.Run
		}
		res, err = db.Exec(`INSERT INTO message (at, event, job, run_id, urgency, headline, body, tags)
			VALUES (?,?,?,?,?,?,?,?)`, iso(now()), ev.Event, job, run, ev.Urgency, ev.Headline, ev.Body, string(tags))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "crontinental: the message book: %v\n", err)
		return
	}
	ev.ID, _ = res.LastInsertId()
}

// cmdMessages reads the message book back, newest first.
func cmdMessages(o *opts) {
	db := connect()
	q := `SELECT id, at, event, job, run_id AS run, urgency, headline, body, tags
		FROM message ORDER BY id DESC LIMIT ?`
	if o.json {
		rows, _ := rowsJSON(db, q, o.lines)
		printJSON(rows)
		return
	}
	rows, err := db.Query(q, o.lines)
	if err != nil {
		die("ledger: %v", err)
	}
	defer rows.Close()
	n := 0
	for rows.Next() {
		var id int64
		var at, kind, urgency, headline, body string
		var job, tags sql.NullString
		var run sql.NullInt64
		if err := rows.Scan(&id, &at, &kind, &job, &run, &urgency, &headline, &body, &tags); err != nil {
			die("ledger: %v", err)
		}
		mark := " "
		if urgency == "critical" {
			mark = "!"
		}
		who := job.String
		if who == "" {
			who = "-"
		}
		fmt.Printf("%5d %s %s  %-20s %s: %s\n", id, mark, parseISO(at).Local().Format("Jan _2 15:04"),
			truncate(who, 20), headline, strings.ReplaceAll(body, "\n", " "))
		n++
	}
	if n == 0 {
		fmt.Println("no messages")
	}
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
