package claudecode

import (
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alsiola/open-supervisor/internal/core"
)

// What an agent is doing right now, read from the tail of its session transcript: the description of its
// latest tool call and when it was made. Lets the TUI show "Draw lines for matches cut short" instead of just
// how long the turn has run.

const tailBytes = 96 * 1024

var (
	toolRe  = regexp.MustCompile(`"type":"tool_use","id":"[^"]*","name":"([A-Za-z_]+)","input":\{(?:"command":"(?:[^"\\]|\\.)*",)?(?:"description":"((?:[^"\\]|\\.){0,160})|"file_path":"((?:[^"\\]|\\.){0,160})|"skill":"((?:[^"\\]|\\.){0,60})|"prompt":"((?:[^"\\]|\\.){0,160}))?`)
	stampRe = regexp.MustCompile(`"timestamp":"([^"]+)"`)
	escRe   = regexp.MustCompile(`\\(.)`)
	cmdName = regexp.MustCompile(`<command-name>([^<]*)</command-name>`)
	cmdArgs = regexp.MustCompile(`(?s)<command-args>(.*?)</command-args>`)
)

// Activity is the latest tool call in the session's transcript.
func (a *Agent) Activity(cwd, session string) *core.Activity {
	f, err := os.Open(a.TranscriptPath(cwd, session))
	if err != nil {
		return nil
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil
	}
	start := info.Size() - tailBytes
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return nil
	}
	data, _ := io.ReadAll(f)
	tail := string(data)
	// Tool calls carry a description (Bash) or a target (a file, a skill); the last is the one in progress.
	all := toolRe.FindAllStringSubmatch(tail, -1)
	if len(all) == 0 {
		return nil
	}
	m := all[len(all)-1]
	tool, desc, target, skill, prompt := m[1], m[2], m[3], m[4], m[5]
	detail := desc
	switch {
	case detail != "":
	case target != "":
		detail = filepath.Base(escRe.ReplaceAllString(target, "$1"))
	case skill != "":
		detail = "/" + skill
	default:
		detail = prompt
	}
	text := tool
	if detail != "" {
		text = tool + ": " + escRe.ReplaceAllString(detail, "$1")
	}
	act := &core.Activity{Text: text}
	if stamps := stampRe.FindAllStringSubmatch(tail, -1); len(stamps) > 0 {
		act.At, _ = time.Parse(time.RFC3339Nano, stamps[len(stamps)-1][1])
	}
	return act
}

type cached struct {
	offset  int64
	entries []core.Entry
}

var (
	cacheMu sync.Mutex
	cache   = map[string]*cached{}
)

// Transcript is the session as entries, read incrementally: only bytes appended since the last call are parsed.
func (a *Agent) Transcript(cwd, session string) []core.Entry {
	file := a.TranscriptPath(cwd, session)
	info, err := os.Stat(file)
	if err != nil {
		return nil
	}
	cacheMu.Lock()
	defer cacheMu.Unlock()
	c := cache[file]
	if c == nil || info.Size() < c.offset {
		c = &cached{}
		cache[file] = c
	}
	if info.Size() == c.offset {
		return c.entries
	}
	f, err := os.Open(file)
	if err != nil {
		return c.entries
	}
	defer f.Close()
	if _, err := f.Seek(c.offset, io.SeekStart); err != nil {
		return c.entries
	}
	data, _ := io.ReadAll(io.LimitReader(f, info.Size()-c.offset))
	// Stop at the last complete line, so a line still being written is read next time.
	end := strings.LastIndexByte(string(data), '\n') + 1
	c.offset += int64(end)
	for _, line := range strings.Split(string(data[:end]), "\n") {
		if line != "" {
			c.entries = append(c.entries, entriesOf(line)...)
		}
	}
	return c.entries
}

func blockText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var parts []string
		for _, x := range c {
			if m, ok := x.(map[string]any); ok {
				if t, ok := m["text"].(string); ok && t != "" {
					parts = append(parts, t)
				}
			}
		}
		return strings.Join(parts, "\n")
	}
	return ""
}

func toolLine(name string, input map[string]any) string {
	pick := func(k string) string {
		s, _ := input[k].(string)
		return s
	}
	detail := pick("description")
	if detail == "" && pick("file_path") != "" {
		detail = filepath.Base(pick("file_path"))
	}
	if detail == "" && pick("skill") != "" {
		detail = "/" + pick("skill")
	}
	for _, k := range []string{"command", "pattern", "query", "prompt"} {
		if detail == "" {
			detail = pick(k)
		}
	}
	if detail == "" {
		return name
	}
	return name + ": " + detail
}

// entriesOf turns one transcript line into what a reader of the session would see: prompts, replies, tool
// calls and the first lines of their results.
func entriesOf(line string) []core.Entry {
	var o struct {
		Type        string `json:"type"`
		IsSidechain bool   `json:"isSidechain"`
		Timestamp   string `json:"timestamp"`
		Message     *struct {
			Content any `json:"content"`
		} `json:"message"`
	}
	if json.Unmarshal([]byte(line), &o) != nil || (o.Type != "user" && o.Type != "assistant") || o.IsSidechain || o.Message == nil {
		return nil
	}
	at, _ := time.Parse(time.RFC3339Nano, o.Timestamp)
	var out []core.Entry
	if s, ok := o.Message.Content.(string); ok && o.Type == "user" {
		text := s
		if m := cmdName.FindStringSubmatch(s); m != nil {
			args := ""
			if a := cmdArgs.FindStringSubmatch(s); a != nil {
				args = a[1]
			}
			text = strings.TrimSpace(m[1] + " " + args)
		}
		return []core.Entry{{At: at, Kind: core.EntryUser, Text: text}}
	}
	blocks, ok := o.Message.Content.([]any)
	if !ok {
		return nil
	}
	for _, b := range blocks {
		c, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch c["type"] {
		case "text":
			t, _ := c["text"].(string)
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			if o.Type == "assistant" {
				out = append(out, core.Entry{At: at, Kind: core.EntryAssistant, Text: t})
			} else {
				// Skill bodies and system reminders arrive as user text; show only their first line.
				first, _, _ := strings.Cut(t, "\n")
				out = append(out, core.Entry{At: at, Kind: core.EntryUser, Text: first})
			}
		case "thinking":
			if t, _ := c["thinking"].(string); strings.TrimSpace(t) != "" {
				out = append(out, core.Entry{At: at, Kind: core.EntryAssistant, Text: strings.TrimSpace(t)})
			}
		case "tool_use":
			name, _ := c["name"].(string)
			input, _ := c["input"].(map[string]any)
			out = append(out, core.Entry{At: at, Kind: core.EntryTool, Text: toolLine(name, input)})
		case "tool_result":
			lines := strings.Split(strings.TrimSpace(blockText(c["content"])), "\n")
			text := strings.Join(lines[:min(3, len(lines))], "\n")
			if len(lines) > 3 {
				text += "\n… " + strconv.Itoa(len(lines)-3) + " more line(s)"
			}
			if text == "" {
				text = "(no output)"
			}
			kind := core.EntryResult
			if e, _ := c["is_error"].(bool); e {
				kind = core.EntryError
			}
			out = append(out, core.Entry{At: at, Kind: kind, Text: text})
		}
	}
	return out
}
