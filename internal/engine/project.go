package engine

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/core"
	"github.com/alsiola/open-supervisor/internal/logx"
	"github.com/alsiola/open-supervisor/internal/playbooks"
	"github.com/alsiola/open-supervisor/internal/problems"
	"github.com/alsiola/open-supervisor/internal/store"
	"github.com/alsiola/open-supervisor/internal/worktree"
)

// hub is what a project needs from the supervisor that holds it.
type hub interface {
	settings() config.Settings
	schedule()
	readUsage()
	capture(name string, props map[string]any)
	changed()
	sessionEnv(p *Project, key string) []string
	dataDir() string
	// fact tells every project something happened that an issue may be waiting on.
	fact(f core.Fact)
	// peer is another project under the same supervisor, nil if there is none by the name.
	peer(name string) *Project
	// upstream is the project that is open-supervisor itself, nil if the supervisor doesn't watch it.
	upstream() *Project
	// limit holds every turn on an agent until a time, after one hit the subscription's usage limit; a zero time
	// lifts the hold.
	limit(agent string, until time.Time)
	// jobsChanged rebuilds the hooks after the events or webhooks a project's jobs take changed, and points the
	// project's again.
	jobsChanged(p *Project)
}

// Project is one project under the supervisor: it claims the issues its source routes to it, gives each a
// session, queues what happens on the tracker and the code host for the agent, and sets the issue's status labels
// from what each turn reports. Its source hands it the tracker's webhooks and runs the reconcile; the
// supervisor hands it the repo's webhooks and decides when a turn may start (one cap across every project).
type Project struct {
	Name      string
	Spec      *config.Project
	Root      string
	StateDir  string
	Tracker   adapter.Tracker
	Repo      adapter.Repo
	Agent     adapter.Agent
	AgentName string
	Labels    config.Labels
	Store     *store.Store
	Jobs      *Jobs

	// repoConn is the name of the repo's connection, for routing webhooks.
	repoConn    string
	fingerprint string
	branchTmpl  string
	branchRe    *regexp.Regexp
	git         config.Git
	base        string
	hub         hub
	// src is the source that feeds the project; a reload can swap it for an equal one.
	src atomic.Pointer[Source]

	mu            sync.Mutex
	running       map[string]adapter.Turn
	sweep         adapter.Turn
	triage        adapter.Turn
	checkTimers   map[string]*time.Timer
	lastReconcile time.Time
	reconciling   bool
	stopped       bool
	// begun: start has run (a reload starts only the projects it built).
	begun bool
}

// source is where the project's issues come from.
func (p *Project) source() *Source { return p.src.Load() }

// Source is where the project's issues come from: its own tracker, or a source with a route to it.
func (p *Project) Source() *Source { return p.source() }

func (p *Project) logf(format string, args ...any) {
	logx.Default.Write(p.Name, filepath.Join(p.StateDir, "log"), format, args...)
}

// BranchFor is the issue's branch.
func (p *Project) BranchFor(key string) string {
	return strings.ReplaceAll(p.branchTmpl, "{key}", key)
}

// issueOfBranch reads the issue key back out of a branch name.
func (p *Project) issueOfBranch(branch string) string {
	if m := p.branchRe.FindStringSubmatch(branch); m != nil {
		return m[1]
	}
	return ""
}

// WorktreePath is where the issue's worktree lives.
func (p *Project) WorktreePath(key string) string {
	return worktree.Dir(p.hub.dataDir(), p.Name, key)
}

// CorrectionsDir is where the agents leave correction notes for the sweep.
func (p *Project) CorrectionsDir() string { return filepath.Join(p.StateDir, "corrections") }

// ProblemsDir is where the agents leave problem notes for the triage.
func (p *Project) ProblemsDir() string { return filepath.Join(p.StateDir, "problems") }

// TriageReport is where the triage writes what it found, when it doesn't open issues.
func (p *Project) TriageReport() string { return filepath.Join(p.StateDir, "triage.md") }

// InflightFile is the courtesy copy of the in-flight list agents can read.
func (p *Project) InflightFile() string { return filepath.Join(p.StateDir, "inflight.md") }

// Running returns the issue's running turn, if any.
func (p *Project) Running(key string) adapter.Turn {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.running[key]
}

// RunningCount is the number of turns this project has going, the sweep and the triage included.
func (p *Project) RunningCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	n := len(p.running)
	if p.sweep != nil {
		n++
	}
	if p.triage != nil {
		n++
	}
	return n
}

// Sweeping reports whether the project's sweep is running.
func (p *Project) Sweeping() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sweep != nil
}

// Triaging reports whether the project's triage is running.
func (p *Project) Triaging() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.triage != nil
}

// Reconciled says when the last reconcile finished and whether one is going.
func (p *Project) Reconciled() (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lastReconcile, p.reconciling
}

func (p *Project) start() {
	p.begun = true
	if err := p.Store.Load(); err != nil {
		p.logf("couldn't read the state file: %v", err)
	}
	p.Store.Prune()
	p.recoverInterrupted()
	issues := p.Tracker.Describe()
	if src := p.source(); src != nil && !src.Spec.Implicit() {
		issues += " through " + src.Name
	}
	p.logf("watching %s (issues) and %s (code) from %s with %s", issues, p.Repo.Describe(), p.Root, p.AgentName)
	p.Jobs.start()
}

func (p *Project) stop() {
	p.mu.Lock()
	p.stopped = true
	for _, t := range p.checkTimers {
		t.Stop()
	}
	chores := []adapter.Turn{p.sweep, p.triage}
	p.mu.Unlock()
	p.Jobs.stop()
	for _, t := range chores {
		if t != nil {
			t.Kill()
		}
	}
}

// recoverInterrupted adopts the turns the last supervisor left running; one with no process to adopt gets a
// note and runs again.
func (p *Project) recoverInterrupted() {
	for _, r := range p.Store.All() {
		switch r.State {
		case store.Running:
			if r.LastTurn != nil && r.LastTurn.Handle != nil && r.LastTurn.Handle.PID > 0 {
				turn := p.Agent.Adopt(*r.LastTurn.Handle, true)
				p.mu.Lock()
				p.running[r.Key] = turn
				p.mu.Unlock()
				p.logf("%s: adopted the running turn (pid %d)", p.ref(r.Key), r.LastTurn.Handle.PID)
				go p.settle(r.Key, r.LastTurn.Events, turn)
				continue
			}
			p.Store.Update(r.Key, func(x *store.Record) {
				x.State = store.Failed
				x.Attempts = 0
				x.RetryAfter = nil
				x.Queue = append([]core.Event{{Type: core.EvNote, Text: "Your previous turn was cut short when the supervisor stopped. Check the issue, the PR and your worktree for where you got to, then carry on.", At: time.Now().UTC()}}, x.Queue...)
			})
		case store.Manual:
			p.Store.Update(r.Key, func(x *store.Record) {
				x.State = x.PreviousState
				if x.State == "" {
					x.State = store.Review
				}
				x.PreviousState = ""
			})
		}
	}
}

// ref is how the log names an issue: #12 for numbered trackers, PROJ-12 for keyed ones.
func (p *Project) ref(key string) string { return refOf(key) }

func ctxTimeout(d time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), d)
}

// ---- signals from webhooks ------------------------------------------------------------------------------

