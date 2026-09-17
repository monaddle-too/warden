import { saveFile } from "./export";
import type { Attachment, State } from "./types";
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
const credentials = () => ({
  ...(token ? { Authorization: "Bearer " + token } : {}),
  ...(remoteCSRF ? { "X-Warden-CSRF": remoteCSRF } : {}),
});
async function failure(response: Response) {
  const text = await response.text();
  try {
    return new Error(JSON.parse(text).error || text);
  } catch {
    return new Error(text);
  }
}
export async function api<T = unknown>(
  path: string,
  body?: unknown,
): Promise<T> {
  const response = await fetch("/api/" + path, {
    method: body === undefined ? "GET" : "POST",
    headers: {
      ...credentials(),
      ...(body === undefined ? {} : { "Content-Type": "application/json" }),
    },
    body: body === undefined ? undefined : JSON.stringify(body),
  });
  if (!response.ok) throw await failure(response);
  return response.json();
}

/* One file for the composer: multipart, answered with the stored record
   whose ID the message then names. */
export async function uploadAttachment(
  chatID: string,
  file: File,
): Promise<Attachment> {
  const body = new FormData();
  body.append("file", file, file.name);
  const response = await fetch(
    `/api/chats/${encodeURIComponent(chatID)}/attachments`,
    { method: "POST", headers: credentials(), body },
  );
  if (!response.ok) throw await failure(response);
  return response.json();
}

/* The service's copy of an attachment. Images are PNGs it normalised, so
   the same content-type check as imageBlob keeps anything else out of an
   <img>; files come back as blobs for a download link. */
export async function attachmentBlob(
  chatID: string,
  id: string,
  kind: Attachment["kind"],
): Promise<Blob> {
  const response = await fetch(
    `/api/chats/${encodeURIComponent(chatID)}/attachments/${encodeURIComponent(id)}`,
    { headers: credentials() },
  );
  if (!response.ok) throw new Error("Attachment unavailable");
  const type = response.headers.get("Content-Type");
  if (kind === "image" && type !== "image/png")
    throw new Error("Image unavailable");
  return response.blob();
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
  saveFile(path.split("/").pop() || "download", await response.blob());
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

// A workspace file for an inline transcript image, re-encoded as PNG by the
// chat service; same checks as imageBlob so a non-image answer never lands
// in an <img>.
export async function workspaceImageBlob(
  chatID: string,
  path: string,
): Promise<Blob> {
  const response = await fetch(
    `/api/chats/${encodeURIComponent(chatID)}/image-file?path=${encodeURIComponent(path)}`,
    { headers: token ? { Authorization: "Bearer " + token } : {} },
  );
  if (!response.ok || response.headers.get("Content-Type") !== "image/png")
    throw new Error("Image unavailable");
  return response.blob();
}
