package forms

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/config"
)

// The drafts below are what the wizard and the config editor both edit: the plain values behind a form, with
// the groups to ask for them and Apply to write them back into a config.

// ---- connections ----------------------------------------------------------------------------------------

// ConnectionDraft is a connection being added or edited.
type ConnectionDraft struct {
	// Original is the connection's name before editing, "" for a new one.
	Original string
	Name     string
	Type     string
	Role     adapter.Role
	Fields   *FieldSet
}

// NewConnectionDraft starts a draft. For a new connection, role narrows the types offered (empty for any).
func NewConnectionDraft(cfg *config.Config, name string, role adapter.Role) *ConnectionDraft {
	d := &ConnectionDraft{Original: name, Name: name, Role: role}
	if c, ok := cfg.Connections[name]; ok {
		d.Type = c.Type
	}
	return d
}

// Types are the adapter types the draft may take.
func (d *ConnectionDraft) Types() []adapter.Spec {
	if d.Role != "" {
		return adapter.ForRole(d.Role)
	}
	return adapter.All()
}

// TypeGroups ask for the type and the name (a new connection only; an existing one keeps its type).
func (d *ConnectionDraft) TypeGroups(cfg *config.Config) []*huh.Group {
	if d.Original != "" {
		return []*huh.Group{huh.NewGroup(huh.NewInput().Title("Name").Value(&d.Name).Validate(d.nameValidator(cfg)))}
	}
	opts := []huh.Option[string]{}
	for _, s := range d.Types() {
		roles := []string{}
		for _, r := range s.Roles {
			roles = append(roles, string(r))
		}
		opts = append(opts, huh.NewOption(fmt.Sprintf("%s  (%s)", s.Title, strings.Join(roles, ", ")), s.Type))
	}
	if d.Type == "" && len(opts) > 0 {
		d.Type = opts[0].Value
	}
	title := "Service"
	if d.Role != "" {
		title = "Which " + RoleNoun(d.Role) + "?"
	}
	return []*huh.Group{
		huh.NewGroup(huh.NewSelect[string]().Title(title).Options(opts...).Value(&d.Type)),
		huh.NewGroup(huh.NewInput().Title("Name for this connection").
			DescriptionFunc(func() string { return "How the config refers to it. Default: " + d.defaultName(cfg) }, &d.Type).
			PlaceholderFunc(func() string { return d.defaultName(cfg) }, &d.Type).
			Value(&d.Name).Validate(d.nameValidator(cfg))),
	}
}

func (d *ConnectionDraft) defaultName(cfg *config.Config) string {
	return UniqueName(cfg, d.Type)
}

// UniqueName is base, or base-2, base-3… when a connection already has the name.
func UniqueName(cfg *config.Config, base string) string {
	if base == "" {
		base = "connection"
	}
	name := base
	for i := 2; ; i++ {
		if _, taken := cfg.Connections[name]; !taken {
			return name
		}
		name = fmt.Sprintf("%s-%d", base, i)
	}
}

func (d *ConnectionDraft) nameValidator(cfg *config.Config) func(string) error {
	return func(s string) error {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil // the default is used
		}
		if !validName(s) {
			return errors.New("letters, digits, dot, dash and underscore only")
		}
		if _, taken := cfg.Connections[s]; taken && s != d.Original {
			return errors.New("a connection already has that name")
		}
		return nil
	}
}

func validName(s string) bool {
	for i, r := range s {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || (i > 0 && (r == '.' || r == '-' || r == '_'))
		if !ok {
			return false
		}
	}
	return s != ""
}

// Spec is the draft's adapter type.
func (d *ConnectionDraft) Spec() (adapter.Spec, bool) { return adapter.Lookup(d.Type) }

