// Package proc is the little process handling that differs between systems: starting a child detached from
// the supervisor, so it outlives it, and asking whether a pid is still alive.
package proc

import "os/exec"

// Detach makes cmd start in its own session, so quitting the supervisor leaves it running.
func Detach(cmd *exec.Cmd) { detach(cmd) }

// Alive reports whether a process with this pid is running.
func Alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return alive(pid)
}

// Terminate asks a process to stop (SIGTERM where there is one).
func Terminate(pid int) error { return terminate(pid) }

// Kill stops a process at once.
func Kill(pid int) error { return kill(pid) }

// TerminateGroup asks a process started with Detach, and everything it started, to stop.
func TerminateGroup(pid int) error { return terminateGroup(pid) }
