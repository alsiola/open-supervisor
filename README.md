# open-supervisor

Claims issues across your repos and works each one in its own coding-agent session, a turn at a time, from a small TUI.

Comments, check results, previews and merges become the next turn's events. Comments that arrive mid-turn are handed straight to the running agent. Between turns nothing runs, so idle issues cost nothing.

Every third party sits behind a small adapter:

| Role | Adapters |
| --- | --- |
| Agent | Claude Code |
| Code host | GitHub, Bitbucket Cloud |
| Issue tracker | GitHub Issues, Jira (Cloud and Data Center) |
| Telemetry | PostHog |
| Webhooks | ngrok, Cloudflare Tunnel, a public URL, or none (polling only) |

Mix them per project: Jira issues with Bitbucket PRs in one project, GitHub for everything in another. [Writing an adapter](docs/ADAPTERS.md) covers adding Codex, Linear, GitLab, OpenTelemetry or Datadog.

## Install

```sh
curl -fsSL https://raw.githubusercontent.com/alsiola/open-supervisor/main/install.sh | sh
# or
brew install alsiola/tap/open-supervisor
# or
go install github.com/alsiola/open-supervisor/cmd/open-supervisor@latest
```

There are single binaries for macOS, Linux and Windows (amd64 and arm64) on the releases page. You also need `git` and your agent's CLI (for Claude Code, `npm i -g @anthropic-ai/claude-code`, signed in).

## Start

```sh
open-supervisor init      # the wizard: agent, code host, tracker, telemetry, webhooks, projects
open-supervisor doctor    # checks every connection and project, read-only
open-supervisor           # the TUI
```

The first `open-supervisor` with no config starts the wizard itself. Everything the wizard sets can be changed later from the TUI: press `,`.

Other ways to run it:

```sh
open-supervisor --headless   # no TUI; the log goes to stdout (launchd, systemd, a server)
open-supervisor --dry-run    # changes nothing on any service; turns run a stub prompt on a cheap model
open-supervisor --no-tunnel  # no webhooks; the reconcile polls
open-supervisor adapters     # what this build can talk to
```

## Configuration

The config lives in `~/.config/open-supervisor/config.yaml` (or `$OPEN_SUPERVISOR_CONFIG`, or `--config`):

```yaml
version: 1
settings:
  agent: claude              # default agent connection
  maxTurns: 4                # turns running at once, across every project
  maxOpenPerProject: 10      # issues claimed at once per project
  reconcileMinutes: 15
  tunnel: ngrok              # empty: no webhooks, poll only
  telemetry: [posthog]
  branch: agent/issue-{key}
  sweepAt: "08:41"           # daily corrections sweep; empty turns it off
  labels:
    claimed: agent-in-progress
    scheduled: scheduled
    skip: [backlog, human, milestone]
    help: agent-needs-help
    review: ready-for-review
    done: might-be-done
connections:
  claude:  { type: claude-code, model: opus }
  github:  { type: github, auth: gh }
  bb:      { type: bitbucket, username: me@acme.com, token: "keyring:bb.token" }
  jira:    { type: jira, baseUrl: https://acme.atlassian.net, email: me@acme.com, token: "keyring:jira.token" }
  posthog: { type: posthog, token: "env:POSTHOG_KEY", host: https://eu.i.posthog.com }
  ngrok:   { type: ngrok, domain: my-supervisor.ngrok.app }
projects:
  - name: site
    root: ~/Code/site
    repo:    { connection: github }        # owner/name is read from the checkout's origin
    tracker: { connection: github }
    worktree: { copy: [.env], setup: pnpm install --frozen-lockfile, setupWhen: [pnpm-lock.yaml] }
  - name: api
    root: ~/Code/api
    repo:    { connection: bb, repo: acme/api }
    tracker: { connection: jira, project: API, transitionClaimed: In Progress, transitionReview: In Review }
    jobs:
      - { name: refresh, command: [pnpm, refresh], at: ["06:00"], status: .local/refresh, key: f }
```

Secrets never go in the file. A secret field holds a reference: `keyring:<name>` (the OS keyring, which is what the wizard uses), `env:VAR`, `cmd:op read …`, or `file:<name>` (`secrets.yaml` next to the config, for machines without a keyring).

