// Package toolbelt is `open-supervisor agent …`: the commands an agent runs inside its session to act on the
// issue it works, whatever tracker and code host the project uses. The supervisor puts the project, the issue
// and the config in every session's environment (OPEN_SUPERVISOR_PROJECT, OPEN_SUPERVISOR_ISSUE,
// OPEN_SUPERVISOR_CONFIG), so the commands need no arguments to know where they are; run by hand, they work
// out the project from the current checkout and the issue from the current branch.
package toolbelt

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/alsiola/open-supervisor/internal/adapter"
	"github.com/alsiola/open-supervisor/internal/config"
	"github.com/alsiola/open-supervisor/internal/core"
	"github.com/alsiola/open-supervisor/internal/engine"
	"github.com/alsiola/open-supervisor/internal/problems"
	"github.com/alsiola/open-supervisor/internal/worktree"
)

// session is where an agent command runs: the project bound to its connections, and the issue.
type session struct {
	cfg *config.Config
	b   *engine.Bound
	// key is the issue the command acts on; own is the issue the agent works (its comments carry it).
	key, own string
	dryRun   bool
}

func (s *session) branch() string   { return s.b.BranchFor(s.key) }
func (s *session) worktree() string { return worktree.Dir(config.DataDir(), s.b.Spec.Name, s.key) }

// mark is what the agent's comments carry so its own never come back to it.
func (s *session) mark() core.AgentMark { return core.AgentMark{From: s.own} }

type flags struct {
	config, project, issue string
}

func open(f *flags, needIssue bool) (*session, error) {
	path := f.config
	if path == "" {
		path = config.DefaultPath()
	}
	cfg, err := config.Load(path)
	if err != nil {
		return nil, err
	}
	project := f.project
	if project == "" {
		project = os.Getenv("OPEN_SUPERVISOR_PROJECT")
	}
	var spec *config.Project
	if project != "" {
		if spec = cfg.Project(project); spec == nil {
			return nil, fmt.Errorf("no project named %q in %s", project, cfg.Path)
		}
	} else if spec = projectFromCwd(cfg); spec == nil {
		return nil, errors.New("couldn't tell which project this is: run from its checkout or pass --project")
	}
	b, err := bind(cfg, spec)
	if err != nil {
		return nil, err
	}
	s := &session{cfg: cfg, b: b, own: os.Getenv("OPEN_SUPERVISOR_ISSUE"), dryRun: cfg.Settings.DryRun}
	if s.own == "" {
		s.own = b.IssueOfBranch(currentBranch())
	}
	s.key = f.issue
	if s.key == "" {
		s.key = s.own
	}
	s.key = strings.TrimPrefix(s.key, "#")
	if needIssue && s.key == "" {
		return nil, errors.New("couldn't tell which issue this is: run in its session or on its branch, or pass --issue")
	}
	return s, nil
}

// bind opens a project's connections and binds them to it.
func bind(cfg *config.Config, spec *config.Project) (*engine.Bound, error) {
	env := adapter.Env{
		Log:     func(format string, args ...any) { fmt.Fprintf(os.Stderr, format+"\n", args...) },
		Secret:  config.ResolveSecret,
		DryRun:  cfg.Settings.DryRun,
		DataDir: config.DataDir(),
		HTTP:    &http.Client{Timeout: 60 * time.Second},
	}
	b, err := engine.Bind(cfg, spec, engine.OpenConnections(cfg, env), config.StateDir())
	if err != nil {
		return nil, fmt.Errorf("%s: %w", spec.Name, err)
	}
	return b, nil
}

// upstream is the project in the config that is open-supervisor itself, nil when the supervisor doesn't watch it.
func (s *session) upstream() *engine.Bound {
	if engine.IsUpstream(s.b.Repo) {
		return s.b
	}
	for _, p := range s.cfg.Projects {
		if p == s.b.Spec || p.Disabled {
			continue
		}
		if b, err := bind(s.cfg, p); err == nil && engine.IsUpstream(b.Repo) {
			return b
		}
	}
	return nil
}

// tracker is the tracker an issue command acts on: the project's own, or with --upstream open-supervisor's.
func (s *session) tracker(upstream bool) (adapter.Tracker, error) {
	if !upstream {
		return s.b.Tracker, nil
	}
	if up := s.upstream(); up != nil {
		return up.Tracker, nil
	}
	return nil, errors.New("open-supervisor isn't one of the projects here, so its issues can't be read or commented on from here")
}

