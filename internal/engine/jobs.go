package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/core"
	"github.com/alsiola/open-supervisor/internal/logx"
	"github.com/alsiola/open-supervisor/internal/proc"
	"github.com/alsiola/open-supervisor/internal/store"
)

// RepoFile is a project's own settings, kept in its checkout: the jobs the supervisor runs for it.
const RepoFile = ".open-supervisor.yaml"

// JobRunning is what a timed job writes to running.json while it runs.
type JobRunning struct {
	PID       int       `json:"pid"`
	Slot      string    `json:"slot"`
	StartedAt time.Time `json:"startedAt"`
	Log       string    `json:"log,omitempty"`
}

// JobResult is what a timed job writes to last.json when it ends.
type JobResult struct {
	Slot       string    `json:"slot"`
	Outcome    string    `json:"outcome"`
	FinishedAt time.Time `json:"finishedAt"`
	StoppedAt  string    `json:"stoppedAt,omitempty"`
	SHA        string    `json:"sha,omitempty"`
	Issues     []any     `json:"issues,omitempty"`
}

// Jobs are a project's own jobs: checks on an interval, which the supervisor waits for, and long runs at set
// UTC times, which it starts detached and follows through their status files (see config.Job).
type Jobs struct {
	Specs []config.Job

	p      *Project
	cancel context.CancelFunc

	mu      sync.Mutex
	busy    map[string]bool
	pending map[string][]string // an interval job's next run, asked for by a delivery while it ran: its env
	running map[string]JobRunning
	last    map[string]JobResult
}

// loadJobs is the project's jobs: those in the config, then those in its checkout's .open-supervisor.yaml.
func loadJobs(p *Project) *Jobs {
	specs := append([]config.Job(nil), p.Spec.Jobs...)
	if data, err := os.ReadFile(filepath.Join(p.Root, RepoFile)); err == nil {
		var own struct {
			Jobs []config.Job `yaml:"jobs"`
		}
		if err := yaml.Unmarshal(data, &own); err != nil {
			p.logf("%s: %v", RepoFile, err)
		}
		specs = append(specs, own.Jobs...)
	}
	return &Jobs{Specs: specs, p: p, busy: map[string]bool{}, pending: map[string][]string{}, running: map[string]JobRunning{}, last: map[string]JobResult{}}
}

// Running is the job's timed run going now, if any.
func (j *Jobs) Running(name string) (JobRunning, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	r, ok := j.running[name]
	return r, ok
}

// Last is how the job's last timed run ended, if known.
func (j *Jobs) Last(name string) (JobResult, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	r, ok := j.last[name]
	return r, ok
}

func (j *Jobs) start() {
	s := j.p.hub.settings()
	if !s.Jobs || s.DryRun || len(j.Specs) == 0 {
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	j.cancel = cancel
	for _, job := range j.Specs {
		job := job
		switch {
		case job.EveryMinutes > 0:
			go func() {
				j.runCheck(job, nil)
				t := time.NewTicker(time.Duration(job.EveryMinutes) * time.Minute)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						j.runCheck(job, nil)
					}
				}
			}()
		case len(job.At) > 0:
			if r, ok := j.readRunning(job); ok {
				j.mu.Lock()
				j.running[job.Name] = r
				j.mu.Unlock()
				j.p.logf("%s: the %s run is going (pid %d)", job.Name, r.Slot, r.PID)
			}
			if last, ok := j.readLast(job); ok {
				j.mu.Lock()
				j.last[job.Name] = last
				j.mu.Unlock()
			}
			go func() {
				j.runIfDue(job, false, nil)
				t := time.NewTicker(time.Minute)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						j.runIfDue(job, false, nil)
					}
				}
			}()
		}
	}
}

func (j *Jobs) stop() {
	if j.cancel != nil {
		j.cancel()
	}
}

