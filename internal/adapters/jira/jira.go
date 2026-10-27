// Package jira is the Jira issue tracker, Cloud or Data Center, through REST API v2 (whose bodies are wiki
// markup, converted to and from the Markdown agents write). Agents' comments carry an entity property saying
// so; status labels are Jira labels, and the issue can also be moved through the project's workflow as the
// agent's state changes.
package jira

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/core"
)

// stateFields map each state the supervisor moves an issue to onto the binding field naming its status.
var stateFields = map[string]string{
	"claimed":  "transitionClaimed",
	"building": "transitionBuilding",
	"review":   "transitionReview",
	"help":     "transitionHelp",
	"done":     "transitionDone",
	"finished": "transitionFinished",
}

func init() {
	transition := func(key, label string) adapter.Field {
		return adapter.Field{Key: key, Label: label, Type: adapter.Text, Help: "The status to move the issue to; empty leaves its status alone."}
	}
	adapter.Register(adapter.Spec{
		Type:        "jira",
		Title:       "Jira",
		Description: "Issues in a Jira project, Cloud or Data Center.",
		Roles:       []adapter.Role{adapter.RoleTracker},
		Fields: []adapter.Field{
			{Key: "baseUrl", Label: "Site URL", Type: adapter.Text, Required: true, Placeholder: "https://acme.atlassian.net"},
			{Key: "auth", Label: "Authentication", Type: adapter.Select, Options: []string{"basic", "bearer"}, Default: "basic",
				Help: "basic: your email and an API token (Cloud). bearer: a personal access token (Data Center)."},
			{Key: "email", Label: "Email", Type: adapter.Text, Required: true, ShowIf: map[string][]string{"auth": {"basic"}}},
			{Key: "token", Label: "Token", Type: adapter.Secret, Required: true},
			{Key: "agentAccount", Label: "Agent account", Type: adapter.Text,
				Help: "An account id or name whose comments always count as an agent's; leave empty when agents post as you."},
		},
		Binding: map[adapter.Role][]adapter.Field{
			adapter.RoleTracker: {
				{Key: "project", Label: "Project key", Type: adapter.Text, Required: true, Placeholder: "PROJ",
					Help: "Several, comma-separated, for a source that routes them to projects: API, WEB."},
				{Key: "jql", Label: "Extra JQL", Type: adapter.Text, Help: "ANDed with the project and open filter, e.g. component = Web."},
				{Key: "fields", Label: "Extra fields for routes", Type: adapter.Text,
					Help: "Comma-separated field ids routes can match on, e.g. customfield_10010."},
				transition("transitionClaimed", "Status when claimed"),
				transition("transitionBuilding", "Status while building"),
				transition("transitionReview", "Status when up for review"),
				transition("transitionHelp", "Status when it needs help"),
				transition("transitionDone", "Status when done"),
				transition("transitionFinished", "Status when finished"),
			},
		},
		New: open,
	})
}

// Conn is a Jira site and the account used on it.
type Conn struct {
	base         string
	api          *adapter.JSONClient
	agentAccount string
	dryRun       bool
	log          func(string, ...any)

	mu         sync.Mutex
	categories map[string]string // status id → category key
}

func open(v adapter.Values, env adapter.Env) (adapter.Connection, error) {
	base := strings.TrimRight(v.Get("baseUrl", ""), "/")
	if base == "" {
		return nil, fmt.Errorf("no site URL")
	}
	token, err := env.Secret(v.Get("token", ""))
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, fmt.Errorf("no token")
	}
	auth := v.Get("auth", "basic")
	email := v.Get("email", "")
	if auth == "basic" && email == "" {
		return nil, fmt.Errorf("basic auth needs an email")
	}
	c := &Conn{base: base, agentAccount: v.Get("agentAccount", ""), dryRun: env.DryRun, log: env.Log}
	if c.log == nil {
		c.log = func(string, ...any) {}
	}
	c.api = &adapter.JSONClient{
		HTTP:    env.HTTP,
		BaseURL: base,
		Auth: func(r *http.Request) error {
			if auth == "bearer" {
				r.Header.Set("Authorization", "Bearer "+token)
			} else {
				r.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(email+":"+token)))
			}
			return nil
		},
	}
	return c, nil
}

