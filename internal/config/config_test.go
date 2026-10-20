package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alsiola/open-supervisor/internal/adapter"
)

func init() {
	adapter.Register(adapter.Spec{Type: "test-host", Title: "Test host", Roles: []adapter.Role{adapter.RoleRepo, adapter.RoleTracker},
		Fields: []adapter.Field{{Key: "token", Label: "Token", Type: adapter.Secret, Required: true}}})
	adapter.Register(adapter.Spec{Type: "test-agent", Title: "Test agent", Roles: []adapter.Role{adapter.RoleAgent}})
}

func TestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	c := New(path)
	c.Settings.Agent = "claude"
	c.Connections["claude"] = &Connection{Type: "test-agent", Values: adapter.Values{"model": "opus"}}
	c.Connections["gh"] = &Connection{Type: "test-host", Values: adapter.Values{"token": "env:GH_TOKEN"}}
	c.Projects = []*Project{{Name: "site", Root: "~/Code/site", Repo: &Binding{Connection: "gh", Values: adapter.Values{"repo": "me/site"}}, Tracker: &Binding{Connection: "gh", Values: adapter.Values{}}}}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "model: opus") || !strings.Contains(string(data), "repo: me/site") {
		t.Fatalf("inline values weren't written flat:\n%s", data)
	}
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Connections["claude"].Values["model"] != "opus" || got.Projects[0].Repo.Values["repo"] != "me/site" || got.Projects[0].Repo.Connection != "gh" {
		t.Fatalf("%+v", got.Projects[0].Repo)
	}
	if got.Settings.MaxTurns != 4 || got.Settings.Labels.Claimed != "agent-in-progress" || !got.Settings.Jobs {
		t.Fatalf("defaults: %+v", got.Settings)
	}
}

func TestValidate(t *testing.T) {
	c := New("x.yaml")
	c.Settings.Agent = "gh"
	c.Connections["gh"] = &Connection{Type: "test-host", Values: adapter.Values{}}
	c.Connections["bad name"] = &Connection{Type: "nope", Values: adapter.Values{}}
	c.Projects = []*Project{{Name: "a", Root: "/x", Repo: &Binding{Connection: "missing"}, Tracker: &Binding{Connection: "gh"},
		Jobs: []Job{{Name: "j", Command: []string{"true"}, On: &JobTrigger{Branches: []string{"main"}}}}}}
	err := c.Validate()
	if err == nil {
		t.Fatal("expected errors")
	}
	for _, want := range []string{"can't be a agent", "unknown type", "Token is required", `no connection named "missing"`, "names are letters", "job j: on needs events or webhook"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
}

func TestLabelsMerge(t *testing.T) {
	base := DefaultSettings().Labels
	l := Labels{Claimed: "claude-in-progress", Help: "claude needs help"}.Merge(base)
	if l.Claimed != "claude-in-progress" || l.Review != base.Review || len(l.Skip) != 3 || l.Status("help") != "claude needs help" {
		t.Fatalf("%+v", l)
	}
}

func TestSecrets(t *testing.T) {
	t.Setenv("OS_TEST_SECRET", "s3cret")
	if v, err := ResolveSecret("env:OS_TEST_SECRET"); err != nil || v != "s3cret" {
		t.Fatal(v, err)
	}
	if v, _ := ResolveSecret("https://example.com"); v != "https://example.com" {
		t.Fatal("a URL isn't a reference")
	}
	if v, err := ResolveSecret("cmd:echo hi"); err != nil || v != "hi" {
		t.Fatal(v, err)
	}
	if _, err := ResolveSecret("env:OS_TEST_UNSET"); err == nil {
		t.Fatal("an unset variable should fail")
	}
}
