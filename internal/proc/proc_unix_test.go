//go:build !windows

package proc

import (
	"os/exec"
	"testing"

	"golang.org/x/sys/unix"
)

func TestStartBackgroundLowersPriority(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := StartBackground(cmd); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	n, err := niceness(cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if n < Niceness {
		t.Fatalf("niceness %d, want at least %d", n, Niceness)
	}
}

func TestLowerLeavesALowerProcessAlone(t *testing.T) {
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	if err := unix.Setpriority(unix.PRIO_PROCESS, cmd.Process.Pid, 15); err != nil {
		t.Fatal(err)
	}
	if err := Lower(cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	if n, _ := niceness(cmd.Process.Pid); n != 15 {
		t.Fatalf("niceness %d, want it left at 15", n)
	}
}
