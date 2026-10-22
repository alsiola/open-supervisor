package engine

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/core"
	"github.com/alsiola/open-supervisor/internal/playbooks"
	"github.com/alsiola/open-supervisor/internal/store"
)

// ---- fakes ------------------------------------------------------------------------------------------------

type fakeHost struct {
	mu        sync.Mutex
	issues    map[string]*core.Issue
	comments  map[string][]core.Comment
	prs       map[int]core.PR
	checks    map[string]core.Checks
	labelOps  []string
	reads     int
	posted    []string
	reactions map[string]map[string]int
}

func newHost() *fakeHost {
	return &fakeHost{issues: map[string]*core.Issue{}, comments: map[string][]core.Comment{}, prs: map[int]core.PR{}, checks: map[string]core.Checks{}}
}

func (h *fakeHost) Check(context.Context) (string, error)    { return "fake", nil }
func (h *fakeHost) DetectRepo(string) (adapter.Values, bool) { return nil, false }
func (h *fakeHost) Repo(adapter.Values, adapter.ProjectInfo) (adapter.Repo, error) {
	return h, nil
}
func (h *fakeHost) Tracker(adapter.Values, adapter.ProjectInfo) (adapter.Tracker, error) {
	return h, nil
}
func (h *fakeHost) Describe() string { return "fake" }
func (h *fakeHost) Candidates(context.Context) ([]core.Issue, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.reads++
	var out []core.Issue
	for _, i := range h.issues {
		if i.Open {
			out = append(out, *i)
		}
	}
	return out, nil
}
func (h *fakeHost) Issue(_ context.Context, key string) (core.Issue, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if i := h.issues[key]; i != nil {
		return *i, nil
	}
	return core.Issue{}, fmt.Errorf("no issue %s", key)
}
func (h *fakeHost) Comments(_ context.Context, key string, since time.Time) ([]core.Comment, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []core.Comment
	for _, c := range h.comments[key] {
		if c.At.After(since) {
			out = append(out, c)
		}
	}
	return out, nil
}
func (h *fakeHost) ReopenedAt(context.Context, string) (time.Time, error) { return time.Time{}, nil }
func (h *fakeHost) EditLabels(_ context.Context, key string, add, remove []string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	i := h.issues[key]
	for _, a := range add {
		h.labelOps = append(h.labelOps, "+"+a)
		if i != nil && !i.HasLabel(a) {
			i.Labels = append(i.Labels, a)
		}
	}
	for _, r := range remove {
		h.labelOps = append(h.labelOps, "-"+r)
		if i != nil {
			var keep []string
			for _, l := range i.Labels {
				if l != r {
					keep = append(keep, l)
				}
			}
			i.Labels = keep
		}
	}
	return nil
}
func (h *fakeHost) hasLabel(key, label string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.issues[key] != nil && h.issues[key].HasLabel(label)
}
func (h *fakeHost) EnsureLabels(context.Context, []adapter.Label) error { return nil }
func (h *fakeHost) Comment(_ context.Context, key, body string, mark core.AgentMark) (string, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.posted = append(h.posted, body)
	return fmt.Sprintf("https://fake/issues/%s#issuecomment-%d", key, len(h.posted)), nil
}
func (h *fakeHost) Reactions(_ context.Context, url string) (map[string]int, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.reactions[url], nil
}
func (h *fakeHost) comments_() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]string(nil), h.posted...)
}
func (h *fakeHost) IssueURL(key string) string { return "https://fake/issues/" + key }
func (h *fakeHost) RefsIn(text string) []string {
	var out []string
	for _, f := range strings.Fields(text) {
		if strings.HasPrefix(f, "#") {
			out = append(out, f[1:])
		}
	}
	return out
}
func (h *fakeHost) RefFor(key string) string { return "Closes #" + key }
func (h *fakeHost) RunAtMarker(t time.Time) string {
	return "<!-- runAt: " + t.Format(time.RFC3339) + " -->"
}
func (h *fakeHost) WebURL() string                                { return "https://fake" }
func (h *fakeHost) DefaultBranch(context.Context) (string, error) { return "main", nil }
func (h *fakeHost) PRsFor(_ context.Context, branch, ref string) ([]core.PR, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []core.PR
	for _, pr := range h.prs {
		if pr.Branch == branch || strings.Contains(pr.Body, ref) {
			out = append(out, pr)
		}
	}
	return out, nil
}
func (h *fakeHost) PR(_ context.Context, id int) (core.PR, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.prs[id], nil
}
func (h *fakeHost) PRComments(context.Context, int, time.Time) ([]core.Comment, error) {
	return nil, nil
}
func (h *fakeHost) Checks(_ context.Context, sha string) (core.Checks, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.checks[sha], nil
}
func (h *fakeHost) CreatePR(context.Context, adapter.PRRequest) (core.PR, error) {
	return core.PR{}, nil
}
func (h *fakeHost) CommentPR(context.Context, int, string, core.AgentMark) (string, error) {
	return "", nil
}
func (h *fakeHost) ReplyInline(context.Context, int, string, string, core.AgentMark) (string, error) {
	return "", nil
}

