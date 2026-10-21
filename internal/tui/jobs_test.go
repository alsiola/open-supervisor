package tui

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"

	"github.com/alsiola/open-supervisor/internal/engine"
)

func TestRunDetail(t *testing.T) {
	log := filepath.Join(t.TempDir(), "run.log")
	_ = os.WriteFile(log, []byte("posting 2 reels\nfailed: 401\n"), 0o600)
	started := time.Now().Add(-90 * time.Second)
	ended := started.Add(12 * time.Second)
	exit := 1
	r := engine.JobRun{ID: "x", Job: "post reels", Trigger: "push 1a2b3c4", StartedAt: started, EndedAt: &ended, Outcome: "failed", Exit: &exit, Log: log, Dir: "/tmp/job-post-reels-x"}
	if got := runSummary(r); got != "failed (exit 1) · 12s" {
		t.Errorf("summary = %q", got)
	}
	m := &model{width: 100, height: 30}
	body := ansi.Strip(strings.Join(m.runDetailLines(jobRun{p: &engine.Project{Name: "demo"}, job: "post reels"}, r), "\n"))
	for _, want := range []string{"demo job post reels", "push 1a2b3c4", "failed", "exit          1", "/tmp/job-post-reels-x"} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %q in:\n%s", want, body)
		}
	}
	if out := m.runOutput(r); len(out) != 2 || out[1] != "failed: 401" {
		t.Errorf("output = %q", out)
	}
}
