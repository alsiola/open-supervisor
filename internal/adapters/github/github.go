// Package github is GitHub as a code host and an issue tracker, through the REST API. Reads go through one
// identity (the gh CLI's login, a token or a GitHub App); writes (comments, labels, reactions, PRs) can go
// through a second App so people can see the agent did them. A tracker and a repo bound to the same repository
// share one webhook.
package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/alsiola/open-supervisor/internal/adapter"
)

func init() {
	adapter.Register(adapter.Spec{
		Type:        "github",
		Title:       "GitHub",
		Description: "GitHub (or GitHub Enterprise) repositories and their issues",
		Roles:       []adapter.Role{adapter.RoleRepo, adapter.RoleTracker},
		Fields: []adapter.Field{
			{Key: "auth", Label: "Sign in with", Type: adapter.Select, Options: []string{"gh", "token", "app"}, Default: "gh",
				Help: "gh: the gh CLI's login. token: a personal access token. app: a GitHub App installed on the repos."},
			{Key: "ghBinary", Label: "gh binary", Type: adapter.Text, Default: "gh", ShowIf: map[string][]string{"auth": {"gh"}},
				Help: "The gh CLI to read the login from (use the real one if gh on your PATH is a wrapper)."},
			{Key: "token", Label: "Token", Type: adapter.Secret, Required: true, ShowIf: map[string][]string{"auth": {"token"}},
				Help: "A fine-grained token with issues, pull requests, contents, checks and webhooks access."},
			{Key: "appId", Label: "App ID", Type: adapter.Text, Required: true, ShowIf: map[string][]string{"auth": {"app"}}},
			{Key: "appPrivateKey", Label: "App private key (PEM)", Type: adapter.Secret, Required: true, ShowIf: map[string][]string{"auth": {"app"}}},
			{Key: "botAppId", Label: "Bot App ID (optional)", Type: adapter.Text,
				Help: "A GitHub App that posts the agents' comments, labels, reactions and PRs, so people see who did them. Reads still use the sign-in above."},
			{Key: "botPrivateKey", Label: "Bot App private key (PEM)", Type: adapter.Secret},
			{Key: "apiUrl", Label: "API URL", Type: adapter.Text, Default: "https://api.github.com",
				Help: "https://<host>/api/v3 for GitHub Enterprise Server."},
			{Key: "previewPattern", Label: "Deploy preview check", Type: adapter.Text, Default: "deploy-preview",
				Help: "A regex over commit status contexts and check names; a passing one's link is the deploy preview."},
		},
		Binding: map[adapter.Role][]adapter.Field{
			adapter.RoleRepo:    {{Key: "repo", Label: "Repository (owner/name)", Type: adapter.Text, Required: true, Placeholder: "octo/widgets"}},
			adapter.RoleTracker: {{Key: "repo", Label: "Repository (owner/name)", Type: adapter.Text, Required: true, Placeholder: "octo/widgets"}},
		},
		New: func(v adapter.Values, env adapter.Env) (adapter.Connection, error) { return newConn(v, env) },
	})
}

// conn is one GitHub connection.
type conn struct {
	api, web, host string
	env            adapter.Env
	http           *http.Client
	read, write    tokenSource
	preview        *regexp.Regexp
	authMode       string
}

func newConn(v adapter.Values, env adapter.Env) (*conn, error) {
	api := strings.TrimRight(v.Get("apiUrl", "https://api.github.com"), "/")
	u, err := url.Parse(api)
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("apiUrl %q isn't a URL", api)
	}
	c := &conn{api: api, env: env, http: env.HTTP, authMode: v.Get("auth", "gh")}
	if c.http == nil {
		c.http = http.DefaultClient
	}
	if u.Host == "api.github.com" {
		c.web, c.host = "https://github.com", "github.com"
	} else {
		c.web, c.host = u.Scheme+"://"+u.Host, u.Host
	}
	pattern := v.Get("previewPattern", "deploy-preview")
	if c.preview, err = regexp.Compile(pattern); err != nil {
		return nil, fmt.Errorf("previewPattern: %w", err)
	}
	secret := func(key string) (string, error) {
		if env.Secret == nil {
			return v[key], nil
		}
		return env.Secret(v[key])
	}
	switch c.authMode {
	case "gh":
		c.read = &ghToken{host: c.host, bin: v.Get("ghBinary", "gh")}
	case "token":
		tok, err := secret("token")
		if err != nil {
			return nil, fmt.Errorf("token: %w", err)
		}
		if tok == "" {
			return nil, fmt.Errorf("auth is token but no token is set")
		}
		c.read = staticToken(tok)
	case "app":
		pem, err := secret("appPrivateKey")
		if err != nil {
			return nil, fmt.Errorf("appPrivateKey: %w", err)
		}
		app, err := newAppToken(v.Get("appId", ""), pem, c)
		if err != nil {
			return nil, err
		}
		c.read = app
	default:
		return nil, fmt.Errorf("unknown auth %q (gh, token or app)", c.authMode)
	}
	c.write = c.read
	if id := v.Get("botAppId", ""); id != "" {
		pem, err := secret("botPrivateKey")
		if err != nil {
			return nil, fmt.Errorf("botPrivateKey: %w", err)
		}
		bot, err := newAppToken(id, pem, c)
		if err != nil {
			return nil, fmt.Errorf("bot app: %w", err)
		}
		c.write = bot
	}
	return c, nil
}

