# Coming from the original supervisor

The original TypeScript supervisor maps onto open-supervisor like this:

| Before | Now |
| --- | --- |
| `projects.json` | `projects:` in `~/.config/open-supervisor/config.yaml` |
| `.env` settings (`SUPERVISOR_MAX_TURNS` …) | `settings:` in the config, editable with `,` in the TUI |
| `<root>/.claude/supervisor.json` jobs | `jobs:` on the project, or `<root>/.open-supervisor.yaml` |
| `<root>/.local/issue-watch/` | `~/.local/state/open-supervisor/projects/<name>/` |
| `<root>/.claude/worktrees/issue-N` | `~/.local/share/open-supervisor/worktrees/<name>/issue-N` |
| `<root>/.claude/temp/corrections/` | `…/projects/<name>/corrections/` (`agent correction` writes them) |
| `/supervisor:work-issue` and the plugin's scripts | `/open-supervisor:work-issue` and `open-supervisor agent …` |
| the Claudesworth `gh` shim | the GitHub connection's `botAppId` / `botPrivateKey` |
| `PUBLIC_POSTHOG_PROJECT_TOKEN` | a `posthog` connection listed in `settings.telemetry` |
| `NGROK_DOMAIN` | an `ngrok` connection's `domain`, named in `settings.tunnel` |

To keep the labels and branches your repos already use, set:

```yaml
settings:
  branch: claude/issue-{key}
  labels:
    claimed: claude-in-progress
    help: claude needs help
    review: ready for review
    done: might be done
```

Agent comments posted by the old `reply.sh` (`<!-- claude-agent -->`, `<!-- claude-agent from #N -->`) are still recognised.

Running issues don't carry over: their state was in the old format. Stop the old supervisor, let its turns finish, then start this one. It reclaims issues that are still open only if they are reopened or claimed by hand with `c`, because the old one left its claimed label on them.

The port also changes. The old supervisor listened on 8787. Run only one of them at a time, or set `settings.port`.

Project-specific rules from the old work-issue skill belong in each project's `CLAUDE.md` now: the Netlify drawer comments, the `agent-browser` path and the `pnpm` install.
