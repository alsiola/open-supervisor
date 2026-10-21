// Package tui is the terminal view over the supervisor: one row per issue across every project with the log
// underneath, a detail page per issue with its live transcript, and the config editor. Attaching hands the
// terminal to the agent's interactive session and takes it back when that exits.
package tui

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/engine"
	"github.com/alsiola/open-supervisor/internal/logx"
	"github.com/alsiola/open-supervisor/internal/store"
)

// Run shows the TUI until the person quits.
func Run(sup *engine.Supervisor) error {
	m := newModel(sup)
	_, err := tea.NewProgram(m, tea.WithAltScreen()).Run()
	return err
}

type view int

const (
	listView view = iota
	detailView
	editorView
)

// row is an issue (p and r) or a shell task (sh).
type row struct {
	p  *engine.Project
	r  *store.Record
	sh *engine.Shell
}

type input struct {
	prompt string
	ti     textinput.Model
	done   func(string) tea.Cmd
}

type model struct {
	sup           *engine.Supervisor
	width, height int
	view          view
	selected      int
	showLog       bool
	detail        struct {
		project, key string
		shell        int // the shell task's id, when the page is one
	}
	scroll      int
	focusT      bool // the transcript box has the scroll keys
	tTop        int  // first transcript line shown; -1 follows the end
	tMax, tPage int
	tCache      struct {
		key   string
		lines []string
	}
	input  *input
	picker *picker
	cwd    string // where open-supervisor was started: the first place a shell task can run
	editor *editor
	flash  string
	logs   <-chan struct{}
}

type changedMsg struct{}
type logMsg struct{}
type tickMsg struct{}
type attachedMsg struct {
	p   *engine.Project
	key string
	err error
}
type flashMsg string

func newModel(sup *engine.Supervisor) *model {
	cwd, _ := os.Getwd()
	return &model{sup: sup, showLog: true, tTop: -1, logs: logx.Default.Subscribe(), width: 120, height: 40, cwd: cwd}
}

func (m *model) waitChange() tea.Cmd {
	ch := m.sup.Changes()
	return func() tea.Msg {
		<-ch
		return changedMsg{}
	}
}

func (m *model) waitLog() tea.Cmd {
	ch := m.logs
	return func() tea.Msg {
		<-ch
		return logMsg{}
	}
}

