# foreman

A terminal board of Jira tickets with their assignees. Pick a ticket and start pi or Claude Code on
it in its own git worktree, in a Rex session of its own. `tab` switches to open pull requests, where
the same dispatch starts a review, or a fix on the PR's own branch, instead. After @almonk's linear-dispatch.

```
go build -o ~/.local/bin/foreman .   # puts foreman on PATH
foreman                     # FLD open tickets
foreman OO                  # another project
foreman 'assignee = currentUser() AND statusCategory != Done'
foreman --logout            # forget the Atlassian sign-in
foreman dispatch --repo foreman 'Add a --verbose flag'   # a task, no ticket
```

In Rex, `cmd+alt+j` (the `foreman` action in `~/.config/rex/init.lua`) goes to the `foreman`
session, starting it if it isn't running: the board on the left, the [queue](#queue)
(`foreman queue`) on the right. A running session without the queue pane gets one.

## Jira access

It talks to Atlassian's remote MCP (`mcp.atlassian.com/v2/mcp`) with its own OAuth client. The
first run opens the browser to sign in; tokens live in the macOS Keychain under `foreman`,
separate from pi's and Claude Code's, split across `atlassian-mcp.1`, `.2`… since one Keychain
write takes at most 4 KB. The OAuth callback listens on `localhost:3127`.

## Keys

