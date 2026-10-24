package tui

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/alsiola/open-supervisor/internal/core"
	"github.com/alsiola/open-supervisor/internal/logx"
	"github.com/alsiola/open-supervisor/internal/store"
)

func (m *model) header() []string {
	sup := m.sup
	st := sup.Config().Settings
	sep := dim("  ·  ")
	var hook string
	switch {
	case sup.HookURL() != "":
		hook = greenS.Render("webhook up")
	case sup.TunnelEnabled():
		hook = redS.Render("webhook down")
	default:
		hook = dim("polling only")
	}
	parts := []string{" " + bold("open-supervisor") + "  " + hook, fmt.Sprintf("turns %d/%d", sup.TurnsRunning(), st.MaxTurns)}
	if sup.Sweeping() {
		parts = append(parts, yellowS.Render("sweep running"))
	}
	if sup.Triaging() {
		parts = append(parts, yellowS.Render("triage running"))
	}
	if n := sup.Problems(); n == 1 {
		parts = append(parts, yellowS.Render("1 problem"))
	} else if n > 1 {
		parts = append(parts, yellowS.Render(fmt.Sprintf("%d problems", n)))
	}
	if j := m.jobsText(); j != "" {
		parts = append(parts, j)
	}
	if n := sup.Unrouted(); n > 0 {
		parts = append(parts, yellowS.Render(fmt.Sprintf("%d unrouted", n)))
	}
	parts = append(parts, m.reconcileText())
	if u := m.usageText(); u != "" {
		parts = append(parts, u)
	}
	if sup.DryRun() {
		parts = append(parts, yellowS.Render("DRY RUN"))
	}
	return []string{strings.Join(parts, sep), dim(strings.Repeat("─", m.width))}
}

func (m *model) reconcileText() string {
	var oldest time.Time
	busy := false
	for _, src := range m.sup.Sources() {
		at, going := src.Reconciled()
		busy = busy || going
		if oldest.IsZero() || at.Before(oldest) {
			oldest = at
		}
	}
	switch {
	case busy:
		return "reconciling…"
	case oldest.IsZero():
		return "not yet reconciled"
	}
	return "reconciled " + ago(oldest) + " ago"
}

func (m *model) jobsText() string {
	var parts []string
	for _, p := range m.sup.Projects() {
		for _, j := range p.Jobs.Specs {
			if !j.Timed() {
				continue
			}
			d := p.Jobs.Describe(j)
			text := p.Name + " " + j.Name + " " + d
			switch {
			case strings.HasPrefix(d, "running"):
				text = yellowS.Render(text)
			case strings.HasPrefix(d, "stopped"), strings.HasPrefix(d, "failed"), strings.HasPrefix(d, "timed out"), strings.HasPrefix(d, "couldn't"), strings.HasPrefix(d, "checkout failed"):
				text = redS.Render(text)
			default:
				text = dim(text)
			}
			parts = append(parts, text)
		}
	}
	return strings.Join(parts, dim("  ·  "))
}

func (m *model) usageText() string {
	usage, errs := m.sup.Usage()
	names := map[string]bool{}
	for k := range usage {
		names[k] = true
	}
	for k := range errs {
		names[k] = true
	}
	var keys []string
	for k := range names {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		prefix := ""
		if len(keys) > 1 {
			prefix = k + " "
		}
		u := usage[k]
		if u == nil {
			parts = append(parts, prefix+redS.Render("usage unknown"))
			continue
		}
		var ws []string
		for _, w := range u.Windows {
			at := w.ResetsAt.Local()
			reset := at.Format("15:04")
			if at.Format("2006-01-02") != time.Now().Format("2006-01-02") {
				reset = at.Format("Mon 15:04")
			}
			ws = append(ws, fmt.Sprintf("%s %s%s", w.Name, usageColour(w.Used).Render(fmt.Sprintf("%d%%", int(math.Round(w.Used)))), dim(" ↻"+reset)))
		}
		parts = append(parts, prefix+strings.Join(ws, "  "))
	}
	return strings.Join(parts, "  ")
}

