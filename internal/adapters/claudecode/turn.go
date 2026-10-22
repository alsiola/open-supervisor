package claudecode

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/core"
	"github.com/alsiola/open-supervisor/internal/proc"
)

// Turns run detached, with their output in files, so they outlive the supervisor: quitting it leaves them
// running, and the next start adopts them by pid and reads their result when they exit.
//
// The prompt goes in on stdin as stream-json, and stdin stays open while the turn runs, so the supervisor can
// hand the agent more (a comment, a message typed into the TUI) mid-turn. claude takes each one in after the
// tool call in progress, before its next model call, and echoes it back (--replay-user-messages) when it does.
// stdin is closed once the result arrives; if the supervisor goes first, claude sees the end of its input,
// finishes the turn and exits.

type pending struct {
	text  string
	taken func(bool)
}

type turn struct {
	handle     adapter.TurnHandle
	structured bool
	done       chan struct{}

	mu      sync.Mutex
	outcome core.Outcome
	stdin   io.WriteCloser
	open    bool
	waiting []pending
	offset  int64
	partial string
	killed  bool
	cmd     *exec.Cmd
}

func (t *turn) Handle() adapter.TurnHandle { return t.handle }
func (t *turn) Done() <-chan struct{}      { return t.done }

func (t *turn) Outcome() core.Outcome {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.outcome
}

// Start begins a turn: claude -p on the session, its output in files under spec.Dir.
func (a *Agent) Start(ctx context.Context, spec adapter.TurnSpec) (adapter.Turn, error) {
	started := time.Now().UTC()
	model := a.model
	if spec.DryRun {
		model = a.dryModel
	}
	args := []string{"-p", "--model", model, "--permission-mode", a.permission,
		"--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--replay-user-messages",
		"--plugin-dir", a.pluginDir}
	if spec.Structured {
		schema, _ := json.Marshal(core.StatusSchema)
		args = append(args, "--json-schema", string(schema))
	}
	if spec.Session != "" {
		if spec.Resume {
			args = append(args, "--resume", spec.Session)
		} else {
			args = append(args, "--session-id", spec.Session)
		}
	}
	args = append(args, a.extra...)

	if err := os.MkdirAll(spec.Dir, 0o755); err != nil {
		return nil, err
	}
	stamp := strings.NewReplacer(":", "-", ".", "-").Replace(started.Format(time.RFC3339Nano))
	outPath := filepath.Join(spec.Dir, stamp+".out")
	errPath := filepath.Join(spec.Dir, stamp+".err")
	out, err := os.Create(outPath)
	if err != nil {
		return nil, err
	}
	defer out.Close()
	errf, err := os.Create(errPath)
	if err != nil {
		return nil, err
	}
	defer errf.Close()

	cmd := exec.Command(a.binary, args...)
	cmd.Dir = spec.Cwd
	cmd.Env = a.childEnv(spec.Env)
	cmd.Stdout = out
	cmd.Stderr = errf
	proc.Detach(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("couldn't start %s: %w", a.binary, err)
	}
	prompt := a.prompt(spec)
	t := &turn{
		handle:     adapter.TurnHandle{PID: cmd.Process.Pid, Out: outPath, Err: errPath, StartedAt: started},
		structured: spec.Structured,
		done:       make(chan struct{}),
		stdin:      stdin,
		open:       true,
		cmd:        cmd,
	}
	t.write(prompt)
	keep(outPath, map[string]any{"args": args, "prompt": prompt})

	stop := make(chan struct{})
	go func() {
		tick := time.NewTicker(time.Second)
		defer tick.Stop()
		for {
			select {
			case <-stop:
				return
			case <-tick.C:
				t.follow()
			}
		}
	}()
	go func() {
		werr := cmd.Wait()
		close(stop)
		t.follow()
		t.mu.Lock()
		t.open = false
		waiting := t.waiting
		t.waiting = nil
		killed := t.killed
		t.mu.Unlock()
		// Anything claude never echoed back goes to the next turn; settle that before the turn itself.
		for _, p := range waiting {
			p.taken(false)
		}
		var o core.Outcome
		if killed {
			o = core.Outcome{Error: "killed"}
		} else {
			code := 0
			if ee, ok := werr.(*exec.ExitError); ok {
				code = ee.ExitCode()
			} else if werr != nil {
				code = -1
			}
			o = perTurn(outPath, parse(read(outPath), read(errPath), &code, spec.Structured))
		}
		o.Duration = time.Since(started)
		keep(outPath, map[string]any{"args": args, "prompt": prompt, "result": o})
		t.mu.Lock()
		t.outcome = o
		t.mu.Unlock()
		close(t.done)
	}()
	return t, nil
}

