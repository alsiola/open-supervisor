package github

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/core"
)

func testConn(t *testing.T, h http.Handler) *conn {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	c, err := newConn(adapter.Values{"auth": "token", "token": "tok", "apiUrl": srv.URL}, adapter.Env{HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func bound(t *testing.T, h http.Handler) (*Tracker, *Repo) {
	c := testConn(t, h)
	tr, err := c.Tracker(adapter.Values{"repo": "octo/widgets"}, adapter.ProjectInfo{})
	if err != nil {
		t.Fatal(err)
	}
	rp, err := c.Repo(adapter.Values{"repo": "octo/widgets"}, adapter.ProjectInfo{})
	if err != nil {
		t.Fatal(err)
	}
	return tr.(*Tracker), rp.(*Repo)
}

func TestRegistered(t *testing.T) {
	s, ok := adapter.Lookup("github")
	if !ok || !s.Has(adapter.RoleRepo) || !s.Has(adapter.RoleTracker) {
		t.Fatalf("github not registered with both roles: %+v", s)
	}
}

func TestCandidatesPaginatesAndSkipsPRs(t *testing.T) {
	var pages int32
	tr, _ := bound(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("auth header %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/repos/octo/widgets/issues" {
			t.Errorf("path %s", r.URL.Path)
		}
		atomic.AddInt32(&pages, 1)
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		var items []map[string]any
		n := 100
		if page == 2 {
			n = 3
		}
		for i := 0; i < n; i++ {
			num := (page-1)*100 + i + 1
			item := map[string]any{"number": num, "title": fmt.Sprint("t", num), "state": "open", "labels": []map[string]string{{"name": "bug"}}}
			if num%50 == 0 {
				item["pull_request"] = map[string]any{"url": "x"}
			}
			if num == 7 {
				item["state_reason"] = "reopened"
			}
			items = append(items, item)
		}
		_ = json.NewEncoder(w).Encode(items)
	}))
	issues, err := tr.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if pages != 2 {
		t.Errorf("read %d pages, want 2", pages)
	}
	if len(issues) != 101 { // 103 minus PRs 50 and 100
		t.Errorf("got %d issues", len(issues))
	}
	if issues[0].Key != "1" || !issues[0].Open || !issues[0].HasLabel("BUG") {
		t.Errorf("first issue %+v", issues[0])
	}
	if !issues[6].Reopened {
		t.Errorf("issue 7 should be reopened")
	}
}

func TestChecksEvaluation(t *testing.T) {
	runs := `{"check_runs":[{"name":"build","status":"completed","conclusion":"success","html_url":"https://x/build"},{"name":"lint","status":"completed","conclusion":"failure","html_url":"https://x/lint"}]}`
	statuses := `{"statuses":[{"context":"netlify/site/deploy-preview","state":"success","target_url":"https://preview.example"}]}`
	_, rp := bound(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/check-runs"):
			fmt.Fprint(w, runs)
		case strings.HasSuffix(r.URL.Path, "/status"):
			fmt.Fprint(w, statuses)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	c, err := rp.Checks(context.Background(), "abc")
	if err != nil {
		t.Fatal(err)
	}
	if !c.Complete || !c.Any || len(c.Failed) != 1 || c.Failed[0] != "lint" || c.Preview != "https://preview.example" || len(c.Runs) != 3 {
		t.Errorf("checks %+v", c)
	}

	pending := rp.evaluate([]checkRun{{Name: "build", Status: "in_progress"}}, nil)
	if pending.Complete || !pending.Any {
		t.Errorf("pending %+v", pending)
	}
	none := rp.evaluate(nil, nil)
	if none.Complete || none.Any {
		t.Errorf("none %+v", none)
	}
}

func TestRequiredChecks(t *testing.T) {
	_, rp := bound(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case "/repos/octo/widgets/branches/release/v%231":
			fmt.Fprint(w, `{"protection":{"required_status_checks":{"contexts":["netlify/site/deploy-preview"],"checks":[{"context":"netlify/site/deploy-preview"},{"context":"test"}]}}}`)
		case "/repos/octo/widgets/rules/branches/release/v%231":
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"Upgrade to GitHub Pro or make this repository public to enable this feature."}`)
		case "/repos/octo/widgets/branches/main":
			fmt.Fprint(w, `{"protection":{"enabled":false,"required_status_checks":{"contexts":[],"checks":[]}}}`)
		case "/repos/octo/widgets/rules/branches/main":
			fmt.Fprint(w, `[{"type":"deletion"},{"type":"required_status_checks","parameters":{"required_status_checks":[{"context":"typecheck"}]}}]`)
		default:
			t.Errorf("unexpected %s", r.URL.EscapedPath())
		}
	}))
	got, err := rp.RequiredChecks(context.Background(), "release/v#1")
	if err != nil || strings.Join(got, ",") != "netlify/site/deploy-preview,test" {
		t.Fatalf("protected branch: %v %v", got, err)
	}
	got, err = rp.RequiredChecks(context.Background(), "main")
	if err != nil || strings.Join(got, ",") != "typecheck" {
		t.Fatalf("ruleset: %v %v", got, err)
	}
}

func TestMarkers(t *testing.T) {
	cases := map[string]*core.AgentMark{
		withMark("hi", core.AgentMark{From: "12"}): {From: "12"},
		withMark("hi", core.AgentMark{}):           {From: ""},
		"done\n\n<!-- claude-agent -->":            {From: ""},
		"done\n<!-- claude-agent from #34 -->":     {From: "34"},
		"just a person":                            nil,
	}
	for body, want := range cases {
		got := agentMark(body)
		if (got == nil) != (want == nil) || (got != nil && got.From != want.From) {
			t.Errorf("agentMark(%q) = %+v, want %+v", body, got, want)
		}
	}
	tr := &Tracker{binding: &binding{c: &conn{web: "https://github.com"}, repo: "octo/widgets"}}
	at := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	if got, ok := core.RunAtIn(tr.HoldMarker(core.Hold{Until: &at})); !ok || !got.Equal(at) {
		t.Errorf("runAt marker round trip: %v %v", got, ok)
	}
	wait := core.Hold{Conds: []core.Cond{{Kind: core.CondPR, Ref: "12", On: "merged"}}, Until: &at}
	if got, ok := core.HoldIn("reason\n\n" + tr.HoldMarker(wait)); !ok || got.Spec() != wait.Spec() {
		t.Errorf("waitFor marker round trip: %+v %v", got, ok)
	}
	if got := tr.RefsIn("Closes #12, fixes #3 and refs #12; see #99"); strings.Join(got, ",") != "12,3" {
		t.Errorf("RefsIn = %v", got)
	}
	if len(tr.RefsIn(tr.RefFor("5"))) != 1 {
		t.Errorf("RefFor isn't read back by RefsIn")
	}
	if got := tr.RefsIn("Closes octo/widgets#7 and fixes other/repo#8"); strings.Join(got, ",") != "7" {
		t.Errorf("full refs: %v", got)
	}
	// A planning repo's issues, worked in other repositories: only full references are its own.
	shared := &Tracker{binding: tr.binding, shared: true}
	if ref := shared.RefFor("5"); ref != "Closes octo/widgets#5" {
		t.Errorf("shared RefFor = %q", ref)
	}
	if got := shared.RefsIn("Closes #3, closes Octo/Widgets#5"); strings.Join(got, ",") != "5" {
		t.Errorf("shared RefsIn = %v", got)
	}
}

func TestCommentsFilterSinceAndKinds(t *testing.T) {
	since := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	_, rp := bound(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/issues/4/comments"):
			if r.URL.Query().Get("since") == "" {
				t.Errorf("no since")
			}
			fmt.Fprint(w, `[{"id":1,"user":{"login":"a","type":"User"},"body":"old","updated_at":"2026-01-01T12:00:00Z"},{"id":2,"user":{"login":"b","type":"Bot"},"body":"new","updated_at":"2026-01-01T13:00:00Z"}]`)
		case strings.Contains(r.URL.Path, "/pulls/4/comments"):
			fmt.Fprint(w, `[{"id":3,"user":{"login":"c","type":"User"},"body":"inline","path":"a.go","original_line":9,"in_reply_to_id":2,"updated_at":"2026-01-01T14:00:00Z"}]`)
		case strings.Contains(r.URL.Path, "/pulls/4/reviews"):
			fmt.Fprint(w, `[{"id":3,"user":{"login":"d","type":"User"},"body":"lgtm","state":"APPROVED","submitted_at":"2026-01-01T15:00:00Z"},{"id":5,"user":{"login":"d","type":"User"},"body":"","state":"APPROVED","submitted_at":"2026-01-01T15:00:00Z"},{"id":6,"user":{"login":"d","type":"User"},"body":"draft","state":"PENDING","submitted_at":"2026-01-01T15:00:00Z"}]`)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
			fmt.Fprint(w, `[]`)
		}
	}))
	cs, err := rp.PRComments(context.Background(), 4, since)
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 3 {
		t.Fatalf("got %d comments: %+v", len(cs), cs)
	}
	if cs[0].ID != "2" || !cs[0].Bot || cs[0].Kind != core.OnPR || cs[0].PR != 4 {
		t.Errorf("conversation %+v", cs[0])
	}
	if cs[1].Kind != core.OnInline || cs[1].Line != 9 || cs[1].InReplyTo != "2" || cs[1].Path != "a.go" {
		t.Errorf("inline %+v", cs[1])
	}
	if cs[2].Kind != core.OnReview || cs[2].ID != "review-3" || cs[2].ReviewState != "APPROVED" {
		t.Errorf("review %+v", cs[2])
	}
}

func TestEditLabelsIgnoresMissing(t *testing.T) {
	var calls []string
	tr, _ := bound(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls = append(calls, r.Method+" "+r.URL.EscapedPath())
		if r.Method == http.MethodDelete {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		fmt.Fprint(w, `[]`)
	}))
	if err := tr.EditLabels(context.Background(), "7", []string{"agent-in-progress"}, []string{"ready for review"}); err != nil {
		t.Fatal(err)
	}
	want := "POST /repos/octo/widgets/issues/7/labels,DELETE /repos/octo/widgets/issues/7/labels/ready%20for%20review"
	if strings.Join(calls, ",") != want {
		t.Errorf("calls %v", calls)
	}
}

func TestDetectRepo(t *testing.T) {
	c := &conn{host: "github.com"}
	for remote, want := range map[string]string{
		"git@github.com:Oddsworth/supervisor.git":       "Oddsworth/supervisor",
		"https://github.com/Oddsworth/supervisor":       "Oddsworth/supervisor",
		"https://x:y@github.com/Oddsworth/sup.git":      "Oddsworth/sup",
		"ssh://git@github.com/Oddsworth/supervisor.git": "Oddsworth/supervisor",
		"git@bitbucket.org:team/repo.git":               "",
	} {
		v, ok := c.DetectRepo(remote)
		if got := v.Get("repo", ""); got != want || ok != (want != "") {
			t.Errorf("DetectRepo(%q) = %q %v", remote, got, ok)
		}
	}
}

func deliver(t *testing.T, h adapter.Hooked, event, body string) []core.Signal {
	t.Helper()
	hdr := http.Header{}
	hdr.Set("X-GitHub-Event", event)
	hdr.Set("X-Hub-Signature-256", adapter.Sign("s3cret", []byte(body)))
	sigs, err := h.ParseWebhook(hdr, []byte(body), "s3cret")
	if err != nil {
		t.Fatalf("%s: %v", event, err)
	}
	return sigs
}

func TestWebhookSignature(t *testing.T) {
	tr, _ := bound(t, http.NotFoundHandler())
	hdr := http.Header{"X-Github-Event": {"ping"}, "X-Hub-Signature-256": {"sha256=00"}}
	if _, err := tr.ParseWebhook(hdr, []byte(`{}`), "s3cret"); err == nil {
		t.Fatal("a bad signature was accepted")
	}
}

func TestWebhookSignals(t *testing.T) {
	tr, rp := bound(t, http.NotFoundHandler())
	repo := `"repository":{"full_name":"Octo/Widgets"}`
	issue := `"issue":{"number":5,"title":"T","state":"open","labels":[{"name":"x"}]}`
	prIssue := `"issue":{"number":8,"title":"P","body":"Closes #5","state":"open","pull_request":{"url":"u"}}`
	pull := `"pull_request":{"number":8,"state":"closed","merged":true,"head":{"ref":"agent/issue-5","sha":"abc"},"body":"b"}`
	comment := `"comment":{"id":11,"user":{"login":"al","type":"User"},"body":"hi","updated_at":"2026-01-01T00:00:00Z","path":"f.go","line":3}`

	type want struct {
		h           adapter.Hooked
		event, body string
		typ         core.SignalType
		key, sha    string
		pr          int
		kind        core.CommentKind
		prState     core.PRState
	}
	cases := []want{
		{tr, "ping", `{` + repo + `}`, core.SigPing, "", "", 0, "", ""},
		{tr, "issues", `{"action":"opened",` + repo + `,` + issue + `}`, core.SigIssueChanged, "5", "", 0, "", ""},
		{tr, "issues", `{"action":"labeled",` + repo + `,` + issue + `}`, core.SigIssueChanged, "5", "", 0, "", ""},
		{tr, "issues", `{"action":"closed",` + repo + `,` + issue + `}`, core.SigIssueClosed, "5", "", 0, "", ""},
		{tr, "issues", `{"action":"reopened",` + repo + `,` + issue + `}`, core.SigIssueReopened, "5", "", 0, "", ""},
		{tr, "issue_comment", `{"action":"created",` + repo + `,` + issue + `,` + comment + `}`, core.SigIssueComment, "5", "", 0, core.OnIssue, ""},
		{rp, "issue_comment", `{"action":"created",` + repo + `,` + prIssue + `,` + comment + `}`, core.SigPRComment, "", "", 8, core.OnPR, core.PROpen},
		{rp, "pull_request", `{"action":"synchronize",` + repo + `,"pull_request":{"number":8,"state":"open","head":{"ref":"agent/issue-5","sha":"abc"}}}`, core.SigPRChanged, "", "", 8, "", core.PROpen},
		{rp, "pull_request", `{"action":"closed",` + repo + `,` + pull + `}`, core.SigPRClosed, "", "", 8, "", core.PRMerged},
		{rp, "pull_request_review_comment", `{"action":"created",` + repo + `,` + pull + `,` + comment + `}`, core.SigPRComment, "", "", 8, core.OnInline, core.PRMerged},
		{rp, "pull_request_review", `{"action":"submitted",` + repo + `,` + pull + `,"review":{"id":2,"user":{"login":"r","type":"User"},"body":"please fix","state":"CHANGES_REQUESTED","submitted_at":"2026-01-01T00:00:00Z"}}`, core.SigPRComment, "", "", 8, core.OnReview, core.PRMerged},
		{rp, "check_suite", `{"action":"completed",` + repo + `,"check_suite":{"head_sha":"abc"}}`, core.SigChecks, "", "abc", 0, "", ""},
		{rp, "check_run", `{"action":"completed",` + repo + `,"check_run":{"head_sha":"def"}}`, core.SigChecks, "", "def", 0, "", ""},
		{rp, "status", `{"sha":"ghi","state":"success",` + repo + `}`, core.SigChecks, "", "ghi", 0, "", ""},
	}
	for i, c := range cases {
		sigs := withoutEvents(deliver(t, c.h, c.event, c.body))
		if len(sigs) != 1 {
			t.Errorf("case %d (%s): %d signals", i, c.event, len(sigs))
			continue
		}
		s := sigs[0]
		if s.Type != c.typ || (c.key != "" && s.Key != c.key) || s.SHA != c.sha {
			t.Errorf("case %d (%s): %+v", i, c.event, s)
		}
		if c.pr != 0 && (s.PR == nil || s.PR.ID != c.pr || s.PR.State != c.prState) {
			t.Errorf("case %d (%s): pr %+v", i, c.event, s.PR)
		}
		if c.kind != "" && (s.Comment == nil || s.Comment.Kind != c.kind) {
			t.Errorf("case %d (%s): comment %+v", i, c.event, s.Comment)
		}
	}

	// Each role takes only its own events; deliveries for another repo and pending statuses are dropped.
	none := []struct {
		h           adapter.Hooked
		event, body string
	}{
		{rp, "issues", `{"action":"opened",` + repo + `,` + issue + `}`},
		{tr, "pull_request", `{"action":"closed",` + repo + `,` + pull + `}`},
		{tr, "issue_comment", `{"action":"created",` + repo + `,` + prIssue + `,` + comment + `}`},
		{rp, "issue_comment", `{"action":"created",` + repo + `,` + issue + `,` + comment + `}`},
		{tr, "issues", `{"action":"opened","repository":{"full_name":"other/repo"},` + issue + `}`},
		{rp, "status", `{"sha":"ghi","state":"pending",` + repo + `}`},
		{rp, "pull_request_review", `{"action":"submitted",` + repo + `,` + pull + `,"review":{"id":2,"user":{"login":"r"},"body":"","state":"APPROVED"}}`},
	}
	for i, c := range none {
		if sigs := withoutEvents(deliver(t, c.h, c.event, c.body)); len(sigs) != 0 {
			t.Errorf("none case %d (%s): %+v", i, c.event, sigs)
		}
	}
}

// withoutEvents drops the SigEvent every delivery carries for jobs.
func withoutEvents(sigs []core.Signal) []core.Signal {
	var out []core.Signal
	for _, s := range sigs {
		if s.Type != core.SigEvent {
			out = append(out, s)
		}
	}
	return out
}

func TestWebhookEvents(t *testing.T) {
	tr, rp := bound(t, http.NotFoundHandler())
	repo := `"repository":{"full_name":"octo/widgets"}`
	for _, c := range []struct {
		h                 adapter.Hooked
		event, body       string
		name, action, ref string
	}{
		{rp, "push", `{"ref":"refs/heads/main",` + repo + `}`, "push", "", "refs/heads/main"},
		{tr, "release", `{"action":"published",` + repo + `}`, "release", "published", ""},
		{rp, "create", `{"ref":"v1.2","ref_type":"tag",` + repo + `}`, "create", "", "refs/tags/v1.2"},
		{rp, "create", `{"ref":"feature","ref_type":"branch",` + repo + `}`, "create", "", "refs/heads/feature"},
	} {
		var ev *core.HookEvent
		for _, s := range deliver(t, c.h, c.event, c.body) {
			if s.Type == core.SigEvent {
				ev = s.Event
			}
		}
		if ev == nil || ev.Name != c.name || ev.Action != c.action || ev.Ref != c.ref || string(ev.Body) != c.body {
			t.Errorf("%s: %+v", c.event, ev)
		}
	}
	push := `{"ref":"refs/heads/main","before":"aaa","after":"bbb","commits":[{"added":["a.json"],"modified":["b.ts"]},{"removed":["c.md"],"modified":["b.ts"]}],` + repo + `}`
	for _, s := range deliver(t, rp, "push", push) {
		if s.Type == core.SigEvent {
			if ev := s.Event; ev.Before != "aaa" || ev.After != "bbb" || strings.Join(ev.Files, ",") != "a.json,b.ts,c.md" {
				t.Errorf("push: %+v", ev)
			}
		}
	}
	for _, s := range deliver(t, tr, "ping", `{`+repo+`}`) {
		if s.Type == core.SigEvent {
			t.Error("a ping went to the jobs")
		}
	}
}

func TestWantEventsSubscribes(t *testing.T) {
	var events []string
	_, rp := bound(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			fmt.Fprint(w, `[]`)
			return
		}
		var body struct {
			Events []string `json:"events"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		events = body.Events
		fmt.Fprint(w, `{"id":1}`)
	}))
	rp.WantEvents([]string{"push", "issues", "push", "release"})
	if _, err := rp.EnsureWebhook(context.Background(), "https://h/x", "s", ""); err != nil {
		t.Fatal(err)
	}
	if want := append(append([]string(nil), hookEvents...), "push", "release"); strings.Join(events, ",") != strings.Join(want, ",") {
		t.Errorf("events = %v", events)
	}
}