Board: type or paste to filter (every word must match), `↑↓` pick, `enter` dispatch, `tab`
tickets/PRs, `^o` mine (on PRs: cycles review requested, mine, everyone), `^a` cycle assignee (on
PRs: author), `^w` set the ticket's status, `^y` set its assignee (see [Changing a
ticket](#changing-a-ticket)), `^u`/`^d` scroll the description, `^b` open in browser, `^s` go to the agent's Rex
session, `^t` a [task](#tasks), `^g` the [orchestrator](#orchestrator), `^x` clean up worktrees, `^r` refresh, `esc` quit.

Dispatch form: `tab`/`↑↓` field, `←→` agent or repo, `space` worktree, poteto-mode or (on a PR) fix on/off, type to filter repos
or write extra context, `alt+enter` new line, paste works, `^e` writes the context in `$VISUAL` /
`$EDITOR`, `enter` dispatch, `esc` back.

Both views reload when the board regains focus after five minutes or more, and every five minutes.

A search loads at most 500 tickets; past that the count reads `500+` and the status line says so.
The PR view stops at 500 open PRs the same way.

## Changing a ticket

`^w` and `^y` change the selected ticket's status and assignee in Jira, in a picker that takes the
detail pane's place. Type to filter, `↑↓` pick, `enter` set, `esc` back.

- `^w` lists the transitions Jira allows the ticket now, by the status each leads to (`via …`
  names a transition called something else); the current status is marked `(now)`.
- `^y` starts with you and `unassigned`, then the users who can be assigned the ticket. Typing
  asks Jira for the matching ones, since it returns at most 50 at a time.
- After the change the board reads the ticket back and its row shows the new status or assignee
  where it is; the next reload sorts it. A failure is shown on the status line and the picker stays
  open.
- These are the only Jira writes foreman makes, and only from these keys: no command does them, so
  the orchestrator and agents can't. They go through the MCP's `transitionJiraIssue` and
  `editJiraIssue`; the sign-in's `write:jira:agent-interface` scope covers them.

## Dispatching

- Repos are the git checkouts directly under `~/work/code` and `~/projects`; the last one used per
  Jira project is preselected.
- The agent starts on the one last dispatched in that repo. A repo never dispatched in starts on
  Claude when it ships Claude Code skills (`.claude/skills/*/SKILL.md`), which pi doesn't load, and
  on pi otherwise. It follows the repo as you move through the list, until you pick one yourself.
- Claude's prompt names the repo's skills it can invoke (those without
  `disable-model-invocation`) and tells it to use them for the steps they cover, minus any part
  that pushes or posts. A review gets the same.
- The worktree is `<repo>-<key>`. The most recently committed `origin/` branch containing the
  ticket key is checked out; a local branch of the same name is used as is, unpushed commits and
  all. Otherwise `rf/<key>-<slug>` is created from `origin/develop`, else origin's default branch.
  A failed fetch is reported and the last fetched refs are used.
- Without a worktree the agent runs in the checkout itself, which must be on a ticket branch, not
  `develop`, `main`, `master` or a detached HEAD.
- Each agent starts in a new background Rex session named after the ticket, such as
  `fld-1671-my-van-always`; `^s` on the row, or `cmd+s`, switches to it. The board stays where it
  is. It is tracked by its ID, so an agent renaming its session doesn't lose it.
- Dispatching to a ticket or PR whose agent session is still open takes `enter` twice: the second
  agent would work in the first one's worktree, and the board would track only the new session.
- `◆` marks a dispatched row, coloured by what its agent reports to Rex: yellow working, red
  waiting for you or failed, green idle or done. `◇` means its session is closed.
- The agent gets the ticket as markdown, your extra context, and a workflow: understand, implement,
  verify, commit. It is told not to push, open a PR or touch the ticket.
- With poteto-mode on, the prompt also tells the agent to read pstack's poteto-mode skill and
  follow it. The workflow still wins: no push, no PR. The skill's path comes from
  `~/.claude/plugins/installed_plugins.json`, so the dispatch fails if pstack isn't installed
  there. It is off each time the form opens.
- The agent's prompt is written to `prompt.md` in its evidence directory and read by a shell in
  the new session, since Rex caps a request at 64 KB.

## Tasks

A task is work with no ticket or PR behind it: `^t` on the board opens the dispatch form with a
**Task** field in place of the extra context, on the repo of the ticket under the cursor.

- Its key is `task:<repo>-<n>`, numbered per repo directory name and never reused. The worktree is
  `<repo>-task-<n>` on a new `rf/<title>` branch from `origin/develop`, else origin's default
  branch; a name already taken locally or on origin gets `-2`, `-3`…
- The title is the task's first line. It names the Rex session (`task-foreman-3-add-a-verbose`)
  and the queue row.
- The agent gets the task and the same workflow as a ticket, plus: a question or an investigation
  is answered in the report, with the code left as it is.
- Picking the agent remembers it for the repo; a task doesn't change the repo a Jira project
  starts on.

### `foreman dispatch`

The board's dispatch from the command line, for scripts and agents. Progress goes to stderr, the
result line to stdout.

```
foreman dispatch --repo foreman [--agent pi|claude|yolo] [--no-worktree] [--poteto] [--title T] 'what to do'
foreman dispatch --ticket FLD-1700 [--repo reports-app] [...] ['extra context']
foreman dispatch --pr reports-app#1185 [--agent claude] ['what to focus on']
foreman dispatch --pr reports-app#1185 --fix [--agent claude] [--title T] ['what to fix']
```

`-` as the text reads it from stdin. A ticket without `--repo` goes to its project's last repo. A
ticket or PR whose agent session is still open needs `--again`, like the board's second `enter`.

### Reading and steering agents

What the queue shows and does, as commands. KEY is a dispatch's key (`FLD-1700`,
`reports-app#1185`, `fix:reports-app#1185`, `task:foreman-3`) or a watcher's name, in any case.

Options go before or after KEY, except `reply`'s, which go before it.

```
foreman agents [--json] [--prs]     every agent and watcher by queue group; --prs adds my PRs that need me
foreman report KEY                  the report and the paths of its evidence; fails when there is none
foreman screen [--lines N] KEY      the end of the agent's pane, 40 lines (0: all), and its seen token
foreman reply [--now] [--seen TOKEN] KEY TEXT|-
                                    type TEXT into the agent's pane and press enter, as the queue's f
foreman wait [--timeout D] KEY      block until the agent stops working (default 30m), print its state
foreman cleanup [--dry-run] [KEY…]  list finished dispatches and remove what ^x ticks, or only KEY…
foreman ask [--timeout D] TEXT|-    from an agent: put a question to the orchestrator, print the answer
foreman answer KEY TEXT|-           answer the question KEY waits on
foreman escalate KEY NOTE|-         hand KEY's question to me in the queue, with a suggestion
```

- `agents --json` gives each one's `key`, `kind` (ticket, review, fix, task, watcher, pr), `group`,
  `state` as the agent reports it to Rex (working, blocked, error, done, idle, open) or `closed`,
  `title`, `agent`, `session`, `dir`, `report` and `reportAt`, evidence counts, and `seen`, the
  seen token of an agent whose session is open.
- `reply` refuses a closed session, and a working agent unless `--now`: pi and Claude Code each
  handle text typed mid-turn their own way. To an agent waiting in `foreman ask`, it is the answer.
- A seen token, printed by `screen` under the pane and by `agents --json` as `seen`, stands for
  what the pane showed: the state the agent reports to Rex and a hash of the pane's last 12
  non-blank lines, where a permission prompt, a menu or the input box sits; output higher up, a
  clock or an old spinner, doesn't change it. `reply --seen TOKEN` reads the pane again just
  before typing and refuses when the token no longer matches, so an answer to a prompt that was
  answered meanwhile isn't typed as a message; `screen` again shows what is there now. A working
  agent's spinner and timer sit in those lines, so its token rarely holds for `--now`. With
  `--seen`, a reply to an agent waiting in `foreman ask` is refused too: the pane isn't what it answers.
- `wait` gives an agent that isn't working ten seconds to start, so `reply` then `wait` waits for the
  answer rather than returning at once.
- `cleanup` never forces: git refuses a worktree with changes, and branches stay.

## Orchestrator

A Claude Code session to talk to: tell it what you want done and it dispatches agents, follows
them and summarises their reports. `^g` on the board goes to its Rex session, `foreman-orchestrator`,
starting it when it isn't running; the queue lists it while it runs, with **needs you** when it
waits on a permission prompt.

- It runs `foreman orchestrator`: Claude Code in `~/.local/state/foreman/orchestrator`, outside
  every repo, continuing its last conversation (`--new` starts a fresh one). Its instructions are
  the `CLAUDE.md` there, rewritten from foreman's source on each start: hand every piece of work to
  an agent, brief it fully, bring permission prompts and scope questions to me, never push or post.
- It runs in the manual permission mode with auto mode turned off (`--permission-mode manual
  --settings '{"disableAutoMode":"disable"}'`), whatever your settings say: without
  `disableAutoMode`, the cursor on a Bash prompt rests on "Yes, and switch to auto mode", and
  one `enter` would hand the session to the auto-mode classifier. The read-only commands
  (`agents`, `report`, `screen`, `wait`), `answer`, `escalate`, `reply` and `cleanup` run without
  asking: `reply` refuses a `blocked` agent, so it can't answer a permission prompt, and the
  instructions keep `cleanup` to `--dry-run` unless you ask. The rest, `dispatch` among them,
  follow manual mode: they prompt, unless Claude Code counts the command as read-only.
- It has the MCP servers in `~/.config/mcp/mcp.json`, to read Jira tickets.
- It is tracked by its session ID, recorded when it starts, so renaming its session doesn't lose it.

### Questions from agents

A ticket's or task's agent is told to run `foreman ask "<question>"` before it stops to ask me.
The command waits inside the agent's turn, up to 8 minutes, and prints the answer, so nothing is
typed into a working agent. Claude agents run it without a permission prompt.

- The question is typed into the orchestrator's pane as `[foreman ask] KEY asks: …`. It answers
  with `foreman answer KEY` what I've settled, quoting me or citing the code, report or ticket, and
  hands the rest to me with `foreman escalate KEY "<suggestion>"`; both run without asking.
- With no orchestrator running, the question comes straight to me.
- A question handed to me puts the agent under **needs you** (`asks you`), with the question and the
  orchestrator's suggestion in the detail pane; `f` answers it, as does `foreman answer` or
  `foreman reply`. While the orchestrator has it, the agent is **working** (`asks the orchestrator`).
- Unanswered after 8 minutes, `ask` tells the agent to put the question in its report and stop to
  ask me; a later reply is typed into its pane as usual. Dispatching the agent again withdraws it.
- foreman's own limits on the orchestrator: three answers or messages per dispatch, then it must
  escalate; and it can't type into a `blocked` agent, which is how a permission prompt shows.
  Its instructions have it pass `--seen` when a reply answers something it read on the pane.
- Every question, answer, escalation and orchestrator message goes to `orchestrator.jsonl` beside
  the agent's report, shown under **Questions** in the queue's detail pane.

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

### Fixing a PR

`fix it` on the PR's dispatch form, or `foreman dispatch --pr REPO#N --fix`, starts an agent that
fixes the PR on its own branch instead of reviewing it: its review comments, or what the extra
context names.

- Its key is `fix:<repo>#<n>`, so a review of the same PR keeps its own session, report and queue
  row. The session is `fix-<repo>-<n>-<title>`; `--title` names it and the queue row in place of
  the PR's title. The board's PR row shows the review's `◆`, else the fix's, and `^s` follows it.
- The worktree is the one that already has the PR's head branch checked out, a ticket's or one
  made by hand, whatever it holds; else `<repo>-fix-<n>` on the local branch of that name, as is,
  or on a new one tracking `origin/<head>`. A branch without an upstream gets `origin/<head>`.
- A PR from a fork is refused: its branch isn't on origin.
- The agent gets the PR description and file list, its reviews with a comment, its unresolved
  review threads (inline comments with their replies; resolved ones are counted, not shown), its
  conversation, the Jira ticket named in the title or branch, and your context. It is told to
  commit on the branch and to leave pushing, replying to and resolving comments, and the PR and
  ticket, to you; its report says what it did about each comment.
- In the queue, `enter` on one of your PRs goes to its fix agent when there is one, before the
  ticket's.

## Queue

`foreman queue` lists every dispatched agent, whatever its state, and your PRs that need you,
grouped most urgent first:

1. **needs you**: agents that stopped to ask something or failed (`●` red), with their report or,
   without one, the end of their screen; then your PRs with failed CI, changes requested or a
   conflict (`✗`).
2. **review**: agents that finished with a report you haven't marked reviewed (`✓`), newest first,
   with their evidence (`▣ 3` screenshots or captures, `▶ video`); then your approved PRs whose CI
   passed or has no checks (`✓`), ready to merge.
3. **working**: agents still working (`●` yellow), with the end of their screen, refreshed every poll.
4. **idle**: agents whose session is open with nothing new for you (`○`), reviewed or without a report.
5. **closed**: agents whose session is closed (`◇`), until `K` drops them from the queue or `^x` on
   the board cleans up their worktree. A dropped agent shows again if its session reopens. `enter`
   resumes one: a new session in its worktree runs the dispatched agent with `--continue`, which
   picks the newest conversation in that directory, so without a worktree it may be another one.
   Dispatches from before the agent was recorded take the agent with the newest conversation there.

An agent is found by the session ID recorded at dispatch, else by a session whose label still starts
with its key (`fld-1704`, `review-reports-app-1185`), whose ID is then recorded. It reads the board's
state file and the Rex sessions every few seconds, and your PRs every five minutes. Its pane title
carries the count of items in the first two groups, `queue (3)`.

Keys: `↑↓` pick, `enter` go to the agent's session, resuming a closed one (a PR goes to the agent of
the ticket its title or branch names; with no agent, to the board's dispatch form on that ticket;
with no ticket, the browser), `f` type a reply that is sent to the agent's pane (the one whose
program reports a status), `v` open the screenshots and videos, `d` the worktree's diff in git's
pager, `o` the ticket or PR in the browser, `x` mark reviewed (the agent moves to idle or closed
until it writes a new report; a PR goes until its status changes), `K` twice close the agent's Rex
session, or on a closed one drop it from the queue (its worktree and report stay), `^u`/`^d` scroll,
`^r` reload, `q` quit (`cmd+alt+j` brings the pane back).

