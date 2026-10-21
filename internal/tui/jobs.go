package tui

import (
	"fmt"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/alsiola/open-supervisor/internal/engine"
)

// jobRun is a run of a project's job, shown on the detail page.
type jobRun struct {
	p   *engine.Project
	job string
	id  string
}

// runOutputLines is how much of a run's log the page reads: its end.
const runOutputLines = 5000

// jobsPicker lists every project's jobs; picking one lists its runs.
func (m *model) jobsPicker() *picker {
	var items []pickItem
	for _, p := range m.sup.Projects() {
		for _, j := range p.Jobs.Specs {
			items = append(items, pickItem{label: p.Name + " " + j.Name, value: p.Jobs.Describe(j), ref: jobRun{p: p, job: j.Name}})
		}
	}
	return &picker{title: "job runs", items: items, done: func(it pickItem) tea.Cmd {
		ref := it.ref.(jobRun)
		runs := ref.p.Jobs.Runs(ref.job)
		if len(runs) == 0 {
			return flash(fmt.Sprintf("%s %s: no runs kept yet", ref.p.Name, ref.job))
		}
		var items []pickItem
		for _, r := range runs {
			items = append(items, pickItem{label: r.StartedAt.Local().Format("2 Jan 15:04") + "  " + r.Trigger, value: runSummary(r), ref: jobRun{p: ref.p, job: ref.job, id: r.ID}})
		}
		m.picker = &picker{title: ref.p.Name + " " + ref.job, items: items, done: func(it pickItem) tea.Cmd {
			m.openRun(it.ref.(jobRun))
			return nil
		}}
		return nil
	}}
}

// runPicker lists every project's jobs; picking one runs it now.
func (m *model) runPicker() *picker {
	var items []pickItem
	for _, p := range m.sup.Projects() {
		for _, j := range p.Jobs.Specs {
			items = append(items, pickItem{label: p.Name + " " + j.Name, value: p.Jobs.Describe(j), ref: jobRun{p: p, job: j.Name}})
		}
	}
	return &picker{title: "run a job now", items: items, done: func(it pickItem) tea.Cmd {
		ref := it.ref.(jobRun)
		if j, ok := ref.p.Jobs.Find(ref.job); ok {
			ref.p.Jobs.RunNow(j)
			return flash(fmt.Sprintf("%s: running %s", ref.p.Name, ref.job))
		}
		return flash(fmt.Sprintf("%s: no job %s", ref.p.Name, ref.job))
	}}
}

func (m *model) openRun(ref jobRun) {
	m.view = detailView
	m.detail.project, m.detail.key, m.detail.shell, m.detail.run = "", "", 0, &ref
	m.scroll, m.focusT, m.tTop = 0, false, -1
}

// runSummary is how a run went and how long it took.
func runSummary(r engine.JobRun) string {
	s := r.Outcome
	if r.Exit != nil && r.Outcome != "ok" {
		s += fmt.Sprintf(" (exit %d)", *r.Exit)
	}
	return s + " · " + r.Took().Round(time.Second).String()
}

// runStyle colours a run's outcome.
func runStyle(outcome string) string {
	switch outcome {
	case "ok", "done":
		return greenS.Render(outcome)
	case "running":
		return yellowS.Render(outcome)
	}
	return redS.Render(outcome)
}

func (m *model) runDetailLines(ref jobRun, r engine.JobRun) []string {
	field := func(name, value string) string { return "  " + dim(pad(name, 14)) + value }
	lines := []string{"  " + dim(ref.p.Name+" job") + " " + bold(ref.job), field("trigger", r.Trigger), field("state", runStyle(r.Outcome)), field("started", when(&r.StartedAt))}
	if r.EndedAt != nil {
		lines = append(lines, field("ended", when(r.EndedAt)))
	}
	lines = append(lines, field("took", r.Took().Round(time.Second).String()))
	if r.Exit != nil {
		lines = append(lines, field("exit", fmt.Sprint(*r.Exit)))
	}
	if r.Error != "" {
		lines = append(lines, field("error", redS.Render(r.Error)))
	}
	if r.Detached {
		lines = append(lines, field("detached", "yes: it reports through its status folder"))
	}
	if r.Dir != "" {
		lines = append(lines, field("ran in", tilde(r.Dir)))
	}
	if r.Log != "" {
		lines = append(lines, field("log", tilde(r.Log)))
	}
	return lines
}

func (m *model) runOutput(r engine.JobRun) []string {
	var size int64
	if info, err := os.Stat(r.Log); err == nil {
		size = info.Size()
	}
	key := fmt.Sprintf("run:%s:%d:%d", r.Log, size, m.width)
	if m.tCache.key != key {
		data, _ := os.ReadFile(r.Log)
		out := strings.Split(strings.TrimRight(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n"), "\n")
		if len(out) == 1 && out[0] == "" {
			out = nil
		}
		if n := len(out) - runOutputLines; n > 0 {
			out = out[n:]
		}
		var lines []string
		for _, l := range out {
			lines = append(lines, wrap(clean(l), max(10, m.width-2))...)
		}
		m.tCache.key, m.tCache.lines = key, lines
	}
	return m.tCache.lines
}

// runAction handles the issue keys on a job's run: r runs the job again.
func (m *model) runAction(s string, ref jobRun) tea.Cmd {
	switch s {
	case "r":
		if j, ok := ref.p.Jobs.Find(ref.job); ok {
			ref.p.Jobs.RunNow(j)
			return flash(fmt.Sprintf("%s: running %s", ref.p.Name, ref.job))
		}
	case "a", "m", "g", "k", "d":
		return flash(ref.job + ": a job's run has no agent or issue")
	}
	return nil
}
