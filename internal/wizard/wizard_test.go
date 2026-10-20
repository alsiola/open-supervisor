package wizard

import (
	"strings"
	"testing"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/config"
)

func TestSummary(t *testing.T) {
	cfg := config.New("/tmp/x.yaml")
	if s := Summary(cfg); !strings.Contains(s, "none yet") || !strings.Contains(s, "poll every 15 min") {
		t.Fatal(s)
	}
	cfg.Connections["gh"] = &config.Connection{Type: "github", Values: adapter.Values{}}
	cfg.Settings.Agent = "claude"
	cfg.Settings.Tunnel = "ngrok"
	cfg.Projects = []*config.Project{{Name: "web", Root: "~/Code/web", Repo: &config.Binding{Connection: "gh"}, Tracker: &config.Binding{Connection: "gh"}}}
	s := Summary(cfg)
	for _, want := range []string{"gh (github)", "Agent: claude", "Webhooks: ngrok", "web  ~/Code/web  (code gh, issues gh)"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q in\n%s", want, s)
		}
	}
}

func TestFoundGit(t *testing.T) {
	if _, ok := Found()["git"]; !ok {
		t.Skip("git not on PATH")
	}
}
