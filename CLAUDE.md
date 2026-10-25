# open-supervisor

## Commits

Write every commit message as a [Conventional Commit](https://www.conventionalcommits.org). The release workflow reads them to pick the next version and write `CHANGELOG.md`, so the type decides what ships:

- `fix: …` makes a patch release, `feat: …` a minor one.
- `feat!: …` (a `!` after the type), or a `BREAKING CHANGE: …` footer, makes a major one. Use it when config, the `agent` commands or the playbooks change in a way existing setups have to adapt to.
- `docs`, `test`, `ci`, `build`, `refactor`, `perf` and `chore` don't release anything on their own.

Add a scope when the change sits in one area: `feat(jobs): …`, `fix(linear): …`. Keep the subject in the imperative, lower case and without a full stop: it becomes the changelog line, so write it for someone reading the release notes.

When a commit both fixes and adds, use the type of the bigger change, or split it into two commits.

The `commitlint` check lints every commit on a PR and the PR title (`.commitlintrc.json`). Give the PR a conventional title too: a squash merge of more than one commit lands under it. If a commit fails the check, reword it and force-push the branch.

## Before a PR

`go vet ./...` and `go test -race ./...` must pass (CI runs both on Linux and macOS, and builds on Windows).

When a change only works if a convention is followed (commit format, naming), add the rule and the check that enforces it in the same PR. Don't leave them as suggestions because the repo has no rules file or lint yet.

## Design

- Put behaviour that varies by project (workflow steps, wording, categories) in the repo's own agent files (`CLAUDE.md`, `AGENTS.md`, skills), not behind an open-supervisor config setting.
- Let project playbook files and rules only add to the core playbooks, never replace them. The core lifecycle stays fixed.
- Measure each arm of an experiment through to the outcome (later turns, tokens, tool calls, time, completion), not on the drawn turn alone, and record how many times each arm was assigned so feedback counts have a denominator.
- Design agent-facing features from the interface in: an agent asks the supervisor for an outcome through an `agent` command and never needs to know about markers, labels or tracker conventions. When asked what agents should know about a feature, answer that, not how the mechanism works.
- When an issue asks for thoughts on an idea, lead with the smallest version that would work and the evidence for or against it. Offer the fuller design (new commands, events, TUI, rollout) only when asked.