// write sends a user message on stdin.
func (t *turn) write(text string) bool {
	line, _ := json.Marshal(map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}})
	if _, err := t.stdin.Write(append(line, '\n')); err != nil {
		t.open = false
		return false
	}
	return true
}

// follow reads what claude has written since last time, for its echoes of what it took in and for the result.
func (t *turn) follow() {
	t.mu.Lock()
	defer t.mu.Unlock()
	f, err := os.Open(t.handle.Out)
	if err != nil {
		return
	}
	defer f.Close()
	if _, err := f.Seek(t.offset, io.SeekStart); err != nil {
		return
	}
	data, _ := io.ReadAll(f)
	t.offset += int64(len(data))
	t.partial += string(data)
	lines := strings.Split(t.partial, "\n")
	t.partial = lines[len(lines)-1]
	for _, line := range lines[:len(lines)-1] {
		var m struct {
			Type     string `json:"type"`
			IsReplay bool   `json:"isReplay"`
			Message  struct {
				Content any `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &m) != nil {
			continue
		}
		switch {
		case m.Type == "user" && m.IsReplay:
			text, _ := m.Message.Content.(string)
			for i, p := range t.waiting {
				if p.text == text {
					t.waiting = append(t.waiting[:i], t.waiting[i+1:]...)
					go p.taken(true)
					break
				}
			}
		case m.Type == "result":
			if t.open {
				t.open = false
				_ = t.stdin.Close()
			}
		}
	}
}

// Send hands the running agent a message.
func (t *turn) Send(text string, taken func(bool)) bool {
	t.follow()
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.open || t.stdin == nil {
		return false
	}
	if !t.write(text) {
		return false
	}
	t.waiting = append(t.waiting, pending{text, taken})
	return true
}

// Kill stops the turn: SIGTERM, then SIGKILL after ten seconds.
func (t *turn) Kill() {
	t.mu.Lock()
	t.killed = true
	pid := t.handle.PID
	t.mu.Unlock()
	_ = proc.Terminate(pid)
	go func() {
		select {
		case <-t.done:
		case <-time.After(10 * time.Second):
			_ = proc.Kill(pid)
		}
	}()
}

// Adopt takes over a turn an earlier supervisor started: wait for its process to exit, then read what it wrote.
// Its stdin went with the supervisor that started it, so it takes no messages.
func (a *Agent) Adopt(h adapter.TurnHandle, structured bool) adapter.Turn {
	t := &turn{handle: h, structured: structured, done: make(chan struct{})}
	go func() {
		for proc.Alive(h.PID) {
			time.Sleep(2 * time.Second)
		}
		// Give the file system a moment to flush the last write.
		time.Sleep(500 * time.Millisecond)
		t.mu.Lock()
		killed := t.killed
		t.mu.Unlock()
		var o core.Outcome
		if killed {
			o = core.Outcome{Error: "killed"}
		} else {
			o = perTurn(h.Out, parse(read(h.Out), read(h.Err), nil, structured))
		}
		o.Duration = time.Since(h.StartedAt)
		keep(h.Out, map[string]any{"adopted": true, "result": o})
		t.mu.Lock()
		t.outcome = o
		t.mu.Unlock()
		close(t.done)
	}()
	return t
}

func read(file string) string {
	data, err := os.ReadFile(file)
	if err != nil {
		return ""
	}
	return string(data)
}

// keep writes the turn's arguments, prompt and result next to its output, for debugging.
func keep(out string, record any) {
	data, err := json.MarshalIndent(record, "", "  ")
	if err == nil {
		_ = os.WriteFile(strings.TrimSuffix(out, ".out")+".json", data, 0o644)
	}
}

type result struct {
	Type             string                    `json:"type"`
	Subtype          string                    `json:"subtype"`
	IsError          bool                      `json:"is_error"`
	Result           any                       `json:"result"`
	SessionID        string                    `json:"session_id"`
	TotalCostUSD     *float64                  `json:"total_cost_usd"`
	NumTurns         int                       `json:"num_turns"`
	StructuredOutput json.RawMessage           `json:"structured_output"`
	ModelUsage       map[string]map[string]any `json:"modelUsage"`
}

// lastResult is the last result in claude's output: one JSON object, or one per line (stream-json).
func lastResult(stdout string) *result {
	var r result
	trimmed := strings.TrimSpace(stdout)
	if json.Unmarshal([]byte(trimmed), &r) == nil && r.Type == "result" {
		return &r
	}
	lines := strings.Split(trimmed, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		var m result
		if json.Unmarshal([]byte(lines[i]), &m) == nil && m.Type == "result" {
			return &m
		}
	}
	return nil
}

// toolCalls counts the tool calls in the turn's assistant messages, a subagent's included.
func toolCalls(stdout string) int {
	n := 0
	for _, line := range strings.Split(stdout, "\n") {
		if !strings.Contains(line, `"tool_use"`) {
			continue
		}
		var m struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type string `json:"type"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(line), &m) != nil || m.Type != "assistant" {
			continue
		}
		for _, c := range m.Message.Content {
			if c.Type == "tool_use" {
				n++
			}
		}
	}
	return n
}

func num(m map[string]any, k string) int64 {
	if f, ok := m[k].(float64); ok {
		return int64(f)
	}
	return 0
}

// usage sums the tokens per model, less what before had for the same model, and lists the models that used any.
func usage(models, before map[string]map[string]any) (*core.Tokens, []string) {
	if len(models) == 0 {
		return nil, nil
	}
	t := &core.Tokens{}
	var used []string
	for model, m := range models {
		b := before[model]
		u := core.Tokens{
			Input:         num(m, "inputTokens") - num(b, "inputTokens"),
			Output:        num(m, "outputTokens") - num(b, "outputTokens"),
			CacheRead:     num(m, "cacheReadInputTokens") - num(b, "cacheReadInputTokens"),
			CacheCreation: num(m, "cacheCreationInputTokens") - num(b, "cacheCreationInputTokens"),
		}
		if u == (core.Tokens{}) {
			continue
		}
		used = append(used, model)
		t = t.Add(&u)
	}
	sort.Strings(used)
	return t, used
}

// perTurn makes an outcome this turn's alone. On a resumed session claude reports the cost and tokens since the
// session began, so the totals in the session's previous result (the latest earlier output in the same folder
// with one) come off. A session that has gone backwards, as after a fork, is left as it is.
func perTurn(out string, o core.Outcome) core.Outcome {
	cur := lastResult(read(out))
	if cur == nil || cur.SessionID == "" {
		return o
	}
	files, _ := filepath.Glob(filepath.Join(filepath.Dir(out), "*.out"))
	sort.Strings(files)
	var prev *result
	for i := len(files) - 1; i >= 0 && prev == nil; i-- {
		if files[i] >= out {
			continue
		}
		if r := lastResult(read(files[i])); r != nil && r.SessionID == cur.SessionID {
			prev = r
		}
	}
	if prev == nil || prev.TotalCostUSD == nil || cur.TotalCostUSD == nil || *prev.TotalCostUSD > *cur.TotalCostUSD {
		return o
	}
	for model, b := range prev.ModelUsage {
		m := cur.ModelUsage[model]
		for _, k := range []string{"inputTokens", "outputTokens", "cacheReadInputTokens", "cacheCreationInputTokens"} {
			if num(b, k) > num(m, k) {
				return o
			}
		}
	}
	o.Cost = *cur.TotalCostUSD - *prev.TotalCostUSD
	o.Tokens, o.Models = usage(cur.ModelUsage, prev.ModelUsage)
	return o
}

// parse turns claude's output into an outcome. code is nil for an adopted turn, whose exit code is unknown.
func parse(stdout, stderr string, code *int, structured bool) core.Outcome {
	// A message that slipped in just after a result starts another in the same process; the totals are
	// cumulative, so the last one covers both.
	r := lastResult(stdout)
	if r == nil {
		tail := strings.TrimSpace(stderr)
		if tail == "" {
			tail = strings.TrimSpace(stdout)
		}
		lines := strings.Split(tail, "\n")
		if len(lines) > 3 {
			lines = lines[len(lines)-3:]
		}
		tail = strings.Join(lines, " ")
		if len(tail) > 300 {
			tail = tail[:300]
		}
		exit := "unknown"
		if code != nil {
			exit = fmt.Sprint(*code)
		}
		msg := "claude exited " + exit + " without a result"
		if tail != "" {
			msg += ": " + tail
		}
		return core.Outcome{Error: msg}
	}
	o := core.Outcome{}
	if r.TotalCostUSD != nil {
		o.Cost = *r.TotalCostUSD
	}
	if s, ok := r.Result.(string); ok {
		o.Text = s
	}
	o.ModelCalls, o.ToolCalls = r.NumTurns, toolCalls(stdout)
	o.Tokens, o.Models = usage(r.ModelUsage, nil)
	if r.IsError || (code != nil && *code != 0) {
		sub := r.Subtype
		if sub == "" {
			sub = "error"
		}
		o.Error = sub
		if o.Text != "" {
			text := o.Text
			if len(text) > 300 {
				text = text[:300]
			}
			o.Error += ": " + text
		}
		return o
	}
	if !structured {
		o.OK = true
		return o
	}
	var s core.Status
	if len(r.StructuredOutput) == 0 || json.Unmarshal(r.StructuredOutput, &s) != nil || s.State == "" {
		o.Error = fmt.Sprintf("the turn ended without a status (%s)", r.Subtype)
		return o
	}
	o.OK = true
	o.Status = &s
	return o
}
