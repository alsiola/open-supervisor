# Writing an adapter

Every service open-supervisor talks to sits behind a small Go interface in [`internal/adapter`](../internal/adapter/adapter.go). An adapter is one package that implements the interfaces for the roles it can fill and registers a `Spec` describing its settings. The onboarding wizard, the TUI's config editor and the `config` commands all work from that `Spec` (`open-supervisor adapters <type>` prints it), so a new adapter needs no UI code.

| Role | Interface(s) | Shipped | Ideas |
| --- | --- | --- | --- |
| agent | `Agent`, optionally `Transcripts`, `UsageReporter` | Claude Code | Codex CLI, Gemini CLI, an Anthropic API tool loop |
| repo | `RepoProvider` → `Repo`, optionally `Reactor`, `Hooked` | GitHub, Bitbucket Cloud | GitLab, Bitbucket Data Center, Gitea |
| tracker | `TrackerProvider` → `Tracker`, optionally `Reactor`, `ReactionReader`, `Transitioner`, `Hooked` | GitHub Issues, Jira, Linear | GitLab Issues, Shortcut |
| telemetry | `Telemetry` | PostHog | OpenTelemetry (OTLP), Datadog, StatsD |
| tunnel | `Tunnel` | ngrok, Cloudflare Tunnel, static URL | Tailscale Funnel |

## The shape of one

```go
package shortcut

import "github.com/alsiola/open-supervisor/internal/adapter"

func init() {
	adapter.Register(adapter.Spec{
		Type:        "shortcut",            // the `type:` in the config
		Title:       "Shortcut",
		Description: "Stories from a Shortcut team.",
		Roles:       []adapter.Role{adapter.RoleTracker},
		// The connection's own settings: credentials, a site.
		Fields: []adapter.Field{
			{Key: "token", Label: "API key", Type: adapter.Secret, Required: true},
		},
		// What each project (or source) says when it binds the role to this connection.
		Binding: map[adapter.Role][]adapter.Field{
			adapter.RoleTracker: {{Key: "team", Label: "Team", Type: adapter.Text, Required: true}},
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
- **Keys.** Issue keys are strings, written the way people write them: `12` on GitHub, `PROJ-12` on Jira, `ENG-12` on Linear. Comment IDs are strings, and PR numbers are ints.
- **Route properties.** Fill `core.Issue.Attrs` (with `SetAttr`) with every property a route might match on: project, team, components, state, priority, assignee, cycle, custom fields. Use lowercase names, give both the key and the display name where a thing has both (`ENG`, `Engineering`), and leave out what the issue lacks. Fill them the same way from `Candidates`, `Issue` and webhooks: a source routes an issue the reconcile finds exactly as it routes one a webhook delivers. A tracker whose deliveries don't carry the whole issue can read it back from the API in `ParseWebhook` (Jira and Linear do), or send a signal with only `Key` set and let the source read it. List the names in the binding's `Help` or the package doc.
- **Several at once.** A tracker bound for a source often spans more than one of the service's projects or teams (`project: API, WEB`, `team: ENG, OPS`). Accept a comma-separated list where the binding names one, and drop deliveries for issues outside it.
- **Agent marks.** Comments the toolbelt posts carry a `core.AgentMark` naming the posting agent's issue. Encode it in whatever invisible form the service allows: an HTML comment on GitHub, a Markdown reference line on Bitbucket, a comment property on Jira, and a short visible footer on Linear, which keeps nothing hidden. Decode it into `Comment.Agent` when reading. That's how agents avoid hearing their own comments while still hearing each other's.
- **Hold markers.** `HoldMarker` wraps `core.Hold.Spec()` in a form `core.HoldIn` can find in an issue body or comment on your service: `<!-- runAt: … -->` / `<!-- waitFor: … -->` where HTML comments are hidden, `[runAt: …]` / `[waitFor: …]` where they aren't. Check that a marker survives the trip through the service's markup.
- **Opening issues.** `IssueOpener.OpenIssue` puts the labels on as it opens the issue, not after, so the supervisor never sees a held issue without its `scheduled` label and claims it early.
- **Webhooks.** Implement `Hooked` when the service can push events. `ParseWebhook` must verify the signature (use `adapter.VerifyHMAC` for `sha256=` signatures) and return only the signals for its role. A tracker returns issue signals, a repo returns PR and check signals. Send `SigIssueChanged` for any change a route could care about, not only labels. Everything with the same `HookTarget` shares one hook, across sources and projects: a source on a GitHub repository and the project whose code is there get one hook, and each parses its own half. Everything a webhook says must also be discoverable by polling, because the reconcile is the source of truth. Implement `EventHooked` too to let jobs run on the service's other events: `WantEvents` adds names to the hook's subscription, and `ParseWebhook` then also returns a `core.SigEvent` carrying the delivery's name, action, git ref and body. Jobs run on these straight from the webhook; the reconcile doesn't replay them.
- **Dry run.** When `env.DryRun` is set, change nothing on the service: no labels, reactions or hooks.
- **Errors.** Return errors with the request in them. The log shows their first line.

## An agent adapter

An `Agent` runs one turn at a time per issue, detached, so turns outlive the supervisor:

- `Start` gets a `TurnSpec`, holding the playbook name and its arguments ("work-issue", "12 events", or "12 handoff" for a fresh session on an issue that has had turns), the message body, the session id, whether to resume, and whether to end with the structured `core.Status` (its JSON schema is `core.StatusSchema`). Write output to files under `spec.Dir`, and return a `TurnHandle` (pid, output files) that a later supervisor can `Adopt`.
- `Send` hands the running agent a message mid-turn. Return false if your agent can't take one, and it waits for the next turn.
- `Prepare` gets the playbooks (`internal/playbooks/*.md`). Claude Code installs them as a plugin of skills. An agent without skills can put `playbooks.Body(text)` at the top of the first prompt instead.
- The playbooks tell the agent to use `$OPEN_SUPERVISOR agent …` for everything that touches a service. Keep `OPEN_SUPERVISOR*` in the environment you pass on.
- `SetupCommand` (optional, `adapter.SetupAgent`) is what makes `init --with <type>` work. It returns an interactive session, attached to the terminal, started on the setup guide (`playbooks.Setup`).
  - **Where the guide goes.** Hand it over the way your agent takes skills, but keep it apart from the turn playbooks, so agents working issues never see it. Claude Code gets it as its own plugin.
  - **Permissions.** Let the commands in `spec.Allow` run without a permission prompt.

Look at [`internal/adapters/claudecode`](../internal/adapters/claudecode) for a complete one, and [`internal/engine/engine_test.go`](../internal/engine/engine_test.go) for fakes of every role.
