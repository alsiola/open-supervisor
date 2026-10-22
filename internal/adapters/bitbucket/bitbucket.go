// Package bitbucket is the Bitbucket Cloud code host: pull requests, their comments and commit statuses through
// the 2.0 REST API, and webhooks signed with the hook's secret.
package bitbucket

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
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/core"
)

func init() {
	adapter.Register(adapter.Spec{
		Type:        "bitbucket",
		Title:       "Bitbucket Cloud",
		Description: "Repositories, pull requests and commit statuses on bitbucket.org.",
		Roles:       []adapter.Role{adapter.RoleRepo},
		Fields: []adapter.Field{
			{Key: "auth", Label: "Authentication", Type: adapter.Select, Options: []string{"basic", "bearer"}, Default: "basic",
				Help: "basic: your Atlassian email and an API token. bearer: a repository, project or workspace access token."},
			{Key: "username", Label: "Email or username", Type: adapter.Text, Required: true, ShowIf: map[string][]string{"auth": {"basic"}},
				Help: "The Atlassian account email the API token belongs to."},
			{Key: "token", Label: "Token", Type: adapter.Secret, Required: true,
				Help: "An API token (basic) or an access token (bearer) with repository, pull request and webhook scopes."},
			{Key: "apiUrl", Label: "API URL", Type: adapter.Text, Default: "https://api.bitbucket.org/2.0"},
			{Key: "previewPattern", Label: "Preview status pattern", Type: adapter.Text, Default: "preview|deploy",
				Help: "A regular expression over commit status keys and names; a successful match's URL is the deploy preview."},
		},
		Binding: map[adapter.Role][]adapter.Field{
			adapter.RoleRepo: {{Key: "repo", Label: "Repository", Type: adapter.Text, Required: true, Placeholder: "workspace/slug",
				Help: "Detected from the checkout's origin when it is on bitbucket.org."}},
		},
		New: open,
	})
}

// Conn is a Bitbucket Cloud account.
type Conn struct {
	api     *adapter.JSONClient
	auth    string
	dryRun  bool
	preview *regexp.Regexp
	log     func(string, ...any)
}

func open(v adapter.Values, env adapter.Env) (adapter.Connection, error) {
	token, err := env.Secret(v.Get("token", ""))
	if err != nil {
		return nil, err
	}
	if token == "" {
		return nil, fmt.Errorf("no token")
	}
	auth := v.Get("auth", "basic")
	user := v.Get("username", "")
	if auth == "basic" && user == "" {
		return nil, fmt.Errorf("basic auth needs a username")
	}
	pattern, err := regexp.Compile("(?i)" + v.Get("previewPattern", "preview|deploy"))
	if err != nil {
		return nil, fmt.Errorf("previewPattern: %w", err)
	}
	c := &Conn{auth: auth, dryRun: env.DryRun, preview: pattern, log: env.Log}
	if c.log == nil {
		c.log = func(string, ...any) {}
	}
	c.api = &adapter.JSONClient{
		HTTP:    env.HTTP,
		BaseURL: v.Get("apiUrl", "https://api.bitbucket.org/2.0"),
		Auth: func(r *http.Request) error {
			if auth == "bearer" {
				r.Header.Set("Authorization", "Bearer "+token)
			} else {
				r.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(user+":"+token)))
			}
			return nil
		},
	}
	return c, nil
}

// Check reads the signed-in user; an access token, which has no user, is proved by listing workspaces instead.
func (c *Conn) Check(ctx context.Context) (string, error) {
	var u struct {
		DisplayName string `json:"display_name"`
		Username    string `json:"username"`
	}
	_, err := c.api.Do(ctx, "GET", "user", nil, &u)
	if err == nil {
		name := u.DisplayName
		if name == "" {
			name = u.Username
		}
		return "signed in as " + name, nil
	}
	if c.auth != "bearer" {
		return "", err
	}
	if _, err2 := c.api.Do(ctx, "GET", "repositories?role=member&pagelen=1", nil, nil); err2 != nil {
		return "", err
	}
	return "access token works (it has no user to name)", nil
}

