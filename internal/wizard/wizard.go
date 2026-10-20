// Package wizard is the onboarding: it asks, a step at a time, for the agent, the code host, the issue tracker,
// telemetry, webhooks and the projects, tests each connection as it goes, and saves the config. Run again over
// an existing config, it adds to it. The questions are the adapters' own fields (see internal/forms), so a new
// adapter shows up here with nothing to change.
package wizard

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/forms"
)

// ErrAborted is the person leaving the wizard before the end; nothing is saved.
var ErrAborted = errors.New("setup cancelled; nothing was saved")

var (
	title = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("212"))
	ok    = lipgloss.NewStyle().Foreground(lipgloss.Color("42"))
	bad   = lipgloss.NewStyle().Foreground(lipgloss.Color("203"))
	dim   = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
)

// Tools are the programs the wizard looks for, and why.
var Tools = []struct{ Name, Why string }{
	{"git", "worktrees and branches (required)"},
	{"claude", "the Claude Code agent"},
	{"codex", "the Codex agent"},
	{"gh", "GitHub's CLI (optional: sign in with `gh auth login`)"},
	{"ngrok", "a tunnel for webhooks"},
	{"cloudflared", "a tunnel for webhooks"},
}

// Found reports which of the tools are on PATH.
func Found() map[string]string {
	out := map[string]string{}
	for _, t := range Tools {
		if p, err := exec.LookPath(t.Name); err == nil {
			out[t.Name] = p
		}
	}
	return out
}

type wizard struct {
	cfg   *config.Config
	isNew bool
}

// ChooseSetup asks, on a first run, how to set up: the wizard ("") or a coding agent found on PATH (its type),
// which looks around and asks in its own words. With no agent installed it is the wizard without asking.
func ChooseSetup() (string, error) {
	var opts []huh.Option[string]
	for _, s := range adapter.ForRole(adapter.RoleAgent) {
		bin := s.Defaults()["binary"]
		if bin == "" {
			continue
		}
		if _, err := exec.LookPath(bin); err == nil {
			opts = append(opts, huh.NewOption("with "+s.Title+": it looks at your checkouts and asks about the rest", s.Type))
		}
	}
	if len(opts) == 0 {
		return "", nil
	}
	opts = append(opts, huh.NewOption("with the wizard, a step at a time", ""))
	how := opts[0].Value
	err := run(huh.NewForm(huh.NewGroup(huh.NewSelect[string]().Title("No config yet. How would you like to set up?").Options(opts...).Value(&how))))
	return how, err
}

func run(f *huh.Form) error {
	err := f.WithTheme(forms.Theme()).Run()
	if errors.Is(err, huh.ErrUserAborted) {
		return ErrAborted
	}
	return err
}

func groups(gs ...[]*huh.Group) []*huh.Group {
	var out []*huh.Group
	for _, g := range gs {
		out = append(out, g...)
	}
	return out
}

// Run walks through setup and saves the config at path. existing is nil on a first run.
func Run(path string, existing *config.Config) (*config.Config, error) {
	w := &wizard{}
	if existing == nil {
		w.cfg = config.New(path)
		w.isNew = true
	} else {
		w.cfg = forms.Clone(existing)
		w.cfg.Path = path
	}
	if err := w.welcome(); err != nil {
		return nil, err
	}
	steps := []func() error{w.agent, w.codeHost, w.tracker, w.telemetry, w.webhooks, w.projects, w.settings}
	if !w.isNew {
		steps = nil
		var pick []string
		if err := run(huh.NewForm(huh.NewGroup(huh.NewMultiSelect[string]().Title("What would you like to add or change?").Options(
			huh.NewOption("a project", "projects").Selected(true),
			huh.NewOption("a coding agent", "agent"),
			huh.NewOption("a code host", "repo"),
			huh.NewOption("an issue tracker", "tracker"),
			huh.NewOption("a tracker shared by several projects (routes)", "sources"),
			huh.NewOption("telemetry", "telemetry"),
			huh.NewOption("webhooks (tunnel)", "webhooks"),
			huh.NewOption("limits and labels", "settings"),
		).Value(&pick)))); err != nil {
			return nil, err
		}
		byName := map[string]func() error{"agent": w.agent, "repo": w.codeHost, "tracker": w.tracker, "telemetry": w.telemetry, "webhooks": w.webhooks, "projects": w.projects, "sources": w.sources, "settings": w.settings}
		for _, name := range []string{"agent", "repo", "tracker", "telemetry", "webhooks", "projects", "sources", "settings"} {
			for _, p := range pick {
				if p == name {
					steps = append(steps, byName[name])
				}
			}
		}
	}
	for _, step := range steps {
		if err := step(); err != nil {
			return nil, err
		}
	}
	return w.finish()
}

