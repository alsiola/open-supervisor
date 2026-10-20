package jira

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/core"
)

func testTracker(t *testing.T, binding adapter.Values, h http.HandlerFunc) *Tracker {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	conn, err := open(adapter.Values{"baseUrl": srv.URL, "auth": "basic", "email": "me@example.com", "token": "tok", "agentAccount": "bot-1"},
		adapter.Env{Secret: func(s string) (string, error) { return s, nil }, HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if binding == nil {
		binding = adapter.Values{"project": "PROJ"}
	}
	tr, err := conn.(*Conn).Tracker(binding, adapter.ProjectInfo{})
	if err != nil {
		t.Fatal(err)
	}
	return tr.(*Tracker)
}

func TestWikiConversion(t *testing.T) {
	md := "## Done\n\nFixed **the bug** in `main.go`, see [the PR](https://x/pr/1).\n\n- one\n  - nested\n1. first\n\n```go\nfmt.Println(\"**not bold**\")\n```\n[runAt: 2026-10-25T09:00Z]\n![shot](https://img/a.png)"
	wiki := ToWiki(md)
	for _, want := range []string{"h2. Done", "*the bug*", "{{main.go}}", "[the PR|https://x/pr/1]", "* one", "** nested", "# first", "{code:go}", "fmt.Println(\"**not bold**\")", "{code}", `\[runAt: 2026-10-25T09:00Z]`, "!https://img/a.png!"} {
		if !strings.Contains(wiki, want) {
			t.Errorf("wiki lacks %q:\n%s", want, wiki)
		}
	}
	back := ToMarkdown(wiki)
	for _, want := range []string{"## Done", "**the bug**", "`main.go`", "[the PR](https://x/pr/1)", "- one", "  - nested", "1. first", "```go", "```", "[runAt: 2026-10-25T09:00Z]"} {
		if !strings.Contains(back, want) {
			t.Errorf("markdown lacks %q:\n%s", want, back)
		}
	}
	if at, ok := core.RunAtIn(back); !ok || at.Format(time.RFC3339) != "2026-10-25T09:00:00Z" {
		t.Errorf("runAt lost in the round trip: %v %v", at, ok)
	}
	if got := ToMarkdown("*a* and *b* but not 2*3*4"); got != "**a** and **b** but not 2*3*4" {
		t.Errorf("bold: %q", got)
	}
}

func TestRefs(t *testing.T) {
	bound, _ := (&Conn{}).Tracker(adapter.Values{"project": "proj"}, adapter.ProjectInfo{})
	tr := bound.(*Tracker)
	got := tr.RefsIn("Fixes proj-12 and PROJ-3, not XPROJ-4 or OTHER-5; PROJ-12 again")
	if len(got) != 2 || got[0] != "PROJ-12" || got[1] != "PROJ-3" {
		t.Errorf("refs: %v", got)
	}
	if tr.RefFor("PROJ-9") != "PROJ-9" {
		t.Error("RefFor")
	}
	if m := tr.RunAtMarker(time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)); m != "[runAt: 2026-10-25T09:00Z]" {
		t.Errorf("marker %q", m)
	}
	if _, err := (&Conn{}).Tracker(adapter.Values{"project": "not a key"}, adapter.ProjectInfo{}); err == nil {
		t.Error("bad key accepted")
	}
}

func TestCandidatesPaginationAndFallback(t *testing.T) {
	calls := 0
	tr := testTracker(t, adapter.Values{"project": "PROJ", "jql": "component = Web"}, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if !strings.Contains(body["jql"].(string), `project = "PROJ" AND statusCategory != Done AND (component = Web)`) {
			t.Errorf("jql: %v", body["jql"])
		}
		issue := func(key string) map[string]any {
			return map[string]any{"key": key, "fields": map[string]any{"summary": "S " + key, "labels": []string{"x"}, "description": "h1. Hi",
				"status": map[string]any{"statusCategory": map[string]string{"key": "new"}}, "created": "2026-10-20T10:00:00.000+0000"}}
		}
		if body["nextPageToken"] == nil {
			json.NewEncoder(w).Encode(map[string]any{"issues": []any{issue("PROJ-1")}, "nextPageToken": "t2"})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"issues": []any{issue("PROJ-2")}, "isLast": true})
	})
	got, err := tr.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[1].Key != "PROJ-2" || !got[0].Open || got[0].Body != "# Hi" || got[0].CreatedAt.Hour() != 10 {
		t.Fatalf("issues: %+v", got)
	}

	legacy := testTracker(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rest/api/2/search/jql" {
			w.WriteHeader(404)
			return
		}
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		start := int(body["startAt"].(float64))
		json.NewEncoder(w).Encode(map[string]any{"total": 2, "issues": []any{map[string]any{"key": "PROJ-" + string(rune('1'+start)),
			"fields": map[string]any{"status": map[string]any{"statusCategory": map[string]string{"key": "indeterminate"}}}}}})
	})
	got, err = legacy.Candidates(context.Background())
	if err != nil || len(got) != 2 || got[1].Key != "PROJ-2" {
		t.Fatalf("legacy: %+v %v", got, err)
	}
}

