// Package config is the supervisor's settings file: the connections it works through, the projects it watches
// and the knobs that govern both. It lives at ~/.config/open-supervisor/config.yaml (or $XDG_CONFIG_HOME, or
// --config, or $OPEN_SUPERVISOR_CONFIG) and is written by the onboarding wizard and the TUI's config editor, so
// everything in it can be edited from either. Secrets are kept out of it: see secrets.go.
package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/alsiola/open-supervisor/internal/adapter"
)

// Name is the tool's name, used for its directories, the keyring service and the agent plugin.
const Name = "open-supervisor"

// Config is the whole settings file.
type Config struct {
	Version     int                    `yaml:"version"`
	Settings    Settings               `yaml:"settings"`
	Connections map[string]*Connection `yaml:"connections"`
	Projects    []*Project             `yaml:"projects"`
	// SourceList are trackers that feed several projects through routes; see Source.
	SourceList []*Source `yaml:"sources,omitempty"`

	// Path is where the config was read from and is saved to.
	Path string `yaml:"-"`
}

// Settings govern every project.
type Settings struct {
	// MaxTurns is the number of turns running at once across every project. Idle sessions cost nothing, so this
	// is the only concurrency that matters.
	MaxTurns int `yaml:"maxTurns"`
	// MaxOpen is the number of issues claimed at once in each project; the rest wait, oldest first.
	MaxOpen          int `yaml:"maxOpenPerProject"`
	ReconcileMinutes int `yaml:"reconcileMinutes"`
	// Port is the local webhook server's port.
	Port int `yaml:"port"`
	// Agent is the connection projects use to run turns unless they name another.
	Agent string `yaml:"agent"`
	// Telemetry are the connections metrics go to.
	Telemetry []string `yaml:"telemetry,omitempty"`
	// Tunnel is the connection that exposes the webhook port; empty means no webhooks, only the reconcile.
	Tunnel string `yaml:"tunnel,omitempty"`
	// Labels are the labels the supervisor sets and reads; a project can override them.
	Labels Labels `yaml:"labels"`
	// Branch names an issue's branch; {key} is the issue key.
	Branch string `yaml:"branch"`
	// Git is how agents use git; a project can override it.
	Git Git `yaml:"git,omitempty"`
	// SweepAt is the local time (HH:MM) of the daily corrections sweep; empty turns it off.
	SweepAt string `yaml:"sweepAt"`
	// TriageAt is the local time (HH:MM) of the daily triage of the problems agents report; empty turns it off.
	TriageAt string `yaml:"triageAt"`
	// Jobs runs the projects' jobs.
	Jobs         bool `yaml:"jobs"`
	UsageMinutes int  `yaml:"usageMinutes"`
	// MaxAttempts is how many times a failed turn is retried before the issue waits for a hand.
	MaxAttempts           int `yaml:"maxAttempts"`
	ChecksDebounceSeconds int `yaml:"checksDebounceSeconds"`
	// NoChecksMinutes is how long a PR head with no checks at all waits before the agent hears none ran.
	NoChecksMinutes int  `yaml:"noChecksMinutes"`
	DryRun          bool `yaml:"dryRun,omitempty"`
}

// Labels are the labels the supervisor uses.
type Labels struct {
	Claimed   string   `yaml:"claimed,omitempty"`
	Scheduled string   `yaml:"scheduled,omitempty"`
	Skip      []string `yaml:"skip,omitempty"`
	// Require, when set, makes claiming opt-in: only issues with one of these labels (or the scheduled label) are
	// claimed. Empty claims every issue the skip labels don't rule out.
	Require []string `yaml:"require,omitempty"`
	Help    string   `yaml:"help,omitempty"`
	Review  string   `yaml:"review,omitempty"`
	Done    string   `yaml:"done,omitempty"`
	// Unrouted marks an issue no route of its source takes, on sources that ask for it.
	Unrouted string `yaml:"unrouted,omitempty"`
}