### Reports and evidence

Each dispatch tells the agent to write `report.md` and its evidence to
`~/.local/state/foreman/evidence/<key>/`, outside every repo: screenshots per acceptance criterion
and a short video of a flow (agent-browser) for web UI, `rex capture` dumps for terminal UI, the
commands and their output otherwise. They are for you only; the agent is told never to commit,
attach or link them. A new dispatch empties the directory, and cleaning up a worktree removes it.

## Watchers

A watcher is a Claude Code session that keeps running one of a repo's skills. Mine is
`fld-pr-watch`: `/loop 10m /fld-watch` in `~/work/code/reports-app-watch`, which reviews the PRs
that request my review and posts the result under my account.

- The board starts each watcher that has no open Rex session, in the background, and records
  each watcher's session ID. A watcher's session is the recorded one, else one named after it, else
  one running Claude Code in its checkout: Claude renames its session when Rex's startup hook says
  so, and a watcher started twice would review every PR twice.
- It runs `claude --permission-mode manual --settings '{"disableAutoMode":"disable"}' <prompt>`:
  only the commands its checkout's `.claude/settings.local.json` allows run unasked, and no prompt
  or `shift+tab` can switch it to auto mode. The skill's own file lists the allowed commands.
- The queue always lists it: with **needs you** when it waits on a permission prompt or failed (a
  pass stalls until you answer), else **working**, **idle** between passes, or **closed** when it
  isn't running, where `enter` starts it. `K` twice stops it.
