package tui

import (
	"fmt"
	"os"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/x/ansi"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/engine"
	"github.com/alsiola/open-supervisor/internal/forms"
)

// editor is the config editor: the general settings, the connections and the projects, edited on a copy of the
// running config and applied (saved, then reloaded into the supervisor) with s.
type editor struct {
	sup           *engine.Supervisor
	cfg           *config.Config
	dirty         bool
	tab           int
	sel           [4]int
	width, height int

	form   *huh.Form
	steps  []func() *huh.Form
	finish func() error

	note    string
	errText string
	confirm string
	onYes   func() tea.Cmd
}

var tabs = []string{"General", "Connections", "Projects", "Sources"}

type testedMsg struct {
	name, who string
	err       error
}

func newEditor(sup *engine.Supervisor, width, height int) *editor {
	return &editor{sup: sup, cfg: forms.Clone(sup.Config()), width: width, height: height}
}

func (e *editor) init() tea.Cmd { return nil }

func (e *editor) resize(msg tea.WindowSizeMsg) tea.Cmd {
	e.width, e.height = msg.Width, msg.Height
	if e.form != nil {
		_, cmd := e.form.Update(msg)
		return cmd
	}
	return nil
}

// start runs a sequence of forms, then finish.
func (e *editor) start(steps []func() *huh.Form, finish func() error) tea.Cmd {
	e.steps, e.finish = steps, finish
	e.note, e.errText = "", ""
	return e.next()
}

func (e *editor) next() tea.Cmd {
	for len(e.steps) > 0 {
		// Popped before it runs: a step may put more steps in front of the rest.
		step := e.steps[0]
		e.steps = e.steps[1:]
		f := step()
		if f == nil {
			continue
		}
		f = f.WithTheme(forms.Theme()).WithKeyMap(forms.KeyMap()).WithShowHelp(true).WithWidth(min(e.width-4, 100))
		e.form = f
		cmd := f.Init()
		_, c2 := f.Update(tea.WindowSizeMsg{Width: min(e.width-4, 100), Height: max(10, e.height-6)})
		return tea.Batch(cmd, c2)
	}
	e.form = nil
	if e.finish != nil {
		if err := e.finish(); err != nil {
			e.errText = err.Error()
		} else {
			e.dirty = true
		}
		e.finish = nil
	}
	return nil
}

func formOf(groups []*huh.Group) *huh.Form {
	if len(groups) == 0 {
		return nil
	}
	return huh.NewForm(groups...)
}

// update handles a message; done reports the editor closing.
func (e *editor) update(msg tea.Msg) (tea.Cmd, bool) {
	if t, ok := msg.(testedMsg); ok {
		if t.err != nil {
			e.errText = fmt.Sprintf("%s: %v", t.name, t.err)
		} else {
			e.note = fmt.Sprintf("%s: ✓ %s", t.name, t.who)
		}
		return nil, false
	}
	if e.form != nil {
		m, cmd := e.form.Update(msg)
		e.form = m.(*huh.Form)
		switch e.form.State {
		case huh.StateAborted:
			e.form, e.steps, e.finish = nil, nil, nil
			e.note = "cancelled"
			return nil, false
		case huh.StateCompleted:
			return tea.Batch(cmd, e.next()), false
		}
		return cmd, false
	}
	k, ok := msg.(tea.KeyMsg)
	if !ok {
		return nil, false
	}
	s := k.String()
	if e.confirm != "" {
		e.confirm = ""
		if s == "y" || s == "Y" {
			return e.onYes(), false
		}
		return nil, false
	}
	e.note, e.errText = "", ""
	switch s {
	case "ctrl+c":
		return tea.Quit, false
	case "esc", "q":
		if e.dirty {
			e.confirm = "Discard the unsaved changes? (y/n)"
			e.onYes = func() tea.Cmd { e.dirty = false; return func() tea.Msg { return tea.KeyMsg{Type: tea.KeyEsc} } }
			return nil, false
		}
		return nil, true
	case "tab", "right":
		e.tab = (e.tab + 1) % len(tabs)
	case "shift+tab", "left":
		e.tab = (e.tab + len(tabs) - 1) % len(tabs)
	case "1", "2", "3", "4":
		e.tab = int(s[0] - '1')
	case "up":
		e.sel[e.tab] = max(0, e.sel[e.tab]-1)
	case "down":
		e.sel[e.tab] = min(max(0, e.count()-1), e.sel[e.tab]+1)
	case "s":
		return e.save(), false
	case "enter":
		return e.edit(), false
	case "a":
		return e.add(), false
	case "d":
		e.remove()
	case "t":
		return e.test(), false
	case "x":
		e.toggle()
	}
	return nil, false
}