// Merge fills the empty labels from base.
func (l Labels) Merge(base Labels) Labels {
	pick := func(a, b string) string {
		if a != "" {
			return a
		}
		return b
	}
	out := Labels{
		Claimed:   pick(l.Claimed, base.Claimed),
		Scheduled: pick(l.Scheduled, base.Scheduled),
		Help:      pick(l.Help, base.Help),
		Review:    pick(l.Review, base.Review),
		Done:      pick(l.Done, base.Done),
		Unrouted:  pick(l.Unrouted, base.Unrouted),
		Skip:      l.Skip,
		Require:   l.Require,
	}
	if out.Skip == nil {
		out.Skip = base.Skip
	}
	if out.Require == nil {
		out.Require = base.Require
	}
	return out
}

// Admits reports whether the labels let an issue be claimed: none of the skip labels, and one of the required
// ones when there are any. has says whether the issue carries a label.
func (l Labels) Admits(has func(string) bool) bool {
	for _, s := range l.Skip {
		if has(s) {
			return false
		}
	}
	if len(l.Require) == 0 || (l.Scheduled != "" && has(l.Scheduled)) || (l.Claimed != "" && has(l.Claimed)) {
		return true
	}
	for _, r := range l.Require {
		if has(r) {
			return true
		}
	}
	return false
}

// Status is the label for an agent state, if it has one.
func (l Labels) Status(state string) string {
	switch state {
	case "help":
		return l.Help
	case "review":
		return l.Review
	case "done":
		return l.Done
	}
	return ""
}

// Statuses are the three status labels.
func (l Labels) Statuses() []string {
	return []string{l.Help, l.Review, l.Done}
}

// Connection is one configured account on a service. Type names the adapter; the rest are its fields.
type Connection struct {
	Type   string         `yaml:"type"`
	Values adapter.Values `yaml:",inline"`
}

// Binding binds a project's role to a connection, with the role's own options.
type Binding struct {
	Connection string         `yaml:"connection"`
	Values     adapter.Values `yaml:",inline"`
}

// Project is one checkout the supervisor watches.
type Project struct {
	Name string `yaml:"name"`
	Root string `yaml:"root"`
	// Agent is the agent connection, when not the default.
	Agent string   `yaml:"agent,omitempty"`
	Repo  *Binding `yaml:"repo"`
	// Tracker is where the project's issues are, when it has a tracker of its own; a project fed by one of the
	// config's sources has none.
	Tracker *Binding `yaml:"tracker,omitempty"`
	Labels  *Labels  `yaml:"labels,omitempty"`
	Branch  string   `yaml:"branch,omitempty"`
	Git     *Git     `yaml:"git,omitempty"`
	// BaseBranch is where new branches start and PRs go; empty asks the code host.
	BaseBranch string            `yaml:"baseBranch,omitempty"`
	Worktree   Worktree          `yaml:"worktree,omitempty"`
	Sweep      Sweep             `yaml:"sweep,omitempty"`
	Triage     Triage            `yaml:"triage,omitempty"`
	Jobs       []Job             `yaml:"jobs,omitempty"`
	Env        map[string]string `yaml:"env,omitempty"`
	Disabled   bool              `yaml:"disabled,omitempty"`
}

// Git is how the supervisor and its agents use git in a project's checkout.
type Git struct {
	// Remote is the remote branches are fetched from and pushed to; empty is origin.
	Remote string `yaml:"remote,omitempty"`
	// Sign keeps commit signing as the checkout has it. Off, the supervisor turns it off for its agents and jobs,
	// so commits never wait on a signer.
	Sign bool `yaml:"sign,omitempty"`
}

// RemoteName is the remote to use.
func (g Git) RemoteName() string {
	if g.Remote != "" {
		return g.Remote
	}
	return "origin"
}

// Worktree is how an issue's worktree is set up.
type Worktree struct {
	// Copy are files copied from the main checkout into the worktree on every setup (.env).
	Copy []string `yaml:"copy,omitempty"`
	// Setup is a shell command run in the worktree when any of SetupWhen has changed since it last ran.
	Setup     string   `yaml:"setup,omitempty"`
	SetupWhen []string `yaml:"setupWhen,omitempty"`
}

