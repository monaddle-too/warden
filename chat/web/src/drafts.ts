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

export function readLocalAttempt(key: string): Attempt | undefined {
  try {
    return readAttempt(localStorage, key);
  } catch {
    return undefined;
  }
}
