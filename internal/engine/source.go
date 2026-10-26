package engine

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/core"
	"github.com/alsiola/open-supervisor/internal/logx"
	"github.com/alsiola/open-supervisor/internal/store"
)

// Source is a tracker and the projects it feeds. It reads the tracker's open issues once per reconcile and
// takes the tracker's webhooks, and sends each issue nobody holds to the project of the first route that
// matches it. Once a project claims an issue, the issue stays there: comments, closes and reopens go to the
// project that holds it, whatever its properties say now. A project with a tracker of its own has a source of
// its own, with one route to it.
type Source struct {
	Name    string
	Spec    *config.Source
	Tracker adapter.Tracker
	Labels  config.Labels

	conn               string
	fingerprint        string
	trackerFingerprint string
	stateDir           string
	hub                hub
	// store keeps an explicit source's hooks; an implicit source's are in its project's store.
	store *store.Store

	mu            sync.Mutex
	projects      []*Project
	unrouted      map[string]core.Issue
	stranded      map[string]core.Issue
	warned        map[string]string
	reconciling   bool
	lastReconcile time.Time
	started       bool

	// claimMu is held while one of the source's projects claims, so no two claim one issue.
	claimMu sync.Mutex
}

func (s *Source) logf(format string, args ...any) {
	if ps := s.Projects(); s.Spec.Implicit() && len(ps) == 1 {
		ps[0].logf(format, args...)
		return
	}
	logx.Default.Write(s.Name, filepath.Join(s.stateDir, "log"), format, args...)
}

// callFailed is Project.callFailed for the source's own calls.
func (s *Source) callFailed(err error, format string, args ...any) {
	if adapter.IsPaused(err) {
		return
	}
	s.logf(format+": %s", append(args, logx.ErrText(err))...)
}

// Projects are the open projects the source feeds, in the order of its routes.
func (s *Source) Projects() []*Project {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]*Project(nil), s.projects...)
}

func (s *Source) setProjects(ps []*Project) {
	s.mu.Lock()
	s.projects = ps
	s.mu.Unlock()
	for _, p := range ps {
		p.src.Store(s)
	}
}

