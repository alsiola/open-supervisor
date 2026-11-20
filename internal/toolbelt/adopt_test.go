package toolbelt

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/core"
	"github.com/alsiola/open-supervisor/internal/engine"
)

// openerTracker opens issue 41 (or fails with err) and refers to issues the way GitHub does.
type openerTracker struct {
	adapter.Tracker
	err         error
	title, body string
	opened      int
}

func (t *openerTracker) Describe() string { return "stub" }

func (t *openerTracker) OpenIssue(_ context.Context, title, body string, _ []string) (core.Issue, error) {
	t.opened++
	t.title, t.body = title, body
	if t.err != nil {
		return core.Issue{}, t.err
	}
	return core.Issue{Key: "41", URL: "https://x/issues/41"}, nil
}

func (t *openerTracker) RefFor(key string, closes bool) string {
	if closes {
		return "Closes #" + key
	}
	return "Refs #" + key
}

// gitRepo makes a checkout with a bare remote, a commit on main, and the branch sweep pushed with one more.
func gitRepo(t *testing.T) (checkout, remote string) {
	t.Helper()
	dir := t.TempDir()
	remote, checkout = filepath.Join(dir, "remote.git"), filepath.Join(dir, "checkout")
	run := func(dir string, args ...string) string {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = append(cmd.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@x", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@x")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run(dir, "init", "-q", "--bare", remote)
	run(dir, "init", "-q", "-b", "main", checkout)
	run(checkout, "remote", "add", "origin", remote)
	run(checkout, "commit", "-q", "--allow-empty", "--no-gpg-sign", "-m", "first")
	run(checkout, "push", "-q", "origin", "main")
	run(checkout, "switch", "-q", "-c", "open-supervisor/sweep-2026-11-19")
	run(checkout, "commit", "-q", "--allow-empty", "--no-gpg-sign", "-m", "rules")
	run(checkout, "push", "-q", "-u", "origin", "open-supervisor/sweep-2026-11-19")
	return checkout, remote
}

func remoteBranches(t *testing.T, remote string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", remote, "for-each-ref", "--format=%(refname:short)", "refs/heads").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(strings.Fields(string(out)), " ")
}

func TestAdoptGivesAPROutsideAnIssueAnIssue(t *testing.T) {
	checkout, remote := gitRepo(t)
	t.Chdir(checkout)
	tr := &openerTracker{}
	s := &session{b: &engine.Bound{Spec: &config.Project{Name: "web"}, Tracker: tr, BranchTemplate: "agent/issue-{key}"}}
	var log strings.Builder
	head, body := s.adopt(context.Background(), "docs: fold corrections", "- a rule\n- another", "open-supervisor/sweep-2026-11-19", &log)
	if head != "agent/issue-41" {
		t.Errorf("head = %q, want the issue's branch (log: %s)", head, log.String())
	}
	if !strings.HasPrefix(body, "Closes #41\n\n- a rule") {
		t.Errorf("body = %q, want it to close the issue", body)
	}
	if tr.title != "docs: fold corrections" || !strings.Contains(tr.body, "> - a rule\n> - another") || !strings.Contains(tr.body, "`open-supervisor/sweep-2026-11-19`") {
		t.Errorf("issue = %q, %q", tr.title, tr.body)
	}
	// The commits are on the issue's branch, and the old name is gone from the remote.
	if got := remoteBranches(t, remote); got != "agent/issue-41 main" {
		t.Errorf("remote branches = %q", got)
	}
	tip, _ := exec.Command("git", "-C", remote, "log", "-1", "--format=%s", "agent/issue-41").Output()
	if strings.TrimSpace(string(tip)) != "rules" {
		t.Errorf("issue branch tip = %q", tip)
	}
}

func TestAdoptLeavesThePRAloneWhenNoIssueOpens(t *testing.T) {
	checkout, remote := gitRepo(t)
	t.Chdir(checkout)
	tr := &openerTracker{err: errors.New("403")}
	s := &session{b: &engine.Bound{Spec: &config.Project{Name: "web"}, Tracker: tr, BranchTemplate: "agent/issue-{key}"}}
	var log strings.Builder
	head, body := s.adopt(context.Background(), "t", "b", "open-supervisor/sweep-2026-11-19", &log)
	if head != "open-supervisor/sweep-2026-11-19" || body != "b" || !strings.Contains(log.String(), "403") {
		t.Errorf("got %q, %q, log %q", head, body, log.String())
	}
	if got := remoteBranches(t, remote); got != "main open-supervisor/sweep-2026-11-19" {
		t.Errorf("remote branches = %q", got)
	}
}

func TestAdoptLinksTheIssueWhenTheBranchCantMove(t *testing.T) {
	checkout, _ := gitRepo(t)
	t.Chdir(checkout)
	tr := &openerTracker{}
	s := &session{b: &engine.Bound{Spec: &config.Project{Name: "web"}, Tracker: tr, BranchTemplate: "agent/issue-{key}", Git: config.Git{Remote: "nowhere"}}}
	var log strings.Builder
	head, body := s.adopt(context.Background(), "t", "b", "open-supervisor/sweep-2026-11-19", &log)
	if head != "open-supervisor/sweep-2026-11-19" || !strings.HasPrefix(body, "Closes #41") || !strings.Contains(log.String(), "couldn't move") {
		t.Errorf("got %q, %q, log %q", head, body, log.String())
	}
}
