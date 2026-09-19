/* The transcript's feedback while the model thinks, in the manner of each
   agent's own desktop app: what a thinking entry's disclosure says and
   whether it stands open, and when the row that stands for a reply not yet
   begun (the pulsing avatar for Claude, "Working" for Codex) shows. */
import { formatDuration } from "./turns";
import type { Chat, Entry } from "./types";

export type Provider = "claude" | "codex";

export const providerOf = (provider: string | undefined): Provider =>
  provider === "claude" ? "claude" : "codex";

type Thinking = Pick<Entry, "isStreaming" | "createdAt" | "endedAt">;

/* The disclosure's heading: claude.ai says "Thinking…" and then "Thought
   for 12s"; the Codex app says "Thinking" without the ellipsis. */
export function thinkingLabel(provider: Provider, entry: Thinking): string {
  if (entry.isStreaming)
    return provider === "claude" ? "Thinking…" : "Thinking";
  const seconds = (entry.endedAt ?? 0) - entry.createdAt;
  return seconds > 0 ? `Thought for ${formatDuration(seconds)}` : "Thought";
}

/* Whether the disclosure is open before the reader touches it: claude.ai
   keeps the thinking folded; the Codex app streams the reasoning summary in
   the open and folds it when the model moves on. */
export const thinkingOpen = (provider: Provider, streaming: boolean) =>
  provider === "codex" && streaming;

/* Whether a reply is awaited with nothing on screen standing for it: a
   turn runs, nothing streams, and the model, not the owner, is the one
   being waited on. Before the sandbox is sending the message the status
   line already says what is happening, and it is not the model thinking.
   `since` is when the turn's message was sent, for an elapsed count. */
export function pendingReply(
  chat: Pick<Chat, "status" | "startup" | "conversation">,
  waiting: boolean,
): { since?: number } | undefined {
  if (chat.status !== "running" || waiting) return undefined;
  if (
    chat.startup &&
    chat.startup.stage !== "sending" &&
    chat.startup.stage !== "firstResponse"
  )
    return undefined;
  const entries = chat.conversation.entries;
  if (entries.some((e) => e.isStreaming)) return undefined;
  let since: number | undefined;
  for (let i = entries.length - 1; i >= 0; i--) {
    if (entries[i].role === "user") {
      since = entries[i].createdAt;
      break;
    }
  }
  return { since };
}
