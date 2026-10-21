// Package linear is the Linear adapter: a tracker over one or more Linear teams' issues, through Linear's
// GraphQL API and its webhooks. One team's issues usually feed several repositories, so a Linear tracker is
// most often a source with routes (on labels, project, cycle, priority, assignee) to the projects it feeds.
package linear

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/core"
)

// stateFields map each state the supervisor moves an issue to onto the binding field naming its workflow state.
var stateFields = map[string]string{
	"claimed":  "stateClaimed",
	"building": "stateBuilding",
	"review":   "stateReview",
	"help":     "stateHelp",
	"done":     "stateDone",
	"finished": "stateFinished",
}

func init() {
	state := func(key, label string) adapter.Field {
		return adapter.Field{Key: key, Label: label, Type: adapter.Text, Help: "The workflow state to move the issue to, by name; empty leaves it alone."}
	}
	adapter.Register(adapter.Spec{
		Type:        "linear",
		Title:       "Linear",
		Description: "Issues in one or more Linear teams.",
		Roles:       []adapter.Role{adapter.RoleTracker},
		Fields: []adapter.Field{
			{Key: "token", Label: "API key", Type: adapter.Secret, Required: true,
				Help: "A personal API key (Settings → Security & access), or an OAuth token."},
			{Key: "agentAccount", Label: "Agent account", Type: adapter.Text,
				Help: "A user's name, email or id whose comments always count as an agent's; leave empty when agents post as you."},
			{Key: "apiUrl", Label: "API URL", Type: adapter.Text, Default: "https://api.linear.app/graphql"},
		},
		Binding: map[adapter.Role][]adapter.Field{
			adapter.RoleTracker: {
				{Key: "team", Label: "Team key", Type: adapter.Text, Required: true, Placeholder: "ENG",
					Help: "Several, comma-separated, for a source that routes them to projects: ENG, OPS."},
				{Key: "states", Label: "States to claim from", Type: adapter.Text, Default: "unstarted, started",
					Help: "Workflow state types (triage, backlog, unstarted, started) or names; issues elsewhere wait."},
				state("stateClaimed", "State when claimed"),
				state("stateBuilding", "State while building"),
				state("stateReview", "State when up for review"),
				state("stateHelp", "State when it needs help"),
				state("stateDone", "State when done"),
				state("stateFinished", "State when finished"),
			},
		},
		New: open,
	})
}

// Conn is a Linear workspace and the account used on it.
type Conn struct {
	api          *adapter.JSONClient
	url          string
	agentAccount string
	dryRun       bool
	log          func(string, ...any)
}

func open(v adapter.Values, env adapter.Env) (adapter.Connection, error) {
	token, err := env.Secret(v.Get("token", ""))
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, fmt.Errorf("no API key")
	}
	c := &Conn{url: v.Get("apiUrl", "https://api.linear.app/graphql"), agentAccount: v.Get("agentAccount", ""), dryRun: env.DryRun, log: env.Log}
	if c.log == nil {
		c.log = func(string, ...any) {}
	}
	c.api = &adapter.JSONClient{
		HTTP: env.HTTP,
		Auth: func(r *http.Request) error {
			// OAuth tokens go as bearer tokens; personal API keys bare.
			if strings.HasPrefix(token, "lin_oauth_") {
				r.Header.Set("Authorization", "Bearer "+token)
			} else {
				r.Header.Set("Authorization", token)
			}
			return nil
		},
	}
	return c, nil
}

// gqlError is a GraphQL answer with errors.
type gqlError struct{ messages []string }

func (e *gqlError) Error() string { return "linear: " + strings.Join(e.messages, "; ") }

