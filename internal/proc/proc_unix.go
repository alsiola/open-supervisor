//go:build !windows

package proc

import (
	"os/exec"
	"syscall"
)

func detach(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
}

func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func terminate(pid int) error { return syscall.Kill(pid, syscall.SIGTERM) }

func kill(pid int) error { return syscall.Kill(pid, syscall.SIGKILL) }
