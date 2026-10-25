package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/core"
	"github.com/alsiola/open-supervisor/internal/logx"
	"github.com/alsiola/open-supervisor/internal/proc"
	"github.com/alsiola/open-supervisor/internal/store"
	"github.com/alsiola/open-supervisor/internal/worktree"
)

// RepoFile is a project's own settings, kept in its checkout: the jobs the supervisor runs for it.
const RepoFile = ".open-supervisor.yaml"

// catchUp is how late a timed run may start: a slot missed by more than this waits for the next.
const catchUp = 6 * time.Hour

// jobMinute is a minute of a job's timeout; tests shorten it.
var jobMinute = time.Minute

// jobsCheck is how often the checkout's RepoFile is read again for jobs added, changed or taken out; tests
// shorten it.
var jobsCheck = time.Minute

// keepFailed is how long a failed run's fresh checkout is kept to look at.
const keepFailed = 24 * time.Hour

// JobRunning is what a detached job writes to running.json while it runs.
type JobRunning struct {
	PID       int       `json:"pid"`
	Slot      string    `json:"slot"`
	StartedAt time.Time `json:"startedAt"`
	Log       string    `json:"log,omitempty"`
}

// JobResult is what a detached job writes to last.json when it ends.
type JobResult struct {
	Slot       string    `json:"slot"`
	Outcome    string    `json:"outcome"`
	FinishedAt time.Time `json:"finishedAt"`
	StoppedAt  string    `json:"stoppedAt,omitempty"`
	SHA        string    `json:"sha,omitempty"`
	Issues     []any     `json:"issues,omitempty"`
}

// jobReq is a run asked for: by what, and the environment a delivery adds.
type jobReq struct {
	trigger  string
	extra    []string
	delivery bool
}

// Jobs are a project's own jobs (see config.Job): runs the supervisor waits for, one at a time per job with at
// most one more waiting, and long timed runs it starts detached and follows through their status files.
type Jobs struct {
	specs atomic.Pointer[[]config.Job]

	p *Project

	// timerMu guards ctx, cancel and timers: start, stop and refresh.
	timerMu sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	// timers are the jobs whose timers run, each as it was started and with what stops it.
	timers map[string]jobTimer
	// repoData is the checkout's RepoFile as last read, so a check reads it again only when it changed.
	repoData []byte

	// syncMu keeps two runs from catching the main checkout up at once.
	syncMu sync.Mutex

	mu      sync.Mutex
	busy    map[string]bool
	pending map[string]*jobReq // the run asked for while the job went
	current map[string]JobRun  // the run going now, of a job the supervisor waits for
	done    map[string]JobRun  // the last run that ended
	running map[string]JobRunning
	last    map[string]JobResult
	// detached is the run record of each detached job going now.
	detached map[string]*JobRun
}

// jobTimer is a job's timer: the job as it was started, and what stops it.
type jobTimer struct {
	job  config.Job
	stop context.CancelFunc
}

// loadJobs is the project's jobs: those in the config, then those in its checkout's .open-supervisor.yaml. A
// job from the checkout that isn't right is logged and left out.
func loadJobs(p *Project) *Jobs {
	data := readRepoFile(p.Root)
	specs := parseSpecs(p.Spec, data, p.logf)
	j := &Jobs{p: p, repoData: data, timers: map[string]jobTimer{}, busy: map[string]bool{}, pending: map[string]*jobReq{}, current: map[string]JobRun{},
		done: map[string]JobRun{}, running: map[string]JobRunning{}, last: map[string]JobResult{}, detached: map[string]*JobRun{}}
	j.SetSpecs(specs)
	for _, job := range specs {
		j.loadDone(job)
	}
	return j
}

// Specs are the project's jobs as last read.
func (j *Jobs) Specs() []config.Job {
	if s := j.specs.Load(); s != nil {
		return *s
	}
	return nil
}

// SetSpecs replaces the jobs, without starting or stopping their timers (see refresh).
func (j *Jobs) SetSpecs(specs []config.Job) { j.specs.Store(&specs) }