// Check reads the signed-in account.
func (c *Conn) Check(ctx context.Context) (string, error) {
	var me struct {
		DisplayName string `json:"displayName"`
	}
	if _, err := c.api.Do(ctx, "GET", "rest/api/2/myself", nil, &me); err != nil {
		return "", err
	}
	return "signed in as " + me.DisplayName, nil
}

var keyRe = regexp.MustCompile(`^[A-Z][A-Z0-9_]*$`)

// Tracker binds the site to one project's issues, or several projects' for a source that routes them.
func (c *Conn) Tracker(b adapter.Values, p adapter.ProjectInfo) (adapter.Tracker, error) {
	var keys, quoted []string
	for _, k := range strings.Split(b.Get("project", ""), ",") {
		k = strings.ToUpper(strings.TrimSpace(k))
		if k == "" {
			continue
		}
		if !keyRe.MatchString(k) {
			return nil, fmt.Errorf("project key %q isn't a Jira key", k)
		}
		keys = append(keys, k)
		quoted = append(quoted, regexp.QuoteMeta(k))
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("no project key")
	}
	t := &Tracker{c: c, projects: keys, project: strings.Join(keys, ","), jql: strings.TrimSpace(b.Get("jql", "")), statuses: map[string]string{}}
	for state, field := range stateFields {
		if s := strings.TrimSpace(b.Get(field, "")); s != "" {
			t.statuses[state] = s
		}
	}
	for _, f := range strings.Split(b.Get("fields", ""), ",") {
		if f = strings.TrimSpace(f); f != "" {
			t.extra = append(t.extra, f)
		}
	}
	t.refRe = regexp.MustCompile(`(?i)\b(?:` + strings.Join(quoted, "|") + `)-\d+\b`)
	return t, nil
}

// Tracker is one or more Jira projects' issues.
type Tracker struct {
	c        *Conn
	projects []string
	// project names them for the log and the hook: "API" or "API,WEB".
	project  string
	jql      string
	statuses map[string]string
	refRe    *regexp.Regexp
	// extra are fields read for routes beyond the ones every issue gets.
	extra []string
}

// owns reports whether an issue key is in one of the tracker's projects.
func (t *Tracker) owns(key string) bool {
	for _, p := range t.projects {
		if strings.HasPrefix(strings.ToUpper(key), p+"-") {
			return true
		}
	}
	return false
}

// projectJQL selects the tracker's projects.
func (t *Tracker) projectJQL() string {
	if len(t.projects) == 1 {
		return fmt.Sprintf("project = %q", t.projects[0])
	}
	q := make([]string, len(t.projects))
	for i, p := range t.projects {
		q[i] = fmt.Sprintf("%q", p)
	}
	return "project in (" + strings.Join(q, ", ") + ")"
}

func (t *Tracker) Describe() string { return "jira " + t.project }

func (t *Tracker) IssueURL(key string) string { return t.c.base + "/browse/" + key }

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

// RefFor is the key alone: Jira links a PR that names it and never closes the issue from one.
func (t *Tracker) RefFor(key string, _ bool) string { return key }

func (t *Tracker) HoldMarker(h core.Hold) string {
	return "[" + h.Spec() + "]"
}

// ---- issues ---------------------------------------------------------------------------------------------

type rawUser struct {
	AccountID    string `json:"accountId"`
	Name         string `json:"name"`
	Key          string `json:"key"`
	EmailAddress string `json:"emailAddress"`
	DisplayName  string `json:"displayName"`
	AccountType  string `json:"accountType"`
}

type rawStatus struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	StatusCategory struct {
		Key string `json:"key"`
	} `json:"statusCategory"`
}

type rawNamed struct {
	Key  string `json:"key"`
	Name string `json:"name"`
}

