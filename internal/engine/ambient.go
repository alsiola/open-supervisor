package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/alsiola/open-supervisor/internal/ambient"
	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/core"
	"github.com/alsiola/open-supervisor/internal/logx"
	"github.com/alsiola/open-supervisor/internal/store"
)

// Ambient sharing (settings.ambient): every comment an agent posts, on its issue or its PRs, is put to a
// decisions model against every other active issue in the same project, and posted on each it could bear on.
// It runs in the background: nothing an agent does waits on it, and a failure only means a share that didn't
// happen. A share never wakes the agent it lands on: like any other agent's comment, it waits for that issue's
// next turn.

// sourceMaxAge: a comment older than this when first seen (the reconcile reading back an issue's comments)
// isn't shared, even if it was posted after ambient sharing was switched on.
const sourceMaxAge = 24 * time.Hour

// ambientSwitched notes when ambient sharing goes on or off, so only comments posted after it went on, in this
// run of the supervisor, are shared: not what the reconcile reads back from before then.
func (s *Supervisor) ambientSwitched(was, now config.Settings) {
	on := func(st config.Settings) bool { return st.Ambient.Settled().On && !st.DryRun }
	s.ambMu.Lock()
	defer s.ambMu.Unlock()
	switch {
	case !on(now):
		s.ambOn = time.Time{}
	case !on(was) || s.ambOn.IsZero():
		s.ambOn = time.Now()
	}
}

// issueTTL is how long an issue's description is reused before it is read again.
const issueTTL = 30 * time.Minute

// sharer is the supervisor's ambient sharing, for one setting of it.
type sharer struct {
	cfg    config.Ambient
	judge  ambient.Judge
	ledger *ambient.Ledger
	// audit is the file every decision is appended to, a JSON line each.
	audit string
	// sem bounds the questions in flight at once.
	sem chan struct{}

	mu     sync.Mutex
	issues map[string]cachedIssue
	// wg is the passes running, for tests to wait on.
	wg sync.WaitGroup
}

type cachedIssue struct {
	text string
	at   time.Time
}

// sharer is the ambient sharing for the current settings, nil while it is off or can't run (no key).
func (s *Supervisor) sharer() *sharer {
	a := s.settings().Ambient.Settled()
	if !a.On || s.settings().DryRun {
		return nil
	}
	s.ambMu.Lock()
	defer s.ambMu.Unlock()
	if s.amb != nil && s.amb.cfg == a {
		return s.amb
	}
	var judge ambient.Judge
	if s.judgeFor != nil {
		judge = s.judgeFor(a)
	} else {
		key, err := config.ResolveSecret(a.Key)
		if err != nil {
			if s.ambErr != err.Error() {
				s.ambErr = err.Error()
				logx.Default.Printf("ambient sharing is off: the OpenRouter key: %s", logx.ErrText(err))
			}
			return nil
		}
		judge = ambient.NewOpenRouter(key, a.Model, "", nil)
	}
	s.ambErr = ""
	if s.ledger == nil {
		s.ledger = ambient.OpenLedger(filepath.Join(s.state, "ambient", "ledger.json"))
	}
	s.amb = &sharer{cfg: a, judge: judge, ledger: s.ledger, audit: filepath.Join(s.state, "ambient", "decisions.jsonl"),
		sem: make(chan struct{}, 8), issues: map[string]cachedIssue{}}
	return s.amb
}

// share takes a comment the project read on one of its issues (on) and, when it is an agent's and has something
// to say, shares it in the background with the other active issues it could bear on.
func (s *Supervisor) share(p *Project, on string, c core.Comment) {
	if c.Agent == nil || c.Agent.From == "" || ambient.IsShare(c.Body) || !ambient.Substantive(c.Body) {
		return
	}
	if !p.shareable(on) || !p.shareable(c.Agent.From) {
		return
	}
	sh := s.sharer()
	if sh == nil {
		return
	}
	sh.wg.Add(1)
	go func() {
		defer sh.wg.Done()
		sh.pass(s, p, on, c)
	}()
}

// shareable reports whether a comment on, or from the agent of, the issue may be shared: it is one the project
// is working, not finished or closed.
func (p *Project) shareable(key string) bool {
	r := p.Store.Get(key)
	return r != nil && r.State != store.Finished && r.State != store.Scheduled && !r.IssueClosedReported
}

// target is an issue a comment may be shared with.
type target struct {
	p *Project
	r *store.Record
}

func (t target) id() string { return t.p.Name + "/" + t.r.Key }