// onSignal takes the repo's signals, and the tracker's about issues the project holds (its source routes the
// rest).
func (p *Project) onSignal(s core.Signal) {
	switch s.Type {
	case core.SigPing:
		p.logf("webhook: hello from %s", s.Key)
	case core.SigIssueReopened:
		key := s.Key
		if s.Issue != nil {
			key = s.Issue.Key
		}
		if r := p.Store.Get(key); r != nil {
			p.Store.Update(r.Key, func(x *store.Record) { x.IssueClosedReported = false })
			p.enqueue(r.Key, core.Event{Type: core.EvIssue, Action: "reopened", At: time.Now().UTC()})
		}
	case core.SigIssueClosed:
		r := p.Store.Get(s.Key)
		if r != nil && !r.IssueClosedReported && r.State != store.Finished {
			p.Store.Update(r.Key, func(x *store.Record) { x.IssueClosedReported = true })
			p.enqueue(r.Key, core.Event{Type: core.EvIssue, Action: "closed", At: time.Now().UTC()})
		}
	case core.SigIssueComment:
		if s.Comment == nil {
			return
		}
		r := p.Store.Get(s.Key)
		if r == nil {
			return
		}
		go p.noteHold(r.Key, s.Comment.ID, s.Comment.Body)
		p.ingest(r, *s.Comment)
	case core.SigPRComment:
		if s.Comment == nil || s.PR == nil {
			return
		}
		r := p.recordForPR(*s.PR)
		if r == nil {
			return
		}
		c := *s.Comment
		c.PR = s.PR.ID
		p.ingest(r, c)
	case core.SigPRChanged:
		if s.PR == nil {
			return
		}
		r := p.recordForPR(*s.PR)
		if r == nil {
			return
		}
		if s.PR.HeadSHA != "" {
			p.Store.Update(r.Key, func(x *store.Record) {
				if x.HeadSHA != s.PR.HeadSHA {
					now := time.Now().UTC()
					x.HeadSHA = s.PR.HeadSHA
					x.HeadAt = &now
					x.ChecksReported = ""
				}
			})
			p.scheduleChecks(s.PR.HeadSHA)
		}
	case core.SigPRClosed:
		if s.PR == nil {
			return
		}
		go p.hub.fact(core.Fact{Kind: core.CondPR, Project: p.Name, Ref: strconv.Itoa(s.PR.ID), On: string(s.PR.State)})
		if r := p.recordForPR(*s.PR); r != nil {
			p.reportPRClosed(r.Key, s.PR.ID, s.PR.State == core.PRMerged)
		}
	case core.SigChecks:
		if s.SHA != "" {
			p.scheduleChecks(s.SHA)
		}
	}
}

// recordForPR finds the issue a PR belongs to: one already known, its branch, or an issue its body refers to.
func (p *Project) recordForPR(pr core.PR) *store.Record {
	if r := p.Store.ByPR(pr.ID); r != nil {
		return r
	}
	key := p.issueOfPR(pr)
	if key == "" {
		return nil
	}
	return p.Store.Update(key, func(x *store.Record) { x.AddPR(pr.ID) })
}

// issueOfPR is the tracked issue a PR is for, if any.
func (p *Project) issueOfPR(pr core.PR) string {
	if key := p.issueOfBranch(pr.Branch); key != "" && p.Store.Get(key) != nil {
		return key
	}
	for _, key := range p.Tracker.RefsIn(pr.Title + "\n" + pr.Body) {
		if p.Store.Get(key) != nil {
			return key
		}
	}
	return ""
}

func (p *Project) reportPRClosed(key string, pr int, merged bool) {
	already := false
	p.Store.Update(key, func(x *store.Record) {
		for _, c := range x.ClosedPRs {
			if c == pr {
				already = true
				return
			}
		}
		x.ClosedPRs = append(x.ClosedPRs, pr)
	})
	if already {
		return
	}
	action := "closed"
	if merged {
		action = "merged"
	}
	p.enqueue(key, core.Event{Type: core.EvPR, PR: pr, Action: action, At: time.Now().UTC()})
}

// ---- comments -------------------------------------------------------------------------------------------

// ingest hands a comment to the agent unless it is a bot's, the agent's own or already seen, and
// acknowledges it where the service allows.
func (p *Project) ingest(r *store.Record, c core.Comment) bool {
	return p.ingestOpt(r, c, true)
}

// ingestOpt is ingest; schedule false queues the comment without starting a turn, so a batch read in one go
// becomes one turn.
func (p *Project) ingestOpt(r *store.Record, c core.Comment, schedule bool) bool {
	// The mark comes first: agents post through a bot app when one is set up, and another agent's comment
	// still has to reach this one.
	if c.Agent != nil {
		if c.Agent.From == "" || c.Agent.From == r.Key {
			return false
		}
	} else if c.Bot {
		return false
	}
	fresh := false
	p.Store.Update(r.Key, func(x *store.Record) {
		if x.HasSeen(c.ID) {
			return
		}
		fresh = true
		x.Seen = append(x.Seen, c.ID)
		if c.At.After(x.Since) {
			x.Since = c.At
		}
	})
	if !fresh {
		return false
	}
	cc := c
	p.enqueueOpt(r.Key, core.Event{Type: core.EvComment, Comment: &cc, At: c.At}, schedule)
	if p.hub.settings().DryRun {
		return true
	}
	var reactor adapter.Reactor
	if c.Kind == core.OnIssue {
		reactor, _ = p.Tracker.(adapter.Reactor)
	} else {
		reactor, _ = p.Repo.(adapter.Reactor)
	}
	if reactor != nil {
		go func() {
			ctx, cancel := ctxTimeout(30 * time.Second)
			defer cancel()
			if err := reactor.React(ctx, c); err != nil {
				p.logf("%s: couldn't acknowledge %s: %s", p.ref(r.Key), c.URL, logx.ErrText(err))
			}
		}()
	}
	return true
}

// enqueue queues an event for the issue's next turn, or hands it to the running turn when it is one people
// said and the turn can take it.
func (p *Project) enqueue(key string, e core.Event) { p.enqueueOpt(key, e, true) }

func (p *Project) enqueueOpt(key string, e core.Event, schedule bool) {
	r := p.Store.Update(key, func(x *store.Record) { x.Queue = append(x.Queue, e) })
	if r == nil {
		return
	}
	if r.State == store.Running && e.Interjects() && p.interject(key, e) {
		return
	}
	suffix := ""
	switch r.State {
	case store.Running:
		suffix = " (queued; a turn is running)"
	case store.Manual:
		suffix = " (queued; you are attached)"
	}
	p.logf("%s: %s%s", p.ref(key), e.Describe(), suffix)
	if schedule {
		p.hub.schedule()
	}
}

// interject hands a queued event to the running turn. It stays in the queue until the agent takes it in, so
// one the turn ends without seeing (or a supervisor restart) leaves it for the next turn.
func (p *Project) interject(key string, e core.Event) bool {
	turn := p.Running(key)
	if turn == nil {
		return false
	}
	sent := turn.Send(core.RenderInterjection(e), func(taken bool) {
		if !taken {
			p.logf("%s: %s wasn't taken in before the turn ended; queued for the next one", p.ref(key), e.Describe())
			return
		}
		p.Store.Update(key, func(x *store.Record) {
			for i, q := range x.Queue {
				if q.Same(e) {
					x.Queue = append(x.Queue[:i], x.Queue[i+1:]...)
					break
				}
			}
			if x.LastTurn != nil {
				x.LastTurn.Events = append(x.LastTurn.Events, e)
			}
		})
		p.logf("%s: the running turn took in the %s", p.ref(key), e.Describe())
	})
	if sent {
		p.logf("%s: %s (sent to the running turn)", p.ref(key), e.Describe())
	}
	return sent
}