func (m *model) info(rw row) string {
	p, r := rw.p, rw.r
	st := m.sup.Config().Settings
	summary := ""
	if r.LastTurn != nil {
		summary = r.LastTurn.Summary
	}
	switch {
	case r.State == store.Running:
		s := "running"
		if r.LastTurn != nil {
			s += " " + ago(r.LastTurn.StartedAt)
			if n := len(r.LastTurn.Events); n > 0 {
				s += fmt.Sprintf(" on %d event(s)", n)
			}
		}
		if a := p.Activity(r.Key); a != nil {
			s += " · " + a.Text
			if !a.At.IsZero() {
				s += dim(" " + ago(a.At) + " ago")
			}
		}
		return s
	case r.State == store.Scheduled && (r.RunAt != nil || len(r.Waits) > 0):
		var on []string
		for _, c := range r.Waits {
			on = append(on, c.Short())
		}
		var parts []string
		if len(on) > 0 {
			parts = append(parts, "on "+strings.Join(on, " or "))
		}
		if r.RunAt != nil {
			parts = append(parts, "until "+r.RunAt.Local().Format("2 Jan 2006 15:04"))
		}
		s := stateStyle(store.Scheduled).Render(strings.Join(parts, " · "))
		if summary != "" {
			s += " · " + summary
		}
		return s
	case r.State == store.Failed:
		e := "failed"
		if r.LastTurn != nil && r.LastTurn.Error != "" {
			e = r.LastTurn.Error
		}
		switch {
		case r.Attempts >= st.MaxAttempts:
			e += " (press r to retry)"
		case r.RetryAfter != nil:
			e += fmt.Sprintf(" (retry in %d min)", max(0, int(math.Ceil(time.Until(*r.RetryAfter).Minutes()))))
		}
		return redS.Render(e)
	case len(r.Queue) > 0:
		s := yellowS.Render(fmt.Sprintf("%d queued", len(r.Queue)))
		if summary != "" {
			s += " · " + summary
		}
		return s
	case summary != "":
		return summary
	case r.State == store.Queued:
		return "waiting for a turn slot"
	}
	return ""
}

func (m *model) renderList() string {
	rows := m.rows()
	if m.selected >= len(rows) {
		m.selected = max(0, len(rows)-1)
	}
	out := m.header()
	infoW := max(20, m.width-96)
	out = append(out, dim(fmt.Sprintf(" %s %s %s %s %s %s %s %s %s", pad("project", 11), pad("issue", 9), pad("title", 34), pad("state", 9), pad("pr", 6), pad("turns", 5), pad("cost", 7), pad("age", 5), pad("info", infoW))))
	logLines := 0
	if m.showLog {
		logLines = min(12, max(4, m.height/3))
	}
	listH := max(3, m.height-4-1)
	if m.showLog {
		listH = max(3, m.height-4-logLines-1)
	}
	first := max(0, min(m.selected-listH/2, len(rows)-listH))
	for i := first; i < len(rows) && i < first+listH; i++ {
		rw := rows[i]
		if rw.sh != nil {
			line := m.shellLine(rw.sh, infoW)
			if i == m.selected {
				line = invS.Render(pad(ansi.Strip(line), m.width))
			}
			out = append(out, line)
			continue
		}
		r := rw.r
		pr := ""
		if len(r.PRs) > 0 {
			pr = fmt.Sprintf("#%d", r.PRs[len(r.PRs)-1])
		}
		line := fmt.Sprintf(" %s %s %s %s %s %s %s %s %s", pad(rw.p.Name, 11), pad(rw.p.Ref(r.Key), 9), pad(r.Title, 34), stateStyle(r.State).Render(pad(string(r.State), 9)), pad(pr, 6), pad(fmt.Sprint(r.Turns), 5), pad(fmt.Sprintf("$%.2f", r.Cost), 7), pad(ago(r.UpdatedAt), 5), pad(m.info(rw), infoW))
		if i == m.selected {
			line = invS.Render(pad(ansi.Strip(line), m.width))
		}
		out = append(out, line)
	}
	// Under the issues in flight, the ones no route takes, for claiming by hand (c project#KEY).
	if un := m.unroutedLines(infoW); len(un) > 0 && len(out) < 3+listH-1 {
		out = append(out, "")
		for _, l := range un {
			if len(out) >= 3+listH {
				break
			}
			out = append(out, l)
		}
	}
	for len(out) < 3+listH {
		if len(rows) == 0 && len(out) == 3 {
			if len(m.sup.Projects()) == 0 {
				out = append(out, dim("  no projects yet: press , to add one"))
			} else {
				out = append(out, dim("  nothing in flight"))
			}
			continue
		}
		out = append(out, "")
	}
	if m.showLog {
		out = append(out, dim("─ log "+strings.Repeat("─", max(0, m.width-6))))
		for _, l := range logx.Default.Lines(logLines) {
			out = append(out, " "+l)
		}
		for len(out) < 3+listH+1+logLines {
			out = append(out, "")
		}
	}
	keys := " ↑↓ select   ⏎ details   g open   m message   a attach   r run now   k kill   d drop   c claim   ! shell   j run job   J job runs   R reconcile   s sweep   T triage   l log   , config   q quit"
	out = append(m.overlay(out), m.footer(keys))
	return m.paint(out)
}

