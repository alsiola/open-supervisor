package engine

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/core"
	hostinfo "github.com/alsiola/open-supervisor/internal/host"
	"github.com/alsiola/open-supervisor/internal/proc"
	"github.com/alsiola/open-supervisor/internal/store"
)

// TestMain lets the test binary stand in for open-supervisor as a task's wrapper: StartTask runs
// `<self> agent task exec <record>`.
func TestMain(m *testing.M) {
	if len(os.Args) == 5 && os.Args[1] == "agent" && os.Args[2] == "task" && os.Args[3] == "exec" {
		if err := ExecTask(os.Args[4]); err != nil {
			os.Exit(1)
		}
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func startTask(t *testing.T, stateDir, dir, name string, command ...string) (string, Task) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("tasks run sh here")
	}
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	file := TaskFile(stateDir, "7", name)
	task := Task{Name: name, Issue: "7", Command: command, Dir: dir, Log: strings.TrimSuffix(file, ".json") + ".log", StartedAt: time.Now().UTC(), Outcome: "running"}
	if err := SaveTask(file, task); err != nil {
		t.Fatal(err)
	}
	if task, err = StartTask(self, file, task); err != nil {
		t.Fatal(err)
	}
	return file, task
}

func ended(t *testing.T, file string) Task {
	t.Helper()
	var got Task
	waitFor(t, "the task to end", func() bool {
		var ok bool
		got, ok = ReadTask(file)
		return ok && got.EndedAt != nil
	})
	return got
}

func TestTaskRecordsHowItEnded(t *testing.T) {
	state, dir := t.TempDir(), t.TempDir()
	file, task := startTask(t, state, dir, "fill", "sh", "-c", "echo filled 36 games; exit 3")
	if task.PID == 0 {
		t.Fatal("no pid recorded")
	}
	got := ended(t, file)
	if got.Outcome != "failed" || got.Exit == nil || *got.Exit != 3 {
		t.Fatalf("ended %+v", got)
	}
	if text := TaskEndText(got); !strings.Contains(text, "failed (exit 3)") || !strings.Contains(text, "filled 36 games") {
		t.Fatalf("end text %q", text)
	}
}

func TestTaskKilledBySignalSaysWhich(t *testing.T) {
	state, dir := t.TempDir(), t.TempDir()
	// What macOS or Linux does to a task when memory runs out: a SIGKILL nothing can catch, with nothing logged.
	file, _ := startTask(t, state, dir, "big", "sh", "-c", "echo scored 12 items; kill -KILL $$")
	got := ended(t, file)
	if got.Outcome != "failed" || got.Signal != "SIGKILL" {
		t.Fatalf("ended %+v", got)
	}
	text := TaskEndText(got)
	if !strings.Contains(text, "was killed by SIGKILL") || strings.Contains(text, "exit -1") || !strings.Contains(text, "memory") {
		t.Fatalf("end text %q", text)
	}
	if got := TaskFailure(Task{Outcome: "failed", Exit: new(int), Signal: "SIGTERM"}); got != "was killed by SIGTERM" {
		t.Fatalf("failure %q", got)
	}
}

func TestStoppedTaskStopsWhatItStarted(t *testing.T) {
	state, dir := t.TempDir(), t.TempDir()
	file, _ := startTask(t, state, dir, "backfill", "sh", "-c", "echo $$ > child.pid; sleep 60")
	if got := TasksWithin(state, dir); len(got) != 1 || got[0].Name != "backfill" {
		t.Fatalf("running within %s: %+v", dir, got)
	}
	waitFor(t, "the child to start", func() bool { _, err := os.Stat(filepath.Join(dir, "child.pid")); return err == nil })
	if _, err := StopTask(file); err != nil {
		t.Fatal(err)
	}
	if got := ended(t, file); got.Outcome != "stopped" {
		t.Fatalf("ended %+v", got)
	}
	if len(TasksWithin(state, dir)) != 0 {
		t.Fatal("a stopped task still counts as running")
	}
}

