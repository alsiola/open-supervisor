// Package problems holds the notes agents leave when something in their environment gets in the way: a
// variable that isn't set, a skill that names a script that has gone, an `agent` command that fails. Each note is one file in the project's problems folder; the daily triage reads them, fixes
// or files each cause, and deletes them.
package problems

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Kinds are the kinds of problem an agent can log.
var Kinds = []string{"env", "docs", "tooling", "service", "other"}

// Note is one problem.
type Note struct {
	// Name is the note's file name, set when it is read.
	Name string
	// Issue is the issue whose session hit it, empty when none did.
	Issue string
	Kind  string
	Body  string
}

// ValidKind reports whether kind is one an agent may log.
func ValidKind(kind string) bool {
	for _, k := range Kinds {
		if k == kind {
			return true
		}
	}
	return false
}

// Write saves the note in dir and returns its path.
func Write(dir string, n Note) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	text := fmt.Sprintf("issue: %s\nkind: %s\n\n%s\n", n.Issue, n.Kind, strings.TrimSpace(n.Body))
	stamp := time.Now().UTC().Format("20060102T150405.000Z")
	for i := 0; ; i++ {
		name := stamp
		if i > 0 {
			name += fmt.Sprintf("_%d", i) // sorts after the first: _ comes after .
		}
		file := filepath.Join(dir, name+".md")
		f, err := os.OpenFile(file, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if os.IsExist(err) {
			continue
		}
		if err != nil {
			return "", err
		}
		_, err = f.WriteString(text)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		return file, err
	}
}

// List reads the notes in dir, oldest first. A missing dir has none.
func List(dir string) []Note {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var out []Note
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		n := parse(string(data))
		n.Name = e.Name()
		out = append(out, n)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Count is the number of notes in dir.
func Count(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			n++
		}
	}
	return n
}

func parse(text string) Note {
	var n Note
	head, body, _ := strings.Cut(text, "\n\n")
	for _, line := range strings.Split(head, "\n") {
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "issue":
			n.Issue = v
		case "kind":
			n.Kind = v
		}
	}
	n.Body = strings.TrimSpace(body)
	return n
}
