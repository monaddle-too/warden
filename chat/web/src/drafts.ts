// A retry keeps its message identity even after a reload or uncertain response.
export type Attempt = { id: string; text: string };
export function readAttempt(
  storage: Pick<Storage, "getItem">,
  key: string,
): Attempt | undefined {
  try {
    const v = JSON.parse(storage.getItem(key) || "null");
    if (v && /^[a-f0-9]{32}$/.test(v.id) && typeof v.text === "string")
      return v;
  } catch {}
  return undefined;
}
export function messageAttempt(
  previous: Attempt | undefined,
  text: string,
  id: () => string,
): Attempt {
  return previous?.text === text ? previous : { id: id(), text };
}

export function readLocalAttempt(key: string): Attempt | undefined {
  try {
    return readAttempt(localStorage, key);
  } catch {
    return undefined;
  }
}
