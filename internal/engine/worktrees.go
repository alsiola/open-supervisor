package engine

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/alsiola/open-supervisor/internal/logx"
	"github.com/alsiola/open-supervisor/internal/store"
	"github.com/alsiola/open-supervisor/internal/worktree"
)

// spareAge is how long a worktree no record holds is left before the sweep takes it, so one made by hand for
// an issue not claimed yet isn't taken from under whoever made it.
const spareAge = 24 * time.Hour

// SpareWorktree is an issue worktree the supervisor no longer needs: its issue is finished, or no record holds it
// (the issue was dropped, or finished long enough ago to be forgotten), and no agent task runs from it.
type SpareWorktree struct {
	Path string
	// Key is the issue, "" when no record holds it.
	Key string
}

// IssueWorktrees lists a project's issue worktrees (not the jobs' fresh checkouts), and of those the spare ones.
// stateDir is the project's, where its tasks are recorded.
func IssueWorktrees(dataDir, stateDir, project string, st *store.Store) (all []string, spare []SpareWorktree) {
	all, _ = filepath.Glob(filepath.Join(dataDir, "worktrees", project, "issue-*"))
	sort.Strings(all)
	held := map[string]*store.Record{}
	for _, r := range st.All() {
		held[worktree.Dir(dataDir, project, r.Key)] = r
	}
	for _, path := range all {
		if len(TasksWithin(stateDir, path)) > 0 {
			continue
		}
		switch r := held[path]; {
		case r == nil:
			spare = append(spare, SpareWorktree{Path: path})
		case r.State == store.Finished:
			spare = append(spare, SpareWorktree{Path: path, Key: r.Key})
		}
	}
	return all, spare
}

// SpareNames are the spare worktrees for people to read: "#12" for a finished issue, the folder for the rest.
func SpareNames(spare []SpareWorktree) string {
	var names []string
	for _, w := range spare {
		if w.Key != "" {
			names = append(names, refOf(w.Key))
		} else {
			names = append(names, filepath.Base(w.Path))
		}
	}
	return strings.Join(names, ", ")
}

// sweepWorktrees removes the project's spare worktrees: those of finished issues, which removing at the finish
// missed, and those no record holds that have nothing uncommitted and have sat a day. The branches stay.
func (p *Project) sweepWorktrees() {
	_, spare := IssueWorktrees(p.hub.dataDir(), p.StateDir, p.Name, p.Store)
	for _, w := range spare {
		if w.Key == "" && !idle(w.Path) {
			continue
		}
		if err := worktree.Remove(worktree.Spec{Root: p.Root, Path: w.Path}); err != nil {
			p.logf("couldn't remove the spare worktree %s: %s", w.Path, logx.ErrText(err))
			continue
		}
		p.logf("removed the spare worktree %s", w.Path)
	}
}

// idle is a worktree untouched for spareAge with nothing uncommitted. One git can't read isn't.
func idle(path string) bool {
	info, err := os.Stat(path)
	if err != nil || time.Since(info.ModTime()) < spareAge {
		return false
	}
	out, err := exec.Command("git", "-C", path, "status", "--porcelain").Output()
	return err == nil && len(out) == 0
}
