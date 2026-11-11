package engine

import (
	"fmt"
	"slices"
	"time"

	"github.com/alsiola/open-supervisor/internal/core"
	"github.com/alsiola/open-supervisor/internal/store"
)

// A code host works out whether a PR merges lazily: right after a push the answer is often not in yet, so the
// read is tried again a little later.
var (
	conflictRetry = 10 * time.Second
	conflictTries = 3
)

// openPR is the issue's open PR, the newest one not yet seen closed; 0 with none.
func openPR(r *store.Record) int {
	for i := len(r.PRs) - 1; i >= 0; i-- {
		if !slices.Contains(r.ClosedPRs, r.PRs[i]) {
			return r.PRs[i]
		}
	}
	return 0
}

// checkConflict reads whether the issue's open PR conflicts with its base branch and, when it does, tells the
// agent, once for each pair of heads. A code host's workflows don't run on a PR that can't merge, so without
// this the agent hears only that its checks never reported. base, when set, is a branch just pushed to: a PR
// into another branch isn't affected. tries is how many more reads to make while the host is still working
// the answer out.
func (p *Project) checkConflict(key, base string, tries int) {
	r := p.Store.Get(key)
	if r == nil || r.State == store.Finished || r.HeadSHA == "" {
		return
	}
	id := openPR(r)
	if id == 0 {
		return
	}
	ctx, cancel := ctxTimeout(time.Minute)
	defer cancel()
	pr, err := p.Repo.PR(ctx, id)
	if err != nil {
		p.callFailed(err, "%s: couldn't read PR #%d", p.ref(key), id)
		return
	}
	if pr.State != core.PROpen || base != "" && pr.Base != base {
		return
	}
	switch pr.Mergeable {
	case core.MergeUnknown:
		if tries > 1 {
			p.later(conflictRetry, func() { p.checkConflict(key, base, tries-1) })
		}
	case core.MergeConflicting:
		p.reportConflict(key, pr)
	}
}

// reportConflict tells the agent its PR conflicts with the base branch, unless it was told about these heads.
func (p *Project) reportConflict(key string, pr core.PR) {
	mark := fmt.Sprintf("%d %s %s", pr.ID, pr.HeadSHA, pr.BaseSHA)
	// Every write moves UpdatedAt, so look before writing.
	if r := p.Store.Get(key); r == nil || r.ConflictReported == mark {
		return
	}
	fresh := false
	p.Store.Update(key, func(x *store.Record) {
		if x.ConflictReported != mark && x.State != store.Finished {
			x.ConflictReported = mark
			fresh = true
		}
	})
	if !fresh {
		return
	}
	p.logf("%s: PR #%d conflicts with %s", p.ref(key), pr.ID, pr.Base)
	p.enqueue(key, core.Event{Type: core.EvConflict, PR: pr.ID, SHA: pr.HeadSHA, Base: pr.Base, BaseSHA: pr.BaseSHA, At: time.Now().UTC()})
}

// basePushed looks again at the open PRs a push to branch may have left conflicting: every issue's but the
// branch's own.
func (p *Project) basePushed(branch string) {
	if p.issueOfBranch(branch) != "" {
		return
	}
	for _, r := range p.Store.All() {
		if r.State != store.Finished && r.HeadSHA != "" {
			key := r.Key
			// The host starts working out each PR's mergeability once the base has moved; give it a moment.
			p.later(conflictRetry, func() { p.checkConflict(key, branch, conflictTries) })
		}
	}
}

// later runs fn after d, unless the project has stopped by then.
func (p *Project) later(d time.Duration, fn func()) {
	time.AfterFunc(d, func() {
		p.mu.Lock()
		stopped := p.stopped
		p.mu.Unlock()
		if !stopped {
			fn()
		}
	})
}