// fakeAgent runs each turn by calling script with the spec; the turn ends when script returns.
type fakeAgent struct {
	mu     sync.Mutex
	specs  []adapter.TurnSpec
	script func(spec adapter.TurnSpec, t *fakeTurn) core.Outcome
}

func (a *fakeAgent) Check(context.Context) (string, error)            { return "fake agent", nil }
func (a *fakeAgent) Prepare(context.Context, map[string]string) error { return nil }
func (a *fakeAgent) NewSession() string                               { return fmt.Sprintf("s-%d", time.Now().UnixNano()) }
func (a *fakeAgent) SessionExists(cwd, session string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, s := range a.specs {
		if s.Session == session {
			return true
		}
	}
	return false
}
func (a *fakeAgent) RulesFiles() []string                                { return []string{"AGENTS.md"} }
func (a *fakeAgent) AttachCommand(adapter.AttachSpec) (*exec.Cmd, error) { return nil, nil }
func (a *fakeAgent) Adopt(h adapter.TurnHandle, structured bool) adapter.Turn {
	t := &fakeTurn{done: make(chan struct{})}
	t.finish(core.Outcome{Error: "adopted"})
	return t
}
func (a *fakeAgent) Start(_ context.Context, spec adapter.TurnSpec) (adapter.Turn, error) {
	a.mu.Lock()
	a.specs = append(a.specs, spec)
	a.mu.Unlock()
	t := &fakeTurn{done: make(chan struct{}), started: time.Now()}
	go func() { t.finish(a.script(spec, t)) }()
	return t, nil
}
func (a *fakeAgent) turns() []adapter.TurnSpec {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]adapter.TurnSpec(nil), a.specs...)
}

type fakeTurn struct {
	mu      sync.Mutex
	done    chan struct{}
	outcome core.Outcome
	started time.Time
	got     []string
	closed  bool
}

func (t *fakeTurn) Handle() adapter.TurnHandle {
	return adapter.TurnHandle{PID: 1, StartedAt: t.started}
}
func (t *fakeTurn) Send(text string, taken func(bool)) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return false
	}
	t.got = append(t.got, text)
	go taken(true)
	return true
}
func (t *fakeTurn) Kill()                 { t.finish(core.Outcome{Error: "killed"}) }
func (t *fakeTurn) Done() <-chan struct{} { return t.done }
func (t *fakeTurn) Outcome() core.Outcome { t.mu.Lock(); defer t.mu.Unlock(); return t.outcome }
func (t *fakeTurn) received() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.got...)
}
func (t *fakeTurn) finish(o core.Outcome) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closed {
		return
	}
	t.closed = true
	t.outcome = o
	close(t.done)
}

var (
	registerOnce sync.Once
	host         *fakeHost
	agent        *fakeAgent
)

