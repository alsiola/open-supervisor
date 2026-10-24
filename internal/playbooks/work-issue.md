---
name: work-issue
description: Work an issue in turns under open-supervisor. The first turn reads the issue, builds the change on the issue's branch in its own worktree, opens a PR and ends; later turns, started by the supervisor, act on new comments, check results, previews and merges. Investigation issues get findings on the issue instead of a PR. Started by the supervisor; usable by hand as /open-supervisor:work-issue <key> from a project's checkout.
---

# Work an issue

You run in turns. The supervisor starts your session on `work-issue <key>` and resumes it with `work-issue <key> events` whenever there is news: comments, a PR's checks finishing, a preview going up, a merge, a close. Between turns nothing runs, so never wait or poll for anything (checks, a preview, a reply): do what you can, then end the turn with your status, and the supervisor wakes you when the rest arrives.

Arguments: `<key>` is the issue (`12` on GitHub, `PROJ-12` on Jira). `<key> events` is a later turn: the events since your last one follow in the message. Anything else is a note from the supervisor (usually about another issue in flight; see Other agents); follow it before step 3.

## Your tools

The project may use GitHub, Bitbucket, Jira or something else; you don't need to know which. Everything that touches the tracker or the code host goes through the supervisor's own command, `$OPEN_SUPERVISOR agent …`, which already knows the project, the issue, its branch and its worktree:

| Command | Does |
| --- | --- |
| `$OPEN_SUPERVISOR agent context` | the project, issue, branch, worktree, base branch, remote and where the rules are |
| `$OPEN_SUPERVISOR agent issue view` | the issue and every comment on it |
| `$OPEN_SUPERVISOR agent issue comment <file>` | comment on the issue (`--issue <key>` for another agent's issue) |
| `$OPEN_SUPERVISOR agent issue create --title <t> [--wait <condition>] [--until <time>] <file>` | open a new issue; with `--wait` or `--until`, it isn't worked until then (see Waiting) |
| `$OPEN_SUPERVISOR agent worktree setup` | create or reuse the issue's worktree on its branch; prints the path last |
| `$OPEN_SUPERVISOR agent worktree remove` | stop anything running from it and remove it (the branch stays) |
| `$OPEN_SUPERVISOR agent pr list` | the issue's PRs, open or not |
| `$OPEN_SUPERVISOR agent pr create --title <t> --body-file <file>` | open a PR from the issue's branch |
| `$OPEN_SUPERVISOR agent pr view <pr>` | a PR and every comment on it |
| `$OPEN_SUPERVISOR agent pr comment <pr> <file>` | comment on a PR's conversation |
| `$OPEN_SUPERVISOR agent pr reply <pr> <comment-id> <file>` | reply in an inline review thread |
| `$OPEN_SUPERVISOR agent checks [pr]` | every check on the PR's head, with links |
| `$OPEN_SUPERVISOR agent wait <condition>... [--until <time>] <file>` | park the issue until something happens or a time comes (see Waiting) |
| `$OPEN_SUPERVISOR agent correction --area <area> <file>` | log a correction (see Log corrections) |
| `$OPEN_SUPERVISOR agent problem --kind <kind> <file>` | log a problem you worked around (see Report problems) |
| `$OPEN_SUPERVISOR agent inflight` | the other issues in flight and what each touches |

A `<file>` can be `-` to read from stdin, e.g. `printf '…' | $OPEN_SUPERVISOR agent issue comment -`. Post every comment this way: it marks the comment as yours, so it is never handed back to you as news. Use `git` itself for commits and pushes.

Every turn starts in the project's main checkout. The project's own rules (`CLAUDE.md`, `AGENTS.md` or wherever `agent context` says) hold what is particular to it: what must pass before a PR, how to run its dev server and database, the checks its PRs get, whether and how to show a change in screenshots. Where they differ from this playbook, they win. A project can also keep additions to this playbook in `.open-supervisor/playbooks/work-issue.md`. The supervisor hands them to you with each turn, under "This project's additions to the playbook". Follow them as well as this playbook, never in place of it.

## Waiting

When the work can't go on until something happens, park the issue and let the supervisor wake you when it does. Don't park it until a time just to come back and check.

- `$OPEN_SUPERVISOR agent wait pr:1257:merged <file>`: until another PR merges (`:closed` for closed without merging). A PR that ends the other way wakes you too.
- `agent wait issue:1267:closed <file>`: until another issue closes.
- `agent wait issue:1267:review <file>`: until the agent on another issue ends a turn in that state (`building`, `review`, `help`, `done`, `finished`), e.g. once it has reported.
- `agent wait job:backup <file>`: until the next run of one of the project's jobs ends.
- `agent wait --until 2026-10-25T09:00Z <file>`: until a time (a match being played, a date the issue names). Use an ISO time with `Z` or an offset.

Write `<project>/` before the number, key or name for another project under the same supervisor (`pr:club/88:merged`). Give several conditions and any one of them wakes you. Add `--until` to any wait as the latest it should last; use it whenever what you're waiting for might never happen. The file says what you're waiting for and why. If it has already happened, the command says so: carry on instead.

The supervisor wakes you with what happened (or that the time came first). Comments and check results still reach you in the meantime, and the wait carries on after those turns. A new `wait` replaces the last one, and `agent wait --cancel <file>` drops it. End the turn with the state the work is in (`review` if the owner has something to look at, else `done`). Don't use this to wait for your own PR's checks, its preview, its merge or a reply: the supervisor already resumes you for those.

When follow-up work shouldn't start yet (wave 2 once wave 1 merges, a check once a job has run), open it now with the same conditions: `agent issue create --title <t> --wait pr:1257:merged <file>`. The supervisor holds it and starts it when the condition is met, so nobody has to remember to open it later.

## Ending a turn

Every turn ends with a status (the structured result the supervisor asks for):

- `state`: one of
  - `building`: you pushed and the checks or the preview haven't landed. The supervisor resumes you when they do.
  - `review`: something is up for the owner: the PR with its preview, findings, or changes pushed and reported after feedback.
  - `help`: you asked the owner something you can't go on without (a decision, access, money, an ask you can't read). Carry on with anything the question doesn't block before ending the turn.
  - `done`: everything the thread asked for is handled and only a merge or close is left.
  - `finished`: the PR merged or closed, or the issue closed with no PR, and you have cleaned up (see Cleaning up).
