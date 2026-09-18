/* Copy and export: the transcript as the markdown the agent and the owner
   wrote, or as JSON for another tool. Everything here is string work on
   `chat.conversation`; nothing is fetched and nothing is rendered, so an
   agent's text goes into the file exactly as it was written. */
import { compactionText } from "./context";
import { formatSize } from "./attachments";
import { nestEntries } from "./transcript";
import { footerText, turnFooters } from "./turns";
import type { Chat, Entry } from "./types";

export type ExportFormat = "markdown" | "json";
export type ExportOptions = {
  format: ExportFormat;
  /* Tool steps are noise in a transcript read by a person, so they are
     left out unless asked for. */
  activity: boolean;
};

export const providerName = (provider?: string) =>
  provider === "claude" ? "Claude" : "Codex";

/* "1 message", "3 messages". */
export const plural = (n: number, one: string, many = one + "s") =>
  `${n} ${n === 1 ? one : many}`;

// Who wrote a user entry: their Google name, else their email, else the
// owner ("You" on a local install, where the owner is the only person).
export function senderLabel(sender?: Entry["sender"]) {
  if (!sender) return "You";
  if (sender.name) return sender.name;
  if (sender.email) return sender.email;
  return sender.principalID === "owner" ? "You" : "Collaborator";
}

export function authorLabel(entry: Entry, provider?: string) {
  return entry.role === "user"
    ? senderLabel(entry.sender)
    : providerName(provider);
}

const two = (n: number) => String(n).padStart(2, "0");
/* Local time to the minute, the same for every reader of the file. */
export function localTime(seconds: number) {
  const at = new Date(seconds * 1000);
  return `${at.getFullYear()}-${two(at.getMonth() + 1)}-${two(at.getDate())} ${two(at.getHours())}:${two(at.getMinutes())}`;
}

