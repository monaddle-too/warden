// Pure logic behind the typed tool cards (ToolCard.tsx): what a card of
// each kind says in its summary row, how a long output folds, and how a
// file change's detail splits into the diffs DiffView renders. Everything
// here is string work over what the service recorded; nothing is
// interpreted, so the cards only ever render text nodes.
import { hasDiff, looseDiff, parseDiff, type DiffSegment } from "./diff";
import type { Entry, Progress, ToolRead, Tool, ToolKind } from "./types";

/* Lines of output a card shows before its "+N lines" control. */
export const FOLD_LINES = 12;

/* The guest's working directory; a path under it reads relative to it. */
export const WORKSPACE = "/home/agent/workspace";

/* The kind a card renders by; entries recorded before the service typed
   tool calls have none and keep the generic rendering. */
export function toolKind(entry: Entry): ToolKind | undefined {
  return entry.tool?.kind;
}

/* Whether the call is still running: the entry streams or the service
   has no result yet. */
export function toolRunning(entry: Entry): boolean {
  return entry.isStreaming || entry.tool?.status === "running";
}

/* Whether the call failed (the tool's own error) or was declined. */
export function toolFailed(tool: Tool | undefined): boolean {
  return !!tool && tool.status !== "running" && tool.status !== "completed";
}

/* A workspace path as the card shows it: relative to the workspace when
   inside it, as given otherwise. */
export function shortPath(path: string): string {
  if (path === WORKSPACE) return ".";
  return path.startsWith(WORKSPACE + "/")
    ? path.slice(WORKSPACE.length + 1)
    : path;
}

/* Lines as the reader counts them: no trailing empty line. */
export function splitLines(text: string): string[] {
  if (!text) return [];
  const body = text.endsWith("\n") ? text.slice(0, -1) : text;
  return body.split("\n");
}

export type Fold = {
  /* The text shown when folded. */
  shown: string;
  /* Lines the fold hides; 0 when the text fits. */
  hidden: number;
  /* All lines of the text. */
  total: number;
};

/* Folds text to its first `limit` lines (the head is where a command's
   output starts and a search's first hits are); `fromEnd` keeps the last
   lines instead, for output still streaming. */
export function foldText(
  text: string,
  limit = FOLD_LINES,
  fromEnd = false,
): Fold {
  const lines = splitLines(text);
  if (lines.length <= limit)
    return { shown: lines.join("\n"), hidden: 0, total: lines.length };
  const kept = fromEnd
    ? lines.slice(lines.length - limit)
    : lines.slice(0, limit);
  return {
    shown: kept.join("\n"),
    hidden: lines.length - limit,
    total: lines.length,
  };
}

/* What a search returned, for its summary: the count a search tool
   reports ("Found 3 files"), or its non-empty lines; "No files found" is
   zero. */
export function hitCount(output: string): { count: number; unit: string } {
  const lines = splitLines(output);
  const first = lines[0] ?? "";
  if (/^No (files|matches) found/.test(first))
    return { count: 0, unit: "results" };
  const found = /^Found (\d+) (\w+)/.exec(first);
  if (found) return { count: Number(found[1]), unit: found[2] };
  return {
    count: lines.filter((l) => l.trim() !== "").length,
    unit: "results",
  };
}

/* What a read returned: its lines. */
export function lineCount(output: string): number {
  return splitLines(output).length;
}

/* The diffs a file change's detail holds, one per file. The service writes
   each change as its path on a line, then a unified diff with a git
   header; a hunk whose place in the file the agent did not know is
   written without an `@@` header, and shows coloured by prefix without
   line numbers. */
