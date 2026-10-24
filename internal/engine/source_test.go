package engine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/core"
	"github.com/alsiola/open-supervisor/internal/store"
)

// setupRouted is a supervisor with two projects, web and api, fed by one source through routes.
func setupRouted(t *testing.T, unrouted string) (*Supervisor, *Source, *Project, *Project) {
	t.Helper()
	_, _ = setup(t, func(adapter.TurnSpec, *fakeTurn) core.Outcome { return review("ok") })
	dir := t.TempDir()
	roots := map[string]string{}
	for _, name := range []string{"web", "api"} {
		root := filepath.Join(dir, name)
		if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
			t.Fatalf("git init: %v %s", err, out)
		}
		roots[name] = root
	}
	cfg := config.New(filepath.Join(dir, "config.yaml"))
	cfg.Settings.Agent = "agent"
	cfg.Settings.ChecksDebounceSeconds = 0
	cfg.Connections["agent"] = &config.Connection{Type: "fake-agent", Values: adapter.Values{}}
	cfg.Connections["host"] = &config.Connection{Type: "fake-host", Values: adapter.Values{}}
	repo := func() *config.Binding { return &config.Binding{Connection: "host", Values: adapter.Values{}} }
	cfg.Projects = []*config.Project{{Name: "web", Root: roots["web"], Repo: repo()}, {Name: "api", Root: roots["api"], Repo: repo()}}
	cfg.SourceList = []*config.Source{{
		Name:    "issues",
		Tracker: repo(),
		Routes: []config.Route{
			{Project: "web", When: &config.Match{Fields: map[string][]string{"labels": {"web", "frontend"}}}},
			{Project: "api", When: &config.Match{Fields: map[string][]string{"project": {"API*"}}}},
		},
		Unrouted: unrouted,
	}}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, Options{NoTunnel: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range s.Projects() {
		p.start()
	}
	for _, src := range s.Sources() {
		src.start()
	}
	t.Cleanup(func() {
		for _, p := range s.Projects() {
			p.stop()
		}
		time.Sleep(100 * time.Millisecond)
	})
	if n := len(s.Sources()); n != 1 {
		t.Fatalf("%d sources", n)
	}
	return s, s.Sources()[0], s.Project("web"), s.Project("api")
}

func attrs(kv ...string) map[string][]string {
	out := map[string][]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i]] = append(out[kv[i]], kv[i+1])
	}
	return out
}

func TestSourceRoutesToProjects(t *testing.T) {
	s, src, web, api := setupRouted(t, config.UnroutedLabel)
	host.issues["1"] = &core.Issue{Key: "1", Title: "Button", Open: true, Labels: []string{"Frontend"}}
	host.issues["2"] = &core.Issue{Key: "2", Title: "Endpoint", Open: true, Attrs: attrs("project", "API v2")}
	host.issues["3"] = &core.Issue{Key: "3", Title: "Who knows", Open: true}
	host.issues["4"] = &core.Issue{Key: "4", Title: "Later", Open: true, Labels: []string{"backlog"}}
	s.Reconcile()
	if host.reads != 1 {
		t.Fatalf("read the tracker %d times for one reconcile", host.reads)
	}
	waitFor(t, "both claimed", func() bool { return state(web, "1") == store.Review && state(api, "2") == store.Review })
	if web.Store.Get("2") != nil || api.Store.Get("1") != nil {
		t.Fatal("an issue went to the wrong project")
	}
	if un := src.Unrouted(); len(un) != 1 || un[0].Key != "3" {
		t.Fatalf("unrouted: %+v", un)
	}
	if !host.hasLabel("3", "needs-routing") || host.hasLabel("4", "needs-routing") {
		t.Fatalf("labels: %v", host.labelOps)
	}
	if s.Unrouted() != 1 {
		t.Fatalf("supervisor counts %d unrouted", s.Unrouted())
	}
	for _, spec := range agent.turns() {
		if spec.Args == "1" && !strings.HasSuffix(spec.Cwd, "web") || spec.Args == "2" && !strings.HasSuffix(spec.Cwd, "api") {
			t.Fatalf("turn for %s ran in %s", spec.Args, spec.Cwd)
		}
	}
}