// query runs a GraphQL query or mutation and decodes its data into out.
func (c *Conn) query(ctx context.Context, q string, vars map[string]any, out any) error {
	var res struct {
		Data   json.RawMessage `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if _, err := c.api.Do(ctx, "POST", c.url, map[string]any{"query": q, "variables": vars}, &res); err != nil {
		// Linear answers a bad query with a 400 and the errors in the body.
		var apiErr *adapter.APIError
		if !errors.As(err, &apiErr) || json.Unmarshal([]byte(apiErr.Body), &res) != nil || len(res.Errors) == 0 {
			return err
		}
	}
	if len(res.Errors) > 0 {
		e := &gqlError{}
		for _, m := range res.Errors {
			e.messages = append(e.messages, m.Message)
		}
		return e
	}
	if out == nil || len(res.Data) == 0 {
		return nil
	}
	return json.Unmarshal(res.Data, out)
}

// Check reads the signed-in account.
func (c *Conn) Check(ctx context.Context) (string, error) {
	var r struct {
		Viewer struct {
			Name  string `json:"name"`
			Email string `json:"email"`
		} `json:"viewer"`
	}
	if err := c.query(ctx, `query { viewer { name email } }`, nil, &r); err != nil {
		return "", err
	}
	return "signed in as " + r.Viewer.Name, nil
}

var teamKeyRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// Tracker binds the workspace to one team's issues, or several teams' for a source that routes them.
func (c *Conn) Tracker(b adapter.Values, _ adapter.ProjectInfo) (adapter.Tracker, error) {
	var teams, quoted []string
	for _, k := range strings.Split(b.Get("team", ""), ",") {
		k = strings.ToUpper(strings.TrimSpace(k))
		if k == "" {
			continue
		}
		if !teamKeyRe.MatchString(k) {
			return nil, fmt.Errorf("team key %q isn't a Linear team key", k)
		}
		teams = append(teams, k)
		quoted = append(quoted, regexp.QuoteMeta(k))
	}
	if len(teams) == 0 {
		return nil, fmt.Errorf("no team key")
	}
	t := &Tracker{c: c, teams: teams, statuses: map[string]string{}}
	for _, s := range strings.Split(b.Get("states", "unstarted, started"), ",") {
		if s = strings.ToLower(strings.TrimSpace(s)); s != "" {
			t.claimFrom = append(t.claimFrom, s)
		}
	}
	for state, field := range stateFields {
		if s := strings.TrimSpace(b.Get(field, "")); s != "" {
			t.statuses[state] = s
		}
	}
	t.refRe = regexp.MustCompile(`(?i)\b(?:` + strings.Join(quoted, "|") + `)-\d+\b`)
	return t, nil
}

// Tracker is one or more Linear teams' issues.
type Tracker struct {
	c     *Conn
	teams []string
	// claimFrom are the state types or names whose issues are candidates.
	claimFrom []string
	statuses  map[string]string
	refRe     *regexp.Regexp

	mu     sync.Mutex
	labels map[string]string // lowercased name → id
	states map[string]rawState
}

var (
	_ adapter.Tracker      = (*Tracker)(nil)
	_ adapter.Hooked       = (*Tracker)(nil)
	_ adapter.Transitioner = (*Tracker)(nil)
	_ adapter.IssueOpener  = (*Tracker)(nil)
)

func (t *Tracker) Describe() string { return "linear " + strings.Join(t.teams, ",") }

// owns reports whether an issue identifier is in one of the tracker's teams.
func (t *Tracker) owns(key string) bool {
	for _, team := range t.teams {
		if strings.HasPrefix(strings.ToUpper(key), team+"-") {
			return true
		}
	}
	return false
}

// IssueURL is the issue's page; Linear redirects /issue/<key> to the workspace's own.
func (t *Tracker) IssueURL(key string) string { return "https://linear.app/issue/" + key }

func (t *Tracker) RefsIn(text string) []string {
	seen := map[string]bool{}
	var out []string
	for _, m := range t.refRe.FindAllString(text, -1) {
		k := strings.ToUpper(m)
		if !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	return out
}

// RefFor uses one of Linear's magic words, so the PR is linked and closes the issue when it merges.
func (t *Tracker) RefFor(key string) string { return "Closes " + key }

func (t *Tracker) RunAtMarker(at time.Time) string {
	return "[runAt: " + at.UTC().Format("2006-01-02T15:04Z") + "]"
}

// ---- issues ---------------------------------------------------------------------------------------------

type rawUser struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Email       string `json:"email"`
}

type rawState struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

type rawIssue struct {
	ID            string    `json:"id"`
	Identifier    string    `json:"identifier"`
	Title         string    `json:"title"`
	Description   string    `json:"description"`
	URL           string    `json:"url"`
	CreatedAt     time.Time `json:"createdAt"`
	UpdatedAt     time.Time `json:"updatedAt"`
	PriorityLabel string    `json:"priorityLabel"`
	Estimate      *float64  `json:"estimate"`
	State         *rawState `json:"state"`
	Team          *struct {
		Key  string `json:"key"`
		Name string `json:"name"`
	} `json:"team"`
	Project *struct {
		Name string `json:"name"`
	} `json:"project"`
	Cycle *struct {
		Number int    `json:"number"`
		Name   string `json:"name"`
	} `json:"cycle"`
	Assignee *rawUser `json:"assignee"`
	Creator  *rawUser `json:"creator"`
	Labels   struct {
		Nodes []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	Parent *struct {
		Identifier string `json:"identifier"`
	} `json:"parent"`
}

const issueFields = `id identifier title description url createdAt updatedAt priorityLabel estimate
	state { id name type } team { key name } project { name } cycle { number name }
	assignee { id name displayName email } creator { id name displayName email }
	labels(first: 100) { nodes { id name } } parent { identifier }`

// closedTypes are the state types an issue is closed in.
var closedTypes = []string{"completed", "canceled", "duplicate"}

func closedType(typ string) bool {
	for _, c := range closedTypes {
		if typ == c {
			return true
		}
	}
	return false
}

// issue is the issue with the properties routes can match: team, project, state, statetype, priority,
// estimate, cycle, assignee, creator and parent.
func (r rawIssue) issue() core.Issue {
	i := core.Issue{
		Key:       r.Identifier,
		Title:     r.Title,
		Body:      r.Description,
		URL:       r.URL,
		Open:      r.State == nil || !closedType(r.State.Type),
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}
	for _, l := range r.Labels.Nodes {
		i.Labels = append(i.Labels, l.Name)
	}
	if r.Creator != nil {
		i.Author = first(r.Creator.DisplayName, r.Creator.Name)
		i.SetAttr("creator", r.Creator.Name, r.Creator.DisplayName, r.Creator.Email)
	}
	if r.Team != nil {
		i.SetAttr("team", r.Team.Key, r.Team.Name)
	}
	if r.Project != nil {
		i.SetAttr("project", r.Project.Name)
	}
	if r.State != nil {
		i.SetAttr("state", r.State.Name)
		i.SetAttr("statetype", r.State.Type)
	}
	i.SetAttr("priority", r.PriorityLabel)
	if r.Estimate != nil {
		i.SetAttr("estimate", strconv.FormatFloat(*r.Estimate, 'f', -1, 64))
	}
	if r.Cycle != nil {
		i.SetAttr("cycle", strconv.Itoa(r.Cycle.Number), r.Cycle.Name)
	}
	if r.Assignee != nil {
		i.SetAttr("assignee", r.Assignee.Name, r.Assignee.DisplayName, r.Assignee.Email)
	}
	if r.Parent != nil {
		i.SetAttr("parent", r.Parent.Identifier)
	}
	return i
}

func first(xs ...string) string {
	for _, x := range xs {
		if x != "" {
			return x
		}
	}
	return ""
}

// claimable reports whether an issue's state is one the tracker claims from.
func (t *Tracker) claimable(s *rawState) bool {
	if s == nil {
		return false
	}
	for _, c := range t.claimFrom {
		if c == strings.ToLower(s.Type) || c == strings.ToLower(s.Name) {
			return true
		}
	}
	return false
}

// Candidates are the teams' open issues, oldest first. Those outside the states to claim from are Waiting:
// listed, so the ones agents hold aren't taken for closed, but never claimed on their own.
func (t *Tracker) Candidates(ctx context.Context) ([]core.Issue, error) {
	q := `query($teams: [String!], $after: String) {
		issues(first: 100, after: $after, orderBy: createdAt,
			filter: { team: { key: { in: $teams } }, state: { type: { nin: ["completed", "canceled", "duplicate"] } } }) {
			nodes { ` + issueFields + ` }
			pageInfo { hasNextPage endCursor }
		}
	}`
	var out []core.Issue
	var after any
	for n := 0; n < 50; n++ {
		var r struct {
			Issues struct {
				Nodes    []rawIssue `json:"nodes"`
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"issues"`
		}
		if err := t.c.query(ctx, q, map[string]any{"teams": t.teams, "after": after}, &r); err != nil {
			return nil, err
		}
		for _, raw := range r.Issues.Nodes {
			out = append(out, t.issue(raw))
		}
		if !r.Issues.PageInfo.HasNextPage || r.Issues.PageInfo.EndCursor == "" {
			break
		}
		after = r.Issues.PageInfo.EndCursor
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

// issue is the raw issue, Waiting when its state isn't one to claim from.
func (t *Tracker) issue(r rawIssue) core.Issue {
	i := r.issue()
	i.Waiting = i.Open && !t.claimable(r.State)
	return i
}

func (t *Tracker) raw(ctx context.Context, key string) (rawIssue, error) {
	var r struct {
		Issue rawIssue `json:"issue"`
	}
	if err := t.c.query(ctx, `query($id: String!) { issue(id: $id) { `+issueFields+` } }`, map[string]any{"id": key}, &r); err != nil {
		return rawIssue{}, err
	}
	if r.Issue.ID == "" {
		return rawIssue{}, fmt.Errorf("linear: no issue %s", key)
	}
	return r.Issue, nil
}

func (t *Tracker) Issue(ctx context.Context, key string) (core.Issue, error) {
	r, err := t.raw(ctx, key)
	if err != nil {
		return core.Issue{}, err
	}
	return t.issue(r), nil
}

// ReopenedAt is when the issue last left a closed state.
func (t *Tracker) ReopenedAt(ctx context.Context, key string) (time.Time, error) {
	var r struct {
		Issue struct {
			History struct {
				Nodes []struct {
					CreatedAt time.Time `json:"createdAt"`
					FromState *rawState `json:"fromState"`
					ToState   *rawState `json:"toState"`
				} `json:"nodes"`
			} `json:"history"`
		} `json:"issue"`
	}
	q := `query($id: String!) { issue(id: $id) { history(first: 100) { nodes { createdAt fromState { type } toState { type } } } } }`
	if err := t.c.query(ctx, q, map[string]any{"id": key}, &r); err != nil {
		return time.Time{}, err
	}
	var at time.Time
	for _, h := range r.Issue.History.Nodes {
		if h.FromState != nil && h.ToState != nil && closedType(h.FromState.Type) && !closedType(h.ToState.Type) && h.CreatedAt.After(at) {
			at = h.CreatedAt
		}
	}
	return at, nil
}

// ---- comments -------------------------------------------------------------------------------------------

type rawComment struct {
	ID        string    `json:"id"`
	Body      string    `json:"body"`
	URL       string    `json:"url"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	User      *rawUser  `json:"user"`
	BotActor  *struct {
		Name string `json:"name"`
	} `json:"botActor"`
	Issue *struct {
		Identifier string `json:"identifier"`
	} `json:"issue"`
}

const commentFields = `id body url createdAt updatedAt user { id name displayName email } botActor { name } issue { identifier }`

// agentRe finds the footer an agent's comment carries. Linear keeps Markdown but drops HTML comments, so the
// mark is a line people can read.
var agentRe = regexp.MustCompile(`(?m)^[\W_]*Posted by an open-supervisor agent(?: working on ([A-Za-z][A-Za-z0-9_]*-\d+))?[\W_]*$`)

func withMark(body string, mark core.AgentMark) string {
	footer := "Posted by an open-supervisor agent"
	if mark.From != "" {
		footer += " working on " + mark.From
	}
	return strings.TrimRight(body, "\n") + "\n\n*" + footer + "*\n"
}

func (t *Tracker) comment(r rawComment) core.Comment {
	at := r.CreatedAt
	c := core.Comment{ID: r.ID, Kind: core.OnIssue, Body: r.Body, URL: r.URL, At: at}
	if r.User != nil {
		c.Author = first(r.User.DisplayName, r.User.Name)
		if a := t.c.agentAccount; a != "" && (a == r.User.ID || strings.EqualFold(a, r.User.Name) || strings.EqualFold(a, r.User.DisplayName) || strings.EqualFold(a, r.User.Email)) {
			c.Agent = &core.AgentMark{}
		}
	}
	if r.BotActor != nil && r.User == nil {
		c.Author = r.BotActor.Name
		c.Bot = true
	}
	if m := agentRe.FindStringSubmatch(r.Body); m != nil {
		c.Agent = &core.AgentMark{From: strings.ToUpper(m[1])}
	}
	return c
}

// Comments on the issue newer than since, oldest first.
func (t *Tracker) Comments(ctx context.Context, key string, since time.Time) ([]core.Comment, error) {
	q := `query($id: String!, $after: String) { issue(id: $id) {
		comments(first: 100, after: $after, orderBy: createdAt) { nodes { ` + commentFields + ` } pageInfo { hasNextPage endCursor } } } }`
	var out []core.Comment
	var after any
	for n := 0; n < 50; n++ {
		var r struct {
			Issue struct {
				Comments struct {
					Nodes    []rawComment `json:"nodes"`
					PageInfo struct {
						HasNextPage bool   `json:"hasNextPage"`
						EndCursor   string `json:"endCursor"`
					} `json:"pageInfo"`
				} `json:"comments"`
			} `json:"issue"`
		}
		if err := t.c.query(ctx, q, map[string]any{"id": key, "after": after}, &r); err != nil {
			return nil, err
		}
		for _, raw := range r.Issue.Comments.Nodes {
			if c := t.comment(raw); since.IsZero() || c.At.After(since) {
				out = append(out, c)
			}
		}
		if !r.Issue.Comments.PageInfo.HasNextPage || r.Issue.Comments.PageInfo.EndCursor == "" {
			break
		}
		after = r.Issue.Comments.PageInfo.EndCursor
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out, nil
}

// Comment posts on the issue with the footer that marks it as an agent's.
func (t *Tracker) Comment(ctx context.Context, key, body string, mark core.AgentMark) (string, error) {
	if t.c.dryRun {
		return "", nil
	}
	issue, err := t.raw(ctx, key)
	if err != nil {
		return "", err
	}
	var r struct {
		CommentCreate struct {
			Success bool `json:"success"`
			Comment struct {
				URL string `json:"url"`
			} `json:"comment"`
		} `json:"commentCreate"`
	}
	q := `mutation($input: CommentCreateInput!) { commentCreate(input: $input) { success comment { url } } }`
	if err := t.c.query(ctx, q, map[string]any{"input": map[string]any{"issueId": issue.ID, "body": withMark(body, mark)}}, &r); err != nil {
		return "", err
	}
	return r.CommentCreate.Comment.URL, nil
}

// OpenIssue opens an issue in the tracker's first team.
func (t *Tracker) OpenIssue(ctx context.Context, title, body string) (core.Issue, error) {
	if t.c.dryRun {
		return core.Issue{}, nil
	}
	var teams struct {
		Teams struct {
			Nodes []struct {
				ID string `json:"id"`
			} `json:"nodes"`
		} `json:"teams"`
	}
	if err := t.c.query(ctx, `query($key: String!) { teams(filter: { key: { eq: $key } }) { nodes { id } } }`, map[string]any{"key": t.teams[0]}, &teams); err != nil {
		return core.Issue{}, err
	}
	if len(teams.Teams.Nodes) == 0 {
		return core.Issue{}, fmt.Errorf("linear: no team %s", t.teams[0])
	}
	var r struct {
		IssueCreate struct {
			Issue rawIssue `json:"issue"`
		} `json:"issueCreate"`
	}
	q := `mutation($input: IssueCreateInput!) { issueCreate(input: $input) { issue { ` + issueFields + ` } } }`
	input := map[string]any{"teamId": teams.Teams.Nodes[0].ID, "title": title, "description": body}
	if err := t.c.query(ctx, q, map[string]any{"input": input}, &r); err != nil {
		return core.Issue{}, err
	}
	return t.issue(r.IssueCreate.Issue), nil
}

// ---- labels and workflow --------------------------------------------------------------------------------

// labelIDs reads the workspace's labels and the teams', by lowercased name.
func (t *Tracker) labelIDs(ctx context.Context, fresh bool) (map[string]string, error) {
	t.mu.Lock()
	cached := t.labels
	t.mu.Unlock()
	if cached != nil && !fresh {
		return cached, nil
	}
	q := `query($after: String) { issueLabels(first: 250, after: $after) { nodes { id name team { key } } pageInfo { hasNextPage endCursor } } }`
	out := map[string]string{}
	var after any
	for n := 0; n < 20; n++ {
		var r struct {
			IssueLabels struct {
				Nodes []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
					Team *struct {
						Key string `json:"key"`
					} `json:"team"`
				} `json:"nodes"`
				PageInfo struct {
					HasNextPage bool   `json:"hasNextPage"`
					EndCursor   string `json:"endCursor"`
				} `json:"pageInfo"`
			} `json:"issueLabels"`
		}
		if err := t.c.query(ctx, q, map[string]any{"after": after}, &r); err != nil {
			return nil, err
		}
		for _, l := range r.IssueLabels.Nodes {
			name := strings.ToLower(l.Name)
			// A workspace label serves every team; a team's own only that team.
			if l.Team == nil {
				out[name] = l.ID
			} else if _, taken := out[name]; !taken && t.ownsTeam(l.Team.Key) {
				out[name] = l.ID
			}
		}
		if !r.IssueLabels.PageInfo.HasNextPage {
			break
		}
		after = r.IssueLabels.PageInfo.EndCursor
	}
	t.mu.Lock()
	t.labels = out
	t.mu.Unlock()
	return out, nil
}

func (t *Tracker) ownsTeam(key string) bool {
	for _, team := range t.teams {
		if strings.EqualFold(team, key) {
			return true
		}
	}
	return false
}

// EditLabels adds and removes labels by name; a label the workspace lacks is skipped.
func (t *Tracker) EditLabels(ctx context.Context, key string, add, remove []string) error {
	if t.c.dryRun {
		return nil
	}
	issue, err := t.raw(ctx, key)
	if err != nil {
		return err
	}
	ids, err := t.labelIDs(ctx, false)
	if err != nil {
		return err
	}
	for _, name := range add {
		if _, ok := ids[strings.ToLower(name)]; !ok {
			if ids, err = t.labelIDs(ctx, true); err != nil {
				return err
			}
			break
		}
	}
	set := map[string]bool{}
	var order []string
	for _, l := range issue.Labels.Nodes {
		set[l.ID] = true
		order = append(order, l.ID)
	}
	changed := false
	for _, name := range remove {
		if id, ok := ids[strings.ToLower(name)]; ok && set[id] {
			delete(set, id)
			changed = true
		}
		for _, l := range issue.Labels.Nodes {
			if strings.EqualFold(l.Name, name) && set[l.ID] {
				delete(set, l.ID)
				changed = true
			}
		}
	}
	for _, name := range add {
		id, ok := ids[strings.ToLower(name)]
		if !ok {
			t.c.log("linear: no label %q to put on %s", name, key)
			continue
		}
		if !set[id] {
			set[id] = true
			order = append(order, id)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	labelIDs := []string{}
	for _, id := range order {
		if set[id] {
			labelIDs = append(labelIDs, id)
		}
	}
	q := `mutation($id: String!, $input: IssueUpdateInput!) { issueUpdate(id: $id, input: $input) { success } }`
	return t.c.query(ctx, q, map[string]any{"id": issue.ID, "input": map[string]any{"labelIds": labelIDs}}, nil)
}

// EnsureLabels creates the workspace labels the supervisor sets that Linear lacks.
func (t *Tracker) EnsureLabels(ctx context.Context, labels []adapter.Label) error {
	if t.c.dryRun {
		return nil
	}
	ids, err := t.labelIDs(ctx, true)
	if err != nil {
		return err
	}
	q := `mutation($input: IssueLabelCreateInput!) { issueLabelCreate(input: $input) { success } }`
	created := false
	for _, l := range labels {
		if l.Name == "" {
			continue
		}
		if _, ok := ids[strings.ToLower(l.Name)]; ok {
			continue
		}
		input := map[string]any{"name": l.Name, "description": l.Description}
		if l.Color != "" {
			input["color"] = "#" + strings.TrimPrefix(l.Color, "#")
		}
		if err := t.c.query(ctx, q, map[string]any{"input": input}, nil); err != nil {
			return fmt.Errorf("creating label %s: %w", l.Name, err)
		}
		created = true
	}
	if created {
		_, err = t.labelIDs(ctx, true)
	}
	return err
}

// workflowStates reads the teams' workflow states, by lowercased "TEAM/name".
func (t *Tracker) workflowStates(ctx context.Context) (map[string]rawState, error) {
	t.mu.Lock()
	cached := t.states
	t.mu.Unlock()
	if cached != nil {
		return cached, nil
	}
	var r struct {
		WorkflowStates struct {
			Nodes []struct {
				rawState
				Team struct {
					Key string `json:"key"`
				} `json:"team"`
			} `json:"nodes"`
		} `json:"workflowStates"`
	}
	q := `query($teams: [String!]) { workflowStates(first: 250, filter: { team: { key: { in: $teams } } }) { nodes { id name type team { key } } } }`
	if err := t.c.query(ctx, q, map[string]any{"teams": t.teams}, &r); err != nil {
		return nil, err
	}
	out := map[string]rawState{}
	for _, s := range r.WorkflowStates.Nodes {
		out[strings.ToLower(s.Team.Key+"/"+s.Name)] = s.rawState
		out[strings.ToLower("id/"+s.ID)] = s.rawState
	}
	t.mu.Lock()
	t.states = out
	t.mu.Unlock()
	return out, nil
}

// Transition moves the issue to the workflow state named for the supervisor's state, if one is.
func (t *Tracker) Transition(ctx context.Context, key, state string) error {
	want := t.statuses[state]
	if want == "" || t.c.dryRun {
		return nil
	}
	team, _, _ := strings.Cut(strings.ToUpper(key), "-")
	states, err := t.workflowStates(ctx)
	if err != nil {
		return err
	}
	target, ok := states[strings.ToLower(team+"/"+want)]
	if !ok {
		return fmt.Errorf("team %s has no workflow state %q", team, want)
	}
	issue, err := t.raw(ctx, key)
	if err != nil {
		return err
	}
	if issue.State != nil && issue.State.ID == target.ID {
		return nil
	}
	q := `mutation($id: String!, $input: IssueUpdateInput!) { issueUpdate(id: $id, input: $input) { success } }`
	return t.c.query(ctx, q, map[string]any{"id": issue.ID, "input": map[string]any{"stateId": target.ID}}, nil)
}

// ---- webhooks -------------------------------------------------------------------------------------------

func (t *Tracker) HookTarget() string { return "linear:" + strings.Join(t.teams, ",") }

var resourceTypes = []string{"Issue", "Comment"}

// EnsureWebhook points the supervisor's hook at url: one team's hook, or every public team's for several (the
// deliveries for other teams are dropped). Creating webhooks needs a workspace admin.
func (t *Tracker) EnsureWebhook(ctx context.Context, url, secret, existing string) (string, error) {
	if t.c.dryRun {
		if existing == "" {
			existing = "dry-run"
		}
		return existing, nil
	}
	if existing != "" {
		var r struct {
			WebhookUpdate struct {
				Success bool `json:"success"`
			} `json:"webhookUpdate"`
		}
		q := `mutation($id: String!, $input: WebhookUpdateInput!) { webhookUpdate(id: $id, input: $input) { success } }`
		err := t.c.query(ctx, q, map[string]any{"id": existing, "input": map[string]any{"url": url, "secret": secret, "resourceTypes": resourceTypes, "enabled": true}}, &r)
		if err == nil && r.WebhookUpdate.Success {
			return existing, nil
		}
		// Only a hook that is gone is made again; any other failure would leave the old one behind.
		if err != nil && !strings.Contains(strings.ToLower(err.Error()), "not found") {
			return "", err
		}
	}
	input := map[string]any{"url": url, "secret": secret, "resourceTypes": resourceTypes, "label": "open-supervisor " + strings.Join(t.teams, ","), "enabled": true}
	if len(t.teams) == 1 {
		input["teamId"] = t.teams[0]
	} else {
		input["allPublicTeams"] = true
	}
	var r struct {
		WebhookCreate struct {
			Success bool `json:"success"`
			Webhook struct {
				ID string `json:"id"`
			} `json:"webhook"`
		} `json:"webhookCreate"`
	}
	q := `mutation($input: WebhookCreateInput!) { webhookCreate(input: $input) { success webhook { id } } }`
	if err := t.c.query(ctx, q, map[string]any{"input": input}, &r); err != nil {
		return "", fmt.Errorf("%w: a workspace admin can add the webhook by hand (Settings → API → Webhooks) for %s with Issues and Comments and the secret in the supervisor's state dir (webhook-secret), or rely on the reconcile", err, url)
	}
	if r.WebhookCreate.Webhook.ID == "" {
		return "", fmt.Errorf("linear made the webhook but didn't say its id")
	}
	return r.WebhookCreate.Webhook.ID, nil
}

// maxAge is how old a delivery may be before it is taken for a replay.
const maxAge = 10 * time.Minute

// ParseWebhook checks the delivery's signature and age, and defers the rest: Linear wants an answer within
// five seconds, and what an issue delivery means needs the issue read back from the API (so it routes just
// as the reconcile would route it).
func (t *Tracker) ParseWebhook(h http.Header, body []byte, secret string) ([]core.Signal, error) {
	if !verify(secret, body, h.Get("Linear-Signature")) {
		return nil, fmt.Errorf("linear: a delivery with a bad signature")
	}
	var p delivery
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("linear: a delivery that isn't JSON: %w", err)
	}
	if p.WebhookTimestamp > 0 && time.Since(time.UnixMilli(p.WebhookTimestamp)) > maxAge {
		return nil, fmt.Errorf("linear: a delivery from %s, too old to trust", time.UnixMilli(p.WebhookTimestamp).UTC().Format(time.RFC3339))
	}
	switch p.Type {
	case "Issue":
		var d struct {
			Identifier string `json:"identifier"`
		}
		if err := json.Unmarshal(p.Data, &d); err != nil || d.Identifier == "" || !t.owns(d.Identifier) {
			return nil, nil
		}
		if p.Action == "remove" {
			return []core.Signal{{Type: core.SigIssueClosed, Key: d.Identifier}}, nil
		}
		return []core.Signal{{Type: core.SigDeferred, Key: d.Identifier, Resolve: func(ctx context.Context) ([]core.Signal, error) {
			return t.issueSignals(ctx, p, d.Identifier)
		}}}, nil
	case "Comment":
		var d struct {
			ID string `json:"id"`
		}
		if p.Action != "create" || json.Unmarshal(p.Data, &d) != nil || d.ID == "" {
			return nil, nil
		}
		return []core.Signal{{Type: core.SigDeferred, Resolve: func(ctx context.Context) ([]core.Signal, error) {
			return t.commentSignals(ctx, d.ID)
		}}}, nil
	}
	return nil, nil
}

type delivery struct {
	Action           string          `json:"action"`
	Type             string          `json:"type"`
	Data             json.RawMessage `json:"data"`
	UpdatedFrom      map[string]any  `json:"updatedFrom"`
	WebhookTimestamp int64           `json:"webhookTimestamp"`
}

// issueSignals reads an issue a delivery was about and says what happened to it.
func (t *Tracker) issueSignals(ctx context.Context, p delivery, key string) ([]core.Signal, error) {
	raw, err := t.raw(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("linear: couldn't read %s: %w", key, err)
	}
	issue := t.issue(raw)
	if !issue.Open {
		if _, moved := p.UpdatedFrom["stateId"]; moved && p.Action == "update" {
			return []core.Signal{{Type: core.SigIssueClosed, Key: issue.Key}}, nil
		}
		return nil, nil
	}
	if from, _ := p.UpdatedFrom["stateId"].(string); p.Action == "update" && from != "" && t.wasClosed(ctx, from) {
		// A Waiting issue goes back to the agent that holds it, but isn't claimed afresh.
		return []core.Signal{{Type: core.SigIssueReopened, Key: issue.Key, Issue: &issue}}, nil
	}
	return []core.Signal{{Type: core.SigIssueChanged, Key: issue.Key, Issue: &issue}}, nil
}

// commentSignals reads a comment a delivery was about.
func (t *Tracker) commentSignals(ctx context.Context, id string) ([]core.Signal, error) {
	var r struct {
		Comment rawComment `json:"comment"`
	}
	if err := t.c.query(ctx, `query($id: String!) { comment(id: $id) { `+commentFields+` } }`, map[string]any{"id": id}, &r); err != nil {
		return nil, fmt.Errorf("linear: couldn't read comment %s: %w", id, err)
	}
	if r.Comment.Issue == nil || !t.owns(r.Comment.Issue.Identifier) {
		return nil, nil
	}
	c := t.comment(r.Comment)
	return []core.Signal{{Type: core.SigIssueComment, Key: r.Comment.Issue.Identifier, Comment: &c}}, nil
}

// wasClosed reports whether a workflow state is a closed one.
func (t *Tracker) wasClosed(ctx context.Context, stateID string) bool {
	if states, err := t.workflowStates(ctx); err == nil {
		if s, ok := states["id/"+strings.ToLower(stateID)]; ok {
			return closedType(s.Type)
		}
	}
	var r struct {
		WorkflowState rawState `json:"workflowState"`
	}
	if err := t.c.query(ctx, `query($id: String!) { workflowState(id: $id) { id name type } }`, map[string]any{"id": stateID}, &r); err != nil {
		return false
	}
	return closedType(r.WorkflowState.Type)
}

// verify checks Linear's signature: the hex HMAC-SHA256 of the body, with no prefix.
func verify(secret string, body []byte, signature string) bool {
	given, err := hex.DecodeString(strings.TrimSpace(signature))
	if err != nil || len(given) == 0 {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return hmac.Equal(mac.Sum(nil), given)
}
