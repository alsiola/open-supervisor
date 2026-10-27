package ambient

import (
	"fmt"
	"regexp"
	"strings"
)

// Header starts every share; IsShare looks for it so a share is never shared again.
const Header = "Shared automatically by open-supervisor"

// shareRe finds the header however a tracker's formatting round trip leaves it (bold, italics, a wiki heading).
var shareRe = regexp.MustCompile(`(?im)^[\W_]*` + regexp.QuoteMeta(Header) + `\b`)

// IsShare reports whether a comment is a share.
func IsShare(body string) bool { return shareRe.MatchString(body) }

// MinLength is the shortest comment, in characters of text, worth asking about: shorter ones are acknowledgements
// and status lines ("Working on this on branch …").
const MinLength = 120

// Substantive reports whether a comment has enough text to carry information another issue could use.
func Substantive(body string) bool {
	return len([]rune(strings.Join(strings.Fields(body), " "))) >= MinLength
}

// MaxExcerpt is the most of a comment a share quotes, in characters; the link has the rest.
const MaxExcerpt = 3000

// Excerpt is the comment cut to n characters at a paragraph or line break where it can be, with a mark where
// it was cut.
func Excerpt(body string, n int) string {
	body = strings.TrimSpace(body)
	r := []rune(body)
	if len(r) <= n {
		return body
	}
	cut := string(r[:n])
	if i := strings.LastIndex(cut, "\n\n"); i > n/2 {
		cut = cut[:i]
	} else if i := strings.LastIndex(cut, "\n"); i > n/2 {
		cut = cut[:i]
	}
	return strings.TrimSpace(cut) + "\n\n[…]"
}

// Origin says where a shared comment was posted.
type Origin struct {
	// Issue is the issue the posting agent works, as the target's readers should see it (#12, ENG-4, site#12);
	// Title is its title.
	Issue, Title string
	// Where is the comment's place: "on the issue", "on PR #34", "in a review of PR #34".
	Where string
	// URL is the comment's own link.
	URL string
}

// Body is the share's comment: who posted it where, the comment quoted, and the link to the original. It
// quotes rather than summarises, so the source's words reach the target unchanged.
func Body(o Origin, body string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**%s** from %s", Header, o.Issue)
	if o.Title != "" {
		fmt.Fprintf(&b, " (%s)", o.Title)
	}
	fmt.Fprintf(&b, ". The agent working on it posted this %s; the supervisor picked it out as something that could bear on this issue. It is the source's account, not checked here.\n\n", o.Where)
	for _, line := range strings.Split(Excerpt(body, MaxExcerpt), "\n") {
		b.WriteString(strings.TrimRight("> "+line, " ") + "\n")
	}
	if o.URL != "" {
		fmt.Fprintf(&b, "\nOriginal: %s\n", o.URL)
	}
	return b.String()
}