func TestRoutedIssueStaysWhereItWasClaimed(t *testing.T) {
	_, src, web, api := setupRouted(t, "")
	host.issues["1"] = &core.Issue{Key: "1", Title: "Button", Open: true, Labels: []string{"web"}}
	src.reconcile()
	waitFor(t, "claimed", func() bool { return state(web, "1") == store.Review })
	// Relabelled for the api: it stays with web, which still hears about it.
	host.mu.Lock()
	host.issues["1"].Labels = []string{"agent-in-progress"}
	host.issues["1"].Attrs = attrs("project", "API")
	host.mu.Unlock()
	src.onSignal(core.Signal{Type: core.SigIssueChanged, Key: "1"})
	src.onSignal(core.Signal{Type: core.SigIssueComment, Key: "1", Comment: &core.Comment{ID: "c1", Kind: core.OnIssue, Body: "still yours", At: time.Now().UTC()}})
	waitFor(t, "second turn", func() bool { return len(agent.turns()) == 2 })
	time.Sleep(50 * time.Millisecond)
	if api.Store.Get("1") != nil {
		t.Fatal("the api claimed an issue web holds")
	}
	if body := agent.turns()[1].Body; !strings.Contains(body, "still yours") || !strings.HasSuffix(agent.turns()[1].Cwd, "web") {
		t.Fatalf("second turn:\n%s", body)
	}
	if err := api.ClaimNow("1"); err == nil || !strings.Contains(err.Error(), "web has it") {
		t.Fatalf("claiming another project's issue by hand: %v", err)
	}
}

func TestUnroutedIssueRoutesOnceItCan(t *testing.T) {
	_, src, web, _ := setupRouted(t, config.UnroutedLabel)
	host.issues["3"] = &core.Issue{Key: "3", Title: "Who knows", Open: true}
	src.reconcile()
	waitFor(t, "unrouted", func() bool { return len(src.Unrouted()) == 1 && host.hasLabel("3", "needs-routing") })
	if _, err := src.ClaimNow("3"); err == nil || !strings.Contains(err.Error(), "no route") {
		t.Fatalf("claim by route of an unrouted issue: %v", err)
	}
	host.mu.Lock()
	host.issues["3"].Labels = append(host.issues["3"].Labels, "web")
	host.mu.Unlock()
	// The delivery carries no issue: the source reads it, as the reconcile would.
	src.onSignal(core.Signal{Type: core.SigIssueChanged, Key: "3"})
	waitFor(t, "claimed by web", func() bool { return state(web, "3") == store.Review })
	if len(src.Unrouted()) != 0 || host.hasLabel("3", "needs-routing") || !host.hasLabel("3", "agent-in-progress") {
		t.Fatalf("after routing: unrouted %v, labels %v", src.Unrouted(), host.labelOps)
	}
}

func TestClaimByHandCanOverrideRoutes(t *testing.T) {
	_, src, web, api := setupRouted(t, "")
	host.issues["5"] = &core.Issue{Key: "5", Title: "Odd one", Open: true, Labels: []string{"web"}}
	if err := api.ClaimNow("5"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "api has it", func() bool { return state(api, "5") == store.Review })
	src.reconcile()
	time.Sleep(50 * time.Millisecond)
	if web.Store.Get("5") != nil {
		t.Fatal("web claimed an issue the api was given by hand")
	}
	host.issues["6"] = &core.Issue{Key: "6", Title: "Routed", Open: true, Labels: []string{"web"}}
	p, err := src.ClaimNow("6")
	if err != nil || p != web {
		t.Fatalf("claim by route: %v %v", p, err)
	}
}

func TestImplicitSourceKeepsTheOldShape(t *testing.T) {
	s, p := setup(t, func(adapter.TurnSpec, *fakeTurn) core.Outcome { return review("ok") })
	srcs := s.Sources()
	if len(srcs) != 1 || srcs[0].Name != "demo" || !srcs[0].Spec.Implicit() || p.source() != srcs[0] {
		t.Fatalf("sources: %+v", srcs)
	}
	host.issues["1"] = &core.Issue{Key: "1", Title: "Anything", Open: true, Labels: []string{"whatever"}}
	s.Reconcile()
	waitFor(t, "claimed", func() bool { return state(p, "1") == store.Review })
}

// ---- hooks ------------------------------------------------------------------------------------------------

