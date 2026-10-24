package core

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// CondKind names what a wait can be on.
type CondKind string

const (
	CondPR    CondKind = "pr"    // a PR merging or closing
	CondIssue CondKind = "issue" // an issue closing, or the agent on it ending a turn in a state
	CondJob   CondKind = "job"   // a run of one of a project's jobs ending
)

// Cond is one thing an issue can wait on: `pr:1257:merged`, `issue:12:review`, `job:backup`, each optionally
// in another project under the same supervisor (`pr:club/88:merged`).
type Cond struct {
	Kind CondKind `json:"kind"`
	// Project is the project the PR, issue or job belongs to; empty is the waiting issue's own.
	Project string `json:"project,omitempty"`
	// Ref is the PR number, the issue key or the job name.
	Ref string `json:"ref"`
	// On is what is waited for: merged or closed for a PR; closed or an agent state for an issue; empty for a job.
	On string `json:"on,omitempty"`
}

// issueOns are what an issue condition can wait for.
var issueOns = []string{"closed", string(Building), string(Review), string(Help), string(Done), string(Finished)}

// ParseCond reads a condition as an agent writes it. A PR's defaults to merged, an issue's to closed.
func ParseCond(s string) (Cond, error) {
	parts := strings.Split(strings.TrimSpace(s), ":")
	if len(parts) < 2 || len(parts) > 3 || parts[1] == "" {
		return Cond{}, fmt.Errorf("%q isn't a condition: write pr:<n>[:merged|closed], issue:<key>[:closed|<state>] or job:<name>", s)
	}
	c := Cond{Kind: CondKind(strings.ToLower(parts[0])), Ref: parts[1]}
	if len(parts) == 3 {
		c.On = strings.ToLower(parts[2])
	}
	if i := strings.LastIndex(c.Ref, "/"); i >= 0 {
		c.Project, c.Ref = c.Ref[:i], c.Ref[i+1:]
		if c.Project == "" || c.Ref == "" {
			return Cond{}, fmt.Errorf("%q: write <project>/<ref> for another project", s)
		}
	}
	switch c.Kind {
	case CondPR:
		c.Ref = strings.TrimPrefix(c.Ref, "#")
		if n, err := strconv.Atoi(c.Ref); err != nil || n <= 0 {
			return Cond{}, fmt.Errorf("%q: %q isn't a PR number", s, c.Ref)
		}
		if c.On == "" {
			c.On = string(PRMerged)
		}
		if c.On != string(PRMerged) && c.On != string(PRClosed) {
			return Cond{}, fmt.Errorf("%q: a PR can be waited on to be merged or closed", s)
		}
	case CondIssue:
		c.Ref = strings.TrimPrefix(c.Ref, "#")
		if c.On == "" {
			c.On = "closed"
		}
		ok := false
		for _, o := range issueOns {
			ok = ok || c.On == o
		}
		if !ok {
			return Cond{}, fmt.Errorf("%q: an issue can be waited on to be %s", s, strings.Join(issueOns, ", "))
		}
	case CondJob:
		if c.On != "" {
			return Cond{}, fmt.Errorf("%q: a job is waited on for its next run to end; write job:<name>", s)
		}
	default:
		return Cond{}, fmt.Errorf("%q: waits are on a pr, an issue or a job", s)
	}
	return c, nil
}

// String is the condition as ParseCond reads it.
func (c Cond) String() string {
	ref := c.Ref
	if c.Project != "" {
		ref = c.Project + "/" + ref
	}
	if c.On == "" {
		return string(c.Kind) + ":" + ref
	}
	return string(c.Kind) + ":" + ref + ":" + c.On
}

// In is the project the condition is about, own for the waiting issue's.
func (c Cond) In(own string) string {
	if c.Project != "" {
		return c.Project
	}
	return own
}

// Describe is what the condition waits for, in words: "PR #1257 to merge", "#12 to reach review".
func (c Cond) Describe() string {
	where := ""
	if c.Project != "" {
		where = " in " + c.Project
	}
	switch c.Kind {
	case CondPR:
		verb := "merge"
		if c.On == string(PRClosed) {
			verb = "close"
		}
		return fmt.Sprintf("PR #%s%s to %s", c.Ref, where, verb)
	case CondIssue:
		if c.On == "closed" {
			return fmt.Sprintf("%s%s to close", issueRef(c.Ref), where)
		}
		return fmt.Sprintf("the agent on %s%s to end a turn in %s", issueRef(c.Ref), where, c.On)
	case CondJob:
		return fmt.Sprintf("the next run of the %s job%s to end", c.Ref, where)
	}
	return c.String()
}

// Short is the condition in a few words, for a list: "PR #1257 merged", "club #12 review", "job backup".
func (c Cond) Short() string {
	where := ""
	if c.Project != "" {
		where = c.Project + " "
	}
	switch c.Kind {
	case CondPR:
		return fmt.Sprintf("%sPR #%s %s", where, c.Ref, c.On)
	case CondIssue:
		return fmt.Sprintf("%s%s %s", where, issueRef(c.Ref), c.On)
	}
	return fmt.Sprintf("%sjob %s", where, c.Ref)
}

// issueRef is how an issue key reads: #12 for a number, PROJ-12 as it is.
func issueRef(key string) string {
	if _, err := strconv.Atoi(key); err == nil {
		return "#" + key
	}
	return key
}