// loadDone takes the job's last ended run from its records.
func (j *Jobs) loadDone(job config.Job) {
	if r, ok := j.lastRun(job.Name); ok && r.EndedAt != nil {
		j.mu.Lock()
		j.done[job.Name] = r
		j.mu.Unlock()
	}
}

// jobSpecs are a project's jobs, from the config and its checkout's RepoFile; logf hears what is left out.
func jobSpecs(spec *config.Project, root string, logf func(string, ...any)) []config.Job {
	return parseSpecs(spec, readRepoFile(root), logf)
}

// readRepoFile is the checkout's RepoFile, nil when it has none.
func readRepoFile(root string) []byte {
	data, _ := os.ReadFile(filepath.Join(root, RepoFile))
	return data
}

// parseSpecs are the jobs in the config, then those in data, the checkout's RepoFile.
func parseSpecs(spec *config.Project, data []byte, logf func(string, ...any)) []config.Job {
	specs := append([]config.Job(nil), spec.Jobs...)
	if data != nil {
		var own struct {
			Jobs []config.Job `yaml:"jobs"`
		}
		if err := yaml.Unmarshal(data, &own); err != nil {
			logf("%s: %v", RepoFile, err)
		}
		for _, job := range own.Jobs {
			if errs := job.Validate(); len(errs) > 0 {
				logf("%s: left out: %s", RepoFile, strings.Join(errs, "; "))
				continue
			}
			specs = append(specs, job)
		}
	}
	return specs
}

// JobNames are the names of a project's jobs, for the agent commands that wait on one.
func JobNames(spec *config.Project) []string {
	var names []string
	for _, j := range jobSpecs(spec, config.ExpandHome(spec.Root), func(string, ...any) {}) {
		names = append(names, j.Name)
	}
	return names
}

// Running is the job's detached run going now, if any.
func (j *Jobs) Running(name string) (JobRunning, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	r, ok := j.running[name]
	return r, ok
}

// Last is how the job's last detached run ended, if known.
func (j *Jobs) Last(name string) (JobResult, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	r, ok := j.last[name]
	return r, ok
}

// Current is the job's run going now, if the supervisor is waiting for one.
func (j *Jobs) Current(name string) (JobRun, bool) {
	j.mu.Lock()
	defer j.mu.Unlock()
	r, ok := j.current[name]
	return r, ok
}

func (j *Jobs) start() {
	s := j.p.hub.settings()
	if !s.Jobs || s.DryRun {
		return
	}
	j.timerMu.Lock()
	j.ctx, j.cancel = context.WithCancel(context.Background())
	if j.timers == nil {
		j.timers = map[string]jobTimer{}
	}
	for _, job := range j.Specs() {
		j.startTimer(job)
	}
	ctx, check := j.ctx, jobsCheck
	j.timerMu.Unlock()
	// A job added to the checkout's RepoFile (a merge pulled in, a fast-forward before a run) starts without a
	// restart.
	go func() {
		t := time.NewTicker(check)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				j.refresh()
			}
		}
	}()
}

// startTimer starts what runs the job on its own: its every-n-minutes ticker, or the minute check of a timed
// job. A job that runs only on events or by hand has none. Called with timerMu held.
func (j *Jobs) startTimer(job config.Job) {
	if job.EveryMinutes <= 0 && !job.Timed() {
		return
	}
	ctx, cancel := context.WithCancel(j.ctx)
	j.timers[job.Name] = jobTimer{job: job, stop: cancel}
	every := func(d time.Duration, f func()) {
		go func() {
			f()
			t := time.NewTicker(d)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					f()
				}
			}
		}()
	}
	switch {
	case job.EveryMinutes > 0:
		trigger := fmt.Sprintf("every %dm", job.EveryMinutes)
		every(time.Duration(job.EveryMinutes)*time.Minute, func() { j.request(job, &jobReq{trigger: trigger}) })
	case job.Timed():
		if job.Detached() {
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
		}
		every(time.Minute, func() { j.runIfDue(job) })
	}
}