// FieldGroups ask for the connection's fields. Call after the type is known.
func (d *ConnectionDraft) FieldGroups(cfg *config.Config) []*huh.Group {
	spec, ok := d.Spec()
	if !ok {
		return nil
	}
	if strings.TrimSpace(d.Name) == "" {
		d.Name = d.defaultName(cfg)
	}
	var values adapter.Values
	if c, ok := cfg.Connections[d.Original]; ok && c.Type == d.Type {
		values = c.Values
	}
	if d.Fields == nil {
		d.Fields = New(d.Name, spec.Fields, values)
	}
	d.Fields.Name = d.Name
	if len(spec.Fields) == 0 {
		return []*huh.Group{huh.NewGroup(huh.NewNote().Title(spec.Title).Description(spec.Description + "\n\nNothing to set."))}
	}
	return d.Fields.Groups(spec.Title + ": " + spec.Description)
}

// Apply writes the connection into cfg, renaming its uses when the name changed. It returns the name used.
func (d *ConnectionDraft) Apply(cfg *config.Config) (string, error) {
	spec, ok := d.Spec()
	if !ok {
		return "", fmt.Errorf("unknown type %q", d.Type)
	}
	name := strings.TrimSpace(d.Name)
	if name == "" {
		name = d.defaultName(cfg)
	}
	if d.Fields == nil {
		d.Fields = New(name, spec.Fields, nil)
	}
	d.Fields.Name = name
	values, err := d.Fields.Apply()
	if err != nil {
		return "", err
	}
	if err := spec.Validate(values); err != nil {
		return "", err
	}
	if d.Original != "" && d.Original != name {
		delete(cfg.Connections, d.Original)
		RenameConnection(cfg, d.Original, name)
	}
	if cfg.Connections == nil {
		cfg.Connections = map[string]*config.Connection{}
	}
	cfg.Connections[name] = &config.Connection{Type: d.Type, Values: values}
	return name, nil
}

// RenameConnection points every use of a connection at its new name.
func RenameConnection(cfg *config.Config, from, to string) {
	s := &cfg.Settings
	if s.Agent == from {
		s.Agent = to
	}
	if s.Tunnel == from {
		s.Tunnel = to
	}
	for i, t := range s.Telemetry {
		if t == from {
			s.Telemetry[i] = to
		}
	}
	for _, p := range cfg.Projects {
		if p.Agent == from {
			p.Agent = to
		}
		if p.Repo != nil && p.Repo.Connection == from {
			p.Repo.Connection = to
		}
		if p.Tracker != nil && p.Tracker.Connection == from {
			p.Tracker.Connection = to
		}
	}
}

// Uses lists what uses a connection, empty when it can be deleted.
func Uses(cfg *config.Config, name string) []string {
	var out []string
	s := cfg.Settings
	if s.Agent == name {
		out = append(out, "the default agent")
	}
	if s.Tunnel == name {
		out = append(out, "the tunnel")
	}
	for _, t := range s.Telemetry {
		if t == name {
			out = append(out, "telemetry")
		}
	}
	for _, p := range cfg.Projects {
		if p.Agent == name {
			out = append(out, p.Name+"'s agent")
		}
		if p.Repo != nil && p.Repo.Connection == name {
			out = append(out, p.Name+"'s repo")
		}
		if p.Tracker != nil && p.Tracker.Connection == name {
			out = append(out, p.Name+"'s tracker")
		}
	}
	return out
}

// RoleNoun names a role for people.
func RoleNoun(r adapter.Role) string {
	switch r {
	case adapter.RoleAgent:
		return "coding agent"
	case adapter.RoleRepo:
		return "code host"
	case adapter.RoleTracker:
		return "issue tracker"
	case adapter.RoleTelemetry:
		return "telemetry service"
	case adapter.RoleTunnel:
		return "tunnel"
	}
	return string(r)
}

// ---- projects -------------------------------------------------------------------------------------------

// ProjectDraft is a project being added or edited.
type ProjectDraft struct {
	Original  string
	Name      string
	Root      string
	Agent     string // "" for the default
	Repo      string
	Tracker   string
	Branch    string
	Base      string
	Copy      string // comma-separated
	Setup     string
	SetupWhen string // comma-separated
	NoSweep   bool
	Disabled  bool

	RepoFields    *FieldSet
	TrackerFields *FieldSet
	project       *config.Project
}

