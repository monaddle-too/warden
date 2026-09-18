/* Desktop notifications: what deserves one, decided from two successive
   states of the chats (the event stream's), so every chat is covered and
   nothing is missed while the tab is hidden. Three things are worth a
   notification: an agent's turn ending (the answer is there), an agent
   asking for something (an approval or a tool permission), and a turn
   failing. Whether to show them at all is the reader's choice, kept in
   localStorage; the browser's own permission is asked once, from the
   chat menu's toggle. */
import type { Approval, Chat, Entry } from "./types";

export type NotifyEvent = {
  kind: "completed" | "approval" | "failed";
  chatID: string;
  title: string;
  body: string;
  /* A tag per event, so the browser replaces a repeat instead of stacking. */
  tag: string;
};

const BUSY = new Set(["running", "queued", "stopping"]);

/* The events between `prev` and `next`. A chat that is new in `next`
   (or `prev` unknown) yields nothing: only changes count. */
export function notifyEvents(
  prev: Chat[] | undefined,
  next: Chat[],
): NotifyEvent[] {
  if (!prev) return [];
  const before = new Map(prev.map((c) => [c.id, c]));
  const out: NotifyEvent[] = [];
  for (const chat of next) {
    const old = before.get(chat.id);
    if (!old) continue;
    const wasBusy = BUSY.has(old.status);
    const busy = BUSY.has(chat.status);
    if (wasBusy && !busy) {
      if (chat.status === "failed" || (chat.error && chat.error !== old.error))
        out.push({
          kind: "failed",
          chatID: chat.id,
          title: `${chat.title}: the agent's turn failed`,
          body: chat.error || "Open the chat for the details",
          tag: `${chat.id}:failed`,
        });
      else if (chat.status === "idle")
        out.push({
          kind: "completed",
          chatID: chat.id,
          title: `${chat.title}: the agent finished`,
          body: lastReply(chat.conversation.entries),
          tag: `${chat.id}:completed`,
        });
    }
    const seen = new Set(
      old.approvals.filter((a) => a.state === "pending").map((a) => a.id),
    );
    for (const a of chat.approvals) {
      if (a.state !== "pending" || seen.has(a.id)) continue;
      out.push({
        kind: "approval",
        chatID: chat.id,
        title: `${chat.title}: the agent is asking`,
        body: approvalBody(a),
        tag: `${chat.id}:approval:${a.id}`,
      });
    }
  }
  return out;
}

/* The agent's last words, for the completion's body. */
export function lastReply(entries: Entry[], max = 140): string {
  for (let i = entries.length - 1; i >= 0; i--) {
    const e = entries[i];
    if (e.role === "assistant" && !e.parentID && e.text.trim())
      return clip(e.text, max);
  }
  return "The agent's turn is over";
}

/* What the agent asks for, in a line. */
export function approvalBody(a: Approval, max = 140): string {
  const p = a.params;
  if (typeof p.tool === "string") {
    const description = typeof p.description === "string" ? p.description : "";
    const entry = p.entry as Entry | undefined;
    const what = description || entry?.text || "";
    return clip(
      p.tool === "ExitPlanMode"
        ? "Claude proposes a plan"
        : `${p.tool}${what ? ": " + what : ""}`,
      max,
    );
  }
  const questions = p.questions;
  if (Array.isArray(questions) && questions.length)
    return clip(questions[0].question || "A question for you", max);
  if (typeof p.reason === "string" && p.reason) return clip(p.reason, max);
  return "An approval is waiting";
}

function clip(text: string, max: number): string {
  const line = text.replace(/\s+/g, " ").trim();
  const chars = Array.from(line);
  return chars.length > max ? chars.slice(0, max).join("") + "…" : line;
}

/* The reader's choice: on, off, or not yet made. */
export const NOTIFY_KEY = "warden-notify";
export type NotifyPreference = "on" | "off" | "unset";

export function readNotifyPreference(
  storage: Pick<Storage, "getItem"> | undefined,
): NotifyPreference {
  try {
    const v = storage?.getItem(NOTIFY_KEY);
    return v === "1" ? "on" : v === "0" ? "off" : "unset";
  } catch {
    return "unset";
  }
}

export function writeNotifyPreference(
  storage: Pick<Storage, "setItem"> | undefined,
  on: boolean,
) {
  try {
    storage?.setItem(NOTIFY_KEY, on ? "1" : "0");
  } catch {}
}

/* Whether a notification should be shown now: the reader wants them, the
   browser allows them, and the tab is not being looked at. */
export function shouldNotify(
  preference: NotifyPreference,
  permission: string | undefined,
  hidden: boolean,
): boolean {
  return preference === "on" && permission === "granted" && hidden;
}