// projectFromCwd finds the project whose checkout (or one of whose issue worktrees) holds the current directory.
func projectFromCwd(cfg *config.Config) *config.Project {
	cwd, _ := os.Getwd()
	top := gitTop(cwd)
	for _, p := range cfg.Projects {
		root := config.ExpandHome(p.Root)
		if same(root, top) || same(root, cwd) {
			return p
		}
		wt := filepath.Join(config.DataDir(), "worktrees", p.Name) + string(filepath.Separator)
		if strings.HasPrefix(cwd+string(filepath.Separator), wt) {
			return p
		}
	}
	return nil
}

func same(a, b string) bool {
	if a == "" || b == "" {
		return false
	}
	x, err1 := filepath.EvalSymlinks(a)
	y, err2 := filepath.EvalSymlinks(b)
	return err1 == nil && err2 == nil && x == y
}

func gitTop(dir string) string {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--path-format=absolute", "--git-common-dir").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSuffix(strings.TrimSpace(string(out)), string(filepath.Separator)+".git")
}

func currentBranch() string {
	out, err := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// readBody reads a body from a file, or stdin for "-".
func readBody(arg string) (string, error) {
	var data []byte
	var err error
	if arg == "-" {
		data, err = io.ReadAll(os.Stdin)
	} else {
		data, err = os.ReadFile(arg)
	}
	if err != nil {
		return "", err
	}
	body := strings.TrimSpace(string(data))
	if body == "" {
		return "", errors.New("the body is empty")
	}
	return body, nil
}

func timeout() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 2*time.Minute)
}

// Command is `open-supervisor agent`.
func Command() *cobra.Command {
	f := &flags{}
	root := &cobra.Command{
		Use:   "agent",
		Short: "Commands for agents working an issue (comments, PRs, worktrees, waiting)",
		Long: "Commands an agent runs inside its session to act on the issue it works, whatever tracker and code host the project uses.\n" +
			"In a session the supervisor started they need no flags; by hand, run them from the project's checkout or an issue worktree.",
		SilenceUsage: true,
	}
	root.PersistentFlags().StringVar(&f.config, "config", "", "config file (default $OPEN_SUPERVISOR_CONFIG or ~/.config/open-supervisor/config.yaml)")
	root.PersistentFlags().StringVar(&f.project, "project", "", "project name (default $OPEN_SUPERVISOR_PROJECT or the current checkout's)")
	root.PersistentFlags().StringVar(&f.issue, "issue", "", "issue key (default $OPEN_SUPERVISOR_ISSUE or the current branch's)")
	root.AddCommand(contextCmd(f), issueCmd(f), prCmd(f), checksCmd(f), attachmentCmd(f), worktreeCmd(f), waitCmd(f), scheduleCmd(f), correctionCmd(f), problemCmd(f), inflightCmd(f))
	return root
}

func contextCmd(f *flags) *cobra.Command {
	return &cobra.Command{
		Use:   "context",
		Short: "Print the project, issue, branch, worktree and where things are",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := open(f, false)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "project:   %s (%s)\n", s.b.Spec.Name, s.b.Root)
			fmt.Fprintf(out, "tracker:   %s\n", s.b.Tracker.Describe())
			fmt.Fprintf(out, "code:      %s %s\n", s.b.Repo.Describe(), s.b.Repo.WebURL())
			if s.key != "" {
				fmt.Fprintf(out, "issue:     %s %s\n", s.key, s.b.Tracker.IssueURL(s.key))
				fmt.Fprintf(out, "branch:    %s\n", s.branch())
				fmt.Fprintf(out, "worktree:  %s\n", s.worktree())
			}
			base := s.b.Spec.BaseBranch
			if base == "" {
				ctx, cancel := timeout()
				base, _ = s.b.Repo.DefaultBranch(ctx)
				cancel()
			}
			fmt.Fprintf(out, "base:      %s\n", base)
			fmt.Fprintf(out, "remote:    %s\n", s.b.Git.RemoteName())
			fmt.Fprintf(out, "inflight:  %s\n", filepath.Join(s.b.StateDir, "inflight.md"))
			fmt.Fprintf(out, "rules:     %s\n", strings.Join(s.b.Agent.RulesFiles(), "; "))
			return nil
		},
	}
}