func TestEndedTaskIsToldOnTheIssueAndToTheAgent(t *testing.T) {
	_, p := setup(t, func(spec adapter.TurnSpec, _ *fakeTurn) core.Outcome { return review("done") })
	p.Store.Create("7", "Backfill", "https://fake/issues/7", "s-1")
	p.Store.Update("7", func(r *store.Record) { r.State = store.Review })
	file, _ := startTask(t, p.StateDir, t.TempDir(), "fill", "sh", "-c", "echo all 1525 done")
	ended(t, file)
	p.checkTasks()
	host.mu.Lock()
	posted := strings.Join(host.posted, "\n")
	host.mu.Unlock()
	if !strings.Contains(posted, "The task `fill`") || !strings.Contains(posted, "finished") || !strings.Contains(posted, "all 1525 done") {
		t.Fatalf("posted %q", posted)
	}
	waitFor(t, "a turn told about the task", func() bool {
		for _, s := range agent.turns() {
			if strings.Contains(s.Body, "The task `fill`") {
				return true
			}
		}
		return false
	})
	if got, _ := ReadTask(file); !got.Reported {
		t.Fatal("not marked reported")
	}
	n := len(host.posted)
	p.checkTasks()
	if len(host.posted) != n {
		t.Fatal("told twice")
	}
}

func TestTaskKeepsAFinishedIssuesWorktreeUntilItEnds(t *testing.T) {
	_, p := setup(t, func(spec adapter.TurnSpec, _ *fakeTurn) core.Outcome { return review("done") })
	p.Store.Create("7", "Backfill", "https://fake/issues/7", "s-1")
	wt := p.WorktreePath("7")
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	file, task := startTask(t, p.StateDir, wt, "backfill", "sh", "-c", "touch started; sleep 60")
	waitFor(t, "the task to start", func() bool { _, err := os.Stat(filepath.Join(wt, "started")); return err == nil })
	p.applyState("7", core.Finished)
	p.Store.Update("7", func(r *store.Record) { r.State = store.Finished })
	if _, err := os.Stat(wt); err != nil {
		t.Fatal("the worktree went while a task ran from it")
	}
	if !proc.Alive(task.PID) {
		t.Fatal("the task was stopped with the issue")
	}
	_, _ = StopTask(file)
	ended(t, file)
	p.checkTasks()
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Fatalf("the worktree is still there once the task ended: %v", err)
	}
}

// busyHost makes the host read as busy for why (or not, for "") to the tasks waiting for it, until the test
// ends; the setter changes why.
func busyHost(t *testing.T, why string) func(string) {
	t.Helper()
	var now atomic.Value
	now.Store(why)
	was := hostBusy
	hostBusy = func(config.Host) string { return now.Load().(string) }
	t.Cleanup(func() { hostBusy = was })
	return func(why string) { now.Store(why) }
}

func queueTask(t *testing.T, stateDir, dir, name string, queuedAgo time.Duration, command ...string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("tasks run sh here")
	}
	file := TaskFile(stateDir, "7", name)
	at := time.Now().UTC().Add(-queuedAgo)
	checked := at
	task := Task{Name: name, Issue: "7", Command: command, Dir: dir, Log: strings.TrimSuffix(file, ".json") + ".log",
		StartedAt: at, Outcome: "queued", QueuedAt: &at, CheckedAt: &checked, Busy: "load 3.0 per CPU, over 1.5"}
	if err := SaveTask(file, task); err != nil {
		t.Fatal(err)
	}
	return file
}

