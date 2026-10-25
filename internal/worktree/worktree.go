// Package worktree gives each issue its own git worktree on its own branch, outside the project's checkout so
// nothing has to be ignored there. Setup copies the files the project lists (.env) on every run and runs the
// project's setup command when one of the files it watches (a lockfile) has changed; Refresh copies the files
// alone, before each turn, so a resumed agent that doesn't run Setup again still sees keys added since. Remove stops anything
// still running from the worktree, removes it and keeps the branch, so nothing committed is lost.
package worktree

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// Spec is one issue's worktree.
type Spec struct {
	// Root is the project's main checkout.
	Root string
	// Path is where the worktree goes.
	Path   string
	Branch string
	// Remote is the remote to fetch from; empty is origin.
	Remote string
	// Base is the branch a new one starts from.
	Base      string
	Copy      []string
	Setup     string
	SetupWhen []string
	// Log receives what happened, a line at a time.
	Log io.Writer
}

var unsafe = regexp.MustCompile(`[^A-Za-z0-9._-]+`)

// Dir is where a project's issue worktree lives under the data directory.
func Dir(dataDir, project, key string) string {
	return filepath.Join(dataDir, "worktrees", project, "issue-"+unsafe.ReplaceAllString(key, "-"))
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %s", strings.Join(args, " "), strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// remote is the remote to fetch from.
func (s Spec) remote() string {
	if s.Remote != "" {
		return s.Remote
	}
	return "origin"
}

func (s Spec) logf(format string, args ...any) {
	if s.Log != nil {
		fmt.Fprintf(s.Log, format+"\n", args...)
	}
}

// Setup creates or reuses the worktree and returns its path.
func Setup(s Spec) (string, error) {
	if _, err := git(s.Root, "fetch", "-q", s.remote()); err != nil {
		s.logf("fetch failed, carrying on: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return "", err
	}
	_, statErr := os.Stat(s.Path)
	switch {
	case statErr == nil:
		s.logf("reusing %s", s.Path)
	case refExists(s.Root, "refs/heads/"+s.Branch):
		if _, err := git(s.Root, "worktree", "add", "-q", s.Path, s.Branch); err != nil {
			return "", err
		}
	case refExists(s.Root, "refs/remotes/"+s.remote()+"/"+s.Branch):
		if _, err := git(s.Root, "worktree", "add", "-q", "-b", s.Branch, s.Path, s.remote()+"/"+s.Branch); err != nil {
			return "", err
		}
		_, _ = git(s.Path, "branch", "-q", "--set-upstream-to", s.remote()+"/"+s.Branch)
	default:
		base := s.Base
		if base == "" {
			base = "main"
		}
		start := s.remote() + "/" + base
		if !refExists(s.Root, "refs/remotes/"+start) {
			start = base
		}
		if _, err := git(s.Root, "worktree", "add", "-q", "--no-track", "-b", s.Branch, s.Path, start); err != nil {
			return "", err
		}
	}
	// Copied on every run, not only when missing, so a reused worktree picks up keys added since.
	s.copyFiles()
	if s.Setup != "" {
		if err := runSetup(s); err != nil {
			return s.Path, err
		}
	}
	return s.Path, nil
}

// Refresh copies the project's files into the worktree again when it exists, so a key added to the main
// checkout's .env since Setup last ran reaches it. It fetches nothing and runs no setup.
func Refresh(s Spec) {
	if _, err := os.Stat(filepath.Join(s.Path, ".git")); err != nil {
		return
	}
	s.copyFiles()
}

func (s Spec) copyFiles() {
	for _, name := range s.Copy {
		from := filepath.Join(s.Root, name)
		to := filepath.Join(s.Path, name)
		if same(from, to) {
			continue
		}
		if err := copyFile(from, to); err == nil {
			s.logf("copied %s", name)
		}
	}
}

// Fresh makes a new worktree at the base branch's remote head, detached, for one run of a job; it copies the
// project's files into it and runs its setup whatever has changed. On a setup failure it returns the path
// with the error, so the worktree can be kept to look at.
func Fresh(s Spec) (string, error) {
	base := s.Base
	if base == "" {
		base = "main"
	}
	if _, err := git(s.Root, "fetch", "-q", s.remote(), base); err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o755); err != nil {
		return "", err
	}
	if _, err := git(s.Root, "worktree", "add", "-q", "--detach", s.Path, s.remote()+"/"+base); err != nil {
		return "", err
	}
	if head, err := git(s.Path, "rev-parse", "--short", "HEAD"); err == nil {
		s.logf("checked out %s/%s at %s in %s", s.remote(), base, head, s.Path)
	}
	s.copyFiles()
	if s.Setup != "" {
		if err := runSetup(s); err != nil {
			return s.Path, err
		}
	}
	return s.Path, nil
}

// Sync is how the main checkout stood against the base branch's remote head before a job ran in it.
type Sync struct {
	// Updated is how many commits it was fast-forwarded by.
	Updated int
	// Behind is how many commits of the remote head it still didn't have, and Stale why ("on branch x").
	// Stale is also set when the fetch failed, Behind then counting from the last fetch.
	Behind int
	Stale  string
}

// CatchUp fetches the base branch into the main checkout (Root) and fast-forwards it when it's on that branch.
// Uncommitted changes don't stop it: git leaves them be, and refuses when the update would touch one. A
// checkout without the remote has nothing to catch up with.
func CatchUp(s Spec) Sync {
	got := Behind(s)
	if got.Behind == 0 || got.Stale != "" {
		return got
	}
	base, head := s.head()
	branch, _ := git(s.Root, "symbolic-ref", "-q", "--short", "HEAD")
	if branch != base {
		if branch == "" {
			branch = "a detached HEAD"
		}
		got.Stale = "on " + branch + ", not " + base
		return got
	}
	if _, err := git(s.Root, "merge", "-q", "--ff-only", head); err != nil {
		switch msg := err.Error(); {
		case strings.Contains(msg, "would be overwritten"):
			got.Stale = "uncommitted changes to files the update touches"
		case strings.Contains(msg, "fast-forward") || strings.Contains(msg, "diverg"):
			got.Stale = base + " has commits " + head + " doesn't"
		default:
			got.Stale = "couldn't fast-forward: " + firstLine(msg)
		}
		return got
	}
	got.Updated, got.Behind = got.Behind, 0
	return got
}

// Behind fetches the base branch into the main checkout (Root) and counts the commits of its remote head that
// the checkout doesn't have, changing nothing else.
func Behind(s Spec) Sync {
	if _, err := git(s.Root, "remote", "get-url", s.remote()); err != nil {
		return Sync{}
	}
	var got Sync
	base, head := s.head()
	if _, err := git(s.Root, "fetch", "-q", s.remote(), base); err != nil {
		got.Stale = "couldn't fetch: " + firstLine(err.Error())
	}
	if !refExists(s.Root, "refs/remotes/"+head) {
		return got
	}
	if count, err := git(s.Root, "rev-list", "--count", "HEAD.."+head); err == nil {
		got.Behind, _ = strconv.Atoi(count)
	}
	return got
}

// head is the base branch and its remote-tracking ref.
func (s Spec) head() (string, string) {
	base := s.Base
	if base == "" {
		base = "main"
	}
	return base, s.remote() + "/" + base
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(s, "\n")
	return line
}

func refExists(dir, ref string) bool {
	_, err := git(dir, "show-ref", "--verify", "-q", ref)
	return err == nil
}

// runSetup runs the setup command when what it depends on has changed since it last ran. When it watches
// files and the checkout has none of them (a pnpm install in a repo with no pnpm-lock.yaml), it doesn't run.
func runSetup(s Spec) error {
	h := sha256.New()
	h.Write([]byte(s.Setup))
	found := false
	for _, name := range s.SetupWhen {
		if data, err := os.ReadFile(filepath.Join(s.Path, name)); err == nil {
			h.Write(data)
			found = true
		}
	}
	if len(s.SetupWhen) > 0 && !found {
		s.logf("skipping setup: none of %s is in the checkout", strings.Join(s.SetupWhen, ", "))
		return nil
	}
	sum := hex.EncodeToString(h.Sum(nil))
	stamp := filepath.Join(gitDir(s.Path), "open-supervisor-setup")
	if old, err := os.ReadFile(stamp); err == nil && strings.TrimSpace(string(old)) == sum {
		return nil
	}
	s.logf("running setup: %s", s.Setup)
	cmd := exec.Command("sh", "-c", s.Setup)
	cmd.Dir = s.Path
	cmd.Stdout = s.Log
	cmd.Stderr = s.Log
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("setup (%s): %w", s.Setup, err)
	}
	return os.WriteFile(stamp, []byte(sum+"\n"), 0o644)
}

