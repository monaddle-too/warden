// A retry keeps its message identity even after a reload or uncertain response.
export type Attempt = { id: string; text: string; attachments?: string[] };
const hex32 = /^[a-f0-9]{32}$/;
export function readAttempt(
  storage: Pick<Storage, "getItem">,
  key: string,
): Attempt | undefined {
  try {
    const v = JSON.parse(storage.getItem(key) || "null");
    if (!v || !hex32.test(v.id) || typeof v.text !== "string") return undefined;
    const attempt: Attempt = { id: v.id, text: v.text };
    if (
      Array.isArray(v.attachments) &&
      v.attachments.every(
        (a: unknown) => typeof a === "string" && hex32.test(a),
      )
    )
      attempt.attachments = v.attachments;
    return attempt;
  } catch {}
  return undefined;
}
/* The same text with the same attachments is the same message; anything
   else is a new one. */
export function messageAttempt(
  previous: Attempt | undefined,
  text: string,
  id: () => string,
  attachments: string[] = [],
): Attempt {
  const same =
    previous?.text === text &&
    (previous.attachments || []).join() === attachments.join();
  if (same) return previous;
  return attachments.length
    ? { id: id(), text, attachments }
    : { id: id(), text };
}

/* A message sent again from the transcript: the same text and the same
   uploads (the service lets a chat name a sent attachment again), under a
   new ID so the service does not take it for the earlier delivery. */
export function resendAttempt(
  entry: { text: string; attachments?: { id: string }[] },
  id: () => string,
): Attempt {
  const attachments = (entry.attachments || []).map((a) => a.id);
  return messageAttempt(undefined, entry.text.trim(), id, attachments);
}

/* Whether a user entry can be sent again or edited now: not while the
   chat is archived or between states, not while the entry itself is still
   waiting for delivery (a retry then would send it twice). */
export function canResend(
  entry: { role: string; delivery?: string },
  chat: { archived: boolean; status: string },
  live: boolean,
): boolean {
  return (
    entry.role === "user" &&
    entry.delivery !== "queued" &&
    live &&
    !chat.archived &&
    chat.status !== "queued" &&
    chat.status !== "stopping"
  );
}

export function readLocalAttempt(key: string): Attempt | undefined {
  try {
    return readAttempt(localStorage, key);
  } catch {
    return undefined;
  }
}
