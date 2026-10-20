package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/config"
)

func TestEditorSourcesTab(t *testing.T) {
	cfg := config.New(t.TempDir() + "/c.yaml")
	cfg.Connections["lin"] = &config.Connection{Type: "linear", Values: adapter.Values{}}
	cfg.Projects = []*config.Project{{Name: "web"}, {Name: "api"}}
	cfg.SourceList = []*config.Source{{Name: "linear", Tracker: &config.Binding{Connection: "lin", Values: adapter.Values{"team": "ENG"}}, Unrouted: config.UnroutedLabel, Routes: []config.Route{
		{Project: "web", When: &config.Match{Fields: map[string][]string{"labels": {"web"}}}},
		{Project: "api"},
	}}}
	e := &editor{cfg: cfg, width: 120, height: 40}
	e.update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("4")})
	v := e.view()
	for _, want := range []string{"4 Sources", "linear", "labelled needs-routing", "1. web", "when labels=web", "2. api", "when everything"} {
		if !strings.Contains(v, want) {
			t.Fatalf("missing %q in\n%s", want, v)
		}
	}
	// Editing starts the source's forms.
	e.update(tea.KeyMsg{Type: tea.KeyEnter})
	if e.form == nil {
		t.Fatal("no form for the source")
	}
}

func TestEditorStepsCanAddSteps(t *testing.T) {
	e := &editor{cfg: config.New("x"), width: 80, height: 30}
	var order []string
	form := func(name string) func() *huh.Form {
		return func() *huh.Form {
			order = append(order, name)
			return nil
		}
	}
	e.start([]func() *huh.Form{
		form("a"),
		func() *huh.Form {
			order = append(order, "b")
			e.steps = append([]func() *huh.Form{form("b1"), form("b2")}, e.steps...)
			return nil
		},
		form("c"),
	}, func() error { order = append(order, "done"); return nil })
	if got := strings.Join(order, " "); got != "a b b1 b2 c done" {
		t.Fatal(got)
	}
}