func setup(t *testing.T, script func(spec adapter.TurnSpec, t *fakeTurn) core.Outcome, tweak ...func(*config.Settings)) (*Supervisor, *Project) {
	t.Helper()
	registerOnce.Do(func() {
		adapter.Register(adapter.Spec{Type: "fake-host", Title: "Fake host", Roles: []adapter.Role{adapter.RoleRepo, adapter.RoleTracker},
			New: func(adapter.Values, adapter.Env) (adapter.Connection, error) { return host, nil }})
		adapter.Register(adapter.Spec{Type: "fake-agent", Title: "Fake agent", Roles: []adapter.Role{adapter.RoleAgent},
			New: func(adapter.Values, adapter.Env) (adapter.Connection, error) { return agent, nil }})
	})
	host = newHost()
	agent = &fakeAgent{script: script}
	dir := t.TempDir()
	t.Setenv("OPEN_SUPERVISOR_STATE", filepath.Join(dir, "state"))
	t.Setenv("XDG_DATA_HOME", filepath.Join(dir, "data"))
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(dir, "config"))
	root := filepath.Join(dir, "repo")
	if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	cfg := config.New(filepath.Join(dir, "config.yaml"))
	cfg.Settings.Agent = "agent"
	cfg.Settings.Port = 0
	cfg.Settings.ChecksDebounceSeconds = 0
	cfg.Connections["agent"] = &config.Connection{Type: "fake-agent", Values: adapter.Values{}}
	cfg.Connections["host"] = &config.Connection{Type: "fake-host", Values: adapter.Values{}}
	cfg.Projects = []*config.Project{{Name: "demo", Root: root, Repo: &config.Binding{Connection: "host", Values: adapter.Values{}}, Tracker: &config.Binding{Connection: "host", Values: adapter.Values{}}}}
	for _, f := range tweak {
		f(&cfg.Settings)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, Options{NoTunnel: true})
	if err != nil {
		t.Fatal(err)
	}
	p := s.Projects()[0]
	for _, p := range s.Projects() {
		p.start()
	}
	t.Cleanup(func() {
		for _, p := range s.Projects() {
			p.stop()
		}
		time.Sleep(100 * time.Millisecond) // let settling goroutines write their last
	})
	return s, p
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func state(p *Project, key string) store.State {
	if r := p.Store.Get(key); r != nil {
		return r.State
	}
	return ""
}

func review(summary string) core.Outcome {
	return core.Outcome{OK: true, Status: &core.Status{State: core.Review, Summary: summary}}
}

// ---- tests ------------------------------------------------------------------------------------------------

func TestClaimRunAndLabel(t *testing.T) {
	_, p := setup(t, func(spec adapter.TurnSpec, _ *fakeTurn) core.Outcome { return review("did it") })
	host.issues["1"] = &core.Issue{Key: "1", Title: "First", Open: true}
	host.issues["2"] = &core.Issue{Key: "2", Title: "Backlog", Open: true, Labels: []string{"backlog"}}
	host.issues["3"] = &core.Issue{Key: "3", Title: "Someone else's", Open: true, Labels: []string{"agent-in-progress"}}
	p.reconcile()
	waitFor(t, "issue 1 in review", func() bool { return state(p, "1") == store.Review && host.hasLabel("1", "ready-for-review") })
	if p.Store.Get("2") != nil || p.Store.Get("3") != nil {
		t.Fatal("claimed a skipped issue")
	}
	if !host.hasLabel("1", "agent-in-progress") || !host.hasLabel("1", "ready-for-review") {
		t.Fatalf("labels: %v", host.labelOps)
	}
	specs := agent.turns()
	if len(specs) != 1 || specs[0].Playbook != "work-issue" || specs[0].Args != "1" || !specs[0].Structured {
		t.Fatalf("turn specs: %+v", specs)
	}
	env := strings.Join(specs[0].Env, "\n")
	for _, want := range []string{"OPEN_SUPERVISOR_PROJECT=demo", "OPEN_SUPERVISOR_ISSUE=1", "OPEN_SUPERVISOR_BRANCH=agent/issue-1"} {
		if !strings.Contains(env, want) {
			t.Fatalf("session env lacks %s", want)
		}
	}
}