type hookedHost struct {
	*fakeHost
	target string
	events []string
	got    int
	sigs   []core.Signal
}

func (h *hookedHost) HookTarget() string { return h.target }
func (h *hookedHost) EnsureWebhook(context.Context, string, string, string) (string, error) {
	return "1", nil
}
func (h *hookedHost) ParseWebhook(hd http.Header, _ []byte, _ string) ([]core.Signal, error) {
	h.got++
	if hd.Get("Reject") != "" {
		return nil, fmt.Errorf("bad signature")
	}
	return h.sigs, nil
}
func (h *hookedHost) WantEvents(names []string) { h.events = append(h.events, names...) }

func TestHooksAreSharedByTarget(t *testing.T) {
	tracker := &hookedHost{fakeHost: newHost(), target: "svc:a"}
	webRepo := &hookedHost{fakeHost: newHost(), target: "svc:a"}
	apiRepo := &hookedHost{fakeHost: newHost(), target: "svc:b"}
	web := &Project{Name: "web", Repo: webRepo, Tracker: tracker, repoConn: "svc", begun: true}
	web.Jobs = &Jobs{p: web}
	web.Jobs.SetSpecs([]config.Job{{Name: "deploy", Command: []string{"true"}, On: &config.JobTrigger{Events: []string{"push"}}}})
	api := &Project{Name: "api", Repo: apiRepo, Tracker: tracker, repoConn: "svc", begun: true}
	api.Jobs = &Jobs{p: api}
	src := &Source{Name: "issues", Spec: &config.Source{Name: "issues"}, Tracker: tracker, conn: "svc"}
	src.setProjects([]*Project{web, api})
	hooks := buildHooks([]*Source{src}, []*Project{web, api})
	// The shared hook answers at its owner's path and at web's own, which it had before the source.
	if len(hooks) != 3 || len(hooks["issues/svc"]) != 1 || hooks["web/svc"][0] != hooks["issues/svc"][0] || len(hooks["api/svc"]) != 1 {
		t.Fatalf("hooks: %v", hooks)
	}
	if n := len(uniqueGroups(hooks)); n != 2 {
		t.Fatalf("%d hooks, want 2", n)
	}
	g := hooks["issues/svc"][0]
	if len(g.members) != 2 || g.members[0].source != src || g.members[1].project != web {
		t.Fatalf("shared hook's members: %+v", g.members)
	}
	// Only web's repo is on the shared hook; the source is explicit, so its tracker carries no project's jobs.
	if !slices.Contains(tracker.events, "push") || !slices.Contains(webRepo.events, "push") || len(apiRepo.events) != 0 {
		t.Fatalf("subscriptions: tracker %v web %v api %v", tracker.events, webRepo.events, apiRepo.events)
	}
	s := &Supervisor{}
	if !s.deliver(hooks["issues/svc"], http.Header{}, []byte("{}")) || tracker.got != 1 || webRepo.got != 1 || apiRepo.got != 0 {
		t.Fatalf("delivery parsed by tracker %d, web %d, api %d", tracker.got, webRepo.got, apiRepo.got)
	}
}

func TestReloadWithNewRoutesKeepsProjects(t *testing.T) {
	s, src, web, api := setupRouted(t, "")
	old := s.Config()
	cfg := *old
	cfg.SourceList = []*config.Source{{
		Name:    "issues",
		Tracker: old.SourceList[0].Tracker,
		Routes: []config.Route{
			{Project: "api", When: &config.Match{Fields: map[string][]string{"labels": {"web"}}}},
			{Project: "web"},
		},
	}}
	if err := s.Reload(&cfg); err != nil {
		t.Fatal(err)
	}
	if s.Project("web") != web || s.Project("api") != api {
		t.Fatal("new routes rebuilt the projects")
	}
	fresh := s.Sources()[0]
	if fresh == src || web.source() != fresh || web.Tracker != fresh.Tracker {
		t.Fatal("the source wasn't rebuilt around the same tracker")
	}
	if name, _ := fresh.Route(core.Issue{Key: "1", Labels: []string{"web"}}); name != "api" {
		t.Fatalf("routes after reload send a web issue to %q", name)
	}
	if name, _ := fresh.Route(core.Issue{Key: "2"}); name != "web" {
		t.Fatalf("the catch-all sends to %q", name)
	}
}