// Message is a message typed into the TUI: into the running turn if there is one, else queued for a turn now.
func (p *Project) Message(key, text string) {
	if p.Store.Get(key) == nil || strings.TrimSpace(text) == "" {
		return
	}
	p.enqueue(key, core.Event{Type: core.EvMessage, Text: text, At: time.Now().UTC()})
}

// ---- claiming -------------------------------------------------------------------------------------------

// maybeClaim claims an open issue nobody has: label it and give it a session. reopened lets a labelled,
// untracked issue through.
func (p *Project) maybeClaim(issue core.Issue, reopened bool) {
	src := p.source()
	src.claimMu.Lock()
	defer src.claimMu.Unlock()
	if !issue.Open || issue.Waiting || src.Holder(issue.Key) != nil {
		return
	}
	if !p.Labels.Admits(issue.HasLabel) {
		return
	}
	if issue.HasLabel(p.Labels.Claimed) && !reopened {
		return
	}
	// A scheduled issue with a hold marker waits for it; one without is a timer issue to run now. An issue
	// whose body carries a marker waits for it too, labelled or not: it may have been written by hand.
	var marker *holdMarker
	labelled := issue.HasLabel(p.Labels.Scheduled)
	if _, inBody := core.HoldIn(issue.Body); labelled || inBody {
		m, err := p.latestHold(issue.Key)
		if err != nil {
			p.logf("%s: couldn't read its hold: %s", p.ref(issue.Key), logx.ErrText(err))
			return
		}
		marker = m
	}
	if marker != nil && waiting(marker.hold) {
		p.Store.Create(issue.Key, issue.Title, issue.URL, p.Agent.NewSession())
		p.Store.Update(issue.Key, func(x *store.Record) {
			x.State = store.Scheduled
			setHold(x, marker.hold, marker.commentID)
		})
		p.logf("%s: held for %s", p.ref(issue.Key), marker.hold.Describe())
		p.warnUnknown(marker.hold)
		if !labelled {
			p.editLabels(issue.Key, []string{p.Labels.Scheduled}, nil)
		}
		p.hub.changed()
		if len(marker.hold.Conds) > 0 {
			go p.checkWaits(issue.Key)
		}
		return
	}
	if len(p.Store.Active()) >= p.hub.settings().MaxOpen {
		p.logf("%s: holding; %d issues already claimed", p.ref(issue.Key), p.hub.settings().MaxOpen)
		return
	}
	note := ""
	if marker != nil && marker.hold.Until != nil {
		note = fmt.Sprintf("This issue was scheduled to start at %s, and that time has come.", marker.hold.Until.Format(time.RFC3339))
	}
	p.claimLocked(issue, note)
	if labelled {
		p.editLabels(issue.Key, nil, []string{p.Labels.Scheduled})
	}
}

// claimLocked claims an issue; the caller holds claimMu.
func (p *Project) claimLocked(issue core.Issue, note string) {
	if p.Store.Get(issue.Key) != nil {
		return
	}
	p.Store.Create(issue.Key, issue.Title, issue.URL, p.Agent.NewSession())
	if note != "" {
		p.Store.Update(issue.Key, func(x *store.Record) {
			x.Queue = append(x.Queue, core.Event{Type: core.EvNote, Text: note, At: time.Now().UTC()})
		})
	}
	var remove []string
	if p.Labels.Unrouted != "" && issue.HasLabel(p.Labels.Unrouted) {
		remove = append(remove, p.Labels.Unrouted)
	}
	p.editLabels(issue.Key, []string{p.Labels.Claimed}, remove)
	p.transition(issue.Key, "claimed")
	p.logf("claimed %s %s", p.ref(issue.Key), issue.Title)
	p.hub.schedule()
}

// ClaimNow claims by hand, labels and routes or not: a backlog issue the owner wants done now, or a run of one
// already tracked.
func (p *Project) ClaimNow(key string) error {
	if p.Store.Get(key) != nil {
		p.RunNow(key)
		return nil
	}
	src := p.source()
	if h := src.Holder(key); h != nil {
		return fmt.Errorf("%s has it", h.Name)
	}
	ctx, cancel := ctxTimeout(time.Minute)
	defer cancel()
	issue, err := p.Tracker.Issue(ctx, key)
	if err != nil {
		return err
	}
	src.claimMu.Lock()
	defer src.claimMu.Unlock()
	if h := src.Holder(key); h != nil {
		return fmt.Errorf("%s has it", h.Name)
	}
	src.forget(issue.Key)
	p.claimLocked(issue, "")
	return nil
}

func (p *Project) editLabels(key string, add, remove []string) {
	if p.hub.settings().DryRun {
		return
	}
	add, remove = nonEmpty(add), nonEmpty(remove)
	if len(add) == 0 && len(remove) == 0 {
		return
	}
	ctx, cancel := ctxTimeout(time.Minute)
	defer cancel()
	if err := p.Tracker.EditLabels(ctx, key, add, remove); err != nil {
		p.logf("%s: couldn't change the labels: %s", p.ref(key), logx.ErrText(err))
	}
}

func (p *Project) transition(key, state string) {
	t, ok := p.Tracker.(adapter.Transitioner)
	if !ok || p.hub.settings().DryRun {
		return
	}
	ctx, cancel := ctxTimeout(time.Minute)
	defer cancel()
	if err := t.Transition(ctx, key, state); err != nil {
		p.logf("%s: couldn't move it to its %s status: %s", p.ref(key), state, logx.ErrText(err))
	}
}

// setStatus puts the status label for the state on the issue and takes the others off.
func (p *Project) setStatus(key string, state string) {
	want := p.Labels.Status(state)
	var add, remove []string
	for _, l := range p.Labels.Statuses() {
		if l == want {
			add = append(add, l)
		} else {
			remove = append(remove, l)
		}
	}
	p.editLabels(key, add, remove)
}

func nonEmpty(xs []string) []string {
	var out []string
	for _, x := range xs {
		if x != "" {
			out = append(out, x)
		}
	}
	return out
}

// RunNow runs a turn on a tracked issue now, with a note if nothing is queued. It also retries a failed one and
// wakes a scheduled one.
func (p *Project) RunNow(key string) {
	r := p.Store.Get(key)
	if r == nil || r.State == store.Running || r.State == store.Manual {
		return
	}
	if r.State == store.Scheduled {
		go func() {
			p.endHold(key, "The owner asked for a turn now, before what the issue was waiting for. Check where it has got to and carry on, or wait again.", "owner", "")
			p.hub.schedule()
		}()
		return
	}
	// The owner asking for a turn lifts a usage-limit hold too: they may know the limit has gone.
	p.hub.limit(p.AgentName, time.Time{})
	p.Store.Update(key, func(x *store.Record) {
		x.Attempts = 0
		x.RetryAfter = nil
		if x.State == store.Failed {
			if x.Turns > 0 {
				x.State = store.Review
			} else {
				x.State = store.Queued
			}
		}
		if len(x.Queue) == 0 && x.Turns > 0 {
			x.Queue = append(x.Queue, core.Event{Type: core.EvNote, Text: "The owner asked for a turn now. Check the issue and the PR for anything outstanding and carry on.", At: time.Now().UTC()})
		}
	})
	p.hub.schedule()
}

// Drop forgets an issue: kills its turn, drops its record and clears its status labels. The claimed label
// stays, so it isn't claimed again unless reopened or claimed by hand.
func (p *Project) Drop(key string) {
	r := p.Store.Get(key)
	if r == nil {
		return
	}
	if t := p.Running(key); t != nil {
		t.Kill()
	}
	p.Store.Remove(key)
	if r.Trial != nil {
		p.endTrial(key, r.Trial, "dropped", string(r.State))
	}
	p.logf("%s: dropped from the list", p.ref(key))
	go p.setStatus(key, "")
	p.hub.changed()
}

