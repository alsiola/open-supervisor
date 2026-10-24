package engine

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/core"
	"github.com/alsiola/open-supervisor/internal/store"
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
	p.Jobs.SetSpecs([]config.Job{envJob("deploy", out, config.JobTrigger{Events: []string{"push"}, Branches: []string{"main"}})})
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
	p.Jobs.SetSpecs([]config.Job{job})
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
	p.Jobs.SetSpecs([]config.Job{envJob("hook", out, config.JobTrigger{Webhook: true}), {Name: "plain", Command: []string{"true"}}})
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

func TestCronSlot(t *testing.T) {
	london, _ := time.LoadLocation("Europe/London")
	at := func(y int, mo time.Month, d, h, mi int) time.Time { return time.Date(y, mo, d, h, mi, 0, 0, london) }
	job := config.Job{Cron: "0 9,13,18 * * *", TimeZone: "Europe/London"}
	for i, c := range []struct {
		job  config.Job
		now  time.Time
		want string
	}{
		{job, at(2026, 10, 21, 9, 0), "2026-10-21 09:00 Europe/London"},
		{job, at(2026, 10, 21, 12, 59), "2026-10-21 09:00 Europe/London"},
		{job, at(2026, 10, 21, 17, 0), "2026-10-21 13:00 Europe/London"},
		// Six hours after the last slot, it waits for the next.
		{job, at(2026, 10, 22, 3, 0), ""},
		{job, at(2026, 10, 21, 23, 30), "2026-10-21 18:00 Europe/London"},
		// In summer time 09:00 London is 08:00 UTC.
		{job, time.Date(2026, 7, 1, 8, 0, 0, 0, time.UTC), "2026-07-01 09:00 Europe/London"},
		{job, time.Date(2026, 7, 1, 7, 59, 0, 0, time.UTC), ""},
		{job, time.Date(2026, 7, 1, 17, 0, 0, 0, time.UTC), "2026-07-01 18:00 Europe/London"},
		{config.Job{Cron: "0 9 * * FRI"}, time.Date(2026, 10, 23, 9, 5, 0, 0, time.UTC), "2026-10-23 09:00 UTC"},
		{config.Job{Cron: "0 9 * * FRI"}, time.Date(2026, 10, 22, 9, 5, 0, 0, time.UTC), ""},
	} {
		if got := dueSlot(c.job, c.now); got != c.want {
			t.Errorf("case %d: %q; want %q", i, got, c.want)
		}
	}
}

func TestCronSlotClockChanges(t *testing.T) {
	job := config.Job{Cron: "30 1 * * *", TimeZone: "Europe/London"}
	// The clocks skip 01:00-02:00 on 29 March 2026: no 01:30 that night, and the next is the night after.
	if got := dueSlot(job, time.Date(2026, 3, 29, 2, 0, 0, 0, time.UTC)); got != "" {
		t.Errorf("spring: %q", got)
	}
	if got := dueSlot(job, time.Date(2026, 3, 30, 0, 31, 0, 0, time.UTC)); got != "2026-03-30 01:30 Europe/London" {
		t.Errorf("spring, the night after: %q", got)
	}
	// They repeat 01:00-02:00 on 25 October 2026: both 01:30s are the one slot, so it runs once.
	first := dueSlot(job, time.Date(2026, 10, 25, 0, 31, 0, 0, time.UTC))  // 01:31 BST
	second := dueSlot(job, time.Date(2026, 10, 25, 1, 31, 0, 0, time.UTC)) // 01:31 GMT
	if first != "2026-10-25 01:30 Europe/London" || second != first {
		t.Errorf("autumn: %q then %q", first, second)
	}
}

func TestPathsMatch(t *testing.T) {
	reels := []string{"data/reels/*.json", "!data/reels/posted.json"}
	for i, c := range []struct {
		patterns, files []string
		want            bool
	}{
		{reels, []string{"data/reels/new.json"}, true},
		{reels, []string{"data/reels/posted.json"}, false},
		{reels, []string{"data/reels/posted.json", "data/reels/b.json"}, true},
		{reels, []string{"data/reels/deep/x.json"}, false},
		{reels, []string{"README.md"}, false},
		{[]string{"src/**"}, []string{"src/a/b/c.ts"}, true},
		{[]string{"**/*.md"}, []string{"README.md"}, true},
		{[]string{"**/*.md"}, []string{"docs/x/README.md"}, true},
		{[]string{"**.json", "!**/posted.json"}, []string{"a/posted.json"}, false},
		{[]string{"data/[ab].json"}, []string{"data/b.json"}, true},
		{[]string{"data/?.json"}, []string{"data/ab.json"}, false},
		{[]string{"!a.txt"}, []string{"a.txt", "b.txt"}, false},
	} {
		if got := pathsMatch(c.patterns, c.files); got != c.want {
			t.Errorf("case %d: %v", i, got)
		}
	}
}

