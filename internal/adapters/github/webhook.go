package github

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/core"
)

// hookEvents are what the supervisor's hook subscribes to.
var hookEvents = []string{"issues", "issue_comment", "pull_request", "pull_request_review", "pull_request_review_comment", "check_suite", "check_run", "status"}

// WantEvents adds events for jobs to the hook's subscription.
func (b *binding) WantEvents(names []string) {
	for _, n := range names {
		if !slices.Contains(b.events, n) && !slices.Contains(hookEvents, n) {
			b.events = append(b.events, n)
		}
	}
}

// subscribed is every event the hook takes: the supervisor's own, then the jobs'.
func (b *binding) subscribed() []string {
	return append(append([]string(nil), hookEvents...), b.events...)
}

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
		"events": b.subscribed(),
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
	SHA     string `json:"sha"`
	State   string `json:"state"`
	Ref     string `json:"ref"`
	RefType string `json:"ref_type"`
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

// hookEvent is the delivery as a job sees it. The tracker and the repo both send it; the engine keeps one.
func hookEvent(event string, p *payload, body []byte) core.Signal {
	ref := p.Ref
	if ref != "" && !strings.HasPrefix(ref, "refs/") {
		// create and delete carry a bare name with ref_type; a push carries the full ref.
		if p.RefType == "tag" {
			ref = "refs/tags/" + ref
		} else {
			ref = "refs/heads/" + ref
		}
	}
	return core.Signal{Type: core.SigEvent, Event: &core.HookEvent{Name: event, Action: p.Action, Ref: ref, Body: body}}
}

// ParseWebhook turns a delivery into issue signals; the repo takes the PR and check ones.
func (t *Tracker) ParseWebhook(h http.Header, body []byte, secret string) ([]core.Signal, error) {
	event, p, err := t.decode(h, body, secret)
	if err != nil || p == nil {
		return nil, err
	}
	sigs := t.signals(event, p)
	if event != "ping" {
		sigs = append(sigs, hookEvent(event, p, body))
	}
	return sigs, nil
}

func (t *Tracker) signals(event string, p *payload) []core.Signal {
	switch event {
	case "ping":
		return []core.Signal{{Type: core.SigPing, Key: "github " + t.repo}}
	case "issues":
		if p.Issue == nil || p.Issue.PullRequest != nil {
			return nil
		}
		issue := p.Issue.issue(t.repo)
		switch p.Action {
		// Anything a route might match on can change where an issue goes.
		case "opened", "labeled", "unlabeled", "edited", "milestoned", "demilestoned", "assigned", "unassigned", "typed", "untyped":
			return []core.Signal{{Type: core.SigIssueChanged, Key: issue.Key, Issue: &issue}}
		case "closed":
			return []core.Signal{{Type: core.SigIssueClosed, Key: issue.Key}}
		case "reopened":
			return []core.Signal{{Type: core.SigIssueReopened, Key: issue.Key, Issue: &issue}}
		}
	case "issue_comment":
		if p.Action != "created" || p.Issue == nil || p.Comment == nil || p.Issue.PullRequest != nil {
			return nil
		}
		c := p.Comment.comment(core.OnIssue, 0)
		return []core.Signal{{Type: core.SigIssueComment, Key: strconv.Itoa(p.Issue.Number), Comment: &c}}
	}
	return nil
}

// ParseWebhook turns a delivery into PR and check signals; the tracker takes the issue ones.
func (r *Repo) ParseWebhook(h http.Header, body []byte, secret string) ([]core.Signal, error) {
	event, p, err := r.decode(h, body, secret)
	if err != nil || p == nil {
		return nil, err
	}
	sigs := r.signals(event, p)
	if event != "ping" {
		sigs = append(sigs, hookEvent(event, p, body))
	}
	return sigs, nil
}

func (r *Repo) signals(event string, p *payload) []core.Signal {
	switch event {
	case "pull_request":
		if p.PullRequest == nil {
			return nil
		}
		pr := p.PullRequest.pr()
		switch p.Action {
		case "opened", "reopened", "synchronize":
			return []core.Signal{{Type: core.SigPRChanged, PR: &pr}}
		case "closed":
			if pr.State == core.PROpen {
				pr.State = core.PRClosed
			}
			return []core.Signal{{Type: core.SigPRClosed, PR: &pr}}
		}
	case "issue_comment":
		if p.Action != "created" || p.Issue == nil || p.Comment == nil || p.Issue.PullRequest == nil {
			return nil
		}
		pr := core.PR{ID: p.Issue.Number, Title: p.Issue.Title, Body: p.Issue.Body, URL: p.Issue.HTMLURL, State: core.PROpen}
		if p.Issue.State == "closed" {
			pr.State = core.PRClosed
		}
		c := p.Comment.comment(core.OnPR, pr.ID)
		return []core.Signal{{Type: core.SigPRComment, PR: &pr, Comment: &c}}
	case "pull_request_review_comment":
		if p.Action != "created" || p.PullRequest == nil || p.Comment == nil {
			return nil
		}
		pr := p.PullRequest.pr()
		c := p.Comment.comment(core.OnInline, pr.ID)
		return []core.Signal{{Type: core.SigPRComment, PR: &pr, Comment: &c}}
	case "pull_request_review":
		if p.Action != "submitted" || p.PullRequest == nil || p.Review == nil || strings.TrimSpace(p.Review.Body) == "" {
			return nil
		}
		pr := p.PullRequest.pr()
		c := p.Review.comment(core.OnReview, pr.ID)
		return []core.Signal{{Type: core.SigPRComment, PR: &pr, Comment: &c}}
	case "check_suite":
		if p.Action == "completed" && p.CheckSuite != nil && p.CheckSuite.HeadSHA != "" {
			return []core.Signal{{Type: core.SigChecks, SHA: p.CheckSuite.HeadSHA}}
		}
	case "check_run":
		if p.Action == "completed" && p.CheckRun != nil && p.CheckRun.HeadSHA != "" {
			return []core.Signal{{Type: core.SigChecks, SHA: p.CheckRun.HeadSHA}}
		}
	case "status":
		if p.SHA != "" && p.State != "pending" {
			return []core.Signal{{Type: core.SigChecks, SHA: p.SHA}}
		}
	}
	return nil
}
