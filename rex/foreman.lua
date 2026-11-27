-- foreman in Rex's command palette, from whatever session I am in, without a trip to the board.
--
-- New Foreman Task… hands an agent a task. It asks for the repo (the focused pane's first, and
-- No repo last), the agent (the repo's usual one first) and the task, then runs `foreman dispatch`,
-- whose result comes as a notification.
--
-- Show Foreman Page opens the PR of the agent in the focused pane, else its ticket, in a browser
-- split beside that pane.
--
-- In ~/.config/rex/init.lua:
--
--     package.path = os.getenv("HOME") .. "/projects/foreman/rex/?.lua;" .. package.path
--     require("foreman")
--     rex.bind("cmd+alt+t", "foreman_task")
--     rex.bind("cmd+alt+b", "foreman_page")

local terminal = "com.superlogical.terminal"

-- foreman runs through the login shell: the server's own PATH has neither foreman nor what it runs
-- (rex, git, the agents). What I picked or typed travels in the environment and the command line
-- only names the variables, so the shell never reads a quote in a task as its own.
local function shell(command)
  return { os.getenv("SHELL") or "/bin/sh", "-lc", command }
end

-- The directory of the program in the focused pane. A remote host's pane has none here: its
-- directory is on that host, and the repos are on this one.
local function pane_dir(ctx)
  if ctx.server or not ctx.block_id then return nil end
  local proc = rex.block.call(terminal, "process", { block_id = ctx.block_id })
  return proc and proc.foreground and proc.foreground.cwd or nil
end

-- What a dispatch can take: { repos = { { name, path, agent, here } }, agents = { { label, note } },
-- noRepoAgent }.
local function listing(ctx)
  local r, err = rex.exec(shell('foreman repos --json --in "$FOREMAN_DIR"'),
    { env = { FOREMAN_DIR = pane_dir(ctx) or "" }, timeout = 15 })
  if not r then error(err, 0) end
  if not r.ok then
    local why = r.stderr:gsub("%s+$", "")
    error(why ~= "" and why or "foreman repos failed", 0)
  end
  return rex.json.decode(r.stdout)
end

-- answered passes a prompt's answer on, nil when I closed the prompt or left it open. Any other
-- reason Rex couldn't ask fails the action with that reason.
local function answered(value, why)
  if value == nil and why ~= "cancelled" and why ~= "timeout" then
    error("New Foreman Task: " .. tostring(why), 0)
  end
  return value
end

-- add puts item at the head of items when it is the one to offer first, else at the end.
local function add(items, item, first)
  if first then table.insert(items, 1, item) else items[#items + 1] = item end
end

local function tilde(path)
  local home = os.getenv("HOME") or ""
  if home ~= "" and path:sub(1, #home + 1) == home .. "/" then return "~" .. path:sub(#home + 1) end
  return path
end

-- The id of the repo pick's No repo row: a repo's id is its absolute path.
local no_repo = "none"

local function run(ctx)
  local list = listing(ctx)

  local items, names, usual = {}, {}, {}
  for _, r in ipairs(list.repos or {}) do
    names[r.path], usual[r.path] = r.name, r.agent
    add(items, {
      id = r.path, title = r.name, subtitle = tilde(r.path), icon = "folder",
      detail = r.here and "here" or nil,
    }, r.here)
  end
  names[no_repo], usual[no_repo] = nil, list.noRepoAgent
  items[#items + 1] = {
    id = no_repo, title = "No repo", subtitle = "A scratch directory, for a question or work outside the repos",
    icon = "questionmark.circle",
  }
  local repo = answered(rex.ui.pick{ title = "New task", placeholder = "Repo for the agent to work in", items = items })
  if repo == nil then return end

  items = {}
  for _, a in ipairs(list.agents or {}) do
    add(items, { id = a.label, title = a.label, subtitle = a.note, icon = "terminal" }, a.label == usual[repo])
  end
  local where = repo == no_repo and ", with no repo" or " in " .. names[repo]
  local agent = answered(rex.ui.pick{
    title = repo == no_repo and "Agent, with no repo" or "Agent in " .. names[repo],
    placeholder = "Agent to start", items = items,
  })
  if agent == nil then return end

  local task = answered(rex.ui.input{
    title = "Task for " .. agent .. where,
    placeholder = repo == no_repo and "What to ask or do; its first words name it"
      or "What to do; its first words name the branch",
    confirm = "Dispatch",
  })
  if task == nil then return end
  task = task:gsub("^%s+", "")
  task = task:gsub("%s+$", "")
  if task == "" then return end

  -- A dispatch fetches origin first and may outlast the 30 seconds an action has, so it runs on by
  -- itself. Nothing reads its output: --notify shows how it ended.
  local command = 'foreman dispatch --notify --repo "$FOREMAN_REPO" --agent "$FOREMAN_AGENT" -- "$FOREMAN_TASK"'
  if repo == no_repo then
    command = 'foreman dispatch --notify --agent "$FOREMAN_AGENT" -- "$FOREMAN_TASK"'
  end
  local started, err = rex.exec.start(
    shell(command),
    {
      env = { FOREMAN_REPO = repo ~= no_repo and repo or nil, FOREMAN_AGENT = agent, FOREMAN_TASK = task },
      detach = true, stdout = false, stderr = false,
    })
  if not started then error(err, 0) end
  return { repo = repo, agent = agent }
end

-- foreman page finds the dispatch by the pane and opens the page beside it: REX_BLOCK is the pane
-- it takes itself to be in. It asks GitHub for the PR, so it gets most of the 30 seconds an
-- action has.
local function page(ctx)
  if ctx.server then error("Show Foreman Page: the pane is on " .. tostring(ctx.server) .. ", and foreman's dispatches are on this host", 0) end
  if not ctx.block_id then error("Show Foreman Page: no pane is in focus", 0) end
  local r, err = rex.exec(shell('foreman page --in "$FOREMAN_DIR"'),
    { env = { FOREMAN_DIR = pane_dir(ctx) or "", REX_BLOCK = ctx.block_id }, timeout = 25 })
  if not r then error(err, 0) end
  if not r.ok then
    local why = r.stderr:gsub("%s+$", "")
    error(why ~= "" and why or "foreman page failed", 0)
  end
end

rex.action{
  name        = "foreman_page",
  title       = "Show Foreman Page",
  description = "Open the PR of the agent in this pane, or else its ticket, in a browser beside it",
  keywords    = "pr pull request ticket jira browser agent open",
  run         = page,
}

rex.action{
  name        = "foreman_task",
  title       = "New Foreman Task…",
  description = "Choose a repo, or none, and an agent, say what to do, and foreman starts the agent on it",
  keywords    = "dispatch agent task repo worktree question pi claude",
  run         = run,
}