func (w *wizard) welcome() error {
	found := Found()
	var b strings.Builder
	for _, t := range Tools {
		if p, ok := found[t.Name]; ok {
			fmt.Fprintf(&b, "✓ %-12s %s\n", t.Name, dim.Render(p))
		} else {
			fmt.Fprintf(&b, "· %-12s %s\n", t.Name, dim.Render("not found: "+t.Why))
		}
	}
	intro := "open-supervisor claims issues from your tracker and works each in its own coding-agent session, one turn at a time.\n\n" +
		"This sets up the services it works through and the projects it watches. Every answer can be changed later, here or by pressing , (comma) in the TUI.\n\n"
	if !w.isNew {
		intro = "Adding to " + w.cfg.Path + ".\n\n"
	}
	return run(huh.NewForm(huh.NewGroup(huh.NewNote().Title("Welcome to open-supervisor").Description(intro + b.String()).Next(true).NextLabel("Start"))))
}

// connection asks for one connection of a role: an existing one, or a new one, tested. It returns its name.
func (w *wizard) connection(role adapter.Role, intro string, allowNone bool) (string, error) {
	existing := w.cfg.ConnectionNames(role)
	choice := "new"
	if len(existing) > 0 || allowNone {
		opts := []huh.Option[string]{}
		for _, n := range existing {
			opts = append(opts, huh.NewOption("use "+n+" ("+w.cfg.Connections[n].Type+")", "use:"+n))
		}
		opts = append(opts, huh.NewOption("set up a new "+forms.RoleNoun(role), "new"))
		if allowNone {
			opts = append(opts, huh.NewOption("skip", "none"))
		}
		choice = opts[0].Value
		if err := run(huh.NewForm(huh.NewGroup(huh.NewSelect[string]().Title(intro).Options(opts...).Value(&choice)))); err != nil {
			return "", err
		}
	}
	if choice == "none" {
		return "", nil
	}
	if name, ok := strings.CutPrefix(choice, "use:"); ok {
		return name, nil
	}
	if len(adapter.ForRole(role)) == 0 {
		fmt.Println(bad.Render("No " + forms.RoleNoun(role) + " adapters are built in."))
		return "", nil
	}
	d := forms.NewConnectionDraft(w.cfg, "", role)
	if err := run(huh.NewForm(d.TypeGroups(w.cfg)...)); err != nil {
		return "", err
	}
	for {
		if err := run(huh.NewForm(d.FieldGroups(w.cfg)...)); err != nil {
			return "", err
		}
		name, err := d.Apply(w.cfg)
		if err != nil {
			fmt.Println(bad.Render("✗ " + err.Error()))
			if again, err2 := w.confirm("Try again?", true); err2 != nil || !again {
				return "", errors.Join(err, err2)
			}
			continue
		}
		fmt.Print(dim.Render("Testing "+name+"… "), "")
		who, err := forms.TestConnection(w.cfg, name)
		if err == nil {
			fmt.Println(ok.Render("✓ " + who))
			return name, nil
		}
		fmt.Println(bad.Render("✗ " + err.Error()))
		choice := "fix"
		if err := run(huh.NewForm(huh.NewGroup(huh.NewSelect[string]().Title(name+" didn't work").Options(
			huh.NewOption("change the settings", "fix"),
			huh.NewOption("keep it anyway (fix it later)", "keep"),
			huh.NewOption("drop it", "drop"),
		).Value(&choice)))); err != nil {
			return "", err
		}
		switch choice {
		case "keep":
			return name, nil
		case "drop":
			delete(w.cfg.Connections, name)
			return "", nil
		}
		d.Original = name
	}
}

func (w *wizard) confirm(q string, def bool) (bool, error) {
	v := def
	err := run(huh.NewForm(huh.NewGroup(huh.NewConfirm().Title(q).Value(&v))))
	return v, err
}

func (w *wizard) header(s string) { fmt.Println("\n" + title.Render(s)) }

func (w *wizard) agent() error {
	w.header("Coding agent")
	name, err := w.connection(adapter.RoleAgent, "Which coding agent runs the turns?", false)
	if err != nil {
		return err
	}
	if name != "" && (w.cfg.Settings.Agent == "" || w.isNew) {
		w.cfg.Settings.Agent = name
	} else if name != "" && name != w.cfg.Settings.Agent {
		if yes, err := w.confirm("Make "+name+" the default agent?", false); err != nil {
			return err
		} else if yes {
			w.cfg.Settings.Agent = name
		}
	}
	return nil
}

func (w *wizard) codeHost() error {
	w.header("Code host")
	_, err := w.connection(adapter.RoleRepo, "Where is the code (and its PRs)?", false)
	return err
}

func (w *wizard) tracker() error {
	w.header("Issue tracker")
	// A code host that can also hold issues is offered first by connection(), as "use <name>".
	_, err := w.connection(adapter.RoleTracker, "Where are the issues?", false)
	return err
}

