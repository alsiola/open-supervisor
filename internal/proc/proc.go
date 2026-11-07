// Package proc is the little process handling that differs between systems: starting a child detached from
// the supervisor, so it outlives it, running it at a lower priority, and asking whether a pid is still alive.
package proc

import (
	"os"
	"os/exec"
)

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

// Signal names the signal that ended a process ("SIGKILL"), or is "" when it exited by itself or the system
// has no signals (Windows).
func Signal(ps *os.ProcessState) string {
	if ps == nil {
		return ""
	}
	return signal(ps)
}

// Niceness is how far below the supervisor's own priority the work it runs in the background goes: jobs, agent
// tasks and worktree setup. 10 is what `nice` uses with no number; on Windows it's "below normal".
const Niceness = 10

// Background marks cmd, before it starts, to run at a lower priority, where that has to be set at creation
// (Windows). Call Lower on its pid once it has started for the rest.
func Background(cmd *exec.Cmd) { background(cmd) }

// Lower drops a process to Niceness, so a build, a test run or an import it starts gives way to whatever
// the person is doing on the machine. What it starts from then on inherits it. A process already lower stays
// as it is. pid 0 is this process.
func Lower(pid int) error { return lower(pid) }

// StartBackground starts cmd at a lower priority: Background, Start, then Lower.
func StartBackground(cmd *exec.Cmd) error {
	Background(cmd)
	if err := cmd.Start(); err != nil {
		return err
	}
	_ = Lower(cmd.Process.Pid)
	return nil
}

// RunBackground runs cmd to the end at a lower priority, like cmd.Run.
func RunBackground(cmd *exec.Cmd) error {
	if err := StartBackground(cmd); err != nil {
		return err
	}
	return cmd.Wait()
}
