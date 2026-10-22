# Changelog

## [1.0.1](https://github.com/alsiola/open-supervisor/compare/v1.0.0...v1.0.1) (2026-10-22)

### Bug Fixes

* **engine:** deliver other agents' comments posted through a bot app ([#19](https://github.com/alsiola/open-supervisor/issues/19)) ([81a55f1](https://github.com/alsiola/open-supervisor/commit/81a55f1bd833541d7b8f070c6a23fa9644718ba2))

## [1.0.0](https://github.com/alsiola/open-supervisor/compare/v0.4.0...v1.0.0) (2026-10-22)

### ⚠ BREAKING CHANGES

* `agent screenshots` and the open-supervisor-screenshots
branch are gone, and work-issue no longer asks for screenshots or warns
about a shared local database. Projects that want either should say so in
their own rules or in .open-supervisor/playbooks/work-issue.md.

Co-authored-by: claudesworth[bot] <342356163+claudesworth[bot]@users.noreply.github.com>
Co-authored-by: Claude Opus 5.5 <noreply@anthropic.com>

### Features

* **engine:** hand off cold sessions to a fresh one, as an a/b test ([#18](https://github.com/alsiola/open-supervisor/issues/18)) ([85e5838](https://github.com/alsiola/open-supervisor/commit/85e583899aaba165633486fb11df3d2a1cb868c7)), closes [#17](https://github.com/alsiola/open-supervisor/issues/17)
* loosen the coupling to one way of working ([#15](https://github.com/alsiola/open-supervisor/issues/15)) ([ef3891c](https://github.com/alsiola/open-supervisor/commit/ef3891c33676bebcff15cbda075e454a6a2a4475))

### Bug Fixes

* **claudecode:** count a resumed turn's own cost and tokens, not the session's running total ([#16](https://github.com/alsiola/open-supervisor/issues/16)) ([bd0bb5a](https://github.com/alsiola/open-supervisor/commit/bd0bb5afcb3ec244e2d5a161b51f575cc02d6b7d))

## [0.4.0](https://github.com/alsiola/open-supervisor/compare/v0.3.0...v0.4.0) (2026-10-21)

### Features

* agents report the problems they work around, and a daily triage acts on them ([#12](https://github.com/alsiola/open-supervisor/issues/12)) ([d66ad28](https://github.com/alsiola/open-supervisor/commit/d66ad2809517b9bb38388941a67df6881eff3983)), closes [#11](https://github.com/alsiola/open-supervisor/issues/11)

## [0.3.0](https://github.com/alsiola/open-supervisor/compare/v0.2.2...v0.3.0) (2026-10-21)

### Features

* **tui:** run jobs from a j picker and drop job hotkeys ([#10](https://github.com/alsiola/open-supervisor/issues/10)) ([30af7db](https://github.com/alsiola/open-supervisor/commit/30af7db0fcb3002300f350d1b6011fda497a96c5))

## [0.2.2](https://github.com/alsiola/open-supervisor/compare/v0.2.1...v0.2.2) (2026-10-21)

### Bug Fixes

* **worktree:** skip setup when none of its watched files is in the checkout ([#8](https://github.com/alsiola/open-supervisor/issues/8)) ([c3cd22a](https://github.com/alsiola/open-supervisor/commit/c3cd22a4c59eb5ad087639e592786312e5b8d9f9))
