package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/ambient"
	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/core"
	"github.com/alsiola/open-supervisor/internal/store"
)

// fakeJudge answers from a table of (message word, issue word) → probability, and counts what it was asked.
type fakeJudge struct {
	mu    sync.Mutex
	asked []ambient.Ask
	// fail makes the next n questions about an issue whose text has the word fail.
	fail map[string]int
	p    func(a ambient.Ask) float64
}

func (j *fakeJudge) Judge(_ context.Context, a ambient.Ask) (ambient.Verdict, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.asked = append(j.asked, a)
	for word, n := range j.fail {
		if n > 0 && strings.Contains(a.Issue, word) {
			j.fail[word] = n - 1
			return ambient.Verdict{Attempts: 3}, errors.New("openrouter: 502 Bad Gateway")
		}
	}
	return ambient.Verdict{P: j.p(a), Model: "cloudflare/clef-omni-test", InputTokens: 200, Cost: 0.00003, Attempts: 1}, nil
}

func (j *fakeJudge) count() int {
	j.mu.Lock()
	defer j.mu.Unlock()
	return len(j.asked)
}

func (j *fakeJudge) issuesAsked() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	var out []string
	for _, a := range j.asked {
		out = append(out, strings.SplitN(a.Issue, "\n", 2)[0])
	}
	return out
}

type post struct {
	key, body string
	mark      core.AgentMark
}

func (h *fakeHost) posts() []post {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []post
	for i := range h.posted {
		out = append(out, post{h.postedTo[i], h.posted[i], h.marks[i]})
	}
	return out
}

// Representative messages and issues, after ones on this repo: a finding on one issue that explains another with
// no words in common, and routine traffic that explains nothing.
const (
	tunnelFinding = "Found why some webhook deliveries never arrive: the tunnel restarts every 8 hours and GitHub " +
		"doesn't redeliver what failed in the gap. I added a replay of the failed deliveries on reconnect."
	tidyNote = "Tidied the column widths on the jobs tab and renamed two of its headings; there are no behaviour " +
		"changes in this one, it is only presentation."
)

// ambientSetup is a supervisor with ambient sharing on, a fake model, and issues 1–5 claimed: 1 to 3 at work,
// 4 finished and 5 closed on the tracker.
func ambientSetup(t *testing.T, tweak ...func(*config.Settings)) (*Supervisor, *Project, *fakeJudge) {
	t.Helper()
	tweaks := append([]func(*config.Settings){func(s *config.Settings) {
		s.Ambient = config.Ambient{On: true, Key: "test-key"}
	}}, tweak...)
	s, p := setup(t, func(adapter.TurnSpec, *fakeTurn) core.Outcome { return review("ok") }, tweaks...)
	j := &fakeJudge{fail: map[string]int{}, p: func(a ambient.Ask) float64 {
		// The tunnel finding bears on agents not being resumed; nothing else bears on anything.
		if strings.Contains(a.Message, "tunnel restarts") && strings.Contains(a.Issue, "not resumed") {
			return 0.81
		}
		return 0.03
	}}
	s.judgeFor = func(config.Ambient) ambient.Judge { return j }
	issues := []struct {
		key, title, body string
		state            store.State
	}{
		{"1", "Webhooks go missing after a while", "Some GitHub events never reach the supervisor.", store.Review},
		{"2", "Agents not resumed after checks", "An agent sits in building for hours after its checks finish.", store.Building},
		{"3", "Jobs tab column widths", "The columns on the jobs tab are cramped.", store.Review},
		{"4", "Old work", "Finished already.", store.Finished},
		{"5", "Closed on the tracker", "Closed by its owner.", store.Review},
	}
	for _, i := range issues {
		host.issues[i.key] = &core.Issue{Key: i.key, Title: i.title, Body: i.body, Open: i.key != "5", URL: "https://fake/issues/" + i.key}
		p.Store.Create(i.key, i.title, "https://fake/issues/"+i.key, "s-"+i.key)
		st := i.state
		p.Store.Update(i.key, func(r *store.Record) {
			r.State = st
			r.IssueClosedReported = i.key == "5"
			r.Touches = "touches of " + i.key
		})
	}
	return s, p, j
}