// project is the open project by name, nil when it is disabled or couldn't be opened.
func (s *Source) project(name string) *Project {
	for _, p := range s.Projects() {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// hookStore is where the source's hooks are remembered.
func (s *Source) hookStore() *store.Store {
	if s.store != nil {
		return s.store
	}
	if ps := s.Projects(); len(ps) > 0 {
		return ps[0].Store
	}
	return nil
}

// Route is the project the issue goes to and the index of the route that sent it there; "" and -1 when no
// route takes it.
func (s *Source) Route(issue core.Issue) (string, int) {
	i := s.Spec.RouteFor(issue.Attr)
	if i < 0 {
		return "", -1
	}
	return s.Spec.Routes[i].Project, i
}

// Holder is the project that holds the issue, nil when none does.
func (s *Source) Holder(key string) *Project {
	for _, p := range s.Projects() {
		if p.Store.Get(key) != nil {
			return p
		}
	}
	return nil
}

// Unrouted are the open issues the source's routes don't take, oldest first.
func (s *Source) Unrouted() []core.Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	return oldestFirst(s.unrouted)
}

func oldestFirst(issues map[string]core.Issue) []core.Issue {
	out := make([]core.Issue, 0, len(issues))
	for _, i := range issues {
		out = append(out, i)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// Reconciled says when the last reconcile finished and whether one is going.
func (s *Source) Reconciled() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastReconcile, s.reconciling
}

func (s *Source) start() {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()
	if s.store != nil {
		_ = os.MkdirAll(s.stateDir, 0o755)
		if err := s.store.Load(); err != nil {
			s.logf("couldn't read the state file: %v", err)
		}
	}
	if !s.Spec.Implicit() {
		names := make([]string, 0, len(s.Spec.Routes))
		for _, r := range s.Spec.Routes {
			names = append(names, r.Project)
		}
		s.logf("routing %s to %s", s.Tracker.Describe(), joinUnique(names))
	}
	tracker, specs := s.Tracker, s.labelSpecs()
	go func() {
		if s.hub.settings().DryRun {
			return
		}
		ctx, cancel := ctxTimeout(2 * time.Minute)
		defer cancel()
		if err := tracker.EnsureLabels(ctx, specs); err != nil {
			s.callFailed(err, "couldn't create the labels")
		}
	}()
}

func (s *Source) labelSpecs() []adapter.Label {
	l := s.Labels
	out := []adapter.Label{
		{Name: l.Claimed, Color: "FBCA04", Description: "An agent has this issue"},
		{Name: l.Scheduled, Color: "C5DEF5", Description: "Opened on a timer, or held until its runAt time"},
		{Name: l.Help, Color: "D93F0B", Description: "The agent asked something it cannot go on without"},
		{Name: l.Review, Color: "0E8A16", Description: "Something is up for the owner"},
		{Name: l.Done, Color: "BFDADC", Description: "Everything asked is handled; only a merge or close is left"},
	}
	for _, sk := range l.Skip {
		out = append(out, adapter.Label{Name: sk, Color: "EDEDED", Description: "The supervisor leaves this issue alone"})
	}
	for _, r := range l.Require {
		out = append(out, adapter.Label{Name: r, Color: "5319E7", Description: "An agent may claim this issue"})
	}
	if s.Spec.Unrouted == config.UnroutedLabel && l.Unrouted != "" {
		out = append(out, adapter.Label{Name: l.Unrouted, Color: "F9D0C4", Description: "No project's route takes this issue"})
	}
	return out
}

// Stranded are the open issues that carry the claimed label though no project of the source ever claimed them,
// oldest first: nothing will claim them until the label goes.
func (s *Source) Stranded() []core.Issue {
	s.mu.Lock()
	defer s.mu.Unlock()
	return oldestFirst(s.stranded)
}

// claimable reports whether an issue is one the supervisor would claim, routes aside.
func (s *Source) claimable(issue core.Issue) bool {
	if !issue.Open || issue.Waiting || issue.HasLabel(s.Labels.Claimed) {
		return false
	}
	return s.Labels.Admits(issue.HasLabel)
}

// dispatch sends an issue nobody holds to its route's project to claim. reopened lets a labelled issue through.
func (s *Source) dispatch(issue core.Issue, reopened bool) {
	name, _ := s.Route(issue)
	if name == "" {
		s.noRoute(issue)
		return
	}
	p := s.project(name)
	if p != nil && !reopened && s.isStranded(issue) {
		s.strand(p, issue)
		return
	}
	s.forget(issue.Key)
	if p != nil {
		p.maybeClaim(issue, reopened)
	}
}

// isStranded reports whether an open issue nobody holds carries the claimed label though no project of the
// source ever claimed it: someone added the label by hand, and maybeClaim will pass it over for good.
func (s *Source) isStranded(issue core.Issue) bool {
	if !issue.Open || issue.Waiting || !issue.HasLabel(s.Labels.Claimed) || !s.Labels.Admits(issue.HasLabel) {
		return false
	}
	for _, p := range s.Projects() {
		if !p.Store.LastClaim(issue.Key).IsZero() {
			return false
		}
	}
	return true
}

// strand notes, once, a stranded issue its route's project would otherwise claim. It isn't claimed: the label
// may be another supervisor's.
func (s *Source) strand(p *Project, issue core.Issue) {
	s.mu.Lock()
	if s.stranded == nil {
		s.stranded = map[string]core.Issue{}
	}
	_, known := s.stranded[issue.Key]
	s.stranded[issue.Key] = issue
	delete(s.unrouted, issue.Key)
	s.mu.Unlock()
	if !known {
		p.logf("%s: carries %s but nothing here ever claimed it, so it is left alone; remove the label to release it, or claim it (c %s#%s)", p.ref(issue.Key), s.Labels.Claimed, p.Name, issue.Key)
		s.hub.changed()
	}
}

// noRoute notes an issue no route takes, and labels it when the source asks for that.
func (s *Source) noRoute(issue core.Issue) {
	if !s.claimable(issue) {
		s.forget(issue.Key)
		return
	}
	s.mu.Lock()
	if s.unrouted == nil {
		s.unrouted = map[string]core.Issue{}
	}
	_, known := s.unrouted[issue.Key]
	s.unrouted[issue.Key] = issue
	s.mu.Unlock()
	if !known {
		s.logf("%s: no route takes it (%s)", refOf(issue.Key), issue.Title)
		s.hub.changed()
	}
	label := s.Labels.Unrouted
	if s.Spec.Unrouted != config.UnroutedLabel || label == "" || issue.HasLabel(label) || s.hub.settings().DryRun {
		return
	}
	ctx, cancel := ctxTimeout(time.Minute)
	defer cancel()
	if err := s.Tracker.EditLabels(ctx, issue.Key, []string{label}, nil); err != nil {
		s.callFailed(err, "%s: couldn't label it %s", refOf(issue.Key), label)
	}
}

// forget drops an issue from the unrouted and stranded lists.
func (s *Source) forget(key string) {
	s.mu.Lock()
	_, un := s.unrouted[key]
	_, st := s.stranded[key]
	ok := un || st
	delete(s.unrouted, key)
	delete(s.stranded, key)
	s.mu.Unlock()
	if ok {
		s.hub.changed()
	}
}

// warnMoved says, once, that an issue a project holds would now route to another.
func (s *Source) warnMoved(holder *Project, issue core.Issue) {
	name, _ := s.Route(issue)
	if name == "" || name == holder.Name {
		return
	}
	s.mu.Lock()
	if s.warned == nil {
		s.warned = map[string]string{}
	}
	if s.warned[issue.Key] == name {
		s.mu.Unlock()
		return
	}
	s.warned[issue.Key] = name
	s.mu.Unlock()
	holder.logf("%s: now routes to %s, but stays here; to move it, drop it (d) and claim it as %s#%s", holder.ref(issue.Key), name, name, issue.Key)
}

// ClaimNow claims an issue by hand through the routes: the project its route names claims it.
func (s *Source) ClaimNow(key string) (*Project, error) {
	if p := s.Holder(key); p != nil {
		p.RunNow(key)
		return p, nil
	}
	ctx, cancel := ctxTimeout(time.Minute)
	issue, err := s.Tracker.Issue(ctx, key)
	cancel()
	if err != nil {
		return nil, err
	}
	name, _ := s.Route(issue)
	if name == "" {
		return nil, fmt.Errorf("no route of %s takes %s; claim it as <project>#%s", s.Name, key, key)
	}
	p := s.project(name)
	if p == nil {
		return nil, fmt.Errorf("%s routes to %s, which isn't running", key, name)
	}
	return p, p.ClaimNow(key)
}

// reopenedSinceLastClaim: an issue carrying the claimed label counts as reopened when the tracker's last reopen
// is newer than this supervisor's last claim of it in any of the source's projects (or it never claimed it but
// the tracker says reopened).
func (s *Source) reopenedSinceLastClaim(ctx context.Context, issue core.Issue) bool {
	at, err := s.Tracker.ReopenedAt(ctx, issue.Key)
	if err != nil || at.IsZero() {
		return false
	}
	var last time.Time
	for _, p := range s.Projects() {
		if t := p.Store.LastClaim(issue.Key); t.After(last) {
			last = t
		}
	}
	return last.IsZero() || at.After(last)
}

// ---- signals from webhooks ------------------------------------------------------------------------------

// onSignal takes the tracker's signals: issues nobody holds are routed, the rest go to the project holding them.
func (s *Source) onSignal(sig core.Signal) {
	switch sig.Type {
	case core.SigPing:
		s.logf("webhook: hello from %s", sig.Key)
	case core.SigIssueChanged, core.SigIssueReopened:
		key := sig.Key
		if sig.Issue != nil {
			key = sig.Issue.Key
		}
		holder := s.Holder(key)
		if holder != nil && sig.Type == core.SigIssueReopened {
			holder.onSignal(sig)
			return
		}
		issue := sig.Issue
		if issue == nil {
			// The delivery said what happened, not everything about the issue: read it, so it routes just as the
			// reconcile would route it.
			ctx, cancel := ctxTimeout(30 * time.Second)
			i, err := s.Tracker.Issue(ctx, key)
			cancel()
			if err != nil {
				s.callFailed(err, "%s: couldn't read it", refOf(key))
				return
			}
			issue = &i
		}
		if holder != nil {
			s.warnMoved(holder, *issue)
			return
		}
		go s.dispatch(*issue, sig.Type == core.SigIssueReopened)
	case core.SigIssueClosed:
		s.forget(sig.Key)
		for _, p := range s.Projects() {
			go s.hub.fact(core.Fact{Kind: core.CondIssue, Project: p.Name, Ref: sig.Key, On: "closed"})
		}
		if p := s.Holder(sig.Key); p != nil {
			p.onSignal(sig)
		}
	case core.SigIssueComment:
		if p := s.Holder(sig.Key); p != nil {
			p.onSignal(sig)
		}
	}
}

// ---- reconcile ------------------------------------------------------------------------------------------

// reconcile reads the tracker's open issues once, routes the ones nobody holds, then has each project read
// everything else a webhook could have missed.
func (s *Source) reconcile() {
	s.mu.Lock()
	if s.reconciling {
		s.mu.Unlock()
		return
	}
	s.reconciling = true
	s.mu.Unlock()
	projects := s.Projects()
	var live []*Project
	before := map[*Project]int{}
	for _, p := range projects {
		if p.beginReconcile() {
			live = append(live, p)
			before[p] = len(p.Store.All())
		}
	}
	s.hub.changed()
	defer func() {
		for _, p := range live {
			p.endReconcile()
		}
		s.mu.Lock()
		s.reconciling = false
		s.mu.Unlock()
		s.hub.schedule()
	}()
	if len(live) == 0 {
		return
	}
	ctx, cancel := ctxTimeout(5 * time.Minute)
	defer cancel()
	open, err := s.Tracker.Candidates(ctx)
	if err != nil {
		s.callFailed(err, "reconcile failed")
		return
	}
	openKeys := map[string]bool{}
	counts := map[*Project]int{}
	for _, issue := range open {
		openKeys[issue.Key] = true
		if p := s.Holder(issue.Key); p != nil {
			counts[p]++
			p.reconcileHeld(ctx, issue)
			continue
		}
		name, _ := s.Route(issue)
		if name == "" {
			s.noRoute(issue)
			continue
		}
		p := s.project(name)
		if p == nil {
			s.forget(issue.Key)
			continue
		}
		counts[p]++
		reopened := false
		if issue.HasLabel(s.Labels.Claimed) {
			reopened = s.reopenedSinceLastClaim(ctx, issue)
		}
		if !reopened && s.isStranded(issue) {
			s.strand(p, issue)
			continue
		}
		s.forget(issue.Key)
		p.maybeClaim(issue, reopened)
	}
	s.mu.Lock()
	for key := range s.unrouted {
		if !openKeys[key] {
			delete(s.unrouted, key)
		}
	}
	for key := range s.stranded {
		if !openKeys[key] {
			delete(s.stranded, key)
		}
	}
	unrouted, stranded := len(s.unrouted), len(s.stranded)
	s.lastReconcile = time.Now()
	s.mu.Unlock()
	for _, p := range live {
		p.maintain(ctx, openKeys, counts[p], before[p])
	}
	if !s.Spec.Implicit() {
		msg := fmt.Sprintf("reconciled: %d open issues", len(open))
		if unrouted > 0 {
			msg += fmt.Sprintf(", %d unrouted", unrouted)
		}
		if stranded > 0 {
			msg += fmt.Sprintf(", %d labelled %s with no session", stranded, s.Labels.Claimed)
		}
		s.logf("%s", msg)
	}
}

// refOf is how the log names an issue: #12 for numbered trackers, PROJ-12 for keyed ones.
func refOf(key string) string {
	if key != "" && key[0] >= '0' && key[0] <= '9' {
		return "#" + key
	}
	return key
}

func joinUnique(xs []string) string {
	seen := map[string]bool{}
	var out []string
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	switch len(out) {
	case 0:
		return "nothing"
	case 1:
		return out[0]
	}
	return fmt.Sprintf("%s and %s", strings.Join(out[:len(out)-1], ", "), out[len(out)-1])
}
