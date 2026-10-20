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
	SigPing          SignalType = "ping"
)

// Signal is one thing a webhook delivery said.
type Signal struct {
	Type    SignalType
	Key     string
	Issue   *Issue
	PR      *PR
	Comment *Comment
	SHA     string
}