func (w *wizard) telemetry() error {
	w.header("Telemetry")
	if len(adapter.ForRole(adapter.RoleTelemetry)) == 0 {
		return nil
	}
	for {
		name, err := w.connection(adapter.RoleTelemetry, "Send per-turn metrics (cost, tokens, duration) somewhere?", true)
		if err != nil || name == "" {
			return err
		}
		if !contains(w.cfg.Settings.Telemetry, name) {
			w.cfg.Settings.Telemetry = append(w.cfg.Settings.Telemetry, name)
			sort.Strings(w.cfg.Settings.Telemetry)
		}
		if more, err := w.confirm("Add another telemetry destination?", false); err != nil || !more {
			return err
		}
	}
}

func contains(xs []string, x string) bool {
	for _, y := range xs {
		if y == x {
			return true
		}
	}
	return false
}

func (w *wizard) webhooks() error {
	w.header("Webhooks")
	mode := "poll"
	if w.cfg.Settings.Tunnel != "" {
		mode = "tunnel"
	}
	minutes := fmt.Sprint(w.cfg.Settings.ReconcileMinutes)
	if err := run(huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Title("How should events reach the supervisor?").Description(
			"Webhooks make it react in seconds; polling (the reconcile) always runs too, so nothing is missed either way.").Options(
			huh.NewOption("poll only (no setup)", "poll"),
			huh.NewOption("webhooks through a tunnel", "tunnel"),
		).Value(&mode),
		huh.NewInput().Title("Poll every (minutes)").Value(&minutes).Validate(func(s string) error {
			var n int
			if _, err := fmt.Sscan(s, &n); err != nil || n <= 0 {
				return errors.New("a whole number of minutes")
			}
			return nil
		}),
	))); err != nil {
		return err
	}
	fmt.Sscan(minutes, &w.cfg.Settings.ReconcileMinutes)
	if mode == "poll" {
		w.cfg.Settings.Tunnel = ""
		return nil
	}
	name, err := w.connection(adapter.RoleTunnel, "Which tunnel?", true)
	if err != nil {
		return err
	}
	w.cfg.Settings.Tunnel = name
	return nil
}

func (w *wizard) projects() error {
	w.header("Projects")
	cwd, _ := os.Getwd()
	for {
		if len(w.cfg.ConnectionNames(adapter.RoleRepo)) == 0 || len(w.cfg.ConnectionNames(adapter.RoleTracker)) == 0 {
			fmt.Println(bad.Render("A project needs a code host and an issue tracker; set those up first."))
			return nil
		}
		dir := ""
		if forms.IsGitCheckout(cwd) && w.cfg.Project(forms.DefaultProjectName(w.cfg, cwd)) == nil && !w.hasRoot(cwd) {
			dir = cwd
		}
		d := forms.NewProjectDraft(w.cfg, nil, dir)
		if err := run(huh.NewForm(d.BasicGroups(w.cfg)...)); err != nil {
			return err
		}
		if bg := d.BindingGroups(w.cfg); len(bg) > 0 {
			if err := run(huh.NewForm(bg...)); err != nil {
				return err
			}
		}
		if err := run(huh.NewForm(d.ExtraGroups(w.cfg)...)); err != nil {
			return err
		}
		p, err := d.Apply(w.cfg)
		if err != nil {
			fmt.Println(bad.Render("✗ " + err.Error()))
		} else {
			fmt.Println(ok.Render("✓ added " + p.Name + " (" + p.Root + ")"))
			if sd := d.Follow(w.cfg, p); sd != nil {
				if err := w.source(sd); err != nil {
					return err
				}
			}
		}
		more, err := w.confirm("Add another project?", false)
		if err != nil || !more {
			return err
		}
	}
}

// source asks for a shared tracker's routes, shows where the open issues would go, and asks again until
// they look right.
func (w *wizard) source(sd *forms.SourceDraft) error {
	steps := sd.Steps(w.cfg)
	for {
		for _, step := range steps {
			if f := step(); f != nil {
				if err := run(f); err != nil {
					return err
				}
			}
		}
		if !sd.Accepted {
			return nil
		}
		if _, err := sd.Routes(); err != nil {
			fmt.Println(bad.Render("✗ " + err.Error()))
		} else if keep, err := w.confirm("Use these routes?", true); err != nil {
			return err
		} else if keep {
			break
		}
		steps = sd.RouteSteps(w.cfg)
	}
	note, err := sd.Apply(w.cfg)
	if err != nil {
		fmt.Println(bad.Render("✗ " + err.Error()))
		return nil
	}
	fmt.Println(ok.Render("✓ " + strings.TrimSpace(sd.Name) + " feeds " + strings.Join(sd.Projects, ", ")))
	if note != "" {
		fmt.Println(dim.Render(note))
	}
	return nil
}

