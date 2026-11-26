# foreman

A terminal board of Jira tickets with their assignees, beside a queue of the agents at work on
them. Pick a ticket and start pi or Claude Code on
it in its own git worktree, in a Rex session of its own. `tab` switches to open pull requests, where
the same dispatch starts a review, or a fix on the PR's own branch, instead. After @almonk's linear-dispatch.

```
go build -o ~/.local/bin/foreman .   # puts foreman on PATH
foreman                     # FLD open tickets, beside the queue
foreman queue               # the same, opened on the queue alone
foreman OO                  # another project
foreman 'assignee = currentUser() AND statusCategory != Done'
foreman --logout            # forget the Atlassian sign-in
foreman open                # go to the foreman Rex session, starting it when it isn't running
foreman watch               # start the watchers and wake them, for launchd
foreman dispatch --repo foreman 'Add a --verbose flag'   # a task, no ticket
foreman repos               # the repos a dispatch takes, each with the agent it starts on
foreman today               # what is in progress, up next and done since yesterday
foreman stats --days 7      # what the agents used: cost, tokens and time worked
```

foreman is one program in one pane: the board on the left and the [queue](#queue) on the right.
`^l` moves between them, and the side in focus gets the keys; the other is drawn faint, without
its cursor. The board takes 45% of the width. `^z` shows the side in focus alone, full width, and
`^z` again both; `^l` while one is alone shows the other alone. A pane under 110 columns shows
only the side in focus. `foreman queue` opens on the queue alone.

Neither side keeps a pane of whole documents beside its list. The row under the cursor opens in
the reading pane (`→` on the board, `space` or `→` in the queue), which takes the place of the
side out of focus, or of the whole pane where one side shows alone. It opens on the row's brief:
a line of what it is, its title, then for a question the question and its numbered choices, for a
report its headline, a line of facts (oracles, evidence, ports), its **Open points** (the section
named `Open`, `Open points`, `Left open`, `Left for you` and the like) as bullets, and **Also
in the report**, its other sections each with its length in lines; for a ticket, its facts and as
much of its description as fits. The same key again opens the whole text, the detail the queue
and the board always had, scrolled by `^u`/`^d`; `←` goes back to the brief and `esc` closes
the pane. `↑↓` move to the next row with the pane open.

The board draws on the terminal its output goes to. Where that is no terminal, in a script or an
agent's shell, foreman starts no board: a word that isn't a command would have been read as a Jira
project or a search, so a misspelt command fails there, named, with the list of commands.

`foreman open` goes to the `foreman` Rex session, starting it if it isn't running. In Rex,
`cmd+alt+j` (the `foreman` action in `~/.config/rex/init.lua`) runs `foreman open --print`, which
prints the session's ID instead of showing it, and selects that session in the app the key was
pressed in.

## Jira access

It talks to Atlassian's remote MCP (`mcp.atlassian.com/v2/mcp`) with its own OAuth client. The
first run opens the browser to sign in; tokens live in the macOS Keychain under `foreman`,
separate from pi's and Claude Code's, split across `atlassian-mcp.1`, `.2`… since one Keychain
write takes at most 4 KB. The OAuth callback listens on `localhost:3127`.

## Keys

Board: type or paste to filter (every word must match), `↑↓` pick, `→` read the row (where the
filter has nothing right of its cursor; `←` back), `enter` dispatch, `tab`
tickets/PRs, `^o` mine (on PRs: cycles review requested, mine, everyone), `^a` cycle assignee (on
PRs: author), `^w` set the ticket's status, `^y` set its assignee (see [Changing a
ticket](#changing-a-ticket)), `^u`/`^d` scroll the description's whole text, opening it, `^b` open in a browser [beside the board](#pages-beside-the-board), `^s` go to the agent's Rex
session, `^t` a [task](#tasks), `^g` the [orchestrator](#orchestrator), `^x` clean up worktrees, `^r` refresh, `esc` close the reading pane, clear the filter, then quit.
The footer, under both sides, names at most five keys, those of the moment and of the row under
the cursor on the side in focus, then `? keys`; `?`, while the filter is empty, lists them all,
and `↑↓` scroll that list in a short pane.

The board groups its rows under quiet lowercase headings, a ticket's status (`blocked` in red)
or, for PRs, `review requested`, `yours` and `others`, so a ticket's row has no status of its
own; a PR's row keeps its state. A row is one line, its title first, without the bracketed tags it
starts with (`[Reports App][Tests]`) or a conventional commit's type and tag (`fix: [FLD-1885]`),
then the assignee and the key, quiet, at the right edge. The tags show as the brief's `area`. The
row under the cursor shows the first three lines of its description under itself, while no reading
pane shows the row. The filter shows over the list only while it is set.

The board opens on the view (tickets or PRs) and the `^o` filter it was last on. A list filtered to
one person, by `^o` or `^a`, leaves the assignee column to the summary.

Every text field (the filter, a picker's, the dispatch form's context, the queue's reply) has a
cursor: `←→` move it, `alt+←→` or `alt+b`/`alt+f` by word, `home`/`end` to the line's ends,
`alt+backspace` deletes the word before it, `delete` the character under it, `ctrl+k` to the
line's end. Where the board doesn't use them itself, `ctrl+a`/`ctrl+e` are `home`/`end`, `ctrl+w`
deletes a word and `ctrl+u` to the line's start. What is pasted lands at the cursor.

Dispatch form: `tab`/`↑↓` field, `←→` agent or repo, `space` worktree, poteto-mode or (on a PR) fix on/off, type to filter repos
or write extra context, `alt+enter` new line, paste works, `^e` writes the context in `$VISUAL` /
`$EDITOR`, `enter` dispatch, `esc` back.

Both views reload when the board regains focus after five minutes or more, and every five minutes.

The row under the cursor has a background and a bar at its left edge. The key footer wraps
between keys onto as many lines as the pane needs, and the reading pane's first line names the lines
of the whole text shown (`1–24 of 88`) when there are more; so does the rule over the today and
stats views' detail.

Colours follow the terminal's theme: the hues are its palette's, and the greys and the selected
row's background are shades between its background and its text, which the board asks it for.
Colour stands for a state alone: red needs you or failed, green to review or passed, yellow
working; everything else is in the greys, headings and code included. A
terminal that doesn't answer gets Catppuccin Macchiato's greys.

A status line clears by itself: after ten seconds, a warning after twenty, an error after a minute.
On the board, one about work under way (`…`) stays until its result replaces it.

A search loads at most 500 tickets; past that the count reads `500+` and the status line says so.
The PR view stops at 500 open PRs the same way.

## Changing a ticket

`^w` and `^y` change the selected ticket's status and assignee in Jira, in a picker that takes the
reading pane's place. Type to filter, `↑↓` pick, `enter` set, `esc` back.

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
- Claude runs one of three ways: with its default permission prompts, in auto mode (`Claude
  (auto)`, `--permission-mode auto`: a classifier reviews what it runs and blocks what looks
  dangerous, by the `autoMode` rules in `~/.claude/settings.json`), or with every check skipped
  (`Claude (yolo)`). Auto mode is the one for an agent left alone that needs no prompts.
- Claude's prompt names the repo's skills it can invoke (those without
  `disable-model-invocation`) and tells it to use them for the steps they cover, minus any part
  that pushes or posts. A review gets the same.
- The worktree is `<repo>-<key>`. The most recently committed `origin/` branch containing the
  ticket key is checked out; a local branch of the same name is used as is, unpushed commits and
  all. With none on origin, the most recently committed local branch containing the key that was
  never pushed (it has no upstream) is: the one an agent committed on, which outlives its
  worktree's cleanup. Otherwise `rf/<key>-<slug>` is created from `origin/develop`, else origin's
  default branch, with `-2`, `-3`… when that name is taken: a local branch whose origin branch is
  gone was merged or dropped there, and stays as it is.
  A failed fetch is reported and the last fetched refs are used.
- A ticket with several such branches, a stack or one for its docs, goes back to the one its
  worktree was last on, while that one is still among them: `^x` records it as it removes the
  worktree, and git still lists a worktree removed by hand with its branch. With neither, it
  takes the most recently committed.
- The repo's worktrees whose directory is gone, removed by hand rather than by `^x`, are forgotten
  first (`git worktree prune`): git still counts their branch as checked out there, so the
  dispatch would reuse a directory that isn't there. A review's and a fix's worktree get the same.
- Without a worktree the agent runs in the checkout itself, which must be on a ticket branch, not
  `develop`, `main`, `master` or a detached HEAD.
- Each agent starts in a new background Rex session named after the ticket, such as
  `fld-1671-my-van-always`; `^s` on the row, or `cmd+s`, switches to it. The board stays where it
  is. It is tracked by its ID, so an agent renaming its session doesn't lose it.
- The new session goes right after the last open session of an agent or watcher in the same repo,
  so Rex's sidebar keeps a repo's sessions together. With none open, it goes where Rex puts it.
- Dispatching to a ticket or PR whose agent session is still open takes `enter` twice: the second
  agent would work in the first one's worktree, and the board would track only the new session.
- `◆` marks a dispatched row, coloured by what its agent reports to Rex: yellow working, red
  waiting for you or failed, grey idle or done. `◇` means its session is closed.
- The agent gets the ticket as markdown, your extra context, and a workflow: understand, name an
  oracle for each acceptance criterion and see it red, implement, verify until every oracle is
  green, commit. It is told not to push, open a PR or touch the ticket.
- With poteto-mode on, the prompt also tells the agent to read pstack's poteto-mode skill and
  follow it. The workflow still wins: no push, no PR. The skill's path comes from
  `~/.claude/plugins/installed_plugins.json`, so the dispatch fails if pstack isn't installed
  there. It is off each time the form opens.
- The agent's prompt is written to `prompt.md` in its evidence directory and read by a shell in
  the new session, so its size doesn't depend on the request Rex takes: 64 KB before server
  0.1.0-preview.22, 1 MiB since.

## Tasks

A task is work with no ticket or PR behind it: `^t` on the board opens the dispatch form with a
**Task** field in place of the extra context, on the repo of the ticket under the cursor.

- Its key is `task:<repo>-<n>`, numbered per repo directory name and never reused. The worktree is
  `<repo>-task-<n>` on a new `rf/<title>` branch from `origin/develop`, else origin's default
  branch; a name already taken locally or on origin gets `-2`, `-3`…
- The title is the task's first line. It names the Rex session (`task-foreman-3-add-a-verbose`)
  and the queue row.
- The agent gets the task and the same workflow as a ticket, with a target it states itself in
  place of the acceptance criteria, plus: a question or an investigation is answered in the
  report, with the code left as it is.
- Picking the agent remembers it for the repo; a task doesn't change the repo a Jira project
  starts on.

### `foreman dispatch`

The board's dispatch from the command line, for scripts and agents. Progress goes to stderr, the
result line to stdout.

```
foreman dispatch --repo foreman [--agent pi|claude|auto|yolo] [--no-worktree] [--poteto] [--title T] 'what to do'
foreman dispatch --ticket FLD-1700 [--repo reports-app] [...] ['extra context']
foreman dispatch --pr reports-app#1185 [--agent claude] ['what to focus on']
foreman dispatch --pr reports-app#1185 --fix [--agent claude] [--title T] ['what to fix']
foreman dispatch --review FLD-1700 [--agent claude] ['what to focus on']
foreman repos [--json] [--in DIR]
```

`-` as the text reads it from stdin. A ticket without `--repo` goes to its project's last repo. A
ticket or PR whose agent session is still open needs `--again`, like the board's second `enter`.

- `--notify` shows the result line, or why nothing was dispatched, as a macOS notification as
  well: for a dispatch started where nobody reads its output, as [the palette's](#from-rexs-command-palette) is.
- `foreman repos` lists the repos `--repo` takes, each with the agent a dispatch there starts on.
  The one the current directory is in, or `--in DIR`, reads `here`, from inside a worktree too.
  `--json` gives `repos` (each one's `name`, `path`, `agent` and `here`) and `agents` (each
  one's `label` and `note`).

### From Rex's command palette

`rex/foreman.lua` adds **New Foreman Task…** to Rex: a task handed to an agent from whatever
session you are in, without a trip to the board. After @almonk's rex-projects.

```lua
-- ~/.config/rex/init.lua
package.path = os.getenv("HOME") .. "/projects/foreman/rex/?.lua;" .. package.path
require("foreman")
rex.bind("cmd+alt+t", "foreman_task")
```

- It asks three things in the palette: the repo, with the one the focused pane's program is in
  first (`here`; a worktree counts as its repo); the agent, with the one a dispatch in that repo
  starts on first; and the task, in one line. Closing any of them dispatches nothing.
- It then runs `foreman dispatch --notify` and doesn't wait for it: a dispatch fetches origin
  first, and Rex gives an action 30 seconds. The result line, or why nothing was dispatched, comes
  as a notification, and the agent shows in the queue.
- foreman runs through the login shell, whose `PATH` has it. The repo, the agent and the task
  travel in the environment, so the shell reads nothing you typed.
- A pane on a remote host puts no repo first: its directory is that host's.
- A longer brief, a ticket or a PR goes through the board's form, which has the editor (`^e`).

### Reading and steering agents

What the queue shows and does, as commands. KEY is a dispatch's key (`FLD-1700`,
`reports-app#1185`, `fix:reports-app#1185`, `task:foreman-3`) or a watcher's name, in any case.

Options go before or after KEY, except `reply`'s, which go before it.

```
foreman agents [--json] [--prs]     every agent and watcher by queue group; --prs adds my open PRs
foreman report KEY                  the report and the paths of its evidence; fails when there is none
foreman screen [--lines N] KEY      the end of the agent's pane, 40 lines (0: all), and its seen token
foreman reply [--now] [--seen TOKEN] KEY TEXT|-
                                    type TEXT into the agent's pane and press enter, as the queue's f
foreman wait [--timeout D] KEY      block until the agent stops working or asks (default 30m), say why
foreman cleanup [--dry-run] [KEY…]  list finished dispatches and remove what ^x ticks, or only KEY…
foreman ask [--timeout D] [--option CHOICE]… [--recommend CHOICE] [--proceed] TEXT|-
                                    from an agent: put a question to me in the queue, print the answer
foreman answer KEY TEXT|-           answer the question KEY asked
foreman oracle [--stays-green] NAME red|green [NOTE]
                                    from an agent: record the result of running an oracle
foreman today [--json]              the dispatches in progress, my tickets no agent is on yet, what got done
foreman stats [--days N] [--by ticket|repo] [--json]
                                    what the agents used today, or over the last N days
```

- `agents --json` gives each one's `key`, `kind` (ticket, review, fix, task, watcher, pr), `group`,
  `state` as the agent reports it to Rex (working, blocked, error, done, idle, open) or `closed`,
  `title`, `agent`, `session`, `pane` (the agent's pane in it), `waitsFor` (what a `blocked` agent
  says it waits for: `permission`, `question` or `auth`, when it says; for a PR, what its row in
  the queue says it waits on), `ports` (what the
  programs in its session [listen on](#ports), each with its `port`, `process` and `url`), `dir`,
  `report` and `reportAt`, evidence counts, `seen`, the seen token of an agent whose session
  is open, `question`, the one it asked and I haven't answered (its `text`, `options` and
  `recommended`; `left` says nothing waits on it any more), `decisions`, the questions it
  [went on without](#going-on-without-an-answer), and `oracles`, the [checks it recorded](#oracles),
  each with its `name`, last `result` and `note`, `wasRed` and `staysGreen`. The plain listing ends a row with those URLs.
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
  answer rather than returning at once. An agent waiting in `foreman ask` returns it at once.
- `wait` prints `KEY STATE`, then why the agent stopped, with an exit code for each: 0 done (idle,
  done or closed), then the report's first line; 2 blocked (blocked or error), then what its pane
  asks, the lines a seen token covers reaching up to the top of a taller prompt, and the seen token
  as `screen` prints it; 3 asked, with the first line `KEY asked`, then its question; 4 still
  working when the timeout ran out. 1 is a failure to wait, as for an unknown KEY.
- `cleanup` never forces: git refuses a worktree with changes, and branches stay.
- `today` prints the queue's [today](#today) view, and with `--json` gives `since`, the start of
  yesterday, and `inProgress`, `upNext` and `done`: rows with a `key`, `kind`, `title`, `repo` and
  `url`, and a dispatch in progress its `state`, a ticket its `status`, one done `what` ended it
  and `at` when. It is there for a script that sums the day up. It reads Rex's sessions, the
  event log and Jira, for the default project's open tickets; when Jira doesn't answer, `upNext`
  is empty and stderr says why.
- `stats` prints the queue's [stats](#stats) view, and with `--json` gives `days`, `since`, `by`,
  `ticketsDone`, `prsMerged`, and `total` and each of `rows` (with its `name`, `title` and `repo`)
  as `in`, `out`, `cacheRead` and `cacheWrite` tokens, `cost` in dollars, `unpriced` tokens and
  `agentSeconds`.

## Orchestrator

A Claude Code session to talk to: tell it what you want done and it writes the briefs and
dispatches the agents, waits on them, says what their reports add up to, and merges, pushes or
opens the PR when you ask. `^g` on the board goes to its Rex session, `foreman-orchestrator`,
starting it when it isn't running; the queue lists it while it runs, with **needs you** when it
waits on a permission prompt.

It doesn't stand between you and the agents: their prompts and questions come to the queue, where
you answer them, and it only passes on what you tell it to.

- It runs `foreman orchestrator`: Claude Code in `~/.local/state/foreman/orchestrator`, outside
  every repo, continuing its last conversation (`--new` starts a fresh one). Its instructions are
  the `CLAUDE.md` there, rewritten from foreman's source on each start: send every code change to
  an agent with a full brief, say what the queue can't show, and merge, push or open a PR only
  when asked.
- It runs in the manual permission mode with auto mode turned off (`--permission-mode manual
  --settings '{"disableAutoMode":"disable"}'`), whatever your settings say: without
  `disableAutoMode`, the cursor on a Bash prompt rests on "Yes, and switch to auto mode", and
  one `enter` would hand the session to the auto-mode classifier. The read-only commands
  (`agents`, `repos`, `report`, `screen`, `wait`), `answer`, `reply` and `cleanup` run without
  asking: `reply` refuses a `blocked` agent, so it can't answer a permission prompt, and the
  instructions keep `cleanup` to `--dry-run` unless you ask. The rest, `dispatch` among them,
  follow manual mode: they prompt, unless Claude Code counts the command as read-only.
- It has the MCP servers in `~/.config/mcp/mcp.json`, to read Jira tickets.
- It is tracked by its session ID, recorded when it starts, so renaming its session doesn't lose it.

## Questions from agents

A ticket's or task's agent is told to run `foreman ask "<question>"` before it stops to ask me.
The command waits inside the agent's turn, up to 8 minutes, and prints the answer, so nothing is
typed into a working agent. Claude agents run it without a permission prompt.

- The question puts the agent under **needs you** (`asks you`) at once, with the question and its
  choices on its card; `f` answers it, as does `foreman answer` or `foreman reply`.
- It raises a macOS notification as it is asked, `FLD-1700 asks: …`: the agent shows as working
  to Rex, so nothing else says outside the queue that it waits, and it waits 8 minutes.
- What you type into the agent's own session while it waits answers it too, for a Claude Code
  agent. Claude Code queues what is typed mid-turn and notes it in the conversation as it is
  typed, so `ask` returns it at the next look, the question leaves the queue, and the queued
  message reaches the agent after the command, as the same answer. pi keeps no such note: a pi
  agent is answered from the queue.
- Unanswered after 8 minutes, `ask` tells the agent to put the question in its report and stop to
  ask me. The question stays under **needs you**, as one it
  [left](#a-question-that-outlives-its-asker): an answer there, by a digit as well, is typed into
  its pane. Dispatching the agent again withdraws it.
- `foreman wait` returns when the agent asks, so an orchestrator waiting on it sees the question
  too; it passes on the answer you give it with `foreman answer`.
- Every question and answer, and every message the orchestrator sends the agent, goes to the
  [event log](#state); the current dispatch's are shown under **Questions** in the item's whole
  text.

### Choices

`foreman ask --option "<choice>" --option "<choice>" --recommend "<choice>" "<question>"` names
the choices the agent sees, at most nine, and the one it would take. The options go before or
after the question. The agent's prompt tells it to name them.

- The card and the brief number the choices and mark the recommended one. In the queue a digit answers
  with that choice and `a` with the recommended one; `f` still takes any answer.
- `--recommend` without options is the answer the agent would give, and `a` answers with it.
- A recommendation that is none of the options, or a tenth option, fails the command with the
  reason, for the agent to ask again.

### Going on without an answer

With `--proceed`, which needs `--recommend`, the command records the question and returns at
once, and the agent goes on with what it recommended. The agent's prompt keeps it to a guess that
is cheap to undo.

- The agent's row moves to **review**, `went on, 2 to confirm`, whatever the agent is doing, and
  its card and whole text start with each such question and its choices, oldest first. The notification reads
  `FLD-1700 went on with “Postgres”: …`.
- `a` confirms the oldest and `x` all of them: they leave the queue, and nothing is sent.
- A digit or `f` gives the oldest another answer. It is typed into the agent's pane as a message
  that names the question and what the agent went on with, working or not, since nothing waits on
  it. The answer the agent went on with confirms it instead.
- `foreman answer KEY TEXT` does the same for the oldest.

### A question that outlives its asker

A question stays mine to answer when nothing waits on it any more: its `foreman ask` gave up
after 8 minutes, its session closed, or its `foreman ask` is gone, as when I interrupt the agent's
turn. Its row stays under **needs you**, `left a question`.

- Each question records the `foreman ask` process that waits on it, and the queue looks for that
  process. One from before that was recorded counts as waiting while its session is open.
- With the session open, an answer (`f`, a digit, `a`, or `foreman answer`) is typed into the
  agent's pane as a message that names the question.
- With the session closed, it resumes the agent, as `enter` on a closed row does, in a
  background session whose first turn is that message. Only I do that: `foreman answer` from the
  orchestrator is refused.
- `x` sets it aside, for one I answered in the agent's session or that needs no answer: it leaves
  the queue. A new question from the agent replaces it too.

## Reviews

- The PR view lists open PRs in the GitHub repos checked out under `~/work/code`, from `gh` (its own
  sign-in), leaving out bots'. They load in the background when the board starts. PRs that request your review come first, marked `requested`, then
  yours.
- Your PRs show their review state (`in review`, `approved`, `changes`, `threads`, `conflict`,
  `draft`) and CI (`✓` passed, `✗` failed, `●` running). `threads` is an open review thread someone
  else had the last word in. Red means something needs you; green means approved with CI done.
  Their whole text lists the failing and running checks.
- A repo may hold a PR back with a commit status that fails until it has its reviews:
  `reviewCheck` in the [config](#config) names it, and mine is `review-tier`. It stands for the
  reviews, not for CI: while it fails or is pending the PR is `in review`, whatever GitHub's own
  rule says, and once it passes the PR is `approved`. `ignoreChecks` are left out of CI
  altogether, as the job that posts that status is: its earlier runs stay on the commit, failed
  or cancelled.
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

### A PR that conflicts

`m` on a PR of yours that conflicts with its base, in the queue, has an agent merge the base in.

- It goes to the agent on the PR's branch: the one fixing the PR, else its ticket's, while its
  worktree is on that branch. The ask is typed into its pane, and a closed agent is resumed on
  it. An agent that waits on a prompt is told nothing, since the prompt would take the text as
  its answer: answer it first.
- With no agent on the branch, the board's dispatch form opens on the PR with `fix it` on and
  the ask as its context, and `enter` dispatches the fix.
- The ask is to merge `origin/<base>`, not to rebase, which would need a push with force; to
  take the base's side where the conflict is with commits the base already has under another
  commit (a PR this one was stacked on, squash-merged); then to build, run the tests and linters
  that cover the files that conflicted, commit the merge, and say in its report what conflicted
  and how each was resolved.
- The agent doesn't push, so the PR conflicts until you do. Its row comes back under **review**
  once it has written that report, the merge commit not yet in the PR.

### Reviewing work before its PR

An agent commits on its branch and stops there, so its work has no PR to review yet. Two ways to
review it first:

- `r` on its row in the queue shows the diff `d` shows, from the commit its dispatch started on,
  in the queue's own pane. `↑↓` pick a line (`^u`/`^d` move ten, `n`/`p` to the next or the
  previous hunk, `g`/`G` to the ends); `c` or `enter` writes a comment on it, or changes the one
  it has, and `x` drops it. A commented line is marked `●`, with the comment under it. `s` sends
  them as one reply: they are written to `review.md` beside the agent's report, each with its file
  and line and the line itself, and the agent is told to read that file and address each one, in
  its pane, or for a closed agent as the first turn of its resumed session. `esc` goes back to
  the queue, and asks once more while comments are unsent.
- `foreman dispatch --review KEY` puts another agent on it: a read-only review of the commits
  KEY's agent made since its dispatch, so of at least one. KEY is a ticket's, a task's or a fix's
  dispatch. The reviewer is a task of the same repo, `task:<repo>-<n>`, titled `Review KEY: …`,
  in a worktree `<repo>-task-<n>` detached at the head of that work, so apart from the agent's
  own conversation. It gets the diff to read, the prompt the agent started with and its report,
  the Jira ticket when KEY is one, and your extra text. It reports findings and changes nothing.
  Its row stays until its session is closed and `^x` cleans it up, as a task's does.

## Queue

The queue lists every dispatched agent, whatever its state, and every open PR of yours, by what
it waits on, grouped most urgent first. Within a group a repo's items sit together, and the
repo with the newest report comes first. The repo is the main checkout's directory name, so a
worktree counts as its repo; the brief names it.

Size follows urgency, under **Your move** and one count of what waits on you; with nothing, it
says `Nothing waits on you.` Each item of the first two groups is a card: its key and what it
waits on, its title, then the question with its numbered choices and the recommended one marked,
what the pane of a blocked agent asks, or the report's headline over a line of its facts (oracles,
evidence, ports, how many points it leaves open). The agents at work are two lines each, under
**Agents at work**: the title, then the key, what the agent says it is doing and its oracles. The
rest is a line each, quieter: under **Waiting on others** the PRs, under **At rest** the idle and
closed agents. The row under the cursor shows a few lines of what it holds under itself: the end
of a working agent's screen, a PR's first sentence and facts, a report's headline.

1. **needs you**: agents that stopped to ask something, [left a question](#a-question-that-outlives-its-asker) or failed (`●` red), with their report or,
   without one, the end of their screen (less the input box Claude Code ends it with, so it ends on
   what the agent last wrote); then your PRs with failed CI (the row names
   the check, `✗ Tests (2/4) +1`), changes requested, a review thread to answer or a conflict (`✗`).
2. **review**: agents that finished with a report you haven't marked reviewed (`✓`), newest first,
   with their evidence (`▣ 3` screenshots or captures, `▶ video`), and agents that
   [went on with their own answer](#going-on-without-an-answer) to a question, until you confirm it. An agent whose worktree is on
   the head commit of an open PR of yours isn't among them: its work is in the PR to review, so
   its row reads `in #1395` and sits under idle or closed, until a commit there isn't pushed yet; then your approved PRs whose CI
   passed or has no checks (`✓`), ready to merge. One stacked on another open PR of yours isn't
   yours to merge yet: it waits, `approved, after #1366`. A PR that waits on others comes here when someone else
   comments on it or reviews it, `new: ana approved`, bots aside, until `x` or `o` on its row,
   or until you answer on the PR or push to it.
3. **working**: agents still working (`●` yellow), with the end of their screen, refreshed every
   poll. The row says what the agent reports doing, when it does (Claude Code names its current
   step over OSC 7501), rather than `working`.
4. **waiting**: your PRs that wait on someone else (`○`), so every PR you have open is somewhere
   in the queue. The row says on what: its reviews (what the review check says is missing,
   `needs 2 approvals, has 0`, less the commit it names; without one, who was asked, `with ana +2`, or `no
   reviewer asked`), `CI running`, the PR under it, or `draft`. A ticket's PRs sit together and a
   stack runs from its bottom up, by the branch each merges into: a row right under the PR it is
   stacked on starts with `↳` and says only what its title adds to that one's. The age is how
   long it has been open for review: since it was made, or last taken out of draft. A PR's
   whole text starts with the latest thing someone else did on it, then what it waits on in full, who
   was asked to review it and the PR it is stacked on.
5. **idle**: agents whose session is open with nothing new for you (`○`), reviewed or without a report.
6. **closed**: agents whose session is closed (`◇`), folded into one row that counts them (`▸ 8
   closed`); `c`, or `enter` on that row, lists them, and `c` folds them again. A stopped watcher
   is always listed. They stay until `K` drops them from the queue or `^x` on the board cleans up
   their worktree, and the board drops one whose ticket is Done or whose PR is merged or closed,
   each time its tickets load (a task stays: nothing outside says it's done). A dropped agent shows
   again if its session reopens. `enter` resumes one: a new session in its worktree runs the dispatched agent with `--continue`, which
   picks the newest conversation in that directory, so without a worktree it may be another one.
   Dispatches from before the agent was recorded take the agent with the newest conversation there.

An agent is found by the session ID recorded at dispatch, else by a session whose label still starts
with its key (`fld-1704`, `review-reports-app-1185`), whose ID is then recorded. Its state is its
own pane's: the one Rex made at dispatch, recorded with the session, so a shell or a fork of its
conversation split beside it doesn't speak for it, and replies and key presses go to that pane. A
session with no recorded pane (an older dispatch, a watcher started by hand) gets its oldest pane
whose program reports a status.

It reads the state file every few seconds, since `foreman dispatch`, `ask` and the other commands
write it too, and takes your PRs from the board's load, every five minutes. The Rex sessions
foreman follows by their events, for the board and the queue alike: one Lua listener
(`rex do --all`) prints each status a program reports, with the record itself, and each change to
the ports a pane's programs listen on, with the list, and foreman applies them to the picture of
the sessions it keeps, with no call to Rex, so a row moves as its agent stops.
A session or a pane opening, or a program exiting, takes a listing; so does every 15 seconds, in
case an event was missed. While the listener isn't running the sessions are listed every few
seconds. The pane's title carries the count of items in the first two groups, `foreman (3)`.

It also reports that count to Rex in the Program Status Protocol (OSC 7501), so the `foreman`
session is marked in Rex's session picker whatever session you are in: `blocked` while anything is
under **needs you**, else `done` while anything is under **review**, else `idle`. Agents at work
aren't reported, since their own sessions show that. Each item in those two groups is a record of
its own under the queue's, with its key and title and what it waits for (the agent's own `kind` and message for one
that is blocked, `kind=permission` when it names none; `kind=question` for one that asks through
`foreman ask`), cleared when it leaves them. Quitting the
queue clears them all.

Keys: `↑↓` pick, `enter` go to the agent's session, resuming a closed one (a PR goes to the agent of
the ticket its title or branch names; with no agent, to the board's dispatch form on that ticket, which takes the focus;
with no ticket, the browser), `f` type a reply that is sent to the agent's pane, or the answer to its question, `1`–`9` or `a` answer a question with one of its [choices](#choices) or the recommended one, `y` or `n` answer the prompt an agent waits on (see [Answering a
prompt](#answering-a-prompt)), `v` show its videos, screenshots and captures in a browser beside the board (see [Reports and
evidence](#reports-and-evidence)), `d` the worktree's diff in git's
pager, `r` that diff in the queue, to [comment on its lines](#reviewing-work-before-its-pr), `m` have the agent on a PR that [conflicts](#a-pr-that-conflicts) merge its base in, `o` the ticket or PR in a browser [beside the board](#pages-beside-the-board), `O` in the system browser (either way a PR's news is then read), `p` a [port](#ports) its session listens on, `x` mark reviewed (the agent moves to idle or closed
until it writes a new report, what it went on with is confirmed, and a question it left is set aside; a PR moves to **waiting** until what it waits on changes or someone writes on it), `K` twice close the agent's Rex
session, or on a closed one drop it from the queue (its worktree and report stay), `c` list or fold
the closed agents, `space` or `→` read the row (again for its whole text, which `space` then
scrolls; `←` back, `esc` closes it), `^u`/`^d` scroll the whole text, `^r` reload, `?` every key, `q` close the queue, leaving the board alone at full width (`^z` there brings the
queue back beside it; `^c` quits foreman).

`tab` turns the queue to its two other views, [today](#today) and the [stats](#stats), and back to
the list. Each is a list under quiet headings over the detail of the row picked, which `space`,
`^u` and `^d` scroll.

### Today

Three lists, which `foreman today` prints too:

- **progress**: the dispatches with something still going on: an agent whose session is open, or
  one that left a report to read or a question. A row says what its row in the queue says. A
  dispatch whose own PR merged, or whose ticket is Done, has left the list whatever its session
  shows.
- **up next**: your tickets no agent was dispatched on, in the board's order, among the tickets
  the board loaded, so those of its project or search. A ticket whose status has `blocked`,
  `review`, `testing` or `staging` in it isn't listed: its work can't start, or is past starting.
- **done**: what got done since the start of yesterday, newest first: a dispatched ticket that
  reached Done, and a PR of yours merged, the one a ticket's or a task's agent opened or the one a
  fix was dispatched on. One dispatch is one row, `reports-app#1415 merged, ticket done`. A
  review's PR merging is someone else's work, and isn't listed.

`enter` goes to an agent's session as it does in the queue, opens the board's dispatch form on a
ticket up next, and opens a done row's ticket or PR in a browser beside the board; `o` opens any
row's, `O` in the system browser. The detail of a dispatch is its timeline: every event the
[log](#state) has of it.

What got done is read from the event log, so it outlives the cleanup of its worktree. foreman
learns of it while it runs. A ticket is found Done each time the board loads its tickets: a
dispatched ticket the load leaves out, as a search for open tickets does once it is done, is
asked about. A PR is found merged when the queue's load of open PRs no longer has it: GitHub is
then asked how it ended, and when. A ticket is logged as done at the time foreman finds it so, a
PR at the time it merged.

### Stats

What the agents foreman started used over today, the last 7 days or the last 30 (`←→`), a row
per dispatch or per repo (`g`), the costliest first. `foreman stats [--days N] [--by repo]` prints
the same. Over the rows are the totals, and how many tickets got done and PRs of yours merged in
those days, by the event log. A row's detail splits its tokens by kind.

- **Tokens** are read from the conversations Claude Code keeps under `~/.claude/projects` and pi
  under `~/.pi/agent/sessions` for the directory the agent worked in: each message's input,
  output, cache read and cache write tokens, its subagents' too, a message counted once however
  many conversations hold it. Nothing is recorded as it happens, so once a conversation's file is
  gone what it used is no longer counted.
- **Cost** is at Anthropic's API prices, whatever a subscription charges. pi writes what each
  message cost. Claude Code's are priced by model, from the table in `stats.go`: a cache write
  costs 1.25 times the input price when it is kept five minutes and twice when kept an hour, and
  fast mode twice the lot. Tokens of a model the table lacks are counted and left out of the
  cost, which the row's detail says.
- **Agent time** is the time the agents worked: in each conversation, from each prompt to the
  last entry before the next one, so not the wait for you. A gap of more than 30 minutes between
  two entries, the machine asleep, isn't counted, nor a subagent's time, which its caller's
  covers.
- What a worktree's conversations hold goes to the dispatch that had started there last, by the
  log's `dispatched` events: a key dispatched again is still one row, and what the directory held
  before its first dispatch isn't counted. A dispatch from before the log is counted from the
  state file, with everything its worktree holds. A dispatch without a worktree is left out: its
  directory is the repo's main checkout, shared with whatever else you run there. Each watcher
  and the orchestrator have a row of their own.

### Ports

A row names the port its agent's session listens on, `:5173`, or `:3000 +1` with more, while a
program there has one open: the dev server the agent started and left running, or one you started
in a shell split beside it. So what the agent built can be opened as it runs in its worktree.

- The whole text starts with where each port is opened and the program that holds it
  (`http://localhost:5173 (vite)`); for an agent that waits on a prompt or asks a question, it
  comes after that.
- `p` opens it in a browser [beside the board](#pages-beside-the-board). With several, each `p` opens the next, lowest first, and the
  status line says which comes after.
- They are every pane's in the session, one entry a port number: a server on both `127.0.0.1`
  and `::1` is one. One bound to a single other address is opened there instead of `localhost`.
- The port Claude Code itself listens on isn't one of them: it answers a browser `not found`.
- Every port is opened as `http`, whatever speaks on it; the program's name is there to tell a
  dev server from a debugger's port.
- Rex lists the ports of programs still attached to the terminal, so a server that detached as a
  daemon isn't among them, and it looks for new ones about once a second while terminals are
  busy. It takes Rex 0.1.0-preview.20; an older server lists none.

### Answering a prompt

An agent that reports `blocked` waits on a permission prompt or a menu. Its brief and whole text
start with what its pane asks, read again on every poll while the cursor is on it, and so does its
card once the cursor has been there (before, the card shows what the agent says it waits for); and `y` presses `enter` there, `n` presses `esc`:
in Claude Code's permission prompt, the highlighted "Yes" and a cancel.

- The key is pressed only while the pane still shows what the queue showed: the queue keeps the
  [seen token](#reading-and-steering-agents) of the pane it read and reads the pane again before
  pressing. A prompt answered meanwhile, or replaced by the next one, gets nothing, and the pane
  is read again.
- They are keys in the pane, not answers: on a menu, `enter` picks whatever is highlighted. Read
  the prompt on the card or in the brief first; `enter` on the row goes to the session for anything else.
- An agent that says it waits on a question or a sign-in (`kind=question`, `kind=auth` in its
  status) isn't a yes or no: `y` and `n` do nothing there, and the whole text says so. One that names
  no kind is taken as a permission prompt.
- Only these keys do it. No command presses a key in a pane, so the orchestrator can't.

### Reports and evidence

Each dispatch tells the agent to write `report.md` and its evidence to
`~/.local/state/foreman/evidence/<key>/`, outside every repo: screenshots per acceptance criterion
and a short video of a flow (agent-browser) for web UI, `rex capture` dumps for terminal UI, the
commands and their output otherwise. They are for you only; the agent is told never to commit,
attach or link them. A new dispatch empties the directory, and cleaning up a worktree removes it.

`v` in the queue shows them on one page: the videos (with controls, paused), then the screenshots
(each links to the full file), then the `.txt` captures inlined, each under its file name, in one
column that fits the pane. Each press writes the page again, to `.evidence.html` in the same
directory; the hidden name keeps it out of the evidence counts and away from what the agent
writes. With nothing to show, `v` says so and writes nothing.

The page opens [beside the board](#pages-beside-the-board). Outside Rex, or without
`terminal-browser` on `PATH`, `v` opens the screenshots and videos in their default apps (Preview,
the video player) instead; captures alone have nothing to open.

### Oracles

An agent on a task or a ticket names an oracle for its change, a check that shows it done, and
is told to see it red before it changes code. It records each run with `foreman oracle`:

```
foreman oracle "login test" red "expected 200, got 500"
foreman oracle "login test" green
foreman oracle --stays-green "lint" green
```

- The agent's row counts them, `3/5 green`: those whose last run is green, of all it recorded.
  The count is green once every oracle is and each was seen red, and yellow until then.
- `, 1 never red` counts the green ones never recorded red: a check that could not fail before
  the change may have been written to fit it. `--stays-green` is for the one nothing can make red
  first, as the check a refactor must keep, which isn't counted so. The command tells the agent
  when the green it records was never red.
- The whole text lists them under the report, each with its last result and the note it came with.
- The runs are kept in `oracles.jsonl` beside the report, a line each, so a new dispatch starts
  with none. A name is one oracle whatever its case.
- Claude Code runs `foreman oracle` without a permission prompt, as it does `foreman ask`.

### Pages beside the board

What foreman opens, it opens in `terminal-browser`, in the Rex tab the board is in: the ticket or
PR of the board's `^b` and the queue's `o`, the PR of the queue's `enter` when it names no ticket,
the port of `p` and the evidence page of `v`.

- The page opens in a new tab of the browser already open in the board's Rex tab, or else in one
  split right of the board's pane.
- Outside Rex, or without `terminal-browser` on `PATH`, a ticket, a PR or a port opens in the
  system browser.
- `O` in the queue opens the ticket or PR in the system browser wherever the board runs.
- `terminal-browser` keeps its own sign-ins, apart from the system browser's: GitHub and Jira
  each ask once there.
- The sign-in of [Jira access](#jira-access) opens in the system browser.
- foreman makes the split itself and starts the browser in it only once Rex has sized the pane.
  Rex creates a pane at a placeholder 80x24 and sizes it seconds later; a browser started at
  once takes the jump in cell height for a font change and zooms the page by the ratio, a zoom
  Chromium then keeps for the page. The wait has no limit, since a pane still at the placeholder
  is one no Rex app shows: the browser starts when one does. The pane says it is waiting only
  once the wait passes two seconds. A pane whose real size is exactly
  80x24 never starts it. A page that already kept a wrong zoom keeps it until `ctrl+0` in the
  browser.

### An agent's status line

A dispatched Claude Code shows a line of foreman's under its prompt, so what waits elsewhere is
seen from inside an agent's session:

```
FLD-1704 · #1395 conflicts with develop · elsewhere 2 need you, 1 to review
```

- It names the dispatch; the open PR of yours on the branch its worktree is on, with what that
  PR's row in the queue says, red when it needs you and green when it is ready to merge or
  someone wrote on it; then how many other items sit under **needs you** and **review**. Its own
  row and its PR's aren't counted: the session shows those.
- The dispatch passes `--settings '{"statusLine":…}'`, which outranks a status line of your own
  settings in that session, and has Claude Code run `foreman statusline` after each turn and
  every 10 seconds. pi has no such line.
- The command calls neither Rex nor GitHub. It reads the dispatch from `state.json`, by the pane
  it runs in (`REX_BLOCK`), else by the directory Claude Code started in, and the rest from
  `glance.json` beside it, which the board rewrites when its queue changes. With no board
  running, the line is the dispatch alone.
- An agent dispatched before this has no line until the queue resumes it.

## Watchers

A watcher is a Claude Code session that runs one of a repo's skills when there is something for it
to do. Mine is `fld-pr-watch`: `/fld-watch --once` in `~/work/code/reports-app-watch`, which
reviews my open PRs, posting what blocks them, and drafts a pending review on the PRs that request
mine.

- [`foreman watch`](#foreman-watch) starts each watcher that has no open Rex session and that you
  didn't stop, in the background, and records
  each watcher's session ID. A watcher's session is the recorded one, else one named after it, else
  one running Claude Code in its checkout, started by foreman or by you from a shell there: Claude renames its session when Rex's startup hook says
  so, and a watcher started twice would review every PR twice.
- It runs `claude --permission-mode manual --settings '{"disableAutoMode":"disable"}' <prompt>`:
  only the commands its checkout's `.claude/settings.local.json` allows run unasked, and no prompt
  or `shift+tab` can switch it to auto mode. The skill's own file lists the allowed commands.
  Starting it runs one pass.
- Every pass's prompt is the watcher's `prompt` followed by rules that keep its tool calls inside
  that allowlist: one plain command per Bash call (a loop, a variable or `;` asks even when each
  part is allowed), files read with Read, Grep and Glob, nothing the skill doesn't name, and the
  Rex session left under its name (Rex's startup hook, run again by each `/clear`, asks for a
  rename).
- The queue always lists it: with **needs you** when it waits on a permission prompt or failed (a
  pass stalls until you answer), else **working**, **idle** while it waits for a PR to change (or
  for its time, `waits for mon-fri 17:45`; or between passes, for one that loops), or **closed**
  when it isn't running, where `enter` starts it. `K` twice stops it, and it stays stopped until
  `enter` starts it or you run Claude Code in its checkout yourself (`watcherStopped` in
  `state.json`). One that `foreman watch` would wake reads `nothing wakes it: foreman watch isn't
  running`, in red, while none runs.
- Its checkout is a worktree nobody works in, detached at `origin/develop`. The skills run from
  it, so foreman moves it forward before every pass, a start's included: it fetches the branch
  (origin's default when it has no `develop`) and checks it out detached. A failed fetch moves
  it to the tip last fetched. Untracked files stay, and so do changes git can carry over; a change
  the move would overwrite fails it, and the pass waits, with the error in `foreman watch`'s log,
  until the checkout can move. One you put on a branch is yours and stays where it is. A skill
  that moved on may change the searches it runs: `prs` in the config doesn't follow them.

### Waking a watcher

A watcher with `prs` (GitHub searches) costs no model turn while nothing happens: [`foreman
watch`](#foreman-watch) polls its PRs and types its prompt into its pane, once, only when one of them changed. It types `/clear`
first, so each pass starts on an empty conversation. A wake waits while you talk to the watcher:
until 30 minutes after the last thing you typed into it since its pass began. Then what you asked
it is gone (`/resume` in its pane brings it back), and it reads a PR's state from GitHub again
when you ask.

- It polls when it starts and then every 2 minutes, with one `gh api graphql` request per
  search, scoped to the repo the checkout's `origin` points at. `fld-pr-watch`'s are the two
  `/fld-watch` runs: `author:@me is:open draft:false -label:agent-review-done -base:main` and
  `user-review-requested:@me is:open draft:false -base:main`.
- Each PR's signature is its head commit, the names of its failing checks (a check queuing,
  running or passing doesn't wake it; a new failure does), whether it has a merge conflict, and
  the newest comment or review by someone other than you. A PR whose signature differs from the
  one recorded at the watcher's last wake, or that is new to the searches, wakes it; a PR leaving
  them doesn't. The signatures are in `state.json` under `watcherPrs`, and `foreman watch`'s
  log says which PRs woke it.
- A watcher that is working or waits on a permission prompt isn't typed into: the next poll tries
  again, and still sees the change, since signatures are only recorded on a wake. Nor is one whose
  program reports no state (still starting, or no longer Claude Code), or one you typed into in the
  last 30 minutes since its pass began. That is read from its newest Claude Code conversation in
  the checkout, where a pass's prompt is the one carrying the allowlist rules. A closed one is started
  first, unless you stopped it.
- Starting a watcher records its PRs' signatures first, so the pass its prompt runs isn't followed
  by a wake for the same PRs.
- The poll runs in `foreman watch` alone. The board polls nothing, so opening, closing or
  restarting it changes nothing for a watcher. While `foreman watch` isn't running the watcher
  sits idle after its last pass, and its row in the queue says so: nothing is reviewed until it
  runs again, and nothing is lost, since its first poll compares against the last wake, not the
  last poll.
- The signatures are recorded once the prompt is typed, so a wake that fails is tried again at
  the next poll.
- `on` picks which of those wake it: `commit`, `checks`, `conflict`, `comment`, and `threads`, a
  review thread opened or resolved, which is in no signature without it. Without `on`: the first
  four. `fld-pr-watch` takes `["commit", "threads"]`: a new head is a review due and a thread
  resolved may finish one, while a failing check or someone's comment gives it nothing to do.
- `batch` is how many PRs a pass takes at most: three for `/fld-watch`. A wake then counts only
  that many changed PRs as seen, lowest number first. The rest still differ at the next poll and
  wake it again once it rests, so every changed PR gets a pass; the pass a start runs counts the
  same way.
- foreman tells the watcher nothing about the PRs: which are due, for what, and what else
  to do about them (`/fld-watch` also labels a PR whose threads are all resolved) is the skill's
  to work out, by its own rules. foreman holds none of them, so they can change with the skill.
  A PR whose review failed is taken again by the next pass, whichever PR wakes it, and by its
  morning sweep (`at`) at the latest.
- A watcher with neither `prs` nor `at` gets its prompt only at start, so it must keep itself
  going with `/loop 10m …`, as watchers did before the poll. One `foreman watch` wakes can't have a
  `/loop` prompt: each wake would start another schedule.

### Waking a watcher by the clock

A watcher with `at` runs its prompt at a time of day: `"17:45"` every day, or days and a time,
`"mon-fri 17:45"`, `"mon,wed 9:00"`, `"fri-mon 8:00"`. The time is the machine's local time.

- The same poll does it, so a pass starts within two minutes of its time, on an empty
  conversation (`/clear` first), and costs no model turn in between. A watcher with only `at`
  starts at its prompt, without a pass.
- The latest time dealt with is in `state.json` under `watcherRan`, recorded once the prompt is
  typed: a wake that fails is tried again at the next poll.
- A watcher that is working, waits on a prompt, was typed into in the last 30 minutes or isn't
  running is left for the next poll, and gets its pass once it rests.
- A time that passed while `foreman watch` wasn't running, or while the watcher was busy, still runs the
  same day. On a later day it is skipped: a pass about a day reports on the wrong one a day late.
- A watcher new to the config starts from its next time, not the one before.
- Unlike `/loop` or a scheduled job inside the session, the schedule doesn't end with the
  conversation or expire after a week, and it types the prompt as you would, so it can name a
  skill with `disable-model-invocation`.
- With `prs` too, `at` is a sweep beside the wakes its PRs cause: a full pass that takes what
  those left undone, such as a review that failed. `fld-pr-watch` has one each weekday morning.
- A watcher gets no MCP servers from `~/.config/mcp/mcp.json`; one it needs goes in a
  `.mcp.json` in its `dir`.

### `foreman watch`

`foreman watch` looks after the watchers, with the board open or closed: it starts them, polls
and wakes them, PRs and schedules alike, until it is stopped. Run by launchd from login, it keeps
reviews and scheduled passes going, and tells you what you would otherwise have to open the queue
to see. The board does none of that: its queue lists the watchers, and starts or stops one on
your key.

- Every poll it starts each watcher whose session is gone (Rex restarted, Claude Code exited),
  unless you stopped it.
- A dispatched agent that stops at a permission prompt or a question in its pane, or fails,
  raises one too, `FLD-1700 waits on you: permission …`, once per thing it waits on. It is seen at
  the next poll, so up to two minutes late, and not at all while `foreman watch` isn't running. A
  question in `foreman ask` raises [its own](#questions-from-agents) as it is asked, with or
  without `foreman watch`.
- A watcher that stops at a permission prompt or a question, or fails, raises a macOS
  notification, once per thing it waits on: its pass stalls until you answer, and with the board
  closed only Rex's session list would show it. Nothing it wasn't allowed runs meanwhile.
- It waits for Rex, looking every 15 seconds, and prints each start, wake, notification and
  failure with its time.
- One runs at a time: it holds `state.json.watch.lock` for its life, and a second `foreman
  watch` exits with `foreman watch already runs`, since two would each wake a watcher for the
  same change. The queue reads the same lock to say when none runs. Starting the watchers takes
  a lock of its own, so a poll and the queue's `enter` never start one twice.
- It runs until it is stopped, and exits with status 0 only when the config names no watcher, so
  launchd's `KeepAlive.SuccessfulExit = false` restarts it after a crash and not after that.

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
  "reviewCheck": "review-tier",
  "ignoreChecks": ["post review-tier status"],
  "watchers": [{
    "name": "fld-pr-watch",
    "dir": "~/work/code/reports-app-watch",
    "prompt": "/fld-watch --once",
    "prs": ["author:@me is:open draft:false -label:agent-review-done -base:main", "user-review-requested:@me is:open draft:false -base:main"],
    "on": ["commit", "threads"],
    "batch": 3,
    "at": "mon-fri 9:00"
  }]
}
```

`reviewCheck` and `ignoreChecks` say which of a PR's checks aren't CI; see [Reviews](#reviews).
`"reviewCheck": ""` and `"ignoreChecks": []` turn that off. `"watchers": []` turns them off. A watcher's `prs` is optional, and `on` and `batch` need them;
see [Waking a watcher](#waking-a-watcher). `at` is optional too, with `prs` or without; see
[Waking a watcher by the clock](#waking-a-watcher-by-the-clock).

## State

- State (repo per project, last agent per repo, dispatched tickets, PRs and tasks with their sessions and titles, each session's agent pane, the last task number per repo, the question each agent asked and those it went on without, queue items
  marked reviewed, each watcher's PR signatures at its last wake, each scheduled watcher's last time, the watchers you stopped, the branch each cleaned-up ticket's worktree was on) is in
  `~/.local/state/foreman/state.json`, written atomically. foreman and its commands (`dispatch`, `ask`, `answer`…) all write it,
  so each write holds `state.json.lock`, and foreman re-reads it every few seconds. An unreadable
  file is moved to `state.json.corrupt` rather than overwritten.
- `~/.local/state/foreman/glance.json` is what [an agent's status line](#an-agents-status-line)
  reads of the queue: the keys under **needs you** and **review**, and for each dispatch the PR on
  its branch with what it waits on. The board writes it, with its own pid, when its queue
  changes; one whose pid no longer runs is read as none.
- What happens in each dispatch is appended to `~/.local/state/foreman/events.jsonl`, one JSON
  line per event, which nothing rewrites or removes: a cleanup or the key's next dispatch keeps it.

  ```json
  {"at":"2026-11-22T12:06:28.877Z","key":"FLD-1","dispatch":"20261122T120628.877Z","by":"agent","kind":"question","text":"Which store? [Redis | Postgres]"}
  ```

  `dispatch` is the dispatch's id, the time it started, which tells apart the runs of a key
  dispatched more than once; a dispatch from before the log has none. `by` is `agent`,
  `orchestrator`, `me` or `foreman`, for what foreman saw itself. `kind` is one of:
  - `dispatched`, with `title`, `agent`, `dir` (where the agent works), `repo` (its main checkout)
    and `startedFrom`.
  - `question`, `decision` (one it went on without), `answer`, `confirm`, `aside`, `reply`,
    `expired` and `review` (my comments on its diff, sent): what the queue lists under
    **Questions**.
  - `reported` the first time the queue or `foreman watch` sees its `report.md`, and
    `report-rewritten` each time it changes after; `at` is the report's time and `text` its first
    line.
  - `pr-opened` when the queue loads a PR of mine on a ticket's or task's worktree branch, `text`
    being its key and URL; `pr-merged` or `pr-closed` once that PR is no longer among the open
    ones, `text` being its key and `at` the time GitHub gives.
  - `ticket-done` when the board finds a dispatched ticket Done, each time its tickets load; and
    for a review or a fix, whose key is its PR's, `pr-merged` or `pr-closed` when the board finds
    the dispatch over (in `^x`, or as it drops finished agents from the queue).
  - `cleaned-up`, with the reason `^x` gave, or `worktree gone`.

  Each change is logged once, whichever process sees it first: the dispatch's record in
  `state.json` keeps its `id`, the time of the report logged (`reported`), the PR logged (`pr`),
  how that PR ended (`prEnded`) and how the dispatch did (`ended`). Each append holds a lock on the file, so lines from `foreman ask`,
  the queue and `foreman watch` never interleave. A `questions.jsonl` an older dispatch left
  beside its report is still shown, and moves into the log before a new dispatch or a cleanup
  removes the directory.

## Development

`go test ./...` covers the pure logic and the worktree branch choice against throwaway git repos.

A running `foreman watch` keeps the code it started with: after `go build -o ~/.local/bin/foreman .`
restart it with `launchctl kickstart -k gui/$(id -u)/org.nixos.foreman-watch`. It reads the config
once too, so the same goes for a change to `config.json`. The board keeps its code as well:
`rex kill foreman && foreman open`. A change to how the watchers are started, polled or woken
needs only the first: the board does none of it.

After a change to what `state.json` holds, restart both before anything writes it: a foreman still
running the old code saves the state without the fields it doesn't know. A file from before each
dispatch was one record under `dispatches` (a map per field: `dispatched`, `sessions`, `agents`,
`titles`, `startedFrom`, `hidden`) is read as it is and rewritten at the next save.

Every call to Rex goes through `rexAPI` (`rex.go`); `rexCLI` (`rex_cli.go`) is the only code that
runs `rex`, and the tests put a fake in its place.

`rex/foreman.lua` runs inside Rex, not in foreman. `go test` covers it as well: `lua_test.go`
runs it in gopher-lua, the Lua Rex embeds, against a stand-in for the `rex` table, with
`foreman repos` answered by the command itself. To try a change in Rex before reloading the
config, `rex do rex/foreman.lua --action foreman_task`: the prompts show in the app, and a task
that is answered is really dispatched. `rex config reload` loads the file for the key.
