package engine

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/alsiola/open-supervisor/internal/logx"
	"github.com/alsiola/open-supervisor/internal/proc"
)

// shellLines is how much of a shell task's output it keeps in memory; the rest is in its log file.
const shellLines = 5000

// Shell is a shell command typed into the TUI, run in a directory of the person's choosing. It belongs to this
// run of the supervisor: quitting stops it, and it isn't kept across restarts.
type Shell struct {
	ID        int
	Dir       string
	Command   string
	StartedAt time.Time
	Log       string // the whole output, in the state folder

	cmd *exec.Cmd

	mu      sync.Mutex
	lines   []string
	partial string
	ended   *time.Time
	exit    int
	err     string
	killed  bool
}

// Running reports whether the command is still going.
func (sh *Shell) Running() bool {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.ended == nil
}

// State is "running", "done" (exit 0), "killed" or "failed".
func (sh *Shell) State() string {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	switch {
	case sh.ended == nil:
		return "running"
	case sh.killed:
		return "killed"
	case sh.exit == 0 && sh.err == "":
		return "done"
	}
	return "failed"
}

// Ended is when the command ended, and its exit code and any error starting or waiting for it.
func (sh *Shell) Ended() (*time.Time, int, string) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	return sh.ended, sh.exit, sh.err
}

// Output is the output kept in memory, stdout and stderr together, a line at a time.
func (sh *Shell) Output() []string {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	out := append([]string(nil), sh.lines...)
	if sh.partial != "" {
		out = append(out, sh.partial)
	}
	return out
}

// LastLine is the last line of output that isn't blank.
func (sh *Shell) LastLine() string {
	out := sh.Output()
	for i := len(out) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(out[i]); l != "" {
			return l
		}
	}
	return ""
}

func (sh *Shell) add(text string) {
	sh.mu.Lock()
	defer sh.mu.Unlock()
	text = sh.partial + strings.ReplaceAll(text, "\r\n", "\n")
	parts := strings.Split(text, "\n")
	sh.partial = parts[len(parts)-1]
	sh.lines = append(sh.lines, parts[:len(parts)-1]...)
	if n := len(sh.lines) - shellLines; n > 0 {
		sh.lines = append([]string(nil), sh.lines[n:]...)
	}
}

// shellWriter keeps what the command writes and tells the TUI.
type shellWriter struct {
	sh      *Shell
	file    io.Writer
	changed func()
}

func (w *shellWriter) Write(b []byte) (int, error) {
	if w.file != nil {
		_, _ = w.file.Write(b)
	}
	w.sh.add(string(b))
	w.changed()
	return len(b), nil
}

// shellCommand is how a typed command runs: through the person's shell, or cmd on Windows.
func shellCommand(command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("cmd", "/C", command)
	}
	sh := os.Getenv("SHELL")
	if sh == "" {
		sh = "/bin/sh"
	}
	return exec.Command(sh, "-c", command)
}

// RunShell starts a shell command in dir and adds it to the shell tasks.
func (s *Supervisor) RunShell(dir, command string) (*Shell, error) {
	command = strings.TrimSpace(command)
	if command == "" {
		return nil, fmt.Errorf("no command")
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		return nil, fmt.Errorf("%s isn't a directory", dir)
	}
	s.shellMu.Lock()
	s.shellN++
	sh := &Shell{ID: s.shellN, Dir: dir, Command: command, StartedAt: time.Now()}
	s.shellMu.Unlock()

	var file *os.File
	logDir := filepath.Join(s.state, "shells")
	if err := os.MkdirAll(logDir, 0o700); err == nil {
		sh.Log = filepath.Join(logDir, fmt.Sprintf("%s-%d.log", sh.StartedAt.Format("20060102-150405"), sh.ID))
		file, _ = os.Create(sh.Log)
	}
	w := &shellWriter{sh: sh, changed: s.changed}
	if file != nil {
		w.file = file
	} else {
		sh.Log = ""
	}
	cmd := shellCommand(command)
	cmd.Dir = dir
	cmd.Stdout, cmd.Stderr = w, w // the same writer, so one pipe keeps the two in order
	// Its own process group, so killing it reaches everything it started, and it can't read the TUI's terminal.
	proc.Detach(cmd)
	if err := cmd.Start(); err != nil {
		if file != nil {
			_ = file.Close()
		}
		return nil, err
	}
	sh.cmd = cmd
	s.shellMu.Lock()
	s.shells = append(s.shells, sh)
	s.shellMu.Unlock()
	logx.Default.Printf("shell !%d: started `%s` in %s (pid %d)", sh.ID, command, dir, cmd.Process.Pid)
	s.changed()
	go func() {
		err := cmd.Wait()
		if file != nil {
			_ = file.Close()
		}
		now := time.Now()
		sh.mu.Lock()
		sh.ended = &now
		sh.exit = cmd.ProcessState.ExitCode()
		if err != nil && cmd.ProcessState.ExitCode() < 0 && !sh.killed {
			sh.err = logx.ErrText(err)
		}
		killed, exit := sh.killed, sh.exit
		sh.mu.Unlock()
		switch {
		case killed:
			logx.Default.Printf("shell !%d: killed", sh.ID)
		default:
			logx.Default.Printf("shell !%d: exited %d after %s", sh.ID, exit, now.Sub(sh.StartedAt).Round(time.Second))
		}
		s.changed()
	}()
	return sh, nil
}

// Shells are the shell tasks, oldest first.
func (s *Supervisor) Shells() []*Shell {
	s.shellMu.Lock()
	defer s.shellMu.Unlock()
	return append([]*Shell(nil), s.shells...)
}

// Shell is the shell task with the id, if it's still listed.
func (s *Supervisor) Shell(id int) *Shell {
	s.shellMu.Lock()
	defer s.shellMu.Unlock()
	for _, sh := range s.shells {
		if sh.ID == id {
			return sh
		}
	}
	return nil
}

// KillShell stops a running shell task and whatever it started.
func (s *Supervisor) KillShell(sh *Shell) {
	sh.mu.Lock()
	if sh.ended != nil || sh.cmd == nil || sh.cmd.Process == nil {
		sh.mu.Unlock()
		return
	}
	sh.killed = true
	pid := sh.cmd.Process.Pid
	sh.mu.Unlock()
	_ = proc.TerminateGroup(pid)
}

// DropShell takes a shell task off the list, killing it first if it's running.
func (s *Supervisor) DropShell(sh *Shell) {
	s.KillShell(sh)
	s.shellMu.Lock()
	for i, x := range s.shells {
		if x == sh {
			s.shells = append(s.shells[:i], s.shells[i+1:]...)
			break
		}
	}
	s.shellMu.Unlock()
	s.changed()
}

// stopShells kills the shell tasks still running: unlike turns, nothing picks them up again.
func (s *Supervisor) stopShells() {
	n := 0
	for _, sh := range s.Shells() {
		if sh.Running() {
			s.KillShell(sh)
			n++
		}
	}
	if n > 0 {
		logx.Default.Printf("stopping: killed %d shell task(s)", n)
	}
}