func TestWaitingIssuesAreNeitherClaimedNorUnrouted(t *testing.T) {
	_, src, web, _ := setupRouted(t, config.UnroutedLabel)
	host.issues["1"] = &core.Issue{Key: "1", Title: "Backlog, routed", Open: true, Waiting: true, Labels: []string{"web"}}
	host.issues["2"] = &core.Issue{Key: "2", Title: "Backlog, unrouted", Open: true, Waiting: true}
	src.reconcile()
	src.onSignal(core.Signal{Type: core.SigIssueReopened, Key: "1", Issue: host.issues["1"]})
	time.Sleep(100 * time.Millisecond)
	if web.Store.Get("1") != nil || len(src.Unrouted()) != 0 || host.hasLabel("2", "needs-routing") {
		t.Fatalf("waiting issues: web has 1 %v, unrouted %v", web.Store.Get("1") != nil, src.Unrouted())
	}
	// By hand is fine.
	if err := web.ClaimNow("1"); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "claimed by hand", func() bool { return state(web, "1") == store.Review })
	// Held and waiting is still open: not reported closed.
	src.reconcile()
	time.Sleep(50 * time.Millisecond)
	if r := web.Store.Get("1"); r == nil || r.IssueClosedReported {
		t.Fatal("a held waiting issue was taken for closed")
	}
}

func TestDeliveriesResolveLaterAndRetriesGetThrough(t *testing.T) {
	s, src, web, _ := setupRouted(t, "")
	host.issues["1"] = &core.Issue{Key: "1", Title: "Button", Open: true, Labels: []string{"web"}}
	resolved := make(chan struct{}, 1)
	tracker := &hookedHost{fakeHost: host, target: "svc:a", sigs: []core.Signal{{Type: core.SigDeferred, Resolve: func(context.Context) ([]core.Signal, error) {
		resolved <- struct{}{}
		return []core.Signal{{Type: core.SigIssueChanged, Key: "1"}}, nil
	}}}}
	src.Tracker = tracker
	src.conn = "svc"
	s.mu.Lock()
	s.hooks = buildHooks([]*Source{src}, nil)
	s.mu.Unlock()
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	post := func(reject bool) int {
		req, _ := http.NewRequest("POST", srv.URL+"/hooks/issues/svc", strings.NewReader("{}"))
		req.Header.Set("X-GitHub-Delivery", "d1")
		if reject {
			req.Header.Set("Reject", "1")
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := post(true); code != http.StatusUnauthorized {
		t.Fatalf("rejected delivery: %d", code)
	}
	// The service retries with the same delivery id: it isn't taken for a duplicate.
	if code := post(false); code != http.StatusOK {
		t.Fatalf("retry: %d", code)
	}
	<-resolved
	waitFor(t, "routed from the deferred signal", func() bool { return state(web, "1") == store.Review })
	n := tracker.got
	if code := post(false); code != http.StatusOK || tracker.got != n {
		t.Fatalf("a duplicate was parsed again: %d", code)
	}
}

func TestReloadCarriesAHookToItsNewOwner(t *testing.T) {
	gone := &Source{Name: "gone", Spec: &config.Source{Name: "gone"}, store: store.New(t.TempDir(), nil)}
	gone.store.SetHook("svc:a", store.Hook{ID: "42", URL: "https://x/hooks/gone/svc"})
	repo := &hookedHost{fakeHost: newHost(), target: "svc:a"}
	web := &Project{Name: "web", Repo: repo, Tracker: repo, repoConn: "svc", Store: store.New(t.TempDir(), nil), begun: true}
	web.Jobs = &Jobs{p: web}
	old := map[string][]*hookGroup{"gone/svc": {{target: "svc:a", path: "gone/svc", members: []hookMember{{h: repo, conn: "svc", source: gone}}}}}
	fresh := buildHooks(nil, []*Project{web})
	carryHooks(old, fresh)
	if h, ok := web.Store.Hook("svc:a"); !ok || h.ID != "42" {
		t.Fatalf("the hook's id didn't move to web: %+v", h)
	}
	if registered(old)["svc:a"] == registered(fresh)["svc:a"] {
		t.Fatal("the hook's path should have changed")
	}
}
