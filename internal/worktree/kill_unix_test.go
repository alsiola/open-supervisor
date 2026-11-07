//go:build !windows

package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/alsiola/open-supervisor/internal/proc"
)

// TestMain lets the test binary stand in for `agent worktree remove` when a test runs it with
// KILL_WITHIN_HELPER set to a worktree's path.
func TestMain(m *testing.M) {
	if path := os.Getenv("KILL_WITHIN_HELPER"); path != "" {
		killWithin(path)
		os.Stdout.WriteString("removed\n")
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// An agent runs the remove from a shell sitting in the worktree, piped through something else. The rest of its
// command shares its process group and has to live, or the command dies with 143 and loses its output; a
// process left running from the worktree by an earlier command, in its own group, still goes.
func TestKillWithinSparesTheCallersCommand(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wt")
	_ = os.MkdirAll(path, 0o755)
	server := exec.Command("sleep", "60")
	server.Dir = path
	server.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := server.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { _ = server.Wait() }()
	defer func() { _ = server.Process.Kill() }()

	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	shell := exec.Command("sh", "-c", `cd "$1" && sleep 0.2 && "$2" | cat; echo "exit $?"`, "sh", path, self)
	shell.Env = append(os.Environ(), "KILL_WITHIN_HELPER="+path)
	shell.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // like an agent's Bash tool call
	out, err := shell.CombinedOutput()
	if err != nil || string(out) != "removed\nexit 0\n" {
		t.Errorf("the calling command was disturbed: %v, %q", err, out)
	}
	time.Sleep(100 * time.Millisecond) // the server is reaped by its Wait
	if proc.Alive(server.Process.Pid) {
		t.Error("the process left running from the worktree survived")
	}
}

// A process run from inside the worktree by a global binary names no path in its command line, and what it
// starts may move elsewhere: both have to go.
func TestKillWithinFindsProcessesByWorkingDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wt")
	_ = os.MkdirAll(filepath.Join(path, "sub"), 0o755)
	outside := t.TempDir()
	other := exec.Command("sleep", "60")
	other.Dir = outside
	parent := exec.Command("sh", "-c", `(cd / && exec sleep 61) & echo $! > child.pid; wait`)
	parent.Dir = filepath.Join(path, "sub")
	parent.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // started by an earlier command, not this one
	for _, c := range []*exec.Cmd{other, parent} {
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { _ = c.Wait() }()
	}
	defer func() { _ = other.Process.Kill() }()
	var child int
	for i := 0; i < 100 && child == 0; i++ {
		time.Sleep(20 * time.Millisecond)
		data, _ := os.ReadFile(filepath.Join(path, "sub", "child.pid"))
		child, _ = strconv.Atoi(strings.TrimSpace(string(data)))
	}
	if child == 0 {
		t.Fatal("the child never started")
	}
	time.Sleep(100 * time.Millisecond) // let the child cd out
	killWithin(path)
	time.Sleep(100 * time.Millisecond) // the parent is reaped by its Wait
	if proc.Alive(parent.Process.Pid) {
		t.Error("the process sitting in the worktree survived")
	}
	if proc.Alive(child) {
		t.Error("its child, which had moved out of the worktree, survived")
	}
	if !proc.Alive(other.Process.Pid) {
		t.Error("a process outside the worktree was killed")
	}
}

// A process left running from a worktree that was removed without stopping it (by hand, or by an older
// version) goes on the next sweep; one running from a worktree still there stays.
func TestKillOrphansStopsProcessesFromWorktreesThatAreGone(t *testing.T) {
	base := filepath.Join(t.TempDir(), "worktrees", "web")
	gone, kept := filepath.Join(base, "issue-1"), filepath.Join(base, "issue-2")
	for _, dir := range []string{filepath.Join(gone, "sub"), kept} {
		_ = os.MkdirAll(dir, 0o755)
	}
	start := func(dir string) *exec.Cmd {
		c := exec.Command("sleep", "60")
		c.Dir = dir
		c.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err := c.Start(); err != nil {
			t.Fatal(err)
		}
		go func() { _ = c.Wait() }()
		t.Cleanup(func() { _ = c.Process.Kill() })
		return c
	}
	orphan, live := start(filepath.Join(gone, "sub")), start(kept)
	time.Sleep(100 * time.Millisecond)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}
	if n := KillOrphans(base); n != 1 {
		t.Errorf("stopped %d, want the one orphan", n)
	}
	time.Sleep(100 * time.Millisecond)
	if proc.Alive(orphan.Process.Pid) {
		t.Error("the process from the removed worktree survived")
	}
	if !proc.Alive(live.Process.Pid) {
		t.Error("the process from the worktree still there was stopped")
	}
}
