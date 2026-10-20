package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
)

func TestPadKeepsWidth(t *testing.T) {
	s := redS.Render("hello world")
	if w := ansi.StringWidth(pad(s, 5)); w != 5 {
		t.Fatalf("width %d", w)
	}
	if w := ansi.StringWidth(pad("hi", 6)); w != 6 {
		t.Fatalf("width %d", w)
	}
}

func TestWrap(t *testing.T) {
	lines := wrap("one two three four five six seven", 12)
	for _, l := range lines {
		if len(l) > 12 {
			t.Fatalf("too long: %q", l)
		}
	}
	if strings.Join(lines, " ") != "one two three four five six seven" {
		t.Fatal(lines)
	}
	if got := wrap("a\n\nb", 20); len(got) != 3 {
		t.Fatal(got)
	}
}

func TestClean(t *testing.T) {
	if got := clean("\x1b[31mred\x1b[0m\tx\x07"); got != "red  x" {
		t.Fatalf("%q", got)
	}
}

func TestClaimSyntax(t *testing.T) {
	for in, want := range map[string][2]string{"12": {"", "12"}, "#12": {"", "12"}, "PROJ-12": {"", "PROJ-12"}, "club#12": {"club", "12"}, "web#PROJ-3": {"web", "PROJ-3"}} {
		g := claimRe.FindStringSubmatch(in)
		if g == nil || g[1] != want[0] || g[2] != want[1] {
			t.Errorf("%s: %v", in, g)
		}
	}
}