// sources adds a shared tracker, or changes one.
func (w *wizard) sources() error {
	w.header("Shared trackers")
	if len(w.cfg.ConnectionNames(adapter.RoleTracker)) == 0 || len(w.cfg.Projects) == 0 {
		fmt.Println(bad.Render("A shared tracker needs an issue tracker and projects to feed; set those up first."))
		return nil
	}
	pick := ""
	if len(w.cfg.SourceList) > 0 {
		var opts []huh.Option[string]
		for _, s := range w.cfg.SourceList {
			opts = append(opts, huh.NewOption("change "+s.Name, s.Name))
		}
		opts = append(opts, huh.NewOption("share another tracker", ""))
		pick = opts[0].Value
		if err := run(huh.NewForm(huh.NewGroup(huh.NewSelect[string]().Title("Which?").Options(opts...).Value(&pick)))); err != nil {
			return err
		}
	}
	var src *config.Source
	for _, s := range w.cfg.SourceList {
		if s.Name == pick {
			src = s
		}
	}
	return w.source(forms.NewSourceDraft(w.cfg, src))
}

func (w *wizard) hasRoot(dir string) bool {
	for _, p := range w.cfg.Projects {
		if config.ExpandHome(p.Root) == dir {
			return true
		}
	}
	return false
}

func (w *wizard) settings() error {
	w.header("Limits and labels")
	d := forms.NewSettingsDraft(w.cfg)
	gs := d.LimitGroups()
	gs = append(gs, huh.NewGroup(huh.NewNote().Title("Everything else").Description("Reconcile interval, branch names, the daily sweep, jobs and more are editable later: press , in the TUI, or edit "+w.cfg.Path+".")))
	if err := run(huh.NewForm(gs...)); err != nil {
		return err
	}
	d.Apply(w.cfg)
	return nil
}

// Summary describes the config, for the last step.
func Summary(cfg *config.Config) string {
	var b strings.Builder
	names := make([]string, 0, len(cfg.Connections))
	for n := range cfg.Connections {
		names = append(names, n)
	}
	sort.Strings(names)
	b.WriteString("Connections:\n")
	for _, n := range names {
		fmt.Fprintf(&b, "  %s (%s)\n", n, cfg.Connections[n].Type)
	}
	if len(names) == 0 {
		b.WriteString("  none\n")
	}
	fmt.Fprintf(&b, "Agent: %s\n", orNone(cfg.Settings.Agent))
	tunnel := "none (poll every " + fmt.Sprint(cfg.Settings.ReconcileMinutes) + " min)"
	if cfg.Settings.Tunnel != "" {
		tunnel = cfg.Settings.Tunnel
	}
	fmt.Fprintf(&b, "Webhooks: %s\n", tunnel)
	if len(cfg.Settings.Telemetry) > 0 {
		fmt.Fprintf(&b, "Telemetry: %s\n", strings.Join(cfg.Settings.Telemetry, ", "))
	}
	b.WriteString("Projects:\n")
	for _, p := range cfg.Projects {
		issues := "none"
		if p.Tracker != nil {
			issues = p.Tracker.Connection
		} else if src := cfg.SourceFor(p); src != nil {
			issues = "from source " + src.Name
		}
		fmt.Fprintf(&b, "  %s  %s  (code %s, issues %s)\n", p.Name, p.Root, p.Repo.Connection, issues)
	}
	if len(cfg.Projects) == 0 {
		b.WriteString("  none yet\n")
	}
	for _, src := range cfg.SourceList {
		fmt.Fprintf(&b, "Source %s:\n", src.Name)
		for _, r := range src.Routes {
			fmt.Fprintf(&b, "  → %s when %s\n", r.Project, r.When.String())
		}
	}
	fmt.Fprintf(&b, "Turns at once: %d; issues per project: %d", cfg.Settings.MaxTurns, cfg.Settings.MaxOpen)
	return b.String()
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}

func (w *wizard) finish() (*config.Config, error) {
	save := true
	if err := run(huh.NewForm(huh.NewGroup(
		huh.NewNote().Title("Summary").Description(Summary(w.cfg)),
		huh.NewConfirm().Title("Save to "+w.cfg.Path+"?").Value(&save),
	))); err != nil {
		return nil, err
	}
	if !save {
		return nil, ErrAborted
	}
	if err := w.cfg.Validate(); err != nil {
		fmt.Println(bad.Render("The config has problems; it is saved anyway so you can fix them with , (comma) in the TUI or by hand:\n" + err.Error()))
	}
	if err := w.cfg.Save(); err != nil {
		return nil, err
	}
	fmt.Println(ok.Render("✓ saved " + w.cfg.Path))
	fmt.Println("\nNext:\n  " + config.Name + " doctor    check every connection and list what each project would claim\n  " + config.Name + "           start the supervisor")
	return w.cfg, nil
}
