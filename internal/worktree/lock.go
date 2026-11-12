package worktree

import (
	"os"
	"path/filepath"
	"strings"
	"time"
)

// lockName is the file, in the project's git directory, that changes to its worktrees are made under.
const lockName = "open-supervisor-worktrees"

// lockWorktrees waits until no other process is adding, repairing, removing or pruning the project's worktrees,
// and returns the function that lets the next one go. git reads every entry under .git/worktrees when it adds
// one, and fails on one another add has only half written; a prune at that moment could delete the other's
// half-written entry. Agents set up their worktrees from separate processes, so the lock is a file. When it
// can't be taken (a git directory it can't find or write) the change goes ahead unlocked, as it did before.
func (s Spec) lockWorktrees() func() {
	dir, err := git(s.Root, "rev-parse", "--git-common-dir")
	if err != nil {
		return func() {}
	}
	if !filepath.IsAbs(dir) {
		dir = filepath.Join(s.Root, dir)
	}
	f, err := os.OpenFile(filepath.Join(dir, lockName), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		s.logf("couldn't open the worktree lock, carrying on without it: %v", err)
		return func() {}
	}
	if err := lockFile(f); err != nil {
		f.Close()
		s.logf("couldn't take the worktree lock, carrying on without it: %v", err)
		return func() {}
	}
	return func() { f.Close() }
}

// addWorktree runs git worktree add under the lock. A worktree being added by something that doesn't take the
// lock (a person, another tool) can still leave a half-written entry that makes git fail, so it tries again a
// few times when that is the error, clearing what the failed try left first.
func (s Spec) addWorktree(args ...string) error {
	unlock := s.lockWorktrees()
	defer unlock()
	var err error
	for try := 0; try < len(addRetries)+1; try++ {
		if try > 0 {
			s.logf("adding the worktree failed on another worktree's half-written entry; trying again: %v", err)
			time.Sleep(addRetries[try-1])
			// git makes a new branch before it fails, so the next try checks that branch out.
			if made := newBranch(args); made != "" && refExists(s.Root, "refs/heads/"+made) {
				args = []string{s.Path, made}
			}
		}
		if _, err = git(s.Root, append([]string{"worktree", "add", "-q"}, args...)...); err == nil {
			return nil
		}
		// A checkout that failed part-way (a full disk) would be reused next time as if it were whole.
		_ = os.RemoveAll(s.Path)
		_, _ = git(s.Root, "worktree", "prune")
		if !halfWritten(err) {
			return err
		}
	}
	return err
}

// addRetries is how long addWorktree waits before each further try.
var addRetries = []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}

// halfWritten reports whether git failed on another worktree's admin entry that isn't all there yet.
func halfWritten(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "worktrees/") && (strings.Contains(msg, "commondir") || strings.Contains(msg, "gitdir"))
}

// prune clears entries for worktrees whose folders have gone, under the lock.
func (s Spec) prune() {
	unlock := s.lockWorktrees()
	defer unlock()
	_, _ = git(s.Root, "worktree", "prune")
}

// newBranch is the branch git worktree add's arguments make (-b), or "".
func newBranch(args []string) string {
	for i, a := range args {
		if a == "-b" && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}
