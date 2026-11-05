import { join } from "node:path";
import { homedir } from "node:os";
import { listRepos, sh } from "./dispatch.ts";

// Only work checkouts: ~/projects holds third-party clones whose PRs are noise here.
const PR_ROOT = join(homedir(), "work", "code");

/** A PR in the board's row shape: key `repo#n`, status, author as assignee. */
export interface Pr {
  key: string;
  summary: string;
  status: string;
  category: string;
  assignee: string;
  repo: string; // owner/name
  number: number;
  url: string;
  checkout: string;
  requested: boolean;
}

export interface PrView {
  title: string;
  body: string;
  url: string;
  author: { login: string };
  headRefName: string;
  baseRefName: string;
  headRefOid: string;
  additions: number;
  deletions: number;
  changedFiles: number;
  isDraft: boolean;
  reviewDecision: string;
  files: { path: string; additions: number; deletions: number }[];
}

/** owner/name → local checkout, for checkouts under PR_ROOT whose origin is on GitHub. */
async function checkouts(): Promise<Map<string, string>> {
  const map = new Map<string, string>();
  const repos = listRepos().filter((r) => r.startsWith(PR_ROOT + "/"));
  await Promise.all(
    repos.map(async (dir) => {
      const url = (await sh(["git", "-C", dir, "remote", "get-url", "origin"]).catch(() => "")).trim();
      const m = url.match(/github\.com[:/]([^/]+\/[^/]+?)(?:\.git)?\/?$/i);
      if (m) map.set(m[1]!.toLowerCase(), dir);
    }),
  );
  return map;
}

let login: Promise<string> | undefined;
export const me = () => (login ??= sh(["gh", "api", "user", "-q", ".login"]).then((s) => s.trim()));

/** Open PRs by humans other than me in repos I have checked out; review requests first. */
export async function openPrs(): Promise<Pr[]> {
  const [repos, self] = await Promise.all([checkouts(), me()]);
  if (!repos.size) return [];
  const repoArgs = [...repos.keys()].flatMap((r) => ["--repo", r]);
  const search = (...extra: string[]) =>
    sh(["gh", "search", "prs", "--state", "open", "--limit", "500", ...repoArgs, ...extra]).then((s) => JSON.parse(s) as any[]);
  const [all, requested] = await Promise.all([
    search("--json", "number,title,repository,author,url,isDraft,updatedAt"),
    search("--review-requested", "@me", "--json", "number,repository"),
  ]);
  const wanted = new Set(requested.map((p) => `${p.repository.nameWithOwner}#${p.number}`.toLowerCase()));
  return all
    .filter((p) => !p.author?.is_bot && p.author?.login !== self)
    .map((p) => {
      const repo: string = p.repository.nameWithOwner;
      const isRequested = wanted.has(`${repo}#${p.number}`.toLowerCase());
      return {
        pr: {
          key: `${p.repository.name}#${p.number}`,
          summary: p.title,
          status: isRequested ? "requested" : p.isDraft ? "draft" : "open",
          category: isRequested ? "In Progress" : "To Do",
          assignee: p.author?.login ?? "",
          repo,
          number: p.number,
          url: p.url,
          checkout: repos.get(repo.toLowerCase())!,
          requested: isRequested,
        } satisfies Pr,
        updated: p.updatedAt as string,
      };
    })
    .sort((a, b) => Number(b.pr.requested) - Number(a.pr.requested) || b.updated.localeCompare(a.updated))
    .map((x) => x.pr);
}

export async function view(pr: Pr): Promise<PrView> {
  const fields = "title,body,url,author,headRefName,baseRefName,headRefOid,additions,deletions,changedFiles,isDraft,reviewDecision,files";
  return JSON.parse(await sh(["gh", "pr", "view", String(pr.number), "-R", pr.repo, "--json", fields]));
}

export function markdown(pr: Pr, v: PrView): string {
  const files = v.files.map((f) => `- \`${f.path}\` +${f.additions} −${f.deletions}`);
  return [
    `# ${pr.key}: ${v.title}`,
    "",
    v.url,
    [
      `Author: ${v.author.login}`,
      `\`${v.headRefName}\` → \`${v.baseRefName}\``,
      `+${v.additions} −${v.deletions} in ${v.changedFiles} files`,
      v.isDraft ? "Draft" : "",
      v.reviewDecision ? `Review: ${v.reviewDecision.toLowerCase().replace(/_/g, " ")}` : "",
    ]
      .filter(Boolean)
      .join(" · "),
    "",
    v.body.replace(/\r/g, "").trim() || "_No description._",
    "",
    "## Files",
    "",
    ...files,
  ].join("\n");
}

/** The first Jira key in the PR's title or branch, e.g. FLD-1574. */
export function jiraKey(pr: Pr, v?: PrView): string | undefined {
  return `${pr.summary} ${v?.headRefName ?? ""}`.toUpperCase().match(/\b[A-Z][A-Z0-9]+-\d+\b/)?.[0];
}
