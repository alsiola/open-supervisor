// Package worktree gives each issue its own git worktree on its own branch, outside the project's checkout so
// nothing has to be ignored there. Setup copies the files the project lists (.env) on every run and runs the
// project's setup command when one of the files it watches (a lockfile) has changed. Remove stops anything
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

func (s Spec) logf(format string, args ...any) {
	if s.Log != nil {
		fmt.Fprintf(s.Log, format+"\n", args...)
	}
}

// Setup creates or reuses the worktree and returns its path.
func Setup(s Spec) (string, error) {
	if _, err := git(s.Root, "fetch", "-q", "origin"); err != nil {
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
	case refExists(s.Root, "refs/remotes/origin/"+s.Branch):
		if _, err := git(s.Root, "worktree", "add", "-q", "-b", s.Branch, s.Path, "origin/"+s.Branch); err != nil {
			return "", err
		}
		_, _ = git(s.Path, "branch", "-q", "--set-upstream-to", "origin/"+s.Branch)
	default:
		base := s.Base
		if base == "" {
			base = "main"
		}
		start := "origin/" + base
		if !refExists(s.Root, "refs/remotes/"+start) {
			start = base
		}
		if _, err := git(s.Root, "worktree", "add", "-q", "--no-track", "-b", s.Branch, s.Path, start); err != nil {
			return "", err
		}
	}
	// Copied on every run, not only when missing, so a reused worktree picks up keys added since.
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
	if s.Setup != "" {
		if err := runSetup(s); err != nil {
			return s.Path, err
		}
	}
	return s.Path, nil
}

func refExists(dir, ref string) bool {
	_, err := git(dir, "show-ref", "--verify", "-q", ref)
	return err == nil
}

// runSetup runs the setup command when what it depends on has changed since it last ran.
func runSetup(s Spec) error {
	h := sha256.New()
	h.Write([]byte(s.Setup))
	for _, name := range s.SetupWhen {
		if data, err := os.ReadFile(filepath.Join(s.Path, name)); err == nil {
			h.Write(data)
		}
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

// killWithin stops processes started from the worktree (a dev server keeps writing into it after the delete).
func killWithin(path string) {
	out, err := exec.Command("pgrep", "-f", path+string(filepath.Separator)).Output()
	if err != nil {
		return
	}
	self := os.Getpid()
	for _, f := range strings.Fields(string(out)) {
		pid, err := strconv.Atoi(f)
		if err != nil || pid == self {
			continue
		}
		if p, err := os.FindProcess(pid); err == nil {
			_ = p.Kill()
		}
	}
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