func agentComment(id, from, body string) core.Comment {
	return core.Comment{ID: id, Kind: core.OnIssue, Author: "agent-bot", Bot: true, Body: body,
		URL: "https://fake/issues/" + from + "#issuecomment-" + id, At: time.Now().UTC(), Agent: &core.AgentMark{From: from}}
}

// settle waits for every pass ambient sharing has started.
func settle(s *Supervisor) {
	s.ambMu.Lock()
	sh := s.amb
	s.ambMu.Unlock()
	if sh != nil {
		sh.wg.Wait()
	}
}

func TestAmbientSharesAFindingWhereItBears(t *testing.T) {
	s, p, j := ambientSetup(t)
	p.ingest(p.Store.Get("1"), agentComment("c1", "1", tunnelFinding))
	settle(s)
	// Every other active issue is asked about; the source's own, the finished and the closed ones aren't.
	asked := j.issuesAsked()
	if len(asked) != 2 || !strings.Contains(strings.Join(asked, "|"), "Agents not resumed") || !strings.Contains(strings.Join(asked, "|"), "Jobs tab") {
		t.Fatalf("asked about %v", asked)
	}
	posts := host.posts()
	if len(posts) != 1 || posts[0].key != "2" {
		t.Fatalf("posted %+v", posts)
	}
	b := posts[0].body
	for _, want := range []string{ambient.Header, "from #1 (Webhooks go missing after a while)", "> Found why some webhook deliveries never arrive", "Original: https://fake/issues/1#issuecomment-c1", "not checked here"} {
		if !strings.Contains(b, want) {
			t.Errorf("share lacks %q:\n%s", want, b)
		}
	}
	if posts[0].mark.From != "1" {
		t.Fatalf("share marked from %q", posts[0].mark.From)
	}
	// The model saw the target's description and where its agent has got to, not only its title.
	for _, a := range j.asked {
		if strings.Contains(a.Issue, "Agents not resumed") && (!strings.Contains(a.Issue, "sits in building") || !strings.Contains(a.Progress, "touches of 2") || !strings.Contains(a.Source, "#1")) {
			t.Fatalf("ask lacks context: %+v", a)
		}
	}
	data, _ := os.ReadFile(filepath.Join(config.StateDir(), "ambient", "decisions.jsonl"))
	if n := strings.Count(string(data), "\n"); n != 2 || !strings.Contains(string(data), `"share_url"`) {
		t.Fatalf("audit has %d lines:\n%s", n, data)
	}
}

func TestAmbientLeavesIrrelevantAndRoutineCommentsAlone(t *testing.T) {
	s, p, j := ambientSetup(t)
	p.ingest(p.Store.Get("3"), agentComment("c1", "3", tidyNote))
	p.ingest(p.Store.Get("1"), agentComment("c2", "1", "Working on this on branch `agent/issue-1`. A PR will follow here."))
	settle(s)
	if n := j.count(); n != 2 {
		t.Fatalf("asked %d times; want the long note put to 1 and 2, the start comment to nobody", n)
	}
	if posts := host.posts(); len(posts) != 0 {
		t.Fatalf("posted %+v", posts)
	}
}

func TestAmbientOnlyTakesAgentsComments(t *testing.T) {
	s, p, j := ambientSetup(t)
	owner := agentComment("c1", "1", tunnelFinding)
	owner.Agent, owner.Bot, owner.Author = nil, false, "owner"
	p.ingest(p.Store.Get("1"), owner)
	settle(s)
	if j.count() != 0 {
		t.Fatal("the owner's comment was shared")
	}
}

func TestAmbientSharesEachCommentOnce(t *testing.T) {
	s, p, j := ambientSetup(t)
	c := agentComment("c1", "1", tunnelFinding)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ { // a webhook delivered again, and the reconcile reading it back, all at once
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.ingest(p.Store.Get("1"), c)
		}()
	}
	wg.Wait()
	settle(s)
	p.ingest(p.Store.Get("1"), c)
	settle(s)
	if n := j.count(); n != 2 {
		t.Fatalf("asked %d times", n)
	}
	if posts := host.posts(); len(posts) != 1 {
		t.Fatalf("posted %d times", len(posts))
	}
}

