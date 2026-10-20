package core

import (
	"strings"
	"testing"
	"time"
)

func TestRunAtIn(t *testing.T) {
	cases := map[string]string{
		"<!-- runAt: 2026-10-25T09:00Z -->":                                  "2026-10-25T09:00:00Z",
		"a\n[runAt: 2026-10-25T09:00:00+01:00]\nb":                           "2026-10-25T08:00:00Z",
		"<!-- runAt: 2026-10-25 -->":                                         "2026-10-25T00:00:00Z",
		"<!-- runAt: 2026-01-01 --> later <!-- runAt: 2026-02-01T10:30Z -->": "2026-02-01T10:30:00Z",
		`\[runAt: 2026-03-01T00:00Z]`:                                        "2026-03-01T00:00:00Z",
	}
	for in, want := range cases {
		got, ok := RunAtIn(in)
		if !ok || got.Format(time.RFC3339) != want {
			t.Errorf("%q: got %v %v, want %s", in, got, ok, want)
		}
	}
	if _, ok := RunAtIn("<!-- runAt: soon -->"); ok {
		t.Error("an unparseable time is not a marker")
	}
}

func TestRender(t *testing.T) {
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	out := Render([]Event{
		{Type: EvComment, Comment: &Comment{ID: "99", Kind: OnInline, Author: "ann", Body: "rename this", PR: 4, Path: "a.go", Line: 3, At: at}},
		{Type: EvChecks, PR: 4, SHA: "abcdef123", Failed: []string{"lint"}},
		{Type: EvMessage, Text: "hi", At: at},
	})
	for _, want := range []string{"Inline comment on PR #4 at a.go:3 by ann", "agent pr reply 4 99", "Checks failed on PR #4 at abcdef1: lint", "Message from the owner"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
}

func TestEventSame(t *testing.T) {
	at := time.Now()
	a := Event{Type: EvMessage, Text: "x", At: at}
	if !a.Same(Event{Type: EvMessage, Text: "x", At: at}) || a.Same(Event{Type: EvMessage, Text: "y", At: at}) {
		t.Fatal("Same on messages")
	}
	c := Event{Type: EvComment, Comment: &Comment{ID: "1", Kind: OnIssue}}
	if !c.Same(Event{Type: EvComment, Comment: &Comment{ID: "1", Kind: OnIssue}}) || c.Same(Event{Type: EvComment, Comment: &Comment{ID: "1", Kind: OnPR}}) {
		t.Fatal("Same on comments")
	}
}
