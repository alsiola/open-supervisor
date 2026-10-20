package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/core"
)

// hookEvents are what the supervisor's hook subscribes to.
var hookEvents = []string{"issues", "issue_comment", "pull_request", "pull_request_review", "pull_request_review_comment", "check_suite", "check_run", "status"}

// HookTarget is the repository: the tracker and the repo bound to it share one hook.
func (b *binding) HookTarget() string { return "github:" + b.repo }

// EnsureWebhook points the supervisor's hook (created on first use) at url. Hooks need admin rights, so this
// goes through the reader.
func (b *binding) EnsureWebhook(ctx context.Context, url, secret, existing string) (string, error) {
	hooks, err := paginate[struct {
		ID int64 `json:"id"`
	}](ctx, b.c, b.repo, b.path("/hooks"))
	if err != nil {
		return "", err
	}
	body := map[string]any{
		"active": true,
		"events": hookEvents,
		"config": map[string]string{"url": url, "content_type": "json", "secret": secret, "insecure_ssl": "0"},
	}
	for _, h := range hooks {
		id := strconv.FormatInt(h.ID, 10)
		if existing == "" || id != existing {
			continue
		}
		if b.c.env.DryRun {
			return id, nil
		}
		if _, err := b.c.call(ctx, false, b.repo, http.MethodPatch, b.path("/hooks/%s", id), body, nil); err != nil {
			return "", err
		}
		return id, nil
	}
	if b.c.env.DryRun {
		return "0", nil
	}
	body["name"] = "web"
	var created struct {
		ID int64 `json:"id"`
	}
	if _, err := b.c.call(ctx, false, b.repo, http.MethodPost, b.path("/hooks"), body, &created); err != nil {
		return "", err
	}
	return strconv.FormatInt(created.ID, 10), nil
}

type payload struct {
	Action     string `json:"action"`
	Repository *struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
	Issue       *rawIssue   `json:"issue"`
	Comment     *rawComment `json:"comment"`
	Review      *rawComment `json:"review"`
	PullRequest *rawPull    `json:"pull_request"`
	CheckSuite  *struct {
		HeadSHA string `json:"head_sha"`
	} `json:"check_suite"`
	CheckRun *struct {
		HeadSHA string `json:"head_sha"`
	} `json:"check_run"`
	SHA   string `json:"sha"`
	State string `json:"state"`
}

// decode checks a delivery's signature and reads it, returning nil for a delivery about another repository.
func (b *binding) decode(h http.Header, body []byte, secret string) (string, *payload, error) {
	if !adapter.VerifyHMAC(secret, body, h.Get("X-Hub-Signature-256")) {
		return "", nil, errors.New("github: a delivery with a bad signature")
	}
	var p payload
	if err := json.Unmarshal(body, &p); err != nil {
		return "", nil, fmt.Errorf("github: a delivery that isn't JSON: %w", err)
	}
	if p.Repository == nil || !strings.EqualFold(p.Repository.FullName, b.repo) {
		return "", nil, nil
	}
	return h.Get("X-GitHub-Event"), &p, nil
}

// ParseWebhook turns a delivery into issue signals; the repo takes the PR and check ones.
func (t *Tracker) ParseWebhook(h http.Header, body []byte, secret string) ([]core.Signal, error) {
	event, p, err := t.decode(h, body, secret)
	if err != nil || p == nil {
		return nil, err
	}
	switch event {
	case "ping":
		return []core.Signal{{Type: core.SigPing, Key: "github " + t.repo}}, nil
	case "issues":
		if p.Issue == nil || p.Issue.PullRequest != nil {
			return nil, nil
		}
		issue := p.Issue.issue()
		switch p.Action {
		case "opened", "labeled", "unlabeled":
			return []core.Signal{{Type: core.SigIssueChanged, Key: issue.Key, Issue: &issue}}, nil
		case "closed":
			return []core.Signal{{Type: core.SigIssueClosed, Key: issue.Key}}, nil
		case "reopened":
			return []core.Signal{{Type: core.SigIssueReopened, Key: issue.Key, Issue: &issue}}, nil
		}
	case "issue_comment":
		if p.Action != "created" || p.Issue == nil || p.Comment == nil || p.Issue.PullRequest != nil {
			return nil, nil
		}
		c := p.Comment.comment(core.OnIssue, 0)
		return []core.Signal{{Type: core.SigIssueComment, Key: strconv.Itoa(p.Issue.Number), Comment: &c}}, nil
	}
	return nil, nil
}

// ParseWebhook turns a delivery into PR and check signals; the tracker takes the issue ones.
func (r *Repo) ParseWebhook(h http.Header, body []byte, secret string) ([]core.Signal, error) {
	event, p, err := r.decode(h, body, secret)
	if err != nil || p == nil {
		return nil, err
	}
	switch event {
	case "pull_request":
		if p.PullRequest == nil {
			return nil, nil
		}
		pr := p.PullRequest.pr()
		switch p.Action {
		case "opened", "reopened", "synchronize":
			return []core.Signal{{Type: core.SigPRChanged, PR: &pr}}, nil
		case "closed":
			if pr.State == core.PROpen {
				pr.State = core.PRClosed
			}
			return []core.Signal{{Type: core.SigPRClosed, PR: &pr}}, nil
		}
	case "issue_comment":
		if p.Action != "created" || p.Issue == nil || p.Comment == nil || p.Issue.PullRequest == nil {
			return nil, nil
		}
		pr := core.PR{ID: p.Issue.Number, Title: p.Issue.Title, Body: p.Issue.Body, URL: p.Issue.HTMLURL, State: core.PROpen}
		if p.Issue.State == "closed" {
			pr.State = core.PRClosed
		}
		c := p.Comment.comment(core.OnPR, pr.ID)
		return []core.Signal{{Type: core.SigPRComment, PR: &pr, Comment: &c}}, nil
	case "pull_request_review_comment":
		if p.Action != "created" || p.PullRequest == nil || p.Comment == nil {
			return nil, nil
		}
		pr := p.PullRequest.pr()
		c := p.Comment.comment(core.OnInline, pr.ID)
		return []core.Signal{{Type: core.SigPRComment, PR: &pr, Comment: &c}}, nil
	case "pull_request_review":
		if p.Action != "submitted" || p.PullRequest == nil || p.Review == nil || strings.TrimSpace(p.Review.Body) == "" {
			return nil, nil
		}
		pr := p.PullRequest.pr()
		c := p.Review.comment(core.OnReview, pr.ID)
		return []core.Signal{{Type: core.SigPRComment, PR: &pr, Comment: &c}}, nil
	case "check_suite":
		if p.Action == "completed" && p.CheckSuite != nil && p.CheckSuite.HeadSHA != "" {
			return []core.Signal{{Type: core.SigChecks, SHA: p.CheckSuite.HeadSHA}}, nil
		}
	case "check_run":
		if p.Action == "completed" && p.CheckRun != nil && p.CheckRun.HeadSHA != "" {
			return []core.Signal{{Type: core.SigChecks, SHA: p.CheckRun.HeadSHA}}, nil
		}
	case "status":
		if p.SHA != "" && p.State != "pending" {
			return []core.Signal{{Type: core.SigChecks, SHA: p.SHA}}, nil
		}
	}
	return nil, nil
}
