package engine

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/alsiola/open-supervisor/internal/core"
	"github.com/alsiola/open-supervisor/internal/store"
)

// ---- holds ----------------------------------------------------------------------------------------------
//
// An issue is held, in the scheduled state, until a time or until something happens: a PR merging, another
// issue closing or its agent reaching a state, a job's run ending (see core.Hold). The hold is written on the
// issue as a marker, by `agent wait`, `agent schedule` or `agent issue create --wait`, so it survives a restart
// and is there for an issue nobody has claimed yet. Signals, the end of each turn and of each job run meet the
// waits as they happen; the reconcile reads what they may have missed.

type holdMarker struct {
	hold      core.Hold
	commentID string // "" for the issue body
}

// latestHold is the latest hold marker on an issue: its body, then its comments in order.
func (p *Project) latestHold(key string) (*holdMarker, error) {
	ctx, cancel := ctxTimeout(time.Minute)
	defer cancel()
	issue, err := p.Tracker.Issue(ctx, key)
	if err != nil {
		return nil, err
	}
	var m *holdMarker
	if h, ok := core.HoldIn(issue.Body); ok {
		m = &holdMarker{hold: h}
	}
	comments, err := p.Tracker.Comments(ctx, key, time.Time{})
	if err != nil {
		return nil, err
	}
	for _, c := range comments {
		if h, ok := core.HoldIn(c.Body); ok {
			m = &holdMarker{hold: h, commentID: c.ID}
		}
	}
	if m != nil && m.hold.Cancel {
		return nil, nil
	}
	return m, nil
}

// waiting reports whether a hold still has something to wait for: a time ahead or a condition.
func waiting(h core.Hold) bool {
	return len(h.Conds) > 0 || h.Until != nil && h.Until.After(time.Now())
}

// setHold puts a hold on the record.
func setHold(x *store.Record, h core.Hold, commentID string) {
	now := time.Now().UTC()
	x.RunAt = nil
	if h.Until != nil {
		t := *h.Until
		x.RunAt = &t
	}
	x.Waits = append([]core.Cond(nil), h.Conds...)
	x.WaitSince = &now
	x.ScheduledBy = commentID
}

// noteHold: a hold marker in a new issue comment (the agent's or anyone's) holds the issue's next turn until
// then, or drops the hold it had.
func (p *Project) noteHold(key, commentID, body string) {
	h, ok := core.HoldIn(body)
	r := p.Store.Get(key)
	if !ok || r == nil || commentID == r.ScheduledBy {
		return
	}
	if h.Cancel {
		if r.RunAt == nil && len(r.Waits) == 0 {
			p.Store.Update(key, func(x *store.Record) { x.ScheduledBy = commentID })
			return
		}
		p.logf("%s: wait cancelled", p.ref(key))
		if r.State == store.Scheduled {
			// Someone else dropped the wait of a parked issue: it goes back to the agent.
			p.Store.Update(key, func(x *store.Record) { x.ScheduledBy = commentID })
			p.endHold(key, "The wait was cancelled.", "cancelled", "")
			p.hub.schedule()
			return
		}
		// The agent dropped its own wait in its turn: nothing to tell it.
		p.Store.Update(key, func(x *store.Record) { x.RunAt, x.Waits, x.WaitSince, x.ScheduledBy = nil, nil, nil, commentID })
		p.editLabels(key, nil, []string{p.Labels.Scheduled})
		return
	}
	if !waiting(h) {
		// A time already past on a waiting issue wakes it at the next check.
		p.Store.Update(key, func(x *store.Record) {
			if x.State == store.Scheduled {
				t := *h.Until
				x.RunAt = &t
			}
			x.ScheduledBy = commentID
		})
		return
	}
	p.Store.Update(key, func(x *store.Record) { setHold(x, h, commentID) })
	p.logf("%s: waiting for %s", p.ref(key), h.Describe())
	p.warnUnknown(h)
	p.editLabels(key, []string{p.Labels.Scheduled}, nil)
	// A running turn is moved to scheduled when it settles.
	if r.State != store.Running && r.State != store.Manual && !p.reserved(key) {
		p.hold(key)
	}
	if len(h.Conds) > 0 {
		go p.checkWaits(key)
	}
}