func (j *Jobs) stop() {
	j.timerMu.Lock()
	defer j.timerMu.Unlock()
	if j.cancel != nil {
		j.cancel()
	}
}

// refresh reads the jobs again when the checkout's RepoFile changed, and starts the timers of jobs
// added or changed and stops those of jobs changed or taken out; the rest run on undisturbed. A run going
// carries on. When the events or webhooks the jobs take change, the project's hooks are pointed again.
func (j *Jobs) refresh() {
	j.timerMu.Lock()
	if j.ctx == nil || j.ctx.Err() != nil {
		j.timerMu.Unlock()
		return
	}
	data := readRepoFile(j.p.Root)
	if bytes.Equal(data, j.repoData) {
		j.timerMu.Unlock()
		return
	}
	j.repoData = data
	old := j.Specs()
	specs := parseSpecs(j.p.Spec, data, j.p.logf)
	if reflect.DeepEqual(old, specs) {
		j.timerMu.Unlock()
		return
	}
	was := map[string]config.Job{}
	for _, job := range old {
		was[job.Name] = job
	}
	now := map[string]config.Job{}
	var added, changed, removed []string
	for _, job := range specs {
		now[job.Name] = job
		if prev, ok := was[job.Name]; !ok {
			added = append(added, job.Name)
		} else if !reflect.DeepEqual(prev, job) {
			changed = append(changed, job.Name)
		}
	}
	for _, job := range old {
		if _, ok := now[job.Name]; !ok {
			removed = append(removed, job.Name)
		}
	}
	hooksBefore := hookShape(j)
	j.SetSpecs(specs)
	for name, t := range j.timers {
		if job, ok := now[name]; !ok || !reflect.DeepEqual(job, t.job) {
			t.stop()
			delete(j.timers, name)
		}
	}
	for _, name := range removed {
		// A detached run of a job taken out is no longer followed, so it mustn't hold the checkout.
		j.mu.Lock()
		delete(j.running, name)
		j.mu.Unlock()
	}
	for _, job := range specs {
		if _, ok := was[job.Name]; !ok {
			j.loadDone(job)
		}
		if _, ok := j.timers[job.Name]; !ok {
			j.startTimer(job)
		}
	}
	rehook := hookShape(j) != hooksBefore
	j.timerMu.Unlock()
	var parts []string
	for _, p := range []struct {
		what  string
		names []string
	}{{"added", added}, {"changed", changed}, {"taken out", removed}} {
		if len(p.names) > 0 {
			parts = append(parts, p.what+" "+strings.Join(p.names, ", "))
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "reordered")
	}
	j.p.logf("jobs reloaded from %s: %s", RepoFile, strings.Join(parts, "; "))
	if rehook {
		j.p.hub.jobsChanged(j.p)
	}
	j.p.hub.changed()
}

// hookShape is what the project's hooks are built from: the events its jobs run on and the jobs that take
// webhooks.
func hookShape(j *Jobs) string {
	var hooked []string
	for _, job := range j.Webhooked() {
		hooked = append(hooked, job.Name)
	}
	return strings.Join(j.Events(), ",") + "|" + strings.Join(hooked, ",")
}

// RunNow runs a job now, whatever the time.
func (j *Jobs) RunNow(job config.Job) {
	if j.p.hub.settings().DryRun {
		return
	}
	if job.Detached() {
		go j.startDetachedNow(job, "by hand", nil)
	} else {
		j.request(job, &jobReq{trigger: "by hand"})
	}
}

// env is the environment of a run: the project's sessions', the job's own and what a delivery adds.
func (j *Jobs) env(job config.Job, run *JobRun, extra []string) []string {
	env := append(j.p.hub.sessionEnv(j.p, ""), "OPEN_SUPERVISOR_JOB="+job.Name, "OPEN_SUPERVISOR_JOB_TRIGGER="+run.Trigger, "OPEN_SUPERVISOR_JOB_RUN="+run.ID)
	return append(env, extra...)
}

