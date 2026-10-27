# Changelog

## [1.5.1](https://github.com/alsiola/open-supervisor/compare/v1.5.0...v1.5.1) (2026-10-27)

### Bug Fixes

* **engine:** hold an agent at its usage limit before its turn is marked failed ([#80](https://github.com/alsiola/open-supervisor/issues/80)) ([eda6159](https://github.com/alsiola/open-supervisor/commit/eda6159bcc832b8c9f6c4d8c5c2cc4652b474adb))

## [1.5.0](https://github.com/alsiola/open-supervisor/compare/v1.4.0...v1.5.0) (2026-10-27)

### Features

* share agents' findings with the other issues they bear on, as an opt-in coordination mode ([#73](https://github.com/alsiola/open-supervisor/issues/73)) ([d76a817](https://github.com/alsiola/open-supervisor/commit/d76a817bbea98092bacb719c6c66defdae75782b))

### Bug Fixes

* **engine:** list issues labelled claimed with no session instead of skipping them silently ([#71](https://github.com/alsiola/open-supervisor/issues/71)) ([c7d597c](https://github.com/alsiola/open-supervisor/commit/c7d597c5e14510cafaa92f6fa2587632cc35793c)), closes [#70](https://github.com/alsiola/open-supervisor/issues/70)
* **toolbelt:** report the PR that agent pr create made when the host answers 5xx ([#77](https://github.com/alsiola/open-supervisor/issues/77)) ([dd51ba1](https://github.com/alsiola/open-supervisor/commit/dd51ba115e5d2dd5d600f83bb9b30867ab2b710e))
* **worktree:** clear landed issues' worktrees and tell an agent when its worktree is gone ([e44185e](https://github.com/alsiola/open-supervisor/commit/e44185e8dd0e1b0575a5175be596b82fbde9b0a2))

## [1.4.0](https://github.com/alsiola/open-supervisor/compare/v1.3.0...v1.4.0) (2026-10-26)

### Features

* **engine:** hand off every cold session to a fresh one and end the handoff trial ([#69](https://github.com/alsiola/open-supervisor/issues/69)) ([da4727d](https://github.com/alsiola/open-supervisor/commit/da4727d587988005ecffcd82ab43772cca155e4d))
* **toolbelt:** add agent pr edit to change a PR's title and body ([#67](https://github.com/alsiola/open-supervisor/issues/67)) ([1b5107d](https://github.com/alsiola/open-supervisor/commit/1b5107d81a2151130a6d9cbcf727ed1adfc1b3cf)), closes [#66](https://github.com/alsiola/open-supervisor/issues/66)

## [1.3.0](https://github.com/alsiola/open-supervisor/compare/v1.2.1...v1.3.0) (2026-10-25)

### Features

* **toolbelt:** add agent task to run long commands past the turn and the issue ([#65](https://github.com/alsiola/open-supervisor/issues/65)) ([7df2456](https://github.com/alsiola/open-supervisor/commit/7df24562cec920cacdaf38e21b2fdf68fb55b136)), closes [#58](https://github.com/alsiola/open-supervisor/issues/58)
* **worktree:** refuse a new worktree on a full disk and sweep spare ones ([#63](https://github.com/alsiola/open-supervisor/issues/63)) ([3dcf60c](https://github.com/alsiola/open-supervisor/commit/3dcf60cc9f6b15c66fdd4d96b211fff2d2d1824c))

### Bug Fixes

* **engine:** wait for the checks the base branch requires before reporting a pass ([#64](https://github.com/alsiola/open-supervisor/issues/64)) ([c37edf1](https://github.com/alsiola/open-supervisor/commit/c37edf144e784324604040b1705a384f9d7b4586)), closes [#59](https://github.com/alsiola/open-supervisor/issues/59)
* **github:** fall back to the bot token and to git when rate limited ([#61](https://github.com/alsiola/open-supervisor/issues/61)) ([de775ef](https://github.com/alsiola/open-supervisor/commit/de775efd36362ce8ccd7d8217d84e42c60947a0e))
* **worktree:** create the .local/ scratch folder in new worktrees ([#62](https://github.com/alsiola/open-supervisor/issues/62)) ([265fe98](https://github.com/alsiola/open-supervisor/commit/265fe98b849746a0c67de5b71bf2135cb0ae68e9)), closes [#56](https://github.com/alsiola/open-supervisor/issues/56)

## [1.2.1](https://github.com/alsiola/open-supervisor/compare/v1.2.0...v1.2.1) (2026-10-25)

### Bug Fixes

* **playbooks:** keep sweep and triage body files out of the checkout ([#54](https://github.com/alsiola/open-supervisor/issues/54)) ([f904597](https://github.com/alsiola/open-supervisor/commit/f90459784d91f1a1b505a7565d33c1eed0ea8a8e))

## [1.2.0](https://github.com/alsiola/open-supervisor/compare/v1.1.0...v1.2.0) (2026-10-24)

### Features

* **engine:** watch the host's cpu load, memory and disk space ([#51](https://github.com/alsiola/open-supervisor/issues/51)) ([e1a09f8](https://github.com/alsiola/open-supervisor/commit/e1a09f8f873675f68244188bd82962c26f15f6aa))
* **github:** send conditional requests and pause on rate limits ([#53](https://github.com/alsiola/open-supervisor/issues/53)) ([85bf37e](https://github.com/alsiola/open-supervisor/commit/85bf37edf1e29025d1315c22ed22577ad142fbcb))

## [1.1.0](https://github.com/alsiola/open-supervisor/compare/v1.0.1...v1.1.0) (2026-10-24)

### Features

* **engine:** ask the same blind feedback question after both arms of the handoff experiment ([#26](https://github.com/alsiola/open-supervisor/issues/26)) ([8abdffa](https://github.com/alsiola/open-supervisor/commit/8abdffaa843fb04e2aedc2e1de867701b1837578))
* **engine:** retry a turn the usage limit stopped once the limit lifts ([#33](https://github.com/alsiola/open-supervisor/issues/33)) ([8923d2f](https://github.com/alsiola/open-supervisor/commit/8923d2f97e6562d080ce0d46fa918d358191c797))
* **engine:** wake agents on events with agent wait ([#28](https://github.com/alsiola/open-supervisor/issues/28)) ([f527dcb](https://github.com/alsiola/open-supervisor/commit/f527dcbca325b75cecf6c7cd2922f30dcd5fa4d5))
* **playbooks:** keep agents from commenting on each other's issues without cause ([#45](https://github.com/alsiola/open-supervisor/issues/45)) ([811806d](https://github.com/alsiola/open-supervisor/commit/811806d41e9b6cbb36a0ce0b33344e8e3066aaff))
* **toolbelt:** add agent attachment to upload and download comment files ([#47](https://github.com/alsiola/open-supervisor/issues/47)) ([c90662a](https://github.com/alsiola/open-supervisor/commit/c90662a9d916bfa6fe3b701f65037cb94e662a7c)), closes [#39](https://github.com/alsiola/open-supervisor/issues/39)
* **triage:** file open-supervisor causes on its tracker when it is watched ([#36](https://github.com/alsiola/open-supervisor/issues/36)) ([f34e750](https://github.com/alsiola/open-supervisor/commit/f34e750044a0d737b9bc8ce20ef95d99eba71691))

### Bug Fixes

* **engine:** hold an issue on a runAt marker in its body without the scheduled label ([#31](https://github.com/alsiola/open-supervisor/issues/31)) ([8bf7c69](https://github.com/alsiola/open-supervisor/commit/8bf7c69a7e0c38601c120e3385d95581e35a5d71))
* **engine:** pass on other agents' comments and end dropped issues' trials ([#20](https://github.com/alsiola/open-supervisor/issues/20)) ([b1b8021](https://github.com/alsiola/open-supervisor/commit/b1b80210199a1b670b4d65422b496cdc25f1ad0b))
* **engine:** re-copy a worktree's listed files before every turn ([#49](https://github.com/alsiola/open-supervisor/issues/49)) ([c5ec9f2](https://github.com/alsiola/open-supervisor/commit/c5ec9f29165cf1ca58b4cbe07e16ca01265c362a))
* **engine:** take the status label off while a turn runs ([#24](https://github.com/alsiola/open-supervisor/issues/24)) ([f532b44](https://github.com/alsiola/open-supervisor/commit/f532b4422e0f32cbc0de1e998625319ed57c10c9))
* **github:** find an issue's PRs from its timeline, not the search API ([#46](https://github.com/alsiola/open-supervisor/issues/46)) ([6b706ec](https://github.com/alsiola/open-supervisor/commit/6b706ec209a604d0da2a172b3178397e8ddcec9f))
* **jobs:** fast-forward the main checkout before a job runs in it ([#22](https://github.com/alsiola/open-supervisor/issues/22)) ([5584bf8](https://github.com/alsiola/open-supervisor/commit/5584bf8f79f728fc1bfbf0c7b0bd572b674b60d2)), closes [#21](https://github.com/alsiola/open-supervisor/issues/21)
* **jobs:** load jobs added to a project's .open-supervisor.yaml without a restart ([#48](https://github.com/alsiola/open-supervisor/issues/48)) ([b695804](https://github.com/alsiola/open-supervisor/commit/b6958046415ed2f0407112818e190085856b5231)), closes [#37](https://github.com/alsiola/open-supervisor/issues/37)
* **worktree:** stop processes whose working directory is in a removed worktree ([#43](https://github.com/alsiola/open-supervisor/issues/43)) ([a1b5617](https://github.com/alsiola/open-supervisor/commit/a1b5617f7558ad776d43d07ff2b55d71f089a740))

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