func printComments(out io.Writer, comments []core.Comment) {
	for _, c := range comments {
		who := c.Author
		if c.Agent != nil {
			who += " (agent"
			if c.Agent.From != "" {
				who += " on " + c.Agent.From
			}
			who += ")"
		} else if c.Bot {
			who += " (bot)"
		}
		where := string(c.Kind)
		if c.Kind == core.OnInline && c.Path != "" {
			where += " " + c.Path
			if c.Line > 0 {
				where += fmt.Sprintf(":%d", c.Line)
			}
		}
		if c.ReviewState != "" {
			where += " " + strings.ToLower(c.ReviewState)
		}
		fmt.Fprintf(out, "\n--- %s · %s · %s · id %s\n%s\n%s\n", who, c.At.Local().Format("2006-01-02 15:04"), where, c.ID, c.URL, strings.TrimSpace(c.Body))
	}
}

func issueCmd(f *flags) *cobra.Command {
	cmd := &cobra.Command{Use: "issue", Short: "Read, comment on, list or open issues"}
	view := &cobra.Command{
		Use:   "view",
		Short: "Print the issue and every comment on it",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := open(f, true)
			if err != nil {
				return err
			}
			ctx, cancel := timeout()
			defer cancel()
			issue, err := s.b.Tracker.Issue(ctx, s.key)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			state := "open"
			if !issue.Open {
				state = "closed"
			}
			fmt.Fprintf(out, "%s %s\n%s\nstate: %s · author: %s · labels: %s\n\n%s\n", s.key, issue.Title, issue.URL, state, issue.Author, strings.Join(issue.Labels, ", "), strings.TrimSpace(issue.Body))
			comments, err := s.b.Tracker.Comments(ctx, s.key, time.Time{})
			if err != nil {
				return err
			}
			printComments(out, comments)
			return nil
		},
	}
	var upstream bool
	comment := &cobra.Command{
		Use:   "comment <file|->",
		Short: "Post a comment on the issue (or, with --issue, on another agent's issue; with --upstream, on open-supervisor's)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := open(f, true)
			if err != nil {
				return err
			}
			tracker, err := s.tracker(upstream)
			if err != nil {
				return err
			}
			body, err := readBody(args[0])
			if err != nil {
				return err
			}
			if s.dryRun {
				fmt.Fprintln(cmd.OutOrStdout(), "dry run: would comment on", s.key)
				return nil
			}
			ctx, cancel := timeout()
			defer cancel()
			url, err := tracker.Comment(ctx, s.key, body, s.mark())
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), url)
			return nil
		},
	}
	list := &cobra.Command{
		Use:   "list",
		Short: "List the tracker's open issues (with --upstream, open-supervisor's), to check for one before opening another",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := open(f, false)
			if err != nil {
				return err
			}
			tracker, err := s.tracker(upstream)
			if err != nil {
				return err
			}
			ctx, cancel := timeout()
			defer cancel()
			issues, err := tracker.Candidates(ctx)
			if err != nil {
				return err
			}
			for _, i := range issues {
				fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", i.Key, i.Title, strings.Join(i.Labels, ","))
			}
			return nil
		},
	}
	var title, until string
	var waits []string
	create := &cobra.Command{
		Use:   "create --title <title> [--wait <condition>]... [--until <time>] <file|->",
		Short: "Open an issue in the project's tracker; --wait and --until hold it until then (with --upstream, open it on open-supervisor's, or print a link that does)",
		Long: "Open an issue in the project's tracker. With --wait or --until it is held, not worked, until any one of the conditions is met or the time comes; " +
			"see `agent wait --help` for the conditions.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(title) == "" {
				return errors.New("--title is required")
			}
			body, err := readBody(args[0])
			if err != nil {
				return err
			}
			if upstream && (len(waits) > 0 || until != "") {
				return errors.New("--upstream can't hold the issue: leave out --wait and --until")
			}
			s, err := open(f, false)
			if err != nil {
				return err
			}
			tracker := s.b.Tracker
			if upstream {
				up := s.upstream()
				if up == nil {
					fmt.Fprintln(cmd.OutOrStdout(), upstreamIssueURL(title, body))
					return nil
				}
				tracker = up.Tracker
			}
			opener, ok := tracker.(adapter.IssueOpener)
			if !ok {
				return fmt.Errorf("%s can't open issues from here: report the cause instead", tracker.Describe())
			}
			ctx, cancel := timeout()
			defer cancel()
			var labels []string
			held := len(waits) > 0 || until != ""
			var h core.Hold
			if held {
				if h, err = s.holdFor(ctx, waits, until); err != nil {
					return err
				}
				body += "\n\n" + s.b.Tracker.HoldMarker(h)
				labels = []string{s.cfg.LabelsFor(s.b.Spec).Scheduled}
			}
			if s.dryRun {
				fmt.Fprintln(cmd.OutOrStdout(), "dry run: would open", title)
				return nil
			}
			issue, err := opener.OpenIssue(ctx, title, body, labels)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "%s %s\n", issue.Key, issue.URL)
			if held {
				fmt.Fprintf(cmd.OutOrStdout(), "held: it starts once %s\n", h.Describe())
			}
			return nil
		},
	}
	create.Flags().StringVar(&title, "title", "", "the issue's title")
	create.Flags().StringArrayVar(&waits, "wait", nil, "hold the issue until this happens (repeat for any of several)")
	create.Flags().StringVar(&until, "until", "", "hold the issue until this time (an ISO instant), or at most until then with --wait")
	cmd.PersistentFlags().BoolVar(&upstream, "upstream", false, "act on open-supervisor's own tracker; create prints a link that opens the issue when no project here is open-supervisor")
	cmd.AddCommand(view, comment, list, create)
	return cmd
}

