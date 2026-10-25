// Package core holds the domain the supervisor works in, free of any vendor: issues and their comments, pull
// requests and their checks, the events an agent is told about between turns, and the status a turn ends with.
// Adapters translate their service's shapes into these; the engine only ever sees these.
package core

import (
	"strings"
	"time"
)

// Issue is one item in a tracker (a GitHub issue, a Jira issue, a Linear issue). Key is the tracker's own
// identifier as people write it: "12" for GitHub, "PROJ-12" for Jira.
type Issue struct {
	Key      string   `json:"key"`
	Title    string   `json:"title"`
	Body     string   `json:"body,omitempty"`
	URL      string   `json:"url,omitempty"`
	Author   string   `json:"author,omitempty"`
	Labels   []string `json:"labels,omitempty"`
	Open     bool     `json:"open"`
	Reopened bool     `json:"reopened,omitempty"` // the tracker says it was reopened (GitHub's stateReason)
	// Waiting: open, but somewhere the tracker isn't offering it to be worked yet (a Linear backlog state).
	// It is never claimed on its own, only by hand.
	Waiting   bool      `json:"waiting,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	// Attrs are the tracker's other properties, for routing issues to projects: "project", "team",
	// "components", "assignee", a custom field. Each adapter documents the names it fills; a value the issue
	// lacks is absent, never an empty string.
	Attrs map[string][]string `json:"attrs,omitempty"`
}

// Attr is a property's values by name: "labels", "key", "title" and "author" from the issue itself, anything
// else from Attrs. Names ignore case.
func (i Issue) Attr(name string) []string {
	switch strings.ToLower(name) {
	case "labels", "label":
		return i.Labels
	case "key":
		return []string{i.Key}
	case "title":
		return []string{i.Title}
	case "author":
		if i.Author == "" {
			return nil
		}
		return []string{i.Author}
	}
	if v, ok := i.Attrs[name]; ok {
		return v
	}
	for k, v := range i.Attrs {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return nil
}

// SetAttr records a property, leaving out empty values; nothing at all leaves the property absent.
func (i *Issue) SetAttr(name string, values ...string) {
	var keep []string
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			keep = append(keep, v)
		}
	}
	if len(keep) == 0 {
		return
	}
	if i.Attrs == nil {
		i.Attrs = map[string][]string{}
	}
	i.Attrs[name] = append(i.Attrs[name], keep...)
}

// HasLabel reports whether the issue carries the label, ignoring case.
func (i Issue) HasLabel(label string) bool {
	for _, l := range i.Labels {
		if strings.EqualFold(l, label) {
			return true
		}
	}
	return false
}

// CommentKind says where a comment was made.
type CommentKind string

const (
	OnIssue  CommentKind = "issue"  // on the issue itself
	OnPR     CommentKind = "pr"     // on a PR's conversation
	OnInline CommentKind = "inline" // in a PR's inline review thread
	OnReview CommentKind = "review" // a PR review's body
)

// AgentMark is what an adapter finds on a comment an agent posted through the supervisor. From is the issue
// the posting agent works, so a comment one agent leaves on another's issue reaches that agent while each
// agent's own comments never come back to it; an empty From is skipped by every agent.
type AgentMark struct {
	From string `json:"from,omitempty"`
}

// Comment is anything a person said on an issue or one of its PRs.
type Comment struct {
	ID          string      `json:"id"`
	Kind        CommentKind `json:"kind"`
	Author      string      `json:"author"`
	Bot         bool        `json:"bot,omitempty"`
	Body        string      `json:"body"`
	URL         string      `json:"url,omitempty"`
	At          time.Time   `json:"at"`
	PR          int         `json:"pr,omitempty"`
	Path        string      `json:"path,omitempty"`
	Line        int         `json:"line,omitempty"`
	InReplyTo   string      `json:"inReplyTo,omitempty"`
	ReviewState string      `json:"reviewState,omitempty"`
	Agent       *AgentMark  `json:"agent,omitempty"`
}

// PRState is a pull request's lifecycle.
type PRState string

const (
	PROpen   PRState = "open"
	PRMerged PRState = "merged"
	PRClosed PRState = "closed" // closed (declined) without merging
)

// PR is a pull request on a code host.
type PR struct {
	ID      int     `json:"id"`
	Title   string  `json:"title,omitempty"`
	State   PRState `json:"state"`
	Branch  string  `json:"branch"`
	Base    string  `json:"base,omitempty"`
	HeadSHA string  `json:"headSha"`
	Body    string  `json:"body,omitempty"`
	URL     string  `json:"url,omitempty"`
}

// CheckRun is one check or commit status on a commit.
type CheckRun struct {
	Name  string `json:"name"`
	State string `json:"state"` // pending, success, failure
	URL   string `json:"url,omitempty"`
}

// Checks is the combined state of every check on a commit.
type Checks struct {
	// Complete: every check has finished and at least one exists.
	Complete bool `json:"complete"`
	// Any: something at all is known about the commit.
	Any     bool       `json:"any"`
	Failed  []string   `json:"failed,omitempty"`
	Preview string     `json:"preview,omitempty"`
	Runs    []CheckRun `json:"runs,omitempty"`
}

// Missing are the required checks that haven't reported on the commit at all.
func (c Checks) Missing(required []string) []string {
	seen := map[string]bool{}
	for _, r := range c.Runs {
		seen[r.Name] = true
	}
	var out []string
	for _, name := range required {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}

// Tokens are a turn's token counts, summed over the models it used.
type Tokens struct {
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	CacheRead     int64 `json:"cacheRead"`
	CacheCreation int64 `json:"cacheCreation"`
}

// Add returns the sum of two counts; either may be nil.
func (t *Tokens) Add(o *Tokens) *Tokens {
	if o == nil {
		return t
	}
	if t == nil {
		c := *o
		return &c
	}
	return &Tokens{t.Input + o.Input, t.Output + o.Output, t.CacheRead + o.CacheRead, t.CacheCreation + o.CacheCreation}
}

// UsageWindow is one of an agent subscription's rate-limit windows.
type UsageWindow struct {
	Name     string    `json:"name"` // "5h", "7d"
	Used     float64   `json:"used"` // percent
	ResetsAt time.Time `json:"resetsAt"`
}

// Usage is what an agent reports about its subscription's limits.
type Usage struct {
	Windows   []UsageWindow `json:"windows"`
	FetchedAt time.Time     `json:"fetchedAt"`
}

// Activity is what an agent is doing right now, from its transcript.
type Activity struct {
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// EntryKind is the kind of line in a session transcript.
type EntryKind string

const (
	EntryUser      EntryKind = "user"
	EntryAssistant EntryKind = "assistant"
	EntryTool      EntryKind = "tool"
	EntryResult    EntryKind = "result"
	EntryError     EntryKind = "error"
)

// Entry is one line of a session transcript as a reader of the session would see it.
type Entry struct {
	At   time.Time `json:"at"`
	Kind EntryKind `json:"kind"`
	Text string    `json:"text"`
}