// KillTurn stops the issue's running turn; it is marked failed and RunNow reruns it.
func (p *Project) KillTurn(key string) {
	if t := p.Running(key); t != nil {
		p.logf("%s: killing the running turn", p.ref(key))
		t.Kill()
	}
}

// ---- turns ----------------------------------------------------------------------------------------------

func (p *Project) runnable(r *store.Record) bool {
	s := p.hub.settings()
	switch {
	case r.State == store.Running || r.State == store.Manual:
		return false
	case r.State == store.Scheduled && r.Turns == 0:
		return false
	case r.State == store.Failed && r.Attempts >= s.MaxAttempts:
		return false
	case r.State == store.Failed && r.RetryAfter != nil && time.Now().Before(*r.RetryAfter):
		return false
	}
	return r.State == store.Queued || len(r.Queue) > 0
}

// candidates are the issues with a turn to run: the ones already under way first, then the oldest claim.
func (p *Project) candidates() []*store.Record {
	var out []*store.Record
	for _, r := range p.Store.All() {
		if p.runnable(r) && !p.reserved(r.Key) {
			out = append(out, r)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i].Turns == 0, out[j].Turns == 0
		if a == b {
			return out[i].ClaimedAt.Before(out[j].ClaimedAt)
		}
		return !a
	})
	return out
}

// inflight is the list of the project's other issues in flight, with what each touches.
func (p *Project) inflight(except string) string {
	var all, others []string
	for _, r := range p.Store.Active() {
		touches := r.Touches
		if touches == "" {
			touches = "not reported yet"
		}
		line := fmt.Sprintf("- %s %s: %s", p.ref(r.Key), r.Title, touches)
		all = append(all, line)
		if r.Key != except {
			others = append(others, line)
		}
	}
	_ = os.MkdirAll(p.StateDir, 0o755)
	_ = os.WriteFile(p.InflightFile(), []byte(strings.Join(all, "\n")+"\n"), 0o644)
	return strings.Join(others, "\n")
}

func (p *Project) turnBody(r *store.Record, events []core.Event, handoff bool) (args, body string) {
	if handoff {
		return r.Key + " handoff", "## Handoff from your last session\n\n" + strings.TrimSpace(r.Handoff) +
			"\n\n## Since your last turn\n\n" + core.Render(events)
	}
	if r.Turns == 0 {
		var b strings.Builder
		if in := p.inflight(r.Key); in != "" {
			b.WriteString("In flight (other agents' issues; before you build, check whether any overlaps yours):\n" + in)
		}
		if len(events) > 0 {
			if b.Len() > 0 {
				b.WriteString("\n\n")
			}
			b.WriteString("## Already waiting\n\n" + core.Render(events))
		}
		return r.Key, b.String()
	}
	return r.Key + " events", "## Since your last turn\n\n" + core.Render(events)
}

// reserved reports whether the issue has a turn running or about to start.
func (p *Project) reserved(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, ok := p.running[key]
	return ok
}

// tryStart reserves the issue's slot in the running map so two schedulers never start the same turn.
func (p *Project) tryStart(key string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return false
	}
	if _, ok := p.running[key]; ok {
		return false
	}
	p.running[key] = nil
	return true
}

// runTurn starts a turn on the issue and waits for it. The caller has reserved the slot with tryStart.
func (p *Project) runTurn(key string) {
	r := p.Store.Get(key)
	if r == nil {
		p.mu.Lock()
		delete(p.running, key)
		p.mu.Unlock()
		return
	}
	var events []core.Event
	p.Store.Update(key, func(x *store.Record) {
		events = x.Queue
		x.Queue = []core.Event{}
	})
	kind := "events"
	if r.Turns == 0 {
		kind = "start"
	}
	// A session that has run before is resumed, even when its first turn never ended. A cold one may instead
	// hand off to a fresh session that starts from the last turn's note.
	session := r.Session
	resume := p.Agent.SessionExists(p.Root, session)
	handoff, arm := p.handsOff(r, resume)
	if handoff && resume {
		session, resume = p.Agent.NewSession(), false
	}
	// A resumed turn works in the worktree without running its setup again, so bring the copied files (.env)
	// up to date with the main checkout first.
	worktree.Refresh(worktree.Spec{Root: p.Root, Path: p.WorktreePath(key), Copy: p.Spec.Worktree.Copy})
	args, body := p.turnBody(r, events, handoff)
	body = withExtra(body, playbooks.Project(p.Root, "work-issue"))
	ctx := context.Background()
	turn, err := p.Agent.Start(ctx, adapter.TurnSpec{
		Cwd:        p.Root,
		Env:        p.hub.sessionEnv(p, key),
		Dir:        filepath.Join(p.StateDir, "turns", safeName(key)),
		Playbook:   "work-issue",
		Args:       args,
		Body:       body,
		Session:    session,
		Resume:     resume,
		Structured: true,
		DryRun:     p.hub.settings().DryRun,
	})
	if err != nil {
		p.mu.Lock()
		delete(p.running, key)
		p.mu.Unlock()
		p.Store.Update(key, func(x *store.Record) {
			x.Queue = append(events, x.Queue...)
		})
		p.failed(key, events, core.Outcome{Error: "couldn't start the agent: " + err.Error()})
		return
	}
	h := turn.Handle()
	var ended *store.Trial
	p.Store.Update(key, func(x *store.Record) {
		if session != x.Session {
			x.PastSessions = append(x.PastSessions, x.Session)
			x.Session = session
		}
		if arm != "" {
			ended = x.Trial
			x.Trial = &store.Trial{ID: fmt.Sprintf("%s-%d", key, x.Turns+1), Arm: arm, StartedAt: h.StartedAt}
		}
		x.State = store.Running
		x.LastTurn = &store.Turn{Kind: kind, StartedAt: h.StartedAt, Events: events, Handle: &h, Arm: arm, Handoff: handoff}
	})
	p.mu.Lock()
	p.running[key] = turn
	p.mu.Unlock()
	p.hub.changed()
	// The agent is at work again, so nothing waits on the owner until the turn ends.
	if r.State == store.Failed || p.Labels.Status(string(r.State)) != "" {
		p.setStatus(key, "")
	}
	if ended != nil {
		p.endTrial(key, ended, "next_draw", "")
	}
	if arm != "" {
		props := p.trialProps(key, p.Store.Get(key).Trial)
		props["session"] = session
		p.hub.capture("handoff_assigned", props)
	}
	desc := make([]string, len(events))
	for i, e := range events {
		desc[i] = e.Describe()
	}
	detail := kind
	switch {
	case handoff:
		detail = "handoff to a fresh session"
	case arm != "":
		detail += ", resumed cold"
	}
	if len(desc) > 0 {
		detail += ": " + strings.Join(desc, ", ")
	}
	p.logf("%s: turn %d started (%s)", p.ref(key), r.Turns+1, detail)
	p.settle(key, events, turn)
}