func TestEnsureWebhookPatchesOrCreates(t *testing.T) {
	var method string
	_, rp := bound(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet:
			fmt.Fprint(w, `[{"id":42}]`)
		default:
			method = r.Method + " " + r.URL.Path
			fmt.Fprint(w, `{"id":43}`)
		}
	}))
	id, err := rp.EnsureWebhook(context.Background(), "https://h/x", "s", "42")
	if err != nil || id != "42" || method != "PATCH /repos/octo/widgets/hooks/42" {
		t.Errorf("patch: %v %v %v", id, err, method)
	}
	id, err = rp.EnsureWebhook(context.Background(), "https://h/x", "s", "7")
	if err != nil || id != "43" || method != "POST /repos/octo/widgets/hooks" {
		t.Errorf("create: %v %v %v", id, err, method)
	}
}

func TestAppTokens(t *testing.T) {
	key, _ := rsa.GenerateKey(rand.Reader, 2048)
	pemText := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	var mints int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/octo/widgets/installation":
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ey") {
				t.Errorf("no jwt: %q", r.Header.Get("Authorization"))
			}
			fmt.Fprint(w, `{"id":9}`)
		case "/app/installations/9/access_tokens":
			atomic.AddInt32(&mints, 1)
			fmt.Fprintf(w, `{"token":"inst","expires_at":%q}`, time.Now().Add(time.Hour).Format(time.RFC3339))
		case "/repos/octo/widgets/issues/1/comments":
			if r.Header.Get("Authorization") != "Bearer inst" {
				t.Errorf("write didn't use the bot: %q", r.Header.Get("Authorization"))
			}
			fmt.Fprint(w, `{"html_url":"https://c"}`)
		default:
			t.Errorf("unexpected %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c, err := newConn(adapter.Values{"auth": "token", "token": "tok", "apiUrl": srv.URL, "botAppId": "1", "botPrivateKey": pemText}, adapter.Env{HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	tr, _ := c.Tracker(adapter.Values{"repo": "octo/widgets"}, adapter.ProjectInfo{})
	for i := 0; i < 2; i++ {
		if u, err := tr.Comment(context.Background(), "1", "hi", core.AgentMark{From: "1"}); err != nil || u != "https://c" {
			t.Fatalf("comment: %v %v", u, err)
		}
	}
	if mints != 1 {
		t.Errorf("minted %d tokens, want 1 (cached)", mints)
	}
}

