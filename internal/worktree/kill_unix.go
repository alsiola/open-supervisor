//go:build !windows

package worktree

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/alsiola/open-supervisor/internal/proc"
)

// killGrace is how long a process gets to stop after SIGTERM (an agent-browser daemon closes its Chrome) before
// it is killed.
var killGrace = 3 * time.Second

// killWithin stops processes started from the worktree (a dev server keeps writing into it after the delete),
// with everything they started. A process counts when the path is in its command line, or when its working
// directory is in the worktree: a global binary run from inside it (agent-browser, npx) never names the path.
// The caller, its ancestors and its process group are spared: an agent's shell may itself be sitting in the
// worktree, and so may the rest of the command it ran (the `tail` in `agent worktree remove | tail`), which would
// otherwise be stopped mid-command and lose its output. Everything the supervisor starts that outlives a command
// (turns, tasks, jobs) runs in its own session, so the caller's group never covers it.
func killWithin(path string) {
	roots := map[int]bool{}
	for _, pid := range pgrep(path + string(filepath.Separator)) {
		roots[pid] = true
	}
	for _, pid := range cwdWithin(path) {
		roots[pid] = true
	}
	if len(roots) == 0 {
		return
	}
	parents, groups := processTable()
	spared := map[int]bool{}
	for pid := os.Getpid(); pid > 1 && !spared[pid]; pid = parents[pid] {
		spared[pid] = true
	}
	own := syscall.Getpgrp()
	for pid, pgid := range groups {
		if pgid == own {
			spared[pid] = true
		}
	}
	var pids []int
	for _, pid := range withDescendants(roots, parents) {
		if !spared[pid] && pid > 1 {
			pids = append(pids, pid)
		}
	}
	for _, pid := range pids {
		_ = proc.Terminate(pid)
	}
	deadline := time.Now().Add(killGrace)
	for _, pid := range pids {
		for proc.Alive(pid) && time.Now().Before(deadline) {
			time.Sleep(50 * time.Millisecond)
		}
		if proc.Alive(pid) {
			_ = proc.Kill(pid)
		}
	}
}

func pgrep(pattern string) []int {
	out, _ := exec.Command("pgrep", "-f", pattern).Output()
	return pidList(strings.Fields(string(out)))
}

// cwdWithin lists the processes whose working directory is the path or under it, from /proc where there is
// one (Linux) and from lsof otherwise (macOS).
func cwdWithin(path string) []int {
	prefixes := []string{filepath.Clean(path)}
	if real, err := filepath.EvalSymlinks(path); err == nil && real != prefixes[0] {
		prefixes = append(prefixes, real)
	}
	within := func(dir string) bool {
		for _, p := range prefixes {
			if dir == p || strings.HasPrefix(dir, p+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}
	var pids []int
	if entries, err := os.ReadDir("/proc"); err == nil && len(entries) > 0 {
		if _, err := os.Readlink("/proc/self/cwd"); err == nil {
			for _, e := range entries {
				pid, err := strconv.Atoi(e.Name())
				if err != nil {
					continue
				}
				if dir, err := os.Readlink(filepath.Join("/proc", e.Name(), "cwd")); err == nil && within(dir) {
					pids = append(pids, pid)
				}
			}
			return pids
		}
	}
	// -Fpn prints a "p<pid>" line, then "fcwd" and "n<dir>" for each process.
	out, _ := exec.Command("lsof", "-w", "-d", "cwd", "-Fpn").Output()
	pid := 0
	for _, line := range strings.Split(string(out), "\n") {
		switch {
		case strings.HasPrefix(line, "p"):
			pid, _ = strconv.Atoi(line[1:])
		case strings.HasPrefix(line, "n") && pid > 0 && within(line[1:]):
			pids = append(pids, pid)
		}
	}
	return pids
}

// processTable maps every running process to its parent and to its process group.
func processTable() (parents, groups map[int]int) {
	out, _ := exec.Command("ps", "-A", "-o", "pid=,ppid=,pgid=").Output()
	parents, groups = map[int]int{}, map[int]int{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) != 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		pgid, err3 := strconv.Atoi(f[2])
		if err1 == nil && err2 == nil && err3 == nil {
			parents[pid] = ppid
			groups[pid] = pgid
		}
	}
	return parents, groups
}

// withDescendants is the roots and everything below them, parents before children.
func withDescendants(roots map[int]bool, parents map[int]int) []int {
	children := map[int][]int{}
	for pid, ppid := range parents {
		children[ppid] = append(children[ppid], pid)
	}
	seen := map[int]bool{}
	var all []int
	var walk func(int)
	walk = func(pid int) {
		if seen[pid] {
			return
		}
		seen[pid] = true
		all = append(all, pid)
		for _, c := range children[pid] {
			walk(c)
		}
	}
	for pid := range roots {
		walk(pid)
	}
	return all
}

func pidList(fields []string) []int {
	var pids []int
	for _, f := range fields {
		if pid, err := strconv.Atoi(f); err == nil {
			pids = append(pids, pid)
		}
	}
	return pids
}