// handsOff reports whether the issue's next turn starts a fresh session from the last turn's handoff note. A
// session that never got as far as being written always does. One whose last turn ended more than
// handoffMinutes ago, so its prompt cache has expired and resuming would rebuild the whole context first, is in
// the experiment: a draw sends handoffShare of them to a fresh session and resumes the rest, and arm says which.
func (p *Project) handsOff(r *store.Record, resume bool) (handoff bool, arm string) {
	if r.Turns == 0 || strings.TrimSpace(r.Handoff) == "" {
		return false, ""
	}
	if !resume {
		return true, ""
	}
	st := p.hub.settings()
	if st.HandoffShare == nil || *st.HandoffShare <= 0 || st.HandoffMinutes == nil || r.LastTurn == nil || r.LastTurn.EndedAt == nil {
		return false, ""
	}
	if time.Since(*r.LastTurn.EndedAt) <= time.Duration(*st.HandoffMinutes)*time.Minute {
		return false, ""
	}
	if rand.Float64() < *st.HandoffShare {
		return true, "handoff"
	}
	return false, "resume"
}

// feedbackText is the comment after a turn drawn into the handoff experiment. It reads the same whichever arm the
// draw picked, so whoever answers doesn't know if the session resumed or started afresh from a handoff.
const feedbackText = "There was a long gap between messages, did you find performance was maintained from previous turns? " +
	"Hit 👍 on this comment if it was, or 👎 if it got worse."

