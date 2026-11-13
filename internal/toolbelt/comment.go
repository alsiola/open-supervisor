package toolbelt

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/core"
)

// apiRetries are the pauses before each retry of an agent command's API call that failed transiently and can
// safely go again (a read, or an edit that sets a value). An agent can't wait for the supervisor's next poll.
var apiRetries = []time.Duration{2 * time.Second, 8 * time.Second}

// commentRetries are the pauses before each look for, then repost of, a comment whose post failed in a way
// that may have posted it anyway.
var commentRetries = []time.Duration{2 * time.Second, 8 * time.Second}

// postComment posts a comment with post. When that fails with a 5xx or a timeout, the comment may have been
// made all the same, so it looks among list's comments for one with the same body before posting again: a
// lost comment leaves the owner without news, and a doubled one is noise. list gets the time the first post
// was tried, less a minute's leeway for clocks.
func postComment(ctx context.Context, body string, post func(context.Context) (string, error), list func(context.Context, time.Time) ([]core.Comment, error), log io.Writer) (string, error) {
	start := time.Now()
	url, err := post(ctx)
	for _, pause := range commentRetries {
		if err == nil || !adapter.MayHaveLanded(err) && !adapter.NotSent(err) {
			return url, err
		}
		select {
		case <-time.After(pause):
		case <-ctx.Done():
			return "", err
		}
		if adapter.MayHaveLanded(err) {
			if found, ok := landed(ctx, body, start, list); ok {
				fmt.Fprintf(log, "posting the comment failed (%v), but it was posted\n", err)
				return found, nil
			}
		}
		fmt.Fprintf(log, "posting the comment failed (%v); trying again\n", err)
		url, err = post(ctx)
	}
	return url, err
}

// landed finds an agent's comment made since start whose body is body.
func landed(ctx context.Context, body string, start time.Time, list func(context.Context, time.Time) ([]core.Comment, error)) (string, bool) {
	cs, err := list(ctx, start.Add(-time.Minute))
	if err != nil {
		return "", false
	}
	want := strings.TrimSpace(body)
	for i := len(cs) - 1; i >= 0; i-- {
		c := cs[i]
		if c.Agent != nil && strings.Contains(c.Body, want) {
			return c.URL, true
		}
	}
	return "", false
}