// pass is one look at a source comment: the first puts it to every other active issue, later ones only try the
// pairs that failed again.
func (sh *sharer) pass(s *Supervisor, p *Project, on string, c core.Comment) {
	source := p.Name + "/" + c.ID
	if !sh.ledger.Begin(source) {
		return
	}
	defer sh.ledger.End(source)
	first := !sh.ledger.Seen(source)
	s.ambMu.Lock()
	since := s.ambOn
	s.ambMu.Unlock()
	if first && (time.Since(c.At) > sourceMaxAge || since.IsZero() || c.At.Before(since)) {
		return
	}
	retry := map[string]bool{}
	if !first {
		for _, t := range sh.ledger.Retry(source) {
			retry[t] = true
		}
		if len(retry) == 0 {
			return
		}
	}
	// Only the source's own project: another project's issues are about something else.
	var targets []target
	for _, r := range p.Store.Active() {
		if r.IssueClosedReported || r.Key == on || r.Key == c.Agent.From {
			continue
		}
		t := target{p, r}
		if !first && !retry[t.id()] {
			continue
		}
		targets = append(targets, t)
	}
	if first {
		sh.ledger.MarkSeen(source)
	}
	origin := sourceOrigin(p, on, c)
	ask := ambient.Ask{Message: c.Body, Source: sourceContext(p, on, c)}
	start := time.Now()
	type result struct {
		judged, shared, suppressed, failed bool
		v                                  ambient.Verdict
	}
	results := make([]result, len(targets))
	var wg sync.WaitGroup
	for i, t := range targets {
		if e, ok := sh.ledger.Get(source, t.id()); ok && e.Done() {
			results[i].suppressed = true
			continue
		}
		wg.Add(1)
		go func(i int, t target) {
			defer wg.Done()
			sh.sem <- struct{}{}
			defer func() { <-sh.sem }()
			res := &results[i]
			res.v, res.judged, res.shared, res.failed = sh.one(s, p, c, source, origin, ask, t)
		}(i, t)
	}
	wg.Wait()
	sum := map[string]any{
		"project": p.Name, "issue": c.Agent.From, "comment": c.ID, "comment_url": c.URL, "kind": string(c.Kind),
		"first": first, "targets": len(targets), "model": sh.cfg.Model, "threshold": sh.cfg.Threshold,
		"duration_ms": time.Since(start).Milliseconds(),
	}
	var judged, positive, shared, suppressed, failed, in, out int
	var cost float64
	for _, r := range results {
		if r.judged {
			judged++
			if r.v.P >= sh.cfg.Threshold {
				positive++
			}
		}
		if r.shared {
			shared++
		}
		if r.suppressed {
			suppressed++
		}
		if r.failed {
			failed++
		}
		in += r.v.InputTokens
		out += r.v.OutputTokens
		cost += r.v.Cost
	}
	sum["judged"], sum["positive"], sum["shared"], sum["suppressed"], sum["failed"] = judged, positive, shared, suppressed, failed
	sum["input_tokens"], sum["output_tokens"], sum["cost_usd"] = in, out, cost
	s.capture("ambient_source", sum)
	if len(targets) > 0 {
		p.logf("%s: ambient: put %s to %d issue(s): %d shared, %d failed, $%.5f", p.ref(c.Agent.From), c.URL, len(targets), shared, failed, cost)
	}
}

