package configcli

import (
	"bytes"
	"context"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/config"
)

type fakeConn struct{}

func (fakeConn) Check(context.Context) (string, error) { return "fake", nil }
func (fakeConn) Repo(adapter.Values, adapter.ProjectInfo) (adapter.Repo, error) {
	return nil, nil
}
func (fakeConn) DetectRepo(remote string) (adapter.Values, bool) {
	return adapter.Values{"repo": strings.TrimSuffix(filepath.Base(remote), ".git")}, true
}
func (fakeConn) Tracker(adapter.Values, adapter.ProjectInfo) (adapter.Tracker, error) {
	return nil, nil
}

func init() {
	repo := []adapter.Field{{Key: "repo", Label: "Repo", Type: adapter.Text, Required: true}}
	adapter.Register(adapter.Spec{
		Type: "cli-host", Roles: []adapter.Role{adapter.RoleRepo, adapter.RoleTracker},
		Binding: map[adapter.Role][]adapter.Field{adapter.RoleRepo: repo, adapter.RoleTracker: repo},
		New:     func(adapter.Values, adapter.Env) (adapter.Connection, error) { return fakeConn{}, nil },
	})
	adapter.Register(adapter.Spec{
		Type: "cli-tracker", Roles: []adapter.Role{adapter.RoleTracker},
		Fields:  []adapter.Field{{Key: "token", Label: "Token", Type: adapter.Secret, Required: true}},
		Binding: map[adapter.Role][]adapter.Field{adapter.RoleTracker: {{Key: "team", Label: "Team", Type: adapter.Text, Required: true}}},
		New:     func(adapter.Values, adapter.Env) (adapter.Connection, error) { return fakeConn{}, nil },
	})
	adapter.Register(adapter.Spec{
		Type: "cli-agent", Roles: []adapter.Role{adapter.RoleAgent},
		New: func(adapter.Values, adapter.Env) (adapter.Connection, error) { return fakeConn{}, nil },
	})
}

type harness struct {
	t    *testing.T
	path string
}

func newHarness(t *testing.T) *harness {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	t.Setenv("OPEN_SUPERVISOR_STATE", filepath.Join(dir, "state"))
	keyring.MockInit()
	return &harness{t: t, path: filepath.Join(dir, "config.yaml")}
}

// run runs `config <args>` and returns stdout and stderr.
func (h *harness) run(args ...string) (string, string, error) {
	cmd := Command(func() string { return h.path })
	var out, errs bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&errs)
	cmd.SetArgs(args)
	cmd.SilenceUsage, cmd.SilenceErrors = true, true
	err := cmd.Execute()
	return out.String(), errs.String(), err
}

func (h *harness) ok(args ...string) string {
	h.t.Helper()
	out, errs, err := h.run(args...)
	if err != nil {
		h.t.Fatalf("config %s: %v\n%s", strings.Join(args, " "), err, errs)
	}
	return out + errs
}

func (h *harness) cfg() *config.Config {
	h.t.Helper()
	cfg, err := config.Read(h.path)
	if err != nil {
		h.t.Fatal(err)
	}
	return cfg
}

