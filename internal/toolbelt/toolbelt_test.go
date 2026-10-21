package toolbelt

import (
	"net/url"
	"strings"
	"testing"
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

func TestProblemNeedsAKind(t *testing.T) {
	cmd := Command()
	cmd.SetArgs([]string{"problem", "--kind", "bogus", "-"})
	cmd.SetOut(new(strings.Builder))
	cmd.SetErr(new(strings.Builder))
	if err := cmd.Execute(); err == nil || !strings.Contains(err.Error(), "--kind must be one of") {
		t.Fatalf("err: %v", err)
	}
}
