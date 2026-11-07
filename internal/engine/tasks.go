package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/core"
	hostinfo "github.com/alsiola/open-supervisor/internal/host"
	"github.com/alsiola/open-supervisor/internal/logx"
	"github.com/alsiola/open-supervisor/internal/proc"
	"github.com/alsiola/open-supervisor/internal/store"
	"github.com/alsiola/open-supervisor/internal/worktree"
)

// Task is a long command an agent started with `agent task start`: a backfill or a cache fill that runs past
// the turn, the agent tool's own limit on background commands, and the issue. It runs detached under a small
// wrapper (`agent task exec`) that writes how it ended here; the supervisor follows the records, tells the issue
// when one ends, and keeps a worktree a task runs from until the last one there ends. Each is kept as
// <issue>-<name>.json, with its output in <issue>-<name>.log, in the project's tasks/ folder, which a project's
// own health check can read.
type Task struct {
	Name    string   `json:"name"`
	Issue   string   `json:"issue"`
	Command []string `json:"command"`
	Dir     string   `json:"dir"`
	Log     string   `json:"log"`
	// PID is the wrapper's; the command runs in its process group.
	PID       int        `json:"pid,omitempty"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	// Outcome is "queued" (held until the host has room), "running", "ok", "failed", "stopped", "couldn't
	// start", "lost" (the wrapper went without saying how the command ended: the machine restarted, or it was
	// killed) or "held" (the host stayed busy for TaskHoldFor, so it never started).
	Outcome string `json:"outcome"`
	// QueuedAt is when a task the host was too busy for was registered; CheckedAt when the host was last
	// looked at for it, and Busy why it was busy then.
	QueuedAt  *time.Time `json:"queuedAt,omitempty"`
	CheckedAt *time.Time `json:"checkedAt,omitempty"`
	Busy      string     `json:"busy,omitempty"`
	Exit      *int       `json:"exit,omitempty"`
	Error     string     `json:"error,omitempty"`
	// Stop is set by `agent task stop` before it signals the task, so the wrapper records it as stopped
	// however the command takes the signal.
	Stop bool `json:"stop,omitempty"`
	// Reported is set once the supervisor has told the issue how the task ended.
	Reported bool `json:"reported,omitempty"`
}

// keepTasks is how long an ended task's record and log are kept after it was reported.
const keepTasks = 7 * 24 * time.Hour

// tasksEvery is how often the supervisor looks at the project's tasks; tests shorten it.
var tasksEvery = time.Minute

// A task registered while the host is busy waits: the supervisor looks at the host again every TaskRetryEvery
// and starts it once there is room, and gives up on it after TaskHoldFor. Tests shorten them.
var (
	TaskRetryEvery = 15 * time.Minute
	TaskHoldFor    = 2 * time.Hour
)

// hostBusy says why the host has no room for a new task now, or "" when it has; tests replace it.
var hostBusy = HostBusy

// HostBusy says why the host has no room for a new task now, or "" when it has (or can't be read): the 1 or
// 5-minute load per CPU or the memory in use over the host thresholds, or the OS saying memory pressure is
// critical.
func HostBusy(h config.Host) string {
	s, err := hostinfo.Read(nil)
	if err != nil {
		return ""
	}
	return busyReason(s, h)
}

func busyReason(s hostinfo.Sample, h config.Host) string {
	load, memory, _ := h.Thresholds()
	var why []string
	if s.HasLoad && s.CPUs > 0 {
		if per := max(s.Load[0], s.Load[1]) / float64(s.CPUs); per > load {
			why = append(why, fmt.Sprintf("load %.1f per CPU, over %.1f", per, load))
		}
	}
	switch {
	case s.MemTotal > 0 && s.MemoryUsedPct() > memory:
		why = append(why, fmt.Sprintf("memory %.0f%% in use, over %.0f%%", s.MemoryUsedPct(), memory))
	case s.MemoryCritical:
		why = append(why, "memory pressure critical")
	}
	return strings.Join(why, "; ")
}

// TasksDir is where a project's tasks are kept, under its state directory.
func TasksDir(stateDir string) string { return filepath.Join(stateDir, "tasks") }

// TaskFile is the record of the issue's task by that name.
func TaskFile(stateDir, issue, name string) string {
	return filepath.Join(TasksDir(stateDir), safeName(issue)+"-"+safeName(name)+".json")
}

// Running reports whether the task is still going.
func (t Task) Running() bool { return t.EndedAt == nil && t.Outcome == "running" && proc.Alive(t.PID) }

// Queued reports whether the task is registered and waiting for the host to have room.
func (t Task) Queued() bool { return t.EndedAt == nil && t.Outcome == "queued" }

// Live reports whether the task is going or waiting to start.
func (t Task) Live() bool { return t.Running() || t.Queued() }

// Took is how long the task ran, or has run so far.
func (t Task) Took() time.Duration {
	end := time.Now()
	if t.EndedAt != nil {
		end = *t.EndedAt
	}
	return end.Sub(t.StartedAt).Round(time.Second)
}

// ReadTask reads a task's record.
func ReadTask(file string) (Task, bool) {
	var t Task
	ok := readJSON(file, &t) && t.Name != ""
	return t, ok
}

// SaveTask writes a task's record.
func SaveTask(file string, t Task) error {
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
		return err
	}
	tmp := file + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, file)
}

// Tasks are the project's kept tasks, newest first.
func Tasks(stateDir string) []Task {
	files, _ := filepath.Glob(filepath.Join(TasksDir(stateDir), "*.json"))
	var out []Task
	for _, f := range files {
		if t, ok := ReadTask(f); ok {
			out = append(out, t)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].StartedAt.After(out[b].StartedAt) })
	return out
}

// TasksWithin are the tasks going now, or waiting to start, from the directory or under it.
func TasksWithin(stateDir, dir string) []Task {
	var out []Task
	for _, t := range Tasks(stateDir) {
		if t.Live() && within(t.Dir, dir) {
			out = append(out, t)
		}
	}
	return out
}

func within(path, dir string) bool {
	path, dir = filepath.Clean(path), filepath.Clean(dir)
	return path == dir || strings.HasPrefix(path, dir+string(filepath.Separator))
}

// TaskNames lists tasks by name, for a message.
func TaskNames(ts []Task) string {
	var names []string
	for _, t := range ts {
		names = append(names, t.Name)
	}
	return strings.Join(names, ", ")
}

// StartTask starts the wrapper for a task record already saved at file: self is the open-supervisor binary.
// The wrapper runs in its own session, so it outlives the agent's shell, its turn and the supervisor.
func StartTask(self, file string, t Task) (Task, error) {
	log, err := os.OpenFile(t.Log, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return t, err
	}
	defer log.Close()
	cmd := exec.Command(self, "agent", "task", "exec", file)
	cmd.Dir = t.Dir
	cmd.Stdout, cmd.Stderr = log, log
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		return t, err
	}
	t.PID = cmd.Process.Pid
	// The wrapper reads the record again only when the command ends, so this lands first.
	if err := SaveTask(file, t); err != nil {
		_ = proc.TerminateGroup(t.PID)
		return t, err
	}
	_ = cmd.Process.Release()
	return t, nil
}

// StopTask stops a running task and everything it started, marking it stopped first.
func StopTask(file string) (Task, error) {
	t, ok := ReadTask(file)
	if !ok {
		return t, fmt.Errorf("no task at %s", file)
	}
	if t.Queued() {
		now := time.Now().UTC()
		t.Stop, t.EndedAt, t.Outcome = true, &now, "stopped"
		return t, SaveTask(file, t)
	}
	if !t.Running() {
		return t, fmt.Errorf("the task %s isn't running (%s)", t.Name, t.Outcome)
	}
	t.Stop = true
	if err := SaveTask(file, t); err != nil {
		return t, err
	}
	return t, proc.TerminateGroup(t.PID)
}

// ExecTask is the wrapper: it runs the task's command in the task's directory, its output going where the
// wrapper's goes (the task's log), and writes how it ended into the record. A SIGTERM (`agent task stop`) to the
// process group ends the command and is recorded as stopped.
func ExecTask(file string) error {
	t, ok := ReadTask(file)
	if !ok {
		return fmt.Errorf("no task at %s", file)
	}
	if len(t.Command) == 0 {
		return errors.New("the task has no command")
	}
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGTERM, os.Interrupt)
	defer signal.Stop(stop)
	var stopped atomic.Bool
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		select {
		case <-stop:
			stopped.Store(true)
			cancel()
		case <-ctx.Done():
		}
	}()
	cmd := exec.CommandContext(ctx, t.Command[0], t.Command[1:]...)
	cmd.Dir = t.Dir
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.Cancel = func() error { return proc.Terminate(cmd.Process.Pid) }
	cmd.WaitDelay = 30 * time.Second
	err := cmd.Run()
	cancel()
	// The record as saved after the wrapper started, with its pid.
	if again, ok := ReadTask(file); ok {
		t = again
	}
	now := time.Now().UTC()
	t.EndedAt = &now
	if cmd.ProcessState != nil {
		code := cmd.ProcessState.ExitCode()
		t.Exit = &code
	}
	switch {
	case t.Stop || stopped.Load():
		t.Outcome = "stopped"
	case err == nil:
		t.Outcome = "ok"
	case cmd.ProcessState == nil:
		t.Outcome, t.Error = "couldn't start", logx.ErrText(err)
	default:
		t.Outcome = "failed"
	}
	return SaveTask(file, t)
}

// followTasks looks at the project's tasks every so often until ctx ends.
func (p *Project) followTasks(ctx context.Context) {
	t := time.NewTicker(tasksEvery)
	defer t.Stop()
	for {
		p.checkTasks()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// checkTasks tells each issue about its tasks that have ended since the last look, removes a finished issue's
// worktree once no task runs from it, and drops records long reported.
func (p *Project) checkTasks() {
	p.tasksMu.Lock()
	defer p.tasksMu.Unlock()
	files, _ := filepath.Glob(filepath.Join(TasksDir(p.StateDir), "*.json"))
	sort.Strings(files)
	busy, looked := "", false
	for _, file := range files {
		t, ok := ReadTask(file)
		if !ok {
			continue
		}
		if t.Queued() {
			if t.CheckedAt != nil && time.Since(*t.CheckedAt) < TaskRetryEvery {
				continue
			}
			if !looked {
				busy, looked = hostBusy(p.hub.settings().Host), true
			}
			var started bool
			if t, started = p.startQueued(file, t, busy); !started {
				continue
			}
			// It couldn't start, or the host stayed busy too long: say so below.
		}
		if t.Reported {
			if t.EndedAt != nil && time.Since(*t.EndedAt) > keepTasks {
				_ = os.Remove(file)
				_ = os.Remove(t.Log)
			}
			continue
		}
		if t.EndedAt == nil {
			if proc.Alive(t.PID) || t.PID == 0 && time.Since(t.StartedAt) < time.Minute {
				// Going, or saved by agent task start and not started yet.
				continue
			}
			// It may have ended between the read and the check.
			if again, ok := ReadTask(file); ok && again.EndedAt != nil {
				t = again
			} else {
				now := time.Now().UTC()
				t.EndedAt, t.Outcome = &now, "lost"
				if t.Stop {
					t.Outcome = "stopped"
				}
			}
		}
		p.taskEnded(t)
		t.Reported = true
		if err := SaveTask(file, t); err != nil {
			p.logf("%s: couldn't keep the %s task's record: %v", p.ref(t.Issue), t.Name, err)
		}
	}
}

// startQueued looks again at a task waiting for the host: it starts it when the host has room (busy is ""),
// and gives up on it once it has waited TaskHoldFor. It reports whether the task has ended without starting,
// to be told on the issue.
func (p *Project) startQueued(file string, t Task, busy string) (Task, bool) {
	now := time.Now().UTC()
	queued := now.Sub(t.StartedAt)
	if t.QueuedAt != nil {
		queued = now.Sub(*t.QueuedAt)
	}
	t.CheckedAt = &now
	if busy != "" {
		t.Busy = busy
		if queued >= TaskHoldFor {
			t.EndedAt, t.Outcome = &now, "held"
			return t, true
		}
		if err := SaveTask(file, t); err != nil {
			p.logf("%s: couldn't keep the %s task's record: %v", p.ref(t.Issue), t.Name, err)
		}
		return t, false
	}
	// agent task stop may have ended it since the read.
	if again, ok := ReadTask(file); !ok || !again.Queued() {
		return t, false
	}
	self, err := os.Executable()
	if err == nil {
		t.StartedAt, t.Outcome, t.Busy = now, "running", ""
		if err = SaveTask(file, t); err == nil {
			t, err = StartTask(self, file, t)
		}
	}
	if err != nil {
		t.EndedAt, t.Outcome, t.Error = &now, "couldn't start", logx.ErrText(err)
		return t, true
	}
	p.logf("%s: started the %s task, held %s for the host to have room", p.ref(t.Issue), t.Name, queued.Round(time.Minute))
	return t, false
}

// taskEnded says on the issue how a task ended, and tells its agent at the next turn unless the issue is
// finished. A finished issue's worktree the task kept is removed once nothing else runs from it.
func (p *Project) taskEnded(t Task) {
	text := TaskEndText(t)
	p.logf("%s: %s", p.ref(t.Issue), text)
	follow := " See how it went in the log, and carry on with what was waiting for it."
	if t.Outcome == "held" {
		follow = " Pause the work that needs it for now and end your turn with help: a person has to step in " +
			"(free up the host, or say to go on) before it is started again."
	}
	r := p.Store.Get(t.Issue)
	if !p.hub.settings().DryRun {
		ctx, cancel := ctxTimeout(30 * time.Second)
		if _, err := p.Tracker.Comment(ctx, t.Issue, text, core.AgentMark{From: t.Issue}); err != nil {
			p.callFailed(err, "%s: couldn't say the %s task ended", p.ref(t.Issue), t.Name)
		}
		cancel()
	}
	if r != nil && r.State != store.Finished {
		p.enqueue(t.Issue, core.Event{Type: core.EvNote, Text: text + follow, At: time.Now().UTC()})
		return
	}
	path := p.WorktreePath(t.Issue)
	if !within(t.Dir, path) || len(TasksWithin(p.StateDir, path)) > 0 {
		return
	}
	if _, err := os.Stat(path); err != nil {
		return
	}
	if err := worktree.Remove(worktree.Spec{Root: p.Root, Path: path}); err != nil {
		p.logf("%s: couldn't remove the worktree its tasks kept: %s", p.ref(t.Issue), logx.ErrText(err))
	} else {
		p.logf("%s: removed the worktree now its tasks have ended", p.ref(t.Issue))
	}
}

// TaskEndText says how a task ended, in a sentence or two with its log.
func TaskEndText(t Task) string {
	how := t.Outcome
	switch t.Outcome {
	case "ok":
		how = "finished"
	case "failed":
		how = "failed"
		if t.Exit != nil {
			how = fmt.Sprintf("failed (exit %d)", *t.Exit)
		}
	case "lost":
		how = "stopped without saying how (the machine restarted, or it was killed)"
	case "couldn't start":
		how = "couldn't start: " + t.Error
	case "held":
		waited := t.Took()
		if t.QueuedAt != nil && t.EndedAt != nil {
			waited = t.EndedAt.Sub(*t.QueuedAt).Round(time.Minute)
		}
		return fmt.Sprintf("The task `%s` (`%s`) never started: the host stayed too busy for it for %s (%s).",
			t.Name, strings.Join(t.Command, " "), waited, t.Busy)
	}
	s := fmt.Sprintf("The task `%s` (`%s`) %s after %s.", t.Name, strings.Join(t.Command, " "), how, t.Took())
	if last := lastLine(t.Log); last != "" {
		s += fmt.Sprintf(" Its last line: `%s`.", last)
	}
	return s + fmt.Sprintf(" Log: `%s`.", t.Log)
}
