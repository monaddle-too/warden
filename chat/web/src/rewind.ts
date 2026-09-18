/* Checkpoints, rewind and the session diff: the pure parts. A checkpoint is
   the workspace as it was before one user message (the runner records it
   under the message's ID); a rewind goes back to before a message — its
   code, the conversation, or both — and the session diff is the workspace
   now against the chat's first checkpoint (or its last code rewind).

   A conversation (or both) rewind can be undone until the next turn
   starts or another rewind happens: the service keeps what it removed
   and `chat.undoRewind` names the marker whose Undo puts it back. The
   session follows as far as it can (a pending rewind is cancelled, a
   dropped thread resumed, a session that rewound starts fresh with the
   restored transcript as context); the workspace stays as the rewind
   left it unless the checkpoint the rewind recorded of it is restored
   (`entry.rewind.before`, a both-rewind's). */

import type { Chat, Entry } from "./types";

export type RewindWhat = "code" | "conversation" | "both";

export type Checkpoint = {
  id: string;
  chatID: string;
  commit: string;
  tree: string;
  store: string;
  changed: boolean;
  at: string;
};

export type RewindResult = {
  messageID: string;
  what: RewindWhat;
  restored?: string[];
  removed?: string[];
  conversation?: "rewound" | "pending" | "fresh" | "";
  /* How many queued messages a conversation rewind withdrew (queue.ts). */
  withdrawn?: number;
};

/* What undoing a rewind did (chats.UndoResult). */
export type UndoResult = {
  messageID: string;
  what: RewindWhat;
  entries: number;
  requeued?: number;
  session: "cancelled" | "resumed" | "fresh" | "";
  code?: "restored" | "kept" | "";
  restored?: string[];
  removed?: string[];
};

export type ChangedFile = {
  path: string;
  added: number;
  removed: number;
  binary?: boolean;
};

export type SessionChanges = {
  base: string;
  files: ChangedFile[];
  diff: string;
  truncated?: boolean;
};

/* One message the chat can rewind to: the user message, its number among
   them (from 1) and whether the runner holds a checkpoint for it. */
export type RewindTarget = {
  entry: Entry;
  number: number;
  checkpoint: boolean;
};

export const WHAT_LABELS: Record<RewindWhat, { label: string; hint: string }> =
  {
    both: {
      label: "Code and conversation",
      hint: "The workspace goes back to before the message and the agent forgets it and what followed",
    },
    conversation: {
      label: "Conversation only",
      hint: "The agent forgets the message and what followed; files stay as they are",
    },
    code: {
      label: "Code only",
      hint: "The workspace goes back to before the message; the transcript stays",
    },
  };

/* The user messages one can rewind to, oldest first. A subagent's prompt
   (a user entry under a task card) is not one. */
export function rewindTargets(
  entries: Entry[],
  checkpoints: Iterable<string> = [],
): RewindTarget[] {
  const known = new Set(checkpoints);
  const out: RewindTarget[] = [];
  for (const entry of entries) {
    if (entry.role !== "user" || entry.parentID) continue;
    out.push({
      entry,
      number: out.length + 1,
      checkpoint: known.has(entry.id),
    });
  }
  return out;
}

/* Whether a rewind can be asked for now: the chat is neither busy nor
   archived. The service checks the same. */
export function canRewind(chat: Pick<Chat, "status" | "archived">): boolean {
  return (
    !chat.archived &&
    chat.status !== "running" &&
    chat.status !== "queued" &&
    chat.status !== "stopping"
  );
}

/* Whether a scope can be picked for a target: code needs its checkpoint. */
export function whatAllowed(what: RewindWhat, target: RewindTarget): boolean {
  return what === "conversation" || target.checkpoint;
}

/* A message's first words for a list or a marker. */
export function excerpt(text: string, max = 80): string {
  const line = text.replace(/\s+/g, " ").trim();
  if (!line) return "(attachments)";
  const chars = Array.from(line);
  return chars.length > max ? chars.slice(0, max).join("") + "…" : line;
}