// warnUnknown logs the conditions on projects the supervisor doesn't run, which only the time can end.
func (p *Project) warnUnknown(h core.Hold) {
	for _, c := range h.Conds {
		if name := c.In(p.Name); name != p.Name && p.hub.peer(name) == nil {
			p.logf("waits on %s, but no project here is called %s", c, name)
		}
	}
}

// hold parks a tracked issue that has something to wait for; status labels come off until it wakes.
func (p *Project) hold(key string) bool {
	r := p.Store.Get(key)
	if r == nil || len(r.Waits) == 0 && (r.RunAt == nil || !r.RunAt.After(time.Now())) {
		return false
	}
	p.Store.Update(key, func(x *store.Record) { x.State = store.Scheduled })
	p.setStatus(key, "")
	p.hub.changed()
	return true
}

// holdText is what a record is held for, for the log.
func holdText(r *store.Record) string {
	return core.Hold{Conds: r.Waits, Until: r.RunAt}.Describe()
}

// wakeDue wakes every scheduled issue whose time has come.
func (p *Project) wakeDue() {
	for _, r := range p.Store.All() {
		if r.State != store.Scheduled || r.RunAt == nil || r.RunAt.After(time.Now()) {
			continue
		}
		if r.Turns == 0 {
			// Not started yet: the owner may have moved the time, or what it waits for, since it was read.
			m, err := p.latestHold(r.Key)
			if err != nil {
				p.callFailed(err, "%s: couldn't read its hold", p.ref(r.Key))
				continue
			}
			if m != nil && waiting(m.hold) && (m.hold.Until == nil || m.hold.Until.After(time.Now())) {
				p.Store.Update(r.Key, func(x *store.Record) { setHold(x, m.hold, m.commentID) })
				continue
			}
		}
		at := r.RunAt.UTC().Format(time.RFC3339)
		var why string
		switch {
		case r.Turns == 0 && len(r.Waits) == 0:
			why = fmt.Sprintf("This issue was scheduled to start at %s, and that time has come.", at)
		case r.Turns == 0:
			why = fmt.Sprintf("This issue was held, waiting for %s, or until %s at the latest. That time has come, and none of the rest has happened.", condsText(r.Waits), at)
		case len(r.Waits) == 0:
			why = fmt.Sprintf("It is now past %s, the time this issue was rescheduled for. Pick it up where you left off.", at)
		default:
			why = fmt.Sprintf("It is now past %s, the latest you said to wait until, and none of what you waited for has happened: %s. Check where it has got to, and wait again if it still matters.", at, condsText(r.Waits))
		}
		p.endHold(r.Key, why, "time", "")
	}
	p.hub.schedule()
}

func condsText(cs []core.Cond) string {
	var parts []string
	for _, c := range cs {
		parts = append(parts, c.Describe())
	}
	return strings.Join(parts, ", or ")
}

// meet ends the waits a fact meets, on every issue of the project.
func (p *Project) meet(f core.Fact) {
	for _, r := range p.Store.All() {
		for _, c := range r.Waits {
			if c.Meets(f, p.Name) {
				p.met(r, c, f)
				break
			}
		}
	}
	p.hub.schedule()
}

// met wakes an issue whose wait on c the fact f has met.
func (p *Project) met(r *store.Record, c core.Cond, f core.Fact) {
	what := f.Describe(c)
	var rest []core.Cond
	for _, o := range r.Waits {
		if o != c {
			rest = append(rest, o)
		}
	}
	var why string
	if r.Turns == 0 {
		why = fmt.Sprintf("This issue was held, waiting for %s. That has happened: %s.", c.Describe(), what)
	} else {
		why = fmt.Sprintf("You were waiting for %s. That has happened: %s.", c.Describe(), what)
		if len(rest) > 0 {
			why += fmt.Sprintf(" You were also waiting for %s, which hasn't happened; wait again if you still need it.", condsText(rest))
		}
	}
	p.endHold(r.Key, why, "condition", string(c.Kind))
}