// askFeedback asks on the issue whether the session that just took a turn drawn into the handoff experiment,
// on either arm, kept up its performance, where the tracker can count the 👍 and 👎, and watches the comment.
func (p *Project) askFeedback(key string) {
	_, ok := p.Tracker.(adapter.ReactionReader)
	r := p.Store.Get(key)
	if !ok || r == nil || r.LastTurn == nil || r.LastTurn.Arm == "" || p.hub.settings().DryRun {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	url, err := p.Tracker.Comment(ctx, key, feedbackText, core.AgentMark{From: key})
	if err != nil {
		p.logf("%s: couldn't ask for feedback on the cold turn: %s", p.ref(key), logx.ErrText(err))
		return
	}
	f := store.Feedback{URL: url, Session: r.Session, Arm: r.LastTurn.Arm, Turn: r.Turns, PostedAt: time.Now().UTC()}
	if r.Trial != nil {
		f.Trial = r.Trial.ID
	}
	p.Store.Update(key, func(x *store.Record) { x.Feedback = append(x.Feedback, f) })
	p.hub.capture("handoff_feedback_asked", p.feedbackProps(key, f))
}

// feedbackWindow is how long a feedback comment is watched for a 👍 or 👎.
const feedbackWindow = 7 * 24 * time.Hour

// readFeedback looks for a 👍 or 👎 on the issue's feedback comments, sends an event for each comment that has
// one, and stops watching those and the ones past the window.
func (p *Project) readFeedback(ctx context.Context, r *store.Record) {
	rr, ok := p.Tracker.(adapter.ReactionReader)
	if !ok || len(r.Feedback) == 0 {
		return
	}
	done := map[string]bool{}
	for _, f := range r.Feedback {
		if time.Since(f.PostedAt) > feedbackWindow {
			done[f.URL] = true
			continue
		}
		counts, err := rr.Reactions(ctx, f.URL)
		if err != nil {
			p.logf("%s: couldn't read the reactions on the cold turn's feedback: %s", p.ref(r.Key), logx.ErrText(err))
			continue
		}
		up, down := counts["+1"], counts["-1"]
		if up == 0 && down == 0 {
			continue
		}
		props := p.feedbackProps(r.Key, f)
		props["thumbs_up"] = up
		props["thumbs_down"] = down
		props["verdict"] = verdict(up, down)
		props["hours_after"] = time.Since(f.PostedAt).Hours()
		p.hub.capture("handoff_feedback", props)
		p.logf("%s: feedback on the cold turn in session %s: %s", p.ref(r.Key), f.Session, props["verdict"])
		done[f.URL] = true
	}
	if len(done) == 0 {
		return
	}
	p.Store.Update(r.Key, func(x *store.Record) {
		var keep []store.Feedback
		for _, f := range x.Feedback {
			if !done[f.URL] {
				keep = append(keep, f)
			}
		}
		x.Feedback = keep
	})
}

// verdict is what a feedback comment's reactions say: "maintained" on a 👍, "worse" on a 👎, "mixed" on both.
func verdict(up, down int) string {
	switch {
	case up > 0 && down > 0:
		return "mixed"
	case down > 0:
		return "worse"
	default:
		return "maintained"
	}
}

// endTrial sends what a trial added up to: endedBy is "finished" when the issue got there, "next_draw" when
// another cold turn started a new trial first, or "dropped" when the owner dropped the issue.
func (p *Project) endTrial(key string, t *store.Trial, endedBy, state string) {
	props := p.trialProps(key, t)
	props["ended_by"] = endedBy
	props["completed"] = endedBy == "finished"
	props["final_state"] = state
	props["turns"] = t.Turns
	props["failed_turns"] = t.Failed
	props["cost_usd"] = t.Cost
	props["model_calls"] = t.ModelCalls
	props["tool_calls"] = t.ToolCalls
	props["agent_seconds"] = int(t.AgentSeconds)
	props["elapsed_hours"] = time.Since(t.StartedAt).Hours()
	props["follow_ups"] = t.FollowUps
	if t.Tokens != nil {
		props["input_tokens"] = t.Tokens.Input
		props["output_tokens"] = t.Tokens.Output
		props["cache_read_tokens"] = t.Tokens.CacheRead
		props["cache_creation_tokens"] = t.Tokens.CacheCreation
	}
	p.hub.capture("handoff_trial", props)
}

func (p *Project) trialProps(key string, t *store.Trial) map[string]any {
	return map[string]any{"project": p.Name, "issue": key, "agent": p.AgentName, "trial": t.ID, "arm": t.Arm}
}

func (p *Project) feedbackProps(key string, f store.Feedback) map[string]any {
	return map[string]any{
		"project": p.Name,
		"issue":   key,
		"agent":   p.AgentName,
		"session": f.Session,
		"arm":     f.Arm,
		"trial":   f.Trial,
		"turn":    f.Turn,
		"comment": f.URL,
	}
}

var unsafeName = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

func safeName(s string) string { return unsafeName.ReplaceAllString(s, "-") }

// settle waits for a turn and acts on how it ended.
func (p *Project) settle(key string, events []core.Event, turn adapter.Turn) {
	<-turn.Done()
	outcome := turn.Outcome()
	p.mu.Lock()
	delete(p.running, key)
	stopped := p.stopped
	p.mu.Unlock()
	if stopped {
		// A reload replaced this project; the new one adopts the turn's result.
		return
	}
	if !outcome.OK || outcome.Status == nil {
		p.failed(key, events, outcome)
		return
	}
	s := outcome.Status
	now := time.Now().UTC()
	p.Store.Update(key, func(x *store.Record) {
		x.State = store.State(s.State)
		if s.State == core.Finished {
			// Nothing is left to wait for.
			x.RunAt, x.Waits, x.WaitSince = nil, nil, nil
		}
		x.Turns++
		x.Attempts = 0
		x.RetryAfter = nil
		x.Cost += outcome.Cost
		x.Tokens = x.Tokens.Add(outcome.Tokens)
		if s.Touches != "" {
			x.Touches = s.Touches
		}
		x.Handoff = s.Handoff
		if s.Preview != "" {
			x.Preview = s.Preview
		}
		for _, pr := range s.PRs {
			x.AddPR(pr)
		}
		if x.LastTurn != nil {
			x.LastTurn.EndedAt = &now
			x.LastTurn.Summary = s.Summary
			x.LastTurn.Cost = outcome.Cost
			x.LastTurn.Tokens = outcome.Tokens
			x.LastTurn.Models = outcome.Models
			x.LastTurn.Handle = nil
		}
		if x.Trial != nil {
			followUps := 0
			if x.LastTurn != nil {
				for _, e := range x.LastTurn.Events {
					if e.Interjects() {
						followUps++
					}
				}
			}
			x.Trial.Add(outcome, followUps)
		}
	})
	p.logf("%s: turn ended %s after %.1f min: %s", p.ref(key), s.State, outcome.Duration.Minutes(), s.Summary)
	p.report(key, outcome)
	p.askFeedback(key)
	if s.State == core.Finished {
		var done *store.Trial
		p.Store.Update(key, func(x *store.Record) { done, x.Trial = x.Trial, nil })
		if done != nil {
			p.endTrial(key, done, "finished", string(s.State))
		}
	}
	if s.State != core.Finished && p.hold(key) {
		if r := p.Store.Get(key); r != nil {
			p.logf("%s: waiting for %s", p.ref(key), holdText(r))
		}
	} else {
		p.applyState(key, s.State)
	}
	go p.hub.fact(core.Fact{Kind: core.CondIssue, Project: p.Name, Ref: key, On: string(s.State)})
	p.hub.schedule()
}

const (
	// limitGrace is how long after the usage limit lifts a turn it stopped runs again.
	limitGrace = 2 * time.Minute
	// limitWait is how long to wait when claude didn't say when the limit lifts.
	limitWait = 30 * time.Minute
)

func (p *Project) failed(key string, events []core.Event, outcome core.Outcome) {
	now := time.Now().UTC()
	errText := outcome.Error
	if errText == "" {
		errText = "the turn ended without a status"
	}
	// A turn the usage limit stopped isn't the issue's fault: it doesn't count as an attempt, and it runs again
	// shortly after the limit lifts.
	var limitRetry time.Time
	if outcome.Limited {
		limitRetry = now.Add(limitWait)
		if outcome.ResetsAt != nil {
			limitRetry = outcome.ResetsAt.Add(limitGrace)
		}
	}
	r := p.Store.Update(key, func(x *store.Record) {
		x.State = store.Failed
		retry := limitRetry
		if !outcome.Limited {
			x.Attempts++
			retry = now.Add(time.Duration(x.Attempts*5) * time.Minute)
		}
		x.RetryAfter = &retry
		x.Cost += outcome.Cost
		x.Tokens = x.Tokens.Add(outcome.Tokens)
		// Events taken in mid-turn are in LastTurn.Events too.
		back := events
		if x.LastTurn != nil && x.LastTurn.Events != nil {
			back = x.LastTurn.Events
		}
		x.Queue = append(append([]core.Event{}, back...), x.Queue...)
		if x.LastTurn != nil {
			x.LastTurn.EndedAt = &now
			x.LastTurn.Error = errText
			x.LastTurn.Cost = outcome.Cost
			x.LastTurn.Tokens = outcome.Tokens
			x.LastTurn.Models = outcome.Models
			x.LastTurn.Handle = nil
		}
		// A limit hit says nothing about the arm, so it isn't counted against it.
		if x.Trial != nil && !outcome.Limited {
			x.Trial.Add(outcome, 0)
		}
	})
	p.report(key, outcome)
	retry := " (press r to retry)"
	switch {
	case outcome.Limited:
		retry = " (retry at " + core.Clock(limitRetry) + ")"
		p.hub.limit(p.AgentName, limitRetry)
	case r != nil && r.Attempts < p.hub.settings().MaxAttempts:
		retry = fmt.Sprintf(" (retry in %d min)", r.Attempts*5)
	}
	p.logf("%s: turn failed after %.1f min: %s%s", p.ref(key), outcome.Duration.Minutes(), errText, retry)
	p.hub.schedule()
}

// applyState sets the labels and does the follow-ups for the state a turn ended in.
func (p *Project) applyState(key string, state core.AgentState) {
	p.setStatus(key, string(state))
	p.transition(key, string(state))
	switch state {
	case core.Building:
		// The checks may already have landed while the agent was finishing up.
		p.refreshPRs(key)
		if r := p.Store.Get(key); r != nil && r.HeadSHA != "" {
			p.evaluateChecks(r.HeadSHA)
		}
	case core.Finished:
		path := p.WorktreePath(key)
		if _, err := os.Stat(path); err == nil {
			if err := worktree.Remove(worktree.Spec{Root: p.Root, Path: path}); err != nil {
				p.logf("%s: couldn't remove the worktree: %s", p.ref(key), logx.ErrText(err))
			} else {
				p.logf("%s: removed the worktree", p.ref(key))
			}
		}
	}
}

// report sends one telemetry event per turn, then asks for a fresh read of the usage windows.
func (p *Project) report(key string, o core.Outcome) {
	r := p.Store.Get(key)
	props := map[string]any{
		"project":     p.Name,
		"repo":        p.Repo.Describe(),
		"tracker":     p.Tracker.Describe(),
		"source":      p.source().Name,
		"agent":       p.AgentName,
		"issue":       key,
		"ok":          o.OK,
		"error":       o.Error,
		"duration_s":  int(o.Duration.Seconds()),
		"cost_usd":    o.Cost,
		"models":      o.Models,
		"model_calls": o.ModelCalls,
		"tool_calls":  o.ToolCalls,
	}
	if o.Status != nil {
		props["state"] = string(o.Status.State)
	} else if !o.OK {
		props["state"] = "failed"
	}
	if o.Tokens != nil {
		props["input_tokens"] = o.Tokens.Input
		props["output_tokens"] = o.Tokens.Output
		props["cache_read_tokens"] = o.Tokens.CacheRead
		props["cache_creation_tokens"] = o.Tokens.CacheCreation
	}
	if r != nil {
		props["title"] = r.Title
		props["turn"] = r.Turns
		props["session"] = r.Session
		if r.Trial != nil {
			props["trial"] = r.Trial.ID
			props["trial_arm"] = r.Trial.Arm
		}
		if r.LastTurn != nil {
			props["kind"] = r.LastTurn.Kind
			props["handoff"] = r.LastTurn.Handoff
			if r.LastTurn.Arm != "" {
				props["handoff_arm"] = r.LastTurn.Arm
			}
			types := make([]string, len(r.LastTurn.Events))
			for i, e := range r.LastTurn.Events {
				types[i] = string(e.Type)
			}
			props["events"] = types
		}
	}
	p.hub.capture("agent_turn", props)
	go p.hub.readUsage()
}

// ---- checks ---------------------------------------------------------------------------------------------

// scheduleChecks judges a commit's checks a little after the last event about it, so one push gives one event.
func (p *Project) scheduleChecks(sha string) {
	if p.Store.BySHA(sha) == nil {
		return
	}
	delay := time.Duration(p.hub.settings().ChecksDebounceSeconds) * time.Second
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.stopped {
		return
	}
	if t := p.checkTimers[sha]; t != nil {
		t.Stop()
	}
	p.checkTimers[sha] = time.AfterFunc(delay, func() {
		p.mu.Lock()
		delete(p.checkTimers, sha)
		p.mu.Unlock()
		p.evaluateChecks(sha)
	})
}

func (p *Project) evaluateChecks(sha string) {
	r := p.Store.BySHA(sha)
	if r == nil || store.SameSHA(r.ChecksReported, sha) || r.State != store.Building {
		return
	}
	ctx, cancel := ctxTimeout(time.Minute)
	defer cancel()
	result, err := p.Repo.Checks(ctx, sha)
	if err != nil {
		p.logf("%s: couldn't read the checks on %s: %s", p.ref(r.Key), sha[:min(7, len(sha))], logx.ErrText(err))
		return
	}
	headAt := time.Now()
	if r.HeadAt != nil {
		headAt = *r.HeadAt
	} else {
		p.Store.Update(r.Key, func(x *store.Record) {
			now := time.Now().UTC()
			x.HeadAt = &now
		})
	}
	// A repo with no CI never reports anything: after a while, say so rather than wait.
	none := !result.Any && time.Since(headAt) > time.Duration(p.hub.settings().NoChecksMinutes)*time.Minute
	if !result.Complete && !none {
		return
	}
	pr := 0
	if len(r.PRs) > 0 {
		pr = r.PRs[len(r.PRs)-1]
	}
	sha = r.HeadSHA
	p.Store.Update(r.Key, func(x *store.Record) {
		x.ChecksReported = sha
		if result.Preview != "" {
			x.Preview = result.Preview
		}
	})
	p.enqueue(r.Key, core.Event{Type: core.EvChecks, PR: pr, SHA: sha, Passed: len(result.Failed) == 0, Failed: result.Failed, Preview: result.Preview, None: none, At: time.Now().UTC()})
}

// refreshPRs learns an issue's PRs and the head of its open one.
func (p *Project) refreshPRs(key string) {
	r := p.Store.Get(key)
	if r == nil {
		return
	}
	ctx, cancel := ctxTimeout(2 * time.Minute)
	defer cancel()
	found, err := p.Repo.PRsFor(ctx, p.BranchFor(key), p.Tracker.RefFor(key))
	if err != nil {
		p.logf("%s: couldn't read its PRs: %s", p.ref(key), logx.ErrText(err))
		return
	}
	views := map[int]core.PR{}
	for _, pr := range found {
		if p.issueOfBranch(pr.Branch) == key || containsKey(p.Tracker.RefsIn(pr.Title+"\n"+pr.Body), key) {
			views[pr.ID] = pr
		}
	}
	for _, id := range r.PRs {
		if _, ok := views[id]; ok {
			continue
		}
		pr, err := p.Repo.PR(ctx, id)
		if err != nil {
			p.logf("%s: couldn't read PR #%d: %s", p.ref(key), id, logx.ErrText(err))
			continue
		}
		views[id] = pr
	}
	ids := make([]int, 0, len(views))
	for id := range views {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	var open *core.PR
	for _, id := range ids {
		if pr := views[id]; pr.State == core.PROpen {
			open = &pr
		}
	}
	p.Store.Update(key, func(x *store.Record) {
		for _, id := range ids {
			x.AddPR(id)
		}
		if open != nil && x.HeadSHA != open.HeadSHA {
			now := time.Now().UTC()
			x.HeadSHA = open.HeadSHA
			x.HeadAt = &now
			x.ChecksReported = ""
		}
	})
	for _, id := range ids {
		if pr := views[id]; pr.State != core.PROpen {
			p.reportPRClosed(key, id, pr.State == core.PRMerged)
		}
	}
}

func containsKey(keys []string, key string) bool {
	for _, k := range keys {
		if strings.EqualFold(k, key) {
			return true
		}
	}
	return false
}

// ---- reconcile ------------------------------------------------------------------------------------------

// reconcile runs the reconcile of the project's source, which covers the project.
func (p *Project) reconcile() { p.source().reconcile() }

// beginReconcile marks a reconcile going, false when the project is stopped.
func (p *Project) beginReconcile() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.reconciling || p.stopped {
		return false
	}
	p.reconciling = true
	return true
}

func (p *Project) endReconcile() {
	p.mu.Lock()
	p.reconciling = false
	p.mu.Unlock()
	p.hub.changed()
}

// reconcileHeld looks at an open issue the project holds: a finished one reopened since goes back to the agent.
func (p *Project) reconcileHeld(ctx context.Context, issue core.Issue) {
	r := p.Store.Get(issue.Key)
	if r == nil || r.State != store.Finished {
		return
	}
	at, err := p.Tracker.ReopenedAt(ctx, issue.Key)
	if err == nil && !at.IsZero() && at.After(r.UpdatedAt) {
		p.Store.Update(issue.Key, func(x *store.Record) { x.IssueClosedReported = false })
		p.enqueue(issue.Key, core.Event{Type: core.EvIssue, Action: "reopened", At: time.Now().UTC()})
	}
}

// maintain reads, for every issue the project holds, what the webhooks might have missed: PRs, comments, closes
// and checks. openKeys are the tracker's open issues; routed is how many of them are the project's, and before
// how many issues the project held before the reconcile claimed any.
func (p *Project) maintain(ctx context.Context, openKeys map[string]bool, routed, before int) {
	recent := time.Now().Add(-3 * 24 * time.Hour)
	for _, r := range p.Store.All() {
		if r.State == store.Finished && r.UpdatedAt.Before(recent) {
			continue
		}
		p.refreshPRs(r.Key)
		fresh := p.Store.Get(r.Key)
		if fresh == nil {
			continue
		}
		if fresh.State == store.Scheduled && fresh.Turns == 0 && !openKeys[r.Key] && p.closed(ctx, r.Key) {
			p.Store.Remove(r.Key)
			p.logf("%s: closed before its scheduled start; forgotten", p.ref(r.Key))
			continue
		}
		p.readComments(ctx, fresh)
		p.readFeedback(ctx, fresh)
		p.checkWaits(r.Key)
		if fresh.State != store.Finished && !fresh.IssueClosedReported && !openKeys[r.Key] && p.closed(ctx, r.Key) {
			p.Store.Update(r.Key, func(x *store.Record) { x.IssueClosedReported = true })
			p.enqueue(r.Key, core.Event{Type: core.EvIssue, Action: "closed", At: time.Now().UTC()})
		}
		if fresh.State == store.Building && fresh.HeadSHA != "" {
			p.evaluateChecks(fresh.HeadSHA)
		}
	}
	p.mu.Lock()
	p.lastReconcile = time.Now()
	p.mu.Unlock()
	claimed := len(p.Store.All()) - before
	msg := fmt.Sprintf("reconciled: %d open issues, %d tracked", routed, len(p.Store.Active()))
	if claimed > 0 {
		msg += fmt.Sprintf(", %d claimed", claimed)
	}
	p.logf("%s", msg)
}

// closed confirms that an issue missing from the tracker's open issues is closed, not just outside the
// source's filter (moved back to a backlog state, out of the JQL). An issue that can't be read isn't.
func (p *Project) closed(ctx context.Context, key string) bool {
	issue, err := p.Tracker.Issue(ctx, key)
	if err != nil {
		p.logf("%s: couldn't read it: %s", p.ref(key), logx.ErrText(err))
		return false
	}
	return !issue.Open
}

// readComments ingests the comments on an issue and its PRs since the last one taken.
func (p *Project) readComments(ctx context.Context, r *store.Record) {
	comments, err := p.Tracker.Comments(ctx, r.Key, r.Since)
	if err != nil {
		p.logf("%s: couldn't read its comments: %s", p.ref(r.Key), logx.ErrText(err))
		return
	}
	for _, id := range r.PRs {
		more, err := p.Repo.PRComments(ctx, id, r.Since)
		if err != nil {
			p.logf("%s: couldn't read the comments on PR #%d: %s", p.ref(r.Key), id, logx.ErrText(err))
			continue
		}
		comments = append(comments, more...)
	}
	sort.SliceStable(comments, func(i, j int) bool { return comments[i].At.Before(comments[j].At) })
	for _, c := range comments {
		if !c.At.After(r.Since) {
			continue
		}
		if c.Kind == core.OnIssue {
			p.noteHold(r.Key, c.ID, c.Body)
		}
		p.ingestOpt(r, c, false)
	}
	p.hub.schedule()
}

// ---- sweep ----------------------------------------------------------------------------------------------

// Corrections counts the correction notes waiting for the sweep.
func (p *Project) Corrections() int {
	entries, err := os.ReadDir(p.CorrectionsDir())
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".md") {
			n++
		}
	}
	return n
}

