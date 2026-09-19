/* Slash commands and @path mentions in the composer: the pure parts.

   A command is a message that starts with "/" (the whole first line is the
   command and its argument, so "/model gpt-5.5" reads as one). A mention is
   an "@" at the start of a word; the text from it to the caret is the path
   prefix to complete, shell style. Neither changes what is sent: a local
   command is run only when picked from the list (or sent as exactly
   "/name"), and a mention stays in the text as written for the agent to
   read.

   Besides the local commands, the list offers the agent's own (the chat's
   `commands`, from Claude Code's session: its built-ins and the
   workspace's commands and skills). Those are never run here: picking one
   puts "/name " in the composer and the message is sent as text, which the
   agent expands. A "/name" the agent does not know is sent all the same;
   the agent answers that it is unknown. */

import type { AgentCommand } from "./types";
export type { AgentCommand };

export type Trigger = {
  kind: "command" | "path";
  /* The text to replace when a suggestion is picked. */
  start: number;
  end: number;
  /* What was typed after the "/" or "@", up to the caret. */
  query: string;
};

export type Command = { name: string; label: string; hint: string };

export const COMMANDS: Command[] = [
  { name: "stop", label: "Stop", hint: "Interrupt the agent's turn" },
  { name: "model", label: "Model", hint: "Choose the model for the next turn" },
  {
    name: "mode",
    label: "Permission mode",
    hint: "auto, ask before commands and edits, or plan first",
  },
  {
    name: "thinking",
    label: "Thinking",
    hint: "on (the model decides), off, or a budget in tokens (8k)",
  },
  {
    name: "effort",
    label: "Effort",
    hint: "low, medium, high, xhigh or max; default is the model's",
  },
  { name: "export", label: "Export…", hint: "Download this chat as a file" },
  {
    name: "rewind",
    label: "Rewind…",
    hint: "Go back to before a message: code, conversation or both",
  },
  {
    name: "diff",
    label: "Changes…",
    hint: "What changed in the workspace since this chat began",
  },
  {
    name: "fork",
    label: "Fork…",
    hint: "Copy this chat into a sibling and continue both",
  },
  {
    name: "btw",
    label: "Side question",
    hint: "/btw question — answered from this chat's context, never sent to the agent",
  },
  {
    name: "cost",
    label: "Cost",
    hint: "This chat's turns, tokens and cost so far",
  },
  {
    name: "bug",
    label: "Report a bug",
    hint: "Report a bug to Monaddle — you review it first",
  },
  {
    name: "test",
    label: "Test bug reporting",
    hint: "/test bugreporting — raise a test exception; its report opens for review",
  },
  {
    name: "style",
    label: "Output style",
    hint: "Claude's output style for the next session: default, Explanatory or Learning",
  },
  {
    name: "attach",
    label: "Attach files…",
    hint: "From this computer: /attach PATH… (~ and globs; quote a space), or pick them",
  },
  {
    name: "clear",
    label: "Clear draft",
    hint: "Discard the draft and its attachments",
  },
];

/* Claude's output styles (chats/style.go; the CLI's built-ins). The
   style is a launch setting: it applies when the chat's next session
   starts. */
export type StyleOption = { value: string; label: string; hint: string };
export const STYLES: StyleOption[] = [
  { value: "", label: "Default", hint: "Claude Code's usual answers" },
  {
    value: "Explanatory",
    label: "Explanatory",
    hint: "Adds insights about the choices it makes as it works",
  },
  {
    value: "Learning",
    label: "Learning",
    hint: "Explains and asks you to write small parts yourself",
  },
];

/* A picker row (models.ts): a hint for its title, and disabled with the
   hint saying why when the operator has not allowed the model. */
export type ModelOption = {
  value: string;
  label: string;
  hint?: string;
  disabled?: boolean;
};

/* The permission modes of a Claude chat (chats/permissions.go), in the
   order the surfaces cycle through them; the selector and /mode list the
   same. */
export type ModeOption = { value: string; label: string; hint: string };
export const MODES: ModeOption[] = [
  { value: "auto", label: "Auto", hint: "Every tool call is allowed" },
  {
    value: "ask",
    label: "Ask",
    hint: "Claude asks before commands that write and before file edits",
  },
  {
    value: "plan",
    label: "Plan",
    hint: "Claude explores and proposes a plan; edits wait for its approval",
  },
];

