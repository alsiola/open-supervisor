package toolbelt

import (
	"context"
	"strings"
	"testing"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/core"
	"github.com/alsiola/open-supervisor/internal/engine"
)

type stubTracker struct {
	adapter.Tracker
	issues map[string]core.Issue
}

func (t stubTracker) Issue(_ context.Context, key string) (core.Issue, error) {
	return t.issues[key], nil
}

func TestHoldForAllLeavesOutWhatHasHappened(t *testing.T) {
	tr := stubTracker{issues: map[string]core.Issue{"1": {Key: "1", Open: false}, "2": {Key: "2", Open: true}, "3": {Key: "3", Open: false}}}
	s := &session{b: &engine.Bound{Spec: &config.Project{Name: "web"}, Tracker: tr}, cfg: config.New("")}
	ctx := context.Background()
	// Any of them: one already closed means there's nothing to wait for.
	if _, err := s.holdFor(ctx, []string{"issue:1", "issue:2"}, false, ""); err == nil || !strings.Contains(err.Error(), "already closed") {
		t.Errorf("any: %v", err)
	}
	h, err := s.holdFor(ctx, []string{"issue:1", "issue:2"}, true, "")
	if err != nil || !h.All || len(h.Conds) != 1 || h.Conds[0].Ref != "2" {
		t.Errorf("all: %+v %v", h, err)
	}
	if _, err := s.holdFor(ctx, []string{"issue:1", "issue:3"}, true, ""); err == nil || !strings.Contains(err.Error(), "all of it has happened already") {
		t.Errorf("all met: %v", err)
	}
}
