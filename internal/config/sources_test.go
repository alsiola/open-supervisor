package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alsiola/open-supervisor/internal/adapter"
)

func TestGlob(t *testing.T) {
	cases := []struct {
		pattern, s string
		want       bool
	}{
		{"web", "Web", true},
		{"web", "website", false},
		{"web*", "website", true},
		{"area/*", "area/web/ui", true},
		{"*", "", true},
		{"API v?", "api V2", true},
		{"API v?", "api v10", false},
		{"*end*", "frontend", true},
		{"a*b*c", "aXbYbZc", true},
		{"a*b*c", "aXbYbZ", false},
	}
	for _, c := range cases {
		if got := Glob(c.pattern, c.s); got != c.want {
			t.Errorf("Glob(%q, %q) = %v", c.pattern, c.s, got)
		}
	}
}

const routedYAML = `version: 1
settings:
  agent: claude
connections:
  claude: { type: test-agent }
  gh: { type: test-host, token: "env:X" }
projects:
  - { name: web, root: /w, repo: { connection: gh } }
  - { name: api, root: /a, repo: { connection: gh } }
  - { name: infra, root: /i, repo: { connection: gh } }
sources:
  - name: linear
    tracker: { connection: gh, team: ENG }
    unrouted: label
    labels: { claimed: robot }
    routes:
      - project: web
        when: { labels: [web, frontend] }
      - project: api
        when: { project: "API v2", priority: 1 }
      - project: infra
        when:
          any:
            - { labels: infra }
            - { components: "Terraform*" }
          not: { state: Backlog }
`

func TestSourcesLoadAndRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(routedYAML), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	src := c.SourceFor(c.Project("api"))
	if src == nil || src.Name != "linear" || src.Implicit() || src.Tracker.Values["team"] != "ENG" {
		t.Fatalf("api's source: %+v", src)
	}
	if got := c.LabelsFor(c.Project("web")); got.Claimed != "robot" || got.Review != "ready-for-review" || got.Unrouted != "needs-routing" {
		t.Fatalf("a routed project's labels: %+v", got)
	}
	infra := src.Routes[2].When
	attr := func(m map[string][]string) func(string) []string { return func(k string) []string { return m[k] } }
	if !infra.Matches(attr(map[string][]string{"components": {"Terraform AWS"}, "state": {"Todo"}})) {
		t.Error("infra should take a Terraform issue")
	}
	if infra.Matches(attr(map[string][]string{"labels": {"infra"}, "state": {"backlog"}})) {
		t.Error("infra shouldn't take a backlog issue")
	}
	if src.Routes[1].When.Matches(attr(map[string][]string{"project": {"API v2"}})) {
		t.Error("fields are ANDed: priority is missing")
	}
	if !src.Routes[1].When.Matches(attr(map[string][]string{"project": {"api V2"}, "priority": {"1"}})) {
		t.Error("the api route should match")
	}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	again, err := Load(path)
	if err != nil {
		data, _ := os.ReadFile(path)
		t.Fatalf("%v\n%s", err, data)
	}
	if a, b := src.Routes[2].When.String(), again.SourceList[0].Routes[2].When.String(); a != b {
		t.Fatalf("round trip: %s became %s", a, b)
	}
	if got := again.SourceList[0].Routes[1].When.String(); got != `priority=1 project="API v2"` {
		t.Fatalf("written: %s", got)
	}
	names := []string{}
	for _, s := range again.Sources() {
		names = append(names, s.Name)
	}
	if strings.Join(names, ",") != "linear" {
		t.Fatalf("sources: %v", names)
	}
}

func TestSourcesValidate(t *testing.T) {
	c := New("x.yaml")
	c.Settings.Agent = "claude"
	c.Connections["claude"] = &Connection{Type: "test-agent", Values: adapter.Values{}}
	c.Connections["gh"] = &Connection{Type: "test-host", Values: adapter.Values{"token": "x"}}
	gh := func() *Binding { return &Binding{Connection: "gh", Values: adapter.Values{}} }
	c.Projects = []*Project{
		{Name: "web", Root: "/w", Repo: gh()},
		{Name: "own", Root: "/o", Repo: gh(), Tracker: gh()},
		{Name: "lonely", Root: "/l", Repo: gh()},
		{Name: "labelled", Root: "/x", Repo: gh(), Labels: &Labels{Claimed: "mine"}},
	}
	c.SourceList = []*Source{
		{Name: "a", Tracker: gh(), Unrouted: "shout", Routes: []Route{{Project: "web"}, {Project: "own"}, {Project: "ghost"}, {Project: "labelled"}}},
		{Name: "b", Tracker: gh(), Routes: []Route{{Project: "web", When: &Match{Fields: map[string][]string{"labels": {"x"}}}}}},
		{Name: "web", Tracker: gh(), Routes: []Route{{Project: "web"}}},
	}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{
		`unrouted is "ignore" or "label", not "shout"`,
		"route 1 takes everything, so the routes after it never match",
		"project own has a tracker of its own",
		`no project named "ghost"`,
		"project web: routed from both a and b",
		"source web: a project has the same name",
		"project lonely: needs a tracker connection, or a source with a route to it",
		"project labelled: its labels belong to its source; set them on a",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
}