- `summary`: what the turn did, in one line.
- `prs`: every PR number that belongs to this issue, once there is one.
- `preview`: the deploy preview's URL, once you know it.
- `touches`: what the work changes (pages, data, pipeline steps, packages), in a few words. Other agents on the same project read it to spot overlap.
- `handoff`: a short note to whoever picks the issue up next, in case your session has gone cold by then (see Fresh session). Write it every turn. Say what is pending, the judgement calls you made and why, and what to check next. Leave out what the issue, the PR and git already show.

The supervisor sets the issue's status labels (and, on some trackers, its workflow status) from `state`; never change those yourself.

## Other agents

The first turn's message lists the issues in flight, with what each touches (`agent inflight` prints it again). When the note says another issue overlaps yours, or you notice overlap yourself (the same page, component, dataset or pipeline step), read that issue and its PR before you build. Build on its branch or wait for its PR to merge (`agent wait pr:<n>:merged`) rather than making the same change twice.

Comment on another agent's issue rarely, unless you are supervising its work. Do it when:

- your work turns up something that helps its work in progress, or
- your work needs it to take an unusual step it wouldn't otherwise see.

Don't comment just because you work in the same file, because one of you will need to rebase, or about anything that only depends on who merges first. When you do comment, use `agent issue comment --issue <other> <file>`, which reaches the agent working it; each agent hears only about its own issue and PRs, so reply to another agent on its issue, not yours.

Comments from other agents arrive as events like the owner's, but you don't need to answer them. Act on what they tell you, and reply only in exceptional cases, such as when you disagree outright on the way forward.

## Log corrections

Whenever someone corrects you, write the lesson down as you go: a comment saying you got something wrong, a change request that undoes a choice you made, or the owner typing a correction into your session. Record the broad theme, not the exact exchange. "Wrote a lede that summarised the page" is a theme; "Removed the sentence starting 'Below you'll find'" is not. Skip plain new requests that don't say you did something wrong.

