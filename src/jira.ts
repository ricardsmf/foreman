import type { Mcp } from "./mcp.ts";

export const CLOUD_ID = "1eb9eb54-74a9-4d94-8eb1-a1a7696be552"; // bloqit.atlassian.net
export const SITE = "https://bloqit.atlassian.net";

export interface Issue {
  key: string;
  summary: string;
  status: string;
  category: string;
  assignee: string;
  priority: string;
  type: string;
}

// Board column order; statuses not listed sort after these by category.
const STATUS_ORDER = ["in progress", "blocked", "in review", "testing", "staging", "ongoing", "ready for engineering", "to do", "refining", "triage", "backlog"];
const CATEGORY_ORDER: Record<string, number> = { "In Progress": 0, "To Do": 1, Done: 2 };
const rank = (i: Issue) => {
  const at = STATUS_ORDER.indexOf(i.status.toLowerCase());
  return at >= 0 ? at : STATUS_ORDER.length + (CATEGORY_ORDER[i.category] ?? 9);
};

export class Jira {
  constructor(private mcp: Mcp) {}

  private call(tool: string, args: Record<string, unknown>) {
    return this.mcp.call(tool, { cloudId: CLOUD_ID, ...args });
  }

  async issues(jql: string): Promise<Issue[]> {
    const issues: Issue[] = [];
    let nextPageToken: string | undefined;
    do {
      const data = await this.call("searchJiraIssuesUsingJql", {
        jql,
        maxResults: 100,
        fields: ["summary", "status", "assignee", "priority", "issuetype"],
        ...(nextPageToken ? { nextPageToken } : {}),
      });
      for (const i of data.issues ?? []) {
        issues.push({
          key: i.key,
          summary: i.fields.summary ?? "",
          status: i.fields.status?.name ?? "?",
          category: i.fields.status?.statusCategory?.name ?? "",
          assignee: i.fields.assignee?.displayName ?? "",
          priority: i.fields.priority?.name ?? "",
          type: i.fields.issuetype?.name ?? "",
        });
      }
      nextPageToken = data.isLast ? undefined : data.nextPageToken;
    } while (nextPageToken && issues.length < 500);
    return issues.sort((a, b) => rank(a) - rank(b) || a.assignee.localeCompare(b.assignee) || a.key.localeCompare(b.key));
  }

  async me(): Promise<string | undefined> {
    const data = await this.call("searchJiraIssuesUsingJql", { jql: "assignee = currentUser() ORDER BY updated DESC", maxResults: 1, fields: ["assignee"] });
    return data.issues?.[0]?.fields?.assignee?.displayName;
  }

  /** The ticket as markdown: header, description, linked issues. */
  async detail(key: string): Promise<string> {
    const data = await this.call("getJiraIssue", {
      issueIdOrKey: key,
      fields: ["summary", "description", "status", "assignee", "priority", "issuetype", "labels", "parent", "issuelinks"],
      responseContentFormat: "markdown",
    });
    const f = data.fields ?? {};
    const links = (f.issuelinks ?? [])
      .map((l: any) => {
        const other = l.outwardIssue ?? l.inwardIssue;
        const label = l.outwardIssue ? l.type?.outward : l.type?.inward;
        return other ? `- ${label ?? "relates to"} ${other.key}: ${other.fields?.summary ?? ""}` : undefined;
      })
      .filter(Boolean);
    return [
      `# ${key}: ${f.summary ?? ""}`,
      "",
      `${SITE}/browse/${key}`,
      [
        f.issuetype?.name && `Type: ${f.issuetype.name}`,
        f.status?.name && `Status: ${f.status.name}`,
        f.priority?.name && `Priority: ${f.priority.name}`,
        `Assignee: ${f.assignee?.displayName ?? "unassigned"}`,
      ]
        .filter(Boolean)
        .join(" · "),
      f.parent ? `Parent: ${f.parent.key} ${f.parent.fields?.summary ?? ""}` : "",
      f.labels?.length ? `Labels: ${f.labels.join(", ")}` : "",
      "",
      f.description || "_No description._",
      links.length ? `\n## Linked issues\n\n${links.join("\n")}` : "",
    ]
      .filter((l, i, a) => l !== "" || a[i - 1] !== "")
      .join("\n");
  }
}