func TestJobPathsFilterPushes(t *testing.T) {
	_, p := setup(t, nil)
	out := filepath.Join(t.TempDir(), "out")
	p.Jobs.SetSpecs([]config.Job{envJob("post", out, config.JobTrigger{Events: []string{"push"}, Branches: []string{"main"}, Paths: []string{"data/reels/*.json", "!data/reels/posted.json"}})})
	p.Jobs.OnEvent(core.HookEvent{Name: "push", Ref: "refs/heads/main", After: "bbb", Files: []string{"data/reels/posted.json"}, Body: []byte(`1`)})
	p.Jobs.OnEvent(core.HookEvent{Name: "push", Ref: "refs/heads/main", After: "ccc", Files: []string{"data/reels/new.json"}, Body: []byte(`2`)})
	waitFor(t, "the job", func() bool { return strings.Contains(readOut(out), "\n") })
	if got := readOut(out); got != "push  refs/heads/main 2\n" {
		t.Errorf("out = %q", got)
	}
	waitFor(t, "the run's record", func() bool { r, ok := p.Jobs.lastRun("post"); return ok && r.EndedAt != nil })
	if r, _ := p.Jobs.lastRun("post"); r.Trigger != "push ccc" || r.Outcome != "ok" {
		t.Errorf("run = %+v", r)
	}
}

// gated is a job that waits for gate to exist and appends what asked for it to out.
func gated(name, gate, out string) config.Job {
	return config.Job{Name: name, Command: []string{"sh", "-c", `while [ ! -f ` + gate + ` ]; do sleep 0.02; done; echo "$OPEN_SUPERVISOR_JOB_TRIGGER" >> ` + out}}
}

func TestJobTimerRunWaitsForTheOneGoing(t *testing.T) {
	_, p := setup(t, nil)
	dir := t.TempDir()
	out, gate := filepath.Join(dir, "out"), filepath.Join(dir, "gate")
	job := gated("post", gate, out)
	job.On = &config.JobTrigger{Webhook: true}
	p.Jobs.SetSpecs([]config.Job{job})
	p.Jobs.request(job, &jobReq{trigger: "cron 1"})
	waitFor(t, "the first run", func() bool { _, ok := p.Jobs.Current("post"); return ok })
	p.Jobs.request(job, &jobReq{trigger: "cron 2"})
	p.Jobs.request(job, &jobReq{trigger: "cron 3"}) // one waits already: dropped
	_ = os.WriteFile(gate, nil, 0o600)
	waitFor(t, "both runs", func() bool { return readOut(out) == "cron 1\ncron 2\n" })
	waitFor(t, "the job to settle", func() bool { p.Jobs.mu.Lock(); defer p.Jobs.mu.Unlock(); return !p.Jobs.busy["post"] })
	if got := readOut(out); got != "cron 1\ncron 2\n" {
		t.Errorf("out = %q", got)
	}

	// A delivery takes a waiting timer run's place; a timer run doesn't take a delivery's.
	_ = os.Remove(gate)
	_ = os.Remove(out)
	p.Jobs.request(job, &jobReq{trigger: "cron 4"})
	waitFor(t, "the run", func() bool { _, ok := p.Jobs.Current("post"); return ok })
	p.Jobs.request(job, &jobReq{trigger: "cron 5"})
	p.Jobs.Trigger(job, core.HookEvent{Name: "webhook", Body: []byte("x")})
	p.Jobs.request(job, &jobReq{trigger: "cron 6"})
	_ = os.WriteFile(gate, nil, 0o600)
	waitFor(t, "both runs", func() bool { return readOut(out) == "cron 4\nwebhook\n" })
	waitFor(t, "the job to settle", func() bool { p.Jobs.mu.Lock(); defer p.Jobs.mu.Unlock(); return !p.Jobs.busy["post"] })
}

