# open-supervisor

Claims issues across your repos and works each one in its own coding-agent session, a turn at a time, from a small TUI.

Comments, check results, previews and merges become the next turn's events. Comments that arrive mid-turn are handed straight to the running agent. Between turns nothing runs, so idle issues cost nothing.

Every third party sits behind a small adapter:

| Role | Adapters |
| --- | --- |
| Agent | Claude Code |
| Code host | GitHub, Bitbucket Cloud |
| Issue tracker | GitHub Issues, Jira (Cloud and Data Center), Linear |
| Telemetry | PostHog |
| Webhooks | ngrok, Cloudflare Tunnel, a public URL, or none (polling only) |

Mix them per project: Jira issues with Bitbucket PRs in one project, GitHub for everything in another. One tracker can also feed several repos, with [routes](#routing-issues-to-projects) deciding which repo gets each issue. [Writing an adapter](docs/ADAPTERS.md) covers adding Codex, GitLab, Shortcut, OpenTelemetry or Datadog.

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
open-supervisor init --with claude-code   # your coding agent sets it up with you
open-supervisor init                      # or the wizard: agent, code host, tracker, telemetry, webhooks, projects
open-supervisor doctor                    # checks every connection, project and source, read-only
open-supervisor                           # the TUI
```

The first `open-supervisor` with no config asks which way to set up: with a coding agent it finds installed, or with the wizard. Everything set up can be changed later from the TUI (press `,`), the wizard, or `open-supervisor config`.

### Setting up with your agent

`init --with claude-code` starts Claude Code in your terminal with a setup skill (it isn't one of the playbooks agents get for turns). The agent:

1. Looks for your checkouts and their remotes, and asks which are in scope.
2. Asks which services you use, and how issues map to repos.
3. Writes the config with `open-supervisor config` commands, testing each connection as it adds it.
4. Reads the tracker's open issues to suggest routes.
5. Finishes with `doctor`.

The supervisor's own read and config commands run without asking. Anything else the agent runs asks you first, as usual.

It's an ordinary interactive Claude Code session: you answer its questions, and can stop it at any point and pick up later with the same command. `--with` also takes the name of an agent connection you've already configured.

Tokens never pass through the agent. It asks you to give each one as an `env:` or `cmd:` reference, or to run `open-supervisor config secret <connection> <field>` in a terminal of your own. That command asks for the token without showing it and stores it in the keyring.

### Changing the config from the command line

`open-supervisor config` changes the config one step at a time, for scripts and agents. Each command saves at once, then lists what the config still lacks (`doctor` checks the whole config when you're done):

```sh
open-supervisor config connection set linear --type linear --set token=env:LINEAR_API_KEY   # tested as it's saved
open-supervisor config project set web --root ~/Code/web --code github --source linear      # the repo is read from the checkout
open-supervisor config source set linear --tracker linear --set team=ENG --unrouted label
open-supervisor config source inspect linear                   # the properties its open issues carry, and where routes send them
open-supervisor config route set linear web --when labels=web,frontend
open-supervisor config route set linear api                    # everything else
open-supervisor config set settings.maxTurns 2                 # any value, by its dotted path in `config show`
open-supervisor config get projects.web
open-supervisor adapters linear                                # an adapter's connection and binding fields
```

Other ways to run it:

```sh
open-supervisor --headless   # no TUI; the log goes to stdout (launchd, systemd, a server)
open-supervisor --dry-run    # changes nothing on any service; turns run a stub prompt on a cheap model
open-supervisor --no-tunnel  # no webhooks; the reconcile polls
open-supervisor adapters     # what this build can talk to
open-supervisor route ENG-42 # an issue's properties, and which project its source's routes send it to
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
  git:
    remote: origin           # where branches are fetched from and pushed to
    sign: false              # off: agents' and jobs' commits are never signed, so nothing waits on a signer
  sweepAt: "08:41"           # daily corrections sweep; empty turns it off
  triageAt: "08:51"          # daily triage of the problems agents report; empty turns it off
  handoffMinutes: 55         # a turn this long after the last starts a fresh session from the handoff; 0: every turn does
  host:                      # optional watch on this machine's CPU, memory and disks; off without minutes
    minutes: 1               # how often to take a reading
    load: 1.5                # 1-minute load per CPU over which the host is overloaded (1: every core busy)
    memory: 90               # % of memory in use over which it's running out
    disk: 90                 # % of a disk in use over which it's running out; worktree setup keeps this share free (at most 10 GB)
    disks: [/Volumes/data]   # more paths to watch, besides the worktrees', the state folder's and checkouts'
  ambient:                   # the coordination experiment; off: agents comment on each other's issues (direct)
    on: true                 # share every agent comment with the other active issues it could bear on
    key: "env:OPENROUTER_API_KEY"
    model: cloudflare/clef-omni   # an OpenRouter decisions model
    threshold: 0.4           # the probability at or above which a comment is shared
  labels:
    claimed: agent-in-progress
    scheduled: scheduled
    skip: [backlog, human, milestone]
    require: []              # set, e.g. [agent]: only issues with one of these (or `scheduled`) are claimed
    help: agent-needs-help
    review: ready-for-review
    done: might-be-done
connections:
  claude:  { type: claude-code, model: opus }
  github:  { type: github, auth: gh }
  bb:      { type: bitbucket, username: me@acme.com, token: "keyring:bb.token" }
  jira:    { type: jira, baseUrl: https://acme.atlassian.net, email: me@acme.com, token: "keyring:jira.token" }
  linear:  { type: linear, token: "keyring:linear.token" }
  posthog: { type: posthog, token: "env:POSTHOG_KEY", host: https://eu.i.posthog.com }
  ngrok:   { type: ngrok, domain: my-supervisor.ngrok.app }
projects:
  - name: site
    root: ~/Code/site
    repo:    { connection: github }        # owner/name is read from the checkout's origin
    tracker: { connection: github }
    worktree: { copy: [.env], setup: pnpm install --frozen-lockfile, setupWhen: [pnpm-lock.yaml] }
    triage: { openIssues: true, land: pr }   # file an issue per cause; land its fixes as a PR, not a push
    sweep: { land: pr }                      # the same for the corrections sweep (push by default)
  - name: api
    root: ~/Code/api
    repo:    { connection: bb, repo: acme/api }
    tracker: { connection: jira, project: API, transitionClaimed: In Progress, transitionReview: In Review }
    jobs:
      - { name: refresh, command: [pnpm, refresh], at: ["06:00"], status: .local/refresh }
      - { name: deploy, command: [./scripts/deploy.sh], on: { events: [push], branches: [main] } }
      - { name: reindex, command: [pnpm, reindex], on: { webhook: true } }
  - name: web
    root: ~/Code/web
    repo: { connection: github }       # no tracker: the linear source routes issues here
  - name: mobile
    root: ~/Code/mobile
    repo: { connection: github }
sources:
  - name: linear
    tracker: { connection: linear, team: ENG, stateClaimed: In Progress, stateReview: In Review }
    unrouted: label
    routes:
      - { project: web, when: { labels: [web, frontend] } }
      - { project: mobile, when: { labels: [ios, android], not: { state: Triage } } }
```

Secrets never go in the file. A secret field holds a reference: `keyring:<name>` (the OS keyring, which is what the wizard uses), `env:VAR`, `cmd:op read …`, or `file:<name>` (`secrets.yaml` next to the config, for machines without a keyring).

State (issue records, turn output, logs, correction and problem notes, the triage's report) goes in `~/.local/state/open-supervisor`. Generated files (the agent plugin, issue worktrees) go in `~/.local/share/open-supervisor`. Nothing is written into your checkouts except through git.

### Jobs

A job is a command the supervisor runs for a project. A project can also keep jobs in its own checkout, in `.open-supervisor.yaml` under `jobs:`. The supervisor reads the file again each minute and after a job fast-forwards the checkout, so a job added, changed or taken out there takes effect without a restart. The file is read from the main checkout, so a change merged on the remote counts once the checkout has it.

```yaml
jobs:
  - name: x engagement
    command: [node, pipeline/x/open-issue.ts]
    cron: "0 9,13,18 * * *"
    timeZone: Europe/London       # UTC when left out
  - name: post reels
    command: [node, pipeline/reels/post.ts]
    cron: "0 */6 * * *"
    checkout: fresh
    on: { events: [push], branches: [main], paths: ['data/reels/*.json', '!data/reels/posted.json'] }
  - name: smell check
    command: [./scripts/check.sh]
    everyMinutes: 10
    timeoutMinutes: 5
  - name: refresh                 # long: started detached, reports through its status folder
    command: [pnpm, refresh]
    at: ["05:17", "17:17"]
    status: .local/refresh
```

- **When.** `everyMinutes`; `at` (UTC `HH:MM` times); `cron`, five fields read in `timeZone`; webhooks (`on:`, below); or by hand from the TUI (`j`). A timed slot missed by more than six hours (the supervisor wasn't running) waits for the next. The latest slot within six hours runs once.
- **Clock changes.** A `cron` time follows the zone's summer time. A time the clocks skip (01:30 London on the last Sunday in March) doesn't run that day. A time they repeat (01:30 on the last Sunday in October) runs once. To be safe, keep jobs out of 01:00–02:00 in zones that change.
- **Runs the supervisor waits for.** A job runs and the supervisor waits for it, up to `timeoutMinutes` (5 by default). The exit code says how it went. One run of a job goes at a time. A run asked for while one is going waits for it to end. At most one run waits: a delivery's takes the place of a timer's, and a later delivery's takes the place of an earlier one's.
- **Detached runs.** A timed job with `status` is for long runs: it starts detached and keeps `running.json` and `last.json` in that folder itself (see `Job` in `internal/config/jobs.go`). A delivery that arrives while it runs starts nothing.
- **Where.** Jobs run in the project's main checkout. First the supervisor fetches the base branch and fast-forwards the checkout when it's on that branch, so a job runs the code that has merged. Uncommitted changes don't stop it unless the update touches one of those files, and nothing is ever stashed or reset. When it can't catch up (another branch is checked out, the branch has commits of its own, an uncommitted change is in the way, or another job is running in the checkout), the run goes ahead anyway. The run's output and the supervisor's log say how many commits behind it was and why, and so do `behind` and `stale` in the run's JSON. A job that must have the latest code should use `checkout: fresh`. With `checkout: fresh`, each run gets a new worktree, detached at the base branch's remote head. It has the project's `worktree.copy` files (`.env`), and its `worktree.setup` runs every time. The worktree is removed after a run that passes. After a run that fails, it is kept for a day so you can look at it. A job that commits can `git push origin HEAD:$OPEN_SUPERVISOR_BASE_BRANCH`. The worktree shares the checkout's git config and credentials, so it pushes as the agents do.
- **Environment.** The project's sessions' environment: the supervisor's own, the checkout's `.env`, and the project's `env:`, with secret references (`keyring:…`) resolved. Each run also gets `OPEN_SUPERVISOR_JOB`, `OPEN_SUPERVISOR_JOB_TRIGGER` and `OPEN_SUPERVISOR_JOB_RUN`.
- **Run history.** Each run's output goes in `<state>/projects/<project>/job-runs/<job>/<id>.log`, with `<id>.json` beside it. The JSON holds the trigger (`cron 2026-10-21 09:00 Europe/London`, `push 1a2b3c4`, `webhook`, `every 10m`, `by hand`), the start and end, the outcome and the exit code, and for a run in the main checkout, `updated` (commits it was fast-forwarded by) or `behind` and `stale`. The supervisor keeps the last `keepRuns` runs (30 by default). A project's health check can read these files to confirm each slot ran. In the TUI, `J` lists the jobs, then a job's runs, then a run's output.

### Routing issues to projects

A project with a `tracker:` of its own takes every issue in it. When one tracker holds the issues for several repos (a Linear team, a Jira project, a planning repo's GitHub issues), make it a **source** instead: the projects it feeds leave out `tracker:`, and the source's `routes` decide which project gets each issue.

- **One read, one hook.** The source reads the tracker's open issues once per reconcile and registers one webhook, however many projects it feeds.
- **First match wins.** Routes are tried in order. `when:` maps an issue property to a value or a list of values. Fields are ANDed, a field's values are ORed, matching ignores case, and `*` and `?` are wildcards (`area/*`, `API v?`). `any:`, `all:` and `not:` combine matches. A route with no `when:` takes everything, so it belongs last.
- **Properties.** Every tracker gives `labels`, `key`, `title` and `author`. On top of those:
  - **GitHub** adds `repo`, `milestone`, `assignees` and `type`.
  - **Jira** adds `project` (key and name), `issuetype`, `status`, `priority`, `components`, `fixVersions`, `assignee`, `parent`, and any custom field listed in the binding's `fields` (`customfield_10010`).
  - **Linear** adds `team`, `project`, `state`, `statetype`, `priority`, `estimate`, `cycle`, `assignee`, `creator` and `parent`.

  `open-supervisor route <issue>` prints an issue's properties and which route takes it.
- **Linear specifics.** A Linear tracker offers issues in the `unstarted` and `started` state types (Todo, In Progress). `states:` changes that, by type or state name, so Backlog waits until someone moves it on. `stateClaimed`, `stateReview` and the others move the issue through workflow states, as on Jira. Agents' comments end with a short "Posted by an open-supervisor agent" line, because Linear keeps no hidden markers. Registering the webhook needs a workspace admin; without one, add it by hand or rely on the reconcile.
- **Several projects of the service.** A source can span more than one of them: `project: API, WEB` on Jira, `team: ENG, OPS` on Linear.
- **Unrouted issues.** An issue no route takes is left alone and shown in the TUI under the issues in flight. With `unrouted: label`, it also gets the `needs-routing` label (`settings.labels.unrouted`).
- **Stranded issues.** An issue that carries the claimed label (`agent-in-progress`) though this supervisor never claimed it, say because someone added the label by hand, is left alone too: the label may be another supervisor's. It is logged once and shown in the TUI under the issues in flight as stranded. To release it, remove the label (an issue is released by removing its skip label; the supervisor adds the claimed label itself), or claim it there with `c <project>#<key>`.
- **Claiming by hand.** `c ENG-42` claims an issue through the routes. `c web#ENG-42` gives it to `web` whatever they say.
- **Pinned at claim.** Once a project claims an issue, the issue stays with that project. A change that would route it elsewhere is logged, but comments, closes and reopens still go to the project that has it. To move an issue, drop it (`d`) and claim it as `<project>#<key>`.
- **Labels belong to the source.** The claimed, skip and status labels live on the shared tracker, so a source sets them with its own `labels:` and the projects it feeds take them from it.
- **One source per project.** A project takes issues from one place: its own tracker, or one source. A source's routes can name it more than once.

Sources can be set up in four ways:
- **The wizard.** Adding a project whose tracker reads the same issues as another project's offers to share it. Each way asks:
  - which property splits the issues;
  - which values send an issue to each project, suggesting the values the open issues carry;
  - and then shows where the open issues would go.
- **The TUI's config editor** (`,`, then the Sources tab) does the same, and edits sources that already exist.
- **The `config` commands.**
- **By hand, in the file.**

Routes using `any:`, `all:` or `not:` are kept as written; the forms ask only about projects that are new to the source. Webhook URLs are `/hooks/<project or source>/<connection>`, and a project with its own tracker keeps the URL it had.

### Jobs on webhooks

A job with `on:` also runs when a webhook arrives through the tunnel:

- `events` are the code host's own event names, taken through the project's hook (GitHub for now): `push`, `release`, `workflow_run`, or `release.published` for one action. The supervisor adds them to the hook's subscription. `branches` limits events that carry a git ref, such as a push, to those branches, and takes globs (`release/*`).
- `paths` limits a push to those that change a matching file, as in GitHub Actions. `*` matches within a folder and `**` across folders. A pattern starting `!` takes back the files it matches from the patterns before it. The changed files come from `git diff` between the push's two heads, or else from the delivery's commits.
- `webhook: true` gives the job a URL of its own, `<tunnel>/jobs/<project>/<job>/<token>`, for any other service to POST to. The token is derived from the supervisor's webhook secret, so the URL stays the same across restarts. The supervisor logs the URL when the tunnel comes up.

The job learns about the delivery from `OPEN_SUPERVISOR_EVENT`, `OPEN_SUPERVISOR_EVENT_ACTION`, `OPEN_SUPERVISOR_EVENT_REF`, `OPEN_SUPERVISOR_EVENT_SHA` (a push's new head) and `OPEN_SUPERVISOR_EVENT_FILE` (a file holding the body, kept for a day). `OPEN_SUPERVISOR_EVENT` is `webhook` for the job's own URL. A delivery that arrives while the job is running waits for that run to end (see Jobs). With no tunnel, or under `--dry-run`, these jobs don't run on webhooks. Deliveries aren't replayed by the reconcile, so a missed one stays missed.

## What it does

- **Claims** every open issue nobody has, in the project its source's routes name. An issue is skipped if it carries the claimed label or any of the skip labels. With `labels.require`, claiming is opt-in: only issues carrying one of those labels (or the scheduled label) are claimed, which suits a shared team tracker. Claiming adds the label, makes a session and starts the first turn with the project's in-flight list.
- **Runs turns.** A turn is one agent invocation on the issue's session, from the project's checkout. It ends with a structured status: `building`, `review`, `help`, `done` or `finished`, with a summary, PRs, preview and what it touches. Turns run detached: quitting leaves them running, and the next start adopts them.
- **Hands off cold sessions.** Each turn's status also carries a short handoff note: what is pending and the judgement calls made. A resumed session past its prompt cache's hour rebuilds its whole context first, which can cost ten times a warm turn. So a turn due more than `handoffMinutes` (55) after the last one ended starts a fresh session from the note instead of resuming. `agent_turn` carries `handoff` and `session`. The TUI lists the issue's earlier sessions. Attaching to an issue makes its next turn resume, so what you did in the session isn't lost.
- **Queues events** per issue for the next turn: human comments on the issue and its PRs (inline threads and reviews too), checks finishing on the PR head (with the preview URL, or the failures; a head with no checks after 10 minutes is reported as such), PRs merged or closed, the issue closed or reopened. Bots' comments and agents' own comments are dropped. A comment from another agent on this issue gets through.
- **Interjects.** A comment or TUI message that arrives mid-turn goes into the running turn. The agent takes it in after its current tool call.
- **Sets status labels** from the state each turn ends in, and takes them off while a turn runs. On Jira it can also move the issue through workflow statuses.
- **Waits.** An agent parks an issue until something happens with `agent wait`: another PR merging or closing (`pr:1257:merged`), another issue closing or its agent ending a turn in a state (`issue:1267:review`), a project job's next run ending (`job:backup`), or a time (`--until`). Any one of the conditions wakes it, with a note saying what happened. Conditions can name another project under the same supervisor (`pr:club/88:merged`). `agent issue create --wait … --until …` opens follow-up work that is held the same way until then. Webhooks, the end of each turn and each job run meet waits as they happen, and the reconcile reads what they missed. `agent schedule <time>` is the same as `agent wait --until <time>`. By hand, label an issue `scheduled` and put a hold marker in its body or a comment: `<!-- runAt: 2026-10-25T09:00Z -->`, or `<!-- waitFor: pr:1257:merged; until 2026-10-25T09:00Z -->` (`[runAt: …]` and `[waitFor: …]` on trackers that can't hide comments). `<!-- waitFor: cancel -->` drops a hold.
- **Runs long tasks.** `agent task start <name> -- <command>` runs a command for the issue detached from the agent's shell (a backfill, a cache fill), so it isn't stopped by the agent's limit on background commands, the end of the turn, a restart of the supervisor or the issue finishing. Its output goes to a log, and each task is kept as JSON in the project's state folder (`tasks/`), which a project's own health check can read; `agent task list --all` shows them. When one ends, the supervisor says how on the issue (outcome, exit code, time, last line, log) and wakes its agent with the same note unless the issue has finished. A worktree a task runs from stays until the last task there ends, then goes if its issue has finished. `agent task stop <name>` stops one and everything it started.
- **Shares what agents find**, with `settings.ambient.on` (the coordination experiment; off, agents are asked to comment on each other's issues themselves). Every comment an agent posts on its issue or PRs, of 120 characters or more, is put to a decisions model on OpenRouter (`cloudflare/clef-omni` by default) against every other active issue under the supervisor: its title, description, what it touches and its agent's last notes. Where the model's probability clears `ambient.threshold` (0.4), the comment is posted on that issue, quoted, headed **Shared automatically by open-supervisor**, with its origin and a link to the original, and reaches that issue's agent like another agent's comment. It runs in the background, so nothing waits on it. Shares are never shared again, each comment is put to each issue once (a ledger in the state folder's `ambient/` remembers, across restarts), a pair that failed is tried again when the reconcile reads the comment back (three times at most), and finished, closed and the source's own issues are left out. Every decision is appended to `ambient/decisions.jsonl` and sent to telemetry as `ambient_judged` (source, target, probability, tokens, cost, latency, shared or not); each comment's pass sends `ambient_source` (issues considered, judged, positive, shared, suppressed as already decided, failed, tokens, cost). To compare the two ways of coordinating, `agent_turn` carries `coordination` (`direct` or `ambient`) and, from agents' statuses, `shared_used`: the shares that changed what a turn did. Every comment reaching one agent from another's work, direct or shared, sends `agent_cross_comment`.
- **Webhooks plus reconcile.** Webhooks arrive through the tunnel and are signature-checked. They can also [run a project's jobs](#jobs-on-webhooks). Every few minutes a reconcile reads everything a webhook could have missed, so webhooks are optional.
- **Retries** a failed turn after 5, then 10, then 15 minutes, then waits for `r`.
- **Sweeps** once a day: an agent turn folds the corrections agents logged into the project's rule files. It pushes to the main branch, or opens a PR with `sweep.land: pr` (for a protected branch).
- **Triages** once a day: agents log what they had to work around with `agent problem` (an unset variable, a stale skill, a failing `agent` command). An agent turn groups the notes by cause, fixes what is plainly wrong in the project's own files with one commit to the main branch (or a PR, with `triage.land: pr`), and writes the rest to `triage.md` in the project's state folder. With `triage.openIssues` it opens or updates one issue per cause in the project's tracker instead. Causes in open-supervisor itself are filed on its tracker when the supervisor watches open-supervisor as one of its projects, and otherwise come back as a link that opens an issue here, filled in. The header counts the notes waiting.
- **Usage and metrics.** It shows the Claude subscription's 5-hour and 7-day windows in the header. It sends the telemetry connections an `agent_turn` event per turn (tokens, cost, duration, state), an `agent_usage` event, an `agent_problem` event for each problem an agent logs, an `agent_wait_ended` event each time a wait ends (what ended it, the condition's kind, how long it lasted), the ambient sharing events (above), and the host watch's `host_metrics` and `host_alert` (below).
- **Watches the host**, with `settings.host.minutes` set. Agents, their dev servers and their worktrees share one machine, so running out of CPU, memory or disk stalls every project at once. Each reading goes to telemetry as `host_metrics`: the load averages and `load_per_cpu`, `memory_used_pct` and bytes, and each disk the worktrees, the state folder, the checkouts and `host.disks` are on, with the fullest under `disk_used_pct`. A reading going over its threshold logs a warning, shows in red in the TUI's header and sends `host_alert` (`metric`, `value`, `threshold`, `over: true`, and `path` for a disk). It sends `over: false` once the reading drops back under 95% of the threshold. Load and memory are read on Linux and macOS; disks on Windows too. Whether or not the watch is on, `agent worktree setup` and a job's fresh checkout refuse to make a new worktree whose checkout would leave less free than `host.disk` keeps (10% by default, at most 10 GB); `agent worktree setup` first removes the project's spare worktrees and tries again, then names the ones it couldn't take. Every 6 hours the supervisor removes the spare worktrees: those of finished issues; those of issues whose PRs have all merged or closed, when everything on them is pushed (never one parked on a wait or with a turn running); and, once untouched for a day with nothing uncommitted, those no issue record holds (a dropped issue's). Their branches stay, and an agent resumed after its worktree went is told so, with how to make it again.

### What agents get

The playbooks (`internal/playbooks`) reach Claude Code as a plugin of skills: `/open-supervisor:work-issue`, `/open-supervisor:sweep`, `/open-supervisor:triage`. Agents touch the tracker and code host only through `open-supervisor agent …`, so the playbooks are the same everywhere:

```
agent context | issue view | issue comment | issue list | issue create | pr list | pr create | pr view
agent pr edit | pr comment | pr reply | checks | attachment upload | attachment get | worktree setup | worktree remove | task start | task list | task stop | wait | schedule
agent correction | problem | inflight
```

Rules particular to a project (what must pass, how to run its dev server, whether to take screenshots and where they go) belong in its own `CLAUDE.md` or `AGENTS.md`. A project can also add to a playbook in its checkout, in `.open-supervisor/playbooks/<name>.md` (`work-issue`, `sweep`, `triage`). Each turn hands the file over after the message, under "This project's additions to the playbook". Additions only ever add to the playbook; nothing replaces it.

The supervisor sets `OPEN_SUPERVISOR_REMOTE` for every turn. Unless `git.sign` is on, it also turns off commit and tag signing through `GIT_CONFIG_*`. `open-supervisor playbooks` writes the plugin out and prints its path, for `claude --plugin-dir <path>` by hand.

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
| `c` | claim an issue by key: `12`, `PROJ-12` (through the routes, for a project fed by a source), `project#12` (into that project whatever the routes say) |
| `!` | run a shell command: pick where (the folder open-supervisor started in, or a project's checkout), then type it. It joins the list as a shell task (`!1`, `!2`…) with its output on its detail page; `r` runs it again, `k` kills it, `d` drops it. Shell tasks belong to this run: quitting kills those still going |
| `R` | reconcile now |
| `s` | sweep now |
| `T` | triage now |
| `J` | job runs: pick a job, then one of its kept runs, for its trigger, timing, exit code and output; `r` on the run runs the job again |
| `j` | run a job now: pick it from every project's jobs |
| `,` | edit the config: general settings, connections (with a connection test), projects |
| `l` | show or hide the log |
| `q` | quit; running turns carry on and are adopted at the next start |

## Development

```sh
make test       # go test -race ./...
make build      # bin/open-supervisor
make snapshot   # every platform's release build into dist/ (needs goreleaser)
```

Versions follow [semantic versioning](https://semver.org), worked out from the commits. Write commit messages as [Conventional Commits](https://www.conventionalcommits.org): `fix: …` makes a patch release, `feat: …` a minor one, and a `!` after the type (`feat!: …`) or a `BREAKING CHANGE:` footer a major one. Other types (`docs`, `test`, `ci`, `chore`, `refactor`) don't release anything on their own. A `commitlint` check on each PR holds its commits and its title to this, so a squash merge lands a conventional message whichever one GitHub uses.

To release, run the **release** workflow from the Actions tab (tick *dry run* to see the next version and its notes first). semantic-release picks the version from the commits since the last tag, adds the notes to `CHANGELOG.md`, commits and tags that on `main`, and goreleaser then builds every platform, publishes the GitHub release with the same notes and updates the Homebrew tap. The tap update needs an `alsiola/homebrew-tap` repo and a `HOMEBREW_TAP_TOKEN` secret.
