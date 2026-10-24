package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/core"
)

// keepRuns is how many runs of a job are kept when the job doesn't say.
const keepRuns = 30

// JobRun is one run of a job: what asked for it, when it started and ended, how, and where its output is. Each
// is kept as <id>.json, with its output in <id>.log, in the project's job-runs/<job>/ folder, which a project's
// own health check can read to confirm each slot ran.
type JobRun struct {
	ID  string `json:"id"`
	Job string `json:"job"`
	// Trigger is what asked for the run: "cron 2026-10-21 09:00 Europe/London", "push 1a2b3c4", "webhook",
	// "every 10m", "by hand".
	Trigger   string     `json:"trigger"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	// Outcome is "running", "ok", "failed", "timed out", "couldn't start" or "checkout failed"; a detached run's
	// is what its last.json says.
	Outcome string `json:"outcome"`
	// Exit is the command's exit code, once known.
	Exit  *int   `json:"exit,omitempty"`
	Error string `json:"error,omitempty"`
	// Dir is where it ran; a fresh checkout's is kept for a day when the run fails.
	Dir string `json:"dir,omitempty"`
	// Updated is how many commits the main checkout was fast-forwarded by before the run. Behind is how many
	// of the base branch's remote head it ran without, and Stale why it couldn't catch up, so a health check
	// can tell a run on old code.
	Updated  int    `json:"updated,omitempty"`
	Behind   int    `json:"behind,omitempty"`
	Stale    string `json:"stale,omitempty"`
	Log      string `json:"log,omitempty"`
	Detached bool   `json:"detached,omitempty"`
}

// Took is how long the run took, or has taken so far.
func (r JobRun) Took() time.Duration {
	end := time.Now()
	if r.EndedAt != nil {
		end = *r.EndedAt
	}
	return end.Sub(r.StartedAt)
}

// runsDir is where a job's runs are kept.
func (j *Jobs) runsDir(job string) string {
	return filepath.Join(j.p.StateDir, "job-runs", safeName(job))
}

// newRun starts a run's record and opens its log.
func (j *Jobs) newRun(job config.Job, trigger string) (*JobRun, *os.File) {
	now := time.Now().UTC()
	run := &JobRun{ID: now.Format("20060102T150405.000000000Z"), Job: job.Name, Trigger: trigger, StartedAt: now, Outcome: "running"}
	dir := j.runsDir(job.Name)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		j.p.logf("%s: couldn't keep the run: %v", job.Name, err)
		return run, nil
	}
	run.Log = filepath.Join(dir, run.ID+".log")
	f, err := os.Create(run.Log)
	if err != nil {
		run.Log = ""
		f = nil
	}
	j.saveRun(*run)
	return run, f
}

// saveRun writes a run's record.
func (j *Jobs) saveRun(run JobRun) {
	data, err := json.MarshalIndent(run, "", "  ")
	if err != nil {
		return
	}
	file := filepath.Join(j.runsDir(run.Job), run.ID+".json")
	tmp := file + ".tmp"
	if os.WriteFile(tmp, data, 0o600) == nil {
		_ = os.Rename(tmp, file)
	}
}

// endRun records how a run ended and drops the job's oldest runs beyond what it keeps.
func (j *Jobs) endRun(job config.Job, run *JobRun) {
	now := time.Now().UTC()
	run.EndedAt = &now
	j.saveRun(*run)
	if j.p != nil && j.p.hub != nil {
		go j.p.hub.fact(core.Fact{Kind: core.CondJob, Project: j.p.Name, Ref: job.Name, On: run.Outcome})
	}
	keep := job.KeepRuns
	if keep <= 0 {
		keep = keepRuns
	}
	runs := j.Runs(job.Name)
	for _, old := range runs[min(len(runs), keep):] {
		base := filepath.Join(j.runsDir(job.Name), old.ID)
		_ = os.Remove(base + ".json")
		_ = os.Remove(base + ".log")
	}
}

// Runs are the job's kept runs, newest first.
func (j *Jobs) Runs(name string) []JobRun {
	dir := j.runsDir(name)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []JobRun
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		var r JobRun
		if readJSON(filepath.Join(dir, e.Name()), &r) && r.ID != "" {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID > out[b].ID })
	return out
}

// Run is one of the job's kept runs.
func (j *Jobs) Run(name, id string) (JobRun, bool) {
	var r JobRun
	ok := readJSON(filepath.Join(j.runsDir(name), safeName(id)+".json"), &r)
	return r, ok
}

// lastRun is the job's newest kept run, if any.
func (j *Jobs) lastRun(name string) (JobRun, bool) {
	runs := j.Runs(name)
	if len(runs) == 0 {
		return JobRun{}, false
	}
	return runs[0], true
}

// lastLine is the last line of a run's output that isn't blank.
func lastLine(file string) string {
	if file == "" {
		return ""
	}
	f, err := os.Open(file)
	if err != nil {
		return ""
	}
	defer f.Close()
	const tail = 4096
	if info, err := f.Stat(); err == nil && info.Size() > tail {
		_, _ = f.Seek(info.Size()-tail, 0)
	}
	buf := make([]byte, tail)
	n, _ := f.Read(buf)
	lines := strings.Split(strings.TrimSpace(string(buf[:n])), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
