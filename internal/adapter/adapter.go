// Package adapter is the contract between the supervisor and the services it works with. Every third party
// sits behind one of these interfaces, and each implementation registers a Spec saying what it needs to be
// configured; the onboarding wizard and the TUI's config editor build their forms from those specs, so a new
// adapter needs no UI code.
//
// A connection is one configured account on a service (a GitHub login, a Jira site, a PostHog project, the
// Claude Code CLI). One connection can fill several roles: a GitHub connection is both a code host and an issue
// tracker. A project binds each role it needs to a connection, with options of its own (the repo's name, the
// Jira project key).
package adapter

import (
	"context"
	"io"
	"net/http"
	"os/exec"
	"time"

	"github.com/alsiola/open-supervisor/internal/core"
)

// Role is a part a connection can play.
type Role string

const (
	RoleAgent     Role = "agent"     // runs turns: Claude Code, Codex, an API loop
	RoleRepo      Role = "repo"      // hosts code and PRs: GitHub, Bitbucket
	RoleTracker   Role = "tracker"   // holds issues: GitHub, Jira, Linear
	RoleTelemetry Role = "telemetry" // receives metrics: PostHog, OpenTelemetry, Datadog
	RoleTunnel    Role = "tunnel"    // exposes the webhook port: ngrok, cloudflared
)

// Roles in the order the wizard and the config editor show them.
var Roles = []Role{RoleAgent, RoleRepo, RoleTracker, RoleTelemetry, RoleTunnel}

// Env is what the supervisor lends an adapter.
type Env struct {
	// Log writes a line to the supervisor's log.
	Log func(format string, args ...any)
	// Secret resolves a secret reference from the config ("keyring:github.token", "env:GITHUB_TOKEN", a literal).
	Secret func(ref string) (string, error)
	// DryRun: change nothing outside this machine.
	DryRun bool
	// DataDir is the supervisor's data directory, for anything an adapter writes out (a plugin dir, a cache).
	DataDir string
	// HTTP is the client to use for API calls.
	HTTP *http.Client
}

// Connection is a configured account on a service. Every connection can say whether it works.
type Connection interface {
	// Check proves the connection works, with a short description of who it is connected as.
	Check(ctx context.Context) (string, error)
}

// ProjectInfo is what an adapter is told about the project it is bound to.
type ProjectInfo struct {
	Name string
	// Root is the project's main checkout.
	Root string
	// Remote is the checkout's origin URL, when it has one.
	Remote string
	// Shared: a tracker bound for a source that feeds several projects, so PRs in other repositories refer to
	// its issues (GitHub then writes owner/repo#12, not #12). Name is the source's; Root and Remote are empty.
	Shared bool
}

// RepoProvider is a connection that can host code.
type RepoProvider interface {
	Connection
	// Repo binds the connection to one repository.
	Repo(binding Values, p ProjectInfo) (Repo, error)
	// DetectRepo fills the binding from a checkout's remote URL, when the remote is on this service.
	DetectRepo(remote string) (Values, bool)
}

// TrackerProvider is a connection that can hold issues.
type TrackerProvider interface {
	Connection
	// Tracker binds the connection to one project's issues.
	Tracker(binding Values, p ProjectInfo) (Tracker, error)
}

// Tracker is one project's issues.
type Tracker interface {
	// Describe names what is tracked, for the log and the TUI ("github Oddsworth/pre-crict", "jira PROJ").
	Describe() string
	// Candidates are the open issues the supervisor might claim; the engine filters them by label.
	Candidates(ctx context.Context) ([]core.Issue, error)
	// Issue reads one issue, its body included.
	Issue(ctx context.Context, key string) (core.Issue, error)
	// Comments on the issue newer than since (all of them for the zero time), bots and agents included.
	Comments(ctx context.Context, key string, since time.Time) ([]core.Comment, error)
	// ReopenedAt is when the issue was last reopened, zero if never.
	ReopenedAt(ctx context.Context, key string) (time.Time, error)
	// EditLabels adds and removes labels in one go.
	EditLabels(ctx context.Context, key string, add, remove []string) error
	// EnsureLabels creates any of the labels the tracker lacks, where labels must exist before use.
	EnsureLabels(ctx context.Context, labels []Label) error
	// Comment posts on the issue, marked as an agent's so it is never handed back as news.
	Comment(ctx context.Context, key, body string, mark core.AgentMark) (string, error)
	// IssueURL is the issue's page.
	IssueURL(key string) string
	// RefsIn returns the issue keys a PR's title or body refers to ("Closes #12", "PROJ-12").
	RefsIn(text string) []string
	// RefFor is how a PR body should refer to the issue so the tracker links (and maybe closes) it.
	RefFor(key string) string
	// HoldMarker is a marker for the hold (see core.HoldIn), in the form this tracker's comments and bodies
	// can carry: hidden where they can hide things.
	HoldMarker(h core.Hold) string
}

