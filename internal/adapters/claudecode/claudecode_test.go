package claudecode

import (
	"context"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/core"
)

// fakeClaude reads stream-json user messages from stdin, echoes each back as a replay, and after the second
// one (or when stdin closes) writes a result with a structured status.
const fakeClaude = `#!/bin/sh
echo '{"type":"system","subtype":"init"}'
n=0
while IFS= read -r line; do
  n=$((n+1))
  content=$(printf '%s' "$line" | sed -E 's/.*"content":"([^"]*)".*/\1/')
  printf '{"type":"user","isReplay":true,"message":{"role":"user","content":"%s"}}\n' "$content"
  [ "$n" -ge "${FAKE_MESSAGES:-1}" ] && break
done
echo '{"type":"result","subtype":"success","is_error":false,"result":"all done","total_cost_usd":0.42,"structured_output":{"state":"review","summary":"did the thing","prs":[7]},"modelUsage":{"claude-x":{"inputTokens":10,"outputTokens":5,"cacheReadInputTokens":100,"cacheCreationInputTokens":3}}}'
`

func agentWith(t *testing.T, script string) *Agent {
	t.Helper()
	dir := t.TempDir()
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Agent{binary: bin, model: "opus", permission: "bypassPermissions", dryModel: "haiku", pluginDir: filepath.Join(dir, "plugin")}
}

func TestTurnTakesInAMessageAndEndsWithAStatus(t *testing.T) {
	a := agentWith(t, fakeClaude)
	env := append(os.Environ(), "FAKE_MESSAGES=2", "CLAUDECODE=1", "ANTHROPIC_API_KEY=sk-nope")
	turn, err := a.Start(context.Background(), adapter.TurnSpec{Cwd: t.TempDir(), Env: env, Dir: t.TempDir(), Playbook: "work-issue", Args: "12", Body: "go", Session: "s1", Structured: true})
	if err != nil {
		t.Fatal(err)
	}
	took := make(chan bool, 1)
	deadline := time.Now().Add(5 * time.Second)
	for !turn.Send("also this", func(yes bool) { took <- yes }) {
		if time.Now().After(deadline) {
			t.Fatal("the turn never took a message")
		}
		time.Sleep(20 * time.Millisecond)
	}
	select {
	case yes := <-took:
		if !yes {
			t.Fatal("the message wasn't taken in")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no answer about the message")
	}
	select {
	case <-turn.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("the turn didn't end")
	}
	o := turn.Outcome()
	if !o.OK || o.Status == nil || o.Status.State != core.Review || o.Status.PRs[0] != 7 {
		t.Fatalf("outcome %+v", o)
	}
	if o.Cost != 0.42 || o.Tokens.CacheRead != 100 || o.Models[0] != "claude-x" {
		t.Fatalf("cost and tokens %+v %+v", o, o.Tokens)
	}
	if turn.Send("too late", func(bool) {}) {
		t.Fatal("a finished turn took a message")
	}
}

func TestPromptAndEnv(t *testing.T) {
	a := &Agent{}
	p := a.prompt(adapter.TurnSpec{Playbook: "work-issue", Args: "PROJ-3 events", Body: "## Since your last turn"})
	if p != "/open-supervisor:work-issue PROJ-3 events\n\n## Since your last turn" {
		t.Fatalf("prompt %q", p)
	}
	if !strings.HasPrefix(a.prompt(adapter.TurnSpec{Playbook: "sweep", DryRun: true}), "This is a supervisor dry run") {
		t.Fatal("dry run prompt")
	}
	env := a.childEnv([]string{"CLAUDECODE=1", "CLAUDE_CODE_ENTRYPOINT=x", "ANTHROPIC_API_KEY=k", "PATH=/bin"})
	if len(env) != 1 || env[0] != "PATH=/bin" {
		t.Fatalf("env %v", env)
	}
	a.useAPIKey = true
	if env := a.childEnv([]string{"ANTHROPIC_API_KEY=k"}); len(env) != 1 {
		t.Fatal("useApiKey should keep the key")
	}
}

func TestParse(t *testing.T) {
	code := 1
	o := parse(`{"type":"result","subtype":"error_max_turns","is_error":true,"result":"ran out"}`, "", &code, true)
	if o.OK || !strings.Contains(o.Error, "error_max_turns") {
		t.Fatalf("%+v", o)
	}
	o = parse("", "boom\nreally", nil, true)
	if o.OK || !strings.Contains(o.Error, "without a result: boom really") {
		t.Fatalf("%+v", o)
	}
	ok := 0
	o = parse(`{"type":"result","subtype":"success","result":"x"}`, "", &ok, true)
	if o.OK || !strings.Contains(o.Error, "without a status") {
		t.Fatalf("%+v", o)
	}
	o = parse(`{"type":"result","subtype":"success","result":"Swept 3"}`, "", &ok, false)
	if !o.OK || o.Text != "Swept 3" {
		t.Fatalf("%+v", o)
	}
}

func TestTranscript(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)
	a := &Agent{}
	cwd := "/Users/x/Code/my.repo"
	path := a.TranscriptPath(cwd, "abc")
	if !strings.HasSuffix(path, "projects/-Users-x-Code-my-repo/abc.jsonl") {
		t.Fatalf("path %s", path)
	}
	_ = os.MkdirAll(filepath.Dir(path), 0o755)
	lines := []string{
		`{"type":"user","timestamp":"2026-10-20T10:00:00Z","message":{"content":"<command-name>/open-supervisor:work-issue</command-name><command-args>12</command-args>"}}`,
		`{"type":"assistant","timestamp":"2026-10-20T10:00:01Z","message":{"content":[{"type":"text","text":"Reading the issue."},{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"x","description":"Read the issue"}}]}}`,
		`{"type":"user","timestamp":"2026-10-20T10:00:02Z","message":{"content":[{"type":"tool_result","content":"a\nb\nc\nd\ne"}]}}`,
		`{"type":"assistant","isSidechain":true,"message":{"content":[{"type":"text","text":"hidden"}]}}`,
	}
	_ = os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
	entries := a.Transcript(cwd, "abc")
	if len(entries) != 4 || entries[0].Text != "/open-supervisor:work-issue 12" || entries[2].Text != "Bash: Read the issue" || !strings.Contains(entries[3].Text, "2 more line") {
		t.Fatalf("%+v", entries)
	}
	act := a.Activity(cwd, "abc")
	if act == nil || act.Text != "Bash: Read the issue" {
		t.Fatalf("activity %+v", act)
	}
	if !a.SessionExists(cwd, "abc") || a.SessionExists(cwd, "nope") {
		t.Fatal("SessionExists")
	}
}

