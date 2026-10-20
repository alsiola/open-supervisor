package forms

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/config"
)

type fakeConn struct{ v adapter.Values }

func (f fakeConn) Check(context.Context) (string, error) {
	if f.v["fail"] == "true" {
		return "", errors.New("nope")
	}
	return "fake as " + f.v["user"], nil
}

func (f fakeConn) Repo(adapter.Values, adapter.ProjectInfo) (adapter.Repo, error) { return nil, nil }
func (f fakeConn) DetectRepo(remote string) (adapter.Values, bool) {
	if strings.Contains(remote, "fakehost") {
		return adapter.Values{"repo": strings.TrimSuffix(filepath.Base(remote), ".git")}, true
	}
	return nil, false
}
func (f fakeConn) Tracker(adapter.Values, adapter.ProjectInfo) (adapter.Tracker, error) {
	return nil, nil
}

var authFields = []adapter.Field{
	{Key: "user", Label: "User", Type: adapter.Text},
	{Key: "auth", Label: "Auth", Type: adapter.Select, Options: []string{"cli", "token"}, Default: "cli"},
	{Key: "token", Label: "Token", Type: adapter.Secret, Required: true, ShowIf: map[string][]string{"auth": {"token"}}},
	{Key: "debug", Label: "Debug", Type: adapter.Bool},
	{Key: "retries", Label: "Retries", Type: adapter.Int},
}

func init() {
	adapter.Register(adapter.Spec{
		Type: "fake-host", Title: "Fake host", Roles: []adapter.Role{adapter.RoleRepo, adapter.RoleTracker},
		Fields:  authFields,
		Binding: map[adapter.Role][]adapter.Field{adapter.RoleRepo: {{Key: "repo", Label: "Repo", Type: adapter.Text, Required: true}}},
		New:     func(v adapter.Values, env adapter.Env) (adapter.Connection, error) { return fakeConn{v}, nil },
	})
	adapter.Register(adapter.Spec{
		Type: "fake-agent", Title: "Fake agent", Roles: []adapter.Role{adapter.RoleAgent},
		New: func(v adapter.Values, env adapter.Env) (adapter.Connection, error) { return fakeConn{v}, nil },
	})
}

func isolate(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	t.Setenv("OPEN_SUPERVISOR_STATE", filepath.Join(dir, "state"))
	keyring.MockInit()
}

func TestShowIf(t *testing.T) {
	isolate(t)
	fs := New("gh", authFields, nil)
	if fs.Visible(authFields[2]) {
		t.Fatal("token shown with auth=cli")
	}
	fs.Set("auth", "token")
	if !fs.Visible(authFields[2]) {
		t.Fatal("token hidden with auth=token")
	}
	if err := fs.Validate(); err == nil {
		t.Fatal("missing required token passed")
	}
	// fields sharing a ShowIf share a group: [user auth] [token] [debug retries]
	if n := len(fs.Groups("x")); n != 3 {
		t.Fatalf("groups = %d", n)
	}
	fs.Set("auth", "cli")
	v, err := fs.Apply()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := v["token"]; ok {
		t.Fatal("hidden field applied")
	}
	if v["auth"] != "cli" || v["debug"] != "false" {
		t.Fatalf("values %v", v)
	}
}

func TestSecretKeyring(t *testing.T) {
	isolate(t)
	fs := New("gh", authFields, adapter.Values{"auth": "token"})
	fs.Set("token", "s3cret")
	v, err := fs.Apply()
	if err != nil {
		t.Fatal(err)
	}
	if v["token"] != "keyring:gh.token" {
		t.Fatalf("token = %q", v["token"])
	}
	if got, _ := config.ResolveSecret(v["token"]); got != "s3cret" {
		t.Fatalf("resolved %q", got)
	}
}

