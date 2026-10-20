package bitbucket

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/core"
)

func testRepo(t *testing.T, h http.HandlerFunc) (*Repo, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	conn, err := open(adapter.Values{"auth": "basic", "username": "me@example.com", "token": "tok", "apiUrl": srv.URL, "previewPattern": "preview|deploy"},
		adapter.Env{Secret: func(s string) (string, error) { return s, nil }, HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	r, err := conn.(*Conn).Repo(adapter.Values{"repo": "acme/site"}, adapter.ProjectInfo{})
	if err != nil {
		t.Fatal(err)
	}
	return r.(*Repo), srv
}

func TestDetectRepo(t *testing.T) {
	c := &Conn{}
	for remote, want := range map[string]string{
		"git@bitbucket.org:acme/site.git":        "acme/site",
		"https://me@bitbucket.org/acme/site.git": "acme/site",
		"https://bitbucket.org/acme/site":        "acme/site",
		"git@github.com:acme/site.git":           "",
	} {
		v, ok := c.DetectRepo(remote)
		if want == "" {
			if ok {
				t.Errorf("%s: detected %v", remote, v)
			}
			continue
		}
		if !ok || v["repo"] != want {
			t.Errorf("%s: got %v %v, want %s", remote, v, ok, want)
		}
	}
}

func TestMarkRoundTrip(t *testing.T) {
	body := Mark("Done in abc123.\n", core.AgentMark{From: "PROJ-12"})
	if !strings.Contains(body, "[//]: # (open-supervisor:agent from=PROJ-12)") {
		t.Fatalf("mark: %q", body)
	}
	got, m := Unmark(body)
	if m == nil || m.From != "PROJ-12" || got != "Done in abc123." {
		t.Fatalf("unmark: %q %+v", got, m)
	}
	_, m = Unmark(Mark("x", core.AgentMark{}))
	if m == nil || m.From != "" {
		t.Fatalf("plain mark: %+v", m)
	}
	if _, m := Unmark("a human comment"); m != nil {
		t.Fatal("human comment marked")
	}
}

func TestPaginationAndComments(t *testing.T) {
	var base string
	r, srv := testRepo(t, func(w http.ResponseWriter, req *http.Request) {
		if req.Header.Get("Authorization") == "" {
			t.Error("no auth")
		}
		switch {
		case strings.Contains(req.URL.RawQuery, "page=2"):
			json.NewEncoder(w).Encode(map[string]any{"values": []any{
				map[string]any{"id": 3, "content": map[string]string{"raw": "inline note"}, "user": map[string]string{"display_name": "Ann"},
					"updated_on": "2026-10-20T12:00:00Z", "inline": map[string]any{"path": "a.go", "to": 7}, "parent": map[string]int{"id": 2}},
			}})
		default:
			json.NewEncoder(w).Encode(map[string]any{"values": []any{
				map[string]any{"id": 1, "content": map[string]string{"raw": "old"}, "user": map[string]string{"display_name": "Ann"}, "updated_on": "2026-10-01T00:00:00Z"},
				map[string]any{"id": 2, "content": map[string]string{"raw": Mark("agent", core.AgentMark{From: "5"})}, "user": map[string]string{"display_name": "Bot"}, "updated_on": "2026-10-20T11:00:00Z"},
				map[string]any{"id": 4, "deleted": true, "updated_on": "2026-10-20T11:00:00Z"},
			}, "next": base + "/repositories/acme/site/pullrequests/9/comments?page=2"})
		}
	})
	base = srv.URL
	got, err := r.PRComments(context.Background(), 9, time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d comments: %+v", len(got), got)
	}
	if got[0].Agent == nil || got[0].Agent.From != "5" || got[0].Body != "agent" || got[0].Kind != core.OnPR {
		t.Errorf("agent comment: %+v", got[0])
	}
	c := got[1]
	if c.Kind != core.OnInline || c.Path != "a.go" || c.Line != 7 || c.InReplyTo != "2" || c.ID != "3" || c.PR != 9 {
		t.Errorf("inline comment: %+v", c)
	}
}

func TestPRsFor(t *testing.T) {
	var queries []string
	r, _ := testRepo(t, func(w http.ResponseWriter, req *http.Request) {
		q := req.URL.Query()
		queries = append(queries, q.Get("q"))
		if len(q["state"]) != 4 {
			t.Errorf("states: %v", q["state"])
		}
		pr := map[string]any{"id": 4, "state": "MERGED", "title": "Fix", "description": "PROJ-1",
			"source": map[string]any{"branch": map[string]string{"name": "agent/issue-PROJ-1"}, "commit": map[string]string{"hash": "abc123def456"}}}
		if strings.Contains(q.Get("q"), "description") {
			pr = map[string]any{"id": 6, "state": "DECLINED", "description": "about PROJ-1",
				"source": map[string]any{"branch": map[string]string{"name": "other"}, "commit": map[string]string{"hash": "fff"}}}
		}
		json.NewEncoder(w).Encode(map[string]any{"values": []any{pr}})
	})
	prs, err := r.PRsFor(context.Background(), "agent/issue-PROJ-1", "PROJ-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(prs) != 2 || prs[0].State != core.PRMerged || prs[0].HeadSHA != "abc123def456" || prs[1].State != core.PRClosed {
		t.Fatalf("prs: %+v", prs)
	}
	if queries[0] != `source.branch.name="agent/issue-PROJ-1"` || queries[1] != `description ~ "PROJ-1"` {
		t.Errorf("queries: %q", queries)
	}
}

func TestChecks(t *testing.T) {
	c := &Conn{preview: mustRe("(?i)preview|deploy")}
	ch := c.evaluate([]rawStatus{
		{Key: "build", State: "SUCCESSFUL", URL: "https://ci/1"},
		{Key: "netlify-deploy-preview", State: "SUCCESSFUL", URL: "https://preview.example"},
	})
	if !ch.Complete || !ch.Any || len(ch.Failed) != 0 || ch.Preview != "https://preview.example" {
		t.Errorf("passed: %+v", ch)
	}
	ch = c.evaluate([]rawStatus{{Key: "build", Name: "Pipeline", State: "FAILED"}, {Key: "lint", State: "INPROGRESS"}})
	if ch.Complete || len(ch.Failed) != 1 || ch.Failed[0] != "Pipeline" {
		t.Errorf("failing: %+v", ch)
	}
	if ch := c.evaluate(nil); ch.Complete || ch.Any {
		t.Errorf("none: %+v", ch)
	}
}

func TestEnsureWebhook(t *testing.T) {
	var methods []string
	r, _ := testRepo(t, func(w http.ResponseWriter, req *http.Request) {
		methods = append(methods, req.Method+" "+req.URL.Path)
		b, _ := io.ReadAll(req.Body)
		if !strings.Contains(string(b), `"secret":"s3"`) || !strings.Contains(string(b), "repo:commit_status_updated") {
			t.Errorf("body: %s", b)
		}
		if req.Method == "PUT" {
			w.WriteHeader(404)
			return
		}
		w.Write([]byte(`{"uuid":"{new}"}`))
	})
	id, err := r.EnsureWebhook(context.Background(), "https://x/hooks/p/bb", "s3", "{old}")
	if err != nil || id != "{new}" {
		t.Fatalf("id %q err %v", id, err)
	}
	if len(methods) != 2 || !strings.HasPrefix(methods[0], "PUT") || !strings.HasPrefix(methods[1], "POST /repositories/acme/site/hooks") {
		t.Errorf("calls: %v", methods)
	}
}

func TestParseWebhook(t *testing.T) {
	r := &Repo{ws: "acme", slug: "site"}
	deliver := func(event, body string, sign bool) ([]core.Signal, error) {
		h := http.Header{}
		h.Set("X-Event-Key", event)
		if sign {
			h.Set("X-Hub-Signature", adapter.Sign("sec", []byte(body)))
		}
		return r.ParseWebhook(h, []byte(body), "sec")
	}
	pr := `"pullrequest":{"id":7,"state":"OPEN","source":{"branch":{"name":"agent/issue-3"},"commit":{"hash":"abc"}}},"repository":{"full_name":"acme/site"}`
	if _, err := deliver("pullrequest:created", "{"+pr+"}", false); err == nil {
		t.Error("unsigned delivery accepted")
	}
	sigs, err := deliver("pullrequest:updated", "{"+pr+"}", true)
	if err != nil || len(sigs) != 1 || sigs[0].Type != core.SigPRChanged || sigs[0].PR.HeadSHA != "abc" {
		t.Fatalf("updated: %+v %v", sigs, err)
	}
	sigs, _ = deliver("pullrequest:fulfilled", "{"+pr+"}", true)
	if len(sigs) != 1 || sigs[0].Type != core.SigPRClosed || sigs[0].PR.State != core.PRMerged {
		t.Errorf("fulfilled: %+v", sigs)
	}
	sigs, _ = deliver("pullrequest:rejected", "{"+pr+"}", true)
	if len(sigs) != 1 || sigs[0].PR.State != core.PRClosed {
		t.Errorf("rejected: %+v", sigs)
	}
	sigs, _ = deliver("pullrequest:comment_created", `{`+pr+`,"comment":{"id":11,"content":{"raw":"hi"},"user":{"display_name":"Ann"},"inline":{"path":"x.go","to":3}}}`, true)
	if len(sigs) != 1 || sigs[0].Type != core.SigPRComment || sigs[0].Comment.Kind != core.OnInline || sigs[0].Comment.ID != "11" {
		t.Errorf("comment: %+v", sigs)
	}
	sigs, _ = deliver("repo:commit_status_updated", `{"commit_status":{"commit":{"hash":"def"}},"repository":{"full_name":"acme/site"}}`, true)
	if len(sigs) != 1 || sigs[0].Type != core.SigChecks || sigs[0].SHA != "def" {
		t.Errorf("status: %+v", sigs)
	}
	sigs, _ = deliver("pullrequest:created", `{"pullrequest":{"id":1},"repository":{"full_name":"other/repo"}}`, true)
	if len(sigs) != 0 {
		t.Errorf("other repo: %+v", sigs)
	}
}

func mustRe(s string) *regexp.Regexp { return regexp.MustCompile(s) }