/* The mode after `mode` in the order above (Shift-Tab cycles them, as in
   Claude Code and the TUI); "" is auto. */
export function nextMode(mode: string | undefined): string {
  const i = MODES.findIndex((m) => m.value === (mode || "auto"));
  return MODES[(i + 1) % MODES.length].value;
}

/* The thinking settings of a Claude chat (chats/settings.go): "" is the
   agent's default (the model decides), "off" no thinking, else a budget
   in tokens. The selector offers these; /thinking also takes any budget. */
export type ThinkingOption = { value: string; label: string; hint: string };
export const THINKING: ThinkingOption[] = [
  { value: "", label: "Default", hint: "The model decides how much to think" },
  { value: "off", label: "Off", hint: "No extended thinking" },
  { value: "4000", label: "4k budget", hint: "A budget of 4,000 tokens" },
  { value: "16000", label: "16k budget", hint: "A budget of 16,000 tokens" },
  { value: "32000", label: "32k budget", hint: "A budget of 32,000 tokens" },
];

/* The effort levels of a Claude chat, lowest first. A chat that has not
   chosen one runs the CLI's default, high (models.ts DEFAULT_EFFORT). */
export type EffortOption = { value: string; label: string; hint: string };
export const EFFORTS: EffortOption[] = [
  { value: "low", label: "Low", hint: "Quick, shallow answers" },
  { value: "medium", label: "Medium", hint: "Between low and high" },
  { value: "high", label: "High", hint: "The usual level" },
  { value: "xhigh", label: "Extra high", hint: "More reasoning than high" },
  { value: "max", label: "Max", hint: "As much reasoning as it takes" },
];

/* A /thinking argument as the setting: on/default for the default, off,
   or a budget as digits with an optional k (8k). Undefined when it is
   none of these. */
export function parseThinking(arg: string): string | undefined {
  const a = arg.trim().toLowerCase();
  if (["on", "default", "adaptive", "auto"].includes(a)) return "";
  if (["off", "none", "0"].includes(a)) return "off";
  const m = /^(\d+)(k?)$/.exec(a);
  if (!m) return undefined;
  const n = Number(m[1]) * (m[2] ? 1000 : 1);
  return n > 0 && n <= 128000 ? String(n) : undefined;
}

/* The label a thinking setting shows: "8k" for 8000. */
export function thinkingLabel(setting: string | undefined) {
  if (!setting) return "default";
  if (setting === "off") return "off";
  const n = Number(setting);
  return n >= 1000 && n % 1000 === 0 ? `${n / 1000}k` : String(n);
}

export type CommandItem =
  | { kind: "command"; command: Command }
  | { kind: "model"; model: ModelOption }
  | { kind: "mode"; mode: ModeOption }
  | { kind: "thinking"; thinking: ThinkingOption }
  | { kind: "effort"; effort: EffortOption }
  | { kind: "style"; style: StyleOption }
  | { kind: "agent"; command: AgentCommand };

/* What the agent's built-in commands do, for the list's hint column: the
   CLI names them without a description. A description the agent sends
   wins over these. */
export const AGENT_HINTS: Record<string, string> = {
  compact: "Summarize the conversation to free up context; add a focus",
  init: "Write a CLAUDE.md for this workspace",
  context: "What fills the context window",
  usage: "Session cost and token usage",
  "security-review": "Security review of the pending changes",
  "code-review": "Review the current changes",
  effort: "Set the effort level",
  mcp: "MCP server status",
};

export function agentHint(command: AgentCommand) {
  return command.description || AGENT_HINTS[command.name] || "";
}

const space = (c: string) => c === " " || c === "\t" || c === "\n";