func TestQueuedTaskStartsOnceTheHostHasRoom(t *testing.T) {
	why := "load 3.0 per CPU, over 1.5"
	set := busyHost(t, why)
	_, p := setup(t, func(spec adapter.TurnSpec, _ *fakeTurn) core.Outcome { return review("done") })
	p.Store.Create("7", "Backfill", "https://fake/issues/7", "s-1")
	p.Store.Update("7", func(r *store.Record) { r.State = store.Review })
	dir := t.TempDir()
	file := queueTask(t, p.StateDir, dir, "fill", time.Minute, "sh", "-c", "echo filled")
	if !p.tasksRunning("7") || len(TasksWithin(p.StateDir, dir)) != 1 {
		t.Fatal("a queued task doesn't count as live")
	}
	p.checkTasks()
	if got, _ := ReadTask(file); !got.Queued() || got.CheckedAt.Before(time.Now().Add(-2*time.Minute)) {
		t.Fatalf("looked again before it was due, or started: %+v", got)
	}
	due := func() {
		got, _ := ReadTask(file)
		back := time.Now().UTC().Add(-TaskRetryEvery - time.Minute)
		got.CheckedAt = &back
		if err := SaveTask(file, got); err != nil {
			t.Fatal(err)
		}
	}
	due()
	p.checkTasks()
	if got, _ := ReadTask(file); !got.Queued() || got.Busy != why {
		t.Fatalf("started while the host was busy: %+v", got)
	}
	set("")
	due()
	p.checkTasks()
	got := ended(t, file)
	if got.Outcome != "ok" || got.PID == 0 {
		t.Fatalf("got %+v", got)
	}
	p.checkTasks()
	host.mu.Lock()
	posted := strings.Join(host.posted, "\n")
	host.mu.Unlock()
	if !strings.Contains(posted, "The task `fill`") || !strings.Contains(posted, "filled") {
		t.Fatalf("posted %q", posted)
	}
}

func TestQueuedTaskIsGivenUpAfterTheHoldAndTheAgentPauses(t *testing.T) {
	busyHost(t, "memory pressure critical")
	_, p := setup(t, func(spec adapter.TurnSpec, _ *fakeTurn) core.Outcome { return review("paused") })
	p.Store.Create("7", "Backfill", "https://fake/issues/7", "s-1")
	p.Store.Update("7", func(r *store.Record) { r.State = store.Review })
	file := queueTask(t, p.StateDir, t.TempDir(), "fill", TaskHoldFor+time.Minute, "sh", "-c", "echo filled")
	p.checkTasks()
	got, _ := ReadTask(file)
	if got.Outcome != "held" || got.EndedAt == nil || got.PID != 0 || !got.Reported {
		t.Fatalf("got %+v", got)
	}
	host.mu.Lock()
	posted := strings.Join(host.posted, "\n")
	host.mu.Unlock()
	if !strings.Contains(posted, "never started") || !strings.Contains(posted, "memory pressure critical") {
		t.Fatalf("posted %q", posted)
	}
	waitFor(t, "a turn told to pause", func() bool {
		for _, s := range agent.turns() {
			if strings.Contains(s.Body, "never started") && strings.Contains(s.Body, "Pause the work") {
				return true
			}
		}
		return false
	})
}

func TestStoppingAQueuedTaskDropsIt(t *testing.T) {
	file := queueTask(t, t.TempDir(), t.TempDir(), "fill", time.Minute, "sh", "-c", "echo filled")
	got, err := StopTask(file)
	if err != nil || got.Outcome != "stopped" || got.EndedAt == nil {
		t.Fatalf("got %+v, %v", got, err)
	}
	if again, _ := ReadTask(file); again.Live() {
		t.Fatal("still live")
	}
}

func TestBusyReason(t *testing.T) {
	h := config.Host{}
	if got := busyReason(hostinfo.Sample{CPUs: 4, Load: [3]float64{2, 2, 2}, HasLoad: true, MemTotal: 100, MemAvailable: 50}, h); got != "" {
		t.Errorf("a quiet host: %q", got)
	}
	if got := busyReason(hostinfo.Sample{CPUs: 4, Load: [3]float64{2, 8, 8}, HasLoad: true}, h); !strings.Contains(got, "load 2.0 per CPU") {
		t.Errorf("a high 5-minute load: %q", got)
	}
	if got := busyReason(hostinfo.Sample{CPUs: 4, MemTotal: 100, MemAvailable: 5}, h); !strings.Contains(got, "memory 95%") {
		t.Errorf("memory: %q", got)
	}
	if got := busyReason(hostinfo.Sample{CPUs: 4, MemTotal: 100, MemAvailable: 20, MemoryCritical: true}, h); got != "memory pressure critical" {
		t.Errorf("critical pressure: %q", got)
	}
}