func (e *editor) connectionNames() []string {
	var names []string
	for n := range e.cfg.Connections {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}

func (e *editor) count() int {
	switch e.tab {
	case 1:
		return len(e.cfg.Connections)
	case 2:
		return len(e.cfg.Projects)
	case 3:
		return len(e.cfg.SourceList)
	}
	return 1
}

func (e *editor) selectedSource() *config.Source {
	if i := e.sel[3]; i < len(e.cfg.SourceList) {
		return e.cfg.SourceList[i]
	}
	return nil
}

func (e *editor) selectedConnection() string {
	names := e.connectionNames()
	if i := e.sel[1]; i < len(names) {
		return names[i]
	}
	return ""
}

func (e *editor) selectedProject() *config.Project {
	if i := e.sel[2]; i < len(e.cfg.Projects) {
		return e.cfg.Projects[i]
	}
	return nil
}

func (e *editor) edit() tea.Cmd {
	switch e.tab {
	case 0:
		d := forms.NewSettingsDraft(e.cfg)
		return e.start([]func() *huh.Form{func() *huh.Form { return formOf(d.Groups(e.cfg)) }}, func() error { d.Apply(e.cfg); return nil })
	case 1:
		if name := e.selectedConnection(); name != "" {
			return e.editConnection(forms.NewConnectionDraft(e.cfg, name, ""))
		}
	case 2:
		if p := e.selectedProject(); p != nil {
			return e.editProject(forms.NewProjectDraft(e.cfg, p, ""))
		}
	case 3:
		if src := e.selectedSource(); src != nil {
			return e.editSource(forms.NewSourceDraft(e.cfg, src))
		}
	}
	return nil
}

func (e *editor) add() tea.Cmd {
	switch e.tab {
	case 1:
		return e.editConnection(forms.NewConnectionDraft(e.cfg, "", ""))
	case 2:
		cwd, _ := os.Getwd()
		for _, p := range e.cfg.Projects {
			if config.ExpandHome(p.Root) == cwd {
				cwd = ""
			}
		}
		return e.editProject(forms.NewProjectDraft(e.cfg, nil, cwd))
	case 3:
		if len(e.cfg.ConnectionNames(adapter.RoleTracker)) == 0 || len(e.cfg.Projects) == 0 {
			e.errText = "a source needs an issue tracker connection and a project to feed"
			return nil
		}
		return e.editSource(forms.NewSourceDraft(e.cfg, nil))
	}
	return nil
}

func (e *editor) editSource(d *forms.SourceDraft) tea.Cmd {
	return e.start(d.Steps(e.cfg), func() error {
		note, err := d.Apply(e.cfg)
		if err == nil {
			e.note = "saved " + strings.TrimSpace(d.Name) + " to the draft; s applies"
			if note != "" {
				e.note += " (" + note + ")"
			}
		}
		return err
	})
}

func (e *editor) editConnection(d *forms.ConnectionDraft) tea.Cmd {
	return e.start([]func() *huh.Form{
		func() *huh.Form { return formOf(d.TypeGroups(e.cfg)) },
		func() *huh.Form { return formOf(d.FieldGroups(e.cfg)) },
	}, func() error {
		name, err := d.Apply(e.cfg)
		if err == nil {
			e.note = "saved " + name + " to the draft; t tests it, s applies"
		}
		return err
	})
}

func (e *editor) editProject(d *forms.ProjectDraft) tea.Cmd {
	var share *forms.SourceDraft
	return e.start([]func() *huh.Form{
		func() *huh.Form { return formOf(d.BasicGroups(e.cfg)) },
		func() *huh.Form { return formOf(d.BindingGroups(e.cfg)) },
		func() *huh.Form { return formOf(d.ExtraGroups(e.cfg)) },
		func() *huh.Form {
			// Written into a copy first, to see whether it needs routes or shares another project's issues.
			tmp := forms.Clone(e.cfg)
			if p, err := d.Apply(tmp); err == nil {
				if share = d.Follow(tmp, p); share != nil {
					e.steps = append(share.Steps(tmp), e.steps...)
				}
			}
			return nil
		},
	}, func() error {
		// Both on a copy first, so a source that can't be saved leaves the project as it was.
		next := forms.Clone(e.cfg)
		p, err := d.Apply(next)
		if err != nil {
			return err
		}
		note := "saved " + p.Name + " to the draft; s applies"
		if share != nil {
			more, err := share.Apply(next)
			if err != nil {
				return err
			}
			if share.Accepted {
				note = fmt.Sprintf("saved %s, fed by %s, to the draft; s applies", p.Name, strings.TrimSpace(share.Name))
			}
			if more != "" {
				note += " (" + more + ")"
			}
		}
		*e.cfg = *next
		e.note = note
		return nil
	})
}

func (e *editor) remove() {
	switch e.tab {
	case 1:
		name := e.selectedConnection()
		if name == "" {
			return
		}
		if uses := forms.Uses(e.cfg, name); len(uses) > 0 {
			e.errText = fmt.Sprintf("%s is used by %s", name, strings.Join(uses, ", "))
			return
		}
		e.confirm = fmt.Sprintf("Delete the connection %s? (y/n)", name)
		e.onYes = func() tea.Cmd {
			delete(e.cfg.Connections, name)
			e.dirty = true
			e.sel[1] = max(0, min(e.sel[1], len(e.cfg.Connections)-1))
			return nil
		}
	case 2:
		p := e.selectedProject()
		if p == nil {
			return
		}
		e.confirm = fmt.Sprintf("Remove the project %s from the config? Its state stays on disk. (y/n)", p.Name)
		if forms.RoutedTo(e.cfg, p.Name) {
			e.confirm = fmt.Sprintf("Remove the project %s, and the routes to it, from the config? Its state stays on disk. (y/n)", p.Name)
		}
		e.onYes = func() tea.Cmd {
			forms.RemoveProject(e.cfg, p.Name)
			e.dirty = true
			e.sel[2] = max(0, min(e.sel[2], len(e.cfg.Projects)-1))
			return nil
		}
	case 3:
		src := e.selectedSource()
		if src == nil {
			return
		}
		e.confirm = fmt.Sprintf("Remove the source %s? Each project it feeds reads the tracker on its own again. (y/n)", src.Name)
		e.onYes = func() tea.Cmd {
			forms.RemoveSource(e.cfg, src.Name)
			e.dirty = true
			e.sel[3] = max(0, min(e.sel[3], len(e.cfg.SourceList)-1))
			return nil
		}
	}
}

func (e *editor) toggle() {
	if e.tab != 2 {
		return
	}
	if p := e.selectedProject(); p != nil {
		p.Disabled = !p.Disabled
		e.dirty = true
	}
}

func (e *editor) test() tea.Cmd {
	if e.tab != 1 {
		return nil
	}
	name := e.selectedConnection()
	if name == "" {
		return nil
	}
	cfg := forms.Clone(e.cfg)
	e.note = "testing " + name + "…"
	return func() tea.Msg {
		who, err := forms.TestConnection(cfg, name)
		return testedMsg{name, who, err}
	}
}

func (e *editor) save() tea.Cmd {
	if err := e.cfg.Validate(); err != nil {
		e.errText = err.Error()
		return nil
	}
	if err := e.cfg.Save(); err != nil {
		e.errText = "saving: " + err.Error()
		return nil
	}
	if err := e.sup.Reload(forms.Clone(e.cfg)); err != nil {
		e.errText = "saved, but the supervisor couldn't use it: " + err.Error()
		return nil
	}
	e.dirty = false
	e.note = "saved " + e.cfg.Path + " and applied"
	return nil
}

func (e *editor) view() string {
	var out []string
	var ts []string
	for i, t := range tabs {
		label := fmt.Sprintf("%d %s", i+1, t)
		if i == e.tab {
			ts = append(ts, tabOnS.Render(label))
		} else {
			ts = append(ts, tabOffS.Render(label))
		}
	}
	title := " " + bold("Config") + "  " + dim(e.cfg.Path)
	if e.dirty {
		title += "  " + yellowS.Render("unsaved changes")
	}
	out = append(out, title, " "+strings.Join(ts, " "), dim(strings.Repeat("─", e.width)))
	if e.form != nil {
		for _, l := range strings.Split(e.form.View(), "\n") {
			out = append(out, "  "+l)
		}
		out = append(out, "", dim("  enter next   shift+tab back   esc cancel"))
		return e.paint(out)
	}
	switch e.tab {
	case 0:
		out = append(out, e.generalLines()...)
	case 1:
		out = append(out, e.connectionLines()...)
	case 2:
		out = append(out, e.projectLines()...)
	case 3:
		out = append(out, e.sourceLines()...)
	}
	keys := map[int]string{
		0: " ⏎ edit   tab switch   s save & apply   esc close",
		1: " ↑↓ select   ⏎ edit   a add   d delete   t test   tab switch   s save & apply   esc close",
		2: " ↑↓ select   ⏎ edit   a add   d remove   x disable/enable   tab switch   s save & apply   esc close",
		3: " ↑↓ select   ⏎ edit   a add   d remove   tab switch   s save & apply   esc close",
	}[e.tab]
	status := dim(keys)
	switch {
	case e.confirm != "":
		status = " " + yellowS.Render(e.confirm)
	case e.errText != "":
		for _, l := range strings.Split(e.errText, "\n") {
			out = append(out, " "+redS.Render(l))
		}
	case e.note != "":
		status = " " + greenS.Render(e.note) + "   " + dim(keys)
	}
	return e.paintWithFooter(out, status)
}

func (e *editor) paint(lines []string) string {
	m := &model{width: e.width, height: e.height}
	return m.paint(lines)
}

func (e *editor) paintWithFooter(lines []string, footer string) string {
	if len(lines) > e.height-1 {
		lines = lines[:e.height-1]
	}
	for len(lines) < e.height-1 {
		lines = append(lines, "")
	}
	return e.paint(append(lines, footer))
}

func (e *editor) generalLines() []string {
	s := e.cfg.Settings
	f := func(k, v string) string { return "  " + dim(pad(k, 26)) + v }
	or := func(v, none string) string {
		if v == "" {
			return dim(none)
		}
		return v
	}
	tele := strings.Join(s.Telemetry, ", ")
	return []string{
		"",
		f("turns at once", fmt.Sprint(s.MaxTurns)),
		f("issues per project", fmt.Sprint(s.MaxOpen)),
		f("reconcile every", fmt.Sprintf("%d min", s.ReconcileMinutes)),
		f("retries of a failed turn", fmt.Sprint(s.MaxAttempts)),
		f("webhook port", fmt.Sprint(s.Port)),
		f("default agent", or(s.Agent, "none")),
		f("tunnel", or(s.Tunnel, "none: poll only")),
		f("telemetry", or(tele, "none")),
		f("labels", fmt.Sprintf("%s, %s, %s, %s, %s; skip %s", s.Labels.Claimed, s.Labels.Scheduled, s.Labels.Help, s.Labels.Review, s.Labels.Done, strings.Join(s.Labels.Skip, ", "))),
		f("branch", s.Branch),
		f("daily sweep", or(s.SweepAt, "off")),
		f("jobs", map[bool]string{true: "on", false: "off"}[s.Jobs]),
	}
}

func (e *editor) connectionLines() []string {
	out := []string{"", dim(fmt.Sprintf("  %s %s %s %s", pad("name", 18), pad("type", 14), pad("roles", 30), "used by"))}
	names := e.connectionNames()
	for i, n := range names {
		c := e.cfg.Connections[n]
		roles := ""
		if spec, ok := adapter.Lookup(c.Type); ok {
			var rs []string
			for _, r := range spec.Roles {
				rs = append(rs, string(r))
			}
			roles = strings.Join(rs, ", ")
		}
		uses := strings.Join(forms.Uses(e.cfg, n), ", ")
		if uses == "" {
			uses = dim("nothing")
		}
		line := fmt.Sprintf("  %s %s %s %s", pad(n, 18), pad(c.Type, 14), pad(roles, 30), uses)
		if i == e.sel[1] {
			line = invS.Render(pad(ansi.Strip(line), e.width))
		}
		out = append(out, line)
	}
	if len(names) == 0 {
		out = append(out, dim("  no connections: press a to add one"))
	}
	return out
}

func (e *editor) projectLines() []string {
	out := []string{"", dim(fmt.Sprintf("  %s %s %s %s %s", pad("name", 16), pad("checkout", 36), pad("code", 14), pad("issues", 14), "agent"))}
	for i, p := range e.cfg.Projects {
		agent := p.Agent
		if agent == "" {
			agent = dim("default")
		}
		repo, tracker := "", ""
		if p.Repo != nil {
			repo = p.Repo.Connection
		}
		if p.Tracker != nil {
			tracker = p.Tracker.Connection
		} else if src := e.cfg.SourceFor(p); src != nil {
			tracker = "via " + src.Name
		}
		name := p.Name
		if p.Disabled {
			name += " (off)"
		}
		line := fmt.Sprintf("  %s %s %s %s %s", pad(name, 16), pad(p.Root, 36), pad(repo, 14), pad(tracker, 14), agent)
		if i == e.sel[2] {
			line = invS.Render(pad(ansi.Strip(line), e.width))
		}
		out = append(out, line)
	}
	if len(e.cfg.Projects) == 0 {
		out = append(out, dim("  no projects: press a to add one"))
	}
	return out
}

func (e *editor) sourceLines() []string {
	out := []string{"", dim("  A source is one tracker feeding several projects; its routes send each issue to one of them.")}
	for i, src := range e.cfg.SourceList {
		tracker := ""
		if src.Tracker != nil {
			tracker = src.Tracker.Connection
		}
		unrouted := "unrouted: left alone"
		if src.Unrouted == config.UnroutedLabel {
			unrouted = "unrouted: labelled " + e.cfg.Settings.Labels.Unrouted
		}
		line := fmt.Sprintf("  %s %s %s", pad(src.Name, 16), pad(tracker, 14), dim(unrouted))
		if i == e.sel[3] {
			line = invS.Render(pad(ansi.Strip(line), e.width))
		}
		out = append(out, "", line)
		for n, r := range src.Routes {
			out = append(out, fmt.Sprintf("      %d. %s %s", n+1, pad(r.Project, 14), dim("when "+r.When.String())))
		}
	}
	if len(e.cfg.SourceList) == 0 {
		out = append(out, "", dim("  no sources: every project reads its own tracker. Press a to share one between projects."))
	}
	return out
}
