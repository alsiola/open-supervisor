package engine

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/core"
	"github.com/alsiola/open-supervisor/internal/logx"
	"github.com/alsiola/open-supervisor/internal/store"
)

// hookMember is one tracker or repo that takes deliveries on a hook: a source's tracker or a project's repo.
type hookMember struct {
	h       adapter.Hooked
	conn    string
	source  *Source
	project *Project
}

func (m hookMember) name() string {
	if m.source != nil {
		return m.source.Name
	}
	return m.project.Name
}

func (m hookMember) logf(format string, args ...any) {
	if m.source != nil {
		m.source.logf(format, args...)
		return
	}
	m.project.logf(format, args...)
}

func (m hookMember) store() *store.Store {
	if m.source != nil {
		return m.source.hookStore()
	}
	return m.project.Store
}

// stores are where a hook on the member's target may have been remembered: its own store, and for a source
// its projects' (a project's tracker moved into a source keeps its hook).
func (m hookMember) stores() []*store.Store {
	out := []*store.Store{m.store()}
	if m.source != nil {
		for _, p := range m.source.Projects() {
			out = append(out, p.Store)
		}
	}
	return out
}

// hookGroup is one hook on a service (a GitHub repository, a Jira project, a Linear team) and everything that
// takes its deliveries: a source sharing a repository with a project's code shares its hook, so the service
// sends each event once. The first member owns the hook, and its name and connection make the hook's path.
type hookGroup struct {
	target  string
	path    string
	members []hookMember
}

