# open-supervisor

## Commits

Write every commit message as a [Conventional Commit](https://www.conventionalcommits.org). The release workflow reads them to pick the next version and write `CHANGELOG.md`, so the type decides what ships:

- `fix: …` makes a patch release, `feat: …` a minor one.
- `feat!: …` (a `!` after the type), or a `BREAKING CHANGE: …` footer, makes a major one. Use it when config, the `agent` commands or the playbooks change in a way existing setups have to adapt to.
- `docs`, `test`, `ci`, `build`, `refactor`, `perf` and `chore` don't release anything on their own.

Add a scope when the change sits in one area: `feat(jobs): …`, `fix(linear): …`. Keep the subject in the imperative, lower case and without a full stop: it becomes the changelog line, so write it for someone reading the release notes.

When a commit both fixes and adds, use the type of the bigger change, or split it into two commits.

## Before a PR

`go vet ./...` and `go test -race ./...` must pass (CI runs both on Linux and macOS, and builds on Windows).
