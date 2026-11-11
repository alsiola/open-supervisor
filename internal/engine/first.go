package engine

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/alsiola/open-supervisor/internal/config"
	hostinfo "github.com/alsiola/open-supervisor/internal/host"
	"github.com/alsiola/open-supervisor/internal/logx"
	"github.com/alsiola/open-supervisor/internal/proc"
)

// A job with priority high comes first on the host (see config.Job): while it runs, and for its leadMinutes
// before each of its times, the supervisor starts no new turns and no new tasks. Turns and tasks already going
// carry on.

// firstGrace is how long after a priority job's time the lead hold lasts while the job's minute check gets to
// starting it, so no turn slips in between the two.
const firstGrace = 2 * time.Minute

// firstFile is where the supervisor says, while it lasts, that a job comes first, so `agent task start` in an
// agent's shell holds a new task too.
func firstFile(stateDir string) string { return filepath.Join(stateDir, "job-first.json") }

// firstNote is what firstFile holds: why new work waits, and the supervisor that said so.
type firstNote struct {
	Reason string    `json:"reason"`
	PID    int       `json:"pid"`
	Since  time.Time `json:"since"`
}

// JobFirst says why a job that comes first holds new tasks back now, or "" when none does: read from what the
// supervisor running from stateDir wrote, and only while that supervisor is alive.
func JobFirst(stateDir string) string {
	var n firstNote
	if !readJSON(firstFile(stateDir), &n) || n.Reason == "" || !proc.Alive(n.PID) {
		return ""
	}
	return n.Reason
}

// firstNow lists the priority jobs that hold new work back now: those running, then those whose time is
// within their lead.
func (j *Jobs) firstNow(now time.Time) []string {
	var going, due []string
	for _, job := range j.Specs() {
		if !job.First() {
			continue
		}
		j.mu.Lock()
		_, running := j.running[job.Name]
		busy := j.busy[job.Name]
		j.mu.Unlock()
		if running || busy {
			going = append(going, fmt.Sprintf("the %s job (%s) is running", job.Name, j.p.Name))
			continue
		}
		if job.LeadMinutes <= 0 {
			continue
		}
		next := job.NextSlot(now.Add(-firstGrace))
		if !next.IsZero() && !next.After(now.Add(time.Duration(job.LeadMinutes)*time.Minute)) {
			due = append(due, fmt.Sprintf("the %s job (%s) starts at %s", job.Name, j.p.Name, next.Format("15:04 MST")))
		}
	}
	return append(going, due...)
}

// jobFirst says why new turns and tasks wait for a job that comes first now, or "" when none does.
func (s *Supervisor) jobFirst() string {
	st := s.settings()
	if !st.Jobs || st.DryRun {
		return ""
	}
	var why []string
	now := time.Now()
	for _, p := range s.Projects() {
		if p.Jobs != nil {
			why = append(why, p.Jobs.firstNow(now)...)
		}
	}
	return strings.Join(why, "; ")
}

// JobFirst says why new turns and tasks wait for a job that comes first, as last looked at; "" when they don't.
func (s *Supervisor) JobFirst() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.first
}

// refreshFirst looks again at whether a job comes first, and when that changed logs it, says so to `agent
// task start` through firstFile, and once the hold lifts looks at the tasks waiting for it. It returns why new
// work waits now.
func (s *Supervisor) refreshFirst() string {
	why := s.jobFirst()
	s.mu.Lock()
	was := s.first
	s.first = why
	s.mu.Unlock()
	if why == was {
		return why
	}
	file := firstFile(s.state)
	if why == "" {
		logx.Default.Printf("no job comes first now: new turns and tasks start again")
		_ = os.Remove(file)
		for _, p := range s.Projects() {
			go p.checkTasks()
		}
		return why
	}
	logx.Default.Printf("new turns and tasks wait: %s", why)
	if data, err := json.Marshal(firstNote{Reason: why, PID: os.Getpid(), Since: time.Now().UTC()}); err == nil {
		_ = os.MkdirAll(s.state, 0o755)
		tmp := file + ".tmp"
		if os.WriteFile(tmp, data, 0o644) == nil {
			_ = os.Rename(tmp, file)
		}
	}
	return why
}

// runProps are the telemetry properties of a job run starting or ending: with the host's load then, so a slow
// run can be told from a busy host.
func runProps(p *Project, job config.Job, run JobRun) map[string]any {
	props := map[string]any{"project": p.Name, "job": job.Name, "trigger": run.Trigger, "detached": run.Detached,
		"priority": job.First()}
	if run.EndedAt != nil {
		props["outcome"] = run.Outcome
		props["seconds"] = run.EndedAt.Sub(run.StartedAt).Seconds()
		if run.Exit != nil {
			props["exit"] = *run.Exit
		}
	}
	if sample, err := hostinfo.Read(nil); err == nil {
		props["cpus"] = sample.CPUs
		if sample.HasLoad {
			props["load_1"] = sample.Load[0]
			props["load_per_cpu"] = sample.LoadPerCPU()
		}
		if sample.MemTotal > 0 {
			props["memory_used_pct"] = sample.MemoryUsedPct()
		}
	}
	return props
}
