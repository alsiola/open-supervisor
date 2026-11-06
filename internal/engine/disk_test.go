package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alsiola/open-supervisor/internal/store"
	"github.com/alsiola/open-supervisor/internal/worktree"
)

func TestParkedWorktrees(t *testing.T) {
	data, stateDir := t.TempDir(), t.TempDir()
	st := store.New(t.TempDir(), nil)
	ended := func(ago time.Duration) *store.Turn {
		at := time.Now().Add(-ago)
		return &store.Turn{StartedAt: at.Add(-time.Minute), EndedAt: &at}
	}
	// 1 and 2 are up for review with a PR open, 2 used longer ago; 3 asks for help; 4 is running; 5 is building;
	// 6's PRs have all landed, so it is spare, not parked; 7 has a task running from it.
	set := map[string]func(r *store.Record){
		"1": func(r *store.Record) { r.State, r.PRs, r.LastTurn = store.Review, []int{10}, ended(time.Hour) },
		"2": func(r *store.Record) { r.State, r.PRs, r.LastTurn = store.Review, []int{20}, ended(48*time.Hour) },
		"3": func(r *store.Record) { r.State, r.LastTurn = store.Help, ended(2*time.Hour) },
		"4": func(r *store.Record) { r.State = store.Running },
		"5": func(r *store.Record) { r.State, r.PRs = store.Building, []int{50} },
		"6": func(r *store.Record) { r.State, r.PRs, r.ClosedPRs = store.Review, []int{60}, []int{60} },
		"7": func(r *store.Record) { r.State = store.Done },
	}
	for key, f := range set {
		st.Create(key, "t", "", "s"+key)
		st.Update(key, f)
		_ = os.MkdirAll(worktree.Dir(data, "web", key), 0o755)
	}
	task := Task{Name: "fill", Issue: "7", Dir: worktree.Dir(data, "web", "7"), PID: os.Getpid(), StartedAt: time.Now().UTC(), Outcome: "running"}
	if err := SaveTask(TaskFile(stateDir, "7", "fill"), task); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, w := range ParkedWorktrees(data, stateDir, "web", st) {
		keys = append(keys, w.Key)
	}
	if got := strings.Join(keys, ","); got != "2,3,1" {
		t.Fatalf("parked = %v, want 2, 3, 1: review and help, least recently used first", keys)
	}
}

func TestEvictParked(t *testing.T) {
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
	old := BigScratch
	BigScratch = 1000
	t.Cleanup(func() { BigScratch = old })
	// 1 has a commit not pushed; 2 keeps a big cache in .local/; 3 has started a turn since it was listed; 4 and
	// 5 can go, but the disk is back under its threshold once one has.
	var parked []ParkedWorktree
	for _, key := range []string{"1", "2", "3", "4", "5"} {
		parked = append(parked, ParkedWorktree{Path: setup(key), Key: key})
	}
	_ = os.WriteFile(filepath.Join(parked[0].Path, "b.txt"), []byte("b"), 0o644)
	gitIn(t, parked[0].Path, "add", ".")
	gitIn(t, parked[0].Path, "commit", "-qm", "not pushed")
	_ = os.WriteFile(filepath.Join(parked[1].Path, worktree.Scratch, "cache"), make([]byte, 2000), 0o644)
	still := func(key string) bool { return key != "3" }
	full := func() bool {
		_, err := os.Stat(parked[3].Path)
		return err == nil
	}
	removed := EvictParked(root, "origin", parked, still, full, t.Logf)
	if len(removed) != 1 || removed[0] != "4" {
		t.Fatalf("removed %v, want only 4", removed)
	}
	for i, w := range parked {
		_, err := os.Stat(w.Path)
		if gone := os.IsNotExist(err); gone != (i == 3) {
			t.Errorf("worktree %s gone = %v", w.Key, gone)
		}
	}
	if err := exec.Command("git", "-C", root, "show-ref", "--verify", "-q", "refs/heads/agent/issue-4").Run(); err != nil {
		t.Error("the evicted worktree's branch went with it")
	}
	if got := BigScratches([]string{parked[1].Path, parked[2].Path}); len(got) != 1 || got[0] != "issue-2 (2 KB)" {
		t.Errorf("BigScratches = %v, want issue-2 alone", got)
	}
}