func TestOpenIssue(t *testing.T) {
	var got map[string]any
	tr, _ := bound(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/octo/widgets/issues" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		fmt.Fprint(w, `{"number": 31, "title": "Skills call reply.sh", "html_url": "https://github.com/octo/widgets/issues/31", "state": "open"}`)
	}))
	issue, err := tr.OpenIssue(context.Background(), "Skills call reply.sh", "21 sessions", []string{"scheduled"})
	if err != nil {
		t.Fatal(err)
	}
	if got["title"] != "Skills call reply.sh" || got["body"] != "21 sessions" || fmt.Sprint(got["labels"]) != "[scheduled]" {
		t.Errorf("sent %v", got)
	}
	if issue.Key != "31" || issue.URL != "https://github.com/octo/widgets/issues/31" || !issue.Open {
		t.Errorf("issue %+v", issue)
	}
}

func TestReactions(t *testing.T) {
	tr, _ := bound(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/octo/widgets/issues/comments/77" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		fmt.Fprint(w, `{"id": 77, "reactions": {"url": "x", "total_count": 3, "+1": 1, "-1": 2, "laugh": 0}}`)
	}))
	counts, err := tr.Reactions(context.Background(), "https://github.com/octo/widgets/issues/9#issuecomment-77")
	if err != nil {
		t.Fatal(err)
	}
	if counts["-1"] != 2 || counts["+1"] != 1 || counts["total_count"] != 0 {
		t.Errorf("counts %v", counts)
	}
	if _, err := tr.Reactions(context.Background(), "https://github.com/octo/widgets/issues/9"); err == nil {
		t.Error("read reactions without a comment id")
	}
}

