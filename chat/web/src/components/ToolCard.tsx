// Typed tool cards: what an agent tool call shows in the transcript, by
// its kind (tools.ts decides the words). A command shows its command line
// and its output folded to a dozen lines; a file change its diff
// (DiffView); a read its path, which opens the file; a search its query
// and hit count over the folded hits; a fetch or web search its target; an
// MCP or generic call its input and result; a subagent its prompt, the
// transcript of its own work (nested by EntryView.tsx) and its final
// text; the todo list its items. Every value rendered is a text node: the
// agent's output is never interpreted.
import { memo, useEffect, useMemo, useState, type ReactNode } from "react";
import {
  Bot,
  Circle,
  CircleCheck,
  CircleDot,
  FilePen,
  FileText,
  Globe,
  ListChecks,
  LoaderCircle,
  Plug,
  Search,
  Terminal,
  Wrench,
} from "lucide-react";
import {
  diffCounts,
  editSegments,
  fetchHost,
  foldText,
  formatElapsed,
  hitCount,
  inputText,
  lineCount,
  shortPath,
  subagentInput,
  subagentProgress,
  taskElapsed,
  todoItems,
  todoProgress,
  toolFailed,
  toolRunning,
  toolTitle,
} from "../tools";
import { senderLabel } from "../export";
import type { Entry, ToolKind } from "../types";
import { DiffView } from "./DiffView";

const ICONS: Record<ToolKind, typeof Terminal> = {
  command: Terminal,
  edit: FilePen,
  read: FileText,
  search: Search,
  fetch: Globe,
  webSearch: Globe,
  mcp: Plug,
  task: Bot,
  todo: ListChecks,
  other: Wrench,
};

/* The time, ticking once a second while `active`. */
function useNow(active: boolean): number {
  const [now, setNow] = useState(() => Date.now() / 1000);
  useEffect(() => {
    if (!active) return;
    setNow(Date.now() / 1000);
    const id = setInterval(() => setNow(Date.now() / 1000), 1000);
    return () => clearInterval(id);
  }, [active]);
  return now;
}

/* The summary row of a card: icon, title, what the call amounted to (line
   counts, hits, +/− for a change, a subagent's tool calls and time, a
   todo list's progress), and its state. `steps` are a subagent's own
   entries. */
export const ToolSummary = memo(function ToolSummary({
  entry,
  steps,
}: {
  entry: Entry;
  steps?: Entry[];
}) {
  const tool = entry.tool!;
  const Icon = ICONS[tool.kind] ?? Wrench;
  const running = toolRunning(entry);
  const failed = toolFailed(tool);
  const now = useNow(tool.kind === "task" && running);
  const counts = useMemo(() => {
    if (tool.kind === "task") {
      const parts: string[] = [];
      const { steps: calls, step } = subagentProgress(
        steps ?? [],
        tool.progress,
      );
      if (calls) parts.push(`${calls} tool call${calls === 1 ? "" : "s"}`);
      const elapsed = taskElapsed(entry, now);
      if (elapsed >= 1) parts.push(formatElapsed(elapsed));
      // What the agent says the subagent is doing, while it runs.
      if (running && step) parts.push(step);
      return parts.join(" · ");
    }
    if (tool.kind === "todo") return todoProgress(todoItems(tool));
    if (running || failed || !entry.detail) return "";
    switch (tool.kind) {
      case "edit": {
        const { additions, deletions } = diffCounts(editSegments(entry.detail));
        return `+${additions} −${deletions}`;
      }
      case "search": {
        const { count, unit } = hitCount(entry.detail);
        return `${count} ${unit}`;
      }
      case "read":
        return `${lineCount(entry.detail)} lines`;
      default:
        return "";
    }
  }, [entry, tool, running, failed, steps, now]);
  return (
    <>
      {entry.sender && (
        // A command the person ran themselves ("!cmd"), not the agent's.
        <span className="tool-by" title="Run by this person, not the agent">
          {senderLabel(entry.sender)}
        </span>
      )}
      <Icon size={13} className="tool-icon" aria-hidden="true" />
      <span className={`tool-title${tool.kind === "command" ? " mono" : ""}`}>
        {toolTitle(entry)}
      </span>
      {counts && <span className="tool-count">{counts}</span>}
      {tool.background && (
        <span className="tool-badge" title="Runs in the background">
          background
        </span>
      )}
      {failed && <span className="tool-failed">{tool.status}</span>}
      {running && <LoaderCircle size={12} className="spin" />}
    </>
  );
});

/* The todo list as a checklist: done, in progress (with what the agent
   says it is doing), pending. */
