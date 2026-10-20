package playbooks

import (
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
