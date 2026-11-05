# foreman

A terminal board of Jira tickets with their assignees. Pick a ticket and start pi or Claude Code on
it in its own git worktree, in a new Rex tab. After @almonk's linear-dispatch.

```
bun install && bun link     # puts foreman on PATH (~/.bun/bin)
foreman                     # FLD open tickets
foreman OO                  # another project
foreman 'assignee = currentUser() AND statusCategory != Done'
foreman --logout            # forget the Atlassian sign-in
```

## Jira access

It talks to Atlassian's remote MCP (`mcp.atlassian.com/v2/mcp`) with its own OAuth client. The
first run opens the browser to sign in; tokens live in the macOS Keychain under `foreman`,
separate from pi's and Claude Code's. The OAuth callback listens on `localhost:3127`.

## Keys

Board: type to filter (every word must match), `↑↓` pick, `enter` dispatch, `^o` mine, `^a` cycle
assignee, `^u`/`^d` scroll the description, `^b` open in browser, `^r` refresh, `esc` quit.

Dispatch form: `tab`/`↑↓` field, `←→` agent or repo, `space` worktree on/off, type to filter repos
or write extra context, `enter` dispatch, `esc` back.

## Dispatching

- Repos are the git checkouts directly under `~/work/code` and `~/projects`; the last one used per
  Jira project is preselected.
- The worktree is `<repo>-<key>`. A remote branch containing the ticket key is checked out;
  otherwise `rf/<key>-<slug>` is created from `origin/develop`, `main` or `master`.
- The agent gets the ticket as markdown, your extra context, and a workflow: understand, implement,
  verify, commit. It is told not to push, open a PR or touch the ticket.
- State (repo per project, last agent, dispatched tickets `◆`) is in
  `~/.local/state/foreman/state.json`.