export function TodoList({ entry }: { entry: Entry }) {
  const items = useMemo(() => todoItems(entry.tool), [entry.tool]);
  if (!items.length) return <p className="tool-meta muted">No items</p>;
  return (
    <ul className="todo-list">
      {items.map((item, i) => (
        <li key={i} className={`todo-${item.status}`}>
          {item.status === "completed" ? (
            <CircleCheck size={14} aria-label="Done" />
          ) : item.status === "in_progress" ? (
            <CircleDot size={14} aria-label="In progress" />
          ) : (
            <Circle size={14} aria-label="Pending" />
          )}
          <span className="todo-content">{item.content}</span>
          {item.status === "in_progress" && item.activeForm && (
            <span className="todo-active muted">{item.activeForm}</span>
          )}
        </li>
      ))}
    </ul>
  );
}

/* Output folded to FOLD_LINES with a "+N lines" control; a running
   command shows its last lines, where the output arrives. */
function Folded({
  text,
  running,
  className = "",
}: {
  text: string;
  running?: boolean;
  className?: string;
}) {
  const [open, setOpen] = useState(false);
  const fold = useMemo(
    () => foldText(text, undefined, running),
    [text, running],
  );
  if (!fold.total) return null;
  return (
    <div className={`tool-output ${className}`.trim()}>
      <pre>{open ? text.replace(/\n$/, "") : fold.shown}</pre>
      {fold.hidden > 0 && (
        <button
          type="button"
          className="ghost tool-more"
          aria-expanded={open}
          onClick={() => setOpen((v) => !v)}
        >
          {open ? "Show less" : `+${fold.hidden} lines`}
        </button>
      )}
    </div>
  );
}

/* The body of a card, by kind. `onFile` opens a workspace file the way a
   path link in a message does; `nested` is a subagent's own transcript,
   rendered by the caller between the prompt and the result. */
export const ToolBody = memo(function ToolBody({
  entry,
  onFile,
  nested,
}: {
  entry: Entry;
  onFile?: (href: string) => void;
  nested?: ReactNode;
}) {
  const tool = entry.tool!;
  const running = toolRunning(entry);
  const failed = toolFailed(tool);
  const segments = useMemo(
    () => (tool.kind === "edit" ? editSegments(entry.detail) : []),
    [tool.kind, entry.detail],
  );
  const paths = tool.paths ?? [];
  const input =
    tool.kind === "mcp" || tool.kind === "other" ? inputText(tool.input) : "";
  if (tool.kind === "todo")
    return (
      <div className="tool-body tool-todo">
        <TodoList entry={entry} />
      </div>
    );
  if (tool.kind === "task") {
    const { prompt } = subagentInput(tool);
    return (
      <div className="tool-body tool-task">
        {prompt && <Folded text={prompt} className="tool-prompt" />}
        {nested}
        {entry.detail && (
          <div className="tool-result">
            <p className="tool-meta muted">{failed ? "Error" : "Result"}</p>
            <Folded text={entry.detail} />
          </div>
        )}
        {!entry.detail && !running && !nested && (
          <p className="tool-meta muted">No result</p>
        )}
      </div>
    );
  }
  return (
    <div className={`tool-body tool-${tool.kind}`}>
      {tool.description && (
        <p className="tool-description">{tool.description}</p>
      )}
      {tool.kind === "read" && paths[0] && (
        <p className="tool-meta">
          {onFile ? (
            <a
              href={paths[0]}
              onClick={(event) => {
                event.preventDefault();
                onFile(paths[0]);
              }}
            >
              {shortPath(paths[0])}
            </a>
          ) : (
            <span>{shortPath(paths[0])}</span>
          )}
        </p>
      )}
      {(tool.kind === "search" || tool.kind === "webSearch") && tool.query && (
        <p className="tool-meta">
          <code>{tool.query}</code>
          {tool.kind === "search" && paths[0] && (
            <span className="muted"> in {shortPath(paths[0])}</span>
          )}
        </p>
      )}
      {tool.kind === "fetch" && tool.query && (
        <p className="tool-meta">
          <span className="muted">{fetchHost(tool.query)}</span>{" "}
          <code>{tool.query}</code>
        </p>
      )}
      {tool.kind === "mcp" && (
        <p className="tool-meta">
          <span className="muted">{tool.server || "mcp"}</span> ·{" "}
          <code>{tool.name}</code>
        </p>
      )}
      {input && <Folded text={input} className="tool-input" />}
      {tool.kind === "edit" && !failed && segments.length > 0 && (
        <div className="activity-detail">
          <DiffView segments={segments} />
        </div>
      )}
      {(tool.kind !== "edit" || failed) && (
        <Folded
          text={entry.detail}
          running={running && tool.kind === "command"}
        />
      )}
      {tool.kind === "command" && running && tool.background && (
        <p className="tool-meta muted">
          Running in the background; the output arrives when the agent reads it.
        </p>
      )}
      {tool.kind === "command" && !running && !entry.detail && (
        <p className="tool-meta muted">No output</p>
      )}
    </div>
  );
});
