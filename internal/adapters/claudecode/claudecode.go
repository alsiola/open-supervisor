// Package claudecode runs turns with the Claude Code CLI. A turn is `claude -p` on the issue's session, which
// exits when the agent has nothing more to do and ends with the structured status the supervisor acts on.
// Resuming the session later continues the same conversation, so the agent keeps everything it learned.
//
// The supervisor's playbooks are handed over as a plugin of skills (written to the data directory and loaded
// with --plugin-dir), so a turn starts with `/open-supervisor:work-issue 12`.
package claudecode

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/alsiola/open-supervisor/internal/adapter"
)

// PluginName is the plugin the playbooks are loaded as, and so the prefix of their skills.
const PluginName = "open-supervisor"

func init() {
	adapter.Register(adapter.Spec{
		Type:        "claude-code",
		Title:       "Claude Code",
		Description: "Runs each turn with the Claude Code CLI (`claude -p`), on your Claude subscription or an API key.",
		Roles:       []adapter.Role{adapter.RoleAgent},
		Fields: []adapter.Field{
			{Key: "binary", Label: "Claude binary", Help: "The claude CLI to run.", Type: adapter.Text, Default: "claude"},
			{Key: "model", Label: "Model", Help: "Model alias or id for turns.", Type: adapter.Text, Default: "opus"},
			{Key: "permissionMode", Label: "Permission mode", Help: "Turns run unattended, so they need permissions granted up front.", Type: adapter.Select, Options: []string{"bypassPermissions", "acceptEdits", "auto", "default"}, Default: "bypassPermissions"},
			{Key: "dryRunModel", Label: "Dry-run model", Help: "The cheap model a dry run's stub turns use.", Type: adapter.Text, Default: "haiku"},
			{Key: "useApiKey", Label: "Use ANTHROPIC_API_KEY", Help: "Off: sessions run on the machine's Claude login and any ANTHROPIC_API_KEY is removed from their environment.", Type: adapter.Bool, Default: "false"},
			{Key: "usage", Label: "Show subscription usage", Help: "Read the 5-hour and 7-day windows from the Claude login, as /usage shows them.", Type: adapter.Bool, Default: "true"},
			{Key: "extraArgs", Label: "Extra arguments", Help: "Added to every claude invocation, space-separated.", Type: adapter.Text},
		},
		New: func(v adapter.Values, env adapter.Env) (adapter.Connection, error) {
			return &Agent{
				binary:     v.Get("binary", "claude"),
				model:      v.Get("model", "opus"),
				permission: v.Get("permissionMode", "bypassPermissions"),
				dryModel:   v.Get("dryRunModel", "haiku"),
				useAPIKey:  v.Bool("useApiKey", false),
				usage:      v.Bool("usage", true),
				extra:      strings.Fields(v.Get("extraArgs", "")),
				pluginDir:  filepath.Join(env.DataDir, "claude-plugin"),
				env:        env,
			}, nil
		},
	})
}

// Agent is the Claude Code CLI.
type Agent struct {
	binary, model, permission, dryModel string
	useAPIKey, usage                    bool
	extra                               []string
	pluginDir                           string
	env                                 adapter.Env
}

// Check runs `claude --version`.
func (a *Agent) Check(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, a.binary, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w (is Claude Code installed? npm i -g @anthropic-ai/claude-code)", a.binary, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// Prepare writes the playbooks out as a plugin of skills.
func (a *Agent) Prepare(ctx context.Context, books map[string]string) error {
	if err := os.MkdirAll(filepath.Join(a.pluginDir, ".claude-plugin"), 0o755); err != nil {
		return err
	}
	manifest, _ := json.MarshalIndent(map[string]string{
		"name":        PluginName,
		"description": "Playbooks for agents run by open-supervisor",
		"version":     "1.0.0",
	}, "", "  ")
	if err := os.WriteFile(filepath.Join(a.pluginDir, ".claude-plugin", "plugin.json"), manifest, 0o644); err != nil {
		return err
	}
	for name, text := range books {
		dir := filepath.Join(a.pluginDir, "skills", name)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(text), 0o644); err != nil {
			return err
		}
	}
	if _, err := exec.LookPath(a.binary); err != nil {
		return fmt.Errorf("%s isn't on PATH: install Claude Code (npm i -g @anthropic-ai/claude-code) or set its binary", a.binary)
	}
	return nil
}

// PluginDir is where the playbooks are written, for using them by hand: claude --plugin-dir <dir>.
func (a *Agent) PluginDir() string { return a.pluginDir }

// NewSession makes a random UUID, as claude --session-id wants.
func (a *Agent) NewSession() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// SessionExists reports whether the session's transcript exists.
func (a *Agent) SessionExists(cwd, session string) bool {
	_, err := os.Stat(a.TranscriptPath(cwd, session))
	return err == nil
}

// RulesFiles are where Claude Code reads a project's rules.
func (a *Agent) RulesFiles() []string {
	return []string{"CLAUDE.md or .claude/CLAUDE.md (whichever the project has), and the rule skills it lists"}
}

// childEnv strips what would make a nested claude refuse to run or pick up the wrong login.
func (a *Agent) childEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		if k == "CLAUDECODE" || strings.HasPrefix(k, "CLAUDE_CODE_") {
			continue
		}
		if k == "ANTHROPIC_API_KEY" && !a.useAPIKey {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// AttachCommand is `claude --resume <session>` in the project's checkout.
func (a *Agent) AttachCommand(spec adapter.AttachSpec) (*exec.Cmd, error) {
	args := []string{"--resume", spec.Session, "--model", a.model, "--permission-mode", a.permission, "--plugin-dir", a.pluginDir}
	cmd := exec.Command(a.binary, append(args, a.extra...)...)
	cmd.Dir = spec.Cwd
	cmd.Env = a.childEnv(spec.Env)
	return cmd, nil
}

func (a *Agent) prompt(spec adapter.TurnSpec) string {
	text := "/" + PluginName + ":" + spec.Playbook
	if spec.Args != "" {
		text += " " + spec.Args
	}
	if spec.Body != "" {
		text += "\n\n" + spec.Body
	}
	if spec.DryRun {
		end := "End with a one-line summary."
		if spec.Structured {
			end = `End with state=review and summary="dry run".`
		}
		return "This is a supervisor dry run: do nothing and call no tools. The real prompt would have been:\n\n" + text + "\n\n" + end
	}
	return text
}

var nonAlnum = regexp.MustCompile(`[^A-Za-z0-9]`)

func claudeDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return d
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude")
}

// TranscriptPath: Claude keeps transcripts under ~/.claude/projects/<cwd with every non-alphanumeric as
// "-">/<session id>.jsonl.
func (a *Agent) TranscriptPath(cwd, session string) string {
	return filepath.Join(claudeDir(), "projects", nonAlnum.ReplaceAllString(cwd, "-"), session+".jsonl")
}
