package engine

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/core"
	"github.com/alsiola/open-supervisor/internal/store"
)

func TestWaitOnAnotherPRWakesTheAgentWhenItMerges(t *testing.T) {
	var p *Project
	n := 0
	_, p = setup(t, func(spec adapter.TurnSpec, _ *fakeTurn) core.Outcome {
		n++
		if n == 1 {
			// The agent runs `agent wait pr:50:merged` during its turn.
			p.noteHold("30", "c1", "Waiting for the schema PR.\n\n<!-- waitFor: pr:50:merged -->")
		}
		return review("ok")
	})
	host.prs[50] = core.PR{ID: 50, State: core.PROpen, Branch: "agent/issue-29"}
	host.issues["30"] = &core.Issue{Key: "30", Title: "Thirty", Open: true}
	p.reconcile()
	waitFor(t, "held", func() bool { r := p.Store.Get("30"); return r.Turns == 1 && r.State == store.Scheduled })
	if r := p.Store.Get("30"); len(r.Waits) != 1 || r.WaitSince == nil || !host.hasLabel("30", "scheduled") {
		t.Fatalf("hold: %+v, labels %v", r.Waits, host.labelOps)
	}
	time.Sleep(50 * time.Millisecond)
	if len(agent.turns()) != 1 {
		t.Fatal("a held issue ran again before its PR merged")
	}
	// Some other PR merging doesn't wake it.
	p.onSignal(core.Signal{Type: core.SigPRClosed, PR: &core.PR{ID: 51, State: core.PRMerged, Branch: "elsewhere"}})
	time.Sleep(50 * time.Millisecond)
	if state(p, "30") != store.Scheduled {
		t.Fatal("woke on another PR")
	}
	p.onSignal(core.Signal{Type: core.SigPRClosed, PR: &core.PR{ID: 50, State: core.PRMerged, Branch: "agent/issue-29"}})
	waitFor(t, "woken", func() bool { return len(agent.turns()) == 2 })
	body := agent.turns()[1].Body
	if !strings.Contains(body, "You were waiting for PR #50 to merge. That has happened: PR #50 was merged.") {
		t.Fatalf("wake turn:\n%s", body)
	}
	waitFor(t, "review", func() bool { return state(p, "30") == store.Review })
	if r := p.Store.Get("30"); r.Waits != nil || r.RunAt != nil || host.hasLabel("30", "scheduled") {
		t.Fatalf("after waking: %+v %v, labels %v", r.Waits, r.RunAt, host.labelOps)
	}
}

func TestHeldIssueStartsWhenAnotherAgentReports(t *testing.T) {
	_, p := setup(t, func(spec adapter.TurnSpec, _ *fakeTurn) core.Outcome { return review("reported") })
	host.issues["20"] = &core.Issue{Key: "20", Title: "Wave 2", Open: true, Labels: []string{"scheduled"}, Body: "after wave 1\n\n<!-- waitFor: issue:21:review -->"}
	host.issues["21"] = &core.Issue{Key: "21", Title: "Wave 1", Open: true}
	p.reconcile()
	waitFor(t, "wave 2 ran", func() bool { return len(agent.turns()) == 2 })
	if body := agent.turns()[1].Body; !strings.Contains(body, "This issue was held, waiting for the agent on #21 to end a turn in review. That has happened") {
		t.Fatalf("wave 2's first turn:\n%s", body)
	}
	waitFor(t, "wave 2 claimed", func() bool { return host.hasLabel("20", "agent-in-progress") && !host.hasLabel("20", "scheduled") })
}

func TestWaitMetWhileNobodyHeardIsReadAtTheReconcile(t *testing.T) {
	_, p := setup(t, func(spec adapter.TurnSpec, _ *fakeTurn) core.Outcome { return review("ok") })
	host.prs[60] = core.PR{ID: 60, State: core.PROpen}
	host.issues["40"] = &core.Issue{Key: "40", Title: "Later", Open: true, Labels: []string{"scheduled"}, Body: "[waitFor: pr:60]"}
	p.reconcile()
	waitFor(t, "held", func() bool { return state(p, "40") == store.Scheduled })
	host.mu.Lock()
	host.prs[60] = core.PR{ID: 60, State: core.PRClosed}
	host.mu.Unlock()
	p.reconcile()
	waitFor(t, "ran", func() bool { return len(agent.turns()) == 1 })
	if body := agent.turns()[0].Body; !strings.Contains(body, "PR #60 was closed without merging, so it won't merge") {
		t.Fatalf("first turn:\n%s", body)
	}
}