// Sweep is the daily fold of the agents' correction notes into the project's rule files.
type Sweep struct {
	Disabled bool `yaml:"disabled,omitempty"`
	// RuleFiles are where rules go; empty uses the agent's own (CLAUDE.md, AGENTS.md).
	RuleFiles []string `yaml:"ruleFiles,omitempty"`
	// Guide says which file each kind of rule goes in, if the project has one.
	Guide string `yaml:"guide,omitempty"`
	// Land is how its commit reaches the main branch: push (the default) or pr.
	Land string `yaml:"land,omitempty"`
}

// Triage is the daily pass over the problems the agents report: it fixes what is plainly wrong in the project's
// own files and reports, or files, the rest.
type Triage struct {
	Disabled bool `yaml:"disabled,omitempty"`
	// OpenIssues opens (or updates) one issue per cause in the project's tracker; off, the triage only writes a
	// report for the owner.
	OpenIssues bool `yaml:"openIssues,omitempty"`
	// Land is how its fixes reach the main branch: push (the default) or pr.
	Land string `yaml:"land,omitempty"`
}

// How a sweep's or triage's commit reaches the main branch.
const (
	LandPush = "push"
	LandPR   = "pr"
)

// DefaultSettings are what a new config starts with and what a missing setting falls back to.
func DefaultSettings() Settings {
	return Settings{
		MaxTurns:         4,
		MaxOpen:          10,
		ReconcileMinutes: 15,
		Port:             8787,
		Labels: Labels{
			Claimed:   "agent-in-progress",
			Scheduled: "scheduled",
			Skip:      []string{"backlog", "human", "milestone"},
			Help:      "agent-needs-help",
			Review:    "ready-for-review",
			Done:      "might-be-done",
			Unrouted:  "needs-routing",
		},
		Branch:                "agent/issue-{key}",
		SweepAt:               "08:41",
		TriageAt:              "08:51",
		Jobs:                  true,
		UsageMinutes:          5,
		MaxAttempts:           3,
		ChecksDebounceSeconds: 20,
		NoChecksMinutes:       10,
	}
}

// New is an empty config with the defaults.
func New(path string) *Config {
	return &Config{Version: 1, Settings: DefaultSettings(), Connections: map[string]*Connection{}, Path: path}
}

// ConfigDir is ~/.config/open-supervisor, or under $XDG_CONFIG_HOME.
func ConfigDir() string {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, Name)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", Name)
}

// StateDir is where the supervisor keeps what it remembers: ~/.local/state/open-supervisor, or under
// $XDG_STATE_HOME.
func StateDir() string {
	if x := os.Getenv("OPEN_SUPERVISOR_STATE"); x != "" {
		return x
	}
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, Name)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", Name)
}

// DataDir is where the supervisor writes what it generates (the agent plugin, worktrees):
// ~/.local/share/open-supervisor, or under $XDG_DATA_HOME.
func DataDir() string {
	if x := os.Getenv("XDG_DATA_HOME"); x != "" {
		return filepath.Join(x, Name)
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "share", Name)
}

// DefaultPath is the config file to use: $OPEN_SUPERVISOR_CONFIG or the config dir's config.yaml.
func DefaultPath() string {
	if p := os.Getenv("OPEN_SUPERVISOR_CONFIG"); p != "" {
		return p
	}
	return filepath.Join(ConfigDir(), "config.yaml")
}

// ErrNoConfig is a config file that doesn't exist yet.
var ErrNoConfig = errors.New("no config yet")

// Load reads and checks the config at path.
func Load(path string) (*Config, error) {
	c, err := Read(path)
	if err != nil {
		return nil, err
	}
	return c, c.Validate()
}

// Read reads the config at path with the defaults filled in, but doesn't validate it: for changing a config
// a step at a time, where it may be incomplete in between.
func Read(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w at %s: run `%s init`", ErrNoConfig, path, Name)
	}
	if err != nil {
		return nil, err
	}
	return Parse(path, data)
}

