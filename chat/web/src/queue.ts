/* The message queue and edit-and-resend: the pure parts
   (docs/claude-parity.md, item 10 and round 2 D).

   A message sent while the agent's turn runs is held by Warden as a user
   entry whose delivery is "queued", in transcript order, and becomes its
   own turn once the turn ends. Until then it can be withdrawn, or edited
   in place on its card (Claude Code's ↑ edits the last queued one): the
   edit keeps the message's slot and ID, and a message the agent got
   meanwhile refuses it. Stop holds the queue: the interrupted chat keeps
   its queued messages until Send on a card, or the next message, lets
   them go; a `!` command or a `#` note runs beside the held queue and
   leaves it held, since neither is addressed to the agent.

   Editing a message the agent already got means going back: sending the
   edit rewinds the conversation to before the message (the code too, if
   asked) and sends the new text as a new message. */

import type { Chat, Entry } from "./types";
import { canRewind, type RewindWhat } from "./rewind";

type Person = { principalID: string };

/* The chat's queued messages, in the order they will go. A subagent's
   prompt (a user entry under a task card) is never one. */
export function queuedMessages(entries: Entry[]): Entry[] {
  return entries.filter(
    (e) => e.role === "user" && !e.parentID && e.delivery === "queued",
  );
}

/* The entries with the queued messages last, in their order: a queued
   message waits at the bottom of the transcript however early it was
   sent, and the service moves it there for good when the agent gets it. */
export function queuedLast(entries: Entry[]): Entry[] {
  const queued = queuedMessages(entries);
  if (!queued.length) return entries;
  return entries.filter((e) => !queued.includes(e)).concat(queued);
}

/* Whether the queue is held: nothing runs (Stop interrupted the turn, or
   the run ended) while messages are still queued. They go with Send on a
   card or with the next message. */
export function queueHeld(
  chat: Pick<Chat, "status"> & { conversation: { entries: Entry[] } },
): boolean {
  return (
    chat.status !== "running" &&
    chat.status !== "queued" &&
    chat.status !== "stopping" &&
    queuedMessages(chat.conversation.entries).length > 0
  );
}

/* The line beside a queued message: what will happen to it. */
export function queueLabel(
  chat: Pick<Chat, "status"> & { conversation: { entries: Entry[] } },
): string {
  if (queueHeld(chat)) return "Held · the agent was stopped; send or withdraw it";
  if (chat.status === "running")
    return "Queued · will send when the agent finishes";
  return "Queued · next up";
}

/* Whether this person may withdraw or edit a queued message: its sender,
   or the owner. An entry without a sender is the owner's. */
export function canWithdraw(entry: Entry, me: Person): boolean {
  if (entry.role !== "user" || entry.delivery !== "queued") return false;
  const sender = entry.sender?.principalID || "owner";
  return me.principalID === "owner" || me.principalID === sender;
}

/* The queued message ↑ edits from an empty composer: the last one this
   person may withdraw. */
export function lastQueued(entries: Entry[], me: Person): Entry | undefined {
  const mine = queuedMessages(entries).filter((e) => canWithdraw(e, me));
  return mine[mine.length - 1];
}

/* Whether a message the agent got can be edited and resent now: sending
   the edit rewinds the conversation first, which the service accepts
   only while the chat is idle. */
export function canEditAndResend(
  entry: Pick<Entry, "role" | "delivery" | "parentID">,
  chat: Pick<Chat, "status" | "archived">,
  live: boolean,
): boolean {
  return (
    entry.role === "user" &&
    !entry.parentID &&
    entry.delivery !== "queued" &&
    live &&
    canRewind(chat)
  );
}

/* An edit-and-resend in progress: the message being edited, the draft
   the composer held before (given back on cancel) and whether the code
   goes back too. */
export type Editing = {
  entry: Entry;
  draft: string;
  code: boolean;
};

/* What sending an edit rewinds: the conversation, or the code as well. */
export function editingScope(editing: Pick<Editing, "code">): RewindWhat {
  return editing.code ? "both" : "conversation";
}

/* The composer's hint about the queue, or "" when nothing is queued. */
export function queueHint(
  chat: Pick<Chat, "status"> & { conversation: { entries: Entry[] } },
  me: Person,
): string {
  const queued = queuedMessages(chat.conversation.entries);
  if (!queued.length) return "";
  const n = queued.length;
  const count = `${n} message${n === 1 ? "" : "s"}`;
  if (queueHeld(chat))
    return `${count} held — sent with your next message, or with Send on the card`;
  const editable = lastQueued(chat.conversation.entries, me);
  return `${count} queued · will send in order after this turn${editable ? " · ↑ edits the last one" : ""}`;
}

/* The composer's hint for a `!` command or `#` note while the queue is
   held: it runs beside the queue and leaves it held. "" when the queue
   is not held. */
export function heldHint(
  chat: Pick<Chat, "status"> & { conversation: { entries: Entry[] } },
  kind: "shell" | "memory",
): string {
  if (!queueHeld(chat)) return "";
  const n = queuedMessages(chat.conversation.entries).length;
  const what = kind === "shell" ? "The command runs" : "The note is added";
  return `${what} beside the held queue: ${n} message${n === 1 ? " stays" : "s stay"} held until Send on a card or your next message`;
}