// NewProjectDraft starts a draft from an existing project (nil for a new one). A new project starts in dir
// when it is a git checkout.
func NewProjectDraft(cfg *config.Config, p *config.Project, dir string) *ProjectDraft {
	if p == nil {
		d := &ProjectDraft{}
		if dir != "" && IsGitCheckout(dir) {
			d.Root = config.ShortenHome(dir)
			d.Name = DefaultProjectName(cfg, dir)
		}
		if names := cfg.ConnectionNames(adapter.RoleRepo); len(names) > 0 {
			d.Repo = names[0]
		}
		if names := cfg.ConnectionNames(adapter.RoleTracker); len(names) > 0 {
			d.Tracker = names[0]
			for _, n := range names {
				if n == d.Repo {
					d.Tracker = n
				}
			}
		}
		d.suggestWorktree()
		return d
	}
	d := &ProjectDraft{
		Original: p.Name, Name: p.Name, Root: p.Root, Agent: p.Agent, Branch: p.Branch, Base: p.BaseBranch,
		Copy: strings.Join(p.Worktree.Copy, ", "), Setup: p.Worktree.Setup, SetupWhen: strings.Join(p.Worktree.SetupWhen, ", "),
		NoSweep: p.Sweep.Disabled, Disabled: p.Disabled, project: p,
	}
	if p.Repo != nil {
		d.Repo = p.Repo.Connection
	}
	if p.Tracker != nil {
		d.Tracker = p.Tracker.Connection
	}
	return d
}

// IsGitCheckout reports whether dir is the top of a git checkout.
func IsGitCheckout(dir string) bool {
	if dir == "" {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, ".git"))
	return err == nil
}

// DefaultProjectName is the directory's base name, made unique among the config's projects.
func DefaultProjectName(cfg *config.Config, dir string) string {
	base := strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, filepath.Base(filepath.Clean(dir)))
	base = strings.Trim(base, "-.")
	if base == "" {
		base = "project"
	}
	name := base
	for i := 2; cfg.Project(name) != nil; i++ {
		name = fmt.Sprintf("%s-%d", base, i)
	}
	return name
}

// Lockfiles map a lockfile to the command that installs from it, most specific first.
var Lockfiles = []struct{ File, Setup string }{
	{"pnpm-lock.yaml", "pnpm install --frozen-lockfile"},
	{"bun.lock", "bun install"},
	{"bun.lockb", "bun install"},
	{"yarn.lock", "yarn install --frozen-lockfile"},
	{"package-lock.json", "npm ci"},
	{"uv.lock", "uv sync"},
	{"go.sum", "go mod download"},
	{"Cargo.lock", ""},
}

// SuggestSetup is the worktree setup for a checkout, from its lockfile: the command and the file it watches.
func SuggestSetup(root string) (setup, when string) {
	for _, l := range Lockfiles {
		if _, err := os.Stat(filepath.Join(root, l.File)); err == nil {
			if l.Setup == "" {
				return "", ""
			}
			return l.Setup, l.File
		}
	}
	return "", ""
}

// SuggestCopy are the files to copy into each worktree: .env when the checkout has one.
func SuggestCopy(root string) []string {
	if _, err := os.Stat(filepath.Join(root, ".env")); err == nil {
		return []string{".env"}
	}
	return nil
}

func (d *ProjectDraft) suggestWorktree() {
	root := config.ExpandHome(d.Root)
	if root == "" {
		return
	}
	if d.Setup == "" {
		d.Setup, d.SetupWhen = SuggestSetup(root)
	}
	if d.Copy == "" {
		d.Copy = strings.Join(SuggestCopy(root), ", ")
	}
}