// Events are the event names the jobs run on, without their actions: what the project's hooks subscribe to.
func (j *Jobs) Events() []string {
	var out []string
	for _, job := range j.Specs() {
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
	for _, job := range j.Specs() {
		if job.On != nil && job.On.Webhook {
			out = append(out, job)
		}
	}
	return out
}

// Find is the job with the name.
func (j *Jobs) Find(name string) (config.Job, bool) {
	for _, job := range j.Specs() {
		if job.Name == name {
			return job, true
		}
	}
	return config.Job{}, false
}

// matches reports whether a job runs on the event: one of its events names it ("push"), or it and its action
// ("release.published"), and its ref is on one of the job's branches when it has any. Paths are checked apart,
// since they need the files the push changed.
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
	var files []string
	known, looked := false, false
	for _, job := range j.Specs() {
		if !matches(job, ev) {
			continue
		}
		if len(job.On.Paths) > 0 && ev.Name == "push" {
			if !looked {
				files, known = j.changedFiles(ev)
				looked = true
			}
			switch {
			case !known:
				j.p.logf("%s: couldn't tell which files the push changed; running it anyway", job.Name)
			case !pathsMatch(job.On.Paths, files):
				j.p.logf("%s: not run on push %s: it changed no file the job's paths take", job.Name, short(ev.After))
				continue
			}
		}
		j.Trigger(job, ev)
	}
}

// zeroSHA is the head a push names for a branch created or deleted.
func zeroSHA(sha string) bool { return strings.Trim(sha, "0") == "" }

// changedFiles are the files a push changed, from git where it has both heads, else from the delivery.
func (j *Jobs) changedFiles(ev core.HookEvent) ([]string, bool) {
	if ev.After != "" && zeroSHA(ev.After) {
		return nil, true // a deleted branch changes nothing
	}
	if ev.Before != "" && ev.After != "" && !zeroSHA(ev.Before) {
		_ = exec.Command("git", "-C", j.p.Root, "fetch", "-q", j.p.git.RemoteName()).Run()
		out, err := exec.Command("git", "-C", j.p.Root, "diff", "--name-only", ev.Before, ev.After).Output()
		if err == nil {
			return strings.Fields(string(out)), true
		}
	}
	if len(ev.Files) > 0 {
		return ev.Files, true
	}
	return nil, false
}

// pathsMatch reports whether any of the files is taken by the patterns, as GitHub Actions reads them: a file
// is taken when the last pattern that matches it doesn't start with !.
func pathsMatch(patterns, files []string) bool {
	for _, f := range files {
		in := false
		for _, p := range patterns {
			neg := strings.HasPrefix(p, "!")
			if globRe(strings.TrimPrefix(p, "!")).MatchString(f) {
				in = !neg
			}
		}
		if in {
			return true
		}
	}
	return false
}

var (
	globMu    sync.Mutex
	globCache = map[string]*regexp.Regexp{}
)

// globRe is a path glob as a regexp: * and ? within a folder, ** across folders, [...] a class.
func globRe(glob string) *regexp.Regexp {
	globMu.Lock()
	defer globMu.Unlock()
	if re, ok := globCache[glob]; ok {
		return re
	}
	var b strings.Builder
	b.WriteString("^")
	for i := 0; i < len(glob); i++ {
		c := glob[i]
		switch {
		case strings.HasPrefix(glob[i:], "**/"):
			b.WriteString("(?:.*/)?")
			i += 2
		case strings.HasPrefix(glob[i:], "**"):
			b.WriteString(".*")
			i++
		case c == '*':
			b.WriteString("[^/]*")
		case c == '?':
			b.WriteString("[^/]")
		case c == '[':
			if end := strings.IndexByte(glob[i+1:], ']'); end >= 0 {
				class := glob[i+1 : i+1+end]
				if strings.HasPrefix(class, "!") {
					class = "^" + class[1:]
				}
				b.WriteString("[" + class + "]")
				i += end + 1
				continue
			}
			b.WriteString(`\[`)
		default:
			b.WriteString(regexp.QuoteMeta(string(c)))
		}
	}
	b.WriteString("$")
	re, err := regexp.Compile(b.String())
	if err != nil {
		re = regexp.MustCompile("^" + regexp.QuoteMeta(glob) + "$")
	}
	globCache[glob] = re
	return re
}

