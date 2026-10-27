package toolbelt

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/core"
)

// flakyRepo answers CreatePR with err, and lists prs from the head branch after lists calls to PRsFor.
type flakyRepo struct {
	adapter.Repo
	err          error
	prs          []core.PR
	after, lists int
}

func (r *flakyRepo) CreatePR(context.Context, adapter.PRRequest) (core.PR, error) {
	return core.PR{}, r.err
}

func (r *flakyRepo) PRsFor(context.Context, string, string) ([]core.PR, error) {
	r.lists++
	if r.lists <= r.after {
		return nil, nil
	}
	return r.prs, nil
}

func TestCreatePRFindsThePRA5xxMade(t *testing.T) {
	saved := createLookups
	createLookups = []time.Duration{0, 0, 0}
	t.Cleanup(func() { createLookups = saved })
	req := adapter.PRRequest{Title: "t", Head: "agent/issue-1", Base: "main"}
	bad := &adapter.APIError{Method: "POST", URL: "/pulls", Status: 502, Body: "Server Error"}
	made := core.PR{ID: 7, State: core.PROpen, Branch: "agent/issue-1", Base: "main", URL: "https://x/7"}
	other := core.PR{ID: 3, State: core.PRClosed, Branch: "agent/issue-1", Base: "main"}

	for _, c := range []struct {
		name    string
		repo    *flakyRepo
		want    int
		wantErr string
	}{
		{"made, listed at once", &flakyRepo{err: bad, prs: []core.PR{other, made}}, 7, ""},
		{"made, listed late", &flakyRepo{err: bad, prs: []core.PR{made}, after: 2}, 7, ""},
		{"timed out, made", &flakyRepo{err: context.DeadlineExceeded, prs: []core.PR{made}}, 7, ""},
		{"not made", &flakyRepo{err: bad, prs: []core.PR{other}}, 0, "no open PR from agent/issue-1"},
		{"into another base", &flakyRepo{err: bad, prs: []core.PR{{ID: 9, State: core.PROpen, Branch: "agent/issue-1", Base: "dev"}}}, 0, "no open PR"},
		{"a 422 is final", &flakyRepo{err: &adapter.APIError{Status: 422, Body: "exists"}, prs: []core.PR{made}}, 0, "422"},
	} {
		var log strings.Builder
		pr, err := createPR(context.Background(), c.repo, req, &log)
		switch {
		case c.wantErr != "":
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%s: err %v, want %q", c.name, err, c.wantErr)
			}
		case err != nil || pr.ID != c.want || !strings.Contains(log.String(), "#7"):
			t.Errorf("%s: got #%d %v, log %q", c.name, pr.ID, err, log.String())
		}
	}
	plain := &flakyRepo{err: errors.New("bad credentials")}
	if _, err := createPR(context.Background(), plain, req, new(strings.Builder)); err == nil || plain.lists != 0 {
		t.Errorf("a plain error: %v after %d lookups, want it as is with none", err, plain.lists)
	}
}