func TestAlreadyMetWaitWakesAtOnce(t *testing.T) {
	_, p := setup(t, func(spec adapter.TurnSpec, _ *fakeTurn) core.Outcome { return review("ok") })
	host.prs[61] = core.PR{ID: 61, State: core.PRMerged}
	host.issues["41"] = &core.Issue{Key: "41", Title: "Later", Open: true, Labels: []string{"scheduled"}, Body: "<!-- waitFor: pr:61:merged -->"}
	p.reconcile()
	waitFor(t, "ran", func() bool { return len(agent.turns()) == 1 })
}

func TestJobRunEndsAWait(t *testing.T) {
	_, p := setup(t, func(spec adapter.TurnSpec, _ *fakeTurn) core.Outcome { return review("ok") })
	host.issues["42"] = &core.Issue{Key: "42", Title: "Check the backup", Open: true, Labels: []string{"scheduled"}, Body: "<!-- waitFor: job:backup; until 2099-01-01T00:00Z -->"}
	p.reconcile()
	waitFor(t, "held", func() bool { return state(p, "42") == store.Scheduled })
	if r := p.Store.Get("42"); r.RunAt == nil || len(r.Waits) != 1 {
		t.Fatalf("hold: %+v", r)
	}
	job := config.Job{Name: "backup"}
	if err := os.MkdirAll(p.Jobs.runsDir("backup"), 0o755); err != nil {
		t.Fatal(err)
	}
	p.Jobs.endRun(job, &JobRun{ID: "r1", Job: "backup", StartedAt: time.Now(), Outcome: "ok"})
	waitFor(t, "ran", func() bool { return len(agent.turns()) == 1 })
	if body := agent.turns()[0].Body; !strings.Contains(body, "a run of the backup job ended: ok") {
		t.Fatalf("first turn:\n%s", body)
	}
}

func TestWaitTimesOutAndCancels(t *testing.T) {
	var p *Project
	n := 0
	_, p = setup(t, func(spec adapter.TurnSpec, _ *fakeTurn) core.Outcome {
		n++
		switch n {
		case 1:
			p.noteHold("43", "c1", "<!-- waitFor: pr:70; until 2099-01-01T00:00Z -->")
		case 2:
			p.noteHold("43", "c2", "<!-- waitFor: pr:70 -->")
			p.noteHold("43", "c3", "never mind\n<!-- waitFor: cancel -->")
		}
		return review("ok")
	})
	host.prs[70] = core.PR{ID: 70, State: core.PROpen}
	host.issues["43"] = &core.Issue{Key: "43", Title: "Waits", Open: true}
	p.reconcile()
	waitFor(t, "held", func() bool { r := p.Store.Get("43"); return r.Turns == 1 && r.State == store.Scheduled })
	p.Store.Update("43", func(r *store.Record) { past := time.Now().Add(-time.Minute); r.RunAt = &past })
	p.wakeDue()
	waitFor(t, "timed out", func() bool { return len(agent.turns()) == 2 })
	if body := agent.turns()[1].Body; !strings.Contains(body, "the latest you said to wait until, and none of what you waited for has happened: PR #70 to merge") {
		t.Fatalf("timeout turn:\n%s", body)
	}
	// The second turn waits again, then cancels: it isn't held.
	waitFor(t, "review", func() bool { return state(p, "43") == store.Review })
	if r := p.Store.Get("43"); r.Waits != nil || r.RunAt != nil || host.hasLabel("43", "scheduled") {
		t.Fatalf("after cancelling: %+v", r)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(agent.turns()); n != 2 {
		t.Fatalf("cancelling its own wait gave the agent another turn (%d turns)", n)
	}
}

func TestFinishedIssueDropsItsWait(t *testing.T) {
	var p *Project
	_, p = setup(t, func(spec adapter.TurnSpec, _ *fakeTurn) core.Outcome {
		p.noteHold("44", "c1", "<!-- waitFor: pr:71 -->")
		return core.Outcome{OK: true, Status: &core.Status{State: core.Finished, Summary: "done"}}
	})
	host.prs[71] = core.PR{ID: 71, State: core.PROpen}
	host.issues["44"] = &core.Issue{Key: "44", Title: "Done", Open: true}
	p.reconcile()
	waitFor(t, "finished", func() bool { return state(p, "44") == store.Finished })
	if r := p.Store.Get("44"); r.Waits != nil {
		t.Fatalf("a finished issue still waits: %+v", r.Waits)
	}
}