func short(sha string) string { return sha[:min(7, len(sha))] }

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
	extra := []string{"OPEN_SUPERVISOR_EVENT=" + ev.Name, "OPEN_SUPERVISOR_EVENT_ACTION=" + ev.Action, "OPEN_SUPERVISOR_EVENT_REF=" + ev.Ref, "OPEN_SUPERVISOR_EVENT_SHA=" + ev.After}
	if file, err := j.writeEvent(job, ev.Body); err != nil {
		j.p.logf("%s: couldn't keep the %s delivery: %v", job.Name, what, err)
	} else {
		extra = append(extra, "OPEN_SUPERVISOR_EVENT_FILE="+file)
	}
	trigger := what
	if ev.After != "" {
		trigger += " " + short(ev.After)
	}
	j.p.logf("%s: running on %s", job.Name, trigger)
	if job.Detached() {
		go j.startDetachedNow(job, trigger, extra)
	} else {
		// Queued here rather than in a goroutine, so the run after the current one is the latest delivery's.
		j.request(job, &jobReq{trigger: trigger, extra: extra, delivery: true})
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
	file := filepath.Join(dir, fmt.Sprintf("%s-%d.json", safeName(job.Name), time.Now().UnixNano()))
	return file, os.WriteFile(file, body, 0o600)
}

// request runs a job the supervisor waits for, or keeps the run for after the one going now.
func (j *Jobs) request(job config.Job, req *jobReq) {
	if j.begin(job.Name, req) {
		go j.loop(job, req)
	}
}

// begin marks the job running and reports true. When it's running already, the run waits for it and begin
// reports false: at most one waits, and a delivery's takes the place of a timer's, which it carries no news
// for.
func (j *Jobs) begin(name string, req *jobReq) bool {
	j.mu.Lock()
	defer j.mu.Unlock()
	if j.busy[name] {
		if old := j.pending[name]; old == nil || req.delivery {
			j.pending[name] = req
			if old == nil {
				j.p.logf("%s: the %s run waits for the one going now", name, req.trigger)
			}
		}
		return false
	}
	j.busy[name] = true
	return true
}

// loop runs the job, then the run asked for while it went, until none is left.
func (j *Jobs) loop(job config.Job, req *jobReq) {
	for {
		j.execute(job, req)
		j.mu.Lock()
		next := j.pending[job.Name]
		delete(j.pending, job.Name)
		if next == nil {
			delete(j.busy, job.Name)
			j.mu.Unlock()
			return
		}
		j.mu.Unlock()
		req = next
	}
}