func checkout(t *testing.T, name string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), name)
	for _, args := range [][]string{{"init", "-q", dir}, {"-C", dir, "remote", "add", "origin", "git@host:me/" + name + ".git"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	return dir
}

func TestSettingsPaths(t *testing.T) {
	h := newHarness(t)
	h.ok("set", "settings.maxTurns", "2")
	if got := strings.TrimSpace(h.ok("get", "settings.maxTurns")); got != "2" {
		t.Fatal(got)
	}
	h.ok("set", "settings.labels.skip.+", "wontfix")
	h.ok("set", "settings.branch", "work/{key}", "--string")
	h.ok("set", "settings.telemetry", "[a, b]")
	cfg := h.cfg()
	s := cfg.Settings
	if s.MaxTurns != 2 || s.Labels.Skip[len(s.Labels.Skip)-1] != "wontfix" || s.Branch != "work/{key}" || len(s.Telemetry) != 2 {
		t.Fatalf("%+v", s)
	}
	h.ok("unset", "settings.maxTurns")
	if h.cfg().Settings.MaxTurns != config.DefaultSettings().MaxTurns {
		t.Fatal("unset kept the value")
	}
	if _, _, err := h.run("get", "settings.nope"); err == nil {
		t.Fatal("got a missing key")
	}
	if _, _, err := h.run("set", "settings.maxTurn", "3"); err == nil || !strings.Contains(err.Error(), "no settings.maxTurn") {
		t.Fatal("set a key the config doesn't have", err)
	}
	if _, _, err := h.run("set", "projects.web.branch", "x"); err == nil {
		t.Fatal("made a project from a path")
	}
}

func TestBuildASharedSetup(t *testing.T) {
	h := newHarness(t)
	h.ok("connection", "set", "host", "--type", "cli-host", "--no-test")
	h.ok("connection", "set", "lin", "--type", "cli-tracker", "--set", "token=env:LIN_TOKEN", "--no-test")
	// A plain secret goes to the keyring, never the file.
	h.ok("set", "connections.lin.token", "s3cret")
	if ref := h.cfg().Connections["lin"].Values["token"]; ref != "keyring:lin.token" {
		t.Fatal(ref)
	}
	if v, _ := config.ResolveSecret("keyring:lin.token"); v != "s3cret" {
		t.Fatal(v)
	}
	h.ok("connection", "set", "agent", "--type", "cli-agent", "--no-test")
	h.ok("set", "settings.agent", "agent")
	if _, _, err := h.run("connection", "set", "lin", "--set", "nope=1", "--no-test"); err == nil {
		t.Fatal("took an unknown field")
	}
	web, api := checkout(t, "web"), checkout(t, "api")
	h.ok("project", "set", "web", "--root", web, "--code", "host", "--issues", "lin", "--tracker", "team=ENG")
	out := h.ok("project", "set", "api", "--root", api, "--code", "host", "--issues", "lin", "--tracker", "team=ENG")
	if !strings.Contains(out, "both would claim the same ones") || !strings.Contains(out, "config source set lin --tracker lin --set team=ENG") {
		t.Fatal(out)
	}
	if p := h.cfg().Project("web"); p.Repo.Values["repo"] != "web" {
		t.Fatal("repo not detected", p.Repo.Values)
	}
	h.ok("source", "set", "linear", "--tracker", "lin", "--set", "team=ENG", "--unrouted", "label")
	h.ok("route", "set", "linear", "api")
	h.ok("route", "set", "linear", "web", "--when", "labels=web,frontend")
	out = h.ok("route", "list")
	if !strings.Contains(out, "1. web              issues with labels=web,frontend") || !strings.Contains(out, "2. api              everything else") {
		t.Fatal(out)
	}
	cfg := h.cfg()
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Project("web").Tracker != nil || cfg.SourceList[0].Unrouted != config.UnroutedLabel {
		t.Fatal("web kept its tracker")
	}
	if _, _, err := h.run("route", "set", "linear", "web"); err == nil {
		t.Fatal("two routes taking everything else")
	}
	// A new project from the source is told to add its route.
	ios := checkout(t, "ios")
	out = h.ok("project", "set", "ios", "--root", ios, "--code", "host", "--source", "linear")
	if !strings.Contains(out, "config route set linear ios") {
		t.Fatal(out)
	}
	h.ok("route", "set", "linear", "ios", "--match", "{labels: [ios], not: {title: ['*wip*']}}")
	cfg = h.cfg()
	if r := cfg.SourceList[0].Routes[1]; r.Project != "ios" || r.When.Not == nil {
		t.Fatalf("%+v", cfg.SourceList[0].Routes)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	// Taking the last route out of a source gives the project its tracker back.
	h.ok("route", "remove", "linear", "ios")
	h.ok("route", "remove", "linear", "web")
	h.ok("route", "remove", "linear", "api")
	cfg = h.cfg()
	if len(cfg.SourceList) != 0 || cfg.Project("api").Tracker == nil || cfg.Project("api").Tracker.Values["team"] != "ENG" {
		t.Fatalf("%+v", cfg.Project("api").Tracker)
	}
	if _, _, err := h.run("connection", "remove", "lin"); err == nil {
		t.Fatal("removed a connection in use")
	}
	h.ok("project", "remove", "ios")
	if _, _, err := h.run("project", "set", "web", "--branch", "w/{key}"); err == nil {
		t.Fatal("saved web with no issues from anywhere")
	}
	h.ok("project", "set", "api", "--branch", "w/{key}")
	if p := h.cfg().Project("api"); p.Branch != "w/{key}" || p.Repo.Values["repo"] != "api" || p.Tracker.Values["team"] != "ENG" {
		t.Fatal("edit lost the binding")
	}
}

func TestReviewFixes(t *testing.T) {
	h := newHarness(t)
	h.ok("connection", "set", "host", "--type", "cli-host", "--no-test")
	h.ok("connection", "set", "lin", "--type", "cli-tracker", "--set", "token=env:T", "--no-test")
	// A source with no routes yet survives another project being set up.
	h.ok("source", "set", "linear", "--tracker", "lin", "--set", "team=ENG")
	h.ok("source", "set", "other", "--tracker", "lin", "--set", "team=OPS")
	docs := checkout(t, "docs")
	h.ok("project", "set", "docs", "--root", docs, "--code", "host", "--issues", "host")
	web, api := checkout(t, "web"), checkout(t, "api")
	h.ok("project", "set", "web", "--root", web, "--code", "host", "--source", "linear")
	if len(h.cfg().SourceList) != 2 {
		t.Fatal("an empty source was dropped")
	}
	h.ok("project", "set", "api", "--root", api, "--code", "host", "--source", "linear")
	h.ok("route", "set", "linear", "web", "--when", "labels=web")
	h.ok("route", "set", "linear", "web", "--also")
	// --position past the catch-all stays before it.
	h.ok("route", "set", "linear", "api", "--when", "labels=api", "--position", "9")
	var order []string
	for _, r := range h.cfg().SourceList[0].Routes {
		order = append(order, r.Project)
	}
	if strings.Join(order, " ") != "web api web" {
		t.Fatal(order)
	}
	// The same property twice must match both.
	h.ok("route", "set", "linear", "api", "--when", "labels=api", "--when", "labels=urgent")
	r := h.cfg().SourceList[0].Routes[1]
	if !r.When.Matches(func(string) []string { return []string{"api", "urgent"} }) || r.When.Matches(func(string) []string { return []string{"urgent"} }) {
		t.Fatalf("%s", r.When)
	}
	// Setting an existing key the YAML leaves out, or renaming through the path, works.
	h.ok("set", "projects.docs.disabled", "false")
	h.ok("set", "projects.docs.name", "handbook")
	if h.cfg().Project("handbook") == nil {
		t.Fatal("rename")
	}
	// A secret set inside a map still goes to the keyring.
	h.ok("set", "connections.lin2", "{type: cli-tracker, token: plain}")
	if v := h.cfg().Connections["lin2"].Values["token"]; v != "keyring:lin2.token" {
		t.Fatal(v)
	}
	if _, _, err := h.run("project", "set", "bad name", "--root", web); err == nil {
		t.Fatal("bad name")
	}
}