func TestAmbientNeverSharesAShare(t *testing.T) {
	s, p, j := ambientSetup(t)
	p.ingest(p.Store.Get("1"), agentComment("c1", "1", tunnelFinding))
	settle(s)
	share := host.posts()[0]
	// The share comes back on issue 2 as a comment, carrying the source's mark.
	back := agentComment("c2", share.mark.From, share.body)
	back.URL = "https://fake/issues/2#issuecomment-c2"
	p.ingest(p.Store.Get("2"), back)
	settle(s)
	if n := j.count(); n != 2 {
		t.Fatalf("the share was put to the model (%d questions)", n)
	}
	// It reaches issue 2's agent like any other agent's comment.
	queued := false
	for _, e := range p.Store.Get("2").Queue {
		if e.Type == core.EvComment && e.Comment != nil && e.Comment.ID == "c2" {
			queued = true
		}
	}
	if !queued {
		t.Fatal("the share didn't reach issue 2's agent")
	}
}

func TestAmbientTriesAFailedPairAgainLater(t *testing.T) {
	s, p, j := ambientSetup(t)
	j.fail["not resumed"] = 1
	c := agentComment("c1", "1", tunnelFinding)
	p.ingest(p.Store.Get("1"), c)
	settle(s)
	if posts := host.posts(); len(posts) != 0 {
		t.Fatalf("posted despite the failure: %+v", posts)
	}
	// The source's own agent isn't held up: its comment never comes back to it either way.
	if r := p.Store.Get("1"); len(r.Queue) != 0 {
		t.Fatalf("issue 1 has %d events", len(r.Queue))
	}
	// Seen again (the reconcile), only the failed pair is asked again, and this time it goes through.
	p.ingest(p.Store.Get("1"), c)
	settle(s)
	if n := j.count(); n != 3 {
		t.Fatalf("asked %d times; want 2 then 1", n)
	}
	if posts := host.posts(); len(posts) != 1 || posts[0].key != "2" {
		t.Fatalf("posted %+v", posts)
	}
}

func TestAmbientGivesUpAfterRepeatedFailures(t *testing.T) {
	s, p, j := ambientSetup(t)
	j.fail["not resumed"] = 100
	c := agentComment("c1", "1", tunnelFinding)
	for i := 0; i < ambient.MaxAttempts+2; i++ {
		p.ingest(p.Store.Get("1"), c)
		settle(s)
	}
	if n := j.count(); n != 2+ambient.MaxAttempts-1 {
		t.Fatalf("asked %d times", n)
	}
}

func TestAmbientSkipsCommentsFromBeforeItWasOn(t *testing.T) {
	s, p, j := ambientSetup(t)
	// What the reconcile reads back at start-up, posted the night before.
	c := agentComment("c1", "1", tunnelFinding)
	c.At = time.Now().Add(-time.Hour)
	p.ingest(p.Store.Get("1"), c)
	settle(s)
	if j.count() != 0 {
		t.Fatal("a comment from before ambient sharing was on was shared")
	}
	// Switched off and on again, it starts afresh.
	off := *s.Config()
	off.Settings.Ambient.On = false
	if err := s.Reload(&off); err != nil {
		t.Fatal(err)
	}
	between := agentComment("c2", "1", tunnelFinding)
	on := off
	on.Settings.Ambient.On = true
	if err := s.Reload(&on); err != nil {
		t.Fatal(err)
	}
	p = s.Project("demo")
	p.ingest(p.Store.Get("1"), between)
	p.ingest(p.Store.Get("1"), agentComment("c3", "1", tunnelFinding))
	settle(s)
	if n := j.count(); n != 2 {
		t.Fatalf("asked %d times; want only the comment posted after it went back on, put to 2 and 3", n)
	}
}

func TestAmbientSkipsOldComments(t *testing.T) {
	s, p, j := ambientSetup(t)
	s.ambMu.Lock()
	s.ambOn = time.Now().Add(-72 * time.Hour)
	s.ambMu.Unlock()
	c := agentComment("c1", "1", tunnelFinding)
	c.At = time.Now().Add(-48 * time.Hour)
	p.ingest(p.Store.Get("1"), c)
	settle(s)
	if j.count() != 0 {
		t.Fatal("a two-day-old comment was shared")
	}
}

