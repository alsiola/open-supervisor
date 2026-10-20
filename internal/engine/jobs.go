package engine

import (
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
	"time"

	"gopkg.in/yaml.v3"

	"github.com/alsiola/open-supervisor/internal/config"
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
	return &Jobs{Specs: specs, p: p, busy: map[string]bool{}, running: map[string]JobRunning{}, last: map[string]JobResult{}}
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
				j.runCheck(job)
				t := time.NewTicker(time.Duration(job.EveryMinutes) * time.Minute)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						j.runCheck(job)
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
				j.runIfDue(job, false)
				t := time.NewTicker(time.Minute)
				defer t.Stop()
				for {
					select {
					case <-ctx.Done():
						return
					case <-t.C:
						j.runIfDue(job, false)
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
		go j.runIfDue(job, true)
	} else {
		go j.runCheck(job)
	}
}

func (j *Jobs) env() []string { return j.p.hub.sessionEnv(j.p, "") }

func (j *Jobs) runCheck(job config.Job) {
	j.mu.Lock()
	if j.busy[job.Name] {
		j.mu.Unlock()
		return
	}
	j.busy[job.Name] = true
	j.mu.Unlock()
	defer func() {
		j.mu.Lock()
		delete(j.busy, job.Name)
		j.mu.Unlock()
	}()
	timeout := job.TimeoutMinutes
	if timeout <= 0 {
		timeout = 5
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, job.Command[0], job.Command[1:]...)
	cmd.Dir = j.p.Root
	cmd.Env = j.env()
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
// turn.
func (j *Jobs) runIfDue(job config.Job, force bool) {
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
	cmd.Env = j.env()
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		j.p.logf("%s: couldn't start: %v", job.Name, err)
		return
	}
	go func() { _ = cmd.Wait() }()
	label := slot
	if label == "" {
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