func TestCommentsBecomeTheNextTurnsEvents(t *testing.T) {
	_, p := setup(t, func(spec adapter.TurnSpec, _ *fakeTurn) core.Outcome { return review("ok") })
	host.issues["5"] = &core.Issue{Key: "5", Title: "Five", Open: true}
	p.reconcile()
	waitFor(t, "first turn", func() bool { return state(p, "5") == store.Review })
	time.Sleep(50 * time.Millisecond) // let the first turn finish settling
	now := time.Now().UTC().Add(time.Second)
	host.comments["5"] = []core.Comment{
		{ID: "c1", Kind: core.OnIssue, Author: "owner", Body: "please change the colour", At: now},
		{ID: "c2", Kind: core.OnIssue, Author: "me", Body: "my own note", At: now, Agent: &core.AgentMark{From: "5"}},
		{ID: "c3", Kind: core.OnIssue, Author: "ci", Body: "beep", At: now, Bot: true},
		// Agents post as the bot app when there is one, so another agent's comment is a bot's too.
		{ID: "c4", Kind: core.OnIssue, Author: "agent-app[bot]", Body: "I'm doing the header", At: now, Bot: true, Agent: &core.AgentMark{From: "9"}},
	}
	p.reconcile()
	waitFor(t, "second turn", func() bool { return len(agent.turns()) == 2 && state(p, "5") == store.Review })
	body := agent.turns()[1].Body
	if agent.turns()[1].Args != "5 events" || !strings.Contains(body, "please change the colour") || !strings.Contains(body, "I'm doing the header") {
		t.Fatalf("second turn body:\n%s", body)
	}
	if strings.Contains(body, "my own note") || strings.Contains(body, "beep") {
		t.Fatalf("handed back an agent's own or a bot's comment:\n%s", body)
	}
	// Reading the same comments again hands nothing over.
	p.reconcile()
	time.Sleep(100 * time.Millisecond)
	if n := len(agent.turns()); n != 2 {
		t.Fatalf("%d turns after a quiet reconcile", n)
	}
}

func TestInterjectionReachesTheRunningTurn(t *testing.T) {
	release := make(chan struct{})
	running := make(chan *fakeTurn, 1)
	_, p := setup(t, func(spec adapter.TurnSpec, ft *fakeTurn) core.Outcome {
		running <- ft
		<-release
		return review("ok")
	})
	host.issues["7"] = &core.Issue{Key: "7", Title: "Seven", Open: true}
	p.reconcile()
	ft := <-running
	waitFor(t, "running", func() bool { return state(p, "7") == store.Running && p.Running("7") != nil })
	p.Message("7", "use the blue one")
	waitFor(t, "taken in", func() bool {
		got := ft.received()
		return len(got) == 1 && strings.Contains(got[0], "use the blue one")
	})
	waitFor(t, "dequeued", func() bool { r := p.Store.Get("7"); return r != nil && len(r.Queue) == 0 })
	close(release)
	waitFor(t, "review", func() bool { return state(p, "7") == store.Review })
	time.Sleep(50 * time.Millisecond)
	if n := len(agent.turns()); n != 1 {
		t.Fatalf("an interjected message started another turn (%d turns)", n)
	}
}

func TestFailedTurnRetriesLater(t *testing.T) {
	_, p := setup(t, func(spec adapter.TurnSpec, _ *fakeTurn) core.Outcome { return core.Outcome{Error: "boom"} })
	host.issues["8"] = &core.Issue{Key: "8", Title: "Eight", Open: true}
	p.reconcile()
	waitFor(t, "failed", func() bool { return state(p, "8") == store.Failed })
	r := p.Store.Get("8")
	if r.Attempts != 1 || r.RetryAfter == nil || r.RetryAfter.Before(time.Now().Add(4*time.Minute)) {
		t.Fatalf("retry: attempts %d after %v", r.Attempts, r.RetryAfter)
	}
	if len(p.candidates()) != 0 {
		t.Fatal("a failed turn is runnable before its retry time")
	}
	p.RunNow("8")
	waitFor(t, "rerun", func() bool { return len(agent.turns()) == 2 })
}