// BasicGroups ask for the project's name, checkout and connections.
func (d *ProjectDraft) BasicGroups(cfg *config.Config) []*huh.Group {
	agents := []huh.Option[string]{huh.NewOption("default ("+orNone(cfg.Settings.Agent)+")", "")}
	for _, n := range cfg.ConnectionNames(adapter.RoleAgent) {
		agents = append(agents, huh.NewOption(n, n))
	}
	conns := func(r adapter.Role) []huh.Option[string] {
		var out []huh.Option[string]
		for _, n := range cfg.ConnectionNames(r) {
			out = append(out, huh.NewOption(fmt.Sprintf("%s (%s)", n, cfg.Connections[n].Type), n))
		}
		if len(out) == 0 {
			out = append(out, huh.NewOption("(add a "+RoleNoun(r)+" connection first)", ""))
		}
		return out
	}
	return []*huh.Group{
		huh.NewGroup(
			huh.NewInput().Title("Checkout *").Description("The project's main git checkout. ~ is fine.").Value(&d.Root).Validate(func(s string) error {
				if strings.TrimSpace(s) == "" {
					return errors.New("the project's checkout is required")
				}
				if !IsGitCheckout(config.ExpandHome(strings.TrimSpace(s))) {
					return errors.New("not a git checkout")
				}
				return nil
			}),
			huh.NewInput().Title("Name *").Description("Short; used in the TUI, the log and webhook URLs.").
				PlaceholderFunc(func() string {
					if d.Root == "" {
						return ""
					}
					return DefaultProjectName(cfg, config.ExpandHome(d.Root))
				}, &d.Root).
				Value(&d.Name).Validate(func(s string) error {
				s = strings.TrimSpace(s)
				if s == "" {
					return nil
				}
				if !validName(s) {
					return errors.New("letters, digits, dot, dash and underscore only")
				}
				if cfg.Project(s) != nil && s != d.Original {
					return errors.New("a project already has that name")
				}
				return nil
			}),
		).Title("Project"),
		huh.NewGroup(
			huh.NewSelect[string]().Title("Agent").Options(agents...).Value(&d.Agent),
			huh.NewSelect[string]().Title("Code host").Options(conns(adapter.RoleRepo)...).Value(&d.Repo),
			huh.NewSelect[string]().Title("Issue tracker").Options(conns(adapter.RoleTracker)...).Value(&d.Tracker),
		).Title("Connections"),
	}
}

func orNone(s string) string {
	if s == "" {
		return "none set"
	}
	return s
}

// root is the checkout as an absolute path.
func (d *ProjectDraft) root() string { return config.ExpandHome(strings.TrimSpace(d.Root)) }

// BindingGroups ask for the repo's and tracker's options, pre-filled from the checkout's remote. Call after
// BasicGroups are answered.
func (d *ProjectDraft) BindingGroups(cfg *config.Config) []*huh.Group {
	if strings.TrimSpace(d.Name) == "" && d.Root != "" {
		d.Name = DefaultProjectName(cfg, d.root())
	}
	d.suggestWorktree()
	var groups []*huh.Group
	if c, ok := cfg.Connections[d.Repo]; ok {
		if spec, ok := adapter.Lookup(c.Type); ok {
			values := d.bindingValues(d.project, "repo")
			if len(values) == 0 {
				values = DetectBinding(cfg, d.Repo, d.root())
			}
			d.RepoFields = New(d.Repo, spec.Binding[adapter.RoleRepo], values)
			groups = append(groups, d.RepoFields.Groups("Repository on "+d.Repo)...)
		}
	}
	if c, ok := cfg.Connections[d.Tracker]; ok {
		if spec, ok := adapter.Lookup(c.Type); ok && len(spec.Binding[adapter.RoleTracker]) > 0 {
			values := d.bindingValues(d.project, "tracker")
			if len(values) == 0 && d.Tracker == d.Repo && d.RepoFields != nil {
				// The same service as the code host: start from the repo it detected.
				values = d.RepoFields.Current()
			}
			d.TrackerFields = New(d.Tracker, spec.Binding[adapter.RoleTracker], values)
			groups = append(groups, d.TrackerFields.Groups("Issues on "+d.Tracker)...)
		} else {
			d.TrackerFields = nil
		}
	}
	return groups
}

