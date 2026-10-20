---
name: setup
description: Set open-supervisor up for its owner, from nothing or over an existing config. Finds out which coding agent, code hosts, issue trackers and repos they use and how issues map to repos, then writes the config with `open-supervisor config` commands, tests each connection, and checks the lot with doctor. Started by `open-supervisor init --with <agent>`, as an interactive session with the owner.
---

# Set up open-supervisor

open-supervisor watches issue trackers, claims open issues, and works each one in its own coding-agent session in a git worktree, a turn at a time; comments, check results and merges become the next turn's events. Your job is to set it up for the person you're talking to: find out what they use, write the config, and leave them with a supervisor that `doctor` passes and they understand.

The command is `{{bin}}`. Every change goes through `{{bin}} config …`: never edit the config file by hand. Each `config` command saves at once and then prints `still to do:` with what the config still lacks. That list is normal while you build the config step by step, and empty when you're done.

This is a conversation. Open by saying in two or three sentences what you'll do together, then look around (below) before your first question, so the questions you ask are about what you found. Go one topic at a time, and wait for answers.

## Ground rules

- **Secrets never pass through you.** Never ask for a token in the chat, never put one on a command line, never echo one. A secret field takes a reference instead:
  - `env:LINEAR_API_KEY`: the variable must be set wherever the supervisor runs, not only in this shell.
  - `cmd:op read op://vault/linear/token`: any command that prints the secret (1Password, `pass`, `security find-generic-password -w …`).
  - Or the owner runs `{{bin}} config secret <connection> <field>` in a terminal of their own. It asks for the secret without showing it and stores it in the OS keyring.

  Offer these, let them choose, and wait for them to say it's done.
- **GitHub** needs no token when the `gh` CLI is signed in: the default `auth=gh` uses it. Check with `gh auth status`; if they're signed out, they run `gh auth login`.
- **Ask, don't assume.** This includes which repos are in scope, how issues map to repos, and anything that labels, moves or comments on issues. Read what you can find yourself (git remotes, `gh repo list`, `config source inspect`) and present it for them to confirm.
- **Explain as you go,** in a sentence or two, what each step means for them ("claimed issues get the `agent-in-progress` label").
- **Keep questions few and concrete.** Group them, and offer the likely answer: "Your checkouts under ~/Code with GitHub remotes are web, api and ios. All three?"

## Find out what's there

Start by looking, not asking:

```
{{bin}} config path
{{bin}} config show          # the config as read, defaults included, then what it still lacks
{{bin}} adapters             # the services this build can talk to, and the roles each fills
{{bin}} adapters <type>      # one adapter's fields: connection fields, then per-role binding fields
```

An existing config may be half done: earlier `init` runs, a wizard left part way, a hand edit. Build on it rather than starting over. Tell the owner what's already there and what `config show` says is still to do, then ask whether to finish that or change something. Never remove a connection, project or source the owner didn't ask you to remove.

Look for the owner's checkouts. Try the current directory and the usual places (`~/Code`, `~/src`, `~/dev`, `~/projects`), one or two levels deep. Use `git -C <dir> remote get-url origin` to see which service each one is on. Check which tools are installed (`claude`, `gh`, `ngrok`, `cloudflared`) with `command -v`.

## Questions to settle

1. **Coding agent.** Usually `claude-code`, the agent running you. The defaults are a good start: model `opus`, and permission mode `bypassPermissions`, because turns run unattended. Say what that permission mode means and let them choose. Note too that by default sessions use the machine's Claude login rather than an API key.
2. **Code hosts.** Where the repos and PRs are: GitHub (`gh` login, a token, or a GitHub App) or Bitbucket.
3. **Issue trackers.** GitHub issues on each repo, Jira, or Linear. A tracker can hold the issues for many repos; see below.
4. **Projects.** Which checkouts the supervisor works in. Each project is one git checkout. Ask which branch PRs should target only if it isn't the repo's default.
5. **How issues map to projects** (when one tracker feeds several repos). See the routes section below.
6. **Webhooks.** Without a tunnel the supervisor polls every 15 minutes (`settings.reconcileMinutes`). With one, it reacts in seconds:
   - `ngrok`: a reserved domain keeps the URL stable;
   - `cloudflared`;
   - `static`: a URL they already route to this machine's port 8787.

   Polling always runs as well, so nothing is missed either way. Ask whether they want webhooks, and don't install anything for them.
7. **Limits.** How many turns may run at once (`settings.maxTurns`, default 4) and how many issues each project holds at a time (`settings.maxOpenPerProject`, default 10). Suggest starting low, around 2 and 3, so they can watch it work.

## Write the config

Connections first. Each is named; the name is used everywhere else. `connection set` tests the connection unless you pass `--no-test`; if a test fails, fix it before going on.