// Fact is something that happened which a wait may be on: a PR merged or closed, an issue closed or its turn
// ended in a state, a job's run ended.
type Fact struct {
	Kind    CondKind
	Project string
	Ref     string
	// On is merged or closed for a PR, closed or the agent state for an issue, the run's outcome for a job.
	On string
}

// Meets reports whether the fact ends a wait on the condition, for a waiting issue in project own. A PR that
// ends the other way ends the wait too, since what was waited for can't happen now.
func (c Cond) Meets(f Fact, own string) bool {
	if c.Kind != f.Kind || !strings.EqualFold(c.In(own), f.Project) || !strings.EqualFold(c.Ref, f.Ref) {
		return false
	}
	switch c.Kind {
	case CondPR, CondJob:
		return true
	}
	return c.On == f.On
}

// Describe is what happened, in words, for the note that wakes the waiting issue.
func (f Fact) Describe(c Cond) string {
	where := ""
	if c.Project != "" {
		where = " in " + c.Project
	}
	switch f.Kind {
	case CondPR:
		if f.On == string(PRClosed) && c.On == string(PRMerged) {
			return fmt.Sprintf("PR #%s%s was closed without merging, so it won't merge", f.Ref, where)
		}
		return fmt.Sprintf("PR #%s%s was %s", f.Ref, where, f.On)
	case CondIssue:
		if f.On == "closed" {
			return fmt.Sprintf("%s%s was closed", issueRef(f.Ref), where)
		}
		return fmt.Sprintf("the agent on %s%s ended a turn in %s", issueRef(f.Ref), where, f.On)
	case CondJob:
		return fmt.Sprintf("a run of the %s job%s ended: %s", f.Ref, where, f.On)
	}
	return string(f.Kind) + " " + f.Ref
}

// Hold is what an issue is held for: any one of its conditions being met, or its time coming, whichever is
// first. Cancel is a hold that drops the one before it.
type Hold struct {
	Conds  []Cond
	Until  *time.Time
	Cancel bool
}

// Spec is the hold in the words a marker carries: `runAt: 2026-10-25T09:00Z` for a time alone (as before
// waits), else `waitFor: pr:1257:merged issue:12:review; until 2026-10-25T09:00Z`, or `waitFor: cancel`.
func (h Hold) Spec() string {
	if h.Cancel {
		return "waitFor: cancel"
	}
	if len(h.Conds) == 0 && h.Until != nil {
		return "runAt: " + h.Until.UTC().Format("2006-01-02T15:04Z")
	}
	var conds []string
	for _, c := range h.Conds {
		conds = append(conds, c.String())
	}
	s := "waitFor: " + strings.Join(conds, " ")
	if h.Until != nil {
		s += "; until " + h.Until.UTC().Format("2006-01-02T15:04Z")
	}
	return s
}

// Describe is the hold in words, for the log and the agent: "PR #1257 to merge, or 25 Oct 09:00 UTC".
func (h Hold) Describe() string {
	if h.Cancel {
		return "nothing (cancelled)"
	}
	var parts []string
	for _, c := range h.Conds {
		parts = append(parts, c.Describe())
	}
	if h.Until != nil {
		parts = append(parts, h.Until.UTC().Format("2 Jan 2006 15:04 UTC"))
	}
	return strings.Join(parts, ", or ")
}

// holdRe finds a hold marker: `<!-- runAt: … -->` or `<!-- waitFor: … -->` (hidden in Markdown hosts), or
// `[runAt: …]` or `[waitFor: …]` (for trackers whose comments can't hide anything).
var holdRe = regexp.MustCompile(`(?:<!--\s*(runAt|waitFor)[:(\s]\s*([^>]*?)\s*\)?\s*-->|\[(runAt|waitFor):\s*([^\]]*?)\s*\])`)

// HoldIn returns the last valid hold marker in a body.
func HoldIn(body string) (Hold, bool) {
	var found Hold
	ok := false
	for _, m := range holdRe.FindAllStringSubmatch(body, -1) {
		kind, v := m[1], m[2]
		if kind == "" {
			kind, v = m[3], m[4]
		}
		if h, err := parseHold(kind, v); err == nil {
			found, ok = h, true
		}
	}
	return found, ok
}

func parseHold(kind, v string) (Hold, error) {
	v = strings.TrimSpace(v)
	if kind == "runAt" {
		t, err := ParseTime(v)
		if err != nil {
			return Hold{}, err
		}
		return Hold{Until: &t}, nil
	}
	if strings.EqualFold(v, "cancel") {
		return Hold{Cancel: true}, nil
	}
	var h Hold
	conds, until, _ := strings.Cut(v, ";")
	for _, f := range strings.Fields(conds) {
		c, err := ParseCond(f)
		if err != nil {
			return Hold{}, err
		}
		h.Conds = append(h.Conds, c)
	}
	if until = strings.TrimSpace(until); until != "" {
		t, err := ParseTime(strings.TrimSpace(strings.TrimPrefix(until, "until")))
		if err != nil {
			return Hold{}, err
		}
		h.Until = &t
	}
	if len(h.Conds) == 0 && h.Until == nil {
		return Hold{}, fmt.Errorf("an empty wait")
	}
	return h, nil
}

// RunAtIn returns the time of the last valid hold marker in a body that has one.
func RunAtIn(body string) (time.Time, bool) {
	h, ok := HoldIn(body)
	if !ok || h.Until == nil {
		return time.Time{}, false
	}
	return *h.Until, true
}
