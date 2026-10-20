package forms

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/core"
)

func init() {
	adapter.Register(adapter.Spec{
		Type: "fake-tracker", Title: "Fake tracker", Roles: []adapter.Role{adapter.RoleTracker},
		Binding: map[adapter.Role][]adapter.Field{adapter.RoleTracker: {
			{Key: "team", Label: "Team", Type: adapter.Text, Required: true},
			{Key: "states", Label: "States", Type: adapter.Text, Default: "todo"},
		}},
		New: func(v adapter.Values, env adapter.Env) (adapter.Connection, error) { return fakeConn{v}, nil },
	})
}

// sharedConfig has two projects whose own trackers read the same team.
func sharedConfig(t *testing.T) *config.Config {
	t.Helper()
	isolate(t)
	cfg := config.New(filepath.Join(t.TempDir(), "c.yaml"))
	cfg.Connections["host"] = &config.Connection{Type: "fake-host", Values: adapter.Values{}}
	cfg.Connections["lin"] = &config.Connection{Type: "fake-tracker", Values: adapter.Values{}}
	cfg.Connections["agent"] = &config.Connection{Type: "fake-agent", Values: adapter.Values{}}
	cfg.Settings.Agent = "agent"
	for _, name := range []string{"web", "api"} {
		cfg.Projects = append(cfg.Projects, &config.Project{
			Name: name, Root: t.TempDir(),
			Repo:    &config.Binding{Connection: "host", Values: adapter.Values{"repo": name}},
			Tracker: &config.Binding{Connection: "lin", Values: adapter.Values{"team": "ENG"}},
		})
	}
	return cfg
}

