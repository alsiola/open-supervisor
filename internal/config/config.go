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
	// SweepAt is the local time (HH:MM) of the daily corrections sweep; empty turns it off.
	SweepAt string `yaml:"sweepAt"`
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
	Help      string   `yaml:"help,omitempty"`
	Review    string   `yaml:"review,omitempty"`
	Done      string   `yaml:"done,omitempty"`
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
		Skip:      l.Skip,
	}
	if out.Skip == nil {
		out.Skip = base.Skip
	}
	return out
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
	Agent   string   `yaml:"agent,omitempty"`
	Repo    *Binding `yaml:"repo"`
	Tracker *Binding `yaml:"tracker"`
	Labels  *Labels  `yaml:"labels,omitempty"`
	Branch  string   `yaml:"branch,omitempty"`
	// BaseBranch is where new branches start and PRs go; empty asks the code host.
	BaseBranch string            `yaml:"baseBranch,omitempty"`
	Worktree   Worktree          `yaml:"worktree,omitempty"`
	Sweep      Sweep             `yaml:"sweep,omitempty"`
	Jobs       []Job             `yaml:"jobs,omitempty"`
	Env        map[string]string `yaml:"env,omitempty"`
	Disabled   bool              `yaml:"disabled,omitempty"`
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
}

// Job is a command the supervisor runs in a project's main checkout. With EveryMinutes, it runs on that
// interval and the supervisor waits for it (up to TimeoutMinutes). With At (UTC times, HH:MM), it starts
// detached at each time and reports through Status, a folder where it keeps running.json
// ({pid, slot, startedAt, log}) while it runs and last.json ({slot, outcome, finishedAt}) when it ends; an
// outcome of "busy" is tried again in ten minutes. A run missed by more than six hours waits for the next
// time. Key is the TUI key that runs it now. On runs it on webhooks as well (see JobTrigger).
type Job struct {
	Name           string      `yaml:"name" json:"name"`
	Command        []string    `yaml:"command" json:"command"`
	Key            string      `yaml:"key,omitempty" json:"key,omitempty"`
	EveryMinutes   int         `yaml:"everyMinutes,omitempty" json:"everyMinutes,omitempty"`
	TimeoutMinutes int         `yaml:"timeoutMinutes,omitempty" json:"timeoutMinutes,omitempty"`
	At             []string    `yaml:"at,omitempty" json:"at,omitempty"`
	Status         string      `yaml:"status,omitempty" json:"status,omitempty"`
	On             *JobTrigger `yaml:"on,omitempty" json:"on,omitempty"`
}

// JobTrigger runs a job on webhooks, which arrive through the tunnel. Events are the code host's or tracker's
// own event names, taken through the project's hook: "push", or "release.published" for one action of an
// event. Branches limits events that carry a git ref (a push) to those branches, globs allowed. Webhook takes
// POSTs from anywhere else at /jobs/<project>/<job>/<token>; the supervisor logs the URL once the tunnel is up.
// The job is told about the delivery through OPEN_SUPERVISOR_EVENT, _EVENT_ACTION, _EVENT_REF and
// _EVENT_FILE (the body). A delivery that arrives while an interval job is running runs it again once it ends.
type JobTrigger struct {
	Events   []string `yaml:"events,omitempty" json:"events,omitempty"`
	Branches []string `yaml:"branches,omitempty" json:"branches,omitempty"`
	Webhook  bool     `yaml:"webhook,omitempty" json:"webhook,omitempty"`
}

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
		},
		Branch:                "agent/issue-{key}",
		SweepAt:               "08:41",
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
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("%w at %s: run `%s init`", ErrNoConfig, path, Name)
	}
	if err != nil {
		return nil, err
	}
	c := New(path)
	if err := yaml.Unmarshal(data, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.Path = path
	c.fill()
	return c, c.Validate()
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
		if p.Tracker == nil || p.Tracker.Connection == "" {
			errs = append(errs, where+": needs a tracker connection")
		} else {
			role(where+" tracker", p.Tracker.Connection, adapter.RoleTracker)
		}
		agent := p.Agent
		if agent == "" {
			agent = c.Settings.Agent
		}
		if agent == "" {
			errs = append(errs, where+": no agent (set settings.agent)")
		}
		role(where+" agent", agent, adapter.RoleAgent)
		for _, j := range p.Jobs {
			if j.Name == "" || len(j.Command) == 0 {
				errs = append(errs, where+": every job needs a name and a command")
			}
			if j.On != nil && len(j.On.Events) == 0 && !j.On.Webhook {
				errs = append(errs, fmt.Sprintf("%s: job %s: on needs events or webhook", where, j.Name))
			}
		}
	}
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

// LabelsFor are a project's labels.
func (c *Config) LabelsFor(p *Project) Labels {
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
