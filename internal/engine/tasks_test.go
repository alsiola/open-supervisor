package engine

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/core"
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
