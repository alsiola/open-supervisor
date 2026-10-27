package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/core"
)

// binding is the connection bound to one repository; Tracker and Repo are its two faces.
type binding struct {
	c     *conn
	repo  string
	owner string
	// events are what the hook subscribes to beyond hookEvents, for jobs.
	events []string
}

// Tracker is a repository's issues.
type Tracker struct {
	*binding
	// shared: the issues are worked in other repositories, so references name this one.
	shared bool
}

// Repo is a repository's code and PRs.
type Repo struct{ *binding }

var (
	_ adapter.Tracker     = (*Tracker)(nil)
	_ adapter.Repo        = (*Repo)(nil)
	_ adapter.Reactor     = (*Tracker)(nil)
	_ adapter.IssueOpener = (*Tracker)(nil)
	_ adapter.Reactor     = (*Repo)(nil)
	_ adapter.Hooked      = (*Tracker)(nil)
	_ adapter.Hooked      = (*Repo)(nil)

	_ adapter.EventHooked = (*Tracker)(nil)
	_ adapter.EventHooked = (*Repo)(nil)
)

// ---- API shapes -----------------------------------------------------------------------------------------

type user struct {
	Login string `json:"login"`
	Type  string `json:"type"`
}

type label struct {
	Name string `json:"name"`
}

type rawIssue struct {
	Number      int              `json:"number"`
	Title       string           `json:"title"`
	Body        string           `json:"body"`
	HTMLURL     string           `json:"html_url"`
	User        user             `json:"user"`
	Labels      []label          `json:"labels"`
	State       string           `json:"state"`
	StateReason string           `json:"state_reason"`
	CreatedAt   time.Time        `json:"created_at"`
	UpdatedAt   time.Time        `json:"updated_at"`
	PullRequest *json.RawMessage `json:"pull_request"`
	Milestone   *struct {
		Title string `json:"title"`
	} `json:"milestone"`
	Assignees []user `json:"assignees"`
	Type      *struct {
		Name string `json:"name"`
	} `json:"type"`
}

// issue is the issue with the properties routes can match: repo, milestone, assignees and type.
func (i rawIssue) issue(repo string) core.Issue {
	labels := make([]string, len(i.Labels))
	for n, l := range i.Labels {
		labels[n] = l.Name
	}
	out := core.Issue{
		Key:       strconv.Itoa(i.Number),
		Title:     i.Title,
		Body:      i.Body,
		URL:       i.HTMLURL,
		Author:    i.User.Login,
		Labels:    labels,
		Open:      i.State == "open",
		Reopened:  strings.EqualFold(i.StateReason, "reopened"),
		CreatedAt: i.CreatedAt,
		UpdatedAt: i.UpdatedAt,
	}
	out.SetAttr("repo", repo)
	if i.Milestone != nil {
		out.SetAttr("milestone", i.Milestone.Title)
	}
	for _, a := range i.Assignees {
		out.SetAttr("assignees", a.Login)
	}
	if i.Type != nil {
		out.SetAttr("type", i.Type.Name)
	}
	return out
}

type rawComment struct {
	ID           int64      `json:"id"`
	User         user       `json:"user"`
	Body         string     `json:"body"`
	HTMLURL      string     `json:"html_url"`
	CreatedAt    *time.Time `json:"created_at"`
	UpdatedAt    *time.Time `json:"updated_at"`
	SubmittedAt  *time.Time `json:"submitted_at"`
	Path         string     `json:"path"`
	Line         *int       `json:"line"`
	OriginalLine *int       `json:"original_line"`
	InReplyTo    int64      `json:"in_reply_to_id"`
	State        string     `json:"state"`
}

