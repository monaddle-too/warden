// Typed tool cards: what an agent tool call shows in the transcript, by
// its kind (tools.ts decides the words). A command shows its command line
// and its output folded to a dozen lines; a file change its diff
// (DiffView); a read its path, which opens the file; a search its query
// and hit count over the folded hits; a fetch or web search its target; an
// MCP or generic call its input and result. Every value rendered is a text
// node: the agent's output is never interpreted.
import { memo, useMemo, useState } from "react";
import {
  Bot,
  FilePen,
  FileText,
  Globe,
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
  hitCount,
  inputText,
  lineCount,
  shortPath,
  toolFailed,
  toolRunning,
  toolTitle,
} from "../tools";
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
  other: Wrench,
};

/* The summary row of a card: icon, title, what the call amounted to (line
   counts, hits, +/− for a change), and its state. */
export const ToolSummary = memo(function ToolSummary({
  entry,
}: {
  entry: Entry;
}) {
  const tool = entry.tool!;
  const Icon = ICONS[tool.kind] ?? Wrench;
  const running = toolRunning(entry);
  const failed = toolFailed(tool);
  const counts = useMemo(() => {
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
  }, [entry.detail, tool.kind, running, failed]);
  return (
    <>
      <Icon size={13} className="tool-icon" aria-hidden="true" />
      <span className={`tool-title${tool.kind === "command" ? " mono" : ""}`}>
        {toolTitle(entry)}
      </span>
      {counts && <span className="tool-count">{counts}</span>}
      {failed && <span className="tool-failed">{tool.status}</span>}
      {running && <LoaderCircle size={12} className="spin" />}
    </>
  );
});

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
   path link in a message does. */
export const ToolBody = memo(function ToolBody({
  entry,
  onFile,
}: {
  entry: Entry;
  onFile?: (href: string) => void;
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
    tool.kind === "mcp" || tool.kind === "task" || tool.kind === "other"
      ? inputText(tool.input)
      : "";
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
      {tool.kind === "command" && !running && !entry.detail && (
        <p className="tool-meta muted">No output</p>
      )}
    </div>
  );
});
