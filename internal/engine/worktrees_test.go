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
	// 4's PR has merged; 5's too, but it is parked on a wait; 6 has one PR open still.
	st.Create("4", "landed", "", "s4")
	st.Update("4", func(r *store.Record) { r.State, r.PRs, r.ClosedPRs = store.Review, []int{40}, []int{40} })
	st.Create("5", "parked", "", "s5")
	st.Update("5", func(r *store.Record) { r.State, r.PRs, r.ClosedPRs = store.Scheduled, []int{50}, []int{50} })
	st.Create("6", "open", "", "s6")
	st.Update("6", func(r *store.Record) { r.State, r.PRs, r.ClosedPRs = store.Review, []int{60, 61}, []int{60} })
	for _, key := range []string{"1", "2", "3", "4", "5", "6"} {
		_ = os.MkdirAll(worktree.Dir(data, "web", key), 0o755)
	}
	_ = os.MkdirAll(filepath.Join(data, "worktrees", "web", "job-build-1"), 0o755)
	stateDir := t.TempDir()
	all, spare := IssueWorktrees(data, stateDir, "web", st)
	if len(all) != 6 {
		t.Fatalf("all = %v, want the six issue worktrees and no job checkout", all)
	}
	if got := SpareNames(spare); got != "#2, issue-3, #4" {
		t.Fatalf("spare = %q, want the finished issue, the one no record holds and the landed one", got)
	}
	if !spare[2].Landed || spare[0].Landed {
		t.Fatalf("spare = %+v, want only #4 marked landed", spare)
	}
	// A task running from a finished issue's worktree keeps it.
	task := Task{Name: "fill", Issue: "2", Dir: worktree.Dir(data, "web", "2"), PID: os.Getpid(), StartedAt: time.Now().UTC(), Outcome: "running"}
	if err := SaveTask(TaskFile(stateDir, "2", "fill"), task); err != nil {
		t.Fatal(err)
	}
	if _, spare := IssueWorktrees(data, stateDir, "web", st); SpareNames(spare) != "issue-3, #4" {
		t.Fatalf("spare = %q, want the one a task runs from left out", SpareNames(spare))
	}
}

func TestRemoveSpareKeepsUnpushedWorkOfALandedIssue(t *testing.T) {
	dir := t.TempDir()
	origin, root := filepath.Join(dir, "origin.git"), filepath.Join(dir, "repo")
	gitIn(t, dir, "init", "-q", "--bare", "-b", "main", origin)
	gitIn(t, dir, "clone", "-q", origin, root)
	_ = os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644)
	gitIn(t, root, "add", ".")
	gitIn(t, root, "commit", "-qm", "init")
	gitIn(t, root, "push", "-q", "origin", "main")
	data := filepath.Join(dir, "data")
	setup := func(key string) string {
		path, err := worktree.Setup(worktree.Spec{Root: root, Path: worktree.Dir(data, "web", key), Branch: "agent/issue-" + key, Base: "main"})
		if err != nil {
			t.Fatal(err)
		}
		return path
	}
	pushed, unpushed := setup("1"), setup("2")
	_ = os.WriteFile(filepath.Join(unpushed, "b.txt"), []byte("b"), 0o644)
	gitIn(t, unpushed, "add", ".")
	gitIn(t, unpushed, "commit", "-qm", "not pushed")
	spare := []SpareWorktree{{Path: pushed, Key: "1", Landed: true}, {Path: unpushed, Key: "2", Landed: true}}
	if n := RemoveSpare(root, "origin", spare, t.Logf); n != 1 {
		t.Fatalf("removed %d, want 1", n)
	}
	if _, err := os.Stat(pushed); !os.IsNotExist(err) {
		t.Fatal("the landed worktree with everything pushed is still there")
	}
	if _, err := os.Stat(unpushed); err != nil {
		t.Fatal("the landed worktree with an unpushed commit was removed")
	}
}
