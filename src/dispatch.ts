import { existsSync, mkdirSync, readdirSync, readFileSync, statSync, writeFileSync } from "node:fs";
import { homedir } from "node:os";
import { dirname, join } from "node:path";
import type { Issue, Jira } from "./jira.ts";

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

async function sh(cmd: string[], cwd?: string): Promise<string> {
  const p = Bun.spawn(cmd, { cwd, stdout: "pipe", stderr: "pipe" });
  const [out, err, code] = await Promise.all([new Response(p.stdout).text(), new Response(p.stderr).text(), p.exited]);
  if (code !== 0) throw new Error(err.trim() || `${cmd.join(" ")} exited ${code}`);
  return out;
}

function which(bin: string): string {
  return Bun.which(bin) ?? bin;
}

/** Creates or reuses the ticket's worktree and starts the agent on it in a new Rex tab. */
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

  // Absolute paths: the fish `claude` function would otherwise add a second --mcp-config.
  const cmd =
    d.agent === "pi"
      ? [which("pi"), prompt]
      : [which("claude"), "--mcp-config", MCP_CONFIG, ...(d.agent === "Claude (yolo)" ? ["--dangerously-skip-permissions"] : []), prompt];
  progress(`Starting ${d.agent}…`);
  const rex = ["rex", "run", "--cwd", cwd, "--label", `${issue.key} ${d.agent}`, "--focus=false"];
  if (process.env.REX_SESSION) rex.push("-s", process.env.REX_SESSION);
  await sh([...rex, "--", ...cmd]);

  const state = loadState();
  state.dispatched[issue.key] = cwd;
  saveState(state);
  return `✓ ${d.agent} is working on ${issue.key} in ${tilde(cwd)} (${branch})`;
}
