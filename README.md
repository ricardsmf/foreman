# foreman

A terminal board of Jira tickets with their assignees. Pick a ticket and start pi or Claude Code on
it in its own git worktree, in a Rex session of its own. `tab` switches to open pull requests, where
the same dispatch starts a review instead. After @almonk's linear-dispatch.

```
go build -o ~/.local/bin/foreman .   # puts foreman on PATH
foreman                     # FLD open tickets
foreman OO                  # another project
foreman 'assignee = currentUser() AND statusCategory != Done'
foreman --logout            # forget the Atlassian sign-in
```

In Rex, `cmd+alt+j` (the `foreman` action in `~/.config/rex/init.lua`) goes to the `foreman`
session, starting it if it isn't running: the board on the left, the [queue](#queue)
(`foreman queue`) on the right. A running session without the queue pane gets one.

## Jira access

It talks to Atlassian's remote MCP (`mcp.atlassian.com/v2/mcp`) with its own OAuth client. The
first run opens the browser to sign in; tokens live in the macOS Keychain under `foreman`,
separate from pi's and Claude Code's. The OAuth callback listens on `localhost:3127`.

## Keys

Board: type or paste to filter (every word must match), `↑↓` pick, `enter` dispatch, `tab`
tickets/PRs, `^o` mine (on PRs: cycles review requested, mine, everyone), `^a` cycle assignee (on
PRs: author), `^u`/`^d` scroll the description, `^b` open in browser, `^s` go to the agent's Rex
session, `^x` clean up worktrees, `^r` refresh, `esc` quit.

Dispatch form: `tab`/`↑↓` field, `←→` agent or repo, `space` worktree on/off, type to filter repos
or write extra context, `alt+enter` new line, paste works, `^e` writes the context in `$VISUAL` /
`$EDITOR`, `enter` dispatch, `esc` back.

Both views reload when the board regains focus after five minutes or more, and every five minutes.

## Dispatching

- Repos are the git checkouts directly under `~/work/code` and `~/projects`; the last one used per
  Jira project is preselected.
- The worktree is `<repo>-<key>`. The most recently committed `origin/` branch containing the
  ticket key is checked out; a local branch of the same name is used as is, unpushed commits and
  all. Otherwise `rf/<key>-<slug>` is created from `origin/develop`, else origin's default branch.
  A failed fetch is reported and the last fetched refs are used.
- Without a worktree the agent runs in the checkout itself, which must be on a ticket branch, not
  `develop`, `main`, `master` or a detached HEAD.
- Each agent starts in a new background Rex session named after the ticket, such as
  `fld-1671-my-van-always`; `^s` on the row, or `cmd+s`, switches to it. The board stays where it
  is.
- `◆` marks a dispatched row, coloured by what its agent reports to Rex: yellow working, red
  waiting for you or failed, green idle or done. `◇` means its session is closed.
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

## Queue

`foreman queue` lists what needs you, most urgent first:

1. Agents that stopped to ask something (`●` red), with their report or, without one, the end of
   their screen.
2. Your PRs with failed CI, changes requested or a conflict (`✗`).
3. Agents that finished (`✓`), newest first, with their report and evidence (`▣ 3` screenshots or
   captures, `▶ video`). A finished agent shows once it has written a report.

It reads the board's state file and the agents' Rex states every few seconds, and your PRs every
five minutes. Its pane title carries the count, `queue (3)`.

Keys: `↑↓` pick, `enter` go to the agent's session (a PR opens in the browser), `f` type a reply
that is sent to the agent's pane, `v` open the screenshots and videos, `d` the worktree's diff in
git's pager, `o` the ticket or PR in the browser, `x` dismiss (it comes back when its report or
the PR's status changes), `^u`/`^d` scroll, `^r` reload, `q` quit (`cmd+alt+j` brings the pane back).

### Reports and evidence

Each dispatch tells the agent to write `report.md` and its evidence to
`~/.local/state/foreman/evidence/<key>/`, outside every repo: screenshots per acceptance criterion
and a short video of a flow (agent-browser) for web UI, `rex capture` dumps for terminal UI, the
commands and their output otherwise. They are for you only; the agent is told never to commit,
attach or link them. A new dispatch empties the directory, and cleaning up a worktree removes it.

## Cleaning up

`^x` lists the dispatched worktrees whose ticket is Done or whose PR is merged or closed. Those
with uncommitted changes or a working agent start unticked. `enter` runs `git worktree remove` on
the ticked ones (never `--force`, so changes stop it); branches and their commits stay, and the
reports and evidence go.

## State

- State (repo per project, last agent, dispatched tickets with their sessions and titles, dismissed
  queue items) is in
  `~/.local/state/foreman/state.json`, written atomically. An unreadable file is moved to
  `state.json.corrupt` rather than overwritten.

## Development

`go test ./...` covers the pure logic and the worktree branch choice against throwaway git repos.