// endHold ends an issue's hold: its time came, what it waited for happened (why says which), or the wait was
// cancelled. A held issue's next turn starts: its first, or the one it waited for. A running one hears at its
// next turn.
func (p *Project) endHold(key, why, by, kind string) {
	r := p.Store.Get(key)
	if r == nil || r.State != store.Scheduled && r.RunAt == nil && len(r.Waits) == 0 {
		return
	}
	held := r.State == store.Scheduled
	first := held && r.Turns == 0
	since := r.WaitSince
	conds := len(r.Waits)
	p.Store.Update(key, func(x *store.Record) {
		if held {
			x.State = store.Queued
			if first {
				x.ClaimedAt = time.Now().UTC()
			}
		}
		x.RunAt = nil
		x.Waits = nil
		x.WaitSince = nil
		x.Queue = append(x.Queue, core.Event{Type: core.EvWoke, Text: why, At: time.Now().UTC()})
	})
	p.logf("%s: wait over (%s)", p.ref(key), by)
	switch {
	case first:
		p.editLabels(key, []string{p.Labels.Claimed}, []string{p.Labels.Scheduled})
		p.transition(key, "claimed")
	default:
		p.editLabels(key, nil, []string{p.Labels.Scheduled})
	}
	props := map[string]any{"project": p.Name, "issue": key, "by": by, "conditions": conds, "first": first}
	if kind != "" {
		props["kind"] = kind
	}
	if since != nil {
		props["waited_s"] = int(time.Since(*since).Seconds())
	}
	p.hub.capture("agent_wait_ended", props)
}

// checkWaits reads whether any of an issue's waits has been met while nobody was told: at the reconcile, and
// as a wait begins (the PR may have merged already).
func (p *Project) checkWaits(key string) {
	r := p.Store.Get(key)
	if r == nil || len(r.Waits) == 0 {
		return
	}
	ctx, cancel := ctxTimeout(2 * time.Minute)
	defer cancel()
	for _, c := range r.Waits {
		if f, ok := p.poll(ctx, r, c); ok {
			p.met(r, c, f)
			p.hub.schedule()
			return
		}
	}
}

// poll reads whether one condition has been met, and what met it.
func (p *Project) poll(ctx context.Context, r *store.Record, c core.Cond) (core.Fact, bool) {
	owner := p
	if name := c.In(p.Name); name != p.Name {
		if owner = p.hub.peer(name); owner == nil {
			return core.Fact{}, false
		}
	}
	f := core.Fact{Kind: c.Kind, Project: owner.Name, Ref: c.Ref}
	after := func(t *time.Time) bool { return t != nil && (r.WaitSince == nil || t.After(*r.WaitSince)) }
	switch c.Kind {
	case core.CondPR:
		id, _ := strconv.Atoi(c.Ref)
		pr, err := owner.Repo.PR(ctx, id)
		if err != nil {
			p.callFailed(err, "%s: couldn't read PR #%d it waits on", p.ref(r.Key), id)
			return f, false
		}
		f.On = string(pr.State)
		return f, pr.State != core.PROpen
	case core.CondIssue:
		f.On = c.On
		if c.On == "closed" {
			return f, owner.closed(ctx, c.Ref)
		}
		o := owner.Store.Get(c.Ref)
		return f, o != nil && o.LastTurn != nil && after(o.LastTurn.EndedAt) && string(o.State) == c.On
	case core.CondJob:
		if owner.Jobs == nil {
			return f, false
		}
		run, ok := owner.Jobs.lastRun(c.Ref)
		if !ok || !after(run.EndedAt) {
			return f, false
		}
		f.On = run.Outcome
		return f, true
	}
	return f, false
}
