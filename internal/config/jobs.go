package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

// Job is a command the supervisor runs for a project, in its main checkout or, with Checkout "fresh", in a new
// worktree at the base branch's remote head that is removed after the run (kept for a day when the run
// fails). The main checkout is fast-forwarded to the remote head first when it can be; the run records how
// far behind it was when it can't. It runs:
//
//   - every EveryMinutes minutes;
//   - at set times: At (UTC, HH:MM) or Cron (five fields, in TimeZone, UTC when left out). A slot missed by
//     more than six hours waits for the next; the latest one within six hours that hasn't run runs once;
//   - on webhooks (see JobTrigger), and from the TUI (j).
//
// The supervisor waits for a run (up to TimeoutMinutes, 5 when left out) and records how it ended from its
// exit code. A run asked for while one is going waits for it: at most one waits, and a delivery's takes the
// place of a timer's. A timed job with Status instead starts detached, for long runs, and reports through
// Status, a folder where it keeps running.json ({pid, slot, startedAt, log}) while it runs and last.json
// ({slot, outcome, finishedAt}) when it ends; an outcome of "busy" is tried again in ten minutes, and a
// delivery while it runs starts nothing.
//
// A job with Priority "high" comes first on the host: while it runs, the supervisor starts no new agent turns
// and no new agent tasks (those going carry on), and it runs at the supervisor's own priority rather than the
// lower one other jobs and tasks get. A timed one can ask for the host to be quiet already when it starts:
// LeadMinutes before each of its times, new turns and tasks wait too.
//
// Each run's output, start, length, exit code and trigger are kept: the last KeepRuns (30 when left out) under
// the project's state folder, in job-runs/<job>/, and in the TUI (J).
type Job struct {
	Name           string      `yaml:"name" json:"name"`
	Command        []string    `yaml:"command" json:"command"`
	EveryMinutes   int         `yaml:"everyMinutes,omitempty" json:"everyMinutes,omitempty"`
	TimeoutMinutes int         `yaml:"timeoutMinutes,omitempty" json:"timeoutMinutes,omitempty"`
	At             []string    `yaml:"at,omitempty" json:"at,omitempty"`
	Cron           string      `yaml:"cron,omitempty" json:"cron,omitempty"`
	TimeZone       string      `yaml:"timeZone,omitempty" json:"timeZone,omitempty"`
	Status         string      `yaml:"status,omitempty" json:"status,omitempty"`
	Checkout       string      `yaml:"checkout,omitempty" json:"checkout,omitempty"`
	KeepRuns       int         `yaml:"keepRuns,omitempty" json:"keepRuns,omitempty"`
	On             *JobTrigger `yaml:"on,omitempty" json:"on,omitempty"`
	Priority       string      `yaml:"priority,omitempty" json:"priority,omitempty"`
	LeadMinutes    int         `yaml:"leadMinutes,omitempty" json:"leadMinutes,omitempty"`
}

// JobTrigger runs a job on webhooks, which arrive through the tunnel. Events are the code host's or tracker's
// own event names, taken through the project's hook: "push", or "release.published" for one action of an
// event. Branches limits events that carry a git ref (a push) to those branches, globs allowed. Paths limits a
// push to those that change a file matching them, as GitHub Actions has it: globs (* within a folder, **
// across them), and a pattern starting ! takes back the files it matches from those before it. Webhook takes
// POSTs from anywhere else at /jobs/<project>/<job>/<token>; the supervisor logs the URL once the tunnel is up.
// The job is told about the delivery through OPEN_SUPERVISOR_EVENT, _EVENT_ACTION, _EVENT_REF, _EVENT_SHA (a
// push's new head) and _EVENT_FILE (the body).
type JobTrigger struct {
	Events   []string `yaml:"events,omitempty" json:"events,omitempty"`
	Branches []string `yaml:"branches,omitempty" json:"branches,omitempty"`
	Paths    []string `yaml:"paths,omitempty" json:"paths,omitempty"`
	Webhook  bool     `yaml:"webhook,omitempty" json:"webhook,omitempty"`
}

// CheckoutFresh runs a job in a new worktree for each run.
const CheckoutFresh = "fresh"

// PriorityHigh is the Priority of a job that comes first on the host.
const PriorityHigh = "high"

// First reports whether the job comes first on the host: no new turns or tasks start while it runs.
func (j Job) First() bool { return j.Priority == PriorityHigh }

// Timed reports whether the job runs at set times.
func (j Job) Timed() bool { return len(j.At) > 0 || j.Cron != "" }

