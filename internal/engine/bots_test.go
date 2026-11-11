package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/alsiola/open-supervisor/internal/core"
)

func TestBotCounts(t *testing.T) {
	root := t.TempDir()
	p := &Project{Root: root}
	chatter := core.Comment{Kind: core.OnPR, Author: "netlify[bot]", Bot: true}
	if p.botCounts(chatter) {
		t.Fatal("a bot's PR comment counted with no bots listed")
	}
	for _, k := range []core.CommentKind{core.OnReview, core.OnInline} {
		if !p.botCounts(core.Comment{Kind: k, Author: "reviewer[bot]", Bot: true}) {
			t.Fatalf("a bot's %s comment didn't count", k)
		}
	}
	if err := os.WriteFile(filepath.Join(root, RepoFile), []byte("bots:\n  - Netlify[bot]\n  - triage\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !p.botCounts(chatter) || !p.botCounts(core.Comment{Kind: core.OnIssue, Author: "triage[bot]", Bot: true}) {
		t.Fatal("a listed bot's comment didn't count")
	}
	if p.botCounts(core.Comment{Kind: core.OnIssue, Author: "ci[bot]", Bot: true}) {
		t.Fatal("an unlisted bot's comment counted")
	}
}