// Label is one label the supervisor sets.
type Label struct {
	Name        string
	Color       string
	Description string
}

// Repo is one repository on a code host.
type Repo interface {
	Describe() string
	// WebURL is the repository's page.
	WebURL() string
	// DefaultBranch is the branch PRs target.
	DefaultBranch(ctx context.Context) (string, error)
	// PRsFor finds PRs from the branch, or whose title or body mention ref, open or not.
	PRsFor(ctx context.Context, branch, ref string) ([]core.PR, error)
	PR(ctx context.Context, id int) (core.PR, error)
	// PRComments are the comments on a PR newer than since: its conversation, inline threads and reviews.
	PRComments(ctx context.Context, id int, since time.Time) ([]core.Comment, error)
	// Checks is the combined state of the checks on a commit, with the deploy preview's URL once it is up.
	Checks(ctx context.Context, sha string) (core.Checks, error)
	CreatePR(ctx context.Context, req PRRequest) (core.PR, error)
	// EditPR replaces a PR's title and body.
	EditPR(ctx context.Context, id int, title, body string) (core.PR, error)
	// CommentPR posts on a PR's conversation, marked as an agent's.
	CommentPR(ctx context.Context, id int, body string, mark core.AgentMark) (string, error)
	// ReplyInline answers in an inline review thread.
	ReplyInline(ctx context.Context, id int, commentID, body string, mark core.AgentMark) (string, error)
}

// RequiredChecker is a repo whose branches can require checks before a merge (branch protection, rulesets), so
// a required check that never reports isn't taken for one that passed.
type RequiredChecker interface {
	// RequiredChecks are the names of the checks a PR into branch needs.
	RequiredChecks(ctx context.Context, branch string) ([]string, error)
}

// PRRequest is a PR to open.
type PRRequest struct {
	Title, Body, Head, Base string
	Draft                   bool
}

// Reactor is a tracker or repo that can acknowledge a comment (GitHub's 👍), so people see it was heard.
type Reactor interface {
	React(ctx context.Context, c core.Comment) error
}

// IssueOpener is a tracker that can open issues, for the daily triage to file the causes it finds and for agents
// to open follow-up work. labels go on as the issue is opened, so it is never seen without them.
type IssueOpener interface {
	OpenIssue(ctx context.Context, title, body string, labels []string) (core.Issue, error)
}

// Attacher is a tracker or repo that can hold files for comments: one an agent uploads to link from a comment,
// and the attachments people put in comments, which a private repository serves only to someone signed in.
type Attacher interface {
	// Attach stores a file for the issue and returns a link a comment can carry (as an image, for an image).
	Attach(ctx context.Context, key, name string, data []byte) (string, error)
	// Fetch downloads a file a comment links to, signed in. ok is false when the link isn't this service's.
	Fetch(ctx context.Context, url string) (body io.ReadCloser, ok bool, err error)
}

// Transitioner is a tracker with workflow statuses of its own (Jira), moved as the agent's state changes.
// state is "claimed" or one of the agent states.
type Transitioner interface {
	Transition(ctx context.Context, key, state string) error
}

// Hooked is a tracker or repo that can push events to the supervisor's webhook.
type Hooked interface {
	// HookTarget names what a hook is registered on; a tracker and a repo with the same target share one hook.
	HookTarget() string
	// EnsureWebhook points the supervisor's hook at url, creating it if need be, and returns its id.
	EnsureWebhook(ctx context.Context, url, secret, existing string) (string, error)
	// ParseWebhook checks a delivery's signature and turns it into signals.
	ParseWebhook(h http.Header, body []byte, secret string) ([]core.Signal, error)
}