// RunNow runs a job now: a check at once, a timed run whatever the time.
func (j *Jobs) RunNow(job config.Job) {
	if j.p.hub.settings().DryRun {
		return
	}
	if len(job.At) > 0 {
		go j.runIfDue(job, true, nil)
	} else {
		go j.runCheck(job, nil)
	}
}

func (j *Jobs) env(extra []string) []string { return append(j.p.hub.sessionEnv(j.p, ""), extra...) }

// Events are the event names the jobs run on, without their actions: what the project's hooks subscribe to.
func (j *Jobs) Events() []string {
	var out []string
	for _, job := range j.Specs {
		if job.On == nil {
			continue
		}
		for _, e := range job.On.Events {
			name, _, _ := strings.Cut(e, ".")
			if name != "" && !slices.Contains(out, name) {
				out = append(out, name)
			}
		}
	}
	return out
}

// Webhooked are the jobs that take deliveries at their own URL.
func (j *Jobs) Webhooked() []config.Job {
	var out []config.Job
	for _, job := range j.Specs {
		if job.On != nil && job.On.Webhook {
			out = append(out, job)
		}
	}
	return out
}

// Find is the job with the name.
func (j *Jobs) Find(name string) (config.Job, bool) {
	for _, job := range j.Specs {
		if job.Name == name {
			return job, true
		}
	}
	return config.Job{}, false
}

// matches reports whether a job runs on the event: one of its events names it ("push"), or it and its action
// ("release.published"), and its ref is on one of the job's branches when it has any.
func matches(job config.Job, ev core.HookEvent) bool {
	if job.On == nil {
		return false
	}
	named := false
	for _, e := range job.On.Events {
		name, action, hasAction := strings.Cut(e, ".")
		if name == ev.Name && (!hasAction || action == ev.Action) {
			named = true
			break
		}
	}
	if !named {
		return false
	}
	if len(job.On.Branches) == 0 {
		return true
	}
	branch, ok := strings.CutPrefix(ev.Ref, "refs/heads/")
	if !ok {
		return false
	}
	for _, pat := range job.On.Branches {
		if m, _ := path.Match(pat, branch); m {
			return true
		}
	}
	return false
}

// OnEvent runs each job that runs on the event.
func (j *Jobs) OnEvent(ev core.HookEvent) {
	for _, job := range j.Specs {
		if matches(job, ev) {
			j.Trigger(job, ev)
		}
	}
}

// Trigger runs a job for a delivery, telling it about the delivery through its environment.
func (j *Jobs) Trigger(job config.Job, ev core.HookEvent) {
	s := j.p.hub.settings()
	what := ev.Name
	if ev.Action != "" {
		what += "." + ev.Action
	}
	if !s.Jobs || s.DryRun {
		j.p.logf("%s: not run on %s (jobs are off)", job.Name, what)
		return
	}
	extra := []string{"OPEN_SUPERVISOR_EVENT=" + ev.Name, "OPEN_SUPERVISOR_EVENT_ACTION=" + ev.Action, "OPEN_SUPERVISOR_EVENT_REF=" + ev.Ref}
	if file, err := j.writeEvent(job, ev.Body); err != nil {
		j.p.logf("%s: couldn't keep the %s delivery: %v", job.Name, what, err)
	} else {
		extra = append(extra, "OPEN_SUPERVISOR_EVENT_FILE="+file)
	}
	j.p.logf("%s: running on %s", job.Name, what)
	if len(job.At) > 0 {
		go j.runIfDue(job, true, extra)
	} else if j.begin(job.Name, extra) {
		// Queued here rather than in the goroutine, so the run after the current one is the latest delivery's.
		go j.loop(job, extra)
	}
}

