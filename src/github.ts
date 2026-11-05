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
  mine: boolean;
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
  mergeable: string;
  statusCheckRollup: { name?: string; context?: string; status?: string; conclusion?: string; state?: string }[];
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

// One GraphQL request carries every search, so review requests and my PRs ride on the first page of
// all open PRs. Only mine ask for review, CI and merge state: on every PR those fields make the
// search several times slower. Search pages hold 100, so each further 100 PRs is another request.
const PRS = `query($all: String!, $requested: String!, $mine: String!, $first: Boolean!, $after: String) {
  all: search(query: $all, type: ISSUE, first: 100, after: $after) {
    pageInfo { hasNextPage endCursor }
    nodes { ... on PullRequest { number title url isDraft updatedAt repository { name nameWithOwner } author { __typename login } } }
  }
  requested: search(query: $requested, type: ISSUE, first: 100) @include(if: $first) {
    nodes { ... on PullRequest { number repository { nameWithOwner } } }
  }
  mine: search(query: $mine, type: ISSUE, first: 100) @include(if: $first) {
    nodes { ... on PullRequest {
      number reviewDecision mergeable repository { nameWithOwner }
      commits(last: 1) { nodes { commit { statusCheckRollup { state } } } }
    } }
  }
}`;

/** My PR's status cell and colour category from its review, CI and merge state. */
function myStatus(p: any): { status: string; category: string } {
  const ci: string | undefined = p.commits?.nodes?.[0]?.commit?.statusCheckRollup?.state;
  const mark = ci === "SUCCESS" ? " ✓" : ci === "FAILURE" || ci === "ERROR" ? " ✗" : ci ? " ●" : "";
  const review =
    p.isDraft ? "draft" : p.mergeable === "CONFLICTING" ? "conflict" : ({ APPROVED: "approved", CHANGES_REQUESTED: "changes" } as any)[p.reviewDecision] ?? "in review";
  const failing = mark === " ✗" || review === "changes" || review === "conflict";
  const category = failing ? "Blocked" : review === "approved" && mark !== " ●" ? "Done" : mark === " ●" ? "In Progress" : "To Do";
  return { status: review + mark, category };
}

/** Open PRs in repos I have checked out: review requests, then mine with their status, then other humans'. */
export async function openPrs(): Promise<Pr[]> {
  const [repos, self] = await Promise.all([checkouts(), me()]);
  if (!repos.size) return [];
  const scope = `is:pr is:open archived:false ${[...repos.keys()].map((r) => `repo:${r}`).join(" ")}`;
  const nodes: any[] = [];
  let requested: any[] = [];
  let mine: any[] = [];
  let after: string | undefined;
  for (let page = 0; page < 5; page++) {
    const vars = [`all=${scope}`, `requested=${scope} review-requested:@me`, `mine=${scope} author:@me`, ...(after ? [`after=${after}`] : [])];
    const args = ["gh", "api", "graphql", "-f", `query=${PRS}`, ...vars.flatMap((v) => ["-f", v]), "-F", `first=${page === 0}`];
    const { data } = JSON.parse(await sh(args));
    nodes.push(...data.all.nodes);
    if (page === 0) (requested = data.requested.nodes), (mine = data.mine.nodes);
    if (!data.all.pageInfo.hasNextPage) break;
    after = data.all.pageInfo.endCursor;
  }
  const id = (p: any) => `${p.repository.nameWithOwner}#${p.number}`.toLowerCase();
  const wanted = new Set(requested.map(id));
  const status = new Map(mine.map((p) => [id(p), p]));
  const rank = (pr: Pr) => (pr.requested ? 2 : pr.mine ? 1 : 0);
  return nodes
    .filter((p) => p.number && p.author?.__typename !== "Bot")
    .map((p) => {
      const repo: string = p.repository.nameWithOwner;
      const own = status.get(id(p));
      const isRequested = wanted.has(id(p));
      const state = own
        ? myStatus({ ...p, ...own })
        : { status: isRequested ? "requested" : p.isDraft ? "draft" : "open", category: isRequested ? "In Progress" : "To Do" };
      return {
        pr: {
          key: `${p.repository.name}#${p.number}`,
          summary: p.title,
          ...state,
          assignee: p.author?.login ?? "",
          repo,
          number: p.number,
          url: p.url,
          checkout: repos.get(repo.toLowerCase())!,
          requested: isRequested,
          mine: !!own,
        } satisfies Pr,
        updated: p.updatedAt as string,
      };
    })
    .sort((a, b) => rank(b.pr) - rank(a.pr) || b.updated.localeCompare(a.updated))
    .map((x) => x.pr);
}

export async function view(pr: Pr): Promise<PrView> {
  const fields = "title,body,url,author,headRefName,baseRefName,headRefOid,additions,deletions,changedFiles,isDraft,reviewDecision,mergeable,statusCheckRollup,files";
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
      v.mergeable === "CONFLICTING" ? "Merge conflicts" : "",
    ]
      .filter(Boolean)
      .join(" · "),
    "",
    v.body.replace(/\r/g, "").trim() || "_No description._",
    "",
    ...checks(v),
    "## Files",
    "",
    ...files,
  ].join("\n");
}

/** A Checks section listing failing and running checks, or one line when all pass. */
function checks(v: PrView): string[] {
  const all = (v.statusCheckRollup ?? []).map((c) => ({
    name: c.name ?? c.context ?? "?",
    state: (c.conclusion || c.state || c.status || "").toUpperCase(),
  }));
  if (!all.length) return [];
  const bad = all.filter((c) => ["FAILURE", "ERROR", "TIMED_OUT", "CANCELLED", "ACTION_REQUIRED"].includes(c.state));
  const running = all.filter((c) => ["", "PENDING", "QUEUED", "IN_PROGRESS", "EXPECTED", "WAITING"].includes(c.state));
  const lines = [
    ...bad.map((c) => `- ✗ **${c.name}** ${c.state.toLowerCase().replace(/_/g, " ")}`),
    ...running.map((c) => `- ● ${c.name} running`),
  ];
  const ok = all.length - bad.length - running.length;
  return ["## Checks", "", ...lines, ...(lines.length ? [""] : []), `${ok}/${all.length} passed`, ""];
}

/** The first Jira key in the PR's title or branch, e.g. FLD-1574. */
export function jiraKey(pr: Pr, v?: PrView): string | undefined {
  return `${pr.summary} ${v?.headRefName ?? ""}`.toUpperCase().match(/\b[A-Z][A-Z0-9]+-\d+\b/)?.[0];
}