/* A fence the text cannot close: one backtick longer than its longest run. */
export function fenceFor(text: string) {
  let longest = 0;
  for (const run of text.matchAll(/`{3,}/g))
    longest = Math.max(longest, run[0].length);
  return "`".repeat(Math.max(3, longest + 1));
}

/* An entry with, under it, the entries a subagent produced for it (the
   transcript's nesting, `nestEntries`); a subagent's own subagent nests
   the same way. */
export type ExportNode = { entry: Entry; children: ExportNode[] };

/* Tool steps and the model's thinking are the agent's working, kept out
   unless asked for; a command the person ran themselves (`!cmd`) is
   theirs, not the agent's, and stays. */
export const exportable = (e: Entry, activity: boolean) =>
  activity ||
  (e.role !== "activity" && e.role !== "thinking") ||
  (e.role === "activity" && !!e.sender);

/* What goes into the file, as the transcript nests it: the conversation's
   own entries, a subagent's under its Agent card. A card left out takes
   its subagent's work with it. */
export function exportTree(entries: Entry[], activity: boolean): ExportNode[] {
  const { top, nested } = nestEntries(entries);
  const build = (list: Entry[]): ExportNode[] =>
    list
      .filter((e) => exportable(e, activity))
      .map((entry) => ({
        entry,
        children: build(nested.get(entry.id) ?? []),
      }));
  return build(top);
}

/* The same entries flat, each subagent's after its card. */
export function exportEntries(entries: Entry[], activity: boolean): Entry[] {
  const out: Entry[] = [];
  const walk = (nodes: ExportNode[]) => {
    for (const n of nodes) {
      out.push(n.entry);
      walk(n.children);
    }
  };
  walk(exportTree(entries, activity));
  return out;
}

/* One entry as markdown; a message keeps its text verbatim (it is markdown
   already), everything else is described. `inner` is a subagent's work,
   set under its card's title before the card's result. */
function entryMarkdown(
  entry: Entry,
  provider: string | undefined,
  time: (s: number) => string,
  inner: string[] = [],
) {
  const lines: string[] = [];
  switch (entry.role) {
    case "user":
    case "assistant": {
      lines.push(
        `## ${authorLabel(entry, provider)} — ${time(entry.createdAt)}`,
        "",
      );
      if (entry.delivery === "failed")
        lines.push(
          `_Not delivered${entry.detail ? `: ${entry.detail}` : ""}_`,
          "",
        );
      else if (entry.delivery === "queued") lines.push("_Queued_", "");
      if (entry.text) lines.push(entry.text, "");
      if (entry.attachments?.length)
        lines.push(
          "Attachments: " +
            entry.attachments
              .map(
                (a) => `\`${a.name}\` (${formatSize(a.size)}, \`${a.path}\`)`,
              )
              .join(", "),
          "",
        );
      break;
    }
    case "activity": {
      // A command the person ran is theirs; the agent's steps are its.
      lines.push(
        entry.sender
          ? `### Command by ${senderLabel(entry.sender)} — ${entry.text}`
          : `### Activity — ${entry.text || "Agent activity"}`,
        "",
      );
      lines.push(...inner);
      if (entry.detail) {
        const fence = fenceFor(entry.detail);
        lines.push(fence, entry.detail.replace(/\n$/, ""), fence, "");
      }
      break;
    }
    case "thinking":
      lines.push("### Thinking", "");
      if (entry.text)
        lines.push(`> ${entry.text.split("\n").join("\n> ")}`, "");
      break;
    case "image":
      lines.push(`_Image${entry.text ? `: ${entry.text}` : ""}_`, "");
      break;
    case "compaction":
      lines.push(
        `### ${compactionText(entry.compaction ?? { status: "completed" })}`,
        "",
      );
      if (entry.detail)
        lines.push(`> ${entry.detail.split("\n").join("\n> ")}`, "");
      break;
    case "aside": {
      // A side question and its answer, never part of the conversation.
      lines.push(
        `### Side question — ${senderLabel(entry.sender)}, ${time(entry.createdAt)}`,
        "",
        entry.text,
        "",
      );
      const a = entry.aside;
      if (a?.status === "failed")
        lines.push(`_Could not answer${a.error ? `: ${a.error}` : ""}_`, "");
      else if (entry.detail)
        lines.push(`> ${entry.detail.split("\n").join("\n> ")}`, "");
      break;
    }
    default:
      lines.push(`> ${entry.text.split("\n").join("\n> ")}`, "");
  }
  return lines;
}

export function exportMarkdown(
  chat: Chat,
  options: ExportOptions,
  at: Date,
  time: (seconds: number) => string = localTime,
) {
  const head = [`# ${chat.title || "Chat"}`, ""];
  const facts = [
    `Agent: ${providerName(chat.provider)}${chat.model ? ` (${chat.model})` : ""}`,
  ];
  if (chat.repository) facts.push(`Repository: ${chat.repository}`);
  facts.push(`Exported: ${time(at.getTime() / 1000)}`);
  head.push(...facts.map((f) => `- ${f}`), "", "---", "");
  const tree = exportTree(chat.conversation.entries, options.activity);
  // What each finished turn took, under its last entry of the
  // conversation's own; a turn still running when the file is written
  // has no line.
  const footers = turnFooters(
    nestEntries(chat.conversation.entries).top,
    chat.conversation.turns,
    false,
  );
  // A subagent's work goes under its card as a quotation, so its own
  // headings and fences stay inside the card; a nested subagent's is
  // quoted twice.
  const nodeMarkdown = (node: ExportNode): string[] => {
    const inner = node.children
      .flatMap(nodeMarkdown)
      .flatMap((block) => block.split("\n"))
      .map((line) => (line ? `> ${line}` : ">"));
    if (inner.length) inner.push("");
    return entryMarkdown(node.entry, chat.provider, time, inner);
  };
  const body = tree.flatMap((node) => {
    const lines = nodeMarkdown(node);
    const footer = footers.get(node.entry.id);
    const took = footer ? footerText(footer) : "";
    if (took) lines.push(`_Turn: ${took}_`, "");
    return lines;
  });
  return [...head, ...body].join("\n").replace(/\n+$/, "\n");
}

/* An entry's record with a subagent's entries under it as `children`. */
function nodeJSON(node: ExportNode): Entry & { children?: unknown[] } {
  return node.children.length
    ? { ...node.entry, children: node.children.map(nodeJSON) }
    : node.entry;
}

/* The chat's own records, as the service sent them, under a header that
   names the format so a reader can tell the file apart; a subagent's
   entries nest under their card as `children`. */
export function exportJSON(chat: Chat, options: ExportOptions, at: Date) {
  return JSON.stringify(
    {
      format: "warden-chat",
      version: 1,
      exportedAt: at.toISOString(),
      chat: {
        id: chat.id,
        title: chat.title,
        provider: chat.provider,
        model: chat.model,
        repository: chat.repository,
        status: chat.status,
        archived: chat.archived,
        threadID: chat.conversation.threadID,
      },
      entries: exportTree(chat.conversation.entries, options.activity).map(
        nodeJSON,
      ),
      turns: chat.conversation.turns ?? [],
    },
    null,
    2,
  );
}

export function exportText(chat: Chat, options: ExportOptions, at: Date) {
  return options.format === "json"
    ? exportJSON(chat, options, at)
    : exportMarkdown(chat, options, at);
}

export const exportMime = (format: ExportFormat) =>
  format === "json" ? "application/json" : "text/markdown";

/* A file name from the title: ASCII letters, digits, dots and dashes only,
   so it lands the same on every file system, with the time so two exports
   of one chat do not collide. */
export function exportName(title: string, format: ExportFormat, at: Date) {
  const slug = title
    .toLowerCase()
    .replace(/[^a-z0-9.]+/g, "-")
    .replace(/^[-.]+|[-.]+$/g, "")
    .slice(0, 60)
    .replace(/[-.]+$/, "");
  const stamp = `${at.getFullYear()}${two(at.getMonth() + 1)}${two(at.getDate())}-${two(at.getHours())}${two(at.getMinutes())}`;
  return `${slug || "chat"}-${stamp}.${format === "json" ? "json" : "md"}`;
}

/* Hands a blob to the browser as a download under `name`. */
export function saveFile(name: string, blob: Blob) {
  const url = URL.createObjectURL(blob);
  const link = document.createElement("a");
  link.href = url;
  link.download = name;
  link.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}