func TestSecretFileFallback(t *testing.T) {
	isolate(t)
	keyring.MockInitWithError(errors.New("no keyring here"))
	fs := New("jira", authFields, adapter.Values{"auth": "token"})
	fs.Set("token", "tok")
	v, err := fs.Apply()
	if err != nil {
		t.Fatal(err)
	}
	if v["token"] != "file:jira.token" {
		t.Fatalf("token = %q", v["token"])
	}
	info, err := os.Stat(filepath.Join(config.ConfigDir(), "secrets.yaml"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("secrets file %v %v", info, err)
	}
	if got, _ := config.ResolveSecret(v["token"]); got != "tok" {
		t.Fatalf("resolved %q", got)
	}
}

func TestSecretKeptAndReferences(t *testing.T) {
	isolate(t)
	fs := New("gh", authFields, adapter.Values{"auth": "token", "token": "keyring:gh.token"})
	if err := fs.Validate(); err != nil {
		t.Fatalf("stored secret should satisfy required: %v", err)
	}
	v, _ := fs.Apply()
	if v["token"] != "keyring:gh.token" {
		t.Fatalf("blank didn't keep: %q", v["token"])
	}
	fs.Set("token", "env:GITHUB_TOKEN")
	v, _ = fs.Apply()
	if v["token"] != "env:GITHUB_TOKEN" {
		t.Fatalf("reference not kept verbatim: %q", v["token"])
	}
}

func TestIntValidation(t *testing.T) {
	isolate(t)
	fs := New("x", authFields, nil)
	fs.Set("retries", "three")
	if err := fs.Validate(); err == nil {
		t.Fatal("non-number passed")
	}
}

func TestConnectionDraftAndRename(t *testing.T) {
	isolate(t)
	cfg := config.New(filepath.Join(t.TempDir(), "c.yaml"))
	d := NewConnectionDraft(cfg, "", adapter.RoleRepo)
	d.Type = "fake-host"
	d.FieldGroups(cfg)
	d.Fields.Set("user", "alex")
	name, err := d.Apply(cfg)
	if err != nil || name != "fake-host" {
		t.Fatalf("%q %v", name, err)
	}
	if who, err := TestConnection(cfg, name); err != nil || who != "fake as alex" {
		t.Fatalf("%q %v", who, err)
	}
	cfg.Settings.Telemetry = []string{"fake-host"}
	cfg.Projects = []*config.Project{{Name: "p", Repo: &config.Binding{Connection: "fake-host"}, Tracker: &config.Binding{Connection: "fake-host"}}}
	if len(Uses(cfg, "fake-host")) != 3 {
		t.Fatalf("uses %v", Uses(cfg, "fake-host"))
	}
	e := NewConnectionDraft(cfg, "fake-host", "")
	e.Name = "host"
	e.FieldGroups(cfg)
	if _, err := e.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if _, ok := cfg.Connections["fake-host"]; ok || cfg.Projects[0].Repo.Connection != "host" || cfg.Settings.Telemetry[0] != "host" {
		t.Fatalf("rename didn't follow: %+v", cfg.Projects[0].Repo)
	}
	if cfg.Connections["host"].Values["user"] != "alex" {
		t.Fatal("values lost on rename")
	}
	if UniqueName(cfg, "host") != "host-2" {
		t.Fatal(UniqueName(cfg, "host"))
	}
}

func gitRepo(t *testing.T, remote string, files ...string) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "my app")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"init", "-q"}, {"remote", "add", "origin", remote}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s", args, out)
		}
	}
	for _, f := range files {
		os.WriteFile(filepath.Join(dir, f), []byte("x"), 0o644)
	}
	return dir
}

func TestSuggestions(t *testing.T) {
	cases := map[string]string{"pnpm-lock.yaml": "pnpm install --frozen-lockfile", "package-lock.json": "npm ci", "yarn.lock": "yarn install --frozen-lockfile", "bun.lockb": "bun install", "go.sum": "go mod download", "uv.lock": "uv sync", "Cargo.lock": ""}
	for file, want := range cases {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, file), nil, 0o644)
		setup, when := SuggestSetup(dir)
		if setup != want || (want != "" && when != file) {
			t.Errorf("%s: %q %q", file, setup, when)
		}
	}
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "pnpm-lock.yaml"), nil, 0o644)
	os.WriteFile(filepath.Join(dir, "package-lock.json"), nil, 0o644)
	if s, _ := SuggestSetup(dir); s != "pnpm install --frozen-lockfile" {
		t.Errorf("pnpm should win: %q", s)
	}
	if SuggestCopy(dir) != nil {
		t.Error("no .env, nothing to copy")
	}
	os.WriteFile(filepath.Join(dir, ".env"), nil, 0o644)
	if c := SuggestCopy(dir); len(c) != 1 || c[0] != ".env" {
		t.Errorf("copy %v", c)
	}
}