// runSweep folds the agents' correction notes into the project's rule files with an agent turn.
func (p *Project) runSweep() {
	if p.Spec.Sweep.Disabled {
		return
	}
	count := p.Corrections()
	if count == 0 {
		p.logf("sweep: no corrections to sweep")
		return
	}
	rules := p.Spec.Sweep.RuleFiles
	if len(rules) == 0 {
		rules = p.Agent.RulesFiles()
	}
	body := fmt.Sprintf("Corrections folder: %s\nRule files: %s", p.CorrectionsDir(), strings.Join(rules, ", "))
	if p.Spec.Sweep.Guide != "" {
		body += "\nGuide: " + p.Spec.Sweep.Guide
	}
	body += p.landing(p.Spec.Sweep.Land)
	p.runChore("sweep", &p.sweep, body, fmt.Sprintf("folding %d correction(s) into the rule files", count))
}

// Problems counts the problem notes waiting for the triage.
func (p *Project) Problems() int { return problems.Count(p.ProblemsDir()) }

// runTriage acts on the problems the agents reported with an agent turn: it fixes what is plainly wrong in the
// project's own files and reports, or files, the rest.
func (p *Project) runTriage() {
	if p.Spec.Triage.Disabled {
		return
	}
	count := p.Problems()
	if count == 0 {
		p.logf("triage: no problems to triage")
		return
	}
	body := fmt.Sprintf("Problems folder: %s\nTracker: %s\nCode: %s %s", p.ProblemsDir(), p.Tracker.Describe(), p.Repo.Describe(), p.Repo.WebURL())
	if p.Spec.Triage.OpenIssues {
		body += "\nCauses: open or update an issue for each"
		if up := p.hub.upstream(); up != nil && up != p {
			body += fmt.Sprintf("\nUpstream: open-supervisor is the project %s here, so open or update its issues with --upstream", up.Name)
		}
	} else {
		body += "\nCauses: write them to the report"
	}
	body += "\nReport: " + p.TriageReport()
	body += p.landing(p.Spec.Triage.Land)
	p.runChore("triage", &p.triage, body, fmt.Sprintf("triaging %d problem(s)", count))
}