func TestScheduledIssueWaitsForItsTime(t *testing.T) {
	_, p := setup(t, func(spec adapter.TurnSpec, _ *fakeTurn) core.Outcome { return review("ok") })
	at := time.Now().Add(time.Hour).UTC().Truncate(time.Minute)
	host.issues["9"] = &core.Issue{Key: "9", Title: "Later", Open: true, Labels: []string{"scheduled"}, Body: "do it then\n<!-- runAt: " + at.Format(time.RFC3339) + " -->"}
	p.reconcile()
	waitFor(t, "scheduled", func() bool { return state(p, "9") == store.Scheduled })
	if len(agent.turns()) != 0 {
		t.Fatal("ran a scheduled issue early")
	}
	// The time comes.
	host.mu.Lock()
	host.issues["9"].Body = "do it now\n[runAt: 2020-01-01T00:00Z]"
	host.mu.Unlock()
	p.Store.Update("9", func(r *store.Record) { past := time.Now().Add(-time.Minute); r.RunAt = &past })
	p.wakeDue()
	waitFor(t, "ran", func() bool { return state(p, "9") == store.Review })
	if !strings.Contains(agent.turns()[0].Body, "scheduled to start") {
		t.Fatalf("first turn lacks the scheduled note:\n%s", agent.turns()[0].Body)
	}
	if host.hasLabel("9", "scheduled") || !host.hasLabel("9", "agent-in-progress") {
		t.Fatalf("labels after waking: %v", host.labelOps)
	}
}

func TestChecksAndMergeReachTheAgent(t *testing.T) {
	n := 0
	_, p := setup(t, func(spec adapter.TurnSpec, _ *fakeTurn) core.Outcome {
		n++
		switch n {
		case 1:
			host.mu.Lock()
			host.prs[42] = core.PR{ID: 42, State: core.PROpen, Branch: "agent/issue-4", HeadSHA: "abcdef1234567890"}
			host.checks["abcdef1234567890"] = core.Checks{Complete: true, Any: true, Preview: "https://preview"}
			host.mu.Unlock()
			return core.Outcome{OK: true, Status: &core.Status{State: core.Building, Summary: "pushed", PRs: []int{42}}}
		case 2:
			return review("reported")
		default:
			return core.Outcome{OK: true, Status: &core.Status{State: core.Finished, Summary: "cleaned up"}}
		}
	})
	host.issues["4"] = &core.Issue{Key: "4", Title: "Four", Open: true}
	p.reconcile()
	waitFor(t, "checks turn", func() bool { return len(agent.turns()) >= 2 && state(p, "4") == store.Review })
	if body := agent.turns()[1].Body; !strings.Contains(body, "Checks passed on PR #42") || !strings.Contains(body, "https://preview") {
		t.Fatalf("checks turn:\n%s", body)
	}
	// A Bitbucket-style webhook with the full hash matches the abbreviated head.
	if r := p.Store.BySHA("abcdef1234567890ffff"); r == nil {
		t.Fatal("a longer hash didn't match the head")
	}
	p.onSignal(core.Signal{Type: core.SigPRClosed, PR: &core.PR{ID: 42, State: core.PRMerged, Branch: "agent/issue-4"}})
	waitFor(t, "finished", func() bool { return state(p, "4") == store.Finished })
	if body := agent.turns()[2].Body; !strings.Contains(body, "PR #42 merged") {
		t.Fatalf("merge turn:\n%s", body)
	}
}

func TestConcurrencyCap(t *testing.T) {
	release := make(chan struct{})
	s, p := setup(t, func(spec adapter.TurnSpec, _ *fakeTurn) core.Outcome {
		<-release
		return review("ok")
	}, func(s *config.Settings) { s.MaxTurns = 2 })
	for i := 1; i <= 5; i++ {
		k := fmt.Sprint(i)
		host.issues[k] = &core.Issue{Key: k, Title: "n", Open: true}
	}
	p.reconcile()
	time.Sleep(100 * time.Millisecond)
	if n := s.TurnsRunning(); n != 2 {
		t.Fatalf("%d turns running with a cap of 2", n)
	}
	close(release)
	waitFor(t, "all done", func() bool { return len(agent.turns()) == 5 })
}

func TestDotEnv(t *testing.T) {
	f := filepath.Join(t.TempDir(), ".env")
	_ = os.WriteFile(f, []byte("# c\nA=1\nexport B=\"two\"\nC='3'\nbad\n"), 0o644)
	got := readDotEnv(f)
	if got["A"] != "1" || got["B"] != "two" || got["C"] != "3" || len(got) != 3 {
		t.Fatalf("%v", got)
	}
}