// Parse reads a config from its YAML with the defaults filled in, without validating it.
func Parse(path string, data []byte) (*Config, error) {
	c := New(path)
	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.Path = path
	c.fill()
	return c, nil
}

// fill puts the defaults in for anything left out.
func (c *Config) fill() {
	d := DefaultSettings()
	s := &c.Settings
	if s.MaxTurns <= 0 {
		s.MaxTurns = d.MaxTurns
	}
	if s.MaxOpen <= 0 {
		s.MaxOpen = d.MaxOpen
	}
	if s.ReconcileMinutes <= 0 {
		s.ReconcileMinutes = d.ReconcileMinutes
	}
	if s.Port <= 0 {
		s.Port = d.Port
	}
	if s.Branch == "" {
		s.Branch = d.Branch
	}
	if s.UsageMinutes <= 0 {
		s.UsageMinutes = d.UsageMinutes
	}
	if s.MaxAttempts <= 0 {
		s.MaxAttempts = d.MaxAttempts
	}
	if s.ChecksDebounceSeconds <= 0 {
		s.ChecksDebounceSeconds = d.ChecksDebounceSeconds
	}
	if s.NoChecksMinutes <= 0 {
		s.NoChecksMinutes = d.NoChecksMinutes
	}
	s.Labels = s.Labels.Merge(d.Labels)
	if c.Connections == nil {
		c.Connections = map[string]*Connection{}
	}
	for _, conn := range c.Connections {
		if conn.Values == nil {
			conn.Values = adapter.Values{}
		}
	}
	for _, p := range c.Projects {
		for _, b := range []*Binding{p.Repo, p.Tracker} {
			if b != nil && b.Values == nil {
				b.Values = adapter.Values{}
			}
		}
	}
	for _, s := range c.SourceList {
		if s.Tracker != nil && s.Tracker.Values == nil {
			s.Tracker.Values = adapter.Values{}
		}
	}
}

var nameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)

// Validate checks the config hangs together: every connection has a known type, every binding names a
// connection that can fill the role.
func (c *Config) Validate() error {
	var errs []string
	for name, conn := range c.Connections {
		if !nameRe.MatchString(name) {
			errs = append(errs, fmt.Sprintf("connection %q: names are letters, digits, dot, dash and underscore", name))
		}
		spec, ok := adapter.Lookup(conn.Type)
		if !ok {
			errs = append(errs, fmt.Sprintf("connection %s: unknown type %q", name, conn.Type))
			continue
		}
		if err := spec.Validate(conn.Values); err != nil {
			errs = append(errs, fmt.Sprintf("connection %s: %v", name, err))
		}
	}
	role := func(where, name string, r adapter.Role) {
		if name == "" {
			return
		}
		conn, ok := c.Connections[name]
		if !ok {
			errs = append(errs, fmt.Sprintf("%s: no connection named %q", where, name))
			return
		}
		if spec, ok := adapter.Lookup(conn.Type); ok && !spec.Has(r) {
			errs = append(errs, fmt.Sprintf("%s: %s (%s) can't be a %s", where, name, conn.Type, r))
		}
	}
	role("settings.agent", c.Settings.Agent, adapter.RoleAgent)
	role("settings.tunnel", c.Settings.Tunnel, adapter.RoleTunnel)
	for _, t := range c.Settings.Telemetry {
		role("settings.telemetry", t, adapter.RoleTelemetry)
	}
	names := map[string]bool{}
	for i, p := range c.Projects {
		where := fmt.Sprintf("project %d", i+1)
		if p.Name != "" {
			where = "project " + p.Name
		}
		if !nameRe.MatchString(p.Name) {
			errs = append(errs, where+": needs a name of letters, digits, dot, dash and underscore")
		}
		if names[p.Name] {
			errs = append(errs, where+": named twice")
		}
		names[p.Name] = true
		if p.Root == "" {
			errs = append(errs, where+": needs a root (its checkout)")
		}
		if p.Repo == nil || p.Repo.Connection == "" {
			errs = append(errs, where+": needs a repo connection")
		} else {
			role(where+" repo", p.Repo.Connection, adapter.RoleRepo)
		}
		if p.Tracker != nil {
			if p.Tracker.Connection == "" {
				errs = append(errs, where+": needs a tracker connection")
			} else {
				role(where+" tracker", p.Tracker.Connection, adapter.RoleTracker)
			}
		}
		agent := p.Agent
		if agent == "" {
			agent = c.Settings.Agent
		}
		if agent == "" {
			errs = append(errs, where+": no agent (set settings.agent)")
		}
		role(where+" agent", agent, adapter.RoleAgent)
		for _, l := range []struct{ what, land string }{{"sweep", p.Sweep.Land}, {"triage", p.Triage.Land}} {
			if l.land != "" && l.land != LandPush && l.land != LandPR {
				errs = append(errs, fmt.Sprintf("%s: %s.land is push or pr, not %q", where, l.what, l.land))
			}
		}
		for _, j := range p.Jobs {
			for _, e := range j.Validate() {
				errs = append(errs, where+": "+e)
			}
		}
	}
	c.validateSources(role, &errs)
	if len(errs) > 0 {
		sort.Strings(errs)
		return fmt.Errorf("%s:\n  %s", c.Path, strings.Join(errs, "\n  "))
	}
	return nil
}

