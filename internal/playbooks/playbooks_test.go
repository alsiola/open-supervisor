package playbooks

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSetupGuide(t *testing.T) {
	g := Setup("/usr/local/bin/open-supervisor")
	if strings.Contains(g, "{{bin}}") || !strings.Contains(g, "/usr/local/bin/open-supervisor config connection set") || Description(g) == "" {
		t.Fatal("guide not filled in")
	}
	if _, ok := All("direct")["setup"]; ok {
		t.Fatal("the setup guide is among the playbooks agents get for turns")
	}
}

func TestProjectAdditions(t *testing.T) {
	root := t.TempDir()
	if Project(root, "work-issue") != "" {
		t.Fatal("additions without a file")
	}
	dir := filepath.Join(root, ProjectDir)
	_ = os.MkdirAll(dir, 0o755)
	_ = os.WriteFile(filepath.Join(dir, "work-issue.md"), []byte("---\nreplace: true\n---\n\n- Take screenshots at 1280px.\n"), 0o644)
	if got := Project(root, "work-issue"); got != "- Take screenshots at 1280px." {
		t.Fatalf("additions %q", got)
	}
}

func TestCoordinationModes(t *testing.T) {
	direct, ambient := All("direct")["work-issue"], All("ambient")["work-issue"]
	for _, p := range []string{direct, ambient} {
		if strings.Contains(p, "<!-- direct -->") || strings.Contains(p, "<!-- ambient -->") || strings.Contains(p, "<!-- end -->") {
			t.Fatal("a marker line is left in")
		}
		if !strings.Contains(p, "## Log corrections") || !strings.Contains(p, "The first turn's message lists the issues in flight") {
			t.Fatal("a shared part is missing")
		}
	}
	if !strings.Contains(direct, "--issue <other>") || strings.Contains(direct, "Shared automatically") {
		t.Fatal("direct mode should ask agents to comment on each other's issues, and not mention shares")
	}
	if strings.Contains(ambient, "--issue <") || !strings.Contains(ambient, "Shared automatically") || !strings.Contains(ambient, "`shared`") {
		t.Fatal("ambient mode should drop commenting on other issues and explain shares")
	}
}