func TestColdSessionHandsOffToAFreshOne(t *testing.T) {
	share := 1.0
	s, p := setup(t, func(spec adapter.TurnSpec, _ *fakeTurn) core.Outcome {
		o := review("ok")
		if strings.Contains(spec.Body, "wrap up") {
			o.Status.State = core.Finished
		}
		o.Status.Handoff = "waiting on the owner's pick of colour; kept the old one meanwhile"
		o.Cost, o.ToolCalls, o.ModelCalls = 0.5, 3, 2
		return o
	}, func(s *config.Settings) { s.HandoffShare = &share })
	sink := &fakeSink{}
	s.sinks = []adapter.Telemetry{sink}
	cold := func() {
		p.Store.Update("8", func(x *store.Record) {
			ended := time.Now().UTC().Add(-2 * time.Hour)
			x.LastTurn.EndedAt = &ended
		})
	}
	turn := func(n int, msg string) adapter.TurnSpec {
		t.Helper()
		p.Message("8", msg)
		waitFor(t, msg, func() bool { return len(agent.turns()) == n && state(p, "8") == store.Review })
		time.Sleep(50 * time.Millisecond)
		return agent.turns()[n-1]
	}
	host.issues["8"] = &core.Issue{Key: "8", Title: "Eight", Open: true}
	p.reconcile()
	waitFor(t, "first turn", func() bool { return state(p, "8") == store.Review })
	time.Sleep(50 * time.Millisecond)
	first := p.Store.Get("8").Session
	if p.Store.Get("8").Handoff == "" {
		t.Fatal("the handoff wasn't kept")
	}

	// A warm session resumes, outside the experiment.
	if warm := turn(2, "any news?"); warm.Session != first || !warm.Resume || warm.Args != "8 events" || p.Store.Get("8").LastTurn.Arm != "" {
		t.Fatalf("a warm turn didn't resume: %s %v %s", warm.Session, warm.Resume, warm.Args)
	}

	// A cold one drawn for the handoff starts afresh from the note, keeps the old session and asks for feedback.
	cold()
	fresh := turn(3, "blue please")
	if fresh.Session == first || fresh.Resume || fresh.Args != "8 handoff" {
		t.Fatalf("a cold turn didn't start a fresh session: %s %v %s", fresh.Session, fresh.Resume, fresh.Args)
	}
	if !strings.Contains(fresh.Body, "kept the old one meanwhile") || !strings.Contains(fresh.Body, "blue please") {
		t.Fatalf("the fresh session lacks the handoff or the events:\n%s", fresh.Body)
	}
	r := p.Store.Get("8")
	if r.Session != fresh.Session || len(r.PastSessions) != 1 || r.PastSessions[0] != first || r.LastTurn.Arm != "handoff" {
		t.Fatalf("sessions: %s, earlier %v, arm %q", r.Session, r.PastSessions, r.LastTurn.Arm)
	}
	posted := host.comments_()
	if len(posted) != 1 || posted[0] != feedbackText || len(r.Feedback) != 1 {
		t.Fatalf("feedback asked: %v, watching %v", posted, r.Feedback)
	}
	waitFor(t, "the turn's arm in telemetry", func() bool {
		for _, e := range sink.named("agent_turn") {
			if e.Props["handoff_arm"] == "handoff" && e.Props["session"] == fresh.Session {
				return true
			}
		}
		return false
	})

	// A 👎 on the comment is one event, and the comment is no longer watched.
	host.mu.Lock()
	host.reactions = map[string]map[string]int{r.Feedback[0].URL: {"-1": 1}}
	host.mu.Unlock()
	p.readFeedback(context.Background(), p.Store.Get("8"))
	p.readFeedback(context.Background(), p.Store.Get("8"))
	waitFor(t, "the feedback event", func() bool { return len(sink.named("handoff_feedback")) == 1 })
	if e := sink.named("handoff_feedback")[0]; e.Props["session"] != fresh.Session || e.Props["arm"] != "handoff" || len(p.Store.Get("8").Feedback) != 0 {
		t.Fatalf("feedback event: %+v", e.Props)
	}

	// A cold one drawn to resume resumes, and says so.
	share = 1e-12 // still in the experiment, but the draw all but surely resumes
	cold()
	if back := turn(4, "and the footer"); back.Session != fresh.Session || !back.Resume || p.Store.Get("8").LastTurn.Arm != "resume" {
		t.Fatalf("a cold turn drawn to resume: %s %v", back.Session, back.Resume)
	}
	share = 1

	// After the owner attaches, the next turn resumes even when cold.
	p.Attach("8")
	p.Detach("8")
	cold()
	if after := turn(5, "and the header"); after.Session != fresh.Session || !after.Resume {
		t.Fatalf("a turn after attaching didn't resume: %s %v", after.Session, after.Resume)
	}
	if n := len(host.comments_()); n != 1 {
		t.Fatalf("%d feedback comments for one handoff", n)
	}

	// Each draw is a trial, added up until the next draw or the finish, so the arms compare through to the end.
	p.Message("8", "wrap up")
	waitFor(t, "finished", func() bool { return len(agent.turns()) == 6 && state(p, "8") == store.Finished })
	waitFor(t, "both trials", func() bool { return len(sink.named("handoff_trial")) == 2 })
	if n := len(sink.named("handoff_assigned")); n != 2 {
		t.Fatalf("%d draws recorded", n)
	}
	trials := sink.named("handoff_trial")
	a, b := trials[0].Props, trials[1].Props
	if a["arm"] != "handoff" || a["ended_by"] != "next_draw" || a["completed"] != false || a["turns"] != 1 || a["tool_calls"] != 3 {
		t.Fatalf("the handoff trial: %v", a)
	}
	if b["arm"] != "resume" || b["ended_by"] != "finished" || b["completed"] != true || b["turns"] != 3 || b["cost_usd"] != 1.5 || b["follow_ups"] != 3 {
		t.Fatalf("the resume trial: %v", b)
	}
	if p.Store.Get("8").Trial != nil {
		t.Fatal("a finished issue still has a trial")
	}
}

