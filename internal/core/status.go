package core

import (
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
	Handoff string     `json:"handoff,omitempty"`
	// Shared are the links of comments shared with the issue automatically that changed what the turn did.
	Shared []string `json:"shared,omitempty"`
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
		"handoff": map[string]any{"type": "string", "description": "A note to the session that picks this issue up next if this one has gone cold: " +
			"what is pending, the judgement calls you made and why, and what to check next. Intent only; the issue, the PR and git hold the facts."},
		"shared": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "The links of comments shared with this issue automatically that changed what you did this turn; leave out when none did."},
	},
	"required":             []string{"state", "summary"},
	"additionalProperties": false,
}

// Outcome is how a turn ended, as the agent adapter read it.
type Outcome struct {
	OK     bool     `json:"ok"`
	Status *Status  `json:"status,omitempty"`
	Text   string   `json:"text,omitempty"`
	Cost   float64  `json:"cost,omitempty"`
	Tokens *Tokens  `json:"tokens,omitempty"`
	Models []string `json:"models,omitempty"`
	// ModelCalls and ToolCalls are what the turn did: its model requests and the tools those called.
	ModelCalls int           `json:"modelCalls,omitempty"`
	ToolCalls  int           `json:"toolCalls,omitempty"`
	Error      string        `json:"error,omitempty"`
	Duration   time.Duration `json:"duration"`
	// Limited says the turn stopped on the subscription's usage limit; ResetsAt is when that lifts, if known.
	Limited  bool       `json:"limited,omitempty"`
	ResetsAt *time.Time `json:"resetsAt,omitempty"`
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

// Clock is a time to come as the owner reads it: the local time of day, with the day when it isn't today's.
func Clock(t time.Time) string {
	t = t.Local()
	if y, m, d := time.Now().Date(); t.Year() == y && t.Month() == m && t.Day() == d {
		return t.Format("15:04")
	}
	return t.Format("Mon 2 Jan 15:04")
}
