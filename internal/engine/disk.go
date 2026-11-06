package engine

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	hostinfo "github.com/alsiola/open-supervisor/internal/host"
	"github.com/alsiola/open-supervisor/internal/logx"
	"github.com/alsiola/open-supervisor/internal/store"
	"github.com/alsiola/open-supervisor/internal/worktree"
)

// BigScratch is the size over which a worktree's scratch folder (.local/) is named when the disk runs low. What
// an agent keeps there (caches, exports, tuning runs) can be costly to make again, so it is reported, never
// removed for space, and a parked worktree holding that much isn't evicted.
var BigScratch uint64 = 1 << 30

// reliefEvery is the least time between two passes at freeing a disk that stays over its threshold.
const reliefEvery = 30 * time.Minute

// ParkedWorktree is the worktree of an issue whose agent is idle until someone else acts, which can take days:
// it is up for review, asking for help, done bar the merge, or parked on a wait. Its branch is pushed, so when
// the disk runs low it can go, and agent worktree setup makes it again when the agent resumes.
type ParkedWorktree struct {
	Path, Key string
	// Used is when the issue's agent last ran.
	Used time.Time
}

// Parked reports whether an issue in this state has an agent idle until someone else acts. A turn running
// or about to (queued, building, failed and due a retry) or an owner attached keeps its worktree.
func Parked(s store.State) bool {
	switch s {
	case store.Review, store.Help, store.Done, store.Scheduled:
		return true
	}
	return false
}

// ParkedWorktrees lists a project's parked issue worktrees, least recently used first. Spare ones (see
// IssueWorktrees) and those an agent task runs from are left out.
func ParkedWorktrees(dataDir, stateDir, project string, st *store.Store) []ParkedWorktree {
	all, spare := IssueWorktrees(dataDir, stateDir, project, st)
	skip := map[string]bool{}
	for _, w := range spare {
		skip[w.Path] = true
	}
	held := map[string]*store.Record{}
	for _, r := range st.All() {
		held[worktree.Dir(dataDir, project, r.Key)] = r
	}
	var parked []ParkedWorktree
	for _, path := range all {
		r := held[path]
		if skip[path] || r == nil || !Parked(r.State) || len(TasksWithin(stateDir, path)) > 0 {
			continue
		}
		used := r.UpdatedAt
		if r.LastTurn != nil {
			used = r.LastTurn.StartedAt
			if r.LastTurn.EndedAt != nil {
				used = *r.LastTurn.EndedAt
			}
		}
		parked = append(parked, ParkedWorktree{Path: path, Key: r.Key, Used: used})
	}
	sort.SliceStable(parked, func(i, j int) bool { return parked[i].Used.Before(parked[j].Used) })
	return parked
}

// EvictParked removes parked worktrees, least recently used first, while full says the disk is still too full.
// One with work that isn't pushed, or with more than BigScratch in its scratch folder, stays; so does one whose
// issue is no longer parked by the time its turn comes (still reports whether it is, from a fresh reading).
// It returns the issues whose worktrees it removed.
func EvictParked(root, remote string, parked []ParkedWorktree, still func(key string) bool, full func() bool, logf func(string, ...any)) []string {
	var removed []string
	for _, w := range parked {
		if !full() {
			break
		}
		if !still(w.Key) {
			continue
		}
		spec := worktree.Spec{Root: root, Remote: remote, Path: w.Path}
		if lost, err := worktree.Unpushed(spec); err != nil || lost != "" {
			continue
		}
		if worktree.Size(filepath.Join(w.Path, worktree.Scratch)) > BigScratch {
			continue
		}
		if err := worktree.Remove(spec); err != nil {
			logf("couldn't remove the parked worktree %s: %s", w.Path, logx.ErrText(err))
			continue
		}
		removed = append(removed, w.Key)
	}
	return removed
}

