// Inline `![alt](src)` images in the transcript. Only a plain relative
// workspace path is ever fetched, and only through chats/{id}/image-file,
// where the worker reads the file without following symlinks and imageguard
// re-encodes it as PNG. Everything else the agent may write as a source (a
// scheme such as http, data or javascript, a host, an absolute path, a
// parent reference) is left as alt text, so the markdown can never make the
// browser fetch on the agent's behalf.
export function workspaceImagePath(src: string | undefined) {
  if (!src || src.length > 1024) return undefined;
  if (/^[a-z][a-z0-9+.-]*:/i.test(src) || /^[/\\]/.test(src)) return undefined;
  let path: string;
  try {
    path = decodeURIComponent(src);
  } catch {
    return undefined;
  }
  const parts = path.split("/");
  while (parts[0] === ".") parts.shift();
  if (!parts.length || parts.some((p) => p === "" || p === "." || p === ".."))
    return undefined;
  return parts.join("/");
}

/* Runs tasks with at most `max` in flight; the rest wait in order. */
export function limiter(max: number) {
  let active = 0;
  const waiting: (() => void)[] = [];
  return <T>(task: () => Promise<T>) =>
    new Promise<T>((resolve, reject) => {
      const start = () => {
        active++;
        task()
          .then(resolve, reject)
          .finally(() => {
            active--;
            waiting.shift()?.();
          });
      };
      if (active < max) start();
      else waiting.push(start);
    });
}

/* Shares one fetch per (chat, scope, path) so a remount (switching chats and
   back) does not re-read the sandbox, and caps how many run at once so a
   message with many images does not fan out into that many sandbox execs.
   The scope is the entry: the same path in a later message is read again,
   as the file may have changed. Failures are not kept, the file may appear
   later. */
export function imageLoader(
  load: (chatID: string, path: string) => Promise<Blob>,
  { limit = 24, inflight = 3 } = {},
) {
  const cache = new Map<string, Promise<Blob>>();
  const run = limiter(inflight);
  return (chatID: string, scope: string, path: string) => {
    const key = [chatID, scope, path].join("\n");
    let pending = cache.get(key);
    if (!pending) {
      const started = (pending = run(() => load(chatID, path)));
      // Only this entry is forgotten: the key may have been evicted and
      // re-requested while this fetch was still in flight.
      started.catch(() => {
        if (cache.get(key) === started) cache.delete(key);
      });
      cache.set(key, pending);
      for (const oldest of cache.keys()) {
        if (cache.size <= limit) break;
        cache.delete(oldest);
      }
    }
    return pending;
  };
}