// call makes one API call as the reader (or, with write, the writer) of repo, signing in again after a 401.
func (c *conn) call(ctx context.Context, write bool, repo, method, path string, body, out any) (*http.Response, error) {
	src := c.read
	if write {
		src = c.write
	}
	var res *http.Response
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		var tok string
		if tok, err = src.token(ctx, repo); err != nil {
			return nil, err
		}
		res, err = c.client("Bearer "+tok).Do(ctx, method, path, body, out)
		if attempt == 0 && adapter.IsStatus(err, http.StatusUnauthorized) {
			src.invalidate(repo)
			continue
		}
		break
	}
	return res, err
}

func (c *conn) client(authorization string) *adapter.JSONClient {
	return &adapter.JSONClient{
		HTTP:    c.http,
		BaseURL: c.api,
		Auth: func(r *http.Request) error {
			r.Header.Set("Authorization", authorization)
			return nil
		},
		Header: http.Header{
			"Accept":               {"application/vnd.github+json"},
			"X-Github-Api-Version": {"2022-11-28"},
			"User-Agent":           {"open-supervisor"},
		},
	}
}

// paginate reads every page of a list endpoint, 100 at a time.
func paginate[T any](ctx context.Context, c *conn, repo, path string) ([]T, error) {
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	var all []T
	for page := 1; page < 50; page++ {
		var items []T
		if _, err := c.call(ctx, false, repo, http.MethodGet, fmt.Sprintf("%s%sper_page=100&page=%d", path, sep, page), nil, &items); err != nil {
			return nil, err
		}
		all = append(all, items...)
		if len(items) < 100 {
			break
		}
	}
	return all, nil
}

// Check says who the connection reads (and writes) as.
func (c *conn) Check(ctx context.Context) (string, error) {
	who, err := c.whoami(ctx, c.read)
	if err != nil {
		return "", err
	}
	msg := "signed in as " + who
	if c.write != c.read {
		bot, err := c.whoami(ctx, c.write)
		if err != nil {
			return "", fmt.Errorf("bot app: %w", err)
		}
		msg += ", writes as " + bot
	}
	return msg, nil
}

func (c *conn) whoami(ctx context.Context, src tokenSource) (string, error) {
	if app, ok := src.(*appToken); ok {
		jwt, err := app.jwt()
		if err != nil {
			return "", err
		}
		var a struct {
			Slug string `json:"slug"`
		}
		if _, err := c.client("Bearer "+jwt).Do(ctx, http.MethodGet, "/app", nil, &a); err != nil {
			return "", err
		}
		return a.Slug + " (app)", nil
	}
	tok, err := src.token(ctx, "")
	if err != nil {
		return "", err
	}
	var u struct {
		Login string `json:"login"`
	}
	if _, err := c.client("Bearer "+tok).Do(ctx, http.MethodGet, "/user", nil, &u); err != nil {
		return "", err
	}
	return u.Login, nil
}

// DetectRepo reads owner/name from an ssh or https remote on this connection's host.
func (c *conn) DetectRepo(remote string) (adapter.Values, bool) {
	re := regexp.MustCompile(`^(?:git@|ssh://git@|https?://(?:[^@/]+@)?)` + regexp.QuoteMeta(c.host) + `[:/]([^/]+/[^/]+?)(?:\.git)?/?$`)
	m := re.FindStringSubmatch(strings.TrimSpace(remote))
	if m == nil {
		return nil, false
	}
	return adapter.Values{"repo": m[1]}, true
}

func (c *conn) bind(v adapter.Values) (*binding, error) {
	repo := strings.Trim(v.Get("repo", ""), "/")
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return nil, fmt.Errorf("repo %q isn't owner/name", repo)
	}
	return &binding{c: c, repo: repo, owner: owner}, nil
}

// Repo binds the connection to a repository.
func (c *conn) Repo(v adapter.Values, _ adapter.ProjectInfo) (adapter.Repo, error) {
	b, err := c.bind(v)
	if err != nil {
		return nil, err
	}
	return &Repo{b}, nil
}

// Tracker binds the connection to a repository's issues.
func (c *conn) Tracker(v adapter.Values, _ adapter.ProjectInfo) (adapter.Tracker, error) {
	b, err := c.bind(v)
	if err != nil {
		return nil, err
	}
	return &Tracker{b}, nil
}
