# Writing an adapter

Every service open-supervisor talks to sits behind a small Go interface in [`internal/adapter`](../internal/adapter/adapter.go). An adapter is one package that implements the interfaces for the roles it can fill and registers a `Spec` describing its settings. The onboarding wizard and the TUI's config editor build their forms from that `Spec`, so a new adapter needs no UI code.

| Role | Interface(s) | Shipped | Ideas |
| --- | --- | --- | --- |
| agent | `Agent`, optionally `Transcripts`, `UsageReporter` | Claude Code | Codex CLI, Gemini CLI, an Anthropic API tool loop |
| repo | `RepoProvider` → `Repo`, optionally `Reactor`, `Hooked` | GitHub, Bitbucket Cloud | GitLab, Bitbucket Data Center, Gitea |
| tracker | `TrackerProvider` → `Tracker`, optionally `Reactor`, `Transitioner`, `Hooked` | GitHub Issues, Jira | Linear, GitLab Issues, Shortcut |
| telemetry | `Telemetry` | PostHog | OpenTelemetry (OTLP), Datadog, StatsD |
| tunnel | `Tunnel` | ngrok, Cloudflare Tunnel, static URL | Tailscale Funnel |

## The shape of one

```go
package linear

import "github.com/alsiola/open-supervisor/internal/adapter"

func init() {
	adapter.Register(adapter.Spec{
		Type:        "linear",              // the `type:` in the config
		Title:       "Linear",
		Description: "Issues from a Linear team.",
		Roles:       []adapter.Role{adapter.RoleTracker},
		// The connection's own settings: credentials, a site.
		Fields: []adapter.Field{
			{Key: "token", Label: "API key", Type: adapter.Secret, Required: true},
		},
		// What each project says when it binds the role to this connection.
		Binding: map[adapter.Role][]adapter.Field{
			adapter.RoleTracker: {{Key: "team", Label: "Team key", Type: adapter.Text, Required: true}},
		},
		New: func(v adapter.Values, env adapter.Env) (adapter.Connection, error) {
			token, err := env.Secret(v.Get("token", "")) // resolves keyring:, env:, file:, cmd: references
			if err != nil {
				return nil, err
			}
			return &Conn{token: token, env: env}, nil
		},
	})
}
```

Then add the package to [`internal/adapters/all/all.go`](../internal/adapters/all/all.go).

## Conventions

- **Fields.** `Secret` fields are stored in the OS keyring by the wizard and editor, never written into the config. Use `ShowIf` for fields that only apply to one auth mode. Give every field a `Help` line, because that line is the user's documentation.
- **Keys.** Issue keys are strings, written the way people write them: `12` on GitHub, `PROJ-12` on Jira. Comment IDs are strings, and PR numbers are ints.
- **Agent marks.** Comments the toolbelt posts carry a `core.AgentMark` naming the posting agent's issue. Encode it in whatever invisible form the service allows: an HTML comment on GitHub, a Markdown reference line on Bitbucket, a comment property on Jira. Decode it into `Comment.Agent` when reading. That's how agents avoid hearing their own comments while still hearing each other's.
- **runAt markers.** `RunAtMarker` returns a marker that `core.RunAtIn` can find in a comment on your service: `<!-- runAt: … -->` where HTML comments are hidden, `[runAt: …]` where they aren't.
- **Webhooks.** Implement `Hooked` when the service can push events. `ParseWebhook` must verify the signature (use `adapter.VerifyHMAC`) and return only the signals for its role. A tracker returns issue signals, a repo returns PR and check signals. When a tracker and a repo share a `HookTarget` (GitHub), they share one hook and each parses its own half. Everything a webhook says must also be discoverable by polling, because the reconcile is the source of truth.
- **Dry run.** When `env.DryRun` is set, change nothing on the service: no labels, reactions or hooks.
- **Errors.** Return errors with the request in them. The log shows their first line.

## An agent adapter

An `Agent` runs one turn at a time per issue, detached, so turns outlive the supervisor:

- `Start` gets a `TurnSpec`, holding the playbook name and its arguments ("work-issue", "12 events"), the message body, the session id, whether to resume, and whether to end with the structured `core.Status` (its JSON schema is `core.StatusSchema`). Write output to files under `spec.Dir`, and return a `TurnHandle` (pid, output files) that a later supervisor can `Adopt`.
- `Send` hands the running agent a message mid-turn. Return false if your agent can't take one, and it waits for the next turn.
- `Prepare` gets the playbooks (`internal/playbooks/*.md`). Claude Code installs them as a plugin of skills. An agent without skills can put `playbooks.Body(text)` at the top of the first prompt instead.
- The playbooks tell the agent to use `$OPEN_SUPERVISOR agent …` for everything that touches a service. Keep `OPEN_SUPERVISOR*` in the environment you pass on.

Look at [`internal/adapters/claudecode`](../internal/adapters/claudecode) for a complete one, and [`internal/engine/engine_test.go`](../internal/engine/engine_test.go) for fakes of every role.