// jobProjects are the projects whose jobs run on the hook's events: those whose code it carries, and the
// project an implicit source belongs to.
func (g *hookGroup) jobProjects() []*Project {
	seen := map[*Project]bool{}
	var out []*Project
	add := func(p *Project) {
		if p != nil && !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	for _, m := range g.members {
		if m.project != nil {
			add(m.project)
		} else if m.source.Spec.Implicit() {
			for _, p := range m.source.Projects() {
				add(p)
			}
		}
	}
	return out
}

// buildHooks groups every source's tracker and every project's repo that takes webhooks by the hook they are
// registered on, and subscribes each hook to the events its projects' jobs run on.
func buildHooks(sources []*Source, projects []*Project) map[string][]*hookGroup {
	var members []hookMember
	for _, s := range sources {
		if h, ok := s.Tracker.(adapter.Hooked); ok && len(s.Projects()) > 0 {
			members = append(members, hookMember{h: h, conn: s.conn, source: s})
		}
	}
	for _, p := range projects {
		if h, ok := p.Repo.(adapter.Hooked); ok {
			members = append(members, hookMember{h: h, conn: p.repoConn, project: p})
		}
	}
	byTarget := map[string]*hookGroup{}
	var order []*hookGroup
	for _, m := range members {
		t := m.h.HookTarget()
		g := byTarget[t]
		if g == nil {
			g = &hookGroup{target: t, path: m.name() + "/" + m.conn}
			byTarget[t] = g
			order = append(order, g)
		}
		g.members = append(g.members, m)
	}
	out := map[string][]*hookGroup{}
	subscribed := map[*Project]bool{}
	for _, g := range order {
		// The hook is registered at the owner's path, but every member's own path takes its deliveries too: a
		// hook added by hand, or before the group had its owner, keeps working.
		paths := map[string]bool{g.path: true}
		for _, m := range g.members {
			paths[m.name()+"/"+m.conn] = true
		}
		for p := range paths {
			out[p] = append(out[p], g)
		}
		var events []string
		for _, p := range g.jobProjects() {
			events = append(events, p.Jobs.Events()...)
		}
		for _, m := range g.members {
			if eh, ok := m.h.(adapter.EventHooked); ok {
				if len(events) > 0 {
					eh.WantEvents(events)
				}
				for _, p := range g.jobProjects() {
					subscribed[p] = true
				}
			}
		}
	}
	for _, p := range projects {
		if events := p.Jobs.Events(); len(events) > 0 && !subscribed[p] && !p.begun {
			p.logf("jobs run on %s, but neither %s nor %s sends events", strings.Join(events, ", "), p.Tracker.Describe(), p.Repo.Describe())
		}
	}
	return out
}

// hookGroups are the hooks delivering to a path.
func (s *Supervisor) hookGroups(path string) []*hookGroup {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.hooks[path]
}

// allHooks are every hook, once each.
func (s *Supervisor) allHooks() []*hookGroup {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return uniqueGroups(s.hooks)
}

func uniqueGroups(hooks map[string][]*hookGroup) []*hookGroup {
	var paths []string
	for p := range hooks {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	seen := map[*hookGroup]bool{}
	var out []*hookGroup
	for _, p := range paths {
		for _, g := range hooks[p] {
			if !seen[g] {
				seen[g] = true
				out = append(out, g)
			}
		}
	}
	return out
}

// carryHooks copies each hook's id from the old groups to a new group whose members don't know it (the
// source or project that owned it is gone), so pointing it moves the hook rather than adding a second.
func carryHooks(old, fresh map[string][]*hookGroup) {
	was := map[string]*hookGroup{}
	for _, g := range uniqueGroups(old) {
		was[g.target] = g
	}
	for _, g := range uniqueGroups(fresh) {
		if _, ok := g.known(); ok {
			continue
		}
		if prev := was[g.target]; prev != nil {
			if h, ok := prev.known(); ok {
				if st := g.members[0].store(); st != nil {
					st.SetHook(g.target, h)
				}
			}
		}
	}
}

// known is the hook as one of the members remembers it.
func (g *hookGroup) known() (store.Hook, bool) {
	for _, m := range g.members {
		for _, st := range m.stores() {
			if st == nil {
				continue
			}
			if h, ok := st.Hook(g.target); ok {
				return h, true
			}
		}
	}
	return store.Hook{}, false
}

// registered is each hook's target and the path it was pointed at, to tell after a reload which moved.
func registered(hooks map[string][]*hookGroup) map[string]string {
	out := map[string]string{}
	for _, g := range uniqueGroups(hooks) {
		out[g.target] = g.path
	}
	return out
}

// point points the hook at the public URL, creating it if need be. A hook registered before under another
// member's store (a project's, before it shared a source) is found and kept.
func (g *hookGroup) point(base, secret string, changed func()) {
	owner := g.members[0]
	url := fmt.Sprintf("%s/hooks/%s", strings.TrimRight(base, "/"), g.path)
	existing, _ := g.known()
	ctx, cancel := ctxTimeout(time.Minute)
	id, err := owner.h.EnsureWebhook(ctx, url, secret, existing.ID)
	cancel()
	if err != nil {
		owner.logf("webhook: couldn't point %s's hook at %s: %s", g.target, url, logx.ErrText(err))
		return
	}
	if st := owner.store(); st != nil {
		st.SetHook(g.target, store.Hook{ID: id, URL: url})
	}
	owner.logf("webhook: %s's hook %s points at %s", g.target, id, url)
	changed()
}

// pointHooks points every hook at the public URL.
func (s *Supervisor) pointHooks(base string, only func(*hookGroup) bool) {
	for _, g := range s.allHooks() {
		if only == nil || only(g) {
			go g.point(base, s.secret, s.changed)
		}
	}
}

// resolve reads what deferred signals mean, now the delivery has been answered.
func resolve(m hookMember, sigs []core.Signal) []core.Signal {
	var out []core.Signal
	for _, sig := range sigs {
		if sig.Type != core.SigDeferred || sig.Resolve == nil {
			out = append(out, sig)
			continue
		}
		ctx, cancel := ctxTimeout(time.Minute)
		more, err := sig.Resolve(ctx)
		cancel()
		if err != nil {
			// The reconcile reads it properly later.
			m.logf("webhook: %s", logx.ErrText(err))
			continue
		}
		out = append(out, more...)
	}
	return out
}

// deliver hands a delivery to every member of the hooks at a path: the tracker's signals to its source, the
// repo's to its project, and the delivery itself to the jobs that run on it, once per project.
func (s *Supervisor) deliver(groups []*hookGroup, h http.Header, body []byte) bool {
	type parsed struct {
		m    hookMember
		sigs []core.Signal
	}
	var all []parsed
	ok := false
	for _, g := range groups {
		for _, m := range g.members {
			sigs, err := m.h.ParseWebhook(h, body, s.secret)
			if err != nil {
				m.logf("webhook: %s", logx.ErrText(err))
				continue
			}
			ok = true
			all = append(all, parsed{m, sigs})
		}
	}
	if !ok {
		return false
	}
	go func() {
		var event *core.HookEvent
		for _, x := range all {
			for _, sig := range resolve(x.m, x.sigs) {
				switch {
				case sig.Type == core.SigEvent:
					// Every member on one hook sends the delivery; jobs run on it once.
					if event == nil && sig.Event != nil {
						event = sig.Event
					}
				case x.m.source != nil:
					x.m.source.onSignal(sig)
				default:
					x.m.project.onSignal(sig)
				}
			}
		}
		if event == nil {
			return
		}
		done := map[*Project]bool{}
		seen := map[*hookGroup]bool{}
		for _, g := range groups {
			if seen[g] {
				continue
			}
			seen[g] = true
			for _, p := range g.jobProjects() {
				if !done[p] {
					done[p] = true
					p.Jobs.OnEvent(*event)
				}
			}
		}
	}()
	return true
}