// unroutedLines are the open issues no route takes, by source.
func (m *model) unroutedLines(infoW int) []string {
	var out []string
	for _, src := range m.sup.Sources() {
		for _, i := range src.Unrouted() {
			out = append(out, dim(fmt.Sprintf(" %s %s %s %s", pad(src.Name, 11), pad(i.Key, 9), pad(i.Title, 34), pad("unrouted: no route takes it; c <project>#"+i.Key+" claims it", infoW+38))))
		}
	}
	return out
}

func (m *model) detailLines(rw row) []string {
	p, r := rw.p, rw.r
	w := max(40, m.width-4)
	field := func(name, value string) string { return "  " + dim(pad(name, 14)) + value }
	section := func(name string) []string { return []string{"", bold("  " + name)} }
	var lines []string
	from := p.Tracker.Describe()
	if src := p.Source(); src != nil && !src.Spec.Implicit() {
		from += " via " + src.Name
	}
	lines = append(lines, "  "+dim(from)+" "+bold(p.Ref(r.Key))+" "+r.Title)
	state := stateStyle(r.State).Render(string(r.State))
	if r.PreviousState != "" {
		state += dim(" (was " + string(r.PreviousState) + ")")
	}
	lines = append(lines, field("state", state))
	turns := fmt.Sprintf("%d run, $%.2f", r.Turns, r.Cost)
	if r.Attempts > 0 {
		turns += fmt.Sprintf(", %d failed attempt(s)", r.Attempts)
	}
	if r.RetryAfter != nil && r.RetryAfter.After(time.Now()) {
		turns += ", retry " + when(r.RetryAfter)
	}
	lines = append(lines, field("turns", turns), field("tokens", tokensText(r.Tokens)))
	lines = append(lines, field("claimed", when(&r.ClaimedAt)), field("updated", when(&r.UpdatedAt)))
	if url := p.IssueURL(r.Key); url != "" {
		lines = append(lines, field("issue", url))
	}
	prs := dim("none yet")
	if len(r.PRs) > 0 {
		var ps []string
		for _, id := range r.PRs {
			s := fmt.Sprintf("#%d", id)
			for _, c := range r.ClosedPRs {
				if c == id {
					s += " (closed)"
				}
			}
			ps = append(ps, s)
		}
		prs = strings.Join(ps, ", ")
	}
	lines = append(lines, field("PRs", prs))
	if r.HeadSHA != "" {
		h := r.HeadSHA[:min(7, len(r.HeadSHA))]
		switch {
		case r.ChecksReported == r.HeadSHA:
			h += " (checks reported)"
		case r.State == store.Building:
			h += " (waiting for checks)"
		}
		lines = append(lines, field("head", h))
	}
	for _, c := range r.Waits {
		lines = append(lines, field("waits for", c.Describe()))
	}
	if r.RunAt != nil {
		lines = append(lines, field("runs at", r.RunAt.Local().Format("2006-01-02 15:04:05")))
	}
	if r.Preview != "" {
		lines = append(lines, field("preview", r.Preview))
	}
	if r.Touches != "" {
		lines = append(lines, field("touches", r.Touches))
	}
	lines = append(lines, field("session", r.Session))
	if len(r.PastSessions) > 0 {
		lines = append(lines, field("earlier sessions", strings.Join(r.PastSessions, ", ")))
	}
	if r.Handoff != "" {
		lines = append(lines, field("handoff", r.Handoff))
	}
	if tp := p.TranscriptPath(r.Key); tp != "" {
		lines = append(lines, field("transcript", tp))
	}
	lines = append(lines, field("branch", p.BranchFor(r.Key)), field("worktree", p.WorktreePath(r.Key)))
	lines = append(lines, field("comments", fmt.Sprintf("%d ingested, since %s", len(r.Seen), r.Since.Local().Format("2006-01-02 15:04:05"))))

	if r.State == store.Running {
		lines = append(lines, section("Running")...)
		if r.LastTurn != nil {
			lines = append(lines, field("started", when(&r.LastTurn.StartedAt)))
		}
		if t := p.Running(r.Key); t != nil {
			h := t.Handle()
			if h.PID > 0 {
				lines = append(lines, field("pid", fmt.Sprint(h.PID)))
			}
			if h.Out != "" {
				lines = append(lines, field("output", h.Out))
			}
		}
		if a := p.Activity(r.Key); a != nil {
			d := a.Text
			if !a.At.IsZero() {
				d += dim(" (" + ago(a.At) + " ago)")
			}
			lines = append(lines, field("doing", d))
		}
	}
	if t := r.LastTurn; t != nil && r.State != store.Running {
		lines = append(lines, section("Last turn")...)
		lines = append(lines, field("kind", t.Kind), field("started", when(&t.StartedAt)))
		if t.EndedAt != nil {
			lines = append(lines, field("ended", when(t.EndedAt)))
		}
		if t.Cost > 0 || len(t.Models) > 0 {
			c := fmt.Sprintf("$%.2f", t.Cost)
			if len(t.Models) > 0 {
				c += dim(" (" + strings.Join(t.Models, ", ") + ")")
			}
			lines = append(lines, field("cost", c))
		}
		if t.Tokens != nil {
			lines = append(lines, field("tokens", tokensText(t.Tokens)))
		}
		for i, l := range wrap(t.Summary, w-16) {
			if t.Summary == "" {
				break
			}
			lines = append(lines, field(map[bool]string{true: "summary", false: ""}[i == 0], l))
		}
		for i, l := range wrap(t.Error, w-16) {
			if t.Error == "" {
				break
			}
			lines = append(lines, field(map[bool]string{true: "error", false: ""}[i == 0], redS.Render(l)))
		}
		if len(t.Events) > 0 {
			lines = append(lines, field("events", ""))
			for _, l := range wrap(clean(core.Render(t.Events)), w-4) {
				lines = append(lines, "    "+l)
			}
		}
	}
	lines = append(lines, section(fmt.Sprintf("Queued (%d)", len(r.Queue)))...)
	if len(r.Queue) > 0 {
		for _, l := range wrap(clean(core.Render(r.Queue)), w-4) {
			lines = append(lines, "    "+l)
		}
	} else {
		lines = append(lines, dim("    nothing waiting"))
	}
	ref := p.Ref(r.Key)
	own := logx.Default.Matching("["+p.Name+"] "+ref+":", "["+p.Name+"] claimed "+ref+" ")
	lines = append(lines, section("Log")...)
	if len(own) > 40 {
		own = own[len(own)-40:]
	}
	for _, l := range own {
		lines = append(lines, "    "+l)
	}
	if len(own) == 0 {
		lines = append(lines, dim("    nothing yet"))
	}
	return lines
}

