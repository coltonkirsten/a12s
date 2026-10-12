# Agent guidelines

Building an app ON a12s rather than changing a12s? Read [llms.txt](llms.txt) instead; this file is for changes to
a12s itself.

Most code here is written and reviewed by agents. These rules apply to every change.

## Comments

1. No comments that restate what the code does. Name things well instead.
2. A comment is allowed only for a non-obvious **why**: an invariant, a security constraint, a race, or an upstream
   quirk that the code alone can't show. Keep it to one or two lines.
3. Comments never reference docs, spec sections (`§4.5`), review or issue numbers, dates, people, or the history of
   how the code got here. State the reason itself.
4. When you change code, update or delete its comments. A wrong comment is a bug and should be flagged in review.
5. Go doc comments on exported identifiers are allowed only when they add something the name and signature don't.
6. Before deleting a why-comment, make sure the code doesn't look "simplifiable" without it. If removing it would
   invite someone to break an invariant, keep it.

## Documentation

1. Docs describe the **current state** of the code: no plans, roadmaps, decision logs, review notes, changelogs of
   how a design evolved, dated run logs or "not built yet" sections. Git history is the record of the past.
2. There is one README, at the repo root. Everything else lives in `docs/` (user and reference docs) or
   `CONTRIBUTING.md` (building, testing, layout). Don't add README files to subdirectories.
3. When behavior changes, update the affected page in `docs/` in the same change. If a doc and the code disagree,
   the code is right and the doc is the bug.
4. Write in present tense, for someone using or contributing to a12s today.
5. Keep anchors that code links to working: error messages link to `docs/services.md#manifest`.

## Where things are

- `README.md`: overview and quickstart. `llms.txt`: the whole user-facing surface on one page, for agents; update it
  when a command, a Fleet field or an SDK method changes. `CONTRIBUTING.md`: build, test, layout, end-to-end scripts.
  `CHANGELOG.md`: user-visible changes per release, breaking ones with what to do.
- `docs/`: concepts, recipes, SDK, HTTP API, Fleet file, CLI, inbox and power policy, Services, credentials,
  security, architecture, images.
- Before finishing: `make check-code` while iterating; `make check` green locally before merge (it is the merge
  gate: nothing runs on GitHub automatically). Paste its tail in the PR.