var remoteRe = regexp.MustCompile(`bitbucket\.org[:/]([^/]+)/([^/]+?)(?:\.git)?/?$`)

// DetectRepo reads workspace/slug from a bitbucket.org ssh or https remote.
func (c *Conn) DetectRepo(remote string) (adapter.Values, bool) {
	m := remoteRe.FindStringSubmatch(strings.TrimSpace(remote))
	if m == nil {
		return nil, false
	}
	return adapter.Values{"repo": m[1] + "/" + m[2]}, true
}

// Repo binds the account to one repository.
func (c *Conn) Repo(b adapter.Values, p adapter.ProjectInfo) (adapter.Repo, error) {
	full := strings.Trim(b.Get("repo", ""), "/")
	ws, slug, ok := strings.Cut(full, "/")
	if !ok || ws == "" || slug == "" || strings.Contains(slug, "/") {
		return nil, fmt.Errorf("repo %q isn't workspace/slug", full)
	}
	return &Repo{c: c, ws: ws, slug: slug}, nil
}

// Repo is one Bitbucket repository.
type Repo struct {
	c        *Conn
	ws, slug string
}

func (r *Repo) path(format string, args ...any) string {
	return fmt.Sprintf("repositories/%s/%s/", url.PathEscape(r.ws), url.PathEscape(r.slug)) + fmt.Sprintf(format, args...)
}

func (r *Repo) Describe() string { return "bitbucket " + r.ws + "/" + r.slug }

func (r *Repo) WebURL() string { return "https://bitbucket.org/" + r.ws + "/" + r.slug }

func (r *Repo) DefaultBranch(ctx context.Context) (string, error) {
	var repo struct {
		MainBranch struct {
			Name string `json:"name"`
		} `json:"mainbranch"`
	}
	if _, err := r.c.api.Do(ctx, "GET", strings.TrimSuffix(r.path(""), "/"), nil, &repo); err != nil {
		return "", err
	}
	if repo.MainBranch.Name == "" {
		return "main", nil
	}
	return repo.MainBranch.Name, nil
}

// page is one page of a paginated list.
type page[T any] struct {
	Values []T    `json:"values"`
	Next   string `json:"next"`
}

// all reads every page, following each page's next link.
func all[T any](ctx context.Context, c *adapter.JSONClient, path string) ([]T, error) {
	var out []T
	for next, n := path, 0; next != "" && n < 100; n++ {
		var p page[T]
		if _, err := c.Do(ctx, "GET", next, nil, &p); err != nil {
			return out, err
		}
		out = append(out, p.Values...)
		next = p.Next
	}
	return out, nil
}