func TestPRsForReadsBranchAndTimeline(t *testing.T) {
	_, rp := bound(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/octo/widgets/pulls":
			if r.URL.Query().Get("head") != "octo:agent/issue-12" || r.URL.Query().Get("state") != "all" {
				t.Errorf("pulls query %s", r.URL.RawQuery)
			}
			fmt.Fprint(w, `[{"number": 5, "state": "open", "head": {"ref": "agent/issue-12", "sha": "abc"}}]`)
		case "/repos/octo/widgets/issues/12/timeline":
			fmt.Fprint(w, `[
				{"event": "labeled"},
				{"event": "cross-referenced", "source": {"type": "issue", "issue": {"number": 5, "pull_request": {}, "repository": {"full_name": "octo/widgets"}}}},
				{"event": "cross-referenced", "source": {"type": "issue", "issue": {"number": 9, "pull_request": {}, "repository": {"full_name": "Octo/Widgets"}}}},
				{"event": "cross-referenced", "source": {"type": "issue", "issue": {"number": 10, "repository": {"full_name": "octo/widgets"}}}},
				{"event": "cross-referenced", "source": {"type": "issue", "issue": {"number": 11, "pull_request": {}, "repository": {"full_name": "octo/other"}}}}
			]`)
		case "/repos/octo/widgets/pulls/9":
			fmt.Fprint(w, `{"number": 9, "state": "closed", "merged_at": "2026-10-01T00:00:00Z", "head": {"ref": "fix", "sha": "def"}}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	prs, err := rp.PRsFor(context.Background(), "agent/issue-12", "Closes #12")
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 2 || prs[0].ID != 5 || prs[1].ID != 9 {
		t.Fatalf("PRs %+v, want 5 and 9", prs)
	}
}

func TestPRsForReadsSharedTrackerTimeline(t *testing.T) {
	var asked string
	_, rp := bound(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/octo/tracker/issues/3/timeline":
			asked = r.URL.Path
			fmt.Fprint(w, `[{"event": "cross-referenced", "source": {"type": "issue", "issue": {"number": 4, "pull_request": {}, "repository": {"full_name": "octo/widgets"}}}}]`)
		case "/repos/octo/widgets/pulls/4":
			fmt.Fprint(w, `{"number": 4, "state": "open", "head": {"ref": "x", "sha": "1"}}`)
		default:
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	prs, err := rp.PRsFor(context.Background(), "", "Closes octo/tracker#3")
	if err != nil {
		t.Fatal(err)
	}
	if asked == "" || len(prs) != 1 || prs[0].ID != 4 {
		t.Fatalf("PRs %+v (timeline read: %q)", prs, asked)
	}
}

func TestEditPR(t *testing.T) {
	var got map[string]any
	_, rp := bound(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPatch || r.URL.Path != "/repos/octo/widgets/pulls/7" {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		fmt.Fprint(w, `{"number": 7, "title": "New title", "body": "Closes #3\n\nNew body", "state": "open", "html_url": "https://github.com/octo/widgets/pull/7"}`)
	}))
	pr, err := rp.EditPR(context.Background(), 7, "New title", "Closes #3\n\nNew body")
	if err != nil {
		t.Fatal(err)
	}
	if got["title"] != "New title" || got["body"] != "Closes #3\n\nNew body" {
		t.Errorf("sent %v", got)
	}
	if pr.ID != 7 || pr.URL != "https://github.com/octo/widgets/pull/7" {
		t.Errorf("pr %+v", pr)
	}
}