export function editSegments(detail: string): DiffSegment[] {
  const segments: DiffSegment[] = [];
  const lines = splitLines(detail);
  let start = -1;
  const flush = (end: number) => {
    if (start < 0) return;
    const block = lines.slice(start, end);
    const parsed = parseDiff(block.join("\n"));
    if (hasDiff(parsed)) {
      for (const s of parsed) if (s.kind === "diff") segments.push(s);
      return;
    }
    // Headerless hunks: the path from the `+++` line, the body coloured
    // by prefix.
    let path = "";
    let status = "";
    let body = 1;
    for (; body < block.length; body++) {
      const l = block[body];
      if (l.startsWith("+++ ")) {
        path = l.slice(4).replace(/^b\//, "");
        body++;
        break;
      }
      if (l.startsWith("new file mode")) status = "new file";
      if (
        !/^(--- |index |new file mode|deleted file mode|old mode|new mode)/.test(
          l,
        )
      )
        break;
    }
    const file = looseDiff(block.slice(body).join("\n"));
    file.path = path;
    file.status = status;
    if (file.hunks[0].lines.length) segments.push({ kind: "diff", file });
  };
  lines.forEach((line, i) => {
    if (line.startsWith("diff --git ")) {
      flush(i);
      start = i;
    }
  });
  flush(lines.length);
  return segments;
}

/* The added and removed line counts over a change's diffs. */
export function diffCounts(segments: DiffSegment[]): {
  additions: number;
  deletions: number;
} {
  let additions = 0,
    deletions = 0;
  for (const s of segments)
    if (s.kind === "diff") {
      additions += s.file.additions;
      deletions += s.file.deletions;
    }
  return { additions, deletions };
}

/* The card's title: the entry's text, or the tool's name. */
export function toolTitle(entry: Entry): string {
  return entry.text || entry.tool?.name || "Agent activity";
}

/* What a subagent's card says about its work: how many tool calls its
   entries record (or the agent's own count when it runs ahead), whether
   any still runs, and what the agent says the subagent is doing now (its
   own words, else the tool it used last). */
export function subagentProgress(
  children: Entry[],
  progress?: Progress,
): {
  steps: number;
  running: boolean;
  step: string;
} {
  let steps = 0;
  let running = false;
  for (const e of children) {
    if (e.tool) steps++;
    if (e.isStreaming || e.tool?.status === "running") running = true;
  }
  if (progress && progress.toolCalls > steps) steps = progress.toolCalls;
  const step =
    progress?.activity ||
    (progress?.lastTool ? `using ${progress.lastTool}` : "");
  return { steps, running, step };
}

/* How long a subagent (a task entry) has been at work, in seconds: from
   its start to its end, or to `now` while it runs. */
export function taskElapsed(entry: Entry, now: number): number {
  const end = entry.endedAt || (toolRunning(entry) ? now : 0);
  return end > entry.createdAt ? end - entry.createdAt : 0;
}

/* Seconds as the card shows them: "4s", "1m 12s", "2h 5m". */
export function formatElapsed(seconds: number): string {
  const s = Math.max(0, Math.round(seconds));
  if (s < 60) return `${s}s`;
  const m = Math.floor(s / 60);
  if (m < 60) return `${m}m ${s % 60}s`;
  return `${Math.floor(m / 60)}h ${m % 60}m`;
}

/* The subagent's type and prompt, from the Agent call's input. */
export function subagentInput(tool: Tool | undefined): {
  type: string;
  prompt: string;
} {
  const input = tool?.input ?? {};
  const type =
    typeof input.subagent_type === "string" ? input.subagent_type : "";
  const prompt = typeof input.prompt === "string" ? input.prompt : "";
  return { type, prompt };
}

export type TodoStatus = "pending" | "in_progress" | "completed";
export type TodoItem = {
  content: string;
  status: TodoStatus;
  /* What the agent shows while the item is in progress ("Writing tests"). */
  activeForm: string;
};

/* The items of a todo card, from the list the service recorded in the
   card's input (TodoWrite's shape). Anything malformed reads as pending. */
export function todoItems(tool: Tool | undefined): TodoItem[] {
  const raw = tool?.input?.todos;
  if (!Array.isArray(raw)) return [];
  const items: TodoItem[] = [];
  for (const v of raw) {
    if (!v || typeof v !== "object") continue;
    const o = v as Record<string, unknown>;
    const content = typeof o.content === "string" ? o.content : "";
    if (!content) continue;
    const status =
      o.status === "completed" || o.status === "in_progress"
        ? o.status
        : "pending";
    items.push({
      content,
      status,
      activeForm: typeof o.activeForm === "string" ? o.activeForm : "",
    });
  }
  return items;
}

/* The todo card's progress, for its summary: "2 of 5 done". */
export function todoProgress(items: TodoItem[]): string {
  const done = items.filter((i) => i.status === "completed").length;
  return `${done} of ${items.length} done`;
}

/* The host of a fetched URL, for the summary of a fetch card; the URL
   itself when it does not parse. */
export function fetchHost(url: string): string {
  try {
    return new URL(url).host;
  } catch {
    return url;
  }
}

/* A tool's input as the generic card shows it: JSON, indented, with the
   fields in a stable order. */
export function inputText(input: Record<string, unknown> | undefined): string {
  if (!input || !Object.keys(input).length) return "";
  const ordered: Record<string, unknown> = {};
  for (const k of Object.keys(input).sort()) ordered[k] = input[k];
  return JSON.stringify(ordered, null, 2);
}

/* The count a read of something other than text shows in its header: an
   image's pixels, a PDF's pages, a notebook's cells (tools.ts is where
   the header's words live). */
export function readCount(read: ToolRead): string {
  switch (read.kind) {
    case "image":
      return read.width && read.height
        ? `${read.width}×${read.height}`
        : "image";
    case "pdf":
      return read.pages ? `${read.pages} page${read.pages === 1 ? "" : "s"}` : "PDF";
    case "notebook": {
      const n = read.cells?.length ?? 0;
      return `${n} cell${n === 1 ? "" : "s"}`;
    }
  }
}