func TestProjectDraft(t *testing.T) {
	isolate(t)
	cfg := config.New(filepath.Join(t.TempDir(), "c.yaml"))
	cfg.Connections["host"] = &config.Connection{Type: "fake-host", Values: adapter.Values{}}
	cfg.Connections["agent"] = &config.Connection{Type: "fake-agent", Values: adapter.Values{}}
	cfg.Settings.Agent = "agent"
	dir := gitRepo(t, "git@fakehost:me/widgets.git", "go.sum", ".env")
	d := NewProjectDraft(cfg, nil, dir)
	if d.Name != "my-app" || d.Repo != "host" || d.Tracker != "host" {
		t.Fatalf("defaults %+v", d)
	}
	if d.Setup != "go mod download" || d.Copy != ".env" {
		t.Fatalf("worktree %q %q", d.Setup, d.Copy)
	}
	d.BindingGroups(cfg)
	if d.RepoFields == nil || d.RepoFields.Current()["repo"] != "widgets" {
		t.Fatalf("detected %v", d.RepoFields.Current())
	}
	p, err := d.Apply(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if p.Repo.Values["repo"] != "widgets" || len(p.Tracker.Values) != 0 || p.Worktree.SetupWhen[0] != "go.sum" {
		t.Fatalf("project %+v %+v", p.Repo, p.Tracker)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if DefaultProjectName(cfg, dir) != "my-app-2" {
		t.Fatal(DefaultProjectName(cfg, dir))
	}
	// editing keeps its place and its binding
	e := NewProjectDraft(cfg, p, "")
	e.Branch = "work/{key}"
	if _, err := e.Apply(cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Projects) != 1 || cfg.Projects[0].Branch != "work/{key}" || cfg.Projects[0].Repo.Values["repo"] != "widgets" {
		t.Fatalf("edit %+v", cfg.Projects[0])
	}
}

func TestSettingsDraft(t *testing.T) {
	cfg := config.New("x")
	d := NewSettingsDraft(cfg)
	d.MaxTurns = "7"
	d.Skip = "later, people"
	d.Help = ""
	d.Branch = "nokey"
	d.Apply(cfg)
	s := cfg.Settings
	if s.MaxTurns != 7 || len(s.Labels.Skip) != 2 || s.Labels.Skip[1] != "people" || s.Labels.Help != config.DefaultSettings().Labels.Help || s.Branch != config.DefaultSettings().Branch {
		t.Fatalf("%+v", s)
	}
}

func TestClone(t *testing.T) {
	cfg := config.New("x")
	cfg.Connections["a"] = &config.Connection{Type: "fake-agent", Values: adapter.Values{"k": "v"}}
	cfg.Projects = []*config.Project{{Name: "p", Repo: &config.Binding{Connection: "a", Values: adapter.Values{"r": "1"}}}}
	c := Clone(cfg)
	c.Connections["a"].Values["k"] = "changed"
	c.Projects[0].Repo.Values["r"] = "2"
	c.Projects[0].Name = "q"
	if cfg.Connections["a"].Values["k"] != "v" || cfg.Projects[0].Repo.Values["r"] != "1" || cfg.Projects[0].Name != "p" {
		t.Fatal("clone shares state")
	}
}

func TestProjectNeedsCheckout(t *testing.T) {
	cfg := config.New("x")
	cfg.Connections["host"] = &config.Connection{Type: "fake-host", Values: adapter.Values{}}
	d := NewProjectDraft(cfg, nil, "")
	if _, err := d.Apply(cfg); err == nil {
		t.Fatal("a project with no checkout was accepted")
	}
	if IsGitCheckout("") {
		t.Fatal("empty path counted as a checkout")
	}
}