type rawPR struct {
	ID          int    `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	State       string `json:"state"`
	Source      struct {
		Branch struct {
			Name string `json:"name"`
		} `json:"branch"`
		Commit struct {
			Hash string `json:"hash"`
		} `json:"commit"`
	} `json:"source"`
	Destination struct {
		Branch struct {
			Name string `json:"name"`
		} `json:"branch"`
	} `json:"destination"`
	Links struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
}

func (p rawPR) pr() core.PR {
	state := core.PROpen
	switch p.State {
	case "MERGED":
		state = core.PRMerged
	case "DECLINED", "SUPERSEDED":
		state = core.PRClosed
	}
	return core.PR{ID: p.ID, Title: p.Title, State: state, Branch: p.Source.Branch.Name, Base: p.Destination.Branch.Name,
		HeadSHA: p.Source.Commit.Hash, Body: p.Description, URL: p.Links.HTML.Href}
}

const allStates = "&state=OPEN&state=MERGED&state=DECLINED&state=SUPERSEDED"

func quote(s string) string {
	return `"` + strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `"`, `\"`) + `"`
}

// PRsFor finds PRs from the branch or whose description mentions ref, in any state.
func (r *Repo) PRsFor(ctx context.Context, branch, ref string) ([]core.PR, error) {
	found := map[int]core.PR{}
	var order []int
	add := func(q string) error {
		prs, err := all[rawPR](ctx, r.c.api, r.path("pullrequests?pagelen=50&q=%s%s", url.QueryEscape(q), allStates))
		for _, p := range prs {
			if _, ok := found[p.ID]; !ok {
				order = append(order, p.ID)
			}
			found[p.ID] = p.pr()
		}
		return err
	}
	if branch != "" {
		if err := add("source.branch.name=" + quote(branch)); err != nil {
			return nil, err
		}
	}
	if ref != "" {
		if err := add("description ~ " + quote(ref)); err != nil {
			return nil, err
		}
	}
	out := make([]core.PR, 0, len(order))
	for _, id := range order {
		out = append(out, found[id])
	}
	return out, nil
}

func (r *Repo) PR(ctx context.Context, id int) (core.PR, error) {
	var p rawPR
	if _, err := r.c.api.Do(ctx, "GET", r.path("pullrequests/%d", id), nil, &p); err != nil {
		return core.PR{}, err
	}
	return p.pr(), nil
}

type rawComment struct {
	ID      int `json:"id"`
	Content struct {
		Raw string `json:"raw"`
	} `json:"content"`
	User struct {
		DisplayName string `json:"display_name"`
		Nickname    string `json:"nickname"`
		Type        string `json:"type"`
	} `json:"user"`
	CreatedOn string `json:"created_on"`
	UpdatedOn string `json:"updated_on"`
	Deleted   bool   `json:"deleted"`
	Inline    *struct {
		Path string `json:"path"`
		From *int   `json:"from"`
		To   *int   `json:"to"`
	} `json:"inline"`
	Parent *struct {
		ID int `json:"id"`
	} `json:"parent"`
	Links struct {
		HTML struct {
			Href string `json:"href"`
		} `json:"html"`
	} `json:"links"`
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

func (c rawComment) comment(pr int) core.Comment {
	at := parseTime(c.UpdatedOn)
	if at.IsZero() {
		at = parseTime(c.CreatedOn)
	}
	author := c.User.DisplayName
	if author == "" {
		author = c.User.Nickname
	}
	body, mark := Unmark(c.Content.Raw)
	out := core.Comment{ID: strconv.Itoa(c.ID), Kind: core.OnPR, Author: author, Bot: c.User.Type == "app_user",
		Body: body, URL: c.Links.HTML.Href, At: at, PR: pr, Agent: mark}
	if c.Inline != nil {
		out.Kind = core.OnInline
		out.Path = c.Inline.Path
		if c.Inline.To != nil {
			out.Line = *c.Inline.To
		} else if c.Inline.From != nil {
			out.Line = *c.Inline.From
		}
	}
	if c.Parent != nil {
		out.InReplyTo = strconv.Itoa(c.Parent.ID)
	}
	return out
}

// PRComments are the PR's comments newer than since, conversation and inline alike.
func (r *Repo) PRComments(ctx context.Context, id int, since time.Time) ([]core.Comment, error) {
	raw, err := all[rawComment](ctx, r.c.api, r.path("pullrequests/%d/comments?pagelen=100", id))
	if err != nil {
		return nil, err
	}
	var out []core.Comment
	for _, c := range raw {
		if c.Deleted {
			continue
		}
		cc := c.comment(id)
		if cc.At.After(since) {
			out = append(out, cc)
		}
	}
	return out, nil
}

type rawStatus struct {
	Key   string `json:"key"`
	Name  string `json:"name"`
	State string `json:"state"`
	URL   string `json:"url"`
}

// Checks is the combined state of the commit's statuses (Pipelines and anything else that reports).
func (r *Repo) Checks(ctx context.Context, sha string) (core.Checks, error) {
	raw, err := all[rawStatus](ctx, r.c.api, r.path("commit/%s/statuses?pagelen=100", url.PathEscape(sha)))
	if err != nil {
		return core.Checks{}, err
	}
	return r.c.evaluate(raw), nil
}

func (c *Conn) evaluate(raw []rawStatus) core.Checks {
	out := core.Checks{Any: len(raw) > 0}
	pending := false
	for _, s := range raw {
		name := s.Name
		if name == "" {
			name = s.Key
		}
		run := core.CheckRun{Name: name, URL: s.URL}
		switch s.State {
		case "SUCCESSFUL":
			run.State = "success"
			if out.Preview == "" && s.URL != "" && (c.preview.MatchString(s.Key) || c.preview.MatchString(s.Name)) {
				out.Preview = s.URL
			}
		case "FAILED", "STOPPED":
			run.State = "failure"
			out.Failed = append(out.Failed, name)
		default:
			run.State = "pending"
			pending = true
		}
		out.Runs = append(out.Runs, run)
	}
	out.Complete = out.Any && !pending
	return out
}

func (r *Repo) CreatePR(ctx context.Context, req adapter.PRRequest) (core.PR, error) {
	body := map[string]any{
		"title":       req.Title,
		"description": req.Body,
		"source":      map[string]any{"branch": map[string]string{"name": req.Head}},
		"destination": map[string]any{"branch": map[string]string{"name": req.Base}},
		"draft":       req.Draft,
	}
	if r.c.dryRun {
		return core.PR{Title: req.Title, Branch: req.Head, Base: req.Base, State: core.PROpen}, nil
	}
	var p rawPR
	if _, err := r.c.api.Do(ctx, "POST", r.path("pullrequests"), body, &p); err != nil {
		return core.PR{}, err
	}
	return p.pr(), nil
}

func (r *Repo) postComment(ctx context.Context, id int, body map[string]any) (string, error) {
	if r.c.dryRun {
		return "", nil
	}
	var c rawComment
	if _, err := r.c.api.Do(ctx, "POST", r.path("pullrequests/%d/comments", id), body, &c); err != nil {
		return "", err
	}
	return c.Links.HTML.Href, nil
}

func (r *Repo) CommentPR(ctx context.Context, id int, body string, mark core.AgentMark) (string, error) {
	return r.postComment(ctx, id, map[string]any{"content": map[string]string{"raw": Mark(body, mark)}})
}

func (r *Repo) ReplyInline(ctx context.Context, id int, commentID, body string, mark core.AgentMark) (string, error) {
	parent, err := strconv.Atoi(commentID)
	if err != nil {
		return "", fmt.Errorf("comment id %q isn't a number", commentID)
	}
	return r.postComment(ctx, id, map[string]any{"content": map[string]string{"raw": Mark(body, mark)}, "parent": map[string]int{"id": parent}})
}

// ---- agent marks ----------------------------------------------------------------------------------------

var markRe = regexp.MustCompile(`(?m)\n*^\[//\]: # \(open-supervisor:agent(?: from=([^)\s]+))?\)\s*$`)

// Mark appends the invisible line that says an agent posted the comment.
func Mark(body string, m core.AgentMark) string {
	line := "[//]: # (open-supervisor:agent)"
	if m.From != "" {
		line = "[//]: # (open-supervisor:agent from=" + m.From + ")"
	}
	return strings.TrimRight(body, "\n") + "\n\n" + line
}

// Unmark takes the agent line off a body and says who posted it.
func Unmark(body string) (string, *core.AgentMark) {
	m := markRe.FindStringSubmatch(body)
	if m == nil {
		return body, nil
	}
	return strings.TrimRight(markRe.ReplaceAllString(body, ""), "\n"), &core.AgentMark{From: m[1]}
}

// ---- webhooks -------------------------------------------------------------------------------------------

var hookEvents = []string{
	"pullrequest:created", "pullrequest:updated", "pullrequest:fulfilled", "pullrequest:rejected",
	"pullrequest:comment_created", "repo:commit_status_created", "repo:commit_status_updated",
}

func (r *Repo) HookTarget() string { return "bitbucket:" + r.ws + "/" + r.slug }

// EnsureWebhook points the repository's hook at url, updating the one already registered when it still exists.
func (r *Repo) EnsureWebhook(ctx context.Context, hookURL, secret, existing string) (string, error) {
	body := map[string]any{"description": "open-supervisor", "url": hookURL, "active": true, "secret": secret, "events": hookEvents}
	if existing != "" {
		if r.c.dryRun {
			return existing, nil
		}
		var h struct {
			UUID string `json:"uuid"`
		}
		_, err := r.c.api.Do(ctx, "PUT", r.path("hooks/%s", url.PathEscape(existing)), body, &h)
		if err == nil {
			if h.UUID == "" {
				h.UUID = existing
			}
			return h.UUID, nil
		}
		if !adapter.IsStatus(err, http.StatusNotFound) {
			return "", err
		}
	}
	if r.c.dryRun {
		return "dry-run", nil
	}
	var h struct {
		UUID string `json:"uuid"`
	}
	if _, err := r.c.api.Do(ctx, "POST", r.path("hooks"), body, &h); err != nil {
		return "", err
	}
	return h.UUID, nil
}

// ParseWebhook checks the delivery's signature and turns it into signals.
func (r *Repo) ParseWebhook(h http.Header, body []byte, secret string) ([]core.Signal, error) {
	sig := h.Get("X-Hub-Signature")
	if sig == "" {
		sig = h.Get("X-Hub-Signature-256")
	}
	if !adapter.VerifyHMAC(secret, body, sig) {
		return nil, fmt.Errorf("bitbucket: a delivery with a missing or bad signature")
	}
	event := h.Get("X-Event-Key")
	var p struct {
		PullRequest  *rawPR      `json:"pullrequest"`
		Comment      *rawComment `json:"comment"`
		CommitStatus *struct {
			Commit struct {
				Hash string `json:"hash"`
			} `json:"commit"`
		} `json:"commit_status"`
		Repository struct {
			FullName string `json:"full_name"`
		} `json:"repository"`
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("bitbucket: %s delivery isn't JSON: %w", event, err)
	}
	if p.Repository.FullName != "" && !strings.EqualFold(p.Repository.FullName, r.ws+"/"+r.slug) {
		return nil, nil
	}
	switch event {
	case "pullrequest:created", "pullrequest:updated":
		if p.PullRequest != nil {
			pr := p.PullRequest.pr()
			return []core.Signal{{Type: core.SigPRChanged, PR: &pr}}, nil
		}
	case "pullrequest:fulfilled", "pullrequest:rejected":
		if p.PullRequest != nil {
			pr := p.PullRequest.pr()
			if event == "pullrequest:fulfilled" {
				pr.State = core.PRMerged
			} else {
				pr.State = core.PRClosed
			}
			return []core.Signal{{Type: core.SigPRClosed, PR: &pr}}, nil
		}
	case "pullrequest:comment_created":
		if p.PullRequest != nil && p.Comment != nil {
			pr := p.PullRequest.pr()
			c := p.Comment.comment(pr.ID)
			return []core.Signal{{Type: core.SigPRComment, PR: &pr, Comment: &c}}, nil
		}
	case "repo:commit_status_created", "repo:commit_status_updated":
		if p.CommitStatus != nil && p.CommitStatus.Commit.Hash != "" {
			return []core.Signal{{Type: core.SigChecks, SHA: p.CommitStatus.Commit.Hash}}, nil
		}
	}
	return nil, nil
}
