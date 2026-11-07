//go:build !windows

package proc

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"syscall"

	"golang.org/x/sys/unix"
)

func detach(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func terminate(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

// terminateGroup signals the process group Setsid gave it, whose id is its pid.
func terminateGroup(pid int) error { return syscall.Kill(-pid, syscall.SIGTERM) }

func kill(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }

func signal(ps *os.ProcessState) string {
	ws, ok := ps.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() {
		return ""
	}
	if name := unix.SignalName(ws.Signal()); name != "" {
		return name
	}
	return fmt.Sprintf("signal %d", int(ws.Signal()))
}

func background(*exec.Cmd) {}

func lower(pid int) error {
	cur, err := niceness(pid)
	if err != nil {
		return err
	}
	if cur >= Niceness {
		return nil
	}
	return unix.Setpriority(unix.PRIO_PROCESS, pid, Niceness)
}

// niceness is a process's nice value. Linux's system call answers 20 minus it, so as never to be negative.
func niceness(pid int) (int, error) {
	n, err := unix.Getpriority(unix.PRIO_PROCESS, pid)
	if err != nil {
		return 0, err
	}
	if runtime.GOOS == "linux" {
		n = 20 - n
	}
	return n, nil
}
