package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/alsiola/open-supervisor/internal/engine"
	"github.com/alsiola/open-supervisor/internal/store"
)

// picker is a modal list over the page: ↑↓ choose, ⏎ picks, esc cancels.
type picker struct {
	title string
	items []pickItem
	sel   int
	done  func(pickItem) tea.Cmd
}

type pickItem struct {
	label, value string
	ref          any // what the item stands for, when the value doesn't say
}

func (m *model) pickerKey(k tea.KeyMsg) tea.Cmd {
	pk := m.picker
	switch k.String() {
	case "ctrl+c":
		return tea.Quit
	case "esc":
		m.picker = nil
	case "up", "k":
		pk.sel = max(0, pk.sel-1)
	case "down", "j":
		pk.sel = min(len(pk.items)-1, pk.sel+1)
	case "enter":
		m.picker = nil
		if pk.sel < len(pk.items) {
			return pk.done(pk.items[pk.sel])
		}
	}
	return nil
}

// shellDirs are where a shell task can run: where open-supervisor was started, then each project's checkout.
func (m *model) shellDirs() []pickItem {
	var out []pickItem
	seen := map[string]int{}
	if m.cwd != "" {
		out = append(out, pickItem{label: "here", value: m.cwd})
		seen[filepath.Clean(m.cwd)] = 0
	}
	for _, p := range m.sup.Projects() {
		if p.Root == "" {
			continue
		}
		if i, ok := seen[filepath.Clean(p.Root)]; ok {
			out[i].label += ", " + p.Name
			continue
		}
		seen[filepath.Clean(p.Root)] = len(out)
		out = append(out, pickItem{label: p.Name, value: p.Root})
	}
	return out
}

// shellPicker asks where to run a shell command, then asks for the command.
func (m *model) shellPicker() *picker {
	return &picker{title: "run a shell command in", items: m.shellDirs(), done: func(it pickItem) tea.Cmd {
		dir := it.value
		return m.ask(fmt.Sprintf("shell command in %s (esc cancels): ", tilde(dir)), func(v string) tea.Cmd {
			if v == "" {
				return nil
			}
			return m.runShell(dir, v)
		})
	}}
}

func (m *model) runShell(dir, command string) tea.Cmd {
	sh, err := m.sup.RunShell(dir, command)
	if err != nil {
		return flash("shell: " + err.Error())
	}
	return flash(fmt.Sprintf("!%d: running %s", sh.ID, command))
}

// shellAction handles the issue keys on a shell task: r runs it again, k kills it, d drops it.
func (m *model) shellAction(s string, sh *engine.Shell) tea.Cmd {
	ref := fmt.Sprintf("!%d", sh.ID)
	switch s {
	case "r":
		return m.runShell(sh.Dir, sh.Command)
	case "k":
		if sh.Running() {
			m.sup.KillShell(sh)
		}
	case "d":
		drop := func() {
			if m.view == detailView {
				m.view = listView
			}
			m.sup.DropShell(sh)
		}
		if !sh.Running() {
			drop()
			return nil
		}
		return m.ask(fmt.Sprintf("drop %s from the list and kill it? (y/n) ", ref), func(v string) tea.Cmd {
			if strings.EqualFold(v, "y") {
				drop()
			}
			return nil
		})
	case "a", "m", "g":
		return flash(ref + ": a shell task has no agent or issue")
	}
	return nil
}

// shellState is how a shell task's state shows: its own word, in the colour of the issue state nearest it.
func shellState(sh *engine.Shell) string {
	st := sh.State()
	like := map[string]store.State{"running": store.Running, "done": store.Done, "killed": store.Finished, "failed": store.Failed}[st]
	return stateStyle(like).Render(pad(st, 9))
}

// shellWhere names a shell task's directory: the project whose checkout it is, else the folder.
func (m *model) shellWhere(dir string) string {
	for _, p := range m.sup.Projects() {
		if p.Root != "" && filepath.Clean(p.Root) == filepath.Clean(dir) {
			return p.Name
		}
	}
	return filepath.Base(dir)
}

