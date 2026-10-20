// Package playbooks holds the instructions agents follow under the supervisor: how to work an issue in turns,
// and how to sweep correction notes into a project's rules. They are written for any agent, tracker and code
// host: everything that touches a service goes through `$OPEN_SUPERVISOR agent …`, which the supervisor routes
// to the project's adapters. An agent adapter decides how to hand them over (Claude Code gets them as a plugin
// of skills; an agent without skills gets the text in its prompt).
package playbooks

import (
	"embed"
	"strings"
)

//go:embed *.md
var files embed.FS

//go:embed setup/setup.md
var setupGuide string

// Setup is the setup guide, a skill for an agent setting the supervisor up with its owner (see `init --with`).
// It is kept apart from All, so agents working issues never see it. bin is how the agent runs the supervisor.
func Setup(bin string) string { return strings.ReplaceAll(setupGuide, "{{bin}}", bin) }

// All returns every playbook by name ("work-issue", "sweep").
func All() map[string]string {
	out := map[string]string{}
	entries, _ := files.ReadDir(".")
	for _, e := range entries {
		data, err := files.ReadFile(e.Name())
		if err != nil {
			continue
		}
		out[strings.TrimSuffix(e.Name(), ".md")] = string(data)
	}
	return out
}

// Body is a playbook without its frontmatter, for agents that take it in the prompt.
func Body(text string) string {
	if !strings.HasPrefix(text, "---\n") {
		return text
	}
	if i := strings.Index(text[4:], "\n---\n"); i >= 0 {
		return strings.TrimLeft(text[4+i+5:], "\n")
	}
	return text
}

// Description is a playbook's frontmatter description.
func Description(text string) string {
	if !strings.HasPrefix(text, "---\n") {
		return ""
	}
	end := strings.Index(text[4:], "\n---\n")
	if end < 0 {
		return ""
	}
	for _, line := range strings.Split(text[4:4+end], "\n") {
		if v, ok := strings.CutPrefix(line, "description:"); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}