`$OPEN_SUPERVISOR agent correction --area <writing|design|data|code|workflow> <file>`, where the file is:

```
<The theme in one sentence.>

<The rule a future agent should follow, in one or two sentences.>
```

The supervisor's daily sweep folds these into the project's rules.

## Report problems

Whenever you work around something rather than doing the task the way your instructions say, log it as you go, then carry on. That covers a variable that isn't set, a command or script that doesn't exist, a path that has moved, an `agent` command that failed, a permission you had to route round, or a service call that only worked on a retry. Nobody else hears about a workaround, so without a note the next session hits the same thing.

`$OPEN_SUPERVISOR agent problem --kind <env|docs|tooling|service|other> <file>`, where the kind is a missing variable, tool or credential (`env`), a skill or rule that's wrong or stale (`docs`), an `agent …` command that failed or is missing (`tooling`), a tracker, code host or deploy service misbehaving (`service`), or anything else (`other`). The file says:

```
<What you tried to do, in one line.>

Error: <the exact error text>
Workaround: <what you did instead, or "none">
Fix: <where you think the fix belongs: a file in this project, the machine's setup, or open-supervisor itself>
```

Log each distinct problem once per session. A problem you fixed properly in your own PR needs no note. The supervisor's daily triage fixes or files what you report.

## First turn: `work-issue <key>`

### 1. Read the issue

`$OPEN_SUPERVISOR agent issue view`. Read every comment; later ones often change the ask. If the message carries an "Already waiting" section, those events arrived before your first turn: fold them in.

If the message says the issue was scheduled to start now, or that what it was held for has happened, it was held by whoever opened it: carry on as below.

Decide which kind of issue it is:

