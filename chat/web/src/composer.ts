/* Slash commands and @path mentions in the composer: the pure parts.

   A command is a message that starts with "/" (the whole first line is the
   command and its argument, so "/model gpt-5.5" reads as one). A mention is
   an "@" at the start of a word; the text from it to the caret is the path
   prefix to complete, shell style. Neither changes what is sent: a command
   is run only when picked from the list (or sent as exactly "/name"), and
   a mention stays in the text as written for the agent to read. */

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
  { name: "export", label: "Export…", hint: "Download this chat as a file" },
  {
    name: "clear",
    label: "Clear draft",
    hint: "Discard the draft and its attachments",
  },
];

export type ModelOption = { value: string; label: string };

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

export type CommandItem =
  | { kind: "command"; command: Command }
  | { kind: "model"; model: ModelOption }
  | { kind: "mode"; mode: ModeOption };

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

/* The rows for a command query: the commands whose name starts with the
   word typed, or, once "model" has its argument, the models whose value
   or label contains it ("5.5" finds GPT-5.5); once "mode" has its
   argument, the permission modes it begins. */
export function commandItems(
  query: string,
  models: ModelOption[],
): CommandItem[] {
  const trimmed = query.replace(/^\s+/, "");
  const at = trimmed.search(/\s/);
  const name = (at === -1 ? trimmed : trimmed.slice(0, at)).toLowerCase();
  if (at === -1)
    return COMMANDS.filter((c) => c.name.startsWith(name)).map((command) => ({
      kind: "command",
      command,
    }));
  const arg = trimmed.slice(at).trim().toLowerCase();
  if (name === "mode")
    return MODES.filter((m) => m.value.startsWith(arg)).map((mode) => ({
      kind: "mode",
      mode,
    }));
  if (name !== "model") return [];
  return models
    .filter(
      (m) =>
        m.value.toLowerCase().includes(arg) ||
        m.label.toLowerCase().includes(arg),
    )
    .map((model) => ({ kind: "model", model }));
}

/* A message that is exactly a command ("/stop", "/model opus") runs it
   instead of being sent, so a command typed in full and sent with the
   keyboard does not reach the agent as text. */
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
      (item.kind === "mode" && item.mode.value === arg),
  );
  return hit;
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

/* The text once a command has run: the command line is removed and any
   lines after it keep their place. */
export function withoutCommand(text: string, trigger: Trigger) {
  return text.slice(trigger.end).replace(/^\n/, "");
}
