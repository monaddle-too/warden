/* What the agent is doing right now, in a few words for the chat's status
   line and the sidebar dot: read off the newest entry still running. A
   command says what it runs, a file change which file, a read which file,
   a search what for, a fetch where, a subagent how far it is; the model's
   thinking and a compaction say so. Nothing here looks at the clock or the
   DOM. The terminal client derives the same words in Go
   (internal/tui/activity.go); the two share the cases in
   internal/tui/testdata/activity.json, so they cannot drift apart. */
import type { Entry } from "./types";

/* The fields the derivation reads; the TUI's Entry has the same. */
export type ActivityEntry = Pick<
  Entry,
  "id" | "role" | "text" | "isStreaming" | "tool" | "parentID" | "sender"
>;

export const THINKING = "Agent is thinking";
export const COMPACTING = "Compacting context";
export const WORKING = "Agent is working";

/* A command as the status shows it: its first line, whitespace collapsed,
   cut to `limit` characters. */
export function trimCommand(command: string, limit = 48): string {
  const line = command.trim().split("\n")[0].replace(/\s+/g, " ");
  return line.length > limit ? line.slice(0, limit - 1) + "…" : line;
}

/* The last segment of a path, so a status says "engine.go" rather than
   the whole workspace path. */
export function basename(path: string): string {
  const clean = path.replace(/\/+$/, "");
  const at = clean.lastIndexOf("/");
  return at >= 0 ? clean.slice(at + 1) : clean;
}

/* The host of a URL, for a fetch; the URL itself when it does not parse. */
function host(url: string): string {
  const m = /^[a-z][a-z0-9+.-]*:\/\/([^/?#]+)/i.exec(url);
  return m ? m[1] : url;
}

const running = (e: ActivityEntry) =>
  e.isStreaming || e.tool?.status === "running";

/* The number of tool calls a subagent's entries record. */
function childCalls(parent: string, entries: ActivityEntry[]): number {
  let calls = 0;
  for (const e of entries) if (e.parentID === parent && e.tool) calls++;
  return calls;
}

/* "Explore agent: 3 tool calls" — the subagent's type and how far it is,
   from its entries or the agent's own count, whichever is further. */
export function subagentLabel(
  card: ActivityEntry,
  entries: ActivityEntry[],
): string {
  const type = card.tool?.input?.subagent_type;
  const label = typeof type === "string" && type ? `${type} agent` : "Subagent";
  const calls = Math.max(
    childCalls(card.id, entries),
    card.tool?.progress?.toolCalls ?? 0,
  );
  if (!calls) return `${label}: starting`;
  return `${label}: ${calls} tool call${calls === 1 ? "" : "s"}`;
}

/* The words for one running step, by its tool's kind. */
export function stepLabel(e: ActivityEntry, entries: ActivityEntry[]): string {
  const tool = e.tool;
  if (!tool) return e.text || "";
  const paths = tool.paths ?? [];
  const file = paths[0] ? basename(paths[0]) : "";
  switch (tool.kind) {
    case "command": {
      const command = trimCommand(e.text);
      return command ? `Running ${command}` : "Running a command";
    }
    case "edit": {
      const verb = tool.name === "Write" ? "Writing" : "Editing";
      if (paths.length > 1) return `${verb} ${paths.length} files`;
      return file ? `${verb} ${file}` : `${verb} a file`;
    }
    case "read":
      return file ? `Reading ${file}` : "Reading a file";
    case "search":
      if (tool.name === "LS") return file ? `Listing ${file}` : "Listing files";
      return tool.query
        ? `Searching for ${trimCommand(tool.query, 32)}`
        : "Searching files";
    case "webSearch":
      return tool.query
        ? `Searching the web for ${trimCommand(tool.query, 32)}`
        : "Searching the web";
    case "fetch":
      return tool.query ? `Fetching ${host(tool.query)}` : "Fetching a page";
    case "mcp":
      return tool.name ? `Calling ${tool.name}` : "Calling a tool";
    case "task":
      return subagentLabel(e, entries);
    case "todo":
      return "Updating the todo list";
    default:
      return trimCommand(e.text, 60) || (tool.name ? `Using ${tool.name}` : "");
  }
}

/* The card of the subagent `e` works for, when that subagent still runs:
   the outermost one when subagents nest, so the status names what the
   agent itself is waiting on. */
function subagentOf(
  e: ActivityEntry,
  entries: ActivityEntry[],
): ActivityEntry | undefined {
  let top: ActivityEntry | undefined;
  const seen = new Set<string>();
  for (let id = e.parentID; id && !seen.has(id); ) {
    seen.add(id);
    const parent = entries.find((p) => p.id === id);
    if (!parent || parent.tool?.kind !== "task" || !running(parent)) break;
    top = parent;
    id = parent.parentID;
  }
  return top;
}

/* What the agent is doing, from the newest running entry; "" when no
   entry says (a reply streaming, a turn between steps), which the status
   shows as `WORKING`. A command the person ran (`!cmd`) and a command
   running in the background are not what the agent is doing now. */
export function activityLabel(entries: ActivityEntry[]): string {
  for (let i = entries.length - 1; i >= 0; i--) {
    const e = entries[i];
    if (!running(e)) continue;
    if (e.role === "thinking") return THINKING;
    if (e.role === "compaction") return COMPACTING;
    if (e.role !== "activity") continue;
    if (e.sender) continue;
    if (e.tool?.background && e.tool.kind !== "task") continue;
    const card = subagentOf(e, entries);
    if (card) return subagentLabel(card, entries);
    const label = stepLabel(e, entries);
    if (label) return label;
  }
  return "";
}

/* The status line's words while the agent's turn runs. */
export function runningLabel(entries: ActivityEntry[]): string {
  return activityLabel(entries) || WORKING;
}

