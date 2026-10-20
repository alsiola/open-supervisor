package core

import (
	"fmt"
	"strings"
	"time"
)

// EventType names the things an agent is told about between turns.
type EventType string

const (
	EvComment EventType = "comment" // someone said something on the issue or a PR
	EvChecks  EventType = "checks"  // every check on a PR's head finished (or none ever ran)
	EvPR      EventType = "pr"      // a PR opened, merged or closed
	EvIssue   EventType = "issue"   // the issue closed or reopened
	EvNote    EventType = "note"    // a note from the supervisor itself
	EvMessage EventType = "message" // a message the owner typed into the TUI
)

// Event is one item in an issue's queue. Which fields are set depends on Type.
type Event struct {
	Type    EventType `json:"type"`
	Comment *Comment  `json:"comment,omitempty"`
	PR      int       `json:"pr,omitempty"`
	SHA     string    `json:"sha,omitempty"`
	Passed  bool      `json:"passed,omitempty"`
	Failed  []string  `json:"failed,omitempty"`
	Preview string    `json:"preview,omitempty"`
	None    bool      `json:"none,omitempty"` // checks: nothing ran on the commit at all
	Action  string    `json:"action,omitempty"`
	Text    string    `json:"text,omitempty"`
	At      time.Time `json:"at,omitempty"`
}

// Same reports whether two events are the same item, for taking one back out of a queue.
func (e Event) Same(o Event) bool {
	if e.Type != o.Type {
		return false
	}
	switch e.Type {
	case EvComment:
		return e.Comment != nil && o.Comment != nil && e.Comment.ID == o.Comment.ID && e.Comment.Kind == o.Comment.Kind
	case EvMessage, EvNote:
		return e.Text == o.Text && e.At.Equal(o.At)
	default:
		return e.PR == o.PR && e.SHA == o.SHA && e.Action == o.Action
	}
}

// Interjects reports whether the event may be handed to a turn already running: what people say. The rest
// waits for the next turn.
func (e Event) Interjects() bool {
	return e.Type == EvComment || e.Type == EvMessage
}

func short(sha string) string {
	if len(sha) > 7 {
		return sha[:7]
	}
	return sha
}

// Describe is the event in a few words, for the log.
func (e Event) Describe() string {
	switch e.Type {
	case EvComment:
		if e.Comment == nil {
			return "comment"
		}
		return fmt.Sprintf("%s comment by %s", e.Comment.Kind, e.Comment.Author)
	case EvChecks:
		if e.None {
			return "no checks on " + short(e.SHA)
		}
		if e.Passed {
			return "checks passed on " + short(e.SHA)
		}
		return "checks failed on " + short(e.SHA)
	case EvPR:
		return fmt.Sprintf("PR #%d %s", e.PR, e.Action)
	case EvIssue:
		return "issue " + e.Action
	case EvNote:
		return "note"
	case EvMessage:
		return "message from the owner"
	}
	return string(e.Type)
}

// Render is the events block an agent reads at the start of a turn. Replies go through the supervisor's own
// agent commands, so the instructions are the same whatever tracker or code host the project uses.
func Render(events []Event) string {
	var parts []string
	for _, e := range events {
		switch e.Type {
		case EvComment:
			c := e.Comment
			if c == nil {
				continue
			}
			var where, reply string
			switch c.Kind {
			case OnIssue:
				where = "Comment on the issue"
				reply = "reply with `$OPEN_SUPERVISOR agent issue comment <file>`"
			case OnPR:
				where = fmt.Sprintf("Comment on PR #%d", c.PR)
				reply = fmt.Sprintf("reply with `$OPEN_SUPERVISOR agent pr comment %d <file>`", c.PR)
			case OnInline:
				where = fmt.Sprintf("Inline comment on PR #%d", c.PR)
				if c.Path != "" {
					where += " at " + c.Path
					if c.Line > 0 {
						where += fmt.Sprintf(":%d", c.Line)
					}
				}
				reply = fmt.Sprintf("reply with `$OPEN_SUPERVISOR agent pr reply %d %s <file>`", c.PR, c.ID)
			case OnReview:
				where = fmt.Sprintf("Review on PR #%d", c.PR)
				if c.ReviewState != "" {
					where += " (" + strings.ReplaceAll(strings.ToLower(c.ReviewState), "_", " ") + ")"
				}
				reply = fmt.Sprintf("reply with `$OPEN_SUPERVISOR agent pr comment %d <file>`", c.PR)
			}
			parts = append(parts, fmt.Sprintf("### %s by %s (%s; %s)\n%s\n\n%s", where, c.Author, c.At.UTC().Format(time.RFC3339), reply, c.URL, strings.TrimSpace(c.Body)))
		case EvChecks:
			switch {
			case e.None:
				parts = append(parts, fmt.Sprintf("### No checks ran on PR #%d at %s\nThe repo has no CI or deploy previews: treat the PR as checked once what the project's rules say must pass has passed locally.", e.PR, short(e.SHA)))
			case e.Passed:
				s := fmt.Sprintf("### Checks passed on PR #%d at %s", e.PR, short(e.SHA))
				if e.Preview != "" {
					s += "\nPreview: " + e.Preview
				}
				parts = append(parts, s)
			default:
				parts = append(parts, fmt.Sprintf("### Checks failed on PR #%d at %s: %s\nSee `$OPEN_SUPERVISOR agent checks %d` for links to each.", e.PR, short(e.SHA), strings.Join(e.Failed, ", "), e.PR))
			}
		case EvPR:
			parts = append(parts, fmt.Sprintf("### PR #%d %s", e.PR, e.Action))
		case EvIssue:
			parts = append(parts, "### The issue was "+e.Action)
		case EvNote:
			parts = append(parts, "### Note from the supervisor\n"+e.Text)
		case EvMessage:
			parts = append(parts, fmt.Sprintf("### Message from the owner, typed into the supervisor (%s; answer in your session, no reply on the issue needed)\n\n%s", e.At.UTC().Format(time.RFC3339), strings.TrimSpace(e.Text)))
		}
	}
	return strings.Join(parts, "\n\n")
}

// RenderInterjection is an event handed to a turn that is already running.
func RenderInterjection(e Event) string {
	return "## Arrived during this turn\n\nThis came in while you were working. Take it into account now, in this turn, as you would the events at the start of one.\n\n" + Render([]Event{e})
}