/* Esc pressed twice within `window` ms, Claude Code's rewind key. */
export function doubleEscape(last: number, now: number, window = 600): boolean {
  return last > 0 && now - last <= window;
}

export function changesSummary(files: ChangedFile[]): {
  files: number;
  added: number;
  removed: number;
} {
  let added = 0;
  let removed = 0;
  for (const f of files) {
    added += f.added;
    removed += f.removed;
  }
  return { files: files.length, added, removed };
}

/* One git diff split into its files, by the path each `diff --git` line
   names (the new path, `b/…`), so each file renders as its own DiffView. */
export function splitDiff(diff: string): Map<string, string> {
  const out = new Map<string, string>();
  if (!diff) return out;
  const parts = diff.split(/^(?=diff --git )/m);
  for (const part of parts) {
    if (!part.startsWith("diff --git ")) continue;
    const header = part.split("\n", 1)[0];
    const m = /^diff --git a\/(.*) b\/(.*)$/.exec(header);
    const path = m ? m[2] : header.slice("diff --git ".length);
    out.set(path, (out.get(path) || "") + part);
  }
  return out;
}

/* The line under a rewind marker or a result: what the rewind did. */
export function rewindOutcome(result: RewindResult): string {
  const parts: string[] = [];
  if (result.what !== "conversation") {
    const n = (result.restored?.length || 0) + (result.removed?.length || 0);
    parts.push(
      n === 0
        ? "the workspace already matched the checkpoint"
        : `${plural(result.restored?.length || 0, "file")} restored, ${plural(result.removed?.length || 0, "file")} removed`,
    );
  }
  if (result.conversation === "pending")
    parts.push("the agent forgets the messages when its session resumes");
  if (result.conversation === "fresh")
    parts.push(
      "the agent's session could not rewind; the next message starts a new one with the conversation so far as context",
    );
  if (result.withdrawn)
    parts.push(`${plural(result.withdrawn, "queued message")} withdrawn`);
  return parts.join("; ");
}

/* Whether a rewind marker's rewind can be undone now: the service still
   keeps what it removed (`chat.undoRewind` names this marker) and the
   chat is idle. */
export function canUndoRewind(
  entry: Pick<Entry, "id" | "role">,
  chat: Pick<Chat, "status" | "archived" | "undoRewind">,
): boolean {
  return (
    entry.role === "rewind" && chat.undoRewind === entry.id && canRewind(chat)
  );
}

/* Whether an undo may restore the workspace too: the rewind moved the
   code and recorded the workspace as it was first. */
export function undoOffersCode(entry: Pick<Entry, "rewind">): boolean {
  return entry.rewind?.what === "both" && !!entry.rewind.before;
}

/* The undo's hint on the marker: what happens to the workspace. */
export function undoHint(entry: Pick<Entry, "rewind">): string {
  const base = "Undo puts the removed messages back (until the next turn)";
  if (entry.rewind?.what !== "both") return base;
  return undoOffersCode(entry)
    ? base + "; the files can come back too"
    : base + "; the workspace stays as it is";
}

/* The line an undo's result shows: what came back and how the session
   and the workspace followed. */
export function undoOutcome(result: UndoResult): string {
  const parts = [
    `${result.entries === 1 ? "1 entry" : `${result.entries} entries`} restored`,
  ];
  if (result.requeued)
    parts.push(`${plural(result.requeued, "message")} queued again and held`);
  if (result.session === "cancelled")
    parts.push("the agent's session never saw the rewind");
  if (result.session === "resumed")
    parts.push("the agent's session continues where it was");
  if (result.session === "fresh")
    parts.push(
      "the agent's session cannot take the messages back; the next message starts a new one with the conversation so far as context",
    );
  if (result.code === "restored")
    parts.push(
      `the workspace is back as it was before the rewind (${plural(result.restored?.length || 0, "file")} restored, ${plural(result.removed?.length || 0, "file")} removed)`,
    );
  if (result.code === "kept")
    parts.push("the workspace stays as the rewind left it");
  return parts.join("; ");
}

function plural(n: number, unit: string): string {
  return n === 1 ? `1 ${unit}` : `${n} ${unit}s`;
}