// Detached reports whether the job's timed runs start detached and report through Status.
func (j Job) Detached() bool { return j.Timed() && j.Status != "" }

// Fresh reports whether each run gets a new worktree.
func (j Job) Fresh() bool { return j.Checkout == CheckoutFresh }

// Location is the time zone the job's Cron is read in.
func (j Job) Location() (*time.Location, error) {
	if j.TimeZone == "" {
		return time.UTC, nil
	}
	return time.LoadLocation(j.TimeZone)
}

// NextSlot is the first of a timed job's times after from; zero for a job that isn't timed.
func (j Job) NextSlot(from time.Time) time.Time {
	if j.Cron != "" {
		sched, err := j.Schedule()
		loc, err2 := j.Location()
		if err != nil || err2 != nil {
			return time.Time{}
		}
		return sched.Next(from.In(loc))
	}
	var next time.Time
	from = from.UTC()
	for _, at := range j.At {
		h, m, _ := strings.Cut(at, ":")
		hh, err1 := strconv.Atoi(h)
		mm, err2 := strconv.Atoi(m)
		if err1 != nil || err2 != nil {
			continue
		}
		for ahead := 0; ahead <= 1; ahead++ {
			t := time.Date(from.Year(), from.Month(), from.Day()+ahead, hh, mm, 0, 0, time.UTC)
			if t.After(from) && (next.IsZero() || t.Before(next)) {
				next = t
			}
		}
	}
	return next
}

// Schedule is the job's Cron, parsed.
func (j Job) Schedule() (cron.Schedule, error) {
	return cron.ParseStandard(j.Cron)
}

// Validate is what's wrong with the job, if anything.
func (j Job) Validate() []string {
	var errs []string
	name := "job " + j.Name
	if j.Name == "" || len(j.Command) == 0 {
		errs = append(errs, "every job needs a name and a command")
	}
	if j.EveryMinutes > 0 && j.Timed() {
		errs = append(errs, name+": everyMinutes or at/cron, not both")
	}
	if len(j.At) > 0 && j.Cron != "" {
		errs = append(errs, name+": at or cron, not both")
	}
	for _, at := range j.At {
		h, m, ok := strings.Cut(at, ":")
		hh, err1 := strconv.Atoi(h)
		mm, err2 := strconv.Atoi(m)
		if !ok || err1 != nil || err2 != nil || hh < 0 || hh > 23 || mm < 0 || mm > 59 {
			errs = append(errs, fmt.Sprintf("%s: at %q isn't HH:MM", name, at))
		}
	}
	if j.Cron != "" {
		if strings.HasPrefix(j.Cron, "TZ=") || strings.HasPrefix(j.Cron, "CRON_TZ=") {
			errs = append(errs, name+": set the time zone with timeZone, not in cron")
		} else if _, err := j.Schedule(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: cron: %v", name, err))
		}
	}
	if j.TimeZone != "" {
		if j.Cron == "" {
			errs = append(errs, name+": timeZone is for cron (at is UTC)")
		}
		if _, err := j.Location(); err != nil {
			errs = append(errs, fmt.Sprintf("%s: timeZone: %v", name, err))
		}
	}
	if j.Status != "" && !j.Timed() {
		errs = append(errs, name+": status is for at or cron jobs")
	}
	if j.Checkout != "" && j.Checkout != CheckoutFresh && j.Checkout != "main" {
		errs = append(errs, fmt.Sprintf("%s: checkout %q: fresh or main", name, j.Checkout))
	}
	if j.Fresh() && j.Detached() {
		errs = append(errs, name+": a fresh checkout is for runs the supervisor waits for, not ones with status")
	}
	if j.Priority != "" && !j.First() {
		errs = append(errs, fmt.Sprintf("%s: priority %q: high, or leave it out", name, j.Priority))
	}
	if j.LeadMinutes < 0 {
		errs = append(errs, name+": leadMinutes can't be negative")
	}
	if j.LeadMinutes > 0 && (!j.First() || !j.Timed()) {
		errs = append(errs, name+": leadMinutes is for at or cron jobs with priority high")
	}
	if j.On != nil {
		if len(j.On.Events) == 0 && !j.On.Webhook {
			errs = append(errs, name+": on needs events or webhook")
		}
		for _, p := range j.On.Paths {
			if strings.TrimPrefix(p, "!") == "" {
				errs = append(errs, name+": an empty path pattern")
			}
		}
	}
	return errs
}