- Its checkout is a worktree nobody works in, detached at `origin/develop`. The skills run from
  it, so move it forward when they change: `git -C <dir> fetch origin develop && git -C <dir>
  checkout --detach origin/develop`.

## Cleaning up

`^x` lists the dispatches whose ticket is Done, whose PR is merged or closed, or whose task's session
is closed. Those with
uncommitted changes or a working agent start unticked. `enter` runs `git worktree remove` on the
ticked ones (never `--force`, so changes stop it); branches and their commits stay, and the
reports, evidence and the agent's Rex session go.

- A task has nothing outside to say it's done: it is listed once its agent's session is closed.
- A dispatch that ran without a worktree is listed as `checkout stays`: only its record, reports and
  session go.
- A dispatch whose worktree was removed by hand is dropped, with its reports, as soon as `^x` runs.
- A ticket or PR that couldn't be read (Jira or `gh` failed) is reported in the status line, not
  taken as still open.

## Config

`~/.config/foreman/config.json` overrides the defaults, all optional; an unknown key is an error:

```json
{
  "site": "https://bloqit.atlassian.net",
  "cloudId": "1eb9eb54-74a9-4d94-8eb1-a1a7696be552",
  "defaultProject": "FLD",
  "branchPrefix": "rf/",
  "watchers": [{ "name": "fld-pr-watch", "dir": "~/work/code/reports-app-watch", "prompt": "/loop 10m /fld-watch" }]
}
```

`"watchers": []` turns them off.

## State

- State (repo per project, last agent per repo, dispatched tickets, PRs and tasks with their sessions and titles, the last task number per repo, queue items
  marked reviewed) is in
  `~/.local/state/foreman/state.json`, written atomically. The board and the queue both write it,
  so each write holds `state.json.lock`, and the board re-reads it every few seconds. An unreadable
  file is moved to `state.json.corrupt` rather than overwritten.

## Development

`go test ./...` covers the pure logic and the worktree branch choice against throwaway git repos.
