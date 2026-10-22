---
name: sweep
description: Fold the corrections issue agents have logged for this project into its rule files, land them on the main branch (a push, or a PR), and delete the notes. open-supervisor runs it once a day in each project it watches; the message names the corrections folder, the rule files, the guide if any, and how to land the commit.
---

# Sweep corrections

Issue agents log each correction they get as a file in the project's corrections folder (the message names it; `$OPEN_SUPERVISOR agent correction` writes them). Fold them into the project's rule files and land them on the main branch.

The message says where the rule files are (for example `CLAUDE.md` or `AGENTS.md`, and the rule skills it lists). If it names a guide, read it first: it says which file each kind of rule goes in and how rules there must be written, and it outranks the defaults here. Without one, each rule goes in the skill or section whose work it covers, or else under the closest heading of the main rules file.

The `area` line in a correction file is a hint, not the answer: a `writing` correction about which fields an issue asked for is about reading the request, not writing.

1. List the files. If there are none, say "No corrections to sweep" and stop. Note the exact list: agents may write new files while you work, and those wait for the next sweep.
2. Check the main checkout is on the main branch and no rule file has uncommitted changes. If either isn't true, say why in one line and stop; the files stay for tomorrow. Then `git pull --ff-only`.
3. Read every file in the list and every rule file in full. Group the corrections by theme. Several files about the same mistake become one rule. Drop anything a rule file already says, unless it keeps happening; then sharpen the existing rule rather than add a second one. Drop one-offs that only made sense for that issue.
4. Edit the rule files. Put each rule in its home, under the heading it belongs to, in the same voice: a short imperative, with a banned example when that helps. Don't record where a rule came from, and don't add a changelog.
   - Edit a skill's frontmatter `description` only when the rules it covers change, so it still says when to load it.
   - Don't create a new skill in a sweep. If a theme fits none of the homes, put it in the closest one and say so in the report.
5. Commit only the rule files, with a message listing the rules added or changed and the file each went into. Land it as the message's `Land` line says, on its `Remote`:
   - **push to the main branch**: commit on the main branch and push. If the push is rejected, `git pull --rebase` and push again.
   - **open a PR**: `git switch -c open-supervisor/sweep-<YYYY-MM-DD>` first, commit there, `git push -u <remote> <branch>`, then `$OPEN_SUPERVISOR agent pr create --head <branch> --title <title> --body-file <file>`, with the same list in the body. Then `git switch <main branch>`, so the checkout is back where it was.
6. Delete the files you read, and only those, once the push succeeds. If nothing survived deduplication, delete them anyway and don't commit.
7. Report in one line: how many corrections were read, which rules were added or changed and where, and the PR's link if you opened one.

## Don't

- Don't commit anything in a sweep except the rule files.
- Don't touch another project's files: a sweep works on the checkout it starts in.
