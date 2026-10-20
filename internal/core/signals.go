package core

// SignalType names what a webhook delivery can tell the engine. Adapters turn their service's payloads into
// signals; the engine decides what each means for the issues it holds.
type SignalType string

const (
	SigIssueChanged  SignalType = "issue_changed"  // opened, labelled or unlabelled: a claim candidate
	SigIssueClosed   SignalType = "issue_closed"   // Key
	SigIssueReopened SignalType = "issue_reopened" // Issue
	SigIssueComment  SignalType = "issue_comment"  // Key, Comment
	SigPRChanged     SignalType = "pr_changed"     // opened, reopened or pushed to: PR
	SigPRClosed      SignalType = "pr_closed"      // PR (State says merged or closed)
	SigPRComment     SignalType = "pr_comment"     // PR, Comment
	SigChecks        SignalType = "checks"         // SHA: something about the commit's checks changed
	SigEvent         SignalType = "event"          // Event: the delivery as it came, for jobs run on events
	SigPing          SignalType = "ping"
)

// HookEvent is a webhook delivery named the way its service names it ("push", "release" with action
// "published"), with its body, so a job can run on it.
type HookEvent struct {
	Name   string
	Action string
	// Ref is the git ref the event is about, when it has one ("refs/heads/main" for a push).
	Ref  string
	Body []byte
}

// Signal is one thing a webhook delivery said.
type Signal struct {
	Type    SignalType
	Key     string
	Issue   *Issue
	PR      *PR
	Comment *Comment
	SHA     string
	Event   *HookEvent
}