func (d *ProjectDraft) bindingValues(p *config.Project, which string) adapter.Values {
	if p == nil {
		return nil
	}
	var b *config.Binding
	if which == "repo" {
		b = p.Repo
	} else {
		b = p.Tracker
	}
	if b == nil || (which == "repo" && b.Connection != d.Repo) || (which == "tracker" && b.Connection != d.Tracker) {
		return nil
	}
	return b.Values
}

// DetectBinding fills a repo binding from the checkout's origin, when the connection is the remote's service.
func DetectBinding(cfg *config.Config, conn, root string) adapter.Values {
	remote := GitRemote(root)
	if remote == "" {
		return nil
	}
	c, err := openQuiet(cfg, conn)
	if err != nil {
		return nil
	}
	rp, ok := c.(adapter.RepoProvider)
	if !ok {
		return nil
	}
	v, _ := rp.DetectRepo(remote)
	return v
}

// GitRemote is the checkout's origin URL.
func GitRemote(root string) string {
	out, err := exec.Command("git", "-C", root, "remote", "get-url", "origin").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ExtraGroups ask for the branch template, worktree setup and sweep.
func (d *ProjectDraft) ExtraGroups(cfg *config.Config) []*huh.Group {
	return []*huh.Group{huh.NewGroup(
		huh.NewInput().Title("Branch").Description("{key} is the issue key. Blank uses the default: "+cfg.Settings.Branch).Value(&d.Branch).Validate(func(s string) error {
			if s != "" && !strings.Contains(s, "{key}") {
				return errors.New("needs {key}")
			}
			return nil
		}),
		huh.NewInput().Title("Base branch").Description("Where branches start and PRs go. Blank asks the code host.").Value(&d.Base),
		huh.NewInput().Title("Copy into each worktree").Description("Comma-separated files from the checkout, e.g. .env").Value(&d.Copy),
		huh.NewInput().Title("Worktree setup command").Description("Run in a new worktree, and again when a watched file changes.").Value(&d.Setup),
		huh.NewInput().Title("Rerun setup when these change").Description("Comma-separated, e.g. the lockfile").Value(&d.SetupWhen),
		huh.NewConfirm().Title("Turn off the daily corrections sweep?").Value(&d.NoSweep),
	).Title("Worktrees and branches")}
}

// Apply writes the project into cfg.
func (d *ProjectDraft) Apply(cfg *config.Config) (*config.Project, error) {
	root := strings.TrimSpace(d.Root)
	if root == "" {
		return nil, errors.New("a project needs its checkout")
	}
	if !IsGitCheckout(config.ExpandHome(root)) {
		return nil, fmt.Errorf("%s isn't a git checkout", root)
	}
	name := strings.TrimSpace(d.Name)
	if name == "" {
		name = DefaultProjectName(cfg, config.ExpandHome(root))
	}
	if d.Repo == "" || d.Tracker == "" {
		return nil, errors.New("a project needs a code host and an issue tracker connection")
	}
	p := d.project
	if p == nil {
		p = &config.Project{}
	} else {
		p = CloneProject(p)
	}
	p.Name = name
	p.Root = config.ShortenHome(config.ExpandHome(root))
	p.Agent = d.Agent
	p.Branch = strings.TrimSpace(d.Branch)
	p.BaseBranch = strings.TrimSpace(d.Base)
	p.Worktree.Copy = splitList(d.Copy)
	p.Worktree.Setup = strings.TrimSpace(d.Setup)
	p.Worktree.SetupWhen = splitList(d.SetupWhen)
	p.Sweep.Disabled = d.NoSweep
	p.Disabled = d.Disabled
	repoValues := adapter.Values{}
	if d.RepoFields != nil {
		v, err := d.RepoFields.Apply()
		if err != nil {
			return nil, err
		}
		repoValues = v
	} else if p.Repo != nil && p.Repo.Connection == d.Repo {
		repoValues = p.Repo.Values
	}
	p.Repo = &config.Binding{Connection: d.Repo, Values: repoValues}
	trackerValues := adapter.Values{}
	if d.TrackerFields != nil {
		v, err := d.TrackerFields.Apply()
		if err != nil {
			return nil, err
		}
		trackerValues = v
	} else if p.Tracker != nil && p.Tracker.Connection == d.Tracker {
		trackerValues = p.Tracker.Values
	}
	// The same service as the code host reads the repo's binding; don't repeat it.
	if d.Tracker == d.Repo {
		for k, v := range trackerValues {
			if repoValues[k] == v {
				delete(trackerValues, k)
			}
		}
	}
	p.Tracker = &config.Binding{Connection: d.Tracker, Values: trackerValues}
	replaced := false
	for i, q := range cfg.Projects {
		if q.Name == d.Original && d.Original != "" {
			cfg.Projects[i] = p
			replaced = true
		}
	}
	if !replaced {
		cfg.Projects = append(cfg.Projects, p)
	}
	return p, nil
}

func splitList(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// ---- settings -------------------------------------------------------------------------------------------

// SettingsDraft is the general settings being edited.
type SettingsDraft struct {
	MaxTurns, MaxOpen, Reconcile, Port, MaxAttempts string
	Agent, Tunnel                                   string
	Telemetry                                       []string
	Claimed, Scheduled, Skip, Help, Review, Done    string
	Branch, SweepAt                                 string
	Jobs                                            bool
}

// NewSettingsDraft starts from the config's settings.
func NewSettingsDraft(cfg *config.Config) *SettingsDraft {
	s := cfg.Settings
	return &SettingsDraft{
		MaxTurns: strconv.Itoa(s.MaxTurns), MaxOpen: strconv.Itoa(s.MaxOpen), Reconcile: strconv.Itoa(s.ReconcileMinutes),
		Port: strconv.Itoa(s.Port), MaxAttempts: strconv.Itoa(s.MaxAttempts),
		Agent: s.Agent, Tunnel: s.Tunnel, Telemetry: append([]string(nil), s.Telemetry...),
		Claimed: s.Labels.Claimed, Scheduled: s.Labels.Scheduled, Skip: strings.Join(s.Labels.Skip, ", "),
		Help: s.Labels.Help, Review: s.Labels.Review, Done: s.Labels.Done,
		Branch: s.Branch, SweepAt: s.SweepAt, Jobs: s.Jobs,
	}
}

func positive(label string) func(string) error {
	return func(s string) error {
		if n, err := strconv.Atoi(strings.TrimSpace(s)); err != nil || n <= 0 {
			return fmt.Errorf("%s must be a whole number above 0", label)
		}
		return nil
	}
}

// LimitGroups ask for the limits and labels: what the wizard asks.
func (d *SettingsDraft) LimitGroups() []*huh.Group {
	return []*huh.Group{
		huh.NewGroup(
			huh.NewInput().Title("Turns at once").Description("Across every project. Idle sessions cost nothing; this is the only cap that matters.").Value(&d.MaxTurns).Validate(positive("Turns at once")),
			huh.NewInput().Title("Issues claimed at once, per project").Value(&d.MaxOpen).Validate(positive("Issues claimed")),
		).Title("Limits"),
		huh.NewGroup(
			huh.NewInput().Title("Claimed").Description("Put on an issue an agent has").Value(&d.Claimed),
			huh.NewInput().Title("Scheduled").Description("Held until a runAt time, or opened on a timer").Value(&d.Scheduled),
			huh.NewInput().Title("Leave alone").Description("Comma-separated: issues with any of these are never claimed").Value(&d.Skip),
			huh.NewInput().Title("Needs help").Value(&d.Help),
			huh.NewInput().Title("Ready for review").Value(&d.Review),
			huh.NewInput().Title("Might be done").Value(&d.Done),
		).Title("Labels").Description("Jira labels can't hold spaces."),
	}
}

// Groups ask for every general setting: what the config editor shows.
func (d *SettingsDraft) Groups(cfg *config.Config) []*huh.Group {
	agents := []huh.Option[string]{huh.NewOption("(none)", "")}
	for _, n := range cfg.ConnectionNames(adapter.RoleAgent) {
		agents = append(agents, huh.NewOption(n, n))
	}
	tunnels := []huh.Option[string]{huh.NewOption("none: poll only", "")}
	for _, n := range cfg.ConnectionNames(adapter.RoleTunnel) {
		tunnels = append(tunnels, huh.NewOption(n, n))
	}
	sinks := []huh.Option[string]{}
	for _, n := range cfg.ConnectionNames(adapter.RoleTelemetry) {
		sinks = append(sinks, huh.NewOption(n, n))
	}
	conns := []huh.Field{
		huh.NewSelect[string]().Title("Default agent").Options(agents...).Value(&d.Agent),
		huh.NewSelect[string]().Title("Tunnel for webhooks").Options(tunnels...).Value(&d.Tunnel),
	}
	if len(sinks) > 0 {
		conns = append(conns, huh.NewMultiSelect[string]().Title("Telemetry").Options(sinks...).Value(&d.Telemetry))
	}
	groups := []*huh.Group{
		huh.NewGroup(
			huh.NewInput().Title("Turns at once").Value(&d.MaxTurns).Validate(positive("Turns at once")),
			huh.NewInput().Title("Issues claimed at once, per project").Value(&d.MaxOpen).Validate(positive("Issues claimed")),
			huh.NewInput().Title("Reconcile every (minutes)").Value(&d.Reconcile).Validate(positive("Reconcile")),
			huh.NewInput().Title("Retries of a failed turn").Value(&d.MaxAttempts).Validate(positive("Retries")),
			huh.NewInput().Title("Webhook port").Description("Changes at the next start").Value(&d.Port).Validate(positive("Port")),
		).Title("Limits"),
		huh.NewGroup(conns...).Title("Connections"),
	}
	groups = append(groups, d.LimitGroups()[1])
	groups = append(groups, huh.NewGroup(
		huh.NewInput().Title("Branch").Description("{key} is the issue key").Value(&d.Branch).Validate(func(s string) error {
			if !strings.Contains(s, "{key}") {
				return errors.New("needs {key}")
			}
			return nil
		}),
		huh.NewInput().Title("Daily sweep at").Description("Local HH:MM; blank turns it off").Value(&d.SweepAt).Validate(func(s string) error {
			if s == "" {
				return nil
			}
			var h, m int
			if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil || h < 0 || h > 23 || m < 0 || m > 59 {
				return errors.New("HH:MM")
			}
			return nil
		}),
		huh.NewConfirm().Title("Run the projects' jobs?").Value(&d.Jobs),
	).Title("Branches, sweep and jobs"))
	return groups
}

// Apply writes the settings into cfg.
func (d *SettingsDraft) Apply(cfg *config.Config) {
	s := &cfg.Settings
	atoi := func(v string, keep int) int {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n > 0 {
			return n
		}
		return keep
	}
	s.MaxTurns = atoi(d.MaxTurns, s.MaxTurns)
	s.MaxOpen = atoi(d.MaxOpen, s.MaxOpen)
	s.ReconcileMinutes = atoi(d.Reconcile, s.ReconcileMinutes)
	s.Port = atoi(d.Port, s.Port)
	s.MaxAttempts = atoi(d.MaxAttempts, s.MaxAttempts)
	s.Agent = d.Agent
	s.Tunnel = d.Tunnel
	s.Telemetry = append([]string(nil), d.Telemetry...)
	sort.Strings(s.Telemetry)
	s.Labels = config.Labels{
		Claimed: strings.TrimSpace(d.Claimed), Scheduled: strings.TrimSpace(d.Scheduled), Skip: splitList(d.Skip),
		Help: strings.TrimSpace(d.Help), Review: strings.TrimSpace(d.Review), Done: strings.TrimSpace(d.Done),
	}.Merge(config.DefaultSettings().Labels)
	if strings.Contains(d.Branch, "{key}") {
		s.Branch = strings.TrimSpace(d.Branch)
	}
	s.SweepAt = strings.TrimSpace(d.SweepAt)
	s.Jobs = d.Jobs
}
