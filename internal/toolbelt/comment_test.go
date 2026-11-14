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
