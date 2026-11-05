#!/usr/bin/env bun
import { decodeKittyPrintable, matchesKey, ProcessTerminal, TuiAltScreen, truncateToWidth, visibleWidth, wrapTextWithAnsi, type Component } from "@earendil-works/pi-tui";
import { AGENTS, dispatch, dispatchReview, listRepos, loadState, saveState, tilde, type Dispatch } from "./dispatch.ts";
import { markdown, openPrs, view, type Pr } from "./github.ts";
import { Jira, SITE, type Issue } from "./jira.ts";
import { connect, signOut } from "./mcp.ts";

// foreman [PROJECT | JQL]: Jira board with assignees over a detail pane; Enter opens a
// dispatch form that starts pi or Claude Code on the ticket in a git worktree, in a new Rex tab.
// Tab switches to open PRs in the repos under ~/work/code (mine with review and CI status), where Enter dispatches a review.

const DEFAULT_PROJECT = "FLD";

// Catppuccin Macchiato
const C = {
  text: [202, 211, 245],
  subtext: [165, 173, 203],
  overlay: [128, 135, 162],
  surface: [73, 77, 100],
  mauve: [198, 160, 246],
  blue: [138, 173, 244],
  yellow: [238, 212, 159],
  green: [166, 218, 149],
  red: [237, 135, 150],
  peach: [245, 169, 127],
} as const;
type Color = keyof typeof C;
const fg = (c: Color, s: string) => `\x1b[38;2;${C[c].join(";")}m${s}\x1b[39m`;
const bold = (s: string) => `\x1b[1m${s}\x1b[22m`;

function fit(text: string, width: number): string {
  if (width <= 0) return "";
  const cut = truncateToWidth(text, width, "…");
  return cut + " ".repeat(Math.max(0, width - visibleWidth(cut)));
}

function typed(data: string): string {
  return decodeKittyPrintable(data) ?? (data.length === 1 && data >= " " && data !== "\x7f" ? data : "");
}

