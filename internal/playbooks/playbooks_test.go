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
	if _, ok := All()["setup"]; ok {
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