var marks = map[core.EntryKind]string{
	core.EntryUser:      cyanS.Render("›"),
	core.EntryAssistant: "●",
	core.EntryTool:      yellowS.Render("⏺"),
	core.EntryResult:    dim("⎿"),
	core.EntryError:     redS.Render("⎿"),
}

func entryLines(e core.Entry, width int) []string {
	text := clean(e.Text)
	t := "        "
	if !e.At.IsZero() {
		t = e.At.Local().Format("15:04:05")
	}
	indent := ""
	if e.Kind == core.EntryResult || e.Kind == core.EntryError {
		indent = "  "
	}
	tone := func(l string) string {
		switch e.Kind {
		case core.EntryResult:
			return dim(l)
		case core.EntryError:
			return redS.Render(l)
		case core.EntryUser:
			return cyanS.Render(l)
		}
		return l
	}
	body := wrap(text, max(10, width-11-len(indent)))
	out := make([]string, len(body))
	for i, l := range body {
		if i == 0 {
			out[i] = dim(t) + " " + indent + marks[e.Kind] + " " + tone(l)
		} else {
			out[i] = strings.Repeat(" ", 11) + indent + tone(l)
		}
	}
	return out
}

func (m *model) transcriptLines(rw row) []string {
	entries := rw.p.Transcript(rw.r.Key)
	key := fmt.Sprintf("%s:%s:%d:%d", rw.p.Name, rw.r.Key, len(entries), m.width)
	if m.tCache.key != key {
		var lines []string
		for _, e := range entries {
			lines = append(lines, entryLines(e, m.width-1)...)
		}
		m.tCache.key, m.tCache.lines = key, lines
	}
	return m.tCache.lines
}