func TestAmbientSkipsClosedAndFinishedSources(t *testing.T) {
	s, p, j := ambientSetup(t)
	p.ingest(p.Store.Get("4"), agentComment("c1", "4", tunnelFinding)) // finished
	p.ingest(p.Store.Get("5"), agentComment("c2", "5", tunnelFinding)) // closed on the tracker
	// The closed issue's agent commenting on an open issue is skipped too.
	direct := agentComment("c3", "5", tunnelFinding)
	direct.URL = "https://fake/issues/3#issuecomment-c3"
	p.ingest(p.Store.Get("3"), direct)
	settle(s)
	if n := j.count(); n != 0 {
		t.Fatalf("a closed or finished issue's comment was put to the model %d times", n)
	}
}

func TestAmbientKeepsSharesInTheirProject(t *testing.T) {
	s, p, j := ambientSetup(t)
	root := filepath.Join(t.TempDir(), "club")
	if out, err := exec.Command("git", "init", "-q", root).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	// The other project's issue would take the share if it were asked. Both projects read the same fake tracker,
	// so every issue on it is labelled as claimed already, for the new project's first look to leave alone.
	host.issues["21"] = &core.Issue{Key: "21", Title: "Agents not resumed on the club site", Open: true, URL: "https://fake/issues/21"}
	for _, i := range host.issues {
		i.Labels = append(i.Labels, "agent-in-progress")
	}
	cfg := *s.Config()
	cfg.Projects = append(append([]*config.Project{}, cfg.Projects...), &config.Project{Name: "club", Root: root,
		Repo: cfg.Projects[0].Repo, Tracker: cfg.Projects[0].Tracker})
	if err := s.Reload(&cfg); err != nil {
		t.Fatal(err)
	}
	p, club := s.Project("demo"), s.Project("club")
	club.Store.Create("21", "Agents not resumed on the club site", "https://fake/issues/21", "s-21")
	club.Store.Update("21", func(r *store.Record) { r.State = store.Review })
	p.ingest(p.Store.Get("1"), agentComment("c1", "1", tunnelFinding))
	settle(s)
	for _, issue := range j.issuesAsked() {
		if strings.Contains(issue, "club") {
			t.Fatalf("put to another project's issue: %v", j.issuesAsked())
		}
	}
	if n := j.count(); n != 2 {
		t.Fatalf("asked %d times", n)
	}
	if posts := host.posts(); len(posts) != 1 || posts[0].key != "2" {
		t.Fatalf("posted %+v", posts)
	}
}

func TestAmbientOffSharesNothing(t *testing.T) {
	s, p, j := ambientSetup(t, func(s *config.Settings) { s.Ambient.On = false })
	p.ingest(p.Store.Get("1"), agentComment("c1", "1", tunnelFinding))
	settle(s)
	if j.count() != 0 || len(host.posts()) != 0 {
		t.Fatal("shared with ambient sharing off")
	}
}

func TestAmbientWithManyAgentsAtOnce(t *testing.T) {
	s, p, j := ambientSetup(t)
	j.p = func(a ambient.Ask) float64 { return 0.9 } // everything bears on everything
	active := []string{"1", "2", "3"}
	var wg sync.WaitGroup
	for _, key := range active {
		for n := 0; n < 3; n++ {
			wg.Add(1)
			go func(key string, n int) {
				defer wg.Done()
				p.ingest(p.Store.Get(key), agentComment(fmt.Sprintf("c%s-%d", key, n), key, tunnelFinding))
			}(key, n)
		}
	}
	wg.Wait()
	settle(s)
	// Nine comments, each put to the two other active issues and shared with both.
	if n := j.count(); n != 18 {
		t.Fatalf("asked %d times", n)
	}
	got := map[string]int{}
	for _, post := range host.posts() {
		if post.mark.From == post.key {
			t.Fatalf("a share went back to its own issue: %+v", post)
		}
		got[post.key]++
	}
	if got["1"] != 6 || got["2"] != 6 || got["3"] != 6 || got["4"]+got["5"] != 0 {
		t.Fatalf("shares per issue %v", got)
	}
}