// writeEvent keeps a delivery's body for the job to read, clearing out those more than a day old.
func (j *Jobs) writeEvent(job config.Job, body []byte) (string, error) {
	dir := filepath.Join(j.p.StateDir, "job-events")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if old, err := os.ReadDir(dir); err == nil {
		for _, e := range old {
			if info, err := e.Info(); err == nil && time.Since(info.ModTime()) > 24*time.Hour {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	file := filepath.Join(dir, fmt.Sprintf("%s-%d.json", job.Name, time.Now().UnixNano()))
	return file, os.WriteFile(file, body, 0o600)
}

// runCheck runs an interval job and waits for it. A run asked for by a delivery while the job is going runs
// once it ends; one asked for by the timer is dropped.
func (j *Jobs) runCheck(job config.Job, extra []string) {
	if j.begin(job.Name, extra) {
		j.loop(job, extra)
	}
}

// begin marks the job running and reports true; when it's running already, a delivery's run (extra set) is
// kept for after it, and it reports false.
func (j *Jobs) begin(name string, extra []string) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.busy[name] {
		if extra != nil {
			j.pending[name] = extra
		}
		return false
	}
	j.busy[name] = true
	return true
}

// loop runs the job, then each run asked for while it went, until none is left.
func (j *Jobs) loop(job config.Job, extra []string) {
	for {
		j.check(job, extra)
		j.mu.Lock()
		next, ok := j.pending[job.Name]
		delete(j.pending, job.Name)
		if !ok {
			delete(j.busy, job.Name)
			j.mu.Unlock()
			return
		}
		j.mu.Unlock()
		extra = next
	}
}

func (j *Jobs) check(job config.Job, extra []string) {
	timeout := job.TimeoutMinutes
	if timeout <= 0 {
		timeout = 5
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, job.Command[0], job.Command[1:]...)
	cmd.Dir = j.p.Root
	cmd.Env = j.env(extra)
	if out, err := cmd.CombinedOutput(); err != nil {
		tail := strings.TrimSpace(string(out))
		if i := strings.LastIndex(tail, "\n"); i >= 0 {
			tail = tail[i+1:]
		}
		j.p.logf("%s failed: %s %s", job.Name, logx.ErrText(err), tail)
	}
}

func (j *Jobs) statusFile(job config.Job, name string) string {
	if job.Status == "" {
		return ""
	}
	dir := job.Status
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(j.p.Root, dir)
	}
	return filepath.Join(dir, name)
}

func readJSON(file string, out any) bool {
	if file == "" {
		return false
	}
	data, err := os.ReadFile(file)
	return err == nil && json.Unmarshal(data, out) == nil
}

// readRunning is the job's run going now: its own running.json when it has written one, else the process this
// supervisor started.
func (j *Jobs) readRunning(job config.Job) (JobRunning, bool) {
	var r JobRunning
	if readJSON(j.statusFile(job, "running.json"), &r) && proc.Alive(r.PID) {
		return r, true
	}
	j.mu.Lock()
	mine, ok := j.running[job.Name]
	j.mu.Unlock()
	if ok && proc.Alive(mine.PID) {
		return mine, true
	}
	return JobRunning{}, false
}

func (j *Jobs) readLast(job config.Job) (JobResult, bool) {
	var r JobResult
	ok := readJSON(j.statusFile(job, "last.json"), &r)
	return r, ok
}

// dueSlot is the latest of the job's times within the last six hours, if any: a run missed by more than that
// waits for the next.
func dueSlot(job config.Job, now time.Time) string {
	type cand struct {
		hm string
		t  time.Time
	}
	var cands []cand
	now = now.UTC()
	for _, hm := range job.At {
		h, m, ok := strings.Cut(hm, ":")
		if !ok {
			continue
		}
		hh, err1 := strconv.Atoi(h)
		mm, err2 := strconv.Atoi(m)
		if err1 != nil || err2 != nil {
			continue
		}
		for back := 0; back <= 1; back++ {
			t := time.Date(now.Year(), now.Month(), now.Day()-back, hh, mm, 0, 0, time.UTC)
			if !t.After(now) && now.Sub(t) < 6*time.Hour {
				cands = append(cands, cand{hm, t})
			}
		}
	}
	if len(cands) == 0 {
		return ""
	}
	sort.Slice(cands, func(a, b int) bool { return cands[a].t.After(cands[b].t) })
	return cands[0].t.Format("2006-01-02") + " " + cands[0].hm
}

// runIfDue starts a timed job when its time comes, and notes how the last run ended. It runs detached, like a
// turn. extra is the environment a delivery adds.
func (j *Jobs) runIfDue(job config.Job, force bool, extra []string) {
	running, isRunning := j.readRunning(job)
	j.mu.Lock()
	_, wasRunning := j.running[job.Name]
	j.mu.Unlock()
	if wasRunning && !isRunning {
		last, ok := j.readLast(job)
		if ok {
			j.mu.Lock()
			j.last[job.Name] = last
			j.mu.Unlock()
			if last.Outcome == "busy" {
				// Another run had the checkout: try again in ten minutes, within the slot's six hours.
				j.p.Store.Job(job.Name, func(s *store.JobState) {
					retry := time.Now().Add(10 * time.Minute)
					s.LastSlot = ""
					s.RetryAfter = &retry
				})
			}
			msg := last.Slot + " " + last.Outcome
			if last.SHA != "" {
				msg += " " + last.SHA
			}
			if last.StoppedAt != "" {
				msg += " at " + last.StoppedAt
			}
			if len(last.Issues) > 0 {
				msg += fmt.Sprintf(", issues %v", last.Issues)
			}
			j.p.logf("%s: %s", job.Name, msg)
		} else {
			j.p.logf("%s: ended with no result", job.Name)
		}
		j.mu.Lock()
		delete(j.running, job.Name)
		j.mu.Unlock()
		j.p.hub.changed()
	}
	if isRunning {
		j.mu.Lock()
		j.running[job.Name] = running
		j.mu.Unlock()
		if extra != nil {
			j.p.logf("%s: the %s run is still going; this delivery doesn't start another", job.Name, running.Slot)
		}
		return
	}
	slot := dueSlot(job, time.Now())
	state := j.p.Store.Job(job.Name, nil)
	if !force {
		if state.LastSlot == "" && state.RetryAfter == nil {
			// First start with this job: wait for its next time rather than run a slot someone may already have run.
			j.p.Store.Job(job.Name, func(s *store.JobState) {
				s.LastSlot = slot
				if s.LastSlot == "" {
					s.LastSlot = "none"
				}
			})
			return
		}
		if slot == "" || state.LastSlot == slot {
			return
		}
		if state.RetryAfter != nil && time.Now().Before(*state.RetryAfter) {
			return
		}
	}
	j.p.Store.Job(job.Name, func(s *store.JobState) {
		if slot != "" {
			s.LastSlot = slot
		}
		s.RetryAfter = nil
	})
	cmd := exec.Command(job.Command[0], job.Command[1:]...)
	cmd.Dir = j.p.Root
	cmd.Env = j.env(extra)
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		j.p.logf("%s: couldn't start: %v", job.Name, err)
		return
	}
	go func() { _ = cmd.Wait() }()
	label := slot
	switch {
	case extra != nil:
		label = "on an event"
	case label == "":
		label = "by hand"
	}
	j.mu.Lock()
	j.running[job.Name] = JobRunning{PID: cmd.Process.Pid, Slot: label, StartedAt: time.Now().UTC()}
	j.mu.Unlock()
	j.p.logf("%s: started the %s run (pid %d)", job.Name, label, cmd.Process.Pid)
	j.p.hub.changed()
}

// Describe is a timed job's state in a few words, for the TUI header.
func (j *Jobs) Describe(job config.Job) string {
	if r, ok := j.Running(job.Name); ok {
		return fmt.Sprintf("running %s", time.Since(r.StartedAt).Round(time.Minute))
	}
	if l, ok := j.Last(job.Name); ok {
		return fmt.Sprintf("%s %s ago", l.Outcome, time.Since(l.FinishedAt).Round(time.Minute))
	}
	return "not run yet"
}