// upstreamIssueURL opens a new open-supervisor issue with the title and body filled in. It is a link for the
// owner to follow rather than an issue opened for them, since open-supervisor's tracker is public and the owner
// may not maintain it. When they do (they watch it as one of their projects), --upstream opens the issue there.
func upstreamIssueURL(title, body string) string {
	const limit = 6000 // keep the link under what browsers and GitHub accept
	if len(body) > limit {
		body = strings.ToValidUTF8(body[:limit], "") + "\n\n…"
	}
	q := url.Values{"title": {title}, "body": {body}}
	return "https://github.com/alsiola/open-supervisor/issues/new?" + q.Encode()
}

// issuePRs are the PRs that belong to the issue: from its branch, or referring to it.
func (s *session) issuePRs(ctx context.Context) ([]core.PR, error) {
	found, err := s.b.Repo.PRsFor(ctx, s.branch(), s.b.Tracker.RefFor(s.key))
	if err != nil {
		return nil, err
	}
	var out []core.PR
	for _, pr := range found {
		if pr.Branch == s.branch() || containsFold(s.b.Tracker.RefsIn(pr.Title+"\n"+pr.Body), s.key) {
			out = append(out, pr)
		}
	}
	return out, nil
}

func containsFold(xs []string, x string) bool {
	for _, y := range xs {
		if strings.EqualFold(x, y) {
			return true
		}
	}
	return false
}

func atoi(s string) (int, error) {
	var n int
	_, err := fmt.Sscanf(strings.TrimPrefix(s, "#"), "%d", &n)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q isn't a PR number", s)
	}
	return n, nil
}

