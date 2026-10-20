package tui

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/alsiola/open-supervisor/internal/core"
	"github.com/alsiola/open-supervisor/internal/store"
)

var (
	dimS     = lipgloss.NewStyle().Faint(true)
	boldS    = lipgloss.NewStyle().Bold(true)
	invS     = lipgloss.NewStyle().Reverse(true)
	redS     = lipgloss.NewStyle().Foreground(lipgloss.Color("1"))
	greenS   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	yellowS  = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	cyanS    = lipgloss.NewStyle().Foreground(lipgloss.Color("6"))
	accentS  = lipgloss.NewStyle().Foreground(lipgloss.Color("6")).Bold(true)
	tabOnS   = lipgloss.NewStyle().Bold(true).Reverse(true).Padding(0, 1)
	tabOffS  = lipgloss.NewStyle().Faint(true).Padding(0, 1)
	stateCol = map[store.State]lipgloss.Style{
		store.Queued:    lipgloss.NewStyle().Foreground(lipgloss.Color("7")),
		store.Running:   cyanS,
		store.Building:  yellowS,
		store.Review:    greenS,
		store.Help:      lipgloss.NewStyle().Foreground(lipgloss.Color("5")),
		store.Done:      greenS,
		store.Finished:  lipgloss.NewStyle().Foreground(lipgloss.Color("8")),
		store.Scheduled: lipgloss.NewStyle().Foreground(lipgloss.Color("12")),
		store.Manual:    lipgloss.NewStyle().Foreground(lipgloss.Color("4")),
		store.Failed:    redS,
	}
)

func stateStyle(s store.State) lipgloss.Style {
	if st, ok := stateCol[s]; ok {
		return st
	}
	return lipgloss.NewStyle()
}

func dim(s string) string  { return dimS.Render(s) }
func bold(s string) string { return boldS.Render(s) }

// pad cuts or pads s to width visible cells, keeping its colours.
func pad(s string, width int) string {
	if width <= 0 {
		return ""
	}
	w := ansi.StringWidth(s)
	if w > width {
		return ansi.Truncate(s, width, "…")
	}
	return s + strings.Repeat(" ", width-w)
}

// ago is how long ago t was, in one short unit.
func ago(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	s := math.Max(0, time.Since(t).Seconds())
	switch {
	case s < 60:
		return fmt.Sprintf("%ds", int(s))
	case s < 3600:
		return fmt.Sprintf("%dm", int(math.Round(s/60)))
	case s < 86400:
		return fmt.Sprintf("%.1fh", s/3600)
	}
	return fmt.Sprintf("%dd", int(math.Round(s/86400)))
}

func when(t *time.Time) string {
	if t == nil || t.IsZero() {
		return ""
	}
	return t.Local().Format("2006-01-02 15:04:05") + " (" + ago(*t) + " ago)"
}

func kTokens(n int64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	case n >= 1000:
		return fmt.Sprintf("%dk", int(math.Round(float64(n)/1000)))
	}
	return fmt.Sprint(n)
}

func tokensText(t *core.Tokens) string {
	if t == nil {
		return dim("none recorded")
	}
	return fmt.Sprintf("in %s, out %s, cache read %s, cache write %s", kTokens(t.Input), kTokens(t.Output), kTokens(t.CacheRead), kTokens(t.CacheCreation))
}

// wrap breaks text into lines of at most width cells, keeping blank lines.
func wrap(text string, width int) []string {
	if width < 10 {
		width = 10
	}
	var out []string
	for _, raw := range strings.Split(text, "\n") {
		line := []rune(raw)
		for len(line) > width {
			cut := -1
			for i := width; i > width/2; i-- {
				if line[i] == ' ' {
					cut = i
					break
				}
			}
			if cut < 0 {
				cut = width
			}
			out = append(out, string(line[:cut]))
			line = []rune(strings.TrimLeft(string(line[cut:]), " "))
		}
		out = append(out, string(line))
	}
	return out
}

// clean drops control codes from tool output so it can't move the cursor.
func clean(s string) string {
	s = ansi.Strip(s)
	s = strings.ReplaceAll(s, "\t", "  ")
	return strings.Map(func(r rune) rune {
		if r == '\n' {
			return r
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

func usageColour(pct float64) lipgloss.Style {
	switch {
	case pct >= 85:
		return redS
	case pct >= 60:
		return yellowS
	}
	return greenS
}
