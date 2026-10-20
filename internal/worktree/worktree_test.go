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
