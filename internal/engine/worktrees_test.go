package engine

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alsiola/open-supervisor/internal/store"
	"github.com/alsiola/open-supervisor/internal/worktree"
)

func TestIssueWorktrees(t *testing.T) {
	data := t.TempDir()
	st := store.New(t.TempDir(), nil)
	st.Create("1", "live", "", "s1")
	st.Create("2", "finished", "", "s2")
	st.Update("2", func(r *store.Record) { r.State = store.Finished })
	for _, key := range []string{"1", "2", "3"} {
		_ = os.MkdirAll(worktree.Dir(data, "web", key), 0o755)
	}
	_ = os.MkdirAll(filepath.Join(data, "worktrees", "web", "job-build-1"), 0o755)
	stateDir := t.TempDir()
	all, spare := IssueWorktrees(data, stateDir, "web", st)
	if len(all) != 3 {
		t.Fatalf("all = %v, want the three issue worktrees and no job checkout", all)
	}
	if got := SpareNames(spare); got != "#2, issue-3" {
		t.Fatalf("spare = %q, want the finished issue and the one no record holds", got)
	}
	// A task running from a finished issue's worktree keeps it.
	task := Task{Name: "fill", Issue: "2", Dir: worktree.Dir(data, "web", "2"), PID: os.Getpid(), StartedAt: time.Now().UTC(), Outcome: "running"}
	if err := SaveTask(TaskFile(stateDir, "2", "fill"), task); err != nil {
		t.Fatal(err)
	}
	if _, spare := IssueWorktrees(data, stateDir, "web", st); SpareNames(spare) != "issue-3" {
		t.Fatalf("spare = %q, want the one a task runs from left out", SpareNames(spare))
	}
}
