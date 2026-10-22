package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func runGit(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@t", "-c", "user.name=t", "-c", "commit.gpgsign=false"}, args...)...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
}

func TestSetupAndRemove(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "repo")
	_ = os.MkdirAll(root, 0o755)
	runGit(t, root, "init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(root, "lock.txt"), []byte("v1"), 0o644)
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-qm", "init")
	_ = os.WriteFile(filepath.Join(root, ".env"), []byte("A=1"), 0o644)
	var log strings.Builder
	spec := Spec{Root: root, Path: Dir(filepath.Join(dir, "data"), "demo", "PROJ-1"), Branch: "agent/issue-PROJ-1", Base: "main",
		Copy: []string{".env"}, Setup: "echo ran >> setup.log", SetupWhen: []string{"lock.txt"}, Log: &log}
	path, err := Setup(spec)
	if err != nil {
		t.Fatal(err, log.String())
	}
	if data, _ := os.ReadFile(filepath.Join(path, ".env")); string(data) != "A=1" {
		t.Fatal(".env wasn't copied")
	}
	if _, err := Setup(spec); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(path, "setup.log")); strings.Count(string(data), "ran") != 1 {
		t.Fatalf("setup ran %q; want once while the lockfile is unchanged", data)
	}
	if err := Remove(spec); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("the worktree is still there")
	}
	if out, _ := exec.Command("git", "-C", root, "branch", "--list", spec.Branch).Output(); !strings.Contains(string(out), spec.Branch) {
		t.Fatal("the branch went with the worktree")
	}
}

func TestSetupSkippedWithoutWatchedFiles(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "repo")
	_ = os.MkdirAll(root, 0o755)
	runGit(t, root, "init", "-q", "-b", "main")
	_ = os.WriteFile(filepath.Join(root, "go.mod"), []byte("module x"), 0o644)
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-qm", "init")
	var log strings.Builder
	spec := Spec{Root: root, Path: Dir(filepath.Join(dir, "data"), "demo", "1"), Branch: "agent/issue-1", Base: "main",
		Setup: "exit 1", SetupWhen: []string{"pnpm-lock.yaml"}, Log: &log}
	if _, err := Setup(spec); err != nil {
		t.Fatalf("setup ran with none of its watched files in the checkout: %v", err)
	}
	_ = Remove(spec)
}

func TestFresh(t *testing.T) {
	dir := t.TempDir()
	origin, root, other := filepath.Join(dir, "origin.git"), filepath.Join(dir, "repo"), filepath.Join(dir, "other")
	runGit(t, dir, "init", "-q", "--bare", "-b", "main", origin)
	runGit(t, dir, "clone", "-q", origin, root)
	_ = os.WriteFile(filepath.Join(root, "a.txt"), []byte("v1"), 0o644)
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-qm", "one")
	runGit(t, root, "push", "-q", "origin", "HEAD:main")
	// A later commit pushed from elsewhere: the fresh checkout has it though the main checkout doesn't.
	runGit(t, dir, "clone", "-q", origin, other)
	_ = os.WriteFile(filepath.Join(other, "a.txt"), []byte("v2"), 0o644)
	runGit(t, other, "commit", "-qam", "two")
	runGit(t, other, "push", "-q", "origin", "HEAD:main")
	_ = os.WriteFile(filepath.Join(root, ".env"), []byte("A=1"), 0o644)
	var log strings.Builder
	spec := Spec{Root: root, Path: filepath.Join(dir, "data", "job-1"), Base: "main", Copy: []string{".env"}, Setup: "echo ran >> setup.log", Log: &log}
	path, err := Fresh(spec)
	if err != nil {
		t.Fatal(err, log.String())
	}
	if data, _ := os.ReadFile(filepath.Join(path, "a.txt")); string(data) != "v2" {
		t.Errorf("a.txt = %q; want the remote head's", data)
	}
	if data, _ := os.ReadFile(filepath.Join(path, ".env")); string(data) != "A=1" {
		t.Error(".env wasn't copied")
	}
	if data, _ := os.ReadFile(filepath.Join(path, "setup.log")); string(data) != "ran\n" {
		t.Errorf("setup.log = %q", data)
	}
	if data, _ := os.ReadFile(filepath.Join(root, "a.txt")); string(data) != "v1" {
		t.Error("the main checkout moved")
	}
	if err := Remove(spec); err != nil {
		t.Fatal(err)
	}
	if out, _ := exec.Command("git", "-C", root, "worktree", "list").Output(); strings.Contains(string(out), "job-1") {
		t.Errorf("worktree still listed: %s", out)
	}
}

func TestSetupFromAnotherRemote(t *testing.T) {
	dir := t.TempDir()
	upstream, root := filepath.Join(dir, "upstream.git"), filepath.Join(dir, "repo")
	runGit(t, dir, "init", "-q", "--bare", "-b", "main", upstream)
	runGit(t, dir, "clone", "-q", "-o", "upstream", upstream, root)
	_ = os.WriteFile(filepath.Join(root, "a.txt"), []byte("v1"), 0o644)
	runGit(t, root, "add", ".")
	runGit(t, root, "commit", "-qm", "one")
	runGit(t, root, "push", "-q", "upstream", "HEAD:main", "HEAD:refs/heads/agent/issue-2")
	var log strings.Builder
	spec := Spec{Root: root, Remote: "upstream", Path: filepath.Join(dir, "data", "issue-2"), Branch: "agent/issue-2", Base: "main", Log: &log}
	path, err := Setup(spec)
	if err != nil {
		t.Fatal(err, log.String())
	}
	if strings.Contains(log.String(), "fetch failed") {
		t.Fatalf("fetched from origin: %s", log.String())
	}
	if out, _ := exec.Command("git", "-C", path, "rev-parse", "--abbrev-ref", "@{upstream}").Output(); strings.TrimSpace(string(out)) != "upstream/agent/issue-2" {
		t.Fatalf("tracks %q", out)
	}
}
