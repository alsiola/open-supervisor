package tui

import (
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"

	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/engine"
)

func TestShellTaskFromThePicker(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("runs sh")
	}
	t.Setenv("SHELL", "/bin/sh")
	t.Setenv("OPEN_SUPERVISOR_STATE", t.TempDir())
	sup, err := engine.New(&config.Config{}, engine.Options{})
	if err != nil {
		t.Fatal(err)
	}
	m := newModel(sup)
	m.cwd = t.TempDir()
	press := func(keys ...string) tea.Cmd {
		var last tea.Cmd
		for _, k := range keys {
			msg := tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(k)}
			switch k {
			case "enter":
				msg = tea.KeyMsg{Type: tea.KeyEnter}
			case "down":
				msg = tea.KeyMsg{Type: tea.KeyDown}
			}
			_, last = m.Update(msg)
		}
		return last
	}
	press("!")
	if m.picker == nil || len(m.picker.items) != 1 || m.picker.items[0].value != m.cwd {
		t.Fatalf("picker %+v", m.picker)
	}
	if !strings.Contains(ansi.Strip(m.View()), "run a shell command in") {
		t.Fatal("no picker on screen")
	}
	press("enter")
	if m.picker != nil || m.input == nil {
		t.Fatal("no command prompt")
	}
	m.input.ti.SetValue("echo hello from the shell")
	if cmd := press("enter"); cmd != nil {
		m.Update(cmd())
	}
	shells := sup.Shells()
	if len(shells) != 1 {
		t.Fatalf("%d shells", len(shells))
	}
	for deadline := time.Now().Add(5 * time.Second); shells[0].Running() && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	list := ansi.Strip(m.View())
	if !strings.Contains(list, "!1") || !strings.Contains(list, "done") || !strings.Contains(list, "hello from the shell") {
		t.Fatalf("not in the list:\n%s", list)
	}
	press("enter")
	if m.view != detailView || !strings.Contains(ansi.Strip(m.View()), "output (1-1 of 1") {
		t.Fatalf("detail:\n%s", ansi.Strip(m.View()))
	}
	press("d")
	if len(sup.Shells()) != 0 || m.view != listView {
		t.Fatal("not dropped")
	}
}