type rawFields struct {
	Summary     string     `json:"summary"`
	Description string     `json:"description"`
	Labels      []string   `json:"labels"`
	Status      rawStatus  `json:"status"`
	Created     string     `json:"created"`
	Updated     string     `json:"updated"`
	Reporter    *rawUser   `json:"reporter"`
	Assignee    *rawUser   `json:"assignee"`
	Project     *rawNamed  `json:"project"`
	IssueType   *rawNamed  `json:"issuetype"`
	Priority    *rawNamed  `json:"priority"`
	Components  []rawNamed `json:"components"`
	FixVersions []rawNamed `json:"fixVersions"`
	Parent      *rawNamed  `json:"parent"`
	// all are every field as it came, for the extra ones routes match on.
	all map[string]json.RawMessage
}

func (f *rawFields) UnmarshalJSON(data []byte) error {
	type plain rawFields
	if err := json.Unmarshal(data, (*plain)(f)); err != nil {
		return err
	}
	return json.Unmarshal(data, &f.all)
}

type rawIssue struct {
	Key       string    `json:"key"`
	Fields    rawFields `json:"fields"`
	Changelog *struct {
		Histories []rawHistory `json:"histories"`
	} `json:"changelog"`
}

type rawHistory struct {
	Created string    `json:"created"`
	Changes []rawItem `json:"items"`
}

type rawItem struct {
	Field      string `json:"field"`
	From       string `json:"from"`
	To         string `json:"to"`
	FromString string `json:"fromString"`
	ToString   string `json:"toString"`
}

const issueFields = "summary,labels,status,created,updated,description,reporter,assignee,project,issuetype,priority,components,fixVersions,parent"

// fields are the fields to read: every issue's, and the extra ones routes match on.
func (t *Tracker) fields() []string {
	return append(strings.Split(issueFields, ","), t.extra...)
}