State (issue records, turn output, logs, correction notes) goes in `~/.local/state/open-supervisor`. Generated files (the agent plugin, issue worktrees) go in `~/.local/share/open-supervisor`. Nothing is written into your checkouts except through git.

A project can also keep jobs in its own checkout, in `.open-supervisor.yaml` under `jobs:`.

## What it does

- **Claims** every open issue nobody has. An issue is skipped if it carries the claimed label or any of the skip labels. Claiming adds the label, makes a session and starts the first turn with the project's in-flight list.
- **Runs turns.** A turn is one agent invocation on the issue's session, from the project's checkout. It ends with a structured status: `building`, `review`, `help`, `done` or `finished`, with a summary, PRs, preview and what it touches. Turns run detached: quitting leaves them running, and the next start adopts them.
- **Queues events** per issue for the next turn: human comments on the issue and its PRs (inline threads and reviews too), checks finishing on the PR head (with the preview URL, or the failures; a head with no checks after 10 minutes is reported as such), PRs merged or closed, the issue closed or reopened. Bots' comments and agents' own comments are dropped. A comment from another agent on this issue gets through.
- **Interjects.** A comment or TUI message that arrives mid-turn goes into the running turn. The agent takes it in after its current tool call.
- **Sets status labels** from the state each turn ends in. On Jira it can also move the issue through workflow statuses.
- **Schedules.** An agent parks an issue until a time with `agent schedule`. An issue labelled `scheduled` with a `runAt` marker waits until then.
- **Webhooks plus reconcile.** Webhooks arrive through the tunnel and are signature-checked. Every few minutes a reconcile reads everything a webhook could have missed, so webhooks are optional.
- **Retries** a failed turn after 5, then 10, then 15 minutes, then waits for `r`.
- **Sweeps** once a day: an agent turn folds the corrections agents logged into the project's rule files.
- **Usage and metrics.** It shows the Claude subscription's 5-hour and 7-day windows in the header. It sends an `agent_turn` event per turn (tokens, cost, duration, state) and an `agent_usage` event to the telemetry connections.

### What agents get

The playbooks (`internal/playbooks`) reach Claude Code as a plugin of skills: `/open-supervisor:work-issue`, `/open-supervisor:sweep`. Agents touch the tracker and code host only through `open-supervisor agent …`, so the playbooks are the same everywhere:

```
agent context | issue view | issue comment | pr list | pr create | pr view | pr comment | pr reply
agent checks | worktree setup | worktree remove | screenshots | schedule | correction | inflight
```

Rules particular to a project (what must pass, how to run its dev server) belong in its own `CLAUDE.md` or `AGENTS.md`. `open-supervisor playbooks` writes the plugin out and prints its path, for `claude --plugin-dir <path>` by hand.

## The TUI

| Key | Does |
| --- | --- |
| `↑` `↓` | select an issue |
| `⏎` | the issue's detail page, with its live transcript (`t` scrolls the transcript, `esc` goes back) |
| `g` | open the issue in the browser |
| `m` | message the agent: into the running turn, or a new turn |
| `a` | attach: hand the terminal to the agent's session; events queue until you exit |
| `r` | run a turn now; retries a failed one, wakes a scheduled one |
| `k` | kill the running turn |
| `d` | drop the issue from the list |
| `c` | claim an issue by key: `12`, `PROJ-12`, `project#12` |
| `R` | reconcile now |
| `s` | sweep now |
| job keys | run that job now |
| `,` | edit the config: general settings, connections (with a connection test), projects |
| `l` | show or hide the log |
| `q` | quit; running turns carry on and are adopted at the next start |

## Development

```sh
make test       # go test -race ./...
make build      # bin/open-supervisor
make snapshot   # every platform's release build into dist/ (needs goreleaser)
```

To release, push a `v*` tag. The release workflow builds every platform with goreleaser, publishes the GitHub release and updates the Homebrew tap. The tap update needs an `alsiola/homebrew-tap` repo and a `HOMEBREW_TAP_TOKEN` secret.
