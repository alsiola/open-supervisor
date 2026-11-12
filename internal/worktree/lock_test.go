package worktree

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func lockRepo(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	root := filepath.Join(dir, "repo")
	_ = os.MkdirAll(root, 0o755)
	runGit(t, root, "init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(root, "a.txt"), []byte("a"), 0o644)
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-qm", "init")
	return dir, root
}

func TestSetupConcurrently(t *testing.T) {
	dir, root := lockRepo(t)
	const n = 8
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			key := fmt.Sprint(i)
			_, errs[i] = Setup(Spec{Root: root, Path: Dir(filepath.Join(dir, "data"), "demo", key), Branch: "agent/issue-" + key, Base: "main"})
		}()
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("setup %d: %v", i, err)
		}
	}
}

func TestSetupWaitsForTheLock(t *testing.T) {
	dir, root := lockRepo(t)
	spec := Spec{Root: root, Path: Dir(filepath.Join(dir, "data"), "demo", "1"), Branch: "agent/issue-1", Base: "main"}
	unlock := spec.lockWorktrees()
	done := make(chan error, 1)
	go func() {
		_, err := Setup(spec)
		done <- err
	}()
	select {
	case err := <-done:
		t.Fatalf("setup finished while another held the worktree lock: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	unlock()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("setup didn't go on once the lock was let go")
	}
}

func TestSetupRetriesAHalfWrittenEntry(t *testing.T) {
	dir, root := lockRepo(t)
	old := addRetries
	addRetries = []time.Duration{400 * time.Millisecond, 400 * time.Millisecond}
	t.Cleanup(func() { addRetries = old })
	// What another git worktree add leaves while it is part-way through: an entry whose commondir it has made
	// but not yet written, which it finishes a moment later.
	runGit(t, root, "worktree", "add", "-q", "--detach", filepath.Join(dir, "other"), "main")
	commondir := filepath.Join(root, ".git", "worktrees", "other", "commondir")
	whole, err := os.ReadFile(commondir)
	if err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(commondir, nil, 0o644)
	go func() {
		time.Sleep(150 * time.Millisecond)
		_ = os.WriteFile(commondir, whole, 0o644)
	}()
	var log strings.Builder
	spec := Spec{Root: root, Path: Dir(filepath.Join(dir, "data"), "demo", "1"), Branch: "agent/issue-1", Base: "main", Log: &log}
	if err := spec.addWorktree("--no-track", "-b", spec.Branch, spec.Path, "main"); err != nil {
		t.Fatalf("%v\n%s", err, log.String())
	}
	if !strings.Contains(log.String(), "trying again") {
		t.Fatalf("the add didn't need a second try, so the test doesn't show the retry:\n%s", log.String())
	}
	if !spec.toplevel() {
		t.Fatal("no worktree after the retry")
	}
}

func TestHalfWritten(t *testing.T) {
	for msg, want := range map[string]bool{
		"git worktree add -q -b x p origin/main: fatal: failed to read .git/worktrees/issue-120/commondir: Undefined error: 0": true,
		"git worktree add -q x p: fatal: 'p' already exists": false,
	} {
		if got := halfWritten(fmt.Errorf("%s", msg)); got != want {
			t.Errorf("halfWritten(%q) = %v, want %v", msg, got, want)
		}
	}
}