// execute runs the job once and waits for it, in the main checkout or a fresh one, keeping its output.
func (j *Jobs) execute(job config.Job, req *jobReq) {
	run, file := j.newRun(job, req.trigger)
	var out io.Writer = io.Discard
	if file != nil {
		defer file.Close()
		out = file
	}
	j.setCurrent(job.Name, run)
	defer func() {
		j.endRun(job, run)
		j.mu.Lock()
		delete(j.current, job.Name)
		j.done[job.Name] = *run
		j.mu.Unlock()
		j.p.hub.changed()
	}()
	env := j.env(job, run, req.extra)
	dir := j.p.Root
	if job.Fresh() {
		j.sweepFresh(job)
		base := j.base()
		wt := j.p.Spec.Worktree
		_, _, disk := j.p.hub.settings().Host.Thresholds()
		spec := worktree.Spec{Root: j.p.Root, Remote: j.p.git.RemoteName(), Path: j.freshDir(job, run), Base: base, Copy: wt.Copy, Setup: wt.Setup, Disk: disk, Log: out}
		got, err := worktree.Fresh(spec)
		run.Dir = got
		if err != nil {
			run.Outcome, run.Error = "checkout failed", err.Error()
			fmt.Fprintf(out, "open-supervisor: %v\n", err)
			j.p.logf("%s: checkout failed: %s", job.Name, logx.ErrText(err))
			return
		}
		dir = got
		env = append(env, "OPEN_SUPERVISOR_BASE_BRANCH="+base)
		defer func() {
			if run.Outcome == "ok" {
				spec.Log = nil
				if err := worktree.Remove(spec); err == nil {
					run.Dir = ""
				}
			}
		}()
	} else {
		j.catchUp(job, run, out)
	}
	j.setCurrent(job.Name, run)

	timeout := job.TimeoutMinutes
	if timeout <= 0 {
		timeout = 5
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*jobMinute)
	defer cancel()
	cmd := exec.CommandContext(ctx, job.Command[0], job.Command[1:]...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.Stdout, cmd.Stderr = out, out
	// Its own process group, so a timeout stops everything it started.
	proc.Detach(cmd)
	cmd.Cancel = func() error { return proc.TerminateGroup(cmd.Process.Pid) }
	cmd.WaitDelay = 10 * time.Second
	err := cmd.Run()
	if cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode()
		run.Exit = &code
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		run.Outcome = "timed out"
		j.p.logf("%s timed out after %dm", job.Name, timeout)
	case err == nil:
		run.Outcome = "ok"
	case cmd.ProcessState == nil:
		run.Outcome, run.Error = "couldn't start", logx.ErrText(err)
		j.p.logf("%s: couldn't start: %s", job.Name, run.Error)
	default:
		run.Outcome = "failed"
		j.p.logf("%s failed: %s %s", job.Name, logx.ErrText(err), lastLine(run.Log))
	}
}

func (j *Jobs) setCurrent(name string, run *JobRun) {
	j.mu.Lock()
	j.current[name] = *run
	j.mu.Unlock()
	j.p.hub.changed()
}

// catchUp fast-forwards the main checkout to the base branch's remote head before a run in it, and records
// on the run how it stood. While another job runs in the checkout, it only counts how far behind it is.
func (j *Jobs) catchUp(job config.Job, run *JobRun, out io.Writer) {
	j.syncMu.Lock()
	defer j.syncMu.Unlock()
	base := j.base()
	spec := worktree.Spec{Root: j.p.Root, Remote: j.p.git.RemoteName(), Base: base}
	var got worktree.Sync
	if other := j.inMain(job.Name); other != "" {
		got = worktree.Behind(spec)
		if got.Behind > 0 && got.Stale == "" {
			got.Stale = other + " was running in the checkout"
		}
	} else {
		got = worktree.CatchUp(spec)
	}
	run.Dir, run.Updated, run.Behind, run.Stale = j.p.Root, got.Updated, got.Behind, got.Stale
	if !run.Detached {
		// Seen as in the checkout before the next run checks.
		j.setCurrent(job.Name, run)
	}
	switch {
	case got.Updated > 0:
		fmt.Fprintf(out, "open-supervisor: fast-forwarded the checkout %d commit(s) to %s/%s\n", got.Updated, spec.Remote, base)
		// The fast-forward may have brought jobs in.
		go j.refresh()
	case got.Behind > 0:
		fmt.Fprintf(out, "open-supervisor: running %d commit(s) behind %s/%s: %s\n", got.Behind, spec.Remote, base, got.Stale)
		j.p.logf("%s: running %d commit(s) behind %s/%s: %s", job.Name, got.Behind, spec.Remote, base, got.Stale)
	case got.Stale != "":
		fmt.Fprintf(out, "open-supervisor: %s\n", got.Stale)
		j.p.logf("%s: %s", job.Name, got.Stale)
	}
}

// inMain is another job running in the main checkout now, if any.
func (j *Jobs) inMain(name string) string {
	j.mu.Lock()
	defer j.mu.Unlock()
	for other, r := range j.current {
		if other != name && r.Dir == j.p.Root {
			return other
		}
	}
	for other := range j.running {
		if other != name {
			return other
		}
	}
	return ""
}