func parseTime(s string) time.Time {
	for _, l := range []string{"2006-01-02T15:04:05.000-0700", "2006-01-02T15:04:05-0700", time.RFC3339Nano} {
		if t, err := time.Parse(l, s); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

func (t *Tracker) issue(r rawIssue) core.Issue {
	i := core.Issue{
		Key:       r.Key,
		Title:     r.Fields.Summary,
		Body:      ToMarkdown(r.Fields.Description),
		URL:       t.IssueURL(r.Key),
		Labels:    r.Fields.Labels,
		Open:      r.Fields.Status.StatusCategory.Key != "done",
		CreatedAt: parseTime(r.Fields.Created),
		UpdatedAt: parseTime(r.Fields.Updated),
	}
	f := r.Fields
	if f.Reporter != nil {
		i.Author = f.Reporter.DisplayName
	}
	if f.Project != nil {
		i.SetAttr("project", f.Project.Key, f.Project.Name)
	}
	if f.IssueType != nil {
		i.SetAttr("issuetype", f.IssueType.Name)
	}
	if f.Priority != nil {
		i.SetAttr("priority", f.Priority.Name)
	}
	if f.Assignee != nil {
		i.SetAttr("assignee", f.Assignee.DisplayName, f.Assignee.EmailAddress)
	}
	if f.Parent != nil {
		i.SetAttr("parent", f.Parent.Key)
	}
	i.SetAttr("status", f.Status.Name)
	for _, c := range f.Components {
		i.SetAttr("components", c.Name)
	}
	for _, v := range f.FixVersions {
		i.SetAttr("fixVersions", v.Name)
	}
	for _, id := range t.extra {
		i.SetAttr(id, flatten(f.all[id])...)
	}
	return i
}

// flatten reads a field's value as strings: a string or number as it is, an option or user by its value or
// name, a list as each of its items.
func flatten(raw json.RawMessage) []string {
	var v any
	if len(raw) == 0 || json.Unmarshal(raw, &v) != nil {
		return nil
	}
	var out []string
	var walk func(v any)
	walk = func(v any) {
		switch x := v.(type) {
		case string:
			out = append(out, x)
		case float64:
			out = append(out, strconv.FormatFloat(x, 'f', -1, 64))
		case bool:
			out = append(out, strconv.FormatBool(x))
		case []any:
			for _, y := range x {
				walk(y)
			}
		case map[string]any:
			for _, k := range []string{"value", "name", "key", "displayName"} {
				if s, ok := x[k].(string); ok && s != "" {
					out = append(out, s)
					return
				}
			}
		}
	}
	walk(v)
	return out
}

func (t *Tracker) query() string {
	q := t.projectJQL() + " AND statusCategory != Done"
	if t.jql != "" {
		q += " AND (" + t.jql + ")"
	}
	return q + " ORDER BY created ASC"
}

// Candidates are the project's open issues.
func (t *Tracker) Candidates(ctx context.Context) ([]core.Issue, error) {
	raw, err := t.searchJQL(ctx)
	if adapter.IsStatus(err, http.StatusNotFound) {
		raw, err = t.searchLegacy(ctx)
	}
	if err != nil {
		return nil, err
	}
	out := make([]core.Issue, 0, len(raw))
	for _, r := range raw {
		out = append(out, t.issue(r))
	}
	return out, nil
}

func (t *Tracker) searchJQL(ctx context.Context) ([]rawIssue, error) {
	var all []rawIssue
	token := ""
	for n := 0; n < 50; n++ {
		body := map[string]any{"jql": t.query(), "fields": t.fields(), "maxResults": 100}
		if token != "" {
			body["nextPageToken"] = token
		}
		var page struct {
			Issues        []rawIssue `json:"issues"`
			NextPageToken string     `json:"nextPageToken"`
			IsLast        bool       `json:"isLast"`
		}
		if _, err := t.c.api.Do(ctx, "POST", "rest/api/2/search/jql", body, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Issues...)
		if page.IsLast || page.NextPageToken == "" {
			break
		}
		token = page.NextPageToken
	}
	return all, nil
}

func (t *Tracker) searchLegacy(ctx context.Context) ([]rawIssue, error) {
	var all []rawIssue
	for start := 0; start < 5000; {
		body := map[string]any{"jql": t.query(), "fields": t.fields(), "maxResults": 100, "startAt": start}
		var page struct {
			Issues []rawIssue `json:"issues"`
			Total  int        `json:"total"`
		}
		if _, err := t.c.api.Do(ctx, "POST", "rest/api/2/search", body, &page); err != nil {
			return nil, err
		}
		all = append(all, page.Issues...)
		start += len(page.Issues)
		if len(page.Issues) == 0 || start >= page.Total {
			break
		}
	}
	return all, nil
}

func (t *Tracker) Issue(ctx context.Context, key string) (core.Issue, error) {
	var r rawIssue
	if _, err := t.c.api.Do(ctx, "GET", "rest/api/2/issue/"+url.PathEscape(key)+"?fields="+url.QueryEscape(strings.Join(t.fields(), ",")), nil, &r); err != nil {
		return core.Issue{}, err
	}
	return t.issue(r), nil
}

// ---- comments -------------------------------------------------------------------------------------------

type rawComment struct {
	ID         string   `json:"id"`
	Author     *rawUser `json:"author"`
	Body       string   `json:"body"`
	Created    string   `json:"created"`
	Updated    string   `json:"updated"`
	Properties []struct {
		Key   string          `json:"key"`
		Value json.RawMessage `json:"value"`
	} `json:"properties"`
}

const markProperty = "open-supervisor"

type markValue struct {
	Agent bool   `json:"agent"`
	From  string `json:"from,omitempty"`
}

func (t *Tracker) comment(key string, r rawComment) core.Comment {
	at := parseTime(r.Updated)
	if at.IsZero() {
		at = parseTime(r.Created)
	}
	c := core.Comment{
		ID:   r.ID,
		Kind: core.OnIssue,
		Body: ToMarkdown(r.Body),
		URL:  t.IssueURL(key) + "?focusedCommentId=" + r.ID,
		At:   at,
	}
	if r.Author != nil {
		c.Author = r.Author.DisplayName
		c.Bot = r.Author.AccountType == "app"
		if a := t.c.agentAccount; a != "" && (a == r.Author.AccountID || a == r.Author.Name || a == r.Author.Key || strings.EqualFold(a, r.Author.EmailAddress)) {
			c.Agent = &core.AgentMark{}
		}
	}
	for _, p := range r.Properties {
		if p.Key != markProperty {
			continue
		}
		var v markValue
		if json.Unmarshal(p.Value, &v) == nil && v.Agent {
			c.Agent = &core.AgentMark{From: v.From}
		}
	}
	return c
}

// Comments on the issue newer than since.
func (t *Tracker) Comments(ctx context.Context, key string, since time.Time) ([]core.Comment, error) {
	var out []core.Comment
	for start := 0; start < 10000; {
		var page struct {
			Comments []rawComment `json:"comments"`
			Total    int          `json:"total"`
		}
		path := fmt.Sprintf("rest/api/2/issue/%s/comment?expand=properties&orderBy=created&maxResults=100&startAt=%d", url.PathEscape(key), start)
		if _, err := t.c.api.Do(ctx, "GET", path, nil, &page); err != nil {
			return nil, err
		}
		for _, r := range page.Comments {
			if c := t.comment(key, r); c.At.After(since) {
				out = append(out, c)
			}
		}
		start += len(page.Comments)
		if len(page.Comments) == 0 || start >= page.Total {
			break
		}
	}
	return out, nil
}

// Comment posts on the issue with the property that marks it as an agent's.
func (t *Tracker) Comment(ctx context.Context, key, body string, mark core.AgentMark) (string, error) {
	if t.c.dryRun {
		return "", nil
	}
	req := map[string]any{
		"body":       ToWiki(body),
		"properties": []map[string]any{{"key": markProperty, "value": markValue{Agent: true, From: mark.From}}},
	}
	var r rawComment
	if _, err := t.c.api.Do(ctx, "POST", "rest/api/2/issue/"+url.PathEscape(key)+"/comment", req, &r); err != nil {
		return "", err
	}
	return t.IssueURL(key) + "?focusedCommentId=" + r.ID, nil
}

// OpenIssue opens a Task in the tracker's first project.
func (t *Tracker) OpenIssue(ctx context.Context, title, body string, labels []string) (core.Issue, error) {
	if t.c.dryRun {
		return core.Issue{}, nil
	}
	fields := map[string]any{
		"project":     map[string]string{"key": t.projects[0]},
		"summary":     title,
		"description": ToWiki(body),
		"issuetype":   map[string]string{"name": "Task"},
	}
	if len(labels) > 0 {
		fields["labels"] = labels
	}
	req := map[string]any{"fields": fields}
	var r struct {
		Key string `json:"key"`
	}
	if _, err := t.c.api.Do(ctx, "POST", "rest/api/2/issue", req, &r); err != nil {
		return core.Issue{}, err
	}
	return core.Issue{Key: r.Key, Title: title, Body: body, URL: t.IssueURL(r.Key), Labels: labels, Open: true}, nil
}

// ---- labels and workflow --------------------------------------------------------------------------------

func (t *Tracker) EditLabels(ctx context.Context, key string, add, remove []string) error {
	var ops []map[string]string
	for _, l := range add {
		if strings.ContainsAny(l, " \t") {
			return fmt.Errorf("jira labels can't contain spaces: %q (rename it in the labels settings)", l)
		}
		ops = append(ops, map[string]string{"add": l})
	}
	for _, l := range remove {
		if strings.ContainsAny(l, " \t") {
			return fmt.Errorf("jira labels can't contain spaces: %q (rename it in the labels settings)", l)
		}
		ops = append(ops, map[string]string{"remove": l})
	}
	if len(ops) == 0 || t.c.dryRun {
		return nil
	}
	_, err := t.c.api.Do(ctx, "PUT", "rest/api/2/issue/"+url.PathEscape(key), map[string]any{"update": map[string]any{"labels": ops}}, nil)
	return err
}

// EnsureLabels does nothing: Jira labels exist once used.
func (t *Tracker) EnsureLabels(ctx context.Context, labels []adapter.Label) error { return nil }

// Transition moves the issue to the status configured for the state, if one is.
func (t *Tracker) Transition(ctx context.Context, key, state string) error {
	want, ok := t.statuses[state]
	if !ok || t.c.dryRun {
		return nil
	}
	var res struct {
		Transitions []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
			To   struct {
				Name string `json:"name"`
			} `json:"to"`
		} `json:"transitions"`
	}
	path := "rest/api/2/issue/" + url.PathEscape(key) + "/transitions"
	if _, err := t.c.api.Do(ctx, "GET", path, nil, &res); err != nil {
		return err
	}
	id := ""
	for _, tr := range res.Transitions {
		if strings.EqualFold(tr.To.Name, want) {
			id = tr.ID
			break
		}
	}
	if id == "" {
		for _, tr := range res.Transitions {
			if strings.EqualFold(tr.Name, want) {
				id = tr.ID
				break
			}
		}
	}
	if id == "" {
		var names []string
		for _, tr := range res.Transitions {
			names = append(names, tr.To.Name)
		}
		return fmt.Errorf("no transition to %q from here (can go to: %s)", want, strings.Join(names, ", "))
	}
	_, err := t.c.api.Do(ctx, "POST", path, map[string]any{"transition": map[string]string{"id": id}}, nil)
	return err
}

// ---- reopens --------------------------------------------------------------------------------------------

// categories maps each status id to its category key, read once.
func (c *Conn) statusCategories(ctx context.Context) (map[string]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.categories != nil {
		return c.categories, nil
	}
	var all []rawStatus
	if _, err := c.api.Do(ctx, "GET", "rest/api/2/status", nil, &all); err != nil {
		return nil, err
	}
	m := map[string]string{}
	for _, s := range all {
		m[s.ID] = s.StatusCategory.Key
	}
	c.categories = m
	return m, nil
}

// ReopenedAt is the latest status change out of a done-category status (or, without the categories, the
// latest time the resolution was cleared).
func (t *Tracker) ReopenedAt(ctx context.Context, key string) (time.Time, error) {
	var r rawIssue
	if _, err := t.c.api.Do(ctx, "GET", "rest/api/2/issue/"+url.PathEscape(key)+"?fields=status&expand=changelog", nil, &r); err != nil {
		return time.Time{}, err
	}
	if r.Changelog == nil {
		return time.Time{}, nil
	}
	cats, catErr := t.c.statusCategories(ctx)
	var latest time.Time
	for _, h := range r.Changelog.Histories {
		at := parseTime(h.Created)
		for _, it := range h.Changes {
			reopen := false
			switch {
			case catErr == nil && it.Field == "status":
				reopen = cats[it.From] == "done" && cats[it.To] != "done"
			case catErr != nil && it.Field == "resolution":
				reopen = it.From != "" && it.To == ""
			}
			if reopen && at.After(latest) {
				latest = at
			}
		}
	}
	return latest, nil
}

// ---- webhooks -------------------------------------------------------------------------------------------

func (t *Tracker) HookTarget() string { return "jira:" + t.c.base + ":" + t.project }

// EnsureWebhook registers the project's hook through the admin API, which needs a Jira administrator.
func (t *Tracker) EnsureWebhook(ctx context.Context, hookURL, secret, existing string) (string, error) {
	body := map[string]any{
		"name":        "open-supervisor " + t.project,
		"url":         hookURL,
		"events":      []string{"jira:issue_created", "jira:issue_updated", "comment_created"},
		"filters":     map[string]string{"issue-related-events-section": t.projectJQL()},
		"secret":      secret,
		"excludeBody": false,
	}
	if t.c.dryRun {
		if existing == "" {
			existing = "dry-run"
		}
		return existing, nil
	}
	var res struct {
		Self string `json:"self"`
	}
	var err error
	if existing != "" {
		_, err = t.c.api.Do(ctx, "PUT", "rest/webhooks/1.0/webhook/"+url.PathEscape(existing), body, &res)
		if err == nil {
			return existing, nil
		}
		if !adapter.IsStatus(err, http.StatusNotFound) {
			return "", t.manual(err, hookURL)
		}
	}
	if _, err = t.c.api.Do(ctx, "POST", "rest/webhooks/1.0/webhook", body, &res); err != nil {
		return "", t.manual(err, hookURL)
	}
	id := res.Self[strings.LastIndex(res.Self, "/")+1:]
	if id == "" {
		return "", fmt.Errorf("jira registered the webhook but didn't say its id")
	}
	return id, nil
}

func (t *Tracker) manual(err error, hookURL string) error {
	if adapter.IsStatus(err, http.StatusUnauthorized) || adapter.IsStatus(err, http.StatusForbidden) {
		return fmt.Errorf("only a Jira administrator can register webhooks: add one by hand in Jira's settings (System → WebHooks) for %s with the events issue created, issue updated and comment created, JQL `%s` and the secret in the supervisor's state dir (webhook-secret), or rely on the reconcile", hookURL, t.projectJQL())
	}
	return err
}

// ParseWebhook checks the delivery's signature, when Jira signed it, and turns it into signals.
func (t *Tracker) ParseWebhook(h http.Header, body []byte, secret string) ([]core.Signal, error) {
	if sig := h.Get("X-Hub-Signature"); sig != "" {
		if !adapter.VerifyHMAC(secret, body, sig) {
			return nil, fmt.Errorf("jira: a delivery with a bad signature")
		}
	} else if secret != "" {
		t.c.log("jira: accepted an unsigned delivery for %s (set the webhook's secret in Jira to have them signed)", t.project)
	}
	var p struct {
		WebhookEvent string      `json:"webhookEvent"`
		Issue        *rawIssue   `json:"issue"`
		Comment      *rawComment `json:"comment"`
		Changelog    *struct {
			Items []rawItem `json:"items"`
		} `json:"changelog"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("jira: a delivery that isn't JSON: %w", err)
	}
	if p.Issue == nil || !t.owns(p.Issue.Key) {
		return nil, nil
	}
	issue := t.issue(*p.Issue)
	switch p.WebhookEvent {
	case "jira:issue_created":
		return []core.Signal{{Type: core.SigIssueChanged, Key: issue.Key, Issue: &issue}}, nil
	case "jira:issue_updated":
		if p.Changelog == nil {
			return nil, nil
		}
		changed := false
		for _, it := range p.Changelog.Items {
			if it.Field == "status" && !issue.Open {
				return []core.Signal{{Type: core.SigIssueClosed, Key: issue.Key}}, nil
			}
			if it.Field == "status" && t.wasDone(it) {
				return []core.Signal{{Type: core.SigIssueReopened, Key: issue.Key, Issue: &issue}}, nil
			}
			// Any other change (labels, components, a move to Todo) can make it claimable or change its route.
			changed = true
		}
		if !changed {
			return nil, nil
		}
		return []core.Signal{{Type: core.SigIssueChanged, Key: issue.Key, Issue: &issue}}, nil
	case "comment_created":
		if p.Comment == nil {
			return nil, nil
		}
		// Deliveries leave out the comment's properties, so read it back to see whether an agent wrote it.
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		var full rawComment
		path := "rest/api/2/issue/" + url.PathEscape(issue.Key) + "/comment/" + url.PathEscape(p.Comment.ID) + "?expand=properties"
		if _, err := t.c.api.Do(ctx, "GET", path, nil, &full); err != nil {
			// The reconcile reads it properly later.
			return nil, fmt.Errorf("jira: couldn't read comment %s on %s: %w", p.Comment.ID, issue.Key, err)
		}
		c := t.comment(issue.Key, full)
		return []core.Signal{{Type: core.SigIssueComment, Key: issue.Key, Comment: &c}}, nil
	}
	return nil, nil
}

// wasDone reports whether a status change came out of a done-category status.
func (t *Tracker) wasDone(it rawItem) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cats, err := t.c.statusCategories(ctx)
	if err != nil {
		return false
	}
	return cats[it.From] == "done"
}