func TestCommentsAndMarks(t *testing.T) {
	var posted map[string]any
	tr := testTracker(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "POST" {
			json.NewDecoder(r.Body).Decode(&posted)
			w.Write([]byte(`{"id":"99"}`))
			return
		}
		if r.URL.Query().Get("expand") != "properties" {
			t.Error("properties not expanded")
		}
		start := r.URL.Query().Get("startAt")
		if start == "0" {
			json.NewEncoder(w).Encode(map[string]any{"total": 3, "comments": []any{
				map[string]any{"id": "1", "body": "old", "author": map[string]string{"displayName": "Ann"}, "updated": "2026-10-01T00:00:00.000+0000"},
				map[string]any{"id": "2", "body": "*yes*", "author": map[string]string{"displayName": "Ann"}, "updated": "2026-10-20T00:00:00.000+0000"},
			}})
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"total": 3, "comments": []any{
			map[string]any{"id": "3", "body": "agent", "author": map[string]string{"displayName": "Me"}, "updated": "2026-10-20T01:00:00.000+0000",
				"properties": []any{map[string]any{"key": "open-supervisor", "value": map[string]any{"agent": true, "from": "PROJ-7"}}}},
		}})
	})
	got, err := tr.Comments(context.Background(), "PROJ-1", time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Body != "**yes**" || got[0].Agent != nil || got[1].Agent == nil || got[1].Agent.From != "PROJ-7" {
		t.Fatalf("comments: %+v", got)
	}
	if !strings.Contains(got[0].URL, "/browse/PROJ-1?focusedCommentId=2") {
		t.Errorf("url %s", got[0].URL)
	}
	url, err := tr.Comment(context.Background(), "PROJ-1", "**done**", core.AgentMark{From: "PROJ-1"})
	if err != nil || !strings.HasSuffix(url, "focusedCommentId=99") {
		t.Fatalf("comment: %s %v", url, err)
	}
	if posted["body"] != "*done*" {
		t.Errorf("posted body %v", posted["body"])
	}
	props, _ := json.Marshal(posted["properties"])
	if string(props) != `[{"key":"open-supervisor","value":{"agent":true,"from":"PROJ-1"}}]` {
		t.Errorf("properties %s", props)
	}

	// The agent account's comments count as an agent's even without the property.
	c := tr.comment("PROJ-1", rawComment{ID: "5", Author: &rawUser{AccountID: "bot-1"}})
	if c.Agent == nil || c.Agent.From != "" {
		t.Errorf("agent account: %+v", c.Agent)
	}
}

func TestLabels(t *testing.T) {
	var body string
	tr := testTracker(t, nil, func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		body = string(b)
	})
	if err := tr.EditLabels(context.Background(), "PROJ-1", []string{"agent-in-progress"}, []string{"ready-for-review"}); err != nil {
		t.Fatal(err)
	}
	if body != `{"update":{"labels":[{"add":"agent-in-progress"},{"remove":"ready-for-review"}]}}` {
		t.Errorf("body %s", body)
	}
	if err := tr.EditLabels(context.Background(), "PROJ-1", []string{"claude needs help"}, nil); err == nil || !strings.Contains(err.Error(), "claude needs help") {
		t.Errorf("spaced label: %v", err)
	}
}

func TestTransition(t *testing.T) {
	var chose string
	tr := testTracker(t, adapter.Values{"project": "PROJ", "transitionReview": "in review", "transitionDone": "Ship it"}, func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" {
			w.Write([]byte(`{"transitions":[{"id":"11","name":"Start","to":{"name":"In Progress"}},{"id":"21","name":"Review","to":{"name":"In Review"}},{"id":"31","name":"Ship it","to":{"name":"Closed"}}]}`))
			return
		}
		var b struct {
			Transition struct{ ID string } `json:"transition"`
		}
		json.NewDecoder(r.Body).Decode(&b)
		chose = b.Transition.ID
	})
	ctx := context.Background()
	if err := tr.Transition(ctx, "PROJ-1", "review"); err != nil || chose != "21" {
		t.Errorf("by status: %s %v", chose, err)
	}
	if err := tr.Transition(ctx, "PROJ-1", "done"); err != nil || chose != "31" {
		t.Errorf("by name: %s %v", chose, err)
	}
	chose = ""
	if err := tr.Transition(ctx, "PROJ-1", "help"); err != nil || chose != "" {
		t.Errorf("unconfigured state moved: %s %v", chose, err)
	}
	tr.statuses["help"] = "Nowhere"
	if err := tr.Transition(ctx, "PROJ-1", "help"); err == nil || !strings.Contains(err.Error(), "In Review") {
		t.Errorf("missing transition: %v", err)
	}
}