// BigScratches names the worktrees whose scratch folders hold more than BigScratch, largest first:
// "issue-2057 (32.0 GB)".
func BigScratches(paths []string) []string {
	type big struct {
		name string
		size uint64
	}
	var found []big
	for _, path := range paths {
		if n := worktree.Size(filepath.Join(path, worktree.Scratch)); n > BigScratch {
			found = append(found, big{filepath.Base(path), n})
		}
	}
	sort.SliceStable(found, func(i, j int) bool { return found[i].size > found[j].size })
	var out []string
	for _, b := range found {
		out = append(out, fmt.Sprintf("%s (%s)", b.name, worktree.Bytes(b.size)))
	}
	return out
}

// worktreesDir is the folder the project's worktrees live in.
func (p *Project) worktreesDir() string {
	return filepath.Join(p.hub.dataDir(), "worktrees", p.Name)
}

// killOrphans stops the processes left running from the project's worktrees that are gone.
func (p *Project) killOrphans() {
	if n := worktree.KillOrphans(p.worktreesDir()); n > 0 {
		p.logf("stopped %d process(es) left running from worktrees that are gone", n)
	}
}

// relieveDisk reports whether the disk was over, and runs when the disk the project's worktrees are on may be over its threshold (the percentage in
// use, disk): it removes parked worktrees, least recently used first, until the disk is back under it, then
// logs what it freed and the scratch folders holding the most, which it leaves for the owner to clear.
func (p *Project) relieveDisk(disk float64) bool {
	dir := p.worktreesDir()
	read := func() (hostinfo.Disk, bool) {
		d, err := hostinfo.DiskAt(dir)
		return d, err == nil && d.Total > 0 && d.UsedPct() > disk
	}
	before, over := read()
	if disk <= 0 || disk >= 100 || !over {
		return false
	}
	p.killOrphans()
	parked := ParkedWorktrees(p.hub.dataDir(), p.StateDir, p.Name, p.Store)
	still := func(key string) bool { r := p.Store.Get(key); return r != nil && Parked(r.State) }
	full := func() bool { _, over := read(); return over }
	removed := EvictParked(p.Root, p.git.RemoteName(), parked, still, full, p.logf)
	after, over := read()
	msg := fmt.Sprintf("disk: %s is %.0f%% used, over the %.0f%% threshold", dir, before.UsedPct(), disk)
	if len(removed) > 0 {
		var refs []string
		for _, key := range removed {
			refs = append(refs, refOf(key))
		}
		freed := uint64(0)
		if after.Free > before.Free {
			freed = after.Free - before.Free
		}
		msg += fmt.Sprintf("; removed the worktrees of %d parked issue(s) (%s), freeing %s, now %.0f%%", len(removed), strings.Join(refs, ", "), worktree.Bytes(freed), after.UsedPct())
	} else {
		msg += "; no parked worktree could go"
	}
	all, _ := IssueWorktrees(p.hub.dataDir(), p.StateDir, p.Name, p.Store)
	if big := BigScratches(all); len(big) > 0 {
		msg += fmt.Sprintf("; scratch data over %s, left for you to clear: %s", worktree.Bytes(BigScratch), strings.Join(big, ", "))
	}
	if !over {
		msg += "; back under the threshold"
	}
	p.logf("%s", msg)
	return true
}

// diskRelief keeps one pass at freeing the disk going at a time, and no more often than reliefEvery.
type diskRelief struct {
	mu      sync.Mutex
	running bool
	last    time.Time
}

// start reports whether a pass may begin now, and if so marks it running. A pass that found a disk over its
// threshold holds off the next for reliefEvery; one that found none doesn't, so a disk filling up is caught at
// the next reading.
func (r *diskRelief) start(force bool) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.running || (!force && time.Since(r.last) < reliefEvery) {
		return false
	}
	r.running = true
	return true
}

// done marks the pass over; over is whether it found a disk over its threshold.
func (r *diskRelief) done(over bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.running = false
	if over {
		r.last = time.Now()
	}
}

// relieveDisks gives every project a pass at freeing its worktrees' disk when it is over the threshold. force
// skips the wait between passes (the six-hourly sweep).
func (s *Supervisor) relieveDisks(force bool) {
	if !s.relief.start(force) {
		return
	}
	over := false
	defer func() { s.relief.done(over) }()
	_, _, disk := s.settings().Host.Thresholds()
	for _, p := range s.Projects() {
		if p.relieveDisk(disk) {
			over = true
		}
	}
}
