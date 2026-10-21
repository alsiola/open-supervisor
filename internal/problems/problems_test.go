package problems

import (
	"path/filepath"
	"testing"
)

func TestWriteAndList(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "problems")
	if got := List(dir); got != nil {
		t.Fatalf("a missing folder has notes: %v", got)
	}
	for _, n := range []Note{
		{Issue: "12", Kind: "env", Body: "SUPERVISOR_SCRIPTS is unset\n"},
		{Issue: "12", Kind: "env", Body: "same again"},
		{Kind: "other", Body: "no issue"},
	} {
		if _, err := Write(dir, n); err != nil {
			t.Fatal(err)
		}
	}
	got := List(dir)
	if len(got) != 3 || Count(dir) != 3 {
		t.Fatalf("got %d notes, count %d", len(got), Count(dir))
	}
	if got[0].Name == got[1].Name {
		t.Fatal("two notes in the same millisecond share a name")
	}
	if n := got[0]; n.Issue != "12" || n.Kind != "env" || n.Body != "SUPERVISOR_SCRIPTS is unset" {
		t.Fatalf("first note: %+v", n)
	}
	if n := got[2]; n.Issue != "" || n.Kind != "other" || n.Body != "no issue" {
		t.Fatalf("third note: %+v", n)
	}
}

func TestValidKind(t *testing.T) {
	if !ValidKind("docs") || ValidKind("bogus") || ValidKind("") {
		t.Fatal("ValidKind")
	}
}
