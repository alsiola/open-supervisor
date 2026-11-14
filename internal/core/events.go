package core

import (
	"fmt"
	"strings"
	"time"
)

// EventType names the things an agent is told about between turns.
type EventType string

const (
	EvComment  EventType = "comment"  // someone said something on the issue or a PR
	EvChecks   EventType = "checks"   // every check on a PR's head finished (or none ever ran)
	EvPR       EventType = "pr"       // a PR opened, merged or closed
	EvIssue    EventType = "issue"    // the issue closed or reopened
	EvNote     EventType = "note"     // a note from the supervisor itself
	EvMessage  EventType = "message"  // a message the owner typed into the TUI
	EvWoke     EventType = "woke"     // what the issue was waiting on happened, or its time came
	EvConflict EventType = "conflict" // a PR conflicts with its base branch
)

// Event is one item in an issue's queue. Which fields are set depends on Type.
type Event struct {
	Type    EventType `json:"type"`
	Comment *Comment  `json:"comment,omitempty"`
	PR      int       `json:"pr,omitempty"`
	SHA     string    `json:"sha,omitempty"`
	Passed  bool      `json:"passed,omitempty"`
	Failed  []string  `json:"failed,omitempty"`
	Missing []string  `json:"missing,omitempty"` // checks: required ones that never reported
	Preview string    `json:"preview,omitempty"`
	None    bool      `json:"none,omitempty"` // checks: nothing ran on the commit at all
	// Conflicts, on checks that never reported: the PR conflicts with its base, which is why they didn't run.
	Conflicts bool      `json:"conflicts,omitempty"`
	Base      string    `json:"base,omitempty"`    // conflict: the base branch
	BaseSHA   string    `json:"baseSha,omitempty"` // conflict: the base branch's head it conflicts with
	Action    string    `json:"action,omitempty"`
	Text      string    `json:"text,omitempty"`
	At        time.Time `json:"at,omitempty"`
}

// Same reports whether two events are the same item, for taking one back out of a queue.
func (e Event) Same(o Event) bool {
	if e.Type != o.Type {
		return false
	}
	switch e.Type {
	case EvComment:
		return e.Comment != nil && o.Comment != nil && e.Comment.ID == o.Comment.ID && e.Comment.Kind == o.Comment.Kind
	case EvMessage, EvNote, EvWoke:
		return e.Text == o.Text && e.At.Equal(o.At)
	default:
		return e.PR == o.PR && e.SHA == o.SHA && e.Action == o.Action && e.BaseSHA == o.BaseSHA
	}
}

// Interjects reports whether the event may be handed to a turn already running: what people say. The rest
// waits for the next turn.
func (e Event) Interjects() bool {
	return (e.Type == EvComment || e.Type == EvMessage) && e.Wakes()
}

// Wakes reports whether the event is reason enough for a turn. Another agent's comment, direct or shared, isn't:
// it waits in the queue and arrives with the next turn something else starts.
func (e Event) Wakes() bool {
	return e.Type != EvComment || e.Comment == nil || e.Comment.Agent == nil
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
		if len(e.Failed) == 0 && len(e.Missing) > 0 {
			return "required checks missing on " + short(e.SHA)
		}
		return "checks failed on " + short(e.SHA)
	case EvPR:
		return fmt.Sprintf("PR #%d %s", e.PR, e.Action)
	case EvConflict:
		return fmt.Sprintf("PR #%d conflicts with %s", e.PR, e.Base)
	case EvIssue:
		return "issue " + e.Action
	case EvNote:
		return "note"
	case EvWoke:
		return "woke: " + e.Text
	case EvMessage:
		return "message from the owner"
	}
	return string(e.Type)
}

// conflictAdvice is what to do about a PR that conflicts with its base branch.
func conflictAdvice(base string) string {
	if base == "" {
		base = "the base branch"
	}
	return fmt.Sprintf("Rebase the branch onto %s (fetch it from `$OPEN_SUPERVISOR_REMOTE` first), resolve the conflicts, rerun what the project's rules say must pass, force-push, and end the turn with `building`.", base)
}

// conflictChecks is why a conflicting PR's checks never ran.
func conflictChecks(base string) string {
	return "The PR conflicts with its base branch, and the code host doesn't run its workflows on a PR that can't merge: that's why. " + conflictAdvice(base) + " The checks start on the new head."
}

const missingAdvice = "The PR can't merge without them. A deploy service can queue a build behind an earlier one without reporting anything on the commit, so first look at its deploy list for this commit: another push would only queue another build. If it is queued or building, end the turn with `building`: you'll hear when it reports. If it never started, find out why (a `[skip …]` in the PR title or a commit message stops some deploys) and start it, often with a new push; if it shouldn't run on this PR, say so in your report."

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
			case e.Conflicts && (e.None || len(e.Failed) == 0):
				s := fmt.Sprintf("### No checks ran on PR #%d at %s", e.PR, short(e.SHA))
				if len(e.Missing) > 0 {
					s = fmt.Sprintf("### Required checks never reported on PR #%d at %s: %s", e.PR, short(e.SHA), strings.Join(e.Missing, ", "))
				}
				parts = append(parts, s+"\n"+conflictChecks(e.Base))
			case e.None:
				parts = append(parts, fmt.Sprintf("### No checks ran on PR #%d at %s\nThe repo has no CI or deploy previews: treat the PR as checked once what the project's rules say must pass has passed locally.", e.PR, short(e.SHA)))
			case e.Passed:
				s := fmt.Sprintf("### Checks passed on PR #%d at %s", e.PR, short(e.SHA))
				if e.Preview != "" {
					s += "\nPreview: " + e.Preview
				}
				parts = append(parts, s)
			case len(e.Failed) == 0:
				parts = append(parts, fmt.Sprintf("### Required checks haven't reported on PR #%d at %s: %s\n%s", e.PR, short(e.SHA), strings.Join(e.Missing, ", "), missingAdvice))
			default:
				s := fmt.Sprintf("### Checks failed on PR #%d at %s: %s\nSee `$OPEN_SUPERVISOR agent checks %d` for links to each.", e.PR, short(e.SHA), strings.Join(e.Failed, ", "), e.PR)
				if len(e.Missing) > 0 {
					s += fmt.Sprintf("\nThese required checks haven't reported either: %s. %s", strings.Join(e.Missing, ", "), missingAdvice)
				}
				parts = append(parts, s)
			}
		case EvPR:
			parts = append(parts, fmt.Sprintf("### PR #%d %s", e.PR, e.Action))
		case EvConflict:
			base := e.Base
			if base == "" {
				base = "its base branch"
			}
			s := fmt.Sprintf("### PR #%d conflicts with %s", e.PR, base)
			if e.BaseSHA != "" {
				s += " at " + short(e.BaseSHA)
			}
			parts = append(parts, s+"\nIt can't merge, and the code host won't run its checks until it can. "+conflictAdvice(e.Base))
		case EvIssue:
			parts = append(parts, "### The issue was "+e.Action)
		case EvNote:
			parts = append(parts, "### Note from the supervisor\n"+e.Text)
		case EvWoke:
			parts = append(parts, "### What you were waiting for\n"+e.Text)
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