```
{{bin}} config connection set claude --type claude-code
{{bin}} config connection set github --type github                      # signs in through gh
{{bin}} config connection set linear --type linear --set token=env:LINEAR_API_KEY
{{bin}} config connection set jira --type jira --set baseUrl=https://acme.atlassian.net --set email=me@acme.com --set token=env:JIRA_TOKEN
{{bin}} config set settings.agent claude
```

Then the projects. The repo is detected from the checkout's origin. `--issues` gives a project its own tracker; for a tracker on the same service as the code (GitHub issues), that's the code host connection again.

```
{{bin}} config project set web --root ~/Code/web --code github --issues github
{{bin}} config project set api --root ~/Code/api --code github --issues jira --tracker project=API
```

Other `project set` flags:
- `--branch 'agent/{key}'`
- `--base main`
- `--copy .env`: files copied into each worktree.
- `--setup 'npm ci'`: a command run in each new worktree, with `--setup-when package-lock.json` to rerun it when that file changes.
- `--agent <connection>`
- `--disabled`

Suggest `--copy` and `--setup` from what you see in the checkout (`.env` files, lockfiles).

Settings take a dotted key:

```
{{bin}} config set settings.maxTurns 2
{{bin}} config set settings.labels.skip.+ wontfix          # + appends to a list
{{bin}} config get settings.labels
{{bin}} config unset settings.maxTurns                     # back to the default
```

## One tracker, several repos: sources and routes

When one tracker (a Linear team, a Jira project, a GitHub planning repo) holds issues for several repos, don't give each project the tracker on its own. Each project would read every issue and claim the same ones. Instead, make one source and route each issue to one project:

1. **Create the source** with the tracker's binding, then read its open issues to see what routes could test:

   ```
   {{bin}} config source set linear --tracker linear --set team=ENG
   {{bin}} config source inspect linear
   ```

   `inspect` lists every property the open issues carry, with their most common values:
   - Linear: labels, project, team, state, cycle, priority, assignee.
   - Jira: project, issuetype, components, priority, and any custom fields set in the binding's `fields`.
   - GitHub: labels, milestone, assignees, type.

2. **Propose a mapping from the data.** For example, "label `web` → web, `api` → api, everything else → web?" Then confirm it with the owner.

3. **Write the routes.** The first route that matches takes the issue. Several values for a property are ORed; several `--when` flags are ANDed. `*` and `?` are wildcards, and case doesn't matter. A route with no `--when` takes everything the others don't, and goes last.

   ```
   {{bin}} config project set web --root ~/Code/web --code github --source linear
   {{bin}} config route set linear web --when labels=web,frontend
   {{bin}} config route set linear api --when 'project=API*'
   {{bin}} config route set linear ios --match '{labels: [ios], not: {state: [Triage]}}'
   {{bin}} config route set linear web --also                # web also takes whatever's left, after the rest
   {{bin}} config route list
   ```

   `route set` replaces the source's routes to that project, so without `--also` the last line above would have dropped web's label route. `--also` adds one more, and `--position N` places it (never after the route taking everything else).

4. **Check where the open issues land:**
   - `{{bin}} config source inspect linear` again: the counts per route, and what nothing takes;
   - `{{bin}} route <issue-key>` for a single issue.

   Show the owner the numbers.

5. **Decide what happens to issues no route takes.** By default they're left alone and listed in the TUI. `--unrouted label` on `source set` labels them `needs-routing` so people can see them. Ask which they want.

A project with its own tracker that turns out to share it shows a warning when saved, along with the commands to make a source. If `config project set` prints one, follow it up.

## Behaviour worth telling the owner

- **What gets claimed.** An open issue is claimed when it lacks the claimed label (`agent-in-progress`) and every skip label (`backlog`, `human`, `milestone`, `later`).
- **Linear states.** In Linear only issues in Todo or In Progress are claimed (the binding's `states`); Backlog waits.
- **What the agents do.** They label and comment on issues and open PRs as the person whose credentials they use, unless the connection has a bot account.
- **Status moves.** Jira transitions and Linear state moves happen only when their `transition…`/`state…` binding fields are set.

## Finish

1. Run `{{bin}} doctor`. It checks every connection and project, and shows what each would claim now and where each source's issues go. Fix anything it flags.
2. Make sure `{{bin}} config show` prints nothing they didn't agree to.
3. Summarise in a few lines:
   - the connections;
   - the projects and where each gets its issues;
   - the routes;
   - webhooks or polling;
   - the limits.
4. Tell them:
   - start it with `{{bin}}`, which opens the TUI; `--dry-run` first if they'd like to watch without anything changing;
   - `{{bin}} init` reopens the wizard;
   - pressing `,` in the TUI edits the config;
   - `{{bin}} init --with <agent>` brings you back.

Don't start the supervisor yourself: it's a long-running TUI, and they should run it in their own terminal.
