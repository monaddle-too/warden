/* Tool permission asks (a Claude chat in ask or plan mode): the pure parts
   of the cards. The service records the ask (chats/permissions.go) as an
   approval with method item/tool/requestPermission whose params carry the
   tool, its input, the call as a transcript entry and, for ExitPlanMode,
   the plan. */
import type { Approval, PermissionParams } from "./types";

export const PERMISSION_METHOD = "item/tool/requestPermission";

/* The ask's params when the approval is one, else undefined. */
export function permissionParams(
  approval: Approval,
): PermissionParams | undefined {
  if (approval.method !== PERMISSION_METHOD) return undefined;
  const p = approval.params as Record<string, unknown>;
  if (typeof p.tool !== "string") return undefined;
  return p as unknown as PermissionParams;
}

/* Whether the ask is the model's plan (ExitPlanMode). */
export function isPlan(params: PermissionParams): boolean {
  return params.tool === "ExitPlanMode";
}

/* The card's title, in the person's terms. */
export function permissionTitle(params: PermissionParams): string {
  if (isPlan(params)) return "Claude has a plan";
  const kind = params.entry?.tool?.kind;
  const text = params.entry?.text || "";
  switch (kind) {
    case "command":
      return "Run a command";
    case "edit":
      return text || "Edit a file";
    case "read":
      return text || "Read a file";
    case "fetch":
      return text || "Fetch a URL";
    case "webSearch":
      return text || "Search the web";
    case "mcp":
      return "Call " + (params.entry?.tool?.name || params.tool);
  }
  return text || "Use " + params.tool;
}

/* The command a Bash ask runs, empty for other tools. */
export function askedCommand(params: PermissionParams): string {
  if (params.entry?.tool?.kind !== "command") return "";
  return params.entry.text || String(params.input?.command ?? "");
}

/* What "Allow always" remembers, as the service labelled it. */
export function alwaysLabel(params: PermissionParams): string {
  return params.always ? "Allow always: " + params.always : "Allow always";
}

/* The mode a plan is approved into and its button. */
export const PLAN_ANSWERS: { mode: string; label: string; hint: string }[] = [
  {
    mode: "auto",
    label: "Approve, auto-accept edits",
    hint: "Claude carries the plan out; every tool call is allowed",
  },
  {
    mode: "ask",
    label: "Approve, ask before edits",
    hint: "Claude carries the plan out and asks before commands that write and before file edits",
  },
];