/* The completion the caret asks for, if any. */
export function triggerAt(text: string, caret: number): Trigger | undefined {
  caret = Math.max(0, Math.min(caret, text.length));
  if (text.startsWith("/")) {
    const line = text.indexOf("\n");
    const end = line === -1 ? text.length : line;
    if (caret <= end && caret >= 1)
      return { kind: "command", start: 0, end, query: text.slice(1, caret) };
    return undefined;
  }
  let start = caret;
  while (start > 0 && !space(text[start - 1])) start--;
  if (text[start] !== "@" || caret === start) return undefined;
  let end = caret;
  while (end < text.length && !space(text[end])) end++;
  return { kind: "path", start, end, query: text.slice(start + 1, caret) };
}

/* The first word of a command query, lower-cased, and what follows it. */
function split(query: string): { name: string; rest?: string } {
  const trimmed = query.replace(/^\s+/, "");
  const at = trimmed.search(/\s/);
  if (at === -1) return { name: trimmed.toLowerCase() };
  return { name: trimmed.slice(0, at).toLowerCase(), rest: trimmed.slice(at) };
}

/* The rows for a command query: the local commands whose name starts with
   the word typed, then the agent's (a local name shadows the agent's, so
   "/model" is always the chat's own); or, once "model" has its argument,
   the models whose value or label contains it ("5.5" finds GPT-5.5), and
   once "mode" has its argument, the permission modes it begins (and
   "style" the output styles). "/btw" with its question has no rows: the
   question is asked as typed. An agent command with an argument has no
   rows: the text is sent as it is. */
export function commandItems(
  query: string,
  models: ModelOption[],
  agent: AgentCommand[] = [],
): CommandItem[] {
  const { name, rest } = split(query);
  if (rest === undefined) {
    const local = COMMANDS.filter((c) => c.name.startsWith(name));
    const rows: CommandItem[] = local.map((command) => ({
      kind: "command",
      command,
    }));
    for (const command of agent) {
      const lower = command.name.toLowerCase();
      if (lower.startsWith(name) && !COMMANDS.some((c) => c.name === lower))
        rows.push({ kind: "agent", command });
    }
    return rows;
  }
  const arg = rest.trim().toLowerCase();
  if (name === "mode")
    return MODES.filter((m) => m.value.startsWith(arg)).map((mode) => ({
      kind: "mode",
      mode,
    }));
  if (name === "thinking") return thinkingItems(arg);
  if (name === "effort")
    return EFFORTS.filter((e) => e.value.startsWith(arg)).map((effort) => ({
      kind: "effort",
      effort,
    }));
  if (name === "style")
    return STYLES.filter((s) =>
      (s.value || "default").toLowerCase().startsWith(arg),
    ).map((style) => ({ kind: "style", style }));
  if (name !== "model") return [];
  return models
    .filter(
      (m) =>
        m.value.toLowerCase().includes(arg) ||
        m.label.toLowerCase().includes(arg),
    )
    .map((model) => ({ kind: "model", model }));
}

/* The rows for a /thinking argument: the presets the argument begins
   ("o" lists on and off), or the budget typed as its own row ("8k"). */
function thinkingItems(arg: string): CommandItem[] {
  const rows: CommandItem[] = THINKING.filter((t) =>
    (t.value === "" ? "on" : thinkingLabel(t.value)).startsWith(arg),
  ).map((thinking) => ({ kind: "thinking", thinking }));
  const typed = parseThinking(arg);
  if (
    typed !== undefined &&
    !rows.some((r) => r.kind === "thinking" && r.thinking.value === typed)
  )
    rows.push({
      kind: "thinking",
      thinking: {
        value: typed,
        label:
          typed === ""
            ? "Thinking: default"
            : typed === "off"
              ? "Thinking off"
              : `Thinking ${thinkingLabel(typed)}`,
        hint:
          typed === ""
            ? "The model decides"
            : typed === "off"
              ? "No extended thinking"
              : `A budget of ${Number(typed).toLocaleString()} tokens`,
      },
    });
  return rows;
}

/* The agent command a query names, with or without an argument: "/compact
   focus on the tests" names compact. Undefined when the first word is not
   one of the agent's commands (or is shadowed by a local one). */
export function agentCommandNamed(
  query: string,
  agent: AgentCommand[],
): AgentCommand | undefined {
  const { name } = split(query);
  if (!name || COMMANDS.some((c) => c.name === name)) return undefined;
  return agent.find((c) => c.name.toLowerCase() === name);
}