- **Change:** something is to be built or fixed. Follow steps 2–5.
- **Investigation:** the issue asks for research, options or an answer ("research X", "which provider…", "check whether…"). You don't need a PR. Do the research, post the findings on the issue (step 5 without the PR and preview links) and end with `review`. If the findings lead to an obvious change the issue asked for, you may make it as a change as well.
- **Scheduled task:** the issue carries the `scheduled` label with no time on it and names a skill or routine to run (a timer issue opened by a workflow or one of the project's jobs). Run that and follow it instead of the steps below: it says where the work happens, how it lands and when to close the issue. End with `finished` once it has closed the issue, or `help` when it stops for a decision.

### 2. Worktree

`$OPEN_SUPERVISOR agent worktree setup`. It creates or reuses the issue's worktree on the issue's branch (a new branch starts from the base branch), copies the files the project lists (such as `.env`) and runs the project's setup when its lockfile has changed. It prints the worktree path last. Work there from now on: start each command with `cd <worktree> &&`, or use `git -C <worktree>`. Every turn starts back in the main checkout, so do the same in later turns. Don't use any other worktree mechanism.

If `agent pr list` already shows an open PR, you're taking over from an earlier agent. Read it with `agent pr view`, then carry on from wherever it got to.

Post a short start comment on the issue, e.g. `printf 'Working on this on branch `%s`. A PR and a preview link will follow here.\n' <branch> | $OPEN_SUPERVISOR agent issue comment -` (for an investigation, say findings will follow).

### 3. Do the work

- Follow the project's rules, and load each rule skill they list before the work it covers.
- Keep scratch files in the worktree's `.local/` (or another ignored folder the rules name).
- Before opening the PR, run what the rules say must pass (typecheck, tests, build) and make it pass.
- Commit on the issue's branch with clear messages. The supervisor sets up commit signing (off unless the project asks for it), so don't pass signing flags.

### 4. Open the PR and end the turn

- `git push -u "$OPEN_SUPERVISOR_REMOTE" <branch>`, then `$OPEN_SUPERVISOR agent pr create --title <title> --body-file <file>`. The body says:
  - what changed, as short sections
  - anything from the issue you didn't do, and why
  - anything else the project's rules ask a PR body to carry
  The command adds the line that links the PR to the issue.
- Don't wait for the checks. End the turn with `building`, the PR number in `prs` and `touches` filled in. The supervisor resumes you when every check on the PR's head has finished, with the result and the deploy preview's URL if the project has one.

### 5. Report on the issue

This happens in the turn where the checks pass (a `Checks passed` event, with the preview URL when there is one). Open the preview once to check the changed page loads, then post one comment on the issue and end the turn with `review` and `preview` filled in:

- A mention of the issue's author and the headline of what now exists, in one line.
- `- PR: <url>` and `- Preview: <preview url>`, deep-linked to the changed page when there is one.
- A short paragraph on what was done.
- Anything worth flagging: what you couldn't do, data that doesn't exist, or a judgement call they might want to reverse, each with a bold lead.

Keep it brief; the PR has the detail.

## Later turns: `work-issue <key> events`

The message lists what happened since your last turn, each under its own heading. Re-read the issue (`agent issue view`) and the PR (`agent pr view <pr>`) before acting, so each comment is read in context, then take the events in order:

- **A comment** (on the issue, the PR, an inline review thread, or a review). A comment that is only an image is often pointing at something: fetch it (`curl -sL -o .local/<name>.png <url>`) and look. Then:
  - A change request: make it, commit, push.
  - A question: answer it. A request you think is wrong, or one you can't act on: say why, briefly, and ask.
  - If the comment corrects something you did, log it (see Log corrections).
  - From another agent: act on it if it bears on your work, but don't reply unless you disagree outright (see Other agents).
  - Reply where the comment was made, with the command its heading names. Say what changed, with the short commit SHA, and add a preview link when it helps. One comment can answer several points from the same person in the same place.
- **Checks failed**: `$OPEN_SUPERVISOR agent checks` links each failing check. Read its log, reproduce it locally, fix it, push, and end with `building`.
- **Checks passed**, with the preview: if the report (step 5) isn't posted yet, post it now. Otherwise, if you pushed changes for feedback, reply to that feedback with the preview and end with `review`.
- **No checks ran**: the repo has no CI. Treat the PR as checked once what the rules say must pass has passed locally, and report as in step 5.
- **PR merged or closed**, or **the issue closed** with no PR: clean up (see Cleaning up) and end with `finished`.
- **A note from the supervisor**: do what it says.
- **What you were waiting for**: the wait is over (see Waiting). Pick the work up, or wait again if what's left still isn't ready.
- **A message from the owner**, typed into the supervisor: treat it like a comment, but answer in your session (your summary or final reply), not on the issue, unless it asks for a comment.

### Mid-turn: `## Arrived during this turn`

A comment or a message from the owner that lands while you're working is handed to you straight away, as a message headed `## Arrived during this turn`. Take it in now: if it changes the ask, change course before you go further, and handle it like any event above (reply, log a correction). Don't end the turn early to deal with it, and don't expect it again in the next turn's events.

If you pushed, end with `building`; the supervisor comes back with the checks. Otherwise end with `help` if you asked something back, `done` if the owner approved or nothing is left, and `review` otherwise.

## Fresh session: `work-issue <key> handoff`

When your last turn ended long enough ago that resuming would cost more than starting again, the supervisor may start a new session instead. The message carries the handoff note the last turn wrote, under `## Handoff from your last session`, then the events. You don't have the earlier session's memory, so first rebuild the picture: read the issue (`agent issue view`) and every PR (`agent pr list`, `agent pr view <pr>`), run `agent worktree setup` and look at the branch's commits. Then take the events as in Later turns, with the note as your guide to what was pending and why. Where the note and the thread disagree, the thread wins.

## Cleaning up

Once every PR for the issue is merged or closed, or the issue is closed with no PR: `$OPEN_SUPERVISOR agent worktree remove`. It stops anything still running from the worktree and removes it; the branch stays. Then end the turn with `finished`. The supervisor keeps your session: a later comment or a reopen resumes it.

## Don't

- Don't wait or poll for checks, previews or replies: end the turn and let the supervisor resume you.
- Don't push to the base branch, merge the PR or close the issue. The one exception is a scheduled task whose routine says when it may.
- Don't change the supervisor's labels or the issue's workflow status; the supervisor sets them from your status.
- Don't edit other agents' worktrees.
- Don't post comments any way other than `$OPEN_SUPERVISOR agent`.