func TestJobRunsAreKept(t *testing.T) {
	_, p := setup(t, nil)
	job := config.Job{Name: "check", Command: []string{"sh", "-c", `echo "run $N"; exit $N`}, KeepRuns: 2}
	p.Jobs.SetSpecs([]config.Job{job})
	for _, n := range []string{"0", "3", "0"} {
		p.Jobs.execute(job, &jobReq{trigger: "by hand", extra: []string{"N=" + n}})
	}
	runs := p.Jobs.Runs("check")
	if len(runs) != 2 {
		t.Fatalf("%d runs kept; want 2", len(runs))
	}
	failed := runs[1]
	if failed.Outcome != "failed" || failed.Exit == nil || *failed.Exit != 3 || failed.Trigger != "by hand" || failed.EndedAt == nil || failed.Dir != p.Root {
		t.Errorf("failed run = %+v", failed)
	}
	if data, _ := os.ReadFile(failed.Log); string(data) != "run 3\n" {
		t.Errorf("log = %q", data)
	}
	if runs[0].Outcome != "ok" || *runs[0].Exit != 0 {
		t.Errorf("last run = %+v", runs[0])
	}
	if d := p.Jobs.Describe(job); !strings.HasPrefix(d, "ok ") {
		t.Errorf("Describe = %q", d)
	}
	if files, _ := filepath.Glob(filepath.Join(p.Jobs.runsDir("check"), "*")); len(files) != 4 {
		t.Errorf("files = %v", files)
	}
}

func TestJobTimesOut(t *testing.T) {
	_, p := setup(t, nil)
	defer func(m time.Duration) { jobMinute = m }(jobMinute)
	jobMinute = 200 * time.Millisecond
	job := config.Job{Name: "slow", Command: []string{"sh", "-c", "echo started; sleep 30 & wait"}, TimeoutMinutes: 1}
	p.Jobs.SetSpecs([]config.Job{job})
	began := time.Now()
	p.Jobs.execute(job, &jobReq{trigger: "by hand"})
	if took := time.Since(began); took > 5*time.Second {
		t.Errorf("took %s", took)
	}
	if r, _ := p.Jobs.lastRun("slow"); r.Outcome != "timed out" {
		t.Errorf("run = %+v", r)
	}
}

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestJobFreshCheckout(t *testing.T) {
	_, p := setup(t, nil)
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitIn(t, p.Root, "init", "-q", "--bare", "-b", "main", origin)
	_ = os.WriteFile(filepath.Join(p.Root, "a.txt"), []byte("v1"), 0o644)
	gitIn(t, p.Root, "add", "a.txt")
	gitIn(t, p.Root, "commit", "-qm", "one")
	gitIn(t, p.Root, "remote", "add", "origin", origin)
	gitIn(t, p.Root, "push", "-q", "origin", "HEAD:main")
	_ = os.WriteFile(filepath.Join(p.Root, ".env"), []byte("SECRET=1\n"), 0o600)
	p.Spec.Worktree = config.Worktree{Copy: []string{".env"}, Setup: "echo set up > setup.txt"}
	out := filepath.Join(t.TempDir(), "out")
	job := config.Job{Name: "post reels", Checkout: "fresh", Command: []string{"sh", "-c", `pwd >> ` + out + `; cat .env setup.txt >> ` + out + `; echo "$OPEN_SUPERVISOR_BASE_BRANCH" >> ` + out + `; exit $CODE`}}
	p.Jobs.SetSpecs([]config.Job{job})

	p.Jobs.execute(job, &jobReq{trigger: "by hand", extra: []string{"CODE=0"}})
	run, _ := p.Jobs.lastRun(job.Name)
	lines := strings.Split(readOut(out), "\n")
	if run.Outcome != "ok" || len(lines) < 4 || lines[1] != "SECRET=1" || lines[2] != "set up" || lines[3] != "main" {
		t.Fatalf("run = %+v, out = %q", run, readOut(out))
	}
	if ran := lines[0]; ran == p.Root || !strings.Contains(ran, "job-post-reels-") {
		t.Errorf("ran in %s", ran)
	}
	if _, err := os.Stat(lines[0]); !os.IsNotExist(err) || run.Dir != "" {
		t.Errorf("the checkout of a run that passed is still there (%s)", run.Dir)
	}

	_ = os.Remove(out)
	p.Jobs.execute(job, &jobReq{trigger: "by hand", extra: []string{"CODE=1"}})
	run, _ = p.Jobs.lastRun(job.Name)
	if run.Outcome != "failed" || run.Dir == "" {
		t.Fatalf("run = %+v", run)
	}
	if _, err := os.Stat(filepath.Join(run.Dir, "a.txt")); err != nil {
		t.Errorf("the failed run's checkout wasn't kept: %v", err)
	}
	// Kept a day, then swept by the job's next run.
	old := time.Now().Add(-25 * time.Hour)
	_ = os.Chtimes(run.Dir, old, old)
	p.Jobs.sweepFresh(job)
	if _, err := os.Stat(run.Dir); !os.IsNotExist(err) {
		t.Error("an old checkout wasn't swept")
	}
	if wt := gitIn(t, p.Root, "worktree", "list"); strings.Contains(wt, "job-post-reels") {
		t.Errorf("worktrees: %s", wt)
	}
}