func tick() tea.Cmd {
	return tea.Tick(time.Second, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m *model) Init() tea.Cmd {
	return tea.Batch(m.waitChange(), m.waitLog(), tick())
}

func (m *model) rows() []row {
	var out []row
	for _, p := range m.sup.Projects() {
		for _, r := range p.Store.All() {
			if r.State != store.Finished {
				out = append(out, row{p: p, r: r})
			}
		}
	}
	for _, sh := range m.sup.Shells() {
		out = append(out, row{sh: sh})
	}
	return out
}

func (m *model) current() *row {
	if m.view == detailView {
		if m.detail.shell > 0 {
			if sh := m.sup.Shell(m.detail.shell); sh != nil {
				return &row{sh: sh}
			}
			return nil
		}
		p := m.sup.Project(m.detail.project)
		if p == nil {
			return nil
		}
		r := p.Store.Get(m.detail.key)
		if r == nil {
			return nil
		}
		return &row{p: p, r: r}
	}
	rows := m.rows()
	if m.selected >= 0 && m.selected < len(rows) {
		return &rows[m.selected]
	}
	return nil
}

// reserved are the keys the TUI uses itself; a job can't take one.
var reserved = map[string]bool{"!": true, "a": true, "c": true, "d": true, "g": true, "k": true, "l": true, "m": true, "q": true, "r": true, "s": true, "t": true, "R": true, ",": true}

type jobKey struct {
	p   *engine.Project
	job config.Job
}

// jobKeys maps each job's key to its jobs, the selected row's project first.
func (m *model) jobKeys() map[string][]jobKey {
	out := map[string][]jobKey{}
	projects := m.sup.Projects()
	if cur := m.current(); cur != nil && cur.p != nil {
		ordered := []*engine.Project{cur.p}
		for _, p := range projects {
			if p != cur.p {
				ordered = append(ordered, p)
			}
		}
		projects = ordered
	}
	for _, p := range projects {
		for _, j := range p.Jobs.Specs {
			if j.Key == "" || reserved[j.Key] {
				continue
			}
			out[j.Key] = append(out[j.Key], jobKey{p, j})
		}
	}
	return out
}

func (m *model) ask(prompt string, done func(string) tea.Cmd) tea.Cmd {
	ti := textinput.New()
	ti.Prompt = ""
	ti.CharLimit = 4000
	ti.Width = max(10, m.width-len(prompt)-4)
	in := &input{prompt: prompt, ti: ti, done: done}
	m.input = in
	return in.ti.Focus()
}

func (m *model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		if m.editor != nil {
			return m, m.editor.resize(msg)
		}
		return m, nil
	case changedMsg:
		return m, m.waitChange()
	case logMsg:
		return m, m.waitLog()
	case tickMsg:
		return m, tick()
	case flashMsg:
		m.flash = string(msg)
		return m, nil
	case attachedMsg:
		msg.p.Detach(msg.key)
		if msg.err != nil {
			m.flash = "attach: " + msg.err.Error()
		}
		return m, nil
	}
	if m.view == editorView && m.editor != nil {
		cmd, done := m.editor.update(msg)
		if done {
			m.editor = nil
			m.view = listView
		}
		return m, cmd
	}
	if key, ok := msg.(tea.KeyMsg); ok {
		return m, m.key(key)
	}
	if m.input != nil {
		var cmd tea.Cmd
		m.input.ti, cmd = m.input.ti.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *model) key(k tea.KeyMsg) tea.Cmd {
	if m.picker != nil {
		return m.pickerKey(k)
	}
	if m.input != nil {
		switch k.Type {
		case tea.KeyEsc:
			m.input = nil
			return nil
		case tea.KeyEnter:
			in := m.input
			m.input = nil
			return in.done(strings.TrimSpace(in.ti.Value()))
		case tea.KeyCtrlC:
			return tea.Quit
		}
		var cmd tea.Cmd
		m.input.ti, cmd = m.input.ti.Update(k)
		return cmd
	}
	m.flash = ""
	s := k.String()
	if s == "ctrl+c" {
		return tea.Quit
	}
	cur := m.current()
	if m.view == detailView && m.focusT {
		top := m.tTop
		if top < 0 {
			top = m.tMax
		}
		to := func(n int) {
			if n >= m.tMax {
				m.tTop = -1
			} else {
				m.tTop = max(0, n)
			}
		}
		switch s {
		case "up":
			to(top - 1)
		case "down":
			to(top + 1)
		case "pgup":
			to(top - m.tPage)
		case "pgdown":
			to(top + m.tPage)
		case "home":
			to(0)
		case "end":
			m.tTop = -1
		case "t", "esc":
			m.focusT = false
		default:
			return m.action(s, cur)
		}
		return nil
	}
	if m.view == detailView {
		switch s {
		case "up":
			m.scroll = max(0, m.scroll-1)
		case "down":
			m.scroll++
		case "pgup":
			m.scroll = max(0, m.scroll-20)
		case "pgdown":
			m.scroll += 20
		case "home":
			m.scroll = 0
		case "end":
			m.scroll = 1 << 30
		case "t":
			m.focusT = true
		case "esc", "enter", "left", "backspace":
			m.view = listView
		default:
			return m.action(s, cur)
		}
		return nil
	}
	rows := m.rows()
	switch s {
	case "up":
		m.selected = max(0, m.selected-1)
	case "down":
		m.selected = min(max(0, len(rows)-1), m.selected+1)
	case "enter", "right":
		if cur != nil {
			m.view = detailView
			if cur.sh != nil {
				m.detail.project, m.detail.key, m.detail.shell = "", "", cur.sh.ID
			} else {
				m.detail.project, m.detail.key, m.detail.shell = cur.p.Name, cur.r.Key, 0
			}
			m.scroll, m.focusT, m.tTop = 0, false, -1
		}
	case "!":
		m.picker = m.shellPicker()
	case "c":
		var fallback *engine.Project
		if cur != nil && cur.p != nil {
			fallback = cur.p
		} else if ps := m.sup.Projects(); len(ps) > 0 {
			fallback = ps[0]
		}
		if fallback == nil {
			m.flash = "no projects"
			return nil
		}
		prompt := fmt.Sprintf("claim issue (%s: 12 or PROJ-12, or <project>#12): ", fallback.Name)
		if src := fallback.Source(); !src.Spec.Implicit() {
			prompt = fmt.Sprintf("claim issue (from %s by its routes: PROJ-12, or <project>#PROJ-12): ", src.Name)
		}
		return m.ask(prompt, func(v string) tea.Cmd {
			p, key, forced := m.parseClaim(v, fallback)
			if p == nil || key == "" {
				return flash(fmt.Sprintf("couldn't read %q as an issue to claim", v))
			}
			return func() tea.Msg {
				if forced {
					if err := p.ClaimNow(key); err != nil {
						return flashMsg(fmt.Sprintf("%s %s: couldn't claim: %v", p.Name, key, err))
					}
					return flashMsg(fmt.Sprintf("claimed %s %s", p.Name, p.Ref(key)))
				}
				got, err := p.Source().ClaimNow(key)
				if err != nil {
					return flashMsg(fmt.Sprintf("%s: couldn't claim: %v", key, err))
				}
				return flashMsg(fmt.Sprintf("claimed %s %s", got.Name, got.Ref(key)))
			}
		})
	case "R":
		go m.sup.Reconcile()
		m.flash = "reconciling…"
	case "s":
		go m.sup.RunSweep(nil)
		m.flash = "sweeping…"
	case "l":
		m.showLog = !m.showLog
	case ",":
		m.editor = newEditor(m.sup, m.width, m.height)
		m.view = editorView
		return m.editor.init()
	default:
		if jobs := m.jobKeys()[s]; len(jobs) > 0 {
			j := jobs[0]
			j.p.Jobs.RunNow(j.job)
			m.flash = fmt.Sprintf("%s: running %s", j.p.Name, j.job.Name)
			return nil
		}
		return m.action(s, cur)
	}
	return nil
}

var claimRe = regexp.MustCompile(`^(?:([\w.-]+)#)?#?(\S+)$`)

// parseClaim reads "12", "#12", "PROJ-12" or "project#12". forced: a project was named, so its routes don't
// decide.
func (m *model) parseClaim(v string, fallback *engine.Project) (*engine.Project, string, bool) {
	g := claimRe.FindStringSubmatch(strings.TrimSpace(v))
	if g == nil {
		return nil, "", false
	}
	p := fallback
	if g[1] != "" {
		p = m.sup.Project(g[1])
	}
	return p, g[2], g[1] != ""
}

func flash(s string) tea.Cmd { return func() tea.Msg { return flashMsg(s) } }

// action handles the keys that act on an issue, on the list and the detail page.
func (m *model) action(s string, cur *row) tea.Cmd {
	switch s {
	case "q":
		return tea.Quit
	}
	if cur == nil {
		return nil
	}
	if cur.sh != nil {
		return m.shellAction(s, cur.sh)
	}
	p, r := cur.p, cur.r
	switch s {
	case "a":
		if r.State == store.Running {
			return flash(p.Ref(r.Key) + ": can't attach while a turn is running")
		}
		if p.Attach(r.Key) == nil {
			return flash(p.Ref(r.Key) + ": can't attach now")
		}
		cmd, err := p.AttachCommand(r.Key)
		if err != nil {
			p.Detach(r.Key)
			return flash("attach: " + err.Error())
		}
		key := r.Key
		return tea.ExecProcess(cmd, func(err error) tea.Msg { return attachedMsg{p, key, err} })
	case "r":
		p.RunNow(r.Key)
		return flash(p.Ref(r.Key) + ": turn requested")
	case "k":
		if r.State == store.Running {
			p.KillTurn(r.Key)
		}
	case "m":
		to := "a turn now"
		switch r.State {
		case store.Running:
			to = "the running turn"
		case store.Manual:
			to = "the next turn"
		}
		key := r.Key
		return m.ask(fmt.Sprintf("message to %s %s (into %s; esc cancels): ", p.Name, p.Ref(key), to), func(v string) tea.Cmd {
			if v != "" {
				p.Message(key, v)
			}
			return nil
		})
	case "g":
		if url := p.IssueURL(r.Key); url != "" {
			openURL(url)
		}
	case "d":
		key := r.Key
		extra := ""
		if r.State == store.Running {
			extra = " and kill its turn"
		}
		return m.ask(fmt.Sprintf("drop %s %s from the list%s? (y/n) ", p.Name, p.Ref(key), extra), func(v string) tea.Cmd {
			if strings.EqualFold(v, "y") {
				if m.view == detailView {
					m.view = listView
				}
				p.Drop(key)
			}
			return nil
		})
	}
	return nil
}

func openURL(url string) {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	_ = cmd.Start()
	go func() { _ = cmd.Wait() }()
}

func (m *model) View() string {
	switch m.view {
	case editorView:
		if m.editor != nil {
			return m.editor.view()
		}
	case detailView:
		if cur := m.current(); cur != nil {
			return m.renderDetail(*cur)
		}
		m.view = listView
	}
	return m.renderList()
}

// paint fits lines to the screen.
func (m *model) paint(lines []string) string {
	if len(lines) > m.height {
		lines = lines[:m.height]
	}
	for i, l := range lines {
		lines[i] = pad(l, m.width)
	}
	for len(lines) < m.height {
		lines = append(lines, "")
	}
	return strings.Join(lines, "\n")
}

func (m *model) footer(keys string) string {
	if m.picker != nil {
		return dim(" ↑↓ choose   ⏎ pick   esc cancel")
	}
	if m.input != nil {
		return " " + m.input.prompt + m.input.ti.View()
	}
	if m.flash != "" {
		return " " + yellowS.Render(m.flash)
	}
	return dim(keys)
}
