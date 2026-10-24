package engine

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/core"
	"github.com/alsiola/open-supervisor/internal/problems"
)

func TestTriageRunsOnTheProblems(t *testing.T) {
	s, p := setup(t, func(spec adapter.TurnSpec, _ *fakeTurn) core.Outcome {
		return core.Outcome{OK: true, Text: "Read 2 problems: 1 cause, fixed 1"}
	})
	s.RunTriage(nil)
	if len(agent.turns()) != 0 {
		t.Fatal("triaged with no problems")
	}
	for _, body := range []string{"reply.sh: not found", "reply.sh: not found again"} {
		if _, err := problems.Write(p.ProblemsDir(), problems.Note{Issue: "4", Kind: "docs", Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	if s.Problems() != 2 {
		t.Fatalf("problems: %d", s.Problems())
	}
	s.RunTriage(nil)
	specs := agent.turns()
	if len(specs) != 1 || specs[0].Playbook != "triage" || specs[0].Cwd != p.Root {
		t.Fatalf("turn specs: %+v", specs)
	}
	for _, want := range []string{"Problems folder: " + p.ProblemsDir(), "Report: " + p.TriageReport(), "Causes: write them to the report", "Remote: origin", "Land: push to the main branch"} {
		if !strings.Contains(specs[0].Body, want) {
			t.Fatalf("body lacks %q:\n%s", want, specs[0].Body)
		}
	}
	if s.Triaging() || p.Triaging() {
		t.Fatal("still triaging")
	}

	p.Spec.Triage.OpenIssues = true
	p.Spec.Triage.Land = config.LandPR
	s.RunTriage(nil)
	if specs := agent.turns(); len(specs) != 2 || !strings.Contains(specs[1].Body, "Causes: open or update an issue for each") || !strings.Contains(specs[1].Body, "Land: open a PR") || strings.Contains(specs[1].Body, "Upstream:") {
		t.Fatalf("turn specs: %+v", specs)
	}

	// With open-supervisor among the projects, its causes are filed there.
	s.mu.Lock()
	s.projects = append(s.projects, &Project{Name: "supervisor", Repo: upstreamRepo{host}})
	s.mu.Unlock()
	s.RunTriage([]*Project{p})
	s.mu.Lock()
	s.projects = s.projects[:1]
	s.mu.Unlock()
	if specs := agent.turns(); len(specs) != 3 || !strings.Contains(specs[2].Body, "Upstream: open-supervisor is the project supervisor here") {
		t.Fatalf("turn specs: %+v", specs)
	}

	p.Spec.Triage.Disabled = true
	s.RunTriage(nil)
	if len(agent.turns()) != 3 {
		t.Fatal("triaged a project with the triage off")
	}
}

// upstreamRepo is open-supervisor's own repo.
type upstreamRepo struct{ *fakeHost }

func (upstreamRepo) WebURL() string { return UpstreamRepo + "/" }

func TestIsUpstream(t *testing.T) {
	if !IsUpstream(upstreamRepo{newHost()}) || IsUpstream(newHost()) || IsUpstream(nil) {
		t.Fatal("IsUpstream")
	}
}

type fakeSink struct {
	mu     sync.Mutex
	events []adapter.TelemetryEvent
}

func (f *fakeSink) Check(context.Context) (string, error) { return "sink", nil }
func (f *fakeSink) Close(context.Context) error           { return nil }
func (f *fakeSink) Capture(_ context.Context, e adapter.TelemetryEvent) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, e)
	return nil
}
func (f *fakeSink) named(name string) []adapter.TelemetryEvent {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []adapter.TelemetryEvent
	for _, e := range f.events {
		if e.Name == name {
			out = append(out, e)
		}
	}
	return out
}

func TestProblemsGoToTelemetryOnce(t *testing.T) {
	s, p := setup(t, func(adapter.TurnSpec, *fakeTurn) core.Outcome { return review("ok") })
	sink := &fakeSink{}
	s.mu.Lock()
	s.sinks = []adapter.Telemetry{sink}
	s.mu.Unlock()
	if _, err := problems.Write(p.ProblemsDir(), problems.Note{Issue: "1", Kind: "env", Body: "already here"}); err != nil {
		t.Fatal(err)
	}
	s.reportProblems() // the first look only takes note of what is there
	if _, err := problems.Write(p.ProblemsDir(), problems.Note{Issue: "2", Kind: "env", Body: "SUPERVISOR_SCRIPTS is unset"}); err != nil {
		t.Fatal(err)
	}
	s.reportProblems()
	s.reportProblems()
	waitFor(t, "the problem event", func() bool { return len(sink.named("agent_problem")) > 0 })
	got := sink.named("agent_problem")
	if len(got) != 1 {
		t.Fatalf("events: %+v", got)
	}
	if e := got[0].Props; e["project"] != "demo" || e["issue"] != "2" || e["kind"] != "env" || e["problem"] != "SUPERVISOR_SCRIPTS is unset" {
		t.Fatalf("props: %+v", e)
	}
}