func TestJobCatchesUpTheMainCheckout(t *testing.T) {
	_, p := setup(t, nil)
	p.base = "main"
	origin := filepath.Join(t.TempDir(), "origin.git")
	gitIn(t, p.Root, "init", "-q", "--bare", "-b", "main", origin)
	gitIn(t, p.Root, "symbolic-ref", "HEAD", "refs/heads/main")
	_ = os.WriteFile(filepath.Join(p.Root, "a.txt"), []byte("v1"), 0o644)
	gitIn(t, p.Root, "add", "a.txt")
	gitIn(t, p.Root, "commit", "-qm", "one")
	gitIn(t, p.Root, "remote", "add", "origin", origin)
	gitIn(t, p.Root, "push", "-q", "origin", "HEAD:main")
	other := filepath.Join(t.TempDir(), "other")
	gitIn(t, p.Root, "clone", "-q", origin, other)
	_ = os.WriteFile(filepath.Join(other, "a.txt"), []byte("v2"), 0o644)
	gitIn(t, other, "commit", "-qam", "two")
	gitIn(t, other, "push", "-q", "origin", "main")
	job := config.Job{Name: "check", Command: []string{"cat", "a.txt"}}
	p.Jobs.SetSpecs([]config.Job{job})

	// While another job runs in the checkout, it is only counted.
	p.Jobs.mu.Lock()
	p.Jobs.current["other"] = JobRun{Dir: p.Root}
	p.Jobs.mu.Unlock()
	p.Jobs.execute(job, &jobReq{trigger: "by hand"})
	run, _ := p.Jobs.lastRun(job.Name)
	if run.Behind != 1 || run.Updated != 0 || run.Stale != "other was running in the checkout" {
		t.Errorf("run = %+v", run)
	}
	if data, _ := os.ReadFile(run.Log); !strings.HasSuffix(string(data), "v1") || !strings.Contains(string(data), "1 commit(s) behind origin/main") {
		t.Errorf("log = %q", data)
	}

	p.Jobs.mu.Lock()
	delete(p.Jobs.current, "other")
	p.Jobs.mu.Unlock()
	p.Jobs.execute(job, &jobReq{trigger: "by hand"})
	run, _ = p.Jobs.lastRun(job.Name)
	if run.Updated != 1 || run.Behind != 0 || run.Stale != "" || run.Dir != p.Root {
		t.Errorf("run = %+v", run)
	}
	if data, _ := os.ReadFile(run.Log); !strings.HasSuffix(string(data), "v2") {
		t.Errorf("log = %q", data)
	}
}

func TestCronJobRunsAndIsWaitedFor(t *testing.T) {
	_, p := setup(t, nil)
	job := config.Job{Name: "open issue", Cron: "* * * * *", TimeZone: "Europe/London", Command: []string{"sh", "-c", "echo opened"}}
	p.Jobs.SetSpecs([]config.Job{job})
	p.Jobs.runIfDue(job) // the first time it sees the job, it waits for the next slot
	if _, ok := p.Jobs.lastRun(job.Name); ok {
		t.Fatal("ran a slot on first sight")
	}
	p.Store.Job(job.Name, func(s *store.JobState) { s.LastSlot = "earlier" })
	p.Jobs.runIfDue(job)
	waitFor(t, "the run", func() bool { r, ok := p.Jobs.lastRun(job.Name); return ok && r.EndedAt != nil })
	r, _ := p.Jobs.lastRun(job.Name)
	if r.Outcome != "ok" || !strings.HasPrefix(r.Trigger, "cron ") || !strings.HasSuffix(r.Trigger, " Europe/London") || r.Detached {
		t.Errorf("run = %+v", r)
	}
	if data, _ := os.ReadFile(r.Log); string(data) != "opened\n" {
		t.Errorf("log = %q", data)
	}
	p.Jobs.runIfDue(job) // the same slot again: nothing
	time.Sleep(100 * time.Millisecond)
	if n := len(p.Jobs.Runs(job.Name)); n != 1 {
		t.Errorf("%d runs", n)
	}
}