func prCmd(f *flags) *cobra.Command {
	cmd := &cobra.Command{Use: "pr", Short: "Open, read and comment on the issue's PRs"}
	list := &cobra.Command{
		Use:   "list",
		Short: "List the issue's PRs, open or not",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := open(f, true)
			if err != nil {
				return err
			}
			ctx, cancel := timeout()
			defer cancel()
			prs, err := s.issuePRs(ctx)
			if err != nil {
				return err
			}
			if len(prs) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "no PRs yet")
			}
			for _, pr := range prs {
				fmt.Fprintf(cmd.OutOrStdout(), "#%d\t%s\t%s\t%s\t%s\n", pr.ID, pr.State, pr.Branch, pr.Title, pr.URL)
			}
			return nil
		},
	}
	view := &cobra.Command{
		Use:   "view <pr>",
		Short: "Print a PR and every comment on it",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := open(f, false)
			if err != nil {
				return err
			}
			id, err := atoi(args[0])
			if err != nil {
				return err
			}
			ctx, cancel := timeout()
			defer cancel()
			pr, err := s.b.Repo.PR(ctx, id)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "#%d %s\n%s\nstate: %s · %s → %s · head %s\n\n%s\n", pr.ID, pr.Title, pr.URL, pr.State, pr.Branch, pr.Base, pr.HeadSHA, strings.TrimSpace(pr.Body))
			comments, err := s.b.Repo.PRComments(ctx, id, time.Time{})
			if err != nil {
				return err
			}
			printComments(out, comments)
			return nil
		},
	}
	var title, bodyFile, base, head string
	var draft bool
	create := &cobra.Command{
		Use:   "create --title <title> --body-file <file|->",
		Short: "Open a PR from the issue's branch, or from --head outside an issue",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := open(f, head == "")
			if err != nil {
				return err
			}
			if title == "" || bodyFile == "" {
				return errors.New("--title and --body-file are required")
			}
			body, err := readBody(bodyFile)
			if err != nil {
				return err
			}
			if head == "" {
				head = s.branch()
				if ref := s.b.Tracker.RefFor(s.key); !strings.Contains(body, ref) {
					body = ref + "\n\n" + body
				}
			}
			ctx, cancel := timeout()
			defer cancel()
			if base == "" {
				base = s.b.Spec.BaseBranch
			}
			if base == "" {
				if base, err = s.b.Repo.DefaultBranch(ctx); err != nil {
					return err
				}
			}
			if s.dryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "dry run: would open %s → %s\n", head, base)
				return nil
			}
			pr, err := s.b.Repo.CreatePR(ctx, adapter.PRRequest{Title: title, Body: body, Head: head, Base: base, Draft: draft})
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "#%d %s\n", pr.ID, pr.URL)
			return nil
		},
	}
	create.Flags().StringVar(&title, "title", "", "the PR's title")
	create.Flags().StringVar(&bodyFile, "body-file", "", "a file with the PR's body, or - for stdin")
	create.Flags().StringVar(&base, "base", "", "the branch to merge into (default the project's base branch)")
	create.Flags().StringVar(&head, "head", "", "the branch to open it from, for a PR outside an issue (a sweep's or triage's)")
	create.Flags().BoolVar(&draft, "draft", false, "open it as a draft")
	comment := &cobra.Command{
		Use:   "comment <pr> <file|->",
		Short: "Comment on a PR's conversation",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := open(f, false)
			if err != nil {
				return err
			}
			id, err := atoi(args[0])
			if err != nil {
				return err
			}
			body, err := readBody(args[1])
			if err != nil {
				return err
			}
			if s.dryRun {
				fmt.Fprintln(cmd.OutOrStdout(), "dry run: would comment on PR", id)
				return nil
			}
			ctx, cancel := timeout()
			defer cancel()
			url, err := s.b.Repo.CommentPR(ctx, id, body, s.mark())
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), url)
			return nil
		},
	}
	reply := &cobra.Command{
		Use:   "reply <pr> <comment-id> <file|->",
		Short: "Reply in an inline review thread",
		Args:  cobra.ExactArgs(3),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := open(f, false)
			if err != nil {
				return err
			}
			id, err := atoi(args[0])
			if err != nil {
				return err
			}
			body, err := readBody(args[2])
			if err != nil {
				return err
			}
			if s.dryRun {
				fmt.Fprintln(cmd.OutOrStdout(), "dry run: would reply on PR", id)
				return nil
			}
			ctx, cancel := timeout()
			defer cancel()
			url, err := s.b.Repo.ReplyInline(ctx, id, args[1], body, s.mark())
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), url)
			return nil
		},
	}
	cmd.AddCommand(list, view, create, comment, reply)
	return cmd
}

