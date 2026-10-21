package engine

import (
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

func shellSupervisor(t *testing.T) *Supervisor {
	if runtime.GOOS == "windows" {
		t.Skip("runs sh")
	}
	t.Setenv("SHELL", "/bin/sh")
	return &Supervisor{state: t.TempDir(), changes: make(chan struct{}, 1)}
}

func waitEnded(t *testing.T, sh *Shell) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for sh.Running() {
		if time.Now().After(deadline) {
			t.Fatal("still running")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestShellRunsInItsDirectory(t *testing.T) {
	s := shellSupervisor(t)
	dir := t.TempDir()
	sh, err := s.RunShell(dir, "pwd; echo oops >&2; exit 3")
	if err != nil {
		t.Fatal(err)
	}
	waitEnded(t, sh)
	if sh.State() != "failed" {
		t.Fatalf("state %s", sh.State())
	}
	if _, exit, _ := sh.Ended(); exit != 3 {
		t.Fatalf("exit %d", exit)
	}
	out := sh.Output()
	if len(out) != 2 || !strings.HasSuffix(out[0], strings.TrimPrefix(dir, "/private")) || out[1] != "oops" {
		t.Fatalf("output %q", out)
	}
	if sh.LastLine() != "oops" {
		t.Fatalf("last line %q", sh.LastLine())
	}
	if data, err := os.ReadFile(sh.Log); err != nil || !strings.Contains(string(data), "oops") {
		t.Fatalf("log %q %v", data, err)
	}
	if len(s.Shells()) != 1 || s.Shell(sh.ID) != sh {
		t.Fatal("not listed")
	}
}

func TestShellKillReachesWhatItStarted(t *testing.T) {
	s := shellSupervisor(t)
	sh, err := s.RunShell(t.TempDir(), "sleep 30 | cat")
	if err != nil {
		t.Fatal(err)
	}
	s.KillShell(sh)
	waitEnded(t, sh)
	if sh.State() != "killed" {
		t.Fatalf("state %s", sh.State())
	}
	s.DropShell(sh)
	if len(s.Shells()) != 0 {
		t.Fatal("still listed")
	}
}

func TestShellNeedsADirectoryAndACommand(t *testing.T) {
	s := shellSupervisor(t)
	if _, err := s.RunShell(t.TempDir(), "  "); err == nil {
		t.Fatal("ran nothing")
	}
	if _, err := s.RunShell("/no/such/dir", "true"); err == nil {
		t.Fatal("ran in a missing directory")
	}
}

func TestShellKeepsPartialLines(t *testing.T) {
	sh := &Shell{}
	sh.add("one\ntw")
	sh.add("o\r\nthree")
	if got := sh.Output(); strings.Join(got, "|") != "one|two|three" {
		t.Fatalf("%q", got)
	}
}