func TestDetachedJobIsRecorded(t *testing.T) {
	_, p := setup(t, nil)
	status := filepath.Join(t.TempDir(), "status")
	_ = os.MkdirAll(status, 0o755)
	job := config.Job{Name: "refresh", At: []string{"05:17"}, Status: status, Command: []string{"sh", "-c", `echo refreshing; printf '{"slot":"x","outcome":"done","finishedAt":"2026-10-21T05:30:00Z"}' > ` + filepath.Join(status, "last.json")}}
	p.Jobs.SetSpecs([]config.Job{job})
	p.Jobs.RunNow(job)
	waitFor(t, "the run to end", func() bool {
		p.Jobs.followDetached(job)
		_, going := p.Jobs.Running(job.Name)
		r, ok := p.Jobs.lastRun(job.Name)
		return ok && !going && r.EndedAt != nil
	})
	r, _ := p.Jobs.lastRun(job.Name)
	if !r.Detached || r.Outcome != "done" || r.Trigger != "by hand" {
		t.Errorf("run = %+v", r)
	}
	if data, _ := os.ReadFile(r.Log); string(data) != "refreshing\n" {
		t.Errorf("log = %q", data)
	}
}

func TestJobEnvResolvesSecrets(t *testing.T) {
	_, p := setup(t, nil)
	t.Setenv("OS_TEST_POST_TOKEN", "t0ken")
	p.Spec.Env = map[string]string{"POST_TOKEN": "env:OS_TEST_POST_TOKEN", "PLAIN": "https://example.com"}
	job := config.Job{Name: "env", Command: []string{"sh", "-c", `echo "$POST_TOKEN $PLAIN $OPEN_SUPERVISOR_JOB"`}}
	p.Jobs.execute(job, &jobReq{trigger: "by hand"})
	r, _ := p.Jobs.lastRun("env")
	if data, _ := os.ReadFile(r.Log); string(data) != "t0ken https://example.com env\n" {
		t.Errorf("log = %q", data)
	}
}

func TestJobsReloadFromRepoFile(t *testing.T) {
	was := jobsCheck
	jobsCheck = 20 * time.Millisecond
	t.Cleanup(func() { jobsCheck = was })
	_, p := setup(t, nil, func(s *config.Settings) { s.Jobs = true })
	if len(p.Jobs.Specs()) != 0 {
		t.Fatalf("Specs = %v", p.Jobs.Specs())
	}
	out := filepath.Join(t.TempDir(), "out")
	write := func(yaml string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(p.Root, RepoFile), []byte(yaml), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	timers := func() int {
		p.Jobs.timerMu.Lock()
		defer p.Jobs.timerMu.Unlock()
		return len(p.Jobs.timers)
	}
	// Added after the start: the check picks it up and it runs without a restart.
	write("jobs:\n  - name: tick\n    everyMinutes: 60\n    command: [sh, -c, 'echo tick >> " + out + "']\n")
	waitFor(t, "tick to load", func() bool { _, ok := p.Jobs.Find("tick"); return ok })
	waitFor(t, "tick to run", func() bool { return strings.Count(readOut(out), "tick") == 1 })
	if timers() != 1 {
		t.Errorf("timers = %d", timers())
	}
	// A job added beside it leaves its timer be: tick doesn't run again.
	write("jobs:\n  - name: tick\n    everyMinutes: 60\n    command: [sh, -c, 'echo tick >> " + out + "']\n  - name: hooked\n    command: [\"true\"]\n    on:\n      events: [push]\n")
	p.Jobs.refresh()
	if got := p.Jobs.Events(); len(got) != 1 || got[0] != "push" {
		t.Errorf("Events = %v", got)
	}
	time.Sleep(100 * time.Millisecond)
	if n := strings.Count(readOut(out), "tick"); n != 1 {
		t.Errorf("tick ran %d times", n)
	}
	// Taken out: its timer stops.
	write("jobs: []\n")
	p.Jobs.refresh()
	if len(p.Jobs.Specs()) != 0 || timers() != 0 {
		t.Errorf("Specs = %v, timers = %d", p.Jobs.Specs(), timers())
	}
}