// base is the branch a fresh checkout starts from.
func (j *Jobs) base() string {
	if j.p.base != "" {
		return j.p.base
	}
	if j.p.Repo != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if b, err := j.p.Repo.DefaultBranch(ctx); err == nil && b != "" {
			return b
		}
	}
	return "main"
}

// freshDir is where a run's fresh checkout goes, beside the project's issue worktrees.
func (j *Jobs) freshDir(job config.Job, run *JobRun) string {
	return filepath.Join(j.p.hub.dataDir(), "worktrees", j.p.Name, "job-"+safeName(job.Name)+"-"+run.ID)
}

// sweepFresh removes the job's fresh checkouts kept from failed runs more than a day ago.
func (j *Jobs) sweepFresh(job config.Job) {
	old, _ := filepath.Glob(filepath.Join(j.p.hub.dataDir(), "worktrees", j.p.Name, "job-"+safeName(job.Name)+"-*"))
	for _, dir := range old {
		if info, err := os.Stat(dir); err == nil && time.Since(info.ModTime()) > keepFailed {
			_ = worktree.Remove(worktree.Spec{Root: j.p.Root, Path: dir})
		}
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
	if job.Cron != "" {
		return cronSlot(job, now)
	}
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
			if !t.After(now) && now.Sub(t) < catchUp {
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

// cronSlot is the latest time of a cron job's within the last six hours, if any, named by its wall-clock time
// in the job's zone. The cron library only steps forward, so it steps from six hours ago to now.
//
// On a night the clocks change, a time they skip (01:30 London on the last Sunday in March) has no run that
// day, and a time they repeat (01:30 on the last Sunday in October) comes twice but is one slot by its name, so
// it runs once. A schedule within the hour, like */30, runs at each of the repeated times it hasn't run yet.
func cronSlot(job config.Job, now time.Time) string {
	sched, err := job.Schedule()
	if err != nil {
		return ""
	}
	loc, err := job.Location()
	if err != nil {
		return ""
	}
	var latest time.Time
	for t := sched.Next(now.Add(-catchUp).In(loc)); !t.IsZero() && !t.After(now); t = sched.Next(t) {
		latest = t
	}
	if latest.IsZero() {
		return ""
	}
	return latest.In(loc).Format("2006-01-02 15:04") + " " + loc.String()
}

// slotTrigger names a timed run for its record.
func slotTrigger(job config.Job, slot string) string {
	if job.Cron != "" {
		return "cron " + slot
	}
	return "at " + slot + " UTC"
}

// runIfDue starts a timed job when its time comes.
func (j *Jobs) runIfDue(job config.Job) {
	if job.Detached() && j.followDetached(job) {
		return
	}
	slot := dueSlot(job, time.Now())
	state := j.p.Store.Job(job.Name, nil)
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
	j.p.Store.Job(job.Name, func(s *store.JobState) {
		s.LastSlot = slot
		s.RetryAfter = nil
	})
	if job.Detached() {
		j.startDetached(job, slot, slotTrigger(job, slot), nil)
	} else {
		j.request(job, &jobReq{trigger: slotTrigger(job, slot)})
	}
}

// followDetached notes how a detached job's last run ended once it has, and reports whether one is going.
func (j *Jobs) followDetached(job config.Job) bool {
	running, isRunning := j.readRunning(job)
	j.mu.Lock()
	_, wasRunning := j.running[job.Name]
	j.mu.Unlock()
	if wasRunning && !isRunning {
		outcome := "no result"
		last, ok := j.readLast(job)
		if ok {
			outcome = last.Outcome
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
		j.endDetached(job, outcome)
		j.mu.Lock()
		delete(j.running, job.Name)
		j.mu.Unlock()
		j.p.hub.changed()
	}
	if isRunning {
		j.mu.Lock()
		j.running[job.Name] = running
		j.mu.Unlock()
	}
	return isRunning
}

// endDetached closes the record of a detached job's run: the one this supervisor started, else the newest
// left going by one before it.
func (j *Jobs) endDetached(job config.Job, outcome string) {
	j.mu.Lock()
	run := j.detached[job.Name]
	delete(j.detached, job.Name)
	j.mu.Unlock()
	if run == nil {
		if r, ok := j.lastRun(job.Name); ok && r.Detached && r.EndedAt == nil {
			run = &r
		}
	}
	if run == nil {
		return
	}
	j.mu.Lock()
	run.Outcome = outcome
	j.endRun(job, run)
	j.done[job.Name] = *run
	j.mu.Unlock()
}

// startDetachedNow starts a detached job's run now, by hand or on a delivery, unless one is going.
func (j *Jobs) startDetachedNow(job config.Job, trigger string, extra []string) {
	if j.followDetached(job) {
		if r, ok := j.Running(job.Name); ok && extra != nil {
			j.p.logf("%s: the %s run is still going; this delivery doesn't start another", job.Name, r.Slot)
		}
		return
	}
	slot := dueSlot(job, time.Now())
	j.p.Store.Job(job.Name, func(s *store.JobState) {
		if slot != "" {
			s.LastSlot = slot
		}
		s.RetryAfter = nil
	})
	label := "by hand"
	if extra != nil {
		label = "on an event"
	}
	j.startDetached(job, label, trigger, extra)
}

// startDetached starts a long run that outlives the supervisor, its output going to the run's log.
func (j *Jobs) startDetached(job config.Job, label, trigger string, extra []string) {
	run, file := j.newRun(job, trigger)
	run.Detached = true
	if file != nil {
		j.catchUp(job, run, file)
	} else {
		j.catchUp(job, run, io.Discard)
	}
	cmd := exec.Command(job.Command[0], job.Command[1:]...)
	cmd.Dir = j.p.Root
	cmd.Env = j.env(job, run, extra)
	if file != nil {
		cmd.Stdout, cmd.Stderr = file, file
	}
	proc.Detach(cmd)
	err := cmd.Start()
	if file != nil {
		_ = file.Close()
	}
	if err != nil {
		run.Outcome, run.Error = "couldn't start", logx.ErrText(err)
		j.endRun(job, run)
		j.p.logf("%s: couldn't start: %v", job.Name, err)
		return
	}
	j.saveRun(*run)
	j.mu.Lock()
	j.running[job.Name] = JobRunning{PID: cmd.Process.Pid, Slot: label, StartedAt: time.Now().UTC()}
	j.detached[job.Name] = run
	j.mu.Unlock()
	go func() {
		_ = cmd.Wait()
		code := cmd.ProcessState.ExitCode()
		j.mu.Lock()
		if j.detached[job.Name] == run {
			run.Exit = &code
			j.saveRun(*run)
		}
		j.mu.Unlock()
	}()
	j.p.logf("%s: started the %s run (pid %d)", job.Name, label, cmd.Process.Pid)
	j.p.hub.changed()
}

// Describe is a job's state in a few words, for the TUI.
func (j *Jobs) Describe(job config.Job) string {
	if r, ok := j.Running(job.Name); ok {
		return fmt.Sprintf("running %s", time.Since(r.StartedAt).Round(time.Minute))
	}
	if r, ok := j.Current(job.Name); ok {
		return fmt.Sprintf("running %s", time.Since(r.StartedAt).Round(time.Second))
	}
	if job.Detached() {
		if l, ok := j.Last(job.Name); ok {
			return fmt.Sprintf("%s %s ago", l.Outcome, time.Since(l.FinishedAt).Round(time.Minute))
		}
	}
	j.mu.Lock()
	r, ok := j.done[job.Name]
	j.mu.Unlock()
	if ok && r.EndedAt != nil {
		return fmt.Sprintf("%s %s ago", r.Outcome, time.Since(*r.EndedAt).Round(time.Minute))
	}
	return "not run yet"
}
