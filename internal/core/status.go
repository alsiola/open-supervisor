package core

import (
	"regexp"
	"strings"
	"time"
)

// AgentState is how a turn says it ended.
type AgentState string

const (
	Building AgentState = "building" // pushed; waiting for checks or a preview
	Review   AgentState = "review"   // something is up for the owner
	Help     AgentState = "help"     // asked the owner something it can't go on without
	Done     AgentState = "done"     // everything asked is handled; only a merge or close is left
	Finished AgentState = "finished" // the PR merged or closed (or the issue closed) and it cleaned up
)

// Status is the structured result every issue turn ends with.
type Status struct {
	State   AgentState `json:"state"`
	Summary string     `json:"summary"`
	PRs     []int      `json:"prs,omitempty"`
	Preview string     `json:"preview,omitempty"`
	Touches string     `json:"touches,omitempty"`
}

// StatusSchema is the JSON schema an agent is asked to end each issue turn with.
var StatusSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"state": map[string]any{
			"type": "string",
			"enum": []string{"building", "review", "help", "done", "finished"},
			"description": "building: you pushed and are waiting for checks or a preview. review: something is up for the owner. " +
				"help: you asked the owner something you cannot go on without. done: everything asked is handled and only a merge or close is left. " +
				"finished: the PR merged or closed (or the issue closed) and you have cleaned up.",
		},
		"summary": map[string]any{"type": "string", "description": "What this turn did, in one line."},
		"prs":     map[string]any{"type": "array", "items": map[string]any{"type": "integer"}, "description": "Every PR number that belongs to this issue."},
		"preview": map[string]any{"type": "string", "description": "The deploy preview URL, once known."},
		"touches": map[string]any{"type": "string", "description": "What the work changes (pages, data, pipeline steps), in a few words, for other agents."},
	},
	"required":             []string{"state", "summary"},
	"additionalProperties": false,
}

// Outcome is how a turn ended, as the agent adapter read it.
type Outcome struct {
	OK       bool          `json:"ok"`
	Status   *Status       `json:"status,omitempty"`
	Text     string        `json:"text,omitempty"`
	Cost     float64       `json:"cost,omitempty"`
	Tokens   *Tokens       `json:"tokens,omitempty"`
	Models   []string      `json:"models,omitempty"`
	Error    string        `json:"error,omitempty"`
	Duration time.Duration `json:"duration"`
}

// runAtRe finds a runAt marker: `<!-- runAt: 2026-10-25T09:00Z -->` (hidden in Markdown hosts) or a line of its
// own reading `[runAt: 2026-10-25T09:00Z]` (for trackers whose comments can't hide anything).
var runAtRe = regexp.MustCompile(`(?:<!--\s*runAt[:(\s]\s*([^\s)]+)\s*\)?\s*-->|\[runAt:\s*([^\]\s]+)\s*\])`)

// RunAtIn returns the last valid runAt marker in a body.
func RunAtIn(body string) (time.Time, bool) {
	var found time.Time
	ok := false
	for _, m := range runAtRe.FindAllStringSubmatch(body, -1) {
		v := m[1]
		if v == "" {
			v = m[2]
		}
		if t, err := ParseTime(v); err == nil {
			found, ok = t, true
		}
	}
	return found, ok
}

// ParseTime reads an ISO instant loosely: with or without seconds, Z or an offset; a bare date is midnight UTC.
func ParseTime(s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	layouts := []string{time.RFC3339Nano, time.RFC3339, "2006-01-02T15:04Z07:00", "2006-01-02T15:04Z", "2006-01-02T15:04:05Z", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"}
	var err error
	for _, l := range layouts {
		var t time.Time
		if t, err = time.Parse(l, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, err
}