func (m *model) shellInfo(sh *engine.Shell) string {
	ended, exit, errText := sh.Ended()
	switch {
	case errText != "":
		return redS.Render(errText)
	case ended != nil && exit != 0 && sh.State() == "failed":
		s := redS.Render(fmt.Sprintf("exit %d", exit))
		if l := sh.LastLine(); l != "" {
			s += " · " + clean(l)
		}
		return s
	}
	return clean(sh.LastLine())
}

func (m *model) shellLine(sh *engine.Shell, infoW int) string {
	at := sh.StartedAt
	if ended, _, _ := sh.Ended(); ended != nil {
		at = *ended
	}
	return fmt.Sprintf(" %s %s %s %s %s %s %s %s %s", pad(m.shellWhere(sh.Dir), 11), pad(fmt.Sprintf("!%d", sh.ID), 9), pad(clean(sh.Command), 34), shellState(sh), pad("", 6), pad("", 5), pad("", 7), pad(ago(at), 5), pad(m.shellInfo(sh), infoW))
}

func (m *model) shellDetailLines(sh *engine.Shell) []string {
	w := max(40, m.width-4)
	field := func(name, value string) string { return "  " + dim(pad(name, 14)) + value }
	lines := []string{"  " + dim("shell") + " " + bold(fmt.Sprintf("!%d", sh.ID))}
	for i, l := range wrap(clean(sh.Command), w-16) {
		lines = append(lines, field(map[bool]string{true: "command", false: ""}[i == 0], l))
	}
	lines = append(lines, field("directory", tilde(sh.Dir)), field("state", shellState(sh)), field("started", when(&sh.StartedAt)))
	ended, exit, errText := sh.Ended()
	if ended != nil {
		lines = append(lines, field("ended", when(ended)))
		if sh.State() != "killed" {
			lines = append(lines, field("exit", fmt.Sprint(exit)))
		}
	}
	if errText != "" {
		lines = append(lines, field("error", redS.Render(errText)))
	}
	if sh.Log != "" {
		lines = append(lines, field("log", sh.Log))
	}
	return lines
}

func (m *model) shellOutputLines(sh *engine.Shell) []string {
	out := sh.Output()
	key := fmt.Sprintf("!%d:%d:%d:%s", sh.ID, len(out), m.width, sh.LastLine())
	if m.tCache.key != key {
		var lines []string
		for _, l := range out {
			lines = append(lines, wrap(clean(l), max(10, m.width-2))...)
		}
		m.tCache.key, m.tCache.lines = key, lines
	}
	return m.tCache.lines
}

// overlay draws the picker as a box in the middle of the page's lines.
func (m *model) overlay(lines []string) []string {
	pk := m.picker
	if pk == nil {
		return lines
	}
	inner := ansi.StringWidth(pk.title) + 2
	for _, it := range pk.items {
		inner = max(inner, ansi.StringWidth(it.label)+ansi.StringWidth(tilde(it.value))+7)
	}
	inner = min(inner, max(20, m.width-6))
	box := []string{accentS.Render("╭─ ") + boldS.Render(pk.title) + accentS.Render(" "+strings.Repeat("─", max(0, inner-ansi.StringWidth(pk.title)-3))+"╮")}
	for i, it := range pk.items {
		text := pad(" "+it.label+"  "+dim(tilde(it.value)), inner)
		if i == pk.sel {
			text = invS.Render(pad(" "+it.label+"  "+tilde(it.value), inner))
		}
		box = append(box, accentS.Render("│")+text+accentS.Render("│"))
	}
	box = append(box, accentS.Render("╰"+strings.Repeat("─", inner)+"╯"))
	left := max(0, (m.width-inner-2)/2)
	top := max(2, (len(lines)-len(box))/2)
	for i, b := range box {
		if top+i >= len(lines) {
			lines = append(lines, "")
		}
		lines[top+i] = strings.Repeat(" ", left) + b
	}
	return lines
}

// tilde shortens a path under the home folder.
func tilde(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if rest, ok := strings.CutPrefix(path, home); ok && (rest == "" || strings.HasPrefix(rest, string(filepath.Separator))) {
		return "~" + rest
	}
	return path
}