func (m *model) renderDetail(rw row) string {
	out := m.header()
	var body, lines []string
	what := "transcript"
	switch {
	case rw.run != nil:
		what = "output"
		if r, ok := rw.run.p.Jobs.Run(rw.run.job, rw.run.id); ok {
			body, lines = m.runDetailLines(*rw.run, r), m.runOutput(r)
		} else {
			body = []string{dim("  this run is no longer kept")}
		}
	case rw.sh != nil:
		body, lines, what = m.shellDetailLines(rw.sh), m.shellOutputLines(rw.sh), "output"
	default:
		body, lines = m.detailLines(rw), m.transcriptLines(rw)
	}
	total := m.height - len(out) - 1
	box := max(6, total/2)
	room := max(1, total-box)
	m.scroll = max(0, min(m.scroll, len(body)-room))
	end := min(len(body), m.scroll+room)
	out = append(out, body[m.scroll:end]...)
	for len(out) < m.height-1-box {
		out = append(out, "")
	}
	view := box - 1
	m.tPage = max(1, view-1)
	m.tMax = max(0, len(lines)-view)
	if m.tTop >= m.tMax {
		m.tTop = -1
	}
	top := m.tTop
	if top < 0 {
		top = m.tMax
	}
	rng := "empty"
	if len(lines) > 0 {
		rng = fmt.Sprintf("%d-%d of %d", top+1, min(len(lines), top+view), len(lines))
		if m.tTop < 0 {
			rng += ", following"
		}
	}
	title := " " + what + " " + dim("("+rng+")") + " "
	if m.focusT {
		out = append(out, accentS.Render("━━")+boldS.Render(title)+accentS.Render(strings.Repeat("━", max(0, m.width-ansi.StringWidth(title)-2))))
	} else {
		out = append(out, dim("──")+title+dim(strings.Repeat("─", max(0, m.width-ansi.StringWidth(title)-2))))
	}
	for i := top; i < len(lines) && i < top+view; i++ {
		out = append(out, " "+lines[i])
	}
	for len(out) < m.height-1 {
		if len(lines) == 0 && len(out) == m.height-box {
			out = append(out, dim("  no "+what+" yet"))
			continue
		}
		out = append(out, "")
	}
	var keys string
	switch {
	case m.focusT && rw.sh != nil:
		keys = " ↑↓ pgup pgdn home end scroll   t/esc details   r run again   k kill   q quit"
	case m.focusT:
		keys = " ↑↓ pgup pgdn home end scroll   t/esc details   g open   m message   a attach   r run now   k kill   q quit"
	default:
		keys = " ↑↓ scroll   t transcript   esc back   g open   m message   a attach   r run now   k kill   d drop   q quit"
		if rw.sh != nil {
			keys = " ↑↓ scroll   t output   esc back   r run again   k kill   d drop   q quit"
		}
		if rw.run != nil {
			keys = " ↑↓ scroll   t output   esc back   r run the job again   q quit"
		}
		if len(body) > room {
			keys += fmt.Sprintf("   %d-%d of %d", m.scroll+1, end, len(body))
		}
	}
	out = append(out, m.footer(keys))
	return m.paint(out)
}