/* A message that is exactly a local command ("/stop", "/model opus") runs
   it instead of being sent, so a command typed in full and sent with the
   keyboard does not reach the agent as text. The agent's commands are not
   local: "/compact" is sent. */
export function exactCommand(
  text: string,
  models: ModelOption[],
): CommandItem | undefined {
  const trimmed = text.trim();
  if (!trimmed.startsWith("/") || trimmed.includes("\n")) return undefined;
  const items = commandItems(trimmed.slice(1), models);
  const [first, ...rest] = trimmed.slice(1).trim().split(/\s+/);
  if (rest.length === 0) {
    const command = COMMANDS.find((c) => c.name === first.toLowerCase());
    return command ? { kind: "command", command } : undefined;
  }
  const arg = rest.join(" ").toLowerCase();
  const hit = items.find(
    (item) =>
      (item.kind === "model" &&
        (item.model.value.toLowerCase() === arg ||
          item.model.label.toLowerCase() === arg)) ||
      (item.kind === "mode" && item.mode.value === arg) ||
      (item.kind === "thinking" &&
        item.thinking.value === parseThinking(arg)) ||
      (item.kind === "effort" && item.effort.value === arg) ||
      (item.kind === "style" &&
        (item.style.value || "default").toLowerCase() === arg),
  );
  return hit;
}

/* The question a "/btw …" draft asks: the text after the command, on the
   first line and any that follow. Undefined for anything else, a bare
   "/btw" included (that is the command itself, which prompts for one). */
export function sideQuestion(text: string): string | undefined {
  const m = /^\/btw(?:\s+|$)([\s\S]*)$/i.exec(text.trim());
  if (!m) return undefined;
  const question = m[1].trim();
  return question || undefined;
}

/* The text of a "/bug …" draft: a bug report in the person's words, sent
   to Warden's bug route (never to the agent) once they review it. Undefined
   for anything else, a bare "/bug" included. */
export function bugReport(text: string): string | undefined {
  const m = /^\/bug(?:\s+|$)([\s\S]*)$/i.exec(text.trim());
  if (!m) return undefined;
  const report = m[1].trim();
  return report || undefined;
}

/* "/test bugreporting": raise a test exception in the chat service so the
   automatic report can be seen end to end. */
export function isBugTest(text: string): boolean {
  return /^\/test\s+bugreporting$/i.test(text.trim());
}

/* A draft that starts with "!" runs the rest as a shell command in the
   workspace — by the person, not the agent (the service records it as
   their command card, and the agent sees it only if they quote it into a
   message). "#" appends the rest to the workspace's CLAUDE.md. Neither is
   sent as a message. A lone "!" or "#" is text; so is either after a
   space or on a later line. */
export type Prefixed =
  | { kind: "shell"; command: string }
  | { kind: "memory"; note: string };

export function prefixed(text: string): Prefixed | undefined {
  if (text.startsWith("!")) {
    const command = text.slice(1).trim();
    return command ? { kind: "shell", command } : undefined;
  }
  if (text.startsWith("#")) {
    const note = text.slice(1).trim();
    return note ? { kind: "memory", note } : undefined;
  }
  return undefined;
}

/* What a "!" command's card becomes when quoted into a message for the
   agent: the command and its output as a fenced block, with how it
   ended when that was not a clean exit. */
export function quoteCommand(entry: {
  text: string;
  detail: string;
  tool?: { status: string };
}): string {
  const status = entry.tool?.status;
  const ended =
    status && status !== "completed" && status !== "running"
      ? ` (${status})`
      : "";
  const output = entry.detail.replace(/\n$/, "");
  return (
    "I ran `" +
    entry.text +
    "` in the workspace" +
    ended +
    (output ? ":\n```\n" + output + "\n```" : "; it printed nothing.") +
    "\n"
  );
}

/* The text with the trigger's range replaced, and where the caret goes. */
export function replaceTrigger(
  text: string,
  trigger: Trigger,
  insert: string,
): { text: string; caret: number } {
  const next = text.slice(0, trigger.start) + insert + text.slice(trigger.end);
  return { text: next, caret: trigger.start + insert.length };
}