func TestPrepareWritesThePlugin(t *testing.T) {
	a := agentWith(t, "#!/bin/sh\necho 1.0\n")
	t.Setenv("PATH", filepath.Dir(a.binary)+":"+os.Getenv("PATH"))
	if err := a.Prepare(context.Background(), map[string]string{"work-issue": "---\nname: work-issue\n---\nbody"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.pluginDir, "skills", "work-issue", "SKILL.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(a.pluginDir, ".claude-plugin", "plugin.json")); err != nil {
		t.Fatal(err)
	}
}

func TestSetupCommand(t *testing.T) {
	dir := t.TempDir()
	a := &Agent{binary: "sh", extra: []string{"--verbose"}, env: adapter.Env{DataDir: dir}}
	cmd, err := a.SetupCommand(adapter.SetupSpec{Cwd: dir, Guide: "---\nname: setup\n---\nhi", Allow: []string{"os config", "os doctor"}})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"sh", "/open-supervisor:setup", "--plugin-dir", filepath.Join(dir, "claude-setup-plugin"), "--verbose", "--allowedTools", "Bash(os config:*)", "Bash(os doctor:*)"}
	if strings.Join(cmd.Args, "|") != strings.Join(want, "|") {
		t.Fatal(cmd.Args)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "claude-setup-plugin", "skills", "setup", "SKILL.md")); err != nil || string(b) != "---\nname: setup\n---\nhi" {
		t.Fatal(string(b), err)
	}
	if _, err := os.Stat(filepath.Join(a.pluginDir, "skills", "setup")); a.pluginDir != "" && err == nil {
		t.Fatal("the setup skill is in the playbooks' plugin")
	}
}

func TestParseCountsCalls(t *testing.T) {
	out := strings.Join([]string{
		`{"type":"system","subtype":"init"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"a tool_use"},{"type":"tool_use","name":"Bash"},{"type":"tool_use","name":"Read"}]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":"\"tool_use\""}]}}`,
		`{"type":"assistant","parent_tool_use_id":"x","message":{"content":[{"type":"tool_use","name":"Grep"}]}}`,
		`{"type":"result","subtype":"success","num_turns":4,"total_cost_usd":0.1}`,
	}, "\n")
	o := parse(out, "", nil, false)
	if o.ModelCalls != 4 || o.ToolCalls != 3 {
		t.Fatalf("model calls %d, tool calls %d", o.ModelCalls, o.ToolCalls)
	}
}

func TestPerTurnTakesOffTheSessionsEarlierTotals(t *testing.T) {
	dir := t.TempDir()
	res := func(session string, cost float64, read int) string {
		return fmt.Sprintf(`{"type":"result","subtype":"success","session_id":%q,"total_cost_usd":%v,"modelUsage":{"opus":{"inputTokens":10,"outputTokens":%d,"cacheReadInputTokens":%d,"cacheCreationInputTokens":3},"haiku":{"inputTokens":1}}}`, session, cost, read/10, read)
	}
	write := func(name, body string) string {
		p := filepath.Join(dir, name+".out")
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	first := write("2026-10-21T10-00-00Z", res("s1", 8.5, 1000))
	write("2026-10-21T11-00-00Z", "claude died")
	second := write("2026-10-21T12-00-00Z", res("s1", 9, 1500))

	o := perTurn(first, parse(read(first), "", nil, false))
	if o.Cost != 8.5 || o.Tokens.CacheRead != 1000 || len(o.Models) != 2 {
		t.Fatalf("a session's first turn keeps its totals: %+v %+v", o, o.Tokens)
	}
	o = perTurn(second, parse(read(second), "", nil, false))
	if math.Abs(o.Cost-0.5) > 1e-9 || *o.Tokens != (core.Tokens{Output: 50, CacheRead: 500}) || len(o.Models) != 1 || o.Models[0] != "opus" {
		t.Fatalf("a resumed turn is its own: %+v %+v", o, o.Tokens)
	}

	other := write("2026-10-21T13-00-00Z", res("s2", 1, 100))
	if o := perTurn(other, parse(read(other), "", nil, false)); o.Cost != 1 {
		t.Fatalf("another session starts from nothing: %+v", o)
	}
	back := write("2026-10-21T14-00-00Z", res("s1", 2, 200))
	if o := perTurn(back, parse(read(back), "", nil, false)); o.Cost != 2 || o.Tokens.CacheRead != 200 {
		t.Fatalf("totals that went backwards are left alone: %+v", o)
	}
}
