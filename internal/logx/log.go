// Package logx is the supervisor's one log: appended to a file, kept in memory for the TUI and echoed to stdout
// when there is no TUI. A project's lines carry its name in brackets and also go to the project's own log.
package logx

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const keep = 1000

// Log is the shared log.
type Log struct {
	mu    sync.Mutex
	lines []string
	file  string
	echo  bool
	subs  []chan struct{}
}

// Default is the process's log.
var Default = &Log{}

// Open points the log at a file and says whether to echo lines to stdout.
func (l *Log) Open(file string, echo bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.file = file
	l.echo = echo
	_ = os.MkdirAll(filepath.Dir(file), 0o755)
}

// Write adds a line to the log, and to the project's own log when there is one.
func (l *Log) Write(project, projectFile, format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	stamp := time.Now().Format("2006-01-02 15:04:05")
	line := stamp + " " + msg
	if project != "" {
		line = stamp + " [" + project + "] " + msg
	}
	l.mu.Lock()
	l.lines = append(l.lines, line)
	if len(l.lines) > keep {
		l.lines = l.lines[len(l.lines)-keep:]
	}
	file, echo, subs := l.file, l.echo, l.subs
	l.mu.Unlock()
	appendLine(file, line)
	if projectFile != "" {
		appendLine(projectFile, stamp+" "+msg)
	}
	if echo {
		fmt.Println(line)
	}
	for _, c := range subs {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}

// Printf writes a line that belongs to no project.
func (l *Log) Printf(format string, args ...any) { l.Write("", "", format, args...) }

// Lines returns the last n lines (all of them for n <= 0).
func (l *Log) Lines(n int) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if n <= 0 || n > len(l.lines) {
		n = len(l.lines)
	}
	return append([]string(nil), l.lines[len(l.lines)-n:]...)
}

// Matching returns the lines containing any of the needles.
func (l *Log) Matching(needles ...string) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for _, line := range l.lines {
		for _, n := range needles {
			if strings.Contains(line, n) {
				out = append(out, line)
				break
			}
		}
	}
	return out
}

// Subscribe returns a channel that is signalled (without blocking) whenever a line is written.
func (l *Log) Subscribe() <-chan struct{} {
	l.mu.Lock()
	defer l.mu.Unlock()
	c := make(chan struct{}, 1)
	l.subs = append(l.subs, c)
	return c
}

func appendLine(file, line string) {
	if file == "" {
		return
	}
	f, err := os.OpenFile(file, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return // the log file is a convenience; never let it stop the supervisor
	}
	defer f.Close()
	_, _ = f.WriteString(line + "\n")
}

// ErrText is an error's first line.
func ErrText(err error) string {
	if err == nil {
		return ""
	}
	s, _, _ := strings.Cut(err.Error(), "\n")
	return s
}