func checksCmd(f *flags) *cobra.Command {
	return &cobra.Command{
		Use:   "checks [pr]",
		Short: "Show every check on a PR's head (default the issue's open PR), with links",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := open(f, len(args) == 0)
			if err != nil {
				return err
			}
			ctx, cancel := timeout()
			defer cancel()
			var pr core.PR
			if len(args) == 1 {
				id, err := atoi(args[0])
				if err != nil {
					return err
				}
				if pr, err = s.b.Repo.PR(ctx, id); err != nil {
					return err
				}
			} else {
				prs, err := s.issuePRs(ctx)
				if err != nil {
					return err
				}
				for _, p := range prs {
					if p.State == core.PROpen {
						pr = p
					}
				}
				if pr.ID == 0 {
					return errors.New("the issue has no open PR")
				}
			}
			c, err := s.b.Repo.Checks(ctx, pr.HeadSHA)
			if err != nil {
				return err
			}
			var missing []string
			if rc, ok := s.b.Repo.(adapter.RequiredChecker); ok && pr.Base != "" {
				required, err := rc.RequiredChecks(ctx, pr.Base)
				if err != nil {
					return err
				}
				missing = c.Missing(required)
			}
			out := cmd.OutOrStdout()
			state := "pending"
			switch {
			case !c.Any && len(missing) == 0:
				state = "no checks"
			case c.Complete && len(c.Failed) == 0 && len(missing) == 0:
				state = "passed"
			case c.Complete && len(c.Failed) > 0:
				state = "failed"
			case c.Complete || !c.Any:
				state = "waiting on required checks"
			}
			fmt.Fprintf(out, "PR #%d at %s: %s\n", pr.ID, pr.HeadSHA, state)
			for _, r := range c.Runs {
				fmt.Fprintf(out, "  %-8s %s %s\n", r.State, r.Name, r.URL)
			}
			for _, name := range missing {
				fmt.Fprintf(out, "  %-8s %s (required, not reported)\n", "missing", name)
			}
			if c.Preview != "" {
				fmt.Fprintf(out, "preview: %s\n", c.Preview)
			}
			return nil
		},
	}
}

// attachers are the project's services that hold files for comments: the tracker first, as comments go there.
func (s *session) attachers() []adapter.Attacher {
	var out []adapter.Attacher
	for _, x := range []any{s.b.Tracker, s.b.Repo} {
		if a, ok := x.(adapter.Attacher); ok {
			out = append(out, a)
		}
	}
	return out
}

func attachmentCmd(f *flags) *cobra.Command {
	cmd := &cobra.Command{Use: "attachment", Short: "Upload a file to link from a comment, or download one a comment links to"}
	upload := &cobra.Command{
		Use:   "upload <file>",
		Short: "Upload a file (an image, a zip, a document) for the issue and print a link to put in a comment",
		Long: "Upload a file for the issue and print a link to it. Put it in a comment as ![what it shows](<link>) for an image, " +
			"or [name](<link>) for anything else.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := open(f, true)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(args[0])
			if err != nil {
				return err
			}
			as := s.attachers()
			if len(as) == 0 {
				return errors.New("neither the tracker nor the code host here can hold attachments: link the file from somewhere else")
			}
			if s.dryRun {
				fmt.Fprintln(cmd.OutOrStdout(), "dry run: would upload", args[0])
				return nil
			}
			ctx, cancel := timeout()
			defer cancel()
			link, err := as[0].Attach(ctx, s.key, filepath.Base(args[0]), data)
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), link)
			return nil
		},
	}
	get := &cobra.Command{
		Use:   "get <url> <file>",
		Short: "Download a file a comment links to (an image, an attachment) into <file>, signed in as the project's tracker or code host",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := open(f, false)
			if err != nil {
				return err
			}
			ctx, cancel := timeout()
			defer cancel()
			for _, a := range s.attachers() {
				body, ok, err := a.Fetch(ctx, args[0])
				if !ok {
					continue
				}
				if err != nil {
					return err
				}
				defer body.Close()
				if err := writeFile(args[1], body); err != nil {
					return err
				}
				fmt.Fprintln(cmd.OutOrStdout(), args[1])
				return nil
			}
			where := s.b.Tracker.Describe()
			if repo := s.b.Repo.Describe(); repo != where {
				where += " or " + repo
			}
			return fmt.Errorf("%s isn't a link to %s: fetch it with curl", args[0], where)
		},
	}
	cmd.AddCommand(upload, get)
	return cmd
}

// writeFile writes r to path, making its folder, and leaves nothing behind when the copy fails.
func writeFile(path string, r io.Reader) error {
	if dir := filepath.Dir(path); dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	if _, err = io.Copy(out, r); err == nil {
		err = out.Close()
	} else {
		out.Close()
	}
	if err != nil {
		os.Remove(path)
	}
	return err
}

func (s *session) worktreeSpec(log io.Writer) worktree.Spec {
	base := s.b.Spec.BaseBranch
	if base == "" {
		ctx, cancel := timeout()
		base, _ = s.b.Repo.DefaultBranch(ctx)
		cancel()
	}
	w := s.b.Spec.Worktree
	return worktree.Spec{Root: s.b.Root, Remote: s.b.Git.RemoteName(), Path: s.worktree(), Branch: s.branch(), Base: base, Copy: w.Copy, Setup: w.Setup, SetupWhen: w.SetupWhen, Log: log}
}

