package linear

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/core"
)

var rootRe = regexp.MustCompile(`^\s*(?:query|mutation)[^{]*\{\s*(\w+)`)

// fake is a Linear API answering by which root field the query asks for.
type fake struct {
	mu      sync.Mutex
	answers map[string]func(vars map[string]any) any
	calls   []string
	auth    string
}

func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Query     string         `json:"query"`
		Variables map[string]any `json:"variables"`
	}
	json.NewDecoder(r.Body).Decode(&req)
	f.mu.Lock()
	f.auth = r.Header.Get("Authorization")
	f.mu.Unlock()
	if m := rootRe.FindStringSubmatch(req.Query); m != nil {
		if answer := f.answers[m[1]]; answer != nil {
			f.mu.Lock()
			f.calls = append(f.calls, m[1])
			f.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"data": answer(req.Variables)})
			return
		}
	}
	w.WriteHeader(400)
	json.NewEncoder(w).Encode(map[string]any{"errors": []any{map[string]string{"message": "unexpected query: " + req.Query}}})
}

func testTracker(t *testing.T, binding adapter.Values, f *fake) *Tracker {
	t.Helper()
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	conn, err := open(adapter.Values{"token": "lin_api_x", "apiUrl": srv.URL + "/graphql", "agentAccount": "robot@x"},
		adapter.Env{Secret: func(s string) (string, error) { return s, nil }, HTTP: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	if binding == nil {
		binding = adapter.Values{"team": "ENG"}
	}
	tr, err := conn.(*Conn).Tracker(binding, adapter.ProjectInfo{})
	if err != nil {
		t.Fatal(err)
	}
	return tr.(*Tracker)
}

func issueJSON(key, stateName, stateType string, extra map[string]any) map[string]any {
	i := map[string]any{
		"id": "uuid-" + key, "identifier": key, "title": "T " + key, "description": "body", "url": "https://linear.app/acme/issue/" + key,
		"createdAt": "2026-10-01T10:00:00Z", "updatedAt": "2026-10-02T10:00:00Z", "priorityLabel": "High",
		"state":  map[string]any{"id": "st-" + stateName, "name": stateName, "type": stateType},
		"team":   map[string]any{"key": "ENG", "name": "Engineering"},
		"labels": map[string]any{"nodes": []any{map[string]any{"id": "l-web", "name": "web"}}},
	}
	for k, v := range extra {
		i[k] = v
	}
	return i
}

func TestCandidatesAndAttrs(t *testing.T) {
	f := &fake{answers: map[string]func(map[string]any) any{
		"issues": func(vars map[string]any) any {
			if vars["after"] == nil {
				return map[string]any{"issues": map[string]any{
					"nodes": []any{
						issueJSON("ENG-1", "Todo", "unstarted", map[string]any{
							"project":  map[string]any{"name": "API v2"},
							"cycle":    map[string]any{"number": 12, "name": "Sprint 12"},
							"assignee": map[string]any{"id": "u1", "name": "sam", "displayName": "Sam", "email": "sam@x"},
							"estimate": 3.0,
							"parent":   map[string]any{"identifier": "ENG-0"},
						}),
						issueJSON("ENG-2", "Backlog", "backlog", nil),
					},
					"pageInfo": map[string]any{"hasNextPage": true, "endCursor": "c1"},
				}}
			}
			return map[string]any{"issues": map[string]any{
				"nodes":    []any{issueJSON("ENG-3", "In Progress", "started", nil)},
				"pageInfo": map[string]any{"hasNextPage": false},
			}}
		},
	}}
	tr := testTracker(t, nil, f)
	got, err := tr.Candidates(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || got[0].Key != "ENG-1" || got[1].Key != "ENG-2" || got[2].Key != "ENG-3" {
		t.Fatalf("candidates: %+v", got)
	}
	if got[0].Waiting || !got[1].Waiting || got[2].Waiting {
		t.Fatalf("only the backlog issue waits: %v %v %v", got[0].Waiting, got[1].Waiting, got[2].Waiting)
	}
	i := got[0]
	if !i.Open || i.Body != "body" || !i.HasLabel("web") || i.CreatedAt.Hour() != 10 {
		t.Fatalf("issue: %+v", i)
	}
	for name, want := range map[string]string{"team": "ENG,Engineering", "project": "API v2", "state": "Todo", "statetype": "unstarted",
		"priority": "High", "cycle": "12,Sprint 12", "assignee": "sam,Sam,sam@x", "estimate": "3", "parent": "ENG-0"} {
		if g := strings.Join(i.Attr(name), ","); g != want {
			t.Errorf("%s: got %q, want %q", name, g, want)
		}
	}
	if f.auth != "lin_api_x" {
		t.Errorf("a personal key goes bare: %q", f.auth)
	}

	// Claiming from Backlog too, by state name.
	f.calls = nil
	tr = testTracker(t, adapter.Values{"team": "eng", "states": "Backlog, started"}, f)
	got, _ = tr.Candidates(context.Background())
	if len(got) != 3 || !got[0].Waiting || got[1].Waiting || got[2].Waiting {
		t.Fatalf("by name, ENG-1 (Todo) waits and the backlog doesn't: %+v", got)
	}
}

func TestBindingAndRefs(t *testing.T) {
	c := &Conn{}
	if _, err := c.Tracker(adapter.Values{"team": "not a key"}, adapter.ProjectInfo{}); err == nil {
		t.Error("a bad team key was taken")
	}
	b, err := c.Tracker(adapter.Values{"team": "eng, Ops"}, adapter.ProjectInfo{})
	if err != nil {
		t.Fatal(err)
	}
	tr := b.(*Tracker)
	if tr.Describe() != "linear ENG,OPS" || !tr.owns("ops-3") || tr.owns("WEB-1") {
		t.Fatalf("%s", tr.Describe())
	}
	if refs := tr.RefsIn("Closes eng-12, OPS-3; not XENG-4 or WEB-5. ENG-12"); strings.Join(refs, ",") != "ENG-12,OPS-3" {
		t.Errorf("refs: %v", refs)
	}
	if tr.RefFor("ENG-9") != "Closes ENG-9" {
		t.Error("RefFor")
	}
	at := time.Date(2026, 10, 25, 9, 0, 0, 0, time.UTC)
	if got, ok := core.RunAtIn(tr.RunAtMarker(at)); !ok || !got.Equal(at) {
		t.Errorf("marker doesn't read back: %v", got)
	}
}

func TestMarks(t *testing.T) {
	tr := &Tracker{c: &Conn{agentAccount: "robot@x"}}
	body := withMark("Done.\n", core.AgentMark{From: "ENG-4"})
	c := tr.comment(rawComment{ID: "c1", Body: body, User: &rawUser{Name: "me"}})
	if c.Agent == nil || c.Agent.From != "ENG-4" {
		t.Fatalf("mark lost: %q → %+v", body, c.Agent)
	}
	// Linear may rewrite the emphasis.
	c = tr.comment(rawComment{Body: "ok\n\n_Posted by an open-supervisor agent_", User: &rawUser{Name: "me"}})
	if c.Agent == nil || c.Agent.From != "" {
		t.Fatalf("plain mark: %+v", c.Agent)
	}
	if c := tr.comment(rawComment{Body: "I was posted by an open-supervisor agent, apparently", User: &rawUser{Name: "me"}}); c.Agent != nil {
		t.Fatal("a mention mid-sentence isn't a mark")
	}
	if c := tr.comment(rawComment{Body: "hi", User: &rawUser{Email: "ROBOT@x"}}); c.Agent == nil {
		t.Fatal("the agent account's comment isn't marked")
	}
	if c := tr.comment(rawComment{Body: "deployed", BotActor: &struct {
		Name string `json:"name"`
	}{"GitHub"}}); !c.Bot || c.Author != "GitHub" {
		t.Fatalf("bot: %+v", c)
	}
}

func sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// deliver parses a delivery and resolves what it defers, as the engine does after answering.
func deliver(t *testing.T, tr *Tracker, payload map[string]any) ([]core.Signal, error) {
	t.Helper()
	if _, ok := payload["webhookTimestamp"]; !ok {
		payload["webhookTimestamp"] = time.Now().UnixMilli()
	}
	body, _ := json.Marshal(payload)
	h := http.Header{}
	h.Set("Linear-Signature", sign("s3cret", body))
	sigs, err := tr.ParseWebhook(h, body, "s3cret")
	if err != nil {
		return nil, err
	}
	var out []core.Signal
	for _, s := range sigs {
		if s.Type != core.SigDeferred {
			out = append(out, s)
			continue
		}
		more, err := s.Resolve(context.Background())
		if err != nil {
			return nil, err
		}
		out = append(out, more...)
	}
	return out, nil
}

func TestParseWebhook(t *testing.T) {
	state := map[string]any{"name": "Todo", "type": "unstarted"}
	f := &fake{answers: map[string]func(map[string]any) any{
		"issue": func(vars map[string]any) any {
			return map[string]any{"issue": issueJSON(vars["id"].(string), state["name"].(string), state["type"].(string), nil)}
		},
		"workflowStates": func(map[string]any) any {
			return map[string]any{"workflowStates": map[string]any{"nodes": []any{
				map[string]any{"id": "st-done", "name": "Done", "type": "completed", "team": map[string]any{"key": "ENG"}},
				map[string]any{"id": "st-todo", "name": "Todo", "type": "unstarted", "team": map[string]any{"key": "ENG"}},
			}}}
		},
		"comment": func(vars map[string]any) any {
			return map[string]any{"comment": map[string]any{"id": vars["id"], "body": "please add tests", "createdAt": "2026-10-20T09:00:00Z",
				"user": map[string]any{"id": "u2", "name": "alex"}, "issue": map[string]any{"identifier": "ENG-5"}}}
		},
	}}
	tr := testTracker(t, nil, f)

	body := []byte(`{"type":"Issue"}`)
	h := http.Header{}
	h.Set("Linear-Signature", sign("wrong", body))
	if _, err := tr.ParseWebhook(h, body, "s3cret"); err == nil {
		t.Fatal("a bad signature was taken")
	}
	if _, err := deliver(t, tr, map[string]any{"type": "Issue", "action": "create", "webhookTimestamp": time.Now().Add(-time.Hour).UnixMilli(),
		"data": map[string]any{"identifier": "ENG-5"}}); err == nil {
		t.Fatal("an hour-old delivery was taken")
	}

	sigs, err := deliver(t, tr, map[string]any{"type": "Issue", "action": "create", "data": map[string]any{"id": "x", "identifier": "ENG-5"}})
	if err != nil || len(sigs) != 1 || sigs[0].Type != core.SigIssueChanged || sigs[0].Issue == nil || sigs[0].Issue.Attr("team")[0] != "ENG" {
		t.Fatalf("create: %+v %v", sigs, err)
	}
	if sigs, _ := deliver(t, tr, map[string]any{"type": "Issue", "action": "create", "data": map[string]any{"identifier": "OPS-1"}}); len(sigs) != 0 {
		t.Fatalf("another team's issue: %+v", sigs)
	}
	sigs, _ = deliver(t, tr, map[string]any{"type": "Issue", "action": "update", "updatedFrom": map[string]any{"stateId": "st-done"},
		"data": map[string]any{"identifier": "ENG-5"}})
	if len(sigs) != 1 || sigs[0].Type != core.SigIssueReopened {
		t.Fatalf("reopened: %+v", sigs)
	}
	state["name"], state["type"] = "Done", "completed"
	sigs, _ = deliver(t, tr, map[string]any{"type": "Issue", "action": "update", "updatedFrom": map[string]any{"stateId": "st-todo"},
		"data": map[string]any{"identifier": "ENG-5"}})
	if len(sigs) != 1 || sigs[0].Type != core.SigIssueClosed || sigs[0].Key != "ENG-5" {
		t.Fatalf("closed: %+v", sigs)
	}
	state["name"], state["type"] = "Backlog", "backlog"
	if sigs, _ := deliver(t, tr, map[string]any{"type": "Issue", "action": "update", "updatedFrom": map[string]any{"labelIds": []any{}},
		"data": map[string]any{"identifier": "ENG-5"}}); len(sigs) != 1 || sigs[0].Issue == nil || !sigs[0].Issue.Waiting {
		t.Fatalf("a backlog issue comes through waiting: %+v", sigs)
	}
	// Reopened into the backlog: waiting, so only an agent already holding it hears.
	sigs, _ = deliver(t, tr, map[string]any{"type": "Issue", "action": "update", "updatedFrom": map[string]any{"stateId": "st-done"},
		"data": map[string]any{"identifier": "ENG-5"}})
	if len(sigs) != 1 || sigs[0].Type != core.SigIssueReopened || !sigs[0].Issue.Waiting {
		t.Fatalf("reopened to the backlog: %+v", sigs)
	}
	// The handler answers without reading the API.
	f.mu.Lock()
	before := len(f.calls)
	f.mu.Unlock()
	body, _ = json.Marshal(map[string]any{"type": "Issue", "action": "update", "webhookTimestamp": time.Now().UnixMilli(), "data": map[string]any{"identifier": "ENG-5"}})
	h = http.Header{}
	h.Set("Linear-Signature", sign("s3cret", body))
	if sigs, err := tr.ParseWebhook(h, body, "s3cret"); err != nil || len(sigs) != 1 || sigs[0].Type != core.SigDeferred || len(f.calls) != before {
		t.Fatalf("parse should defer: %+v %v (%d calls)", sigs, err, len(f.calls)-before)
	}
	sigs, err = deliver(t, tr, map[string]any{"type": "Comment", "action": "create", "data": map[string]any{"id": "c9"}})
	if err != nil || len(sigs) != 1 || sigs[0].Type != core.SigIssueComment || sigs[0].Key != "ENG-5" || sigs[0].Comment.Body != "please add tests" {
		t.Fatalf("comment: %+v %v", sigs, err)
	}
}

func TestEditLabelsAndTransition(t *testing.T) {
	var updates []map[string]any
	f := &fake{answers: map[string]func(map[string]any) any{
		"issue": func(vars map[string]any) any {
			return map[string]any{"issue": issueJSON(vars["id"].(string), "Todo", "unstarted", map[string]any{
				"labels": map[string]any{"nodes": []any{map[string]any{"id": "l-web", "name": "web"}, map[string]any{"id": "l-help", "name": "agent-needs-help"}}}})}
		},
		"issueLabels": func(map[string]any) any {
			return map[string]any{"issueLabels": map[string]any{"nodes": []any{
				map[string]any{"id": "l-web", "name": "web"},
				map[string]any{"id": "l-help", "name": "agent-needs-help"},
				map[string]any{"id": "l-review", "name": "Ready-For-Review", "team": map[string]any{"key": "ENG"}},
				map[string]any{"id": "l-ops", "name": "ops-only", "team": map[string]any{"key": "OPS"}},
			}, "pageInfo": map[string]any{"hasNextPage": false}}}
		},
		"workflowStates": func(map[string]any) any {
			return map[string]any{"workflowStates": map[string]any{"nodes": []any{
				map[string]any{"id": "st-review", "name": "In Review", "type": "started", "team": map[string]any{"key": "ENG"}},
			}}}
		},
		"issueUpdate": func(vars map[string]any) any {
			updates = append(updates, vars["input"].(map[string]any))
			return map[string]any{"issueUpdate": map[string]any{"success": true}}
		},
	}}
	tr := testTracker(t, adapter.Values{"team": "ENG", "stateReview": "in review"}, f)
	if err := tr.EditLabels(context.Background(), "ENG-1", []string{"ready-for-review", "ops-only"}, []string{"agent-needs-help"}); err != nil {
		t.Fatal(err)
	}
	if len(updates) != 1 || fmt.Sprint(updates[0]["labelIds"]) != "[l-web l-review]" {
		t.Fatalf("label update: %+v", updates)
	}
	if err := tr.Transition(context.Background(), "ENG-1", "review"); err != nil {
		t.Fatal(err)
	}
	if err := tr.Transition(context.Background(), "ENG-1", "done"); err != nil {
		t.Fatal(err)
	}
	if len(updates) != 2 || updates[1]["stateId"] != "st-review" {
		t.Fatalf("transition: %+v", updates)
	}
}