// EventHooked is a Hooked whose hook can subscribe to more of its service's events, for jobs run on them; its
// ParseWebhook then returns a core.SigEvent for every delivery.
type EventHooked interface {
	Hooked
	// WantEvents adds event names ("push", "release") to what the hook subscribes to, before EnsureWebhook.
	WantEvents(names []string)
}

// Agent runs turns: one invocation of a coding agent on an issue's session, which exits when the agent has
// nothing more to do. Turns run detached so they outlive the supervisor; a later supervisor adopts them.
type Agent interface {
	Connection
	// Prepare readies the agent to run the supervisor's playbooks (write a plugin dir, check the binary).
	Prepare(ctx context.Context, playbooks map[string]string) error
	// NewSession makes a session id for a new issue.
	NewSession() string
	// SessionExists reports whether the session has run before, so the next turn resumes it.
	SessionExists(cwd, session string) bool
	// Start begins a turn.
	Start(ctx context.Context, spec TurnSpec) (Turn, error)
	// Adopt takes over a turn an earlier supervisor started.
	Adopt(h TurnHandle, structured bool) Turn
	// AttachCommand is an interactive session the owner can take over in the terminal.
	AttachCommand(spec AttachSpec) (*exec.Cmd, error)
	// RulesFiles are where an agent of this kind reads a project's rules, for the corrections sweep.
	RulesFiles() []string
}

// TurnSpec is one turn to run.
type TurnSpec struct {
	// Cwd is the project's main checkout.
	Cwd string
	// Env is the full environment for the agent's process.
	Env []string
	// Dir is where the turn's output files go.
	Dir string
	// Playbook names the supervisor playbook to follow ("work-issue", "sweep"); Args follow it.
	Playbook string
	Args     string
	// Body is the rest of the message: the in-flight list, the events.
	Body    string
	Session string
	Resume  bool
	// Structured asks for the core.Status at the end.
	Structured bool
	// DryRun swaps the prompt for a stub on a cheap model.
	DryRun bool
}

// TurnHandle is what is kept about a running turn so a later supervisor can adopt it.
type TurnHandle struct {
	PID       int               `json:"pid"`
	Out       string            `json:"out"`
	Err       string            `json:"err"`
	StartedAt time.Time         `json:"startedAt"`
	Extra     map[string]string `json:"extra,omitempty"`
}

// Turn is one running turn.
type Turn interface {
	Handle() TurnHandle
	// Send hands the running agent a message. False when the turn can't take one; otherwise taken is called
	// once: true when the agent took it in, false when the turn ended first.
	Send(text string, taken func(bool)) bool
	Kill()
	// Done is closed when the turn has ended; Outcome is then final.
	Done() <-chan struct{}
	Outcome() core.Outcome
}

// AttachSpec is an interactive session to hand the terminal to.
type AttachSpec struct {
	Cwd     string
	Env     []string
	Session string
}

// SetupAgent is an agent that can set the supervisor up: an interactive session, started with the setup
// guide, that asks its owner about their services and repos and writes the config through `config` commands.
type SetupAgent interface {
	SetupCommand(spec SetupSpec) (*exec.Cmd, error)
}

// SetupSpec is a setup session to start.
type SetupSpec struct {
	Cwd string
	Env []string
	// Guide is the setup skill, frontmatter included.
	Guide string
	// Allow are command prefixes the session may run without asking ("open-supervisor config").
	Allow []string
}

// Transcripts is an agent whose sessions can be read as they run.
type Transcripts interface {
	Transcript(cwd, session string) []core.Entry
	Activity(cwd, session string) *core.Activity
	TranscriptPath(cwd, session string) string
}

// UsageReporter is an agent that can say how much of its subscription's limits are used.
type UsageReporter interface {
	Usage(ctx context.Context) (*core.Usage, error)
}

// TelemetryEvent is one metric event.
type TelemetryEvent struct {
	Name  string
	Time  time.Time
	Props map[string]any
}

// Telemetry receives the supervisor's metrics: one event per turn, one per change in usage.
type Telemetry interface {
	Connection
	Capture(ctx context.Context, e TelemetryEvent) error
	Close(ctx context.Context) error
}

// Tunnel exposes the local webhook port to the internet.
type Tunnel interface {
	Connection
	// Run keeps the tunnel up until ctx ends, sending each public URL it gets (again after a restart) and ""
	// when it goes down.
	Run(ctx context.Context, port int, urls chan<- string) error
}