/* What a picked path becomes in the text: a file ends the mention with a
   space, a directory keeps the caret after its slash so the next segment
   can be completed. */
export function mentionFor(path: string) {
  return "@" + path + (path.endsWith("/") ? "" : " ");
}

/* Resource mentions (chats/mentions.go): the "@" menu offers, before the
   workspace paths, the chat's shared documents, repositories and
   previews, and picking one inserts a token — `@doc:"Budget 2026"`,
   `@repo:owner/name`, `@preview:Site` — that the service expands for the
   agent when the message is sent (the transcript keeps the token). The
   kind can be typed to narrow the list (`@doc:bud`). */
export type ResourceKind = "doc" | "repo" | "preview";
export type Resources = {
  documents: { id: string; title: string; kind: string; access: string }[];
  repositories: { name: string; cloneURL: string; access: string[] }[];
  previews: { id: string; title: string; port: number; url: string }[];
};
export const EMPTY_RESOURCES: Resources = {
  documents: [],
  repositories: [],
  previews: [],
};
export type ResourceRow = {
  kind: ResourceKind;
  name: string;
  hint: string;
  /* The token the pick inserts, with its trailing space. */
  insert: string;
};
export const RESOURCE_LIMIT = 12;

/* The token for a resource: the name quoted when it has whitespace (or is
   empty), its own quotes dropped; the same as chats.MentionToken. */
export function mentionToken(kind: ResourceKind, name: string) {
  const clean = name.trim().replace(/"/g, "");
  return /\s/.test(clean) || !clean
    ? `@${kind}:"${clean}"`
    : `@${kind}:${clean}`;
}

/* The resource rows for an "@" query: with a kind typed (`doc:`, `repo:`,
   `preview:`) only that kind, names filtered by the rest; otherwise every
   resource whose kind or name contains the query. Documents, then
   repositories, then previews; at most RESOURCE_LIMIT. */
export function resourceItems(
  resources: Resources | undefined,
  query: string,
): ResourceRow[] {
  if (!resources) return [];
  const q = query.trim().toLowerCase();
  const colon = q.indexOf(":");
  let kind: ResourceKind | "" = "";
  let rest = q;
  if (colon !== -1) {
    const head = q.slice(0, colon);
    if (head === "doc" || head === "repo" || head === "preview") {
      kind = head;
      rest = q.slice(colon + 1);
    }
  }
  const matches = (k: ResourceKind, name: string) =>
    kind
      ? k === kind && name.toLowerCase().includes(rest)
      : k.includes(rest) || name.toLowerCase().includes(rest);
  const rows: ResourceRow[] = [];
  const add = (k: ResourceKind, name: string, hint: string) => {
    if (rows.length < RESOURCE_LIMIT && matches(k, name))
      rows.push({ kind: k, name, hint, insert: mentionToken(k, name) + " " });
  };
  for (const d of resources.documents)
    add(
      "doc",
      d.title || d.id,
      [d.kind, d.access && `${d.access} access`].filter(Boolean).join(" · "),
    );
  for (const r of resources.repositories)
    add("repo", r.name, ["repository", r.access.join(", ")].filter(Boolean).join(" · "));
  for (const p of resources.previews) add("preview", p.title || p.id, p.url);
  return rows;
}

/* Whether an "@" query asks for resources only (a kind prefix), so the
   path lookup can be skipped. */
export function resourceQuery(query: string) {
  return /^(doc|repo|preview):/i.test(query.trim());
}

/* The text once a command has run: the command line is removed and any
   lines after it keep their place. */
export function withoutCommand(text: string, trigger: Trigger) {
  return text.slice(trigger.end).replace(/^\n/, "");
}

/* Files from this computer (docs/web-attach-from-disk-plan.md; the TUI's
   tui/attach.go): "/attach PATH…" and a local mention in the draft name a
   path the chat service reads on the owner's machine (a local install
   only: agentOptions.localFiles). A local mention starts with "./",
   "../" or "~/" — a workspace mention never does — and is attached when
   the message is sent, the mention rewritten to the upload's workspace
   path so the agent reads the file where it landed. */

export function isLocalPath(p: string) {
  return p.startsWith("./") || p.startsWith("../") || p.startsWith("~/");
}

const LOCAL_MENTION = /(^|\s)@((?:\.\/|\.\.\/|~\/)\S+)/g;
const MENTION_TRAIL = /[,.;:!?)\]}'"]+$/;

