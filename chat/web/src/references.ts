export type Reference = {
  status?: "deleted";
  kind: "document" | "chat" | "shared_conversation";
  id: string;
  title: string;
  url: string;
  subtitle?: string;
};
export type ReferenceKey = Pick<Reference, "kind" | "id">;
let scope = "",
  csrf = "";
const cache = new Map<string, { at: number; items: Reference[] }>();
const known = new Map<string, Reference>();
const pending = new Map<
  string,
  { controller: AbortController; promise: Promise<Reference[]>; users: number }
>();
export const referencesEnabled = () => !!scope;
export function configureReferences(identity: string, token = "") {
  csrf = token;
  if (identity === scope) return;
  scope = identity;
  for (const request of pending.values()) request.controller.abort();
  pending.clear();
  cache.clear();
  known.clear();
}
export function seedReferences(items: Reference[]) {
  for (const item of items) known.set(item.kind + ":" + item.id, item);
  while (known.size > 200) known.delete(known.keys().next().value!);
}
export function localReferences(query: string) {
  const q = query.trim().toLowerCase();
  const rank = (r: Reference) =>
    r.title.toLowerCase() === q
      ? 0
      : r.title.toLowerCase().startsWith(q)
        ? 1
        : 2;
  return [...known.values()]
    .filter((r) => r.title.toLowerCase().includes(q))
    .sort(
      (a, b) =>
        rank(a) - rank(b) ||
        a.title.localeCompare(b.title) ||
        a.id.localeCompare(b.id),
    )
    .slice(0, 12);
}
export function searchReferences(
  query: string,
  signal: AbortSignal,
): Promise<Reference[]> {
  if (!scope || signal.aborted)
    return Promise.reject(new DOMException("Cancelled", "AbortError"));
  const q = query.trim().toLowerCase();
  const cached = cache.get(q);
  if (cached && Date.now() - cached.at < 15_000)
    return Promise.resolve(cached.items);
  let request = pending.get(q);
  if (!request) {
    const controller = new AbortController(),
      generation = scope;
    const promise = fetch("/api/references/search?q=" + encodeURIComponent(q), {
      signal: controller.signal,
    }).then(async (response) => {
      if (!response.ok) throw new Error("Search unavailable. Try again.");
      const result = (await response.json()) as { items: Reference[] };
      if (controller.signal.aborted || generation !== scope)
        throw new DOMException("Cancelled", "AbortError");
      cache.set(q, { at: Date.now(), items: result.items });
      seedReferences(result.items);
      while (cache.size > 30) cache.delete(cache.keys().next().value!);
      return result.items;
    });
    request = { controller, promise, users: 0 };
    const current = request;
    pending.set(q, current);
    void promise
      .finally(() => {
        if (pending.get(q) === current) pending.delete(q);
      })
      .catch(() => {});
  }
  const current = request;
  current.users++;
  return new Promise((resolve, reject) => {
    let done = false;
    const finish = () => {
      if (done) return false;
      done = true;
      signal.removeEventListener("abort", abort);
      current.users--;
      if (!current.users) {
        current.controller.abort();
        if (pending.get(q) === current) pending.delete(q);
      }
      return true;
    };
    const abort = () => {
      if (finish()) reject(new DOMException("Cancelled", "AbortError"));
    };
    signal.addEventListener("abort", abort, { once: true });
    current.promise.then(
      (items) => {
        if (finish()) resolve(items);
      },
      (error) => {
        if (finish()) reject(error);
      },
    );
  });
}
export async function resolveReferences(
  references: ReferenceKey[],
  signal?: AbortSignal,
): Promise<Reference[]> {
  const response = await fetch("/api/references/resolve", {
    method: "POST",
    signal,
    headers: { "Content-Type": "application/json", "X-Warden-CSRF": csrf },
    body: JSON.stringify({ references }),
  });
  if (!response.ok)
    throw new Error("This link is unavailable in this organization.");
  return (await response.json()).items;
}
// Explicit legacy prefixes win. Multiword titles stop at a newline or another
// mention; email addresses and Markdown links never open the picker.
export function referenceTrigger(text: string, caret: number) {
  const before = text.slice(0, caret);
  const match = /(?:^|[\s(])@([^@\n\r\[\]()]{0,160})$/.exec(before);
  if (
    !match ||
    /^(?:[.~\/]|(?:doc|repo|preview):)/i.test(match[1]) ||
    match[1].includes("/")
  )
    return undefined;
  return {
    kind: "path" as const,
    start: caret - match[1].length - 1,
    end: caret,
    query: match[1],
  };
}
export function referenceMarkdown(item: Reference) {
  return `[${item.title.replace(/([\\\[\]*_`<>])/g, "\\$1").replace(/[\r\n]/g, " ")}](${location.origin}${item.url})`;
}
export function referenceFromURL(href: string): ReferenceKey | undefined {
  const url = new URL(href, location.origin);
  if (url.origin !== location.origin) return;
  const doc = /^\/documents\/([\w-]+)\/?$/.exec(url.pathname);
  if (doc) return { kind: "document", id: doc[1] };
  const shared = /^\/shared-conversations\/([a-f0-9]{32})\/?$/.exec(
    url.pathname,
  );
  if (shared) return { kind: "shared_conversation", id: shared[1] };
  const chat = url.pathname === "/" && url.searchParams.get("chat");
  if (chat && /^[\w-]+$/.test(chat)) return { kind: "chat", id: chat };
}

export function invalidateDocumentReferences() {
  for (const request of pending.values()) request.controller.abort();
  pending.clear();
  cache.clear();
  for (const [key, item] of known)
    if (item.kind === "document") known.delete(key);
}
