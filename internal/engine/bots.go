package engine

import (
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/alsiola/open-supervisor/internal/core"
)

// botCounts reports whether a bot's comment reaches the agent like a person's. A review, and a comment in a
// review's inline thread, always does: a code reviewer that posts as an app is never just chatter. Any other
// comment does only from a bot the checkout's RepoFile lists under bots, with or without its [bot] suffix.
func (p *Project) botCounts(c core.Comment) bool {
	if c.Kind == core.OnReview || c.Kind == core.OnInline {
		return true
	}
	return listsBot(repoBots(readRepoFile(p.Root)), c.Author)
}

// repoBots are the bots a RepoFile lists under bots.
func repoBots(data []byte) []string {
	if data == nil {
		return nil
	}
	var own struct {
		Bots []string `yaml:"bots"`
	}
	_ = yaml.Unmarshal(data, &own) // a file that doesn't parse is logged where its jobs are read
	return own.Bots
}

func listsBot(bots []string, author string) bool {
	name := botName(author)
	for _, b := range bots {
		if b = botName(b); b != "" && b == name {
			return true
		}
	}
	return false
}

// botName is a bot's login without its [bot] suffix, in lower case.
func botName(s string) string {
	return strings.ToLower(strings.TrimSuffix(strings.TrimSpace(s), "[bot]"))
}