/* The local mentions in a draft, as written, in order, once each. */
export function localMentions(text: string): string[] {
  const out: string[] = [];
  for (const m of text.matchAll(LOCAL_MENTION)) {
    const p = m[2].replace(MENTION_TRAIL, "");
    if (p && !out.includes(p)) out.push(p);
  }
  return out;
}

/* The draft with each local mention replaced by the workspace paths of
   what it attached ("@~/a.log" → "@.warden/attachments/….log"; a glob
   that matched several becomes several mentions). A mention that
   attached nothing stays. */
export function rewriteMentions(
  text: string,
  attached: { typed: string; path: string }[],
): string {
  for (const typed of localMentions(text)) {
    const paths = attached.filter((a) => a.typed === typed).map((a) => a.path);
    if (!paths.length) continue;
    const token = "@" + typed;
    const insert = paths.map((p) => "@" + p).join(" ");
    text = text.split(token).join(insert);
  }
  return text;
}

/* A word of an "/attach" line with where it sits: bare, or quoted with
   ' or " for a path with spaces (the quotes are not part of the word).
   `end` is past the closing quote. */
type Word = { word: string; start: number; end: number; quote: string };

function words(line: string, from: number): Word[] {
  const out: Word[] = [];
  let i = from;
  while (i < line.length) {
    if (/\s/.test(line[i])) {
      i++;
      continue;
    }
    const start = i;
    let quote = "";
    let word = "";
    while (i < line.length) {
      const c = line[i];
      if (quote) {
        if (c === quote) quote = "";
        else word += c;
      } else if (c === '"' || c === "'") quote = c;
      else if (/\s/.test(c)) break;
      else word += c;
      i++;
    }
    out.push({ word, start, end: i, quote });
  }
  return out;
}

/* The paths an "/attach" line names (the text after the command). Globs
   and ~ are the service's to expand. */
export function attachArgs(rest: string): string[] {
  return words(rest, 0)
    .map((w) => w.word)
    .filter(Boolean);
}

/* The paths of a draft that is an "/attach …" command, or undefined when
   the draft is something else (a bare "/attach" is the command itself,
   which opens the picker; it is undefined here too). One line only, as
   the other exact commands. */
export function attachCommand(text: string): string[] | undefined {
  const trimmed = text.trim();
  const m = /^\/attach\s+([^\n]*)$/i.exec(trimmed);
  if (!m) return undefined;
  const paths = attachArgs(m[1]);
  return paths.length ? paths : undefined;
}

/* The path being typed on an "/attach" line at the caret: the word the
   caret is in (or a new, empty one where the caret sits between words),
   its text up to the caret as the query and the range a pick replaces.
   Undefined while the caret is still on the command name. */
export function attachQuery(
  text: string,
  caret: number,
): { query: string; start: number; end: number } | undefined {
  const line = text.indexOf("\n") === -1 ? text : text.slice(0, text.indexOf("\n"));
  const m = /^\/attach(\s|$)/i.exec(line);
  if (!m || caret > line.length) return undefined;
  const after = "/attach".length;
  if (caret <= after) return undefined;
  for (const w of words(line, after)) {
    if (caret < w.start) break;
    if (caret <= w.end) {
      // The word's text up to the caret, its opening quote dropped.
      let typed = line.slice(w.start, caret);
      if (typed.startsWith('"') || typed.startsWith("'")) typed = typed.slice(1);
      return { query: typed, start: w.start, end: w.end };
    }
  }
  return { query: "", start: caret, end: caret };
}

/* What a picked local path becomes on an "/attach" line: quoted when it
   has a space; a file ends the word with a space, a directory keeps the
   caret after its slash for the next segment. */
export function attachInsert(path: string) {
  const quoted = /\s/.test(path) ? '"' + path + '"' : path;
  return path.endsWith("/") ? (/\s/.test(path) ? '"' + path : path) : quoted + " ";
}