// one puts the comment to one target and posts the share when the answer is yes. It reports the verdict and
// whether the model answered, the share was posted, or either failed.
func (sh *sharer) one(s *Supervisor, p *Project, c core.Comment, source string, origin ambient.Origin, ask ambient.Ask, t target) (v ambient.Verdict, judged, shared, failed bool) {
	ctx, cancel := ctxTimeout(3 * time.Minute)
	defer cancel()
	ask.Issue = sh.issueText(ctx, t)
	ask.Progress = progress(t.r)
	start := time.Now()
	v, err := sh.judge.Judge(ctx, ask)
	took := time.Since(start)
	line := map[string]any{
		"at": time.Now().UTC(), "source": source, "source_url": c.URL, "from": p.Name + "/" + c.Agent.From,
		"target": t.id(), "target_url": t.r.URL, "model": v.Model, "p": v.P, "threshold": sh.cfg.Threshold,
		"input_tokens": v.InputTokens, "output_tokens": v.OutputTokens, "cost_usd": v.Cost, "attempts": v.Attempts,
		"judge_ms": took.Milliseconds(),
	}
	defer func() {
		line["shared"] = shared
		sh.record(line)
		props := map[string]any{"project": p.Name, "target_project": t.p.Name}
		for k, x := range line {
			if k != "at" {
				props[k] = x
			}
		}
		s.capture("ambient_judged", props)
	}()
	if err != nil {
		line["error"] = "judge: " + logx.ErrText(err)
		sh.ledger.Fail(source, t.id(), 0, err)
		p.logf("%s: ambient: couldn't put %s to %s: %s", p.ref(c.Agent.From), c.URL, t.id(), logx.ErrText(err))
		return v, false, false, true
	}
	if v.P < sh.cfg.Threshold {
		sh.ledger.Decide(source, t.id(), v.P, "")
		return v, true, false, false
	}
	url, err := t.p.Tracker.Comment(ctx, t.r.Key, ambient.Body(origin, c.Body), core.AgentMark{From: c.Agent.From})
	if err != nil {
		line["error"] = "post: " + logx.ErrText(err)
		sh.ledger.Fail(source, t.id(), v.P, err)
		t.p.callFailed(err, "%s: ambient: couldn't share %s", t.p.ref(t.r.Key), c.URL)
		return v, true, false, true
	}
	line["share_url"] = url
	line["latency_ms"] = time.Since(c.At).Milliseconds()
	sh.ledger.Decide(source, t.id(), v.P, url)
	t.p.logf("%s: ambient: shared %s (p=%.2f)", t.p.ref(t.r.Key), c.URL, v.P)
	return v, true, true, false
}

// issueText is the target's title and description, read once and reused for a while across the comments put to it.
func (sh *sharer) issueText(ctx context.Context, t target) string {
	id := t.id()
	sh.mu.Lock()
	ci, ok := sh.issues[id]
	sh.mu.Unlock()
	if ok && time.Since(ci.at) < issueTTL {
		return ci.text
	}
	text := t.r.Title
	if issue, err := t.p.Tracker.Issue(ctx, t.r.Key); err == nil {
		text = issue.Title + "\n\n" + ambient.Excerpt(issue.Body, 6000)
	} else {
		t.p.callFailed(err, "%s: ambient: couldn't read the issue (judging on its title)", t.p.ref(t.r.Key))
	}
	sh.mu.Lock()
	sh.issues[id] = cachedIssue{text: text, at: time.Now()}
	sh.mu.Unlock()
	return text
}

// record appends a decision to the audit file.
func (sh *sharer) record(line map[string]any) {
	data, err := json.Marshal(line)
	if err != nil {
		return
	}
	sh.mu.Lock()
	defer sh.mu.Unlock()
	_ = os.MkdirAll(filepath.Dir(sh.audit), 0o755)
	f, err := os.OpenFile(sh.audit, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(data, '\n'))
}

// progress is where the target's agent has got to, from what it last reported.
func progress(r *store.Record) string {
	var parts []string
	parts = append(parts, "State: "+string(r.State))
	if r.Touches != "" {
		parts = append(parts, "What the work changes: "+r.Touches)
	}
	if r.LastTurn != nil && r.LastTurn.Summary != "" {
		parts = append(parts, "Last turn: "+r.LastTurn.Summary)
	}
	if r.Handoff != "" {
		parts = append(parts, "Agent's notes: "+ambient.Excerpt(r.Handoff, 3000))
	}
	return strings.Join(parts, "\n")
}

// sourceOrigin is where the comment was posted, as the share names it.
func sourceOrigin(p *Project, on string, c core.Comment) ambient.Origin {
	o := ambient.Origin{Issue: p.ref(c.Agent.From), URL: c.URL, Where: "on the issue"}
	if on != c.Agent.From {
		o.Where = "on " + p.ref(on)
	}
	if r := p.Store.Get(c.Agent.From); r != nil {
		o.Title = r.Title
	}
	switch c.Kind {
	case core.OnPR:
		o.Where = fmt.Sprintf("on PR #%d", c.PR)
	case core.OnInline:
		o.Where = fmt.Sprintf("in a review thread on PR #%d", c.PR)
	case core.OnReview:
		o.Where = fmt.Sprintf("in a review of PR #%d", c.PR)
	}
	return o
}

// sourceContext tells the model where the comment comes from: the issue its agent works and where it was posted.
func sourceContext(p *Project, on string, c core.Comment) string {
	o := sourceOrigin(p, on, c)
	s := fmt.Sprintf("Posted %s by the agent working on %s in project %s", o.Where, o.Issue, p.Name)
	if o.Title != "" {
		s += ": " + o.Title
	}
	return s
}
