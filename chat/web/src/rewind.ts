/* Checkpoints, rewind and the session diff: the pure parts. A checkpoint is
   the workspace as it was before one user message (the runner records it
   under the message's ID); a rewind goes back to before a message — its
   code, the conversation, or both — and the session diff is the workspace
   now against the chat's first checkpoint (or its last code rewind). */

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

function plural(n: number, unit: string): string {
  return n === 1 ? `1 ${unit}` : `${n} ${unit}s`;
}
