package toolbelt

import (
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/engine"
)

func TestUpstreamIssueURL(t *testing.T) {
	long := strings.Repeat("é", 4000) // 8000 bytes, cut mid-character
	u, err := url.Parse(upstreamIssueURL("Set $SUPERVISOR_SCRIPTS", long))
	if err != nil {
		t.Fatal(err)
	}
	if u.Host != "github.com" || u.Path != "/alsiola/open-supervisor/issues/new" {
		t.Fatalf("link: %s", u)
	}
	q := u.Query()
	if q.Get("title") != "Set $SUPERVISOR_SCRIPTS" {
		t.Fatalf("title: %q", q.Get("title"))
	}
	body := q.Get("body")
	if len(body) > 6100 || !strings.HasSuffix(body, "…") || strings.ContainsRune(body, '�') {
		t.Fatalf("body is %d bytes, ends %q", len(body), body[len(body)-8:])
	}
}

func TestUpstreamIssueIsNeverHeld(t *testing.T) {
	body := filepath.Join(t.TempDir(), "body.md")
	if err := os.WriteFile(body, []byte("agent pr list fails"), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := Command()
	cmd.SetArgs([]string{"issue", "create", "--upstream", "--title", "pr list fails", "--wait", "pr:1:merged", body})
	cmd.SetOut(new(strings.Builder))
	cmd.SetErr(new(strings.Builder))
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "can't hold the issue") {
		t.Fatalf("err: %v", err)
	}
}

func TestProblemNeedsAKind(t *testing.T) {
	cmd := Command()
	cmd.SetArgs([]string{"problem", "--kind", "bogus", "-"})
	cmd.SetOut(new(strings.Builder))
	cmd.SetErr(new(strings.Builder))
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--kind must be one of") {
		t.Fatalf("err: %v", err)
	}
}

func TestHoldForChecksBeforeAnyCall(t *testing.T) {
	s := &session{b: &engine.Bound{Spec: &config.Project{Name: "web"}}, cfg: config.New("")}
	ctx := context.Background()
	for _, c := range []struct {
		conds []string
		until string
		want  string
	}{
		{nil, "", "say what to wait for"},
		{nil, "2020-01-01T00:00Z", "already past"},
		{nil, "soon", "isn't a time"},
		{[]string{"pr:abc"}, "", "isn't a PR number"},
		{[]string{"job:backup:ok"}, "", "next run to end"},
		{[]string{"pr:club/12"}, "", "no project here is called club"},
	} {
		if _, err := s.holdFor(ctx, c.conds, c.until); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%v %q: got %v, want %q", c.conds, c.until, err, c.want)
		}
	}
	h, err := s.holdFor(ctx, nil, "2099-01-01T09:00Z")
	if err != nil || h.Until == nil || len(h.Conds) != 0 {
		t.Errorf("a time alone: %+v %v", h, err)
	}
}

func TestWithRef(t *testing.T) {
	for _, c := range []struct {
		name, body, ref, other string
		refers, force          bool
		want                   string
	}{
		{"added", "Rewrites the parser.", "Closes #12", "Refs #12", false, false, "Closes #12\n\nRewrites the parser."},
		{"kept", "Rewrites the parser.\n\nCloses #12", "Closes #12", "Refs #12", true, false, "Rewrites the parser.\n\nCloses #12"},
		// pr edit on a partial PR: the body's Refs line stands rather than gaining a Closes above it.
		{"own word kept", "Refs #12\n\nRewrites the parser.", "Closes #12", "Refs #12", true, false, "Refs #12\n\nRewrites the parser."},
		{"partial added", "Rewrites the parser.", "Refs #12", "Closes #12", false, true, "Refs #12\n\nRewrites the parser."},
		{"made partial", "closes #12\n\nRewrites the parser.", "Refs #12", "Closes #12", true, true, "Refs #12\n\nRewrites the parser."},
		{"closing again", "Refs #12\n\nRewrites the parser.", "Closes #12", "Refs #12", true, true, "Closes #12\n\nRewrites the parser."},
		// A closing word in the prose isn't the link line, so the chosen line goes on top.
		{"prose", "Fixes #12 in part.", "Refs #12", "Closes #12", true, true, "Refs #12\n\nFixes #12 in part."},
		{"one form", "Rewrites the parser.", "PROJ-12", "PROJ-12", false, true, "PROJ-12\n\nRewrites the parser."},
	} {
		if got := withRef(c.body, c.ref, c.other, c.refers, c.force); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}

func TestPartialIsForAnIssuesPR(t *testing.T) {
	cmd := Command()
	cmd.SetArgs([]string{"pr", "create", "--title", "t", "--body-file", "-", "--head", "sweep", "--partial"})
	cmd.SetOut(new(strings.Builder))
	cmd.SetErr(new(strings.Builder))
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--partial is for an issue's PR") {
		t.Fatalf("err: %v", err)
	}
}

func TestPREditNeedsAChange(t *testing.T) {
	cmd := Command()
	cmd.SetArgs([]string{"pr", "edit", "7"})
	cmd.SetOut(new(strings.Builder))
	cmd.SetErr(new(strings.Builder))
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--title, --body-file") {
		t.Fatalf("err: %v", err)
	}
}