func (c rawComment) comment(kind core.CommentKind, pr int) core.Comment {
	at := time.Now().UTC()
	for _, t := range []*time.Time{c.SubmittedAt, c.UpdatedAt, c.CreatedAt} {
		if t != nil && !t.IsZero() {
			at = *t
			break
		}
	}
	id := strconv.FormatInt(c.ID, 10)
	if kind == core.OnReview {
		// Reviews number apart from comments; keep the two from colliding in the seen list.
		id = "review-" + id
	}
	out := core.Comment{
		ID:     id,
		Kind:   kind,
		Author: c.User.Login,
		Bot:    c.User.Type == "Bot",
		Body:   c.Body,
		URL:    c.HTMLURL,
		At:     at,
		PR:     pr,
		Path:   c.Path,
		Agent:  agentMark(c.Body),
	}
	if c.Line != nil {
		out.Line = *c.Line
	} else if c.OriginalLine != nil {
		out.Line = *c.OriginalLine
	}
	if c.InReplyTo != 0 {
		out.InReplyTo = strconv.FormatInt(c.InReplyTo, 10)
	}
	if kind == core.OnReview {
		out.ReviewState = c.State
	}
	return out
}

type rawPull struct {
	Number   int        `json:"number"`
	Title    string     `json:"title"`
	State    string     `json:"state"`
	Merged   bool       `json:"merged"`
	MergedAt *time.Time `json:"merged_at"`
	Body     string     `json:"body"`
	HTMLURL  string     `json:"html_url"`
	Head     struct {
		Ref string `json:"ref"`
		SHA string `json:"sha"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (p rawPull) pr() core.PR {
	state := core.PROpen
	switch {
	case p.Merged || p.MergedAt != nil:
		state = core.PRMerged
	case p.State == "closed":
		state = core.PRClosed
	}
	return core.PR{ID: p.Number, Title: p.Title, State: state, Branch: p.Head.Ref, Base: p.Base.Ref, HeadSHA: p.Head.SHA, Body: p.Body, URL: p.HTMLURL}
}

// ---- markers --------------------------------------------------------------------------------------------

var agentRe = regexp.MustCompile(`<!--\s*(?:open-supervisor:agent(?:\s+from=([^\s>]+))?|claude-agent(?:\s+from\s+#(\d+))?)\s*-->`)

// agentMark finds the marker an agent's comment carries: ours, or the old supervisor's.
func agentMark(body string) *core.AgentMark {
	m := agentRe.FindStringSubmatch(body)
	if m == nil {
		return nil
	}
	from := m[1]
	if from == "" {
		from = m[2]
	}
	return &core.AgentMark{From: from}
}

func withMark(body string, mark core.AgentMark) string {
	marker := "<!-- open-supervisor:agent -->"
	if mark.From != "" {
		marker = "<!-- open-supervisor:agent from=" + mark.From + " -->"
	}
	return strings.TrimRight(body, "\n") + "\n\n" + marker + "\n"
}

var refsRe = regexp.MustCompile(`(?i)\b(?:close[sd]?|fix(?:e[sd])?|resolve[sd]?|refs?)\s+([\w.-]+/[\w.-]+)?#(\d+)\b`)

// ---- shared ---------------------------------------------------------------------------------------------

func (b *binding) Describe() string { return "github " + b.repo }

func (b *binding) path(format string, args ...any) string {
	return "/repos/" + b.repo + fmt.Sprintf(format, args...)
}

func number(key string) (int, error) {
	n, err := strconv.Atoi(strings.TrimPrefix(key, "#"))
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q isn't a GitHub issue number", key)
	}
	return n, nil
}

func sinceQuery(since time.Time) string {
	if since.IsZero() {
		return ""
	}
	return "?since=" + url.QueryEscape(since.UTC().Format(time.RFC3339))
}

func after(cs []core.Comment, since time.Time) []core.Comment {
	var out []core.Comment
	for _, c := range cs {
		if since.IsZero() || c.At.After(since) {
			out = append(out, c)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

func (b *binding) comments(ctx context.Context, path string, kind core.CommentKind, pr int) ([]core.Comment, error) {
	raw, err := paginate[rawComment](ctx, b.c, b.repo, path)
	if err != nil {
		return nil, err
	}
	out := make([]core.Comment, 0, len(raw))
	for _, c := range raw {
		out = append(out, c.comment(kind, pr))
	}
	return out, nil
}

func (b *binding) post(ctx context.Context, path, body string) (string, error) {
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	if _, err := b.c.call(ctx, true, b.repo, http.MethodPost, path, map[string]string{"body": body}, &out); err != nil {
		return "", err
	}
	return out.HTMLURL, nil
}

// React puts a 👍 on a comment so people can see it was heard. Reviews have no reactions endpoint.
func (b *binding) React(ctx context.Context, c core.Comment) error {
	if b.c.env.DryRun {
		return nil
	}
	var path string
	switch c.Kind {
	case core.OnIssue, core.OnPR:
		path = b.path("/issues/comments/%s/reactions", c.ID)
	case core.OnInline:
		path = b.path("/pulls/comments/%s/reactions", c.ID)
	default:
		return nil
	}
	_, err := b.c.call(ctx, true, b.repo, http.MethodPost, path, map[string]string{"content": "+1"}, nil)
	return err
}

// ---- tracker --------------------------------------------------------------------------------------------

// Candidates are the repo's open issues (not PRs).
func (t *Tracker) Candidates(ctx context.Context) ([]core.Issue, error) {
	raw, err := paginate[rawIssue](ctx, t.c, t.repo, t.path("/issues?state=open"))
	if err != nil {
		return nil, err
	}
	var out []core.Issue
	for _, i := range raw {
		if i.PullRequest == nil {
			out = append(out, i.issue(t.repo))
		}
	}
	sort.Slice(out, func(i, j int) bool { a, _ := strconv.Atoi(out[i].Key); b, _ := strconv.Atoi(out[j].Key); return a < b })
	return out, nil
}

func (t *Tracker) Issue(ctx context.Context, key string) (core.Issue, error) {
	n, err := number(key)
	if err != nil {
		return core.Issue{}, err
	}
	var i rawIssue
	if _, err := t.c.call(ctx, false, t.repo, http.MethodGet, t.path("/issues/%d", n), nil, &i); err != nil {
		return core.Issue{}, err
	}
	return i.issue(t.repo), nil
}

func (t *Tracker) Comments(ctx context.Context, key string, since time.Time) ([]core.Comment, error) {
	n, err := number(key)
	if err != nil {
		return nil, err
	}
	cs, err := t.comments(ctx, t.path("/issues/%d/comments%s", n, sinceQuery(since)), core.OnIssue, 0)
	if err != nil {
		return nil, err
	}
	return after(cs, since), nil
}

func (t *Tracker) ReopenedAt(ctx context.Context, key string) (time.Time, error) {
	n, err := number(key)
	if err != nil {
		return time.Time{}, err
	}
	events, err := paginate[struct {
		Event     string    `json:"event"`
		CreatedAt time.Time `json:"created_at"`
	}](ctx, t.c, t.repo, t.path("/issues/%d/events", n))
	if err != nil {
		return time.Time{}, err
	}
	var last time.Time
	for _, e := range events {
		if e.Event == "reopened" && e.CreatedAt.After(last) {
			last = e.CreatedAt
		}
	}
	return last, nil
}

func (t *Tracker) EditLabels(ctx context.Context, key string, add, remove []string) error {
	if t.c.env.DryRun {
		return nil
	}
	n, err := number(key)
	if err != nil {
		return err
	}
	if len(add) > 0 {
		if _, err := t.c.call(ctx, true, t.repo, http.MethodPost, t.path("/issues/%d/labels", n), map[string][]string{"labels": add}, nil); err != nil {
			return err
		}
	}
	for _, l := range remove {
		_, err := t.c.call(ctx, true, t.repo, http.MethodDelete, t.path("/issues/%d/labels/%s", n, url.PathEscape(l)), nil, nil)
		if err != nil && !adapter.IsStatus(err, http.StatusNotFound) {
			return err
		}
	}
	return nil
}

// EnsureLabels creates the labels the repo lacks, through the reader (it works before a bot app is installed).
func (t *Tracker) EnsureLabels(ctx context.Context, labels []adapter.Label) error {
	if t.c.env.DryRun {
		return nil
	}
	have, err := paginate[label](ctx, t.c, t.repo, t.path("/labels"))
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, l := range have {
		known[strings.ToLower(l.Name)] = true
	}
	for _, l := range labels {
		if l.Name == "" || known[strings.ToLower(l.Name)] {
			continue
		}
		known[strings.ToLower(l.Name)] = true
		body := map[string]string{"name": l.Name, "color": strings.TrimPrefix(l.Color, "#"), "description": l.Description}
		if _, err := t.c.call(ctx, false, t.repo, http.MethodPost, t.path("/labels"), body, nil); err != nil && !adapter.IsStatus(err, http.StatusUnprocessableEntity) {
			return err
		}
	}
	return nil
}

func (t *Tracker) Comment(ctx context.Context, key, body string, mark core.AgentMark) (string, error) {
	n, err := number(key)
	if err != nil {
		return "", err
	}
	return t.post(ctx, t.path("/issues/%d/comments", n), withMark(body, mark))
}

func (t *Tracker) OpenIssue(ctx context.Context, title, body string, labels []string) (core.Issue, error) {
	var i rawIssue
	req := map[string]any{"title": title, "body": body}
	if len(labels) > 0 {
		req["labels"] = labels
	}
	if _, err := t.c.call(ctx, true, t.repo, http.MethodPost, t.path("/issues"), req, &i); err != nil {
		return core.Issue{}, err
	}
	return i.issue(t.repo), nil
}

func (t *Tracker) IssueURL(key string) string {
	return fmt.Sprintf("%s/%s/issues/%s", t.c.web, t.repo, strings.TrimPrefix(key, "#"))
}

// RefsIn finds "Closes #12", "fixes #3", "Refs #9" and the like; a shared tracker only "Closes owner/repo#12",
// since a bare #12 in another repository's PR is that repository's issue.
func (t *Tracker) RefsIn(text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, m := range refsRe.FindAllStringSubmatch(text, -1) {
		if m[1] == "" && t.shared || m[1] != "" && !strings.EqualFold(m[1], t.repo) {
			continue
		}
		if !seen[m[2]] {
			seen[m[2]] = true
			out = append(out, m[2])
		}
	}
	return out
}

func (t *Tracker) RefFor(key string, closes bool) string {
	word := "Closes "
	if !closes {
		word = "Refs "
	}
	if t.shared {
		return word + t.repo + "#" + strings.TrimPrefix(key, "#")
	}
	return word + "#" + strings.TrimPrefix(key, "#")
}

func (t *Tracker) HoldMarker(h core.Hold) string {
	return "<!-- " + h.Spec() + " -->"
}

// ---- repo -----------------------------------------------------------------------------------------------

func (r *Repo) WebURL() string { return r.c.web + "/" + r.repo }

func (r *Repo) DefaultBranch(ctx context.Context) (string, error) {
	var out struct {
		DefaultBranch string `json:"default_branch"`
	}
	if _, err := r.c.call(ctx, false, r.repo, http.MethodGet, "/repos/"+r.repo, nil, &out); err != nil {
		return "", err
	}
	return out.DefaultBranch, nil
}

// issueRefRe reads the issue a reference names: "Closes #12" or, from a shared tracker, "Closes owner/repo#12".
var issueRefRe = regexp.MustCompile(`(?:([\w.-]+/[\w.-]+))?#(\d+)`)

// rawTimelineEvent is the part of an issue's timeline event that says another issue or PR mentioned it.
type rawTimelineEvent struct {
	Event  string `json:"event"`
	Source struct {
		Issue struct {
			Number      int       `json:"number"`
			PullRequest *struct{} `json:"pull_request"`
			Repository  struct {
				FullName string `json:"full_name"`
			} `json:"repository"`
		} `json:"issue"`
	} `json:"source"`
}

// PRsFor finds the PRs from the branch and those in this repository that mention the issue, open or not.
// The mentions come from the issue's timeline rather than the search API, whose rate limit is far tighter.
func (r *Repo) PRsFor(ctx context.Context, branch, ref string) ([]core.PR, error) {
	found := map[int]core.PR{}
	if branch != "" {
		pulls, err := paginate[rawPull](ctx, r.c, r.repo, r.path("/pulls?state=all&head=%s", url.QueryEscape(r.owner+":"+branch)))
		if err != nil {
			return nil, err
		}
		for _, p := range pulls {
			found[p.Number] = p.pr()
		}
	}
	if m := issueRefRe.FindStringSubmatch(ref); m != nil {
		tracker := r.repo
		if m[1] != "" {
			tracker = m[1]
		}
		events, err := paginate[rawTimelineEvent](ctx, r.c, tracker, "/repos/"+tracker+"/issues/"+m[2]+"/timeline")
		if err != nil {
			return nil, err
		}
		for _, e := range events {
			src := e.Source.Issue
			if e.Event != "cross-referenced" || src.PullRequest == nil || !strings.EqualFold(src.Repository.FullName, r.repo) {
				continue
			}
			if _, ok := found[src.Number]; ok {
				continue
			}
			pr, err := r.PR(ctx, src.Number)
			if err != nil {
				return nil, err
			}
			found[src.Number] = pr
		}
	}
	out := make([]core.PR, 0, len(found))
	for _, p := range found {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func (r *Repo) PR(ctx context.Context, id int) (core.PR, error) {
	var p rawPull
	if _, err := r.c.call(ctx, false, r.repo, http.MethodGet, r.path("/pulls/%d", id), nil, &p); err != nil {
		return core.PR{}, err
	}
	return p.pr(), nil
}

// PRComments are a PR's conversation, its inline threads and its reviews (with a body) newer than since.
func (r *Repo) PRComments(ctx context.Context, id int, since time.Time) ([]core.Comment, error) {
	conv, err := r.comments(ctx, r.path("/issues/%d/comments%s", id, sinceQuery(since)), core.OnPR, id)
	if err != nil {
		return nil, err
	}
	inline, err := r.comments(ctx, r.path("/pulls/%d/comments%s", id, sinceQuery(since)), core.OnInline, id)
	if err != nil {
		return nil, err
	}
	raw, err := paginate[rawComment](ctx, r.c, r.repo, r.path("/pulls/%d/reviews", id))
	if err != nil {
		return nil, err
	}
	all := append(conv, inline...)
	for _, c := range raw {
		if strings.TrimSpace(c.Body) != "" && c.State != "PENDING" {
			all = append(all, c.comment(core.OnReview, id))
		}
	}
	return after(all, since), nil
}

var badConclusions = map[string]bool{"failure": true, "timed_out": true, "cancelled": true, "action_required": true}

// Checks combines a commit's check runs and statuses, with the deploy preview's link once it has passed.
func (r *Repo) Checks(ctx context.Context, sha string) (core.Checks, error) {
	var runs struct {
		CheckRuns []checkRun `json:"check_runs"`
	}
	if _, err := r.c.call(ctx, false, r.repo, http.MethodGet, r.path("/commits/%s/check-runs?per_page=100", sha), nil, &runs); err != nil {
		return core.Checks{}, err
	}
	var status struct {
		Statuses []commitStatus `json:"statuses"`
	}
	if _, err := r.c.call(ctx, false, r.repo, http.MethodGet, r.path("/commits/%s/status", sha), nil, &status); err != nil {
		return core.Checks{}, err
	}
	return r.evaluate(runs.CheckRuns, status.Statuses), nil
}

type checkRun struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	HTMLURL    string `json:"html_url"`
	DetailsURL string `json:"details_url"`
}

type commitStatus struct {
	Context   string `json:"context"`
	State     string `json:"state"`
	TargetURL string `json:"target_url"`
}

func (r *Repo) evaluate(runs []checkRun, statuses []commitStatus) core.Checks {
	out := core.Checks{Complete: true}
	failed := map[string]bool{}
	for _, run := range runs {
		state := "success"
		switch {
		case run.Status != "completed":
			state = "pending"
			out.Complete = false
		case badConclusions[run.Conclusion]:
			state = "failure"
			if !failed[run.Name] {
				failed[run.Name] = true
				out.Failed = append(out.Failed, run.Name)
			}
		case run.Conclusion == "success" && run.DetailsURL != "" && r.c.preview.MatchString(run.Name):
			out.Preview = run.DetailsURL
		}
		link := run.HTMLURL
		if link == "" {
			link = run.DetailsURL
		}
		out.Runs = append(out.Runs, core.CheckRun{Name: run.Name, State: state, URL: link})
	}
	for _, s := range statuses {
		state := "success"
		switch s.State {
		case "pending":
			state = "pending"
			out.Complete = false
		case "failure", "error":
			state = "failure"
			if !failed[s.Context] {
				failed[s.Context] = true
				out.Failed = append(out.Failed, s.Context)
			}
		case "success":
			if s.TargetURL != "" && r.c.preview.MatchString(s.Context) {
				out.Preview = s.TargetURL
			}
		}
		out.Runs = append(out.Runs, core.CheckRun{Name: s.Context, State: state, URL: s.TargetURL})
	}
	out.Any = len(runs)+len(statuses) > 0
	out.Complete = out.Complete && out.Any
	return out
}

// RequiredChecks are the status checks the branch's protection and its rulesets require before a merge.
func (r *Repo) RequiredChecks(ctx context.Context, branch string) ([]string, error) {
	segs := strings.Split(branch, "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	name := strings.Join(segs, "/")
	type check struct {
		Context string `json:"context"`
	}
	var b struct {
		Protection struct {
			RequiredStatusChecks struct {
				Contexts []string `json:"contexts"`
				Checks   []check  `json:"checks"`
			} `json:"required_status_checks"`
		} `json:"protection"`
	}
	if _, err := r.c.call(ctx, false, r.repo, http.MethodGet, r.path("/branches/%s", name), nil, &b); err != nil && !adapter.IsStatus(err, http.StatusNotFound) {
		return nil, err
	}
	var rules []struct {
		Type       string `json:"type"`
		Parameters struct {
			RequiredStatusChecks []check `json:"required_status_checks"`
		} `json:"parameters"`
	}
	// Rulesets aren't on every plan (a private repo on GitHub Free answers 403): then there are none to read.
	if _, err := r.c.call(ctx, false, r.repo, http.MethodGet, r.path("/rules/branches/%s", name), nil, &rules); err != nil &&
		!adapter.IsStatus(err, http.StatusForbidden) && !adapter.IsStatus(err, http.StatusNotFound) {
		return nil, err
	}
	var out []string
	seen := map[string]bool{}
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	rsc := b.Protection.RequiredStatusChecks
	for _, c := range rsc.Contexts {
		add(c)
	}
	for _, c := range rsc.Checks {
		add(c.Context)
	}
	for _, rule := range rules {
		if rule.Type == "required_status_checks" {
			for _, c := range rule.Parameters.RequiredStatusChecks {
				add(c.Context)
			}
		}
	}
	return out, nil
}

func (r *Repo) CreatePR(ctx context.Context, req adapter.PRRequest) (core.PR, error) {
	base := req.Base
	if base == "" {
		var err error
		if base, err = r.DefaultBranch(ctx); err != nil {
			return core.PR{}, err
		}
	}
	body := map[string]any{"title": req.Title, "body": req.Body, "head": req.Head, "base": base, "draft": req.Draft}
	var p rawPull
	if _, err := r.c.call(ctx, true, r.repo, http.MethodPost, r.path("/pulls"), body, &p); err != nil {
		return core.PR{}, err
	}
	return p.pr(), nil
}

func (r *Repo) EditPR(ctx context.Context, id int, title, body string) (core.PR, error) {
	var p rawPull
	if _, err := r.c.call(ctx, true, r.repo, http.MethodPatch, r.path("/pulls/%d", id), map[string]any{"title": title, "body": body}, &p); err != nil {
		return core.PR{}, err
	}
	return p.pr(), nil
}

func (r *Repo) CommentPR(ctx context.Context, id int, body string, mark core.AgentMark) (string, error) {
	return r.post(ctx, r.path("/issues/%d/comments", id), withMark(body, mark))
}

func (r *Repo) ReplyInline(ctx context.Context, id int, commentID, body string, mark core.AgentMark) (string, error) {
	return r.post(ctx, r.path("/pulls/%d/comments/%s/replies", id, url.PathEscape(commentID)), withMark(body, mark))
}
