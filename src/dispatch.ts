import { existsSync, mkdirSync, readdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";
import type { Issue, Jira } from "./jira.ts";
import { jiraKey, markdown, view, type Pr } from "./github.ts";

export const REPO_ROOTS = [join(homedir(), "work", "code"), join(homedir(), "projects")];
const BRANCH_PREFIX = "rf/";
const STATE_FILE = join(homedir(), ".local", "state", "foreman", "state.json");
const MCP_CONFIG = join(homedir(), ".config", "mcp", "mcp.json");

export const AGENTS = [
  { label: "pi", note: "pi, default tools" },
  { label: "Claude", note: "claude, default permission prompts" },
  { label: "Claude (yolo)", note: "claude --dangerously-skip-permissions" },
] as const;
export type AgentLabel = (typeof AGENTS)[number]["label"];

export interface Dispatch {
  issue: Issue;
  agent: AgentLabel;
  worktree: boolean;
  repo: string;
  context: string;
}

export type State = { repoByProject: Record<string, string>; agent?: AgentLabel; dispatched: Record<string, string> };

export function loadState(): State {
  try {
    return { repoByProject: {}, dispatched: {}, ...JSON.parse(readFileSync(STATE_FILE, "utf8")) };
  } catch {
    return { repoByProject: {}, dispatched: {} };
  }
}

export function saveState(s: State): void {
  mkdirSync(dirname(STATE_FILE), { recursive: true });
  writeFileSync(STATE_FILE, JSON.stringify(s, null, 2));
}

export function listRepos(): string[] {
  const repos: string[] = [];
  for (const root of REPO_ROOTS) {
    let entries: string[] = [];
    try {
      entries = readdirSync(root);
    } catch {
      continue;
    }
    for (const name of entries.sort((a, b) => a.localeCompare(b, undefined, { sensitivity: "base" }))) {
      const dir = join(root, name);
      // A main checkout has a .git directory; worktrees have a .git file.
      try {
        if (statSync(join(dir, ".git")).isDirectory()) repos.push(dir);
      } catch {}
    }
  }
  return repos;
}

export const tilde = (p: string) => p.replace(homedir(), "~");

function slug(text: string): string {
  return text
    .toLowerCase()
    .normalize("NFKD")
    .replace(/[^a-z0-9]+/g, "-")
    .replace(/^-|-$/g, "")
    .split("-")
    .slice(0, 6)
    .join("-");
}

export async function sh(cmd: string[], cwd?: string): Promise<string> {
  const p = Bun.spawn(cmd, { cwd, stdout: "pipe", stderr: "pipe" });
  const [out, err, code] = await Promise.all([new Response(p.stdout).text(), new Response(p.stderr).text(), p.exited]);
  if (code !== 0) throw new Error(err.trim() || `${cmd.join(" ")} exited ${code}`);
  return out;
}

function which(bin: string): string {
  return Bun.which(bin) ?? bin;
}

/** Creates or reuses the ticket's worktree and starts the agent on it in a new Rex session named after the ticket. */
export async function dispatch(jira: Jira, d: Dispatch, progress: (line: string) => void): Promise<string> {
  const { issue, repo } = d;
  const keyLower = issue.key.toLowerCase();
  const git = (...args: string[]) => sh(["git", "-C", repo, ...args]);

  let cwd = repo;
  let branch = (await git("branch", "--show-current")).trim();
  if (d.worktree) {
    cwd = `${repo}-${keyLower}`;
    if (existsSync(cwd)) {
      branch = (await sh(["git", "-C", cwd, "branch", "--show-current"])).trim();
      progress(`Reusing worktree ${tilde(cwd)}`);
    } else {
      progress("Fetching origin…");
      await git("fetch", "--quiet", "origin").catch(() => {});
      const remotes = (await git("branch", "-r", "--format=%(refname:short)")).split("\n");
      const keyRe = new RegExp(`(^|[^a-z0-9])${keyLower}($|[^0-9])`);
      const existing = remotes.find((b) => keyRe.test(b.toLowerCase()));
      const base = ["origin/develop", "origin/main", "origin/master"].find((b) => remotes.includes(b));
      if (existing) {
        branch = existing.replace(/^origin\//, "");
        await git("worktree", "add", "-B", branch, cwd, existing);
        await sh(["git", "-C", cwd, "branch", "--set-upstream-to", existing]).catch(() => {});
      } else {
        branch = `${BRANCH_PREFIX}${keyLower}-${slug(issue.summary)}`;
        await git("worktree", "add", "--no-track", "-b", branch, cwd, ...(base ? [base] : []));
      }
      progress(`Created worktree ${tilde(cwd)} on ${branch}${existing ? "" : ` from ${base ?? "HEAD"}`}`);
    }
  }

  progress("Reading the ticket…");
  const ticket = await jira.detail(issue.key);
  const prompt = [
    `# ${issue.key}: "${issue.summary}"`,
    "",
    `You are implementing Jira ticket ${issue.key}.`,
    `Working directory: ${cwd}`,
    `Branch: ${branch}${d.worktree ? " (a git worktree created for this ticket)" : ""}`,
    "",
    "## Ticket",
    "",
    ticket,
    ...(d.context ? ["", "## Extra context from me", "", d.context] : []),
    "",
    "## Workflow",
    "",
    "1. **Understand.** Read the relevant code and the repo's AGENTS.md / CLAUDE.md before changing anything. If the ticket is ambiguous, ask me instead of guessing.",
    "2. **Implement** to the ticket's acceptance criteria. Keep scope to the ticket; respect its constraints and out-of-scope notes.",
    "3. **Verify.** Build, and run the tests and linters that cover what you touched.",
    "4. **Commit** on this branch. Do not push, open a PR, or change the Jira ticket.",
    "5. **Finish** with a short summary: what changed, how you verified it, and anything left open.",
  ].join("\n");

  const session = await start(d.agent, prompt, cwd, `${keyLower}-${slug(issue.summary).split("-").slice(0, 3).join("-")}`, progress);
  markDispatched(issue.key, cwd);
  return `✓ ${d.agent} is working on ${issue.key} in session ${session} · ${tilde(cwd)} (${branch})`;
}

export interface ReviewDispatch {
  pr: Pr;
  agent: AgentLabel;
  context: string;
}

/** Checks the PR's head out detached in `<repo>-pr-<n>` (refreshed if it exists) and starts a read-only review. */
export async function dispatchReview(jira: Jira, d: ReviewDispatch, progress: (line: string) => void): Promise<string> {
  const { pr } = d;
  const repo = pr.checkout;
  const cwd = `${repo}-pr-${pr.number}`;
  progress(`Reading ${pr.key}…`);
  const v = await view(pr);

  progress("Fetching the PR head…");
  // Naming the base updates origin/<base> too, so the diff's left side is current.
  await sh(["git", "-C", repo, "fetch", "--quiet", "origin", v.baseRefName, `pull/${pr.number}/head`]);
  const head = v.headRefOid;
  if (existsSync(cwd)) {
    const dirty = (await sh(["git", "-C", cwd, "status", "--porcelain"])).trim();
    if (dirty) throw new Error(`${tilde(cwd)} has uncommitted changes; clean it up or remove the worktree`);
    await sh(["git", "-C", cwd, "checkout", "--quiet", "--detach", head]);
    progress(`Updated worktree ${tilde(cwd)} to ${v.headRefOid.slice(0, 8)}`);
  } else {
    await sh(["git", "-C", repo, "worktree", "add", "--detach", cwd, head]);
    progress(`Created worktree ${tilde(cwd)} at ${v.headRefOid.slice(0, 8)}`);
  }

  const key = jiraKey(pr, v);
  let ticket: string | undefined;
  if (key) {
    progress(`Reading ${key}…`);
    ticket = await jira.detail(key).catch(() => undefined);
  }
  const base = `origin/${v.baseRefName}`;
  const prompt = [
    `# Review ${pr.repo}#${pr.number}: "${v.title}"`,
    "",
    `You are reviewing a pull request by ${v.author.login}. Do not change it.`,
    `Working directory: ${cwd}`,
    `Checked out: the PR head ${v.headRefOid} (detached), a git worktree created for this review.`,
    `Diff to review: \`git diff ${base}...HEAD\` (\`${v.headRefName}\` → \`${v.baseRefName}\`).`,
    "",
    "## Pull request",
    "",
    markdown(pr, v),
    ...(ticket ? ["", "## Jira ticket", "", ticket] : key ? ["", `The PR mentions ${key}, but the ticket could not be read.`] : []),
    ...(d.context ? ["", "## Extra context from me", "", d.context] : []),
    "",
    "## Workflow",
    "",
    "1. **Understand.** Read the repo's AGENTS.md / CLAUDE.md, the PR description" + (ticket ? ", the ticket's acceptance criteria" : "") + ", and the code around each change.",
    "2. **Review** the diff for correctness, missed cases, regressions, security, tests, and fit with the codebase's conventions" + (ticket ? "; check it against the ticket" : "") + ".",
    "3. **Verify** where it helps: run the tests and linters that cover the changed code. Don't edit tracked files.",
    "4. **Report** in your final message: a verdict, then findings ordered by severity, each with `file:line`, what is wrong and a suggested fix. Separate blocking issues from nits.",
    "",
    "Do not commit, push, comment on, approve or request changes on the PR, or touch the Jira ticket. I'll post the review myself.",
  ].join("\n");

  const session = await start(d.agent, prompt, cwd, `review-${slug(pr.key)}`, progress);
  markDispatched(pr.key, cwd);
  return `✓ ${d.agent} is reviewing ${pr.key} in session ${session} · ${tilde(cwd)}`;
}

async function start(agent: AgentLabel, prompt: string, cwd: string, label: string, progress: (line: string) => void): Promise<string> {
  // Absolute paths: the fish `claude` function would otherwise add a second --mcp-config.
  const cmd =
    agent === "pi"
      ? [which("pi"), prompt]
      : [which("claude"), "--mcp-config", MCP_CONFIG, ...(agent === "Claude (yolo)" ? ["--dangerously-skip-permissions"] : []), prompt];
  progress(`Starting ${agent}…`);
  // `rex new` starts the session in the background; the app stays on the board.
  const session = await freeSessionLabel(label);
  await sh(["rex", "new", session, "--cwd", cwd, "--window", agent, "--", ...cmd]);
  return session;
}

function markDispatched(key: string, cwd: string) {
  const state = loadState();
  state.dispatched[key] = cwd;
  saveState(state);
}

async function freeSessionLabel(base: string): Promise<string> {
  const { sessions } = JSON.parse(await sh(["rex", "ls", "--json"])) as { sessions: { label: string }[] };
  const taken = new Set(sessions.map((s) => s.label));
  let label = base;
  for (let n = 2; taken.has(label); n++) label = `${base}-${n}`;
  return label;
}
