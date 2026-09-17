import type { State } from "./types";
const key = "warden-chat-session";
let token = "";
try {
  token = sessionStorage.getItem(key) || "";
} catch {}
const fragment = new URLSearchParams(location.hash.slice(1));
if (fragment.has("session")) {
  token = fragment.get("session") || "";
  try {
    sessionStorage.setItem(key, token);
  } catch {}
  // Drop the capability from the URL but keep the query: ?chat=ID selects
  // the chat a popup or link points at.
  history.replaceState(null, "", location.pathname + location.search);
}
let remoteCSRF = "";
// The signed-in person, as the edge will attribute them; "owner" without
// sign-in (a local install).
export let me: { principalID: string; email?: string; name?: string } = {
  principalID: "owner",
};
export function remoteSession(
  csrf: string,
  user?: { sub: string; email: string; name?: string },
) {
  remoteCSRF = csrf;
  if (user) me = { principalID: user.sub, email: user.email, name: user.name };
}
export const signedIn = () => !!token || !!remoteCSRF;
export async function api<T = unknown>(
  path: string,
  body?: unknown,
): Promise<T> {
  const response = await fetch("/api/" + path, {
    method: body === undefined ? "GET" : "POST",
    headers: {
      ...(token ? { Authorization: "Bearer " + token } : {}),
      ...(remoteCSRF ? { "X-Warden-CSRF": remoteCSRF } : {}),
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!response.ok) {
    const text = await response.text();
    try {
      throw new Error(JSON.parse(text).error || text);
    } catch (error) {
      if (error instanceof SyntaxError) throw new Error(text);
      throw error;
    }
  }
  return response.json();
}
export async function subscribe(
  signal: AbortSignal,
  receive: (state: State) => void,
  status: (live: boolean) => void,
) {
  while (!signal.aborted) {
    try {
      const response = await fetch("/api/events", {
        headers: { Authorization: "Bearer " + token },
        signal,
      });
      if (response.status === 401) {
        token = "";
        remoteCSRF = "";
        window.dispatchEvent(new Event("warden-session-expired"));
        try {
          sessionStorage.removeItem(key);
        } catch {}
        receive({ version: 1, chats: [] });
        status(false);
        return;
      }
      if (!response.ok || !response.body)
        throw new Error("Chat stream unavailable");
      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let buffer = "";
      while (!signal.aborted) {
        const { value, done } = await reader.read();
        if (done) break;
        buffer += decoder.decode(value, { stream: true });
        let end: number;
        while ((end = buffer.indexOf("\n\n")) >= 0) {
          const frame = buffer.slice(0, end);
          buffer = buffer.slice(end + 2);
          if (frame.startsWith("data: ")) {
            receive(JSON.parse(frame.slice(6)));
            status(true);
          }
        }
      }
    } catch {
      /* Reconnect to a full durable snapshot; no message replay. */
    }
    status(false);
    if (!signal.aborted)
      await new Promise<void>((resolve) => {
        const finish = () => {
          clearTimeout(timer);
          signal.removeEventListener("abort", finish);
          resolve();
        };
        const timer = setTimeout(finish, 1000);
        signal.addEventListener("abort", finish, { once: true });
      });
  }
}
export function newID() {
  return crypto.randomUUID().replaceAll("-", "");
}

export async function downloadFile(chatID: string, path: string) {
  const response = await fetch(
    `/api/chats/${chatID}/file?path=${encodeURIComponent(path)}`,
    { headers: { Authorization: "Bearer " + token } },
  );
  if (!response.ok) throw new Error(await response.text());
  const url = URL.createObjectURL(await response.blob());
  const link = document.createElement("a");
  link.href = url;
  link.download = path.split("/").pop() || "download";
  link.click();
  setTimeout(() => URL.revokeObjectURL(url), 1000);
}

export async function imageBlob(chatID: string, id: string): Promise<Blob> {
  const response = await fetch(
    `/api/chats/${encodeURIComponent(chatID)}/images/${encodeURIComponent(id)}`,
    { headers: token ? { Authorization: "Bearer " + token } : {} },
  );
  if (!response.ok || response.headers.get("Content-Type") !== "image/png")
    throw new Error("Image unavailable");
  return response.blob();
}