// withExtra adds a project's additions to a playbook (see playbooks.Project) to a turn's message.
func withExtra(body, extra string) string {
	if extra == "" {
		return body
	}
	add := "## This project's additions to the playbook\n\nFollow these as well as the playbook. They add to its steps and never replace them.\n\n" + extra
	if body == "" {
		return add
	}
	return body + "\n\n" + add
}

// landing are the lines of a chore's message that say where its commit goes: the main branch, the remote, and
// whether to push it or open a PR.
func (p *Project) landing(land string) string {
	out := ""
	if p.base != "" {
		out += "\nMain branch: " + p.base
	}
	out += "\nRemote: " + p.git.RemoteName()
	if land == config.LandPR {
		return out + "\nLand: open a PR"
	}
	return out + "\nLand: push to the main branch"
}

// runChore runs one of the project's daily maintenance turns (the sweep, the triage) in the main checkout and
// logs the last line of its report. slot holds the turn while it runs.
func (p *Project) runChore(name string, slot *adapter.Turn, body, doing string) {
	p.mu.Lock()
	if *slot != nil || p.stopped {
		p.mu.Unlock()
		return
	}
	p.mu.Unlock()
	p.logf("%s: %s", name, doing)
	turn, err := p.Agent.Start(context.Background(), adapter.TurnSpec{
		Cwd:      p.Root,
		Env:      p.hub.sessionEnv(p, ""),
		Dir:      filepath.Join(p.StateDir, "turns", name),
		Playbook: name,
		Body:     withExtra(body, playbooks.Project(p.Root, name)),
		DryRun:   p.hub.settings().DryRun,
	})
	if err != nil {
		p.logf("%s: couldn't start: %s", name, logx.ErrText(err))
		return
	}
	p.mu.Lock()
	*slot = turn
	p.mu.Unlock()
	p.hub.changed()
	<-turn.Done()
	o := turn.Outcome()
	p.mu.Lock()
	*slot = nil
	p.mu.Unlock()
	if o.OK {
		last := strings.TrimSpace(o.Text)
		if i := strings.LastIndex(last, "\n"); i >= 0 {
			last = last[i+1:]
		}
		if last == "" {
			last = "done"
		}
		p.logf("%s: %s", name, last)
	} else {
		p.logf("%s: failed: %s", name, o.Error)
	}
	p.hub.changed()
}

// ---- attaching ------------------------------------------------------------------------------------------

// Attach marks an issue as in the owner's hands; events queue until Detach.
func (p *Project) Attach(key string) *store.Record {
	r := p.Store.Get(key)
	if r == nil || r.State == store.Running || r.State == store.Manual {
		return nil
	}
	r = p.Store.Update(key, func(x *store.Record) {
		x.PreviousState = x.State
		x.State = store.Manual
	})
	p.logf("%s: attached", p.ref(key))
	return r
}

// Detach hands the issue back to the supervisor.
func (p *Project) Detach(key string) {
	p.Store.Update(key, func(x *store.Record) {
		if x.State == store.Manual {
			x.State = x.PreviousState
			if x.State == "" {
				x.State = store.Review
			}
		}
		x.PreviousState = ""
		// What the owner did in the session isn't in the handoff, so the next turn resumes it.
		x.Handoff = ""
	})
	p.logf("%s: detached", p.ref(key))
	p.hub.schedule()
}

// AttachCommand is the interactive session for the issue, with the session's environment.
func (p *Project) AttachCommand(key string) (*exec.Cmd, error) {
	r := p.Store.Get(key)
	if r == nil {
		return nil, fmt.Errorf("%s isn't tracked", key)
	}
	return p.Agent.AttachCommand(adapter.AttachSpec{Cwd: p.Root, Env: p.hub.sessionEnv(p, key), Session: r.Session})
}

// ---- reading, for the TUI -------------------------------------------------------------------------------

// Transcript is the issue's session as entries, when the agent can show it.
func (p *Project) Transcript(key string) []core.Entry {
	r := p.Store.Get(key)
	t, ok := p.Agent.(adapter.Transcripts)
	if r == nil || !ok {
		return nil
	}
	return t.Transcript(p.Root, r.Session)
}

// Activity is what the issue's agent is doing now, when the agent can say.
func (p *Project) Activity(key string) *core.Activity {
	r := p.Store.Get(key)
	t, ok := p.Agent.(adapter.Transcripts)
	if r == nil || !ok {
		return nil
	}
	return t.Activity(p.Root, r.Session)
}

// TranscriptPath is where the issue's session transcript is, when the agent keeps one.
func (p *Project) TranscriptPath(key string) string {
	r := p.Store.Get(key)
	t, ok := p.Agent.(adapter.Transcripts)
	if r == nil || !ok {
		return ""
	}
	return t.TranscriptPath(p.Root, r.Session)
}

// IssueURL is the issue's page on the tracker.
func (p *Project) IssueURL(key string) string { return p.Tracker.IssueURL(key) }

// Ref is how the issue is written: #12, PROJ-12.
func (p *Project) Ref(key string) string { return p.ref(key) }