func TestReopenedAt(t *testing.T) {
	tr := testTracker(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/rest/api/2/status" {
			w.Write([]byte(`[{"id":"1","statusCategory":{"key":"new"}},{"id":"3","statusCategory":{"key":"indeterminate"}},{"id":"6","statusCategory":{"key":"done"}}]`))
			return
		}
		w.Write([]byte(`{"key":"PROJ-1","changelog":{"histories":[
			{"created":"2026-10-01T00:00:00.000+0000","items":[{"field":"status","from":"6","to":"1"}]},
			{"created":"2026-10-05T00:00:00.000+0000","items":[{"field":"status","from":"1","to":"3"}]},
			{"created":"2026-10-03T00:00:00.000+0000","items":[{"field":"status","from":"6","to":"3"}]}]}}`))
	})
	at, err := tr.ReopenedAt(context.Background(), "PROJ-1")
	if err != nil || at.Day() != 3 {
		t.Errorf("reopened at %v %v", at, err)
	}
}

func TestParseWebhook(t *testing.T) {
	tr := testTracker(t, nil, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/rest/api/2/status":
			w.Write([]byte(`[{"id":"6","statusCategory":{"key":"done"}},{"id":"1","statusCategory":{"key":"new"}}]`))
		case "/rest/api/2/issue/PROJ-4/comment/77":
			w.Write([]byte(`{"id":"77","body":"hello","author":{"displayName":"Ann"},"updated":"2026-10-20T00:00:00.000+0000"}`))
		default:
			w.WriteHeader(404)
		}
	})
	issue := func(cat string) string {
		return `"issue":{"key":"PROJ-4","fields":{"summary":"S","labels":["a"],"status":{"statusCategory":{"key":"` + cat + `"}}}}`
	}
	deliver := func(body string, sig string) ([]core.Signal, error) {
		h := http.Header{}
		if sig != "" {
			h.Set("X-Hub-Signature", sig)
		}
		return tr.ParseWebhook(h, []byte(body), "sec")
	}
	signed := func(body string) ([]core.Signal, error) { return deliver(body, adapter.Sign("sec", []byte(body))) }

	if _, err := deliver(`{"webhookEvent":"jira:issue_created",`+issue("new")+`}`, "sha256=00"); err == nil {
		t.Error("bad signature accepted")
	}
	sigs, err := deliver(`{"webhookEvent":"jira:issue_created",`+issue("new")+`}`, "")
	if err != nil || len(sigs) != 1 || sigs[0].Type != core.SigIssueChanged || sigs[0].Issue.Key != "PROJ-4" {
		t.Fatalf("created (unsigned): %+v %v", sigs, err)
	}
	sigs, _ = signed(`{"webhookEvent":"jira:issue_updated",` + issue("done") + `,"changelog":{"items":[{"field":"status","from":"1","to":"6"}]}}`)
	if len(sigs) != 1 || sigs[0].Type != core.SigIssueClosed {
		t.Errorf("closed: %+v", sigs)
	}
	sigs, _ = signed(`{"webhookEvent":"jira:issue_updated",` + issue("new") + `,"changelog":{"items":[{"field":"status","from":"6","to":"1"}]}}`)
	if len(sigs) != 1 || sigs[0].Type != core.SigIssueReopened {
		t.Errorf("reopened: %+v", sigs)
	}
	sigs, _ = signed(`{"webhookEvent":"jira:issue_updated",` + issue("new") + `,"changelog":{"items":[{"field":"labels","fromString":"","toString":"backlog"}]}}`)
	if len(sigs) != 1 || sigs[0].Type != core.SigIssueChanged {
		t.Errorf("labels: %+v", sigs)
	}
	sigs, err = signed(`{"webhookEvent":"comment_created",` + issue("new") + `,"comment":{"id":"77"}}`)
	if err != nil || len(sigs) != 1 || sigs[0].Type != core.SigIssueComment || sigs[0].Comment.Body != "hello" {
		t.Errorf("comment: %+v %v", sigs, err)
	}
	sigs, _ = signed(`{"webhookEvent":"jira:issue_created","issue":{"key":"OTHER-1","fields":{}}}`)
	if len(sigs) != 0 {
		t.Errorf("other project: %+v", sigs)
	}
}

func TestEnsureWebhookNeedsAdmin(t *testing.T) {
	tr := testTracker(t, nil, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) })
	_, err := tr.EnsureWebhook(context.Background(), "https://x/hooks/p/jira", "sec", "")
	if err == nil || !strings.Contains(err.Error(), "administrator") {
		t.Errorf("err %v", err)
	}
	ok := testTracker(t, nil, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"self":"https://x/rest/webhooks/1.0/webhook/42"}`))
	})
	id, err := ok.EnsureWebhook(context.Background(), "https://x/hooks/p/jira", "sec", "")
	if err != nil || id != "42" {
		t.Errorf("id %q %v", id, err)
	}
}
