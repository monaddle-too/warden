/* Permission rules and history (parity round 2 B): the pure parts of the
   workspace panel's Permissions section, the card's "Allow always" scope
   and the chat menu's history list. The service (chats/rules.go) is the
   matcher and the record; this file checks a pattern before it is sent,
   words rules and decisions for people, and orders the history. */
import type { Actor, PermissionEvent, Rule } from "./types";

export const RULE_KINDS: { kind: Rule["kind"]; hint: string }[] = [
  { kind: "allow", hint: "Answered without asking in ask mode" },
  { kind: "deny", hint: "Refused without asking in every mode" },
  { kind: "ask", hint: "A card even in auto mode" },
];

export const RULE_EXAMPLES = [
  "Bash(git *)",
  "Bash(npm test)",
  "Edit(src/**)",
  "Read",
  "WebFetch(domain:example.com)",
  "mcp__warden__*",
];

/* Tools whose rules take a parenthesised argument (a command, a path
   glob, a domain); every other tool is written alone. */
const ARGUMENT_TOOLS = new Set([
  "Bash",
  "Edit",
  "Write",
  "MultiEdit",
  "NotebookEdit",
  "Read",
  "Glob",
  "Grep",
  "LS",
  "WebFetch",
]);

/* Why a pattern cannot be a rule, or "" when it can — the service checks
   again; this saves a round trip and words the same problems. */
export function ruleError(pattern: string): string {
  const p = pattern.trim();
  if (!p) return "Write a pattern, such as Bash(git *) or Edit(src/**).";
  if (p.length > 512 || /[\n\r]/.test(p)) return "One line, up to 512 characters.";
  const open = p.indexOf("(");
  const tool = open < 0 ? p : p.slice(0, open);
  if (!tool) return "Start with the tool's name: Bash, Edit, Read, WebFetch, mcp__server__tool.";
  if (!/^[A-Za-z0-9_*-]+$/.test(tool)) return `“${tool}” is not a tool name.`;
  if (open < 0) return "";
  if (!p.endsWith(")")) return "Missing the closing parenthesis.";
  const spec = p.slice(open + 1, -1).trim();
  if (!spec) return `Empty parentheses; write ${tool} alone.`;
  if (!ARGUMENT_TOOLS.has(tool)) return `${tool} rules take no argument; write ${tool} alone.`;
  if (tool === "WebFetch" && !/^domain:.+/.test(spec))
    return "WebFetch rules take domain:HOST, such as WebFetch(domain:example.com).";
  return "";
}

/* A person's name, email or principal. */
export function actorLabel(by?: Actor | null): string {
  if (!by) return "";
  if (by.name) return by.name;
  if (by.email) return by.email;
  if (by.principalID === "owner") return "the owner";
  return by.principalID;
}

/* Where a rule came from, for the list: the editor or an "Allow always"
   answer (in which chat), and who added it. */
export function ruleOrigin(
  rule: Rule,
  chats: { id: string; title: string }[] = [],
): string {
  let out = rule.origin === "always" ? "Allow always" : "from the editor";
  if (rule.origin === "always" && rule.chatID) {
    const chat = chats.find((c) => c.id === rule.chatID);
    if (chat) out += ` in “${chat.title}”`;
  }
  const by = actorLabel(rule.by);
  if (by) out += ` by ${by}`;
  return out;
}

/* What a kind does, one line, for a title. */
export function kindHint(kind: string): string {
  return RULE_KINDS.find((k) => k.kind === kind)?.hint ?? "";
}

/* Who or what decided an event, for the history list. */
export function decidedBy(ev: PermissionEvent): string {
  switch (ev.how) {
    case "auto":
      return "auto mode";
    case "rule":
      return ev.rule
        ? `${ev.scope ?? "chat"} rule ${ev.rule.kind} ${ev.rule.pattern}`
        : "a rule";
  }
  let by = actorLabel(ev.by) || "a card";
  if (ev.rule) by += `, always for the ${ev.scope ?? "chat"} (${ev.rule.pattern})`;
  if (ev.message) by += `: ${ev.message}`;
  return by;
}

/* The history newest first. */
export function newestFirst(events: PermissionEvent[]): PermissionEvent[] {
  return [...events].sort((a, b) => b.at - a.at);
}

/* How many of each decision, for the dialog's summary line. */
export function historySummary(events: PermissionEvent[]): string {
  if (!events.length) return "No tool asks decided in this chat yet.";
  const allowed = events.filter((e) => e.decision === "allow").length;
  const denied = events.length - allowed;
  const byRule = events.filter((e) => e.how === "rule").length;
  const parts = [`${allowed} allowed`, `${denied} denied`];
  if (byRule) parts.push(`${byRule} by rules`);
  return parts.join(" · ") + (events.length >= 200 ? " (the last 200)" : "");
}
