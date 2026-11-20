package toolbelt

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/alsiola/open-supervisor/internal/adapter"
)

// adopt gives a PR opened outside any issue (a sweep's, a triage's, a job's) an issue to own it, so its reviews,
// failed checks and conflicts reach an agent the way an issue's PR's do: it opens the issue, moves the head's
// commits to the issue's branch, and links the body to the issue, so merging the PR closes it. It returns the
// branch and body to open the PR with. When the issue can't be opened it says so on log and leaves them as they
// were; when the branch can't be moved the PR still links the issue, from head.
func (s *session) adopt(ctx context.Context, title, body, head string, log io.Writer) (string, string) {
	opener, ok := s.b.Tracker.(adapter.IssueOpener)
	if !ok {
		fmt.Fprintf(log, "%s can't open issues, so no agent will watch this PR's reviews and checks\n", s.b.Tracker.Describe())
		return head, body
	}
	issue, err := opener.OpenIssue(ctx, title, adoptBody(head, body), nil)
	if err != nil {
		fmt.Fprintf(log, "couldn't open an issue to own the PR (%v), so no agent will watch its reviews and checks\n", err)
		return head, body
	}
	t := s.b.Tracker
	body = withRef(body, t.RefFor(issue.Key, true), t.RefFor(issue.Key, false), false, false)
	branch := s.b.BranchFor(issue.Key)
	if err := moveBranch("", s.b.Git.RemoteName(), head, branch); err != nil {
		fmt.Fprintf(log, "opened %s to own the PR, but couldn't move %s to %s (%v); opening it from %s\n", issue.URL, head, branch, err, head)
		return head, body
	}
	fmt.Fprintf(log, "opened %s to own the PR, from its branch %s\n", issue.URL, branch)
	return branch, body
}

// adoptBody is the body of the issue that owns a PR opened outside any issue: what the agent working it is there
// for, and the PR's own description.
func adoptBody(head, prBody string) string {
	quoted := "> " + strings.ReplaceAll(strings.TrimSpace(prBody), "\n", "\n> ")
	return "This issue owns a PR that was opened outside any issue (by the daily sweep or triage, or one of the project's jobs), " +
		"so its reviews, failed checks and conflicts with the base branch reach an agent. Merging the PR closes it.\n\n" +
		"Nothing new to build: the PR has the change. Look after it: answer its reviews and comments, fix what fails, " +
		"rebase it when it conflicts with the base branch, and report when it is ready to merge.\n\n" +
		"It was first pushed as `" + head + "`. The PR's description:\n\n" + quoted
}

// moveBranch pushes from's commits, the local branch or else the remote's, to the branch to on remote, and
// deletes from on remote once they are there. dir is the checkout to run git in ("" for the current directory).
func moveBranch(dir, remote, from, to string) error {
	if remote == "" {
		remote = "origin"
	}
	src := "refs/heads/" + from
	if _, err := runGit(dir, "rev-parse", "--verify", "-q", src); err != nil {
		src = "refs/remotes/" + remote + "/" + from
	}
	if _, err := runGit(dir, "push", "-q", remote, src+":refs/heads/"+to); err != nil {
		return err
	}
	// The PR goes up from to; a copy left under the old name only clutters the remote.
	_, _ = runGit(dir, "push", "-q", remote, "--delete", from)
	return nil
}

func runGit(dir string, args ...string) (string, error) {
	name := args[0]
	if dir != "" {
		args = append([]string{"-C", dir}, args...)
	}
	out, err := exec.Command("git", args...).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