func TestSharedTrackerOffer(t *testing.T) {
	cfg := sharedConfig(t)
	// The team's default states are filled in, and case doesn't matter.
	cfg.Projects[1].Tracker.Values = adapter.Values{"team": "eng", "states": "todo"}
	sd := SharedTracker(cfg, cfg.Projects[1])
	if sd == nil || !strings.Contains(sd.Offer, "web already takes its issues") || sd.Name != "lin" {
		t.Fatalf("offer %+v", sd)
	}
	*sd.value("web") = "web, frontend"
	*sd.value("api") = ""
	note, err := sd.Apply(cfg)
	if err != nil || note != "" {
		t.Fatal(note, err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	src := cfg.SourceList[0]
	if len(src.Routes) != 2 || src.Routes[0].Project != "web" || src.Routes[0].When.String() != "labels=web,frontend" || !src.Routes[1].When.Empty() {
		t.Fatalf("routes %+v", src.Routes)
	}
	if cfg.Projects[0].Tracker != nil || cfg.Projects[1].Tracker != nil || src.Tracker.Values["team"] != "eng" {
		t.Fatal("projects kept their trackers")
	}
	// Another team is somewhere else.
	cfg.Projects = append(cfg.Projects, &config.Project{Name: "ios", Root: t.TempDir(), Repo: &config.Binding{Connection: "host", Values: adapter.Values{"repo": "ios"}},
		Tracker: &config.Binding{Connection: "lin", Values: adapter.Values{"team": "MOB"}}})
	if SharedTracker(cfg, cfg.Projects[2]) != nil {
		t.Fatal("offered to share a different team")
	}
	// The same team again joins the source, ahead of the project taking everything else.
	cfg.Projects[2].Tracker.Values["team"] = "ENG"
	sd = SharedTracker(cfg, cfg.Projects[2])
	if sd == nil || !strings.Contains(sd.Offer, "The source lin already reads these issues") {
		t.Fatalf("offer %+v", sd)
	}
	if got := sd.asked(); len(got) != 3 {
		t.Fatal(got)
	}
	*sd.value("ios") = "mobile"
	if _, err := sd.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, r := range cfg.SourceList[0].Routes {
		order = append(order, r.Project)
	}
	if strings.Join(order, " ") != "web ios api" {
		t.Fatal(order)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestSharedTrackerDeclined(t *testing.T) {
	cfg := sharedConfig(t)
	sd := SharedTracker(cfg, cfg.Projects[1])
	sd.Accepted = false
	if len(sd.BasicGroups(cfg)) != 0 {
		t.Fatal("asked after a no")
	}
	if _, err := sd.Apply(cfg); err != nil || len(cfg.SourceList) != 0 || cfg.Projects[0].Tracker == nil {
		t.Fatal("applied after a no", err)
	}
}

func TestSourceDraftRoutes(t *testing.T) {
	cfg := sharedConfig(t)
	sd := SharedTracker(cfg, cfg.Projects[1])
	if _, err := sd.Routes(); err == nil {
		t.Fatal("two projects taking everything else")
	}
	*sd.value("web") = "web"
	sd.By, sd.Other = otherProperty, "components"
	routes, err := sd.Routes()
	if err != nil || routes[0].When.String() != "components=web" {
		t.Fatal(routes, err)
	}
	sd.issues = []core.Issue{
		{Key: "E-1", Title: "One", Attrs: map[string][]string{"components": {"Web"}}},
		{Key: "E-2", Title: "Two"},
	}
	sd.loadedFor = TrackerKey(cfg, sd.Tracker, sd.binding())
	p := sd.Preview(cfg)
	if !strings.Contains(p, "2 open issues") || !strings.Contains(p, "api                 1   everything else") || !strings.Contains(p, "nothing takes       0") {
		t.Fatal(p)
	}
	props := properties(sd.issues)
	if props[0].name != "labels" && props[0].name != "components" {
		t.Fatal(props)
	}
}

func TestSourceDraftKeepsHandWrittenRoutes(t *testing.T) {
	cfg := sharedConfig(t)
	cfg.Projects[0].Tracker, cfg.Projects[1].Tracker = nil, nil
	cfg.SourceList = []*config.Source{{Name: "lin", Tracker: &config.Binding{Connection: "lin", Values: adapter.Values{"team": "ENG"}}, Routes: []config.Route{
		{Project: "web", When: &config.Match{Fields: map[string][]string{"labels": {"web"}}, Not: &config.Match{Fields: map[string][]string{"state": {"Triage"}}}}},
		{Project: "api"},
	}}}
	sd := NewSourceDraft(cfg, cfg.SourceList[0])
	if !sd.Advanced() || len(sd.asked()) != 0 {
		t.Fatalf("advanced %v asked %v", sd.Advanced(), sd.asked())
	}
	if _, err := sd.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.SourceList[0].Routes[0].When.Not == nil {
		t.Fatal("hand-written route rewritten")
	}
	// Taking a project out gives it the tracker back.
	sd = NewSourceDraft(cfg, cfg.SourceList[0])
	sd.Projects = []string{"web"}
	note, err := sd.Apply(cfg)
	if err != nil || !strings.Contains(note, "api reads lin on its own") || cfg.Projects[1].Tracker == nil {
		t.Fatal(note, err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	RemoveSource(cfg, "lin")
	if len(cfg.SourceList) != 0 || cfg.Projects[0].Tracker == nil || cfg.Projects[0].Tracker.Values["team"] != "ENG" {
		t.Fatal("remove")
	}
}

func TestProjectDraftFromSource(t *testing.T) {
	cfg := sharedConfig(t)
	sd := SharedTracker(cfg, cfg.Projects[1])
	*sd.value("web") = "web"
	if _, err := sd.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	dir := gitRepo(t, "git@fakehost:me/ios.git")
	d := NewProjectDraft(cfg, nil, dir)
	d.Tracker = FromSource + "lin"
	d.BindingGroups(cfg)
	p, err := d.Apply(cfg)
	if err != nil || p.Tracker != nil {
		t.Fatal(p, err)
	}
	follow := d.Follow(cfg, p)
	if follow == nil || follow.Offer != "" || !contains(follow.Projects, p.Name) {
		t.Fatalf("follow %+v", follow)
	}
	*follow.value(p.Name) = "ios"
	if _, err := follow.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	// Back to its own tracker: the route goes.
	e := NewProjectDraft(cfg, cfg.Project(p.Name), "")
	if e.Tracker != FromSource+"lin" {
		t.Fatal(e.Tracker)
	}
	e.Tracker = "host"
	e.BindingGroups(cfg)
	if _, err := e.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if RoutedTo(cfg, p.Name) {
		t.Fatal("route kept")
	}
	// Clone keeps the sources.
	if c := Clone(cfg); len(c.SourceList) != 1 || c.SourceList[0] == cfg.SourceList[0] {
		t.Fatal("clone dropped or shared the sources")
	}
}