// gitDir is the worktree's own git directory, where the setup stamp is kept out of the work tree.
func gitDir(path string) string {
	if d, err := git(path, "rev-parse", "--absolute-git-dir"); err == nil {
		return d
	}
	return path
}

// Remove stops anything running from the worktree and removes it. The branch stays.
func Remove(s Spec) error {
	if _, err := os.Stat(s.Path); err != nil {
		s.logf("no worktree at %s", s.Path)
		return nil
	}
	killWithin(s.Path)
	_, _ = git(s.Root, "worktree", "remove", "--force", s.Path)
	if err := os.RemoveAll(s.Path); err != nil {
		return err
	}
	_, _ = git(s.Root, "worktree", "prune")
	s.logf("removed %s", s.Path)
	return nil
}

// Unpushed says what removing the worktree would lose: uncommitted changes to tracked or new files (ignored
// ones, like .local/, don't count), or commits no remote-tracking branch has. It is "" when there is nothing.
func Unpushed(s Spec) (string, error) {
	if _, err := os.Stat(s.Path); err != nil {
		return "", nil
	}
	status, err := git(s.Path, "status", "--porcelain")
	if err != nil {
		return "", err
	}
	if status != "" {
		return "it has uncommitted changes", nil
	}
	count, err := git(s.Path, "rev-list", "--count", "HEAD", "--not", "--remotes="+s.remote())
	if err != nil {
		return "", err
	}
	if n, _ := strconv.Atoi(count); n > 0 {
		return fmt.Sprintf("%d commit(s) on it aren't pushed", n), nil
	}
	return "", nil
}

func same(a, b string) bool {
	x, err1 := os.ReadFile(a)
	y, err2 := os.ReadFile(b)
	return err1 == nil && err2 == nil && string(x) == string(y)
}

func copyFile(from, to string) error {
	data, err := os.ReadFile(from)
	if err != nil {
		return err
	}
	info, err := os.Stat(from)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(to), 0o755); err != nil {
		return err
	}
	return os.WriteFile(to, data, info.Mode().Perm())
}
