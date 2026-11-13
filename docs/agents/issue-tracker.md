# Issue tracker: GitHub

Issues and specs live in `eslym/deploy-manager`. Use the `gh` CLI.

## Conventions

- Before the first issue write in a session, verify the target with
  `gh repo view --json nameWithOwner`. Target `eslym/deploy-manager`;
  resolve any mismatch with the user.
- Create: `gh issue create --title "..." --body-file <file>`.
- Read: `gh issue view <number> --json number,title,body,labels,comments`.
- List: `gh issue list --state open --json number,title,body,labels`;
  filter by label or state as needed.
- Comment: `gh issue comment <number> --body-file <file>`.
- Label: `gh issue edit <number> --add-label "..." --remove-label "..."`.
- Close: post the explanation as a comment, then `gh issue close <number>`.
  Comment separately even when the issue is already closed.
- Use native GitHub sub-issues and blocking dependencies when supported
  by the installed CLI/API. Otherwise put `Part of #<parent>` and
  `Blocked by: #<number>, ...` at the top of the child body, and link
  children in the parent's task list.
- An issue is startable only when every blocker is closed.

## Pull requests as a triage surface

**PRs as a request surface: no.**

## Skill operations

- "Publish to the issue tracker": create a GitHub issue.
- "Fetch the relevant ticket": read its issue body, labels, and comments.

## Wayfinding

- Map: one issue labelled `wayfinder:map`, containing Notes,
  Decisions-so-far, and Fog.
- Child: link to the map as a sub-issue, or use the parent/task-list
  fallback above. Label `wayfinder:<type>` where type is research,
  prototype, grilling, or task.
- Frontier: the first open child in map order with no open blockers
  and no assignee.
- Claim: `gh issue edit <number> --add-assignee @me`.
- Resolve: comment with the answer, close the child, and append a
  summary and link to the map's Decisions-so-far.
