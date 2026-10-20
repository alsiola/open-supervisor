package engine

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/core"
)

func TestJobMatches(t *testing.T) {
	job := func(on config.JobTrigger) config.Job { return config.Job{Name: "j", On: &on} }
	push := core.HookEvent{Name: "push", Ref: "refs/heads/main"}
	for i, c := range []struct {
		job  config.Job
		ev   core.HookEvent
		want bool
	}{
		{job(config.JobTrigger{Events: []string{"push"}}), push, true},
		{job(config.JobTrigger{Events: []string{"release"}}), push, false},
		{job(config.JobTrigger{Events: []string{"release.published"}}), core.HookEvent{Name: "release", Action: "published"}, true},
		{job(config.JobTrigger{Events: []string{"release.published"}}), core.HookEvent{Name: "release", Action: "created"}, false},
		{job(config.JobTrigger{Events: []string{"push"}, Branches: []string{"main"}}), push, true},
		{job(config.JobTrigger{Events: []string{"push"}, Branches: []string{"release/*"}}), core.HookEvent{Name: "push", Ref: "refs/heads/release/1.2"}, true},
		{job(config.JobTrigger{Events: []string{"push"}, Branches: []string{"main"}}), core.HookEvent{Name: "push", Ref: "refs/tags/v1"}, false},
		{job(config.JobTrigger{Webhook: true}), push, false},
		{config.Job{Name: "j"}, push, false},
	} {
		if got := matches(c.job, c.ev); got != c.want {
			t.Errorf("case %d: matches = %v", i, got)
		}
	}
}

// envJob is a job that writes the delivery it ran on, and its body, to out.
func envJob(name, out string, on config.JobTrigger) config.Job {
	script := `printf '%s %s %s ' "$OPEN_SUPERVISOR_EVENT" "$OPEN_SUPERVISOR_EVENT_ACTION" "$OPEN_SUPERVISOR_EVENT_REF" >> ` + out + `; cat "$OPEN_SUPERVISOR_EVENT_FILE" >> ` + out + `; echo >> ` + out
	return config.Job{Name: name, Command: []string{"sh", "-c", script}, On: &on}
}

func readOut(file string) string {
	data, _ := os.ReadFile(file)
	return string(data)
}

func TestJobRunsOnEvent(t *testing.T) {
	_, p := setup(t, nil)
	out := filepath.Join(t.TempDir(), "out")
	p.Jobs.Specs = []config.Job{envJob("deploy", out, config.JobTrigger{Events: []string{"push"}, Branches: []string{"main"}})}
	if got := p.Jobs.Events(); len(got) != 1 || got[0] != "push" {
		t.Errorf("Events = %v", got)
	}
	p.Jobs.OnEvent(core.HookEvent{Name: "push", Ref: "refs/heads/feature", Body: []byte(`{}`)})
	p.Jobs.OnEvent(core.HookEvent{Name: "push", Ref: "refs/heads/main", Body: []byte(`{"n":1}`)})
	waitFor(t, "the job", func() bool { return strings.Contains(readOut(out), "\n") })
	if got := readOut(out); got != "push  refs/heads/main {\"n\":1}\n" {
		t.Errorf("out = %q", got)
	}
}

func TestJobDeliveryWhileRunningRunsAgain(t *testing.T) {
	_, p := setup(t, nil)
	dir := t.TempDir()
	out, gate := filepath.Join(dir, "out"), filepath.Join(dir, "gate")
	job := config.Job{Name: "slow", Command: []string{"sh", "-c", `while [ ! -f ` + gate + ` ]; do sleep 0.02; done; cat "$OPEN_SUPERVISOR_EVENT_FILE" >> ` + out}, On: &config.JobTrigger{Webhook: true}}
	p.Jobs.Specs = []config.Job{job}
	p.Jobs.Trigger(job, core.HookEvent{Name: "webhook", Body: []byte("1")})
	waitFor(t, "the first run", func() bool { p.Jobs.mu.Lock(); defer p.Jobs.mu.Unlock(); return p.Jobs.busy["slow"] })
	p.Jobs.Trigger(job, core.HookEvent{Name: "webhook", Body: []byte("2")})
	p.Jobs.Trigger(job, core.HookEvent{Name: "webhook", Body: []byte("3")})
	waitFor(t, "the pending run", func() bool { p.Jobs.mu.Lock(); defer p.Jobs.mu.Unlock(); return p.Jobs.pending["slow"] != nil })
	_ = os.WriteFile(gate, nil, 0o600)
	waitFor(t, "both runs", func() bool { return readOut(out) == "13" })
	waitFor(t, "the job to settle", func() bool { p.Jobs.mu.Lock(); defer p.Jobs.mu.Unlock(); return !p.Jobs.busy["slow"] })
}

func TestJobWebhookURL(t *testing.T) {
	s, p := setup(t, nil)
	s.secret = "s3cret"
	out := filepath.Join(t.TempDir(), "out")
	p.Jobs.Specs = []config.Job{envJob("hook", out, config.JobTrigger{Webhook: true}), {Name: "plain", Command: []string{"true"}}}
	srv := httptest.NewServer(s.handler())
	defer srv.Close()
	s.mu.Lock()
	s.hookURL = srv.URL
	s.mu.Unlock()
	u := s.JobHookURL(p, "hook")
	if !strings.HasPrefix(u, srv.URL+"/jobs/demo/hook/") {
		t.Fatalf("url = %s", u)
	}
	post := func(url string) int {
		res, err := http.Post(url, "application/json", strings.NewReader(`{"hi":1}`))
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := post(srv.URL + "/jobs/demo/hook/wrong"); code != http.StatusNotFound {
		t.Errorf("a bad token: %d", code)
	}
	if code := post(srv.URL + "/jobs/demo/plain/" + jobToken(s.secret, "demo", "plain")); code != http.StatusNotFound {
		t.Errorf("a job without on.webhook: %d", code)
	}
	if code := post(u); code != http.StatusAccepted {
		t.Errorf("the job's url: %d", code)
	}
	waitFor(t, "the job", func() bool { return strings.Contains(readOut(out), "\n") })
	if got := readOut(out); got != "webhook   {\"hi\":1}\n" {
		t.Errorf("out = %q", got)
	}
}
