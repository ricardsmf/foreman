# foreman

A terminal board of Jira tickets with their assignees. Pick a ticket and start pi or Claude Code on
it in its own git worktree, in a Rex session of its own. `tab` switches to open pull requests, where
the same dispatch starts a review instead. After @almonk's linear-dispatch.

```
bun install && bun link     # puts foreman on PATH (~/.bun/bin)
foreman                     # FLD open tickets
foreman OO                  # another project
foreman 'assignee = currentUser() AND statusCategory != Done'
foreman --logout            # forget the Atlassian sign-in
```

In Rex, `cmd+alt+j` (the `foreman` action in `~/.config/rex/init.lua`) goes to the `foreman`
session, starting it with the board if it isn't running.

## Jira access

It talks to Atlassian's remote MCP (`mcp.atlassian.com/v2/mcp`) with its own OAuth client. The
first run opens the browser to sign in; tokens live in the macOS Keychain under `foreman`,
separate from pi's and Claude Code's. The OAuth callback listens on `localhost:3127`.

## Keys

Board: type to filter (every word must match), `↑↓` pick, `enter` dispatch, `tab` tickets/PRs, `^o`
mine (on PRs: cycles review requested, mine, everyone), `^a` cycle assignee (on PRs: author), `^u`/`^d` scroll the
description, `^b` open in browser, `^r` refresh, `esc` quit.

Dispatch form: `tab`/`↑↓` field, `←→` agent or repo, `space` worktree on/off, type to filter repos
or write extra context, `enter` dispatch, `esc` back.

## Dispatching

- Repos are the git checkouts directly under `~/work/code` and `~/projects`; the last one used per
  Jira project is preselected.
- The worktree is `<repo>-<key>`. A remote branch containing the ticket key is checked out;
  otherwise `rf/<key>-<slug>` is created from `origin/develop`, `main` or `master`.
- Each agent starts in a new background Rex session named after the ticket, such as
  `fld-1671-my-van-always`; switch to it with `cmd+s`. The board stays where it is.
- The agent gets the ticket as markdown, your extra context, and a workflow: understand, implement,
  verify, commit. It is told not to push, open a PR or touch the ticket.
## Reviews

- The PR view lists open PRs in the GitHub repos checked out under `~/work/code`, from `gh` (its own
  sign-in), leaving out bots'. They load in the background when the board starts. PRs that request your review come first, marked `requested`, then
  yours.
- Your PRs show their review state (`in review`, `approved`, `changes`, `conflict`, `draft`) and CI
  (`✓` passed, `✗` failed, `●` running). Red means something needs you; green means approved with CI
  done. Their detail pane lists the failing and running checks.
- The review worktree is `<repo>-pr-<n>`, detached at the PR head. Dispatching again moves it to
  the current head, unless it has uncommitted changes.
- The agent gets the PR description, its file list, the Jira ticket named in the title or branch,
  and your extra context. It is told to report findings without editing, committing or posting
  anything to GitHub or Jira.

## State

- State (repo per project, last agent, dispatched tickets `◆`) is in
  `~/.local/state/foreman/state.json`.