func worktreeCmd(f *flags) *cobra.Command {
	cmd := &cobra.Command{Use: "worktree", Short: "Set up or remove the issue's worktree"}
	setup := &cobra.Command{
		Use:   "setup",
		Short: "Create or reuse the issue's worktree on its branch; prints its path last",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := open(f, true)
			if err != nil {
				return err
			}
			path, err := worktree.Setup(s.worktreeSpec(cmd.ErrOrStderr()))
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), path)
			return nil
		},
	}
	var force bool
	remove := &cobra.Command{
		Use:   "remove",
		Short: "Stop anything running from the worktree and remove it (the branch stays); refuses while a PR is open",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := open(f, true)
			if err != nil {
				return err
			}
			if !force {
				ctx, cancel := timeout()
				prs, err := s.issuePRs(ctx)
				cancel()
				if err != nil {
					return err
				}
				for _, pr := range prs {
					if pr.State == core.PROpen {
						return fmt.Errorf("PR #%d for %s is still open; leaving the worktree (--force removes it anyway)", pr.ID, s.key)
					}
				}
			}
			return worktree.Remove(s.worktreeSpec(cmd.ErrOrStderr()))
		},
	}
	remove.Flags().BoolVar(&force, "force", false, "remove it even while a PR is open")
	cmd.AddCommand(setup, remove)
	return cmd
}

func correctionCmd(f *flags) *cobra.Command {
	var area string
	cmd := &cobra.Command{
		Use:   "correction --area <area> <file|->",
		Short: "Log a correction you were given, for the daily sweep to fold into the project's rules",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			s, err := open(f, false)
			if err != nil {
				return err
			}
			body, err := readBody(args[0])
			if err != nil {
				return err
			}
			if area == "" {
				area = "workflow"
			}
			dir := filepath.Join(s.b.StateDir, "corrections")
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return err
			}
			name := time.Now().UTC().Format("20060102T150405.000Z") + ".md"
			text := fmt.Sprintf("issue: %s\narea: %s\n\n%s\n", s.key, area, body)
			file := filepath.Join(dir, name)
			if err := os.WriteFile(file, []byte(text), 0o644); err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), file)
			return nil
		},
	}
	cmd.Flags().StringVar(&area, "area", "", "writing, design, data, code or workflow")
	return cmd
}

func problemCmd(f *flags) *cobra.Command {
	var kind string
	cmd := &cobra.Command{
		Use:   "problem --kind <kind> <file|->",
		Short: "Log a problem you worked around, for the daily triage to fix or file",
		Long: "Log a problem you worked around rather than doing the task the way your instructions say: an unset variable, a missing command, a moved path, a failing `agent` command, a service that needed a retry.\n" +
			"The file says what you tried to do, what went wrong (the exact error), the workaround and where you think the fix belongs.\n" +
			"Kinds: " + strings.Join(problems.Kinds, ", ") + ".",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !problems.ValidKind(kind) {
				return fmt.Errorf("--kind must be one of %s", strings.Join(problems.Kinds, ", "))
			}
			s, err := open(f, false)
			if err != nil {
				return err
			}
			body, err := readBody(args[0])
			if err != nil {
				return err
			}
			file, err := problems.Write(filepath.Join(s.b.StateDir, "problems"), problems.Note{Issue: s.key, Kind: kind, Body: body})
			if err != nil {
				return err
			}
			fmt.Fprintln(cmd.OutOrStdout(), file)
			return nil
		},
	}
	cmd.Flags().StringVar(&kind, "kind", "", strings.Join(problems.Kinds, ", "))
	return cmd
}

func inflightCmd(f *flags) *cobra.Command {
	return &cobra.Command{
		Use:   "inflight",
		Short: "List the project's issues in flight and what each touches",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			s, err := open(f, false)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(filepath.Join(s.b.StateDir, "inflight.md"))
			if errors.Is(err, os.ErrNotExist) || len(strings.TrimSpace(string(data))) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "nothing else in flight")
				return nil
			}
			if err != nil {
				return err
			}
			fmt.Fprint(cmd.OutOrStdout(), string(data))
			return nil
		},
	}
}
