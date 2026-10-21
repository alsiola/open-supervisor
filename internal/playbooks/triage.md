---
name: triage
description: Act on the problems issue agents have reported for this project, the things they had to work around. Fix what is plainly wrong in the project's own files with one commit to the main branch, report or file an issue for each remaining cause, and delete the notes. open-supervisor runs it once a day in each project it watches; the message names the problems folder and the report file, and says whether to open issues.
---

# Triage problems

Issue agents log each problem they work around as a file in the project's problems folder (the message names it; `$OPEN_SUPERVISOR agent problem` writes them). Each note has the issue, a `kind` (`env`, `docs`, `tooling`, `service` or `other`), what the agent tried, the exact error, its workaround and where it thinks the fix belongs. Find the causes behind them and act on each one.

The message also names a report file and says what to do with the causes you can't fix:

- **open or update an issue for each**: file them in the project's tracker (step 5). The report holds only the open-supervisor causes.
- **write them to the report**: write every one to the report, and file nothing.

1. List the files. If there are none, say "No problems to triage" and stop. Note the exact list: agents may write new files while you work, and those wait for the next triage.
2. Read every file in the list. Group the notes by cause, not by wording: 21 sessions that each tripped over `$SUPERVISOR_SCRIPTS/reply.sh` are one cause. For each cause, note how many notes and which issues it came from, and keep one or two of the exact errors.
3. Decide where each cause's fix belongs:
   - **This project's own files**: a skill, rule or script in the checkout that is plainly wrong: a stale command, a path that has moved, a variable the supervisor doesn't set. You fix these in step 4.
   - **This project, but not plainly**: a fix that needs a decision, a design or more than a small edit. Report or file it.
   - **The machine or an account**: a missing tool, credential or permission only the owner can add. Report or file it, and say what the owner has to do.
   - **open-supervisor itself**: an `agent` command that's missing or fails, an environment variable it should set, a playbook that's wrong. Report it, or file it upstream (step 5).
   - **A passing outage**: a service that failed once and worked on a retry. Drop it unless several sessions hit it.
4. Fix the causes in the project's own files, in one commit:
   - Check the main checkout is on the main branch and has no uncommitted changes. If it isn't, fix nothing: treat these causes as "not plainly" and carry on. Otherwise `git pull --ff-only`.
   - Make the smallest edit that removes the cause (replace the stale command with the right one, fix the path). Search the checkout for every other place with the same mistake and fix those too.
   - Commit only those files on the main branch, with `--no-gpg-sign` and a message listing each fix and the problem it removes. Push. If the push is rejected, `git pull --rebase` and push again. If it still fails, treat these causes as "not plainly".
5. For each remaining cause:
   - **Opening issues**: `$OPEN_SUPERVISOR agent issue list` first. If an open issue already covers the cause, comment on it with today's count, the issues and an exact error (`$OPEN_SUPERVISOR agent issue comment --issue <key> <file>`). Otherwise open one with `$OPEN_SUPERVISOR agent issue create --title <title> <file>`. For an open-supervisor cause, `agent issue create --upstream --title <title> <file>` prints a link that opens the issue on open-supervisor's tracker with your text filled in; put the cause and its link in the report rather than following it. When the message's code line shows this project is open-supervisor itself, open its issues here without `--upstream`.
   - **Writing the report**: one section per cause.
   - Write the report file fresh each triage (replace whatever is there), headed with today's date. When nothing goes in it, delete it.
   - Either way, each cause says: what the agents were trying to do, how many notes and which issues, the exact error, the workaround they used, where the fix belongs and what you think the fix is. Title an issue for the cause, not the symptom ("Skills still call `$SUPERVISOR_SCRIPTS/reply.sh`", not "reply.sh not found").
6. Delete the files you read, and only those, once the steps above are done. If a step failed partway, keep the notes for the causes it didn't handle, so the next triage picks them up.
7. Report in one line: how many problems were read, how many causes they came to, what you fixed, how many issues you opened or updated, and the report's path when you wrote one. The supervisor logs this line.

## Don't

- Don't commit anything in a triage except the fixes in step 4.
- Don't fix anything that needs a judgement call: report or file it.
- Don't change open-supervisor's own files from a project's triage, or open issues on its tracker yourself.
- Don't touch another project's files: a triage works on the checkout it starts in.