function markdownLine(l: string): string {
  if (l.startsWith("# ")) return bold(fg("mauve", l.slice(2)));
  if (/^#{2,} /.test(l)) return bold(fg("blue", l.replace(/^#+ /, "")));
  return l.replace(/\*\*(.+?)\*\*/g, (_, t) => bold(t)).replace(/`([^`]+)`/g, (_, t) => fg("peach", t));
}

function parseArgs(argv: string[]): { jql: string; title: string } {
  const arg = argv.join(" ").trim();
  const isProject = !arg || /^[A-Z][A-Z0-9]+$/i.test(arg);
  const project = (arg || DEFAULT_PROJECT).toUpperCase();
  return isProject
    ? { jql: `project = ${project} AND statusCategory != Done ORDER BY updated DESC`, title: `${project} open tickets` }
    : { jql: arg, title: "custom JQL" };
}

const open = (url: string) => void Bun.spawn(["open", url]);

type Row = Issue | Pr;
const isPr = (r: Row): r is Pr => "number" in r;

class Board implements Component {
  private view: "tickets" | "prs" = "tickets";
  private issues: Issue[] = [];
  private prs: Pr[] | undefined;
  private prFilter: "all" | "requested" | "mine" = "all";
  private cursor = 0;
  private top = 0;
  private query = "";
  private mine = false;
  private assigneeFilter: string | undefined; // undefined = everyone, "" = unassigned
  private pending = 0; // reloads in flight; tickets and PRs can load side by side
  private get loading() {
    return this.pending > 0;
  }
  private status = "";
  private statusColor: Color = "overlay";
  private detailScroll = 0;
  private details = new Map<string, string[] | "loading">();
  private detailTimer: ReturnType<typeof setTimeout> | undefined;
  private state = loadState();
  private repos = listRepos();
  private form:
    | { issue: Row; field: number; agent: number; worktree: boolean; repo: number; repoQuery: string; context: string; busy: boolean }
    | undefined;

  constructor(
    private tui: TuiAltScreen,
    private jira: Jira,
    private jql: string,
    private title: string,
    private me: string | undefined,
    private quit: () => void,
  ) {
    void this.reload();
  }

  private refresh = () => this.tui.requestRender();
  private say(text: string, color: Color = "overlay") {
    this.status = text;
    this.statusColor = color;
    this.refresh();
  }

  private rows(): Row[] {
    return this.view === "prs" ? (this.prs ?? []) : this.issues;
  }

  private async reload(which = this.view) {
    this.pending++;
    this.refresh();
    try {
      if (which === "prs") this.prs = await openPrs();
      else this.issues = await this.jira.issues(this.jql);
      this.details.clear();
      this.say("");
    } catch (e: any) {
      this.say(String(e.message ?? e), "red");
    } finally {
      this.pending--;
      this.refresh();
    }
  }

  private assignees() {
    return [...new Set(this.rows().map((i) => i.assignee))].sort((a, b) => (a === "" ? -1 : b === "" ? 1 : a.localeCompare(b)));
  }

  private visible() {
    const words = this.query.toLowerCase().split(/\s+/).filter(Boolean);
    return this.rows().filter((i) => {
      if (isPr(i) ? (this.prFilter === "requested" && !i.requested) || (this.prFilter === "mine" && !i.mine) : this.mine && i.assignee !== this.me) return false;
      if (this.assigneeFilter !== undefined && i.assignee !== this.assigneeFilter) return false;
      const hay = `${i.key} ${i.status} ${i.assignee || "unassigned"} ${i.summary}`.toLowerCase();
      return words.every((w) => hay.includes(w));
    });
  }

  private scheduleDetail(row: Row) {
    const key = row.key;
    if (this.details.has(key)) return;
    if (this.detailTimer) clearTimeout(this.detailTimer);
    this.detailTimer = setTimeout(() => {
      if (this.details.has(key)) return;
      this.details.set(key, "loading");
      (isPr(row) ? view(row).then((v) => markdown(row, v)) : this.jira.detail(key))
        .then(
          (md) => this.details.set(key, md.split("\n")),
          (e) => this.details.set(key, [fg("red", `Failed to load ${key}: ${e.message ?? e}`)]),
        )
        .finally(this.refresh);
    }, 120);
  }

  private repoChoices() {
    const q = this.form?.repoQuery.toLowerCase() ?? "";
    return this.repos.filter((r) => !q || tilde(r).toLowerCase().includes(q));
  }

  private openForm(issue: Row) {
    const remembered = this.state.repoByProject[issue.key.split("-")[0]!] ?? "";
    this.form = {
      issue,
      field: 0,
      agent: Math.max(0, AGENTS.findIndex((a) => a.label === this.state.agent)),
      worktree: true,
      repo: Math.max(0, this.repos.indexOf(remembered)),
      repoQuery: "",
      context: "",
      busy: false,
    };
  }

  private async submit() {
    const f = this.form!;
    const repo = this.repoChoices()[f.repo];
    const pr = isPr(f.issue) ? f.issue : undefined;
    if ((!repo && !pr) || f.busy) return;
    f.busy = true;
    if (!pr) this.state.repoByProject[f.issue.key.split("-")[0]!] = repo!;
    this.state.agent = AGENTS[f.agent]!.label;
    saveState(this.state);
    const agent = AGENTS[f.agent]!.label;
    const progress = (line: string) => this.say(line, "subtext");
    try {
      const done = pr
        ? await dispatchReview(this.jira, { pr, agent, context: f.context.trim() }, progress)
        : await dispatch(this.jira, { issue: f.issue as Issue, agent, worktree: f.worktree, repo: repo!, context: f.context.trim() } satisfies Dispatch, progress);
      this.state = loadState();
      this.form = undefined;
      this.say(done, "green");
    } catch (e: any) {
      f.busy = false;
      this.say(`${f.issue.key}: ${e.message ?? e}`, "red");
    }
  }

  private header(extra: string): string {
    const dot = fg("surface", " · ");
    const prs = this.view === "prs";
    const who =
      prs && this.prFilter === "requested" ? "review requested" : prs && this.prFilter === "mine" ? "mine" : this.mine && !prs ? "mine" : this.assigneeFilter === undefined ? "everyone" : this.assigneeFilter || "unassigned";
    return (
      bold(fg("mauve", "foreman")) +
      dot +
      (prs ? "open PRs" : this.title) +
      dot +
      fg("yellow", who) +
      dot +
      fg("overlay", extra) +
      (this.loading ? fg("yellow", "  loading…") : "")
    );
  }

  render(width: number): string[] {
    const h = this.tui.terminal.rows;
    const pad = 1;
    const inner = width - pad * 2;
    const body = this.form ? this.renderForm(inner, h - 1) : this.renderBoard(inner, h - 1);
    const lines = body.map((l) => " ".repeat(pad) + fit(l, inner));
    lines.push(" ".repeat(pad) + fit(this.status ? fg(this.statusColor, this.status) : "", inner));
    return lines;
  }

  private renderBoard(width: number, h: number): string[] {
    const items = this.visible();
    this.cursor = Math.max(0, Math.min(this.cursor, items.length - 1));
    const current = items[this.cursor];
    if (current) this.scheduleDetail(current);

    const listRows = Math.max(4, Math.floor((h - 4) * 0.45));
    const detailRows = h - listRows - 4;
    if (this.cursor < this.top) this.top = this.cursor;
    if (this.cursor >= this.top + listRows) this.top = this.cursor - listRows + 1;
    this.top = Math.max(0, Math.min(this.top, Math.max(0, items.length - listRows)));

    const prs = this.view === "prs";
    const out = [this.header(`${items.length}/${this.rows().length}`)];
    out.push(`${fg("mauve", "› ")}${this.query}${fg("mauve", "█")}${this.query ? "" : fg("overlay", ` fuzzy search ${prs ? "PRs" : "tickets"}…`)}`);

    const keyW = Math.min(24, Math.max(7, ...items.map((i) => i.key.length)));
    const statusW = Math.min(14, Math.max(6, ...items.map((i) => i.status.length)));
    const whoW = Math.min(18, Math.max(10, ...items.map((i) => (i.assignee || "unassigned").length)));
    const sumW = width - keyW - statusW - whoW - 8;
    for (let n = this.top; n < this.top + listRows; n++) {
      const i = items[n];
      if (!i) {
        out.push(n === 0 && !this.loading ? fg("overlay", prs ? "  no open PRs" : "  no tickets") : "");
        continue;
      }
      const sel = n === this.cursor;
      const statusColor: Color = i.category === "In Progress" ? "yellow" : i.category === "Done" ? "green" : i.category === "Blocked" ? "red" : "overlay";
      const whoColor: Color = !i.assignee ? "overlay" : i.assignee === this.me || (isPr(i) && i.mine) ? "mauve" : "subtext";
      out.push(
        (sel ? fg("mauve", "▌") : " ") +
          (this.state.dispatched[i.key] ? fg("green", "◆") : " ") +
          [
            fg(sel ? "text" : "overlay", fit(i.key, keyW)),
            fg(statusColor, fit(i.status, statusW)),
            fg(whoColor, fit(i.assignee || "unassigned", whoW)),
            sel ? bold(fg("text", fit(i.summary, sumW))) : fg("subtext", fit(i.summary, sumW)),
          ].join("  "),
      );
    }

    out.push(fg("surface", "─".repeat(width)));
    const raw = current ? this.details.get(current.key) : undefined;
    const body =
      !current ? [] : raw === undefined || raw === "loading" ? [`# ${current.key}: ${current.summary}`, "", fg("overlay", "Loading…")] : raw;
    const wrapped = body.flatMap((l) => (l === "" ? [""] : wrapTextWithAnsi(markdownLine(l), width)));
    this.detailScroll = Math.max(0, Math.min(this.detailScroll, wrapped.length - detailRows));
    for (let n = 0; n < detailRows; n++) out.push(wrapped[this.detailScroll + n] ?? "");

    out.push(
      fg(
        "overlay",
        `↑↓ pick · enter ${prs ? "review" : "dispatch"} · tab ${prs ? "tickets" : "PRs"} · ^o ${prs ? "requested/mine" : "mine"} · ^a ${prs ? "author" : "assignee"} · ^u/^d scroll · ^b browser · ^r refresh · esc quit`,
      ),
    );
    return out;
  }

  private renderForm(width: number, h: number): string[] {
    const f = this.form!;
    const pr = isPr(f.issue);
    const out = [this.header(`${pr ? "review" : "dispatch"} ${f.issue.key}`)];
    out.push(`${fg("mauve", f.issue.key)} ${bold(`"${f.issue.summary}"`)}`, fg("mauve", "─".repeat(width)));
    const label = (n: number, text: string) => (f.field === n ? fg("mauve", `› ${text}`) : `  ${text}`);
    const radio = (on: boolean) => (on ? fg("green", "●") : fg("overlay", "○"));

    out.push(label(0, bold("Agent")));
    AGENTS.forEach((a, n) => out.push(`    ${radio(n === f.agent)} ${fit(n === f.agent ? bold(a.label) : a.label, 16)}${fg("overlay", a.note)}`));
    if (pr) {
      const p = f.issue as Pr;
      out.push("", `  ${bold("Checkout")}`, `    ${fg("subtext", tilde(`${p.checkout}-pr-${p.number}`))} ${fg("overlay", "(worktree at the PR head)")}`);
    } else {
      out.push("", label(1, bold("Options")));
      out.push(`    ${f.worktree ? fg("green", "[x]") : "[ ]"} git worktree ${fg("overlay", "(isolated checkout, parallel-safe)")}`);
      out.push("");
      const choices = this.repoChoices();
      f.repo = Math.max(0, Math.min(f.repo, choices.length - 1));
      out.push(label(2, `${bold("Repo")} ${f.repoQuery ? fg("mauve", `/${f.repoQuery}`) : fg("overlay", "type to filter")}`));
      const rows = 6;
      const start = Math.max(0, Math.min(f.repo - 2, choices.length - rows));
      for (let n = start; n < Math.min(choices.length, start + rows); n++) {
        const on = n === f.repo;
        out.push(`    ${radio(on)} ${on ? bold(tilde(choices[n]!)) : fg("subtext", tilde(choices[n]!))}`);
      }
      if (!choices.length) out.push(fg("red", "    no repo matches"));
    }
    out.push("", label(3, `${bold("Extra context")} ${fg("overlay", "(optional)")}`));
    if (f.context || f.field === 3) for (const l of wrapTextWithAnsi(f.context + (f.field === 3 ? "█" : ""), width - 4)) out.push(`    ${l}`);
    else
      out.push(
        `    ${fg("overlay", pr ? "What to focus on: risky areas, things you already know, questions to answer…" : "Anything the ticket doesn't say: approach, files to look at, constraints…")}`,
      );

    while (out.length < h - 1) out.push("");
    out.length = h - 1;
    const fieldKeys = ["←→ agent", "space toggle", "←→ repo", "type"][f.field];
    out.push(fg("overlay", f.busy ? "dispatching…" : `tab/↑↓ field · ${fieldKeys} · enter ${pr ? "review" : "dispatch"} · esc back`));
    return out;
  }

  handleInput(data: string): void {
    if (this.form) return this.formInput(data);

    const items = this.visible();
    const current = items[this.cursor];
    const move = (to: number) => ((this.cursor = Math.max(0, to)), (this.detailScroll = 0));
    if (matchesKey(data, "escape")) {
      if (this.query) this.query = "";
      else return this.quit();
    } else if (matchesKey(data, "ctrl+c")) return this.quit();
    else if (matchesKey(data, "up") || matchesKey(data, "ctrl+p")) move(this.cursor - 1);
    else if (matchesKey(data, "down") || matchesKey(data, "ctrl+n")) move(this.cursor + 1);
    else if (matchesKey(data, "pageUp")) move(this.cursor - 10);
    else if (matchesKey(data, "pageDown")) move(this.cursor + 10);
    else if (matchesKey(data, "ctrl+u")) this.detailScroll -= 10;
    else if (matchesKey(data, "ctrl+d")) this.detailScroll += 10;
    else if (matchesKey(data, "tab")) {
      this.view = this.view === "prs" ? "tickets" : "prs";
      this.assigneeFilter = undefined;
      this.query = "";
      move(0);
      if (this.view === "prs" && !this.prs) void this.reload();
    } else if (matchesKey(data, "ctrl+o")) {
      if (this.view === "prs") this.prFilter = ({ all: "requested", requested: "mine", mine: "all" } as const)[this.prFilter];
      else this.mine = !this.mine && !!this.me;
      this.assigneeFilter = undefined;
      move(0);
    } else if (matchesKey(data, "ctrl+a")) {
      const list: (string | undefined)[] = [undefined, ...this.assignees()];
      this.assigneeFilter = list[(list.indexOf(this.assigneeFilter) + 1) % list.length];
      this.mine = false;
      move(0);
    } else if (matchesKey(data, "ctrl+b") && current) open(isPr(current) ? current.url : `${SITE}/browse/${current.key}`);
    else if (matchesKey(data, "ctrl+r") && !this.loading) void this.reload();
    else if (matchesKey(data, "enter") && current) this.openForm(current);
    else if (matchesKey(data, "backspace")) (this.query = this.query.slice(0, -1)), move(0);
    else if (typed(data)) (this.query += typed(data)), move(0);
    else return;
    this.refresh();
  }

  private formInput(data: string): void {
    const f = this.form!;
    if (f.busy) return;
    const left = matchesKey(data, "left");
    const right = matchesKey(data, "right");
    if (matchesKey(data, "escape")) this.form = undefined;
    else if (matchesKey(data, "enter")) return void this.submit();
    // A review has no options or repo to pick: its fields are agent (0) and context (3).
    else if (matchesKey(data, "tab") || (matchesKey(data, "down") && f.field !== 2)) f.field = isPr(f.issue) ? 3 - f.field : (f.field + 1) % 4;
    else if (matchesKey(data, "shift+tab") || (matchesKey(data, "up") && f.field !== 2)) f.field = isPr(f.issue) ? 3 - f.field : (f.field + 3) % 4;
    else if (f.field === 0 && (left || right)) f.agent = (f.agent + (left ? AGENTS.length - 1 : 1)) % AGENTS.length;
    else if (f.field === 1 && (data === " " || left || right)) f.worktree = !f.worktree;
    else if (f.field === 2) {
      if (left || matchesKey(data, "up")) f.repo = Math.max(0, f.repo - 1);
      else if (right || matchesKey(data, "down")) f.repo += 1;
      else if (matchesKey(data, "backspace")) (f.repoQuery = f.repoQuery.slice(0, -1)), (f.repo = 0);
      else if (typed(data)) (f.repoQuery += typed(data)), (f.repo = 0);
    } else if (f.field === 3) {
      if (matchesKey(data, "backspace")) f.context = f.context.slice(0, -1);
      else f.context += typed(data);
    }
    this.refresh();
  }

  invalidate() {}
}

async function main() {
  const argv = process.argv.slice(2);
  if (argv[0] === "--help" || argv[0] === "-h") {
    console.log("usage: foreman [PROJECT | JQL]\n       foreman --logout   forget the Atlassian sign-in");
    return;
  }
  if (argv[0] === "--logout") return signOut();

  const { jql, title } = parseArgs(argv);
  const mcp = await connect();
  const jira = new Jira(mcp);
  const me = await jira.me().catch(() => undefined);

  const tui = new TuiAltScreen(new ProcessTerminal());
  const quit = () => {
    tui.stop();
    void mcp.close().finally(() => process.exit(0));
  };
  const board = new Board(tui, jira, jql, title, me, quit);
  tui.addChild(board);
  tui.setFocus(board);
  tui.start();
}

main().catch((e) => {
  console.error(e instanceof Error ? e.message : e);
  process.exit(1);
});