// Save writes the config, keeping a backup of the last one.
func (c *Config) Save() error {
	if err := os.MkdirAll(filepath.Dir(c.Path), 0o755); err != nil {
		return err
	}
	data, err := yaml.Marshal(c)
	if err != nil {
		return err
	}
	header := []byte("# open-supervisor config. Edit it here, with `open-supervisor init`, or from the TUI (press `,`).\n# Secrets are kept out of this file: see `keyring:` and `env:` references.\n")
	if old, err := os.ReadFile(c.Path); err == nil {
		_ = os.WriteFile(c.Path+".bak", old, 0o600)
	}
	tmp := c.Path + ".tmp"
	if err := os.WriteFile(tmp, append(header, data...), 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.Path)
}

// Project finds a project by name.
func (c *Config) Project(name string) *Project {
	for _, p := range c.Projects {
		if p.Name == name {
			return p
		}
	}
	return nil
}

// AgentFor is the agent connection a project uses.
func (c *Config) AgentFor(p *Project) string {
	if p.Agent != "" {
		return p.Agent
	}
	return c.Settings.Agent
}

// GitFor is how a project uses git: its own git settings, else the config's.
func (c *Config) GitFor(p *Project) Git {
	if p.Git == nil {
		return c.Settings.Git
	}
	g := *p.Git
	if g.Remote == "" {
		g.Remote = c.Settings.Git.Remote
	}
	return g
}

// LabelsFor are a project's labels: its source's, when a source feeds it.
func (c *Config) LabelsFor(p *Project) Labels {
	if p.Tracker == nil {
		if s := c.SourceFor(p); s != nil {
			return c.SourceLabels(s)
		}
	}
	if p.Labels == nil {
		return c.Settings.Labels
	}
	return p.Labels.Merge(c.Settings.Labels)
}

// BranchFor is a project's branch template.
func (c *Config) BranchFor(p *Project) string {
	if p.Branch != "" {
		return p.Branch
	}
	return c.Settings.Branch
}

// ConnectionNames lists the connections that can fill a role, by name.
func (c *Config) ConnectionNames(r adapter.Role) []string {
	var out []string
	for name, conn := range c.Connections {
		if spec, ok := adapter.Lookup(conn.Type); ok && spec.Has(r) {
			out = append(out, name)
		}
	}
	sort.Strings(out)
	return out
}

// ExpandHome turns a leading ~/ into the home directory.
func ExpandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}

// ShortenHome turns the home directory back into ~, for paths written to the config.
func ShortenHome(p string) string {
	home, _ := os.UserHomeDir()
	if home != "" && (p == home || strings.HasPrefix(p, home+string(filepath.Separator))) {
		return "~" + strings.TrimPrefix(p, home)
	}
	return p
}
