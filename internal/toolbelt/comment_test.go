package toolbelt

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/core"
)

func TestPostCommentRecoversFromTransientFailures(t *testing.T) {
	saved := commentRetries
	commentRetries = []time.Duration{0, 0}
	t.Cleanup(func() { commentRetries = saved })
	bad := &adapter.APIError{Method: "POST", URL: "/comments", Status: 500}
	refused := fmt.Errorf("post: %w", &net.OpError{Op: "dial", Err: errors.New("connection refused")})
	mine := core.Comment{Body: "hello\n\n<!-- open-supervisor:agent -->", URL: "https://c/landed", Agent: &core.AgentMark{}}
	other := core.Comment{Body: "hello", URL: "https://c/person"}

	for _, c := range []struct {
		name     string
		errs     []error        // what each post returns, then success
		listed   []core.Comment // what a look finds
		want     string
		posts    int
		wantErr  bool
		wantLogs string
	}{
		{"fine first time", nil, nil, "https://c/1", 1, false, ""},
		{"500, then posted", []error{bad}, nil, "https://c/2", 2, false, "trying again"},
		{"500, but it landed", []error{bad}, []core.Comment{other, mine}, "https://c/landed", 1, false, "it was posted"},
		{"a person's comment isn't ours", []error{bad}, []core.Comment{other}, "https://c/2", 2, false, "trying again"},
		{"refused, then posted", []error{refused}, []core.Comment{mine}, "https://c/2", 2, false, "trying again"},
		{"500 every time", []error{bad, bad, bad}, nil, "", 3, true, ""},
		{"a 422 is final", []error{&adapter.APIError{Status: 422}}, nil, "", 1, true, ""},
	} {
		posts := 0
		post := func(context.Context) (string, error) {
			posts++
			if posts <= len(c.errs) {
				return "", c.errs[posts-1]
			}
			return "https://c/" + string(rune('0'+posts)), nil
		}
		list := func(context.Context, time.Time) ([]core.Comment, error) { return c.listed, nil }
		var log strings.Builder
		got, err := postComment(context.Background(), "hello", post, list, &log)
		if (err != nil) != c.wantErr || got != c.want || posts != c.posts || !strings.Contains(log.String(), c.wantLogs) {
			t.Errorf("%s: got %q %v after %d posts, log %q", c.name, got, err, posts, log.String())
		}
	}
}

func TestEditCommentKeepsTheMarkAndOnlyEditsTheAgentsOwn(t *testing.T) {
	cs := []core.Comment{
		{ID: "1", URL: "https://c/1", Body: "a person's"},
		{ID: "2", URL: "https://c/2", Body: "mine", Agent: &core.AgentMark{From: "12"}},
		{ID: "3", URL: "https://c/3", Body: "another agent's", Agent: &core.AgentMark{From: "40"}},
		{ID: "4", URL: "https://c/4", Body: "an agent's, unsigned", Agent: &core.AgentMark{}},
	}
	list := func(context.Context) ([]core.Comment, error) { return cs, nil }
	for _, c := range []struct {
		name, ref, wantID, wantErr string
		wantMark                   core.AgentMark
	}{
		{"by id", "2", "2", "", core.AgentMark{From: "12"}},
		{"by link", "https://c/2", "2", "", core.AgentMark{From: "12"}},
		{"an agent's with no issue on it", "4", "4", "", core.AgentMark{}},
		{"a person's", "1", "", "isn't an agent's", core.AgentMark{}},
		{"another agent's", "3", "", "agent's on 40", core.AgentMark{}},
		{"missing", "9", "", "no comment 9", core.AgentMark{}},
	} {
		var gotID string
		var gotMark core.AgentMark
		edit := func(_ context.Context, id string, mark core.AgentMark) (string, error) {
			gotID, gotMark = id, mark
			return "https://c/" + id, nil
		}
		url, err := editComment(context.Background(), c.ref, "#12", list, edit)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) || gotID != "" {
				t.Errorf("%s: got %q %v, edited %q", c.name, url, err, gotID)
			}
			continue
		}
		if err != nil || gotID != c.wantID || gotMark != c.wantMark || url != "https://c/"+c.wantID {
			t.Errorf("%s: got %q %v, edited %q with %+v", c.name, url, err, gotID, gotMark)
		}
	}
}