func TestOptInClaimingAndTheTurnsGitAndAdditions(t *testing.T) {
	t.Setenv("GIT_CONFIG_COUNT", "")
	_, p := setup(t, func(spec adapter.TurnSpec, _ *fakeTurn) core.Outcome { return review("did it") }, func(s *config.Settings) {
		s.Labels.Require = []string{"agent"}
		s.Git.Remote = "upstream"
	})
	dir := filepath.Join(p.Root, playbooks.ProjectDir)
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "work-issue.md"), []byte("Run `make lint` before the PR.\n"), 0o644)
	host.issues["1"] = &core.Issue{Key: "1", Title: "Unmarked", Open: true}
	host.issues["2"] = &core.Issue{Key: "2", Title: "Marked", Open: true, Labels: []string{"agent"}}
	p.reconcile()
	waitFor(t, "issue 2 in review", func() bool { return state(p, "2") == store.Review })
	if p.Store.Get("1") != nil {
		t.Fatal("claimed an issue without the required label")
	}
	specs := agent.turns()
	if len(specs) != 1 || !strings.Contains(specs[0].Body, "## This project's additions to the playbook") || !strings.Contains(specs[0].Body, "Run `make lint` before the PR.") {
		t.Fatalf("turn specs: %+v", specs)
	}
	env := strings.Join(specs[0].Env, "\n")
	for _, want := range []string{"OPEN_SUPERVISOR_REMOTE=upstream", "commit.gpgsign", "GIT_CONFIG_VALUE_0=false"} {
		if !strings.Contains(env, want) {
			t.Fatalf("session env lacks %s", want)
		}
	}
}

func TestSigningKeptWhenAskedFor(t *testing.T) {
	s, p := setup(t, func(spec adapter.TurnSpec, _ *fakeTurn) core.Outcome { return review("ok") }, func(s *config.Settings) { s.Git.Sign = true })
	if env := strings.Join(s.sessionEnv(p, "1"), "\n"); strings.Contains(env, "gpgsign") {
		t.Fatalf("signing turned off: %s", env)
	}
}
