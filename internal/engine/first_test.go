package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/core"
	"github.com/alsiola/open-supervisor/internal/store"
)

func TestPriorityJobHoldsNewTurnsAndTasks(t *testing.T) {
	busyHost(t, "")
	s, p := setup(t, func(adapter.TurnSpec, *fakeTurn) core.Outcome { return review("did it") })
	dir := t.TempDir()
	out, gate := filepath.Join(dir, "out"), filepath.Join(dir, "gate")
	job := gated("refresh", gate, out)
	job.Priority = config.PriorityHigh
	p.Jobs.SetSpecs([]config.Job{job})
	p.Jobs.request(job, &jobReq{trigger: "by hand"})
	waitFor(t, "the hold", func() bool { return strings.Contains(s.JobFirst(), "the refresh job (demo) is running") })
	if got := JobFirst(s.state); !strings.Contains(got, "refresh") {
		t.Fatalf("agent task start isn't told: %q", got)
	}

	host.issues["1"] = &core.Issue{Key: "1", Title: "First", Open: true}
	p.reconcile()
	waitFor(t, "the claim", func() bool { return p.Store.Get("1") != nil })
	s.schedule()
	time.Sleep(50 * time.Millisecond)
	if n := len(agent.turns()); n != 0 {
		t.Fatalf("%d turn(s) started while the priority job ran", n)
	}

	// A task waiting for the host waits for the job instead, without its clock running down.
	file := queueTask(t, p.StateDir, dir, "fill", TaskHoldFor+time.Minute, "sh", "-c", "echo filled")
	p.checkTasks()
	if got, _ := ReadTask(file); !got.Queued() || !got.First || time.Since(*got.QueuedAt) > time.Minute {
		t.Fatalf("task while the job ran: %+v", got)
	}

	_ = os.WriteFile(gate, nil, 0o600)
	waitFor(t, "the turn once the job ended", func() bool { return state(p, "1") == store.Review })
	if s.JobFirst() != "" || JobFirst(s.state) != "" {
		t.Fatalf("still held: %q", s.JobFirst())
	}
	// The task starts once the hold lifts, without waiting for the host's next look.
	if got := ended(t, file); got.Outcome != "ok" {
		t.Fatalf("task: %+v", got)
	}
}

func TestPriorityJobLeadTime(t *testing.T) {
	_, p := setup(t, nil)
	now := time.Now().UTC()
	at := func(d time.Duration) string { return now.Add(d).Format("15:04") }
	job := config.Job{Name: "refresh", Command: []string{"true"}, Priority: config.PriorityHigh, LeadMinutes: 20}
	for _, c := range []struct {
		at   string
		lead int
		want bool
	}{
		{at(10 * time.Minute), 20, true},
		{at(30 * time.Minute), 20, false},
		{at(-time.Minute), 20, true}, // its time has just come: held until the minute check starts it
		{at(-10 * time.Minute), 20, false},
		{at(10 * time.Minute), 0, false},
	} {
		job.At, job.LeadMinutes = []string{c.at}, c.lead
		p.Jobs.SetSpecs([]config.Job{job})
		if got := p.Jobs.firstNow(now); (len(got) > 0) != c.want {
			t.Errorf("at %s lead %d: %v", c.at, c.lead, got)
		}
	}
	job.Priority = ""
	job.At, job.LeadMinutes = []string{at(10 * time.Minute)}, 20
	p.Jobs.SetSpecs([]config.Job{job})
	if got := p.Jobs.firstNow(now); len(got) > 0 {
		t.Errorf("a job without priority holds: %v", got)
	}
}
