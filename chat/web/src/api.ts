import type { Resources } from "./composer";
import { saveFile } from "./export";
import type {
  Checkpoint,
  RewindResult,
  RewindWhat,
  SessionChanges,
} from "./rewind";
import type { Attachment, Entry, State } from "./types";
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
    { headers: credentials() },
  );
  if (!response.ok) throw new Error(await response.text());
  saveFile(path.split("/").pop() || "download", await response.blob());
}

export async function imageBlob(chatID: string, id: string): Promise<Blob> {
  const response = await fetch(
    `/api/chats/${encodeURIComponent(chatID)}/images/${encodeURIComponent(id)}`,
    { headers: credentials() },
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
    { headers: credentials() },
  );
  if (!response.ok || response.headers.get("Content-Type") !== "image/png")
    throw new Error("Image unavailable");
  return response.blob();
}

// Workspace paths starting with what the composer has after an "@", from a
// bounded listing the worker makes inside the sandbox. The names are the
// agent's; the list shows them as text and never fetches them.
export async function workspacePaths(
  chatID: string,
  query: string,
): Promise<string[]> {
  const result = await api<{ paths?: unknown }>(
    `chats/${encodeURIComponent(chatID)}/paths?q=${encodeURIComponent(query)}`,
  );
  return Array.isArray(result.paths)
    ? result.paths.filter((p): p is string => typeof p === "string")
    : [];
}

// Checkpoints, rewind and the session diff (rewind.ts). A rewind names the
// message by its ID (or its turn's) and what to take back: the workspace,
// the conversation, or both.
export async function chatCheckpoints(chatID: string): Promise<Checkpoint[]> {
  const result = await api<{ checkpoints?: unknown }>(
    `chats/${encodeURIComponent(chatID)}/checkpoints`,
  );
  return Array.isArray(result.checkpoints)
    ? (result.checkpoints as Checkpoint[])
    : [];
}

export function rewindChat(
  chatID: string,
  turnID: string,
  what: RewindWhat,
): Promise<RewindResult> {
  return api<RewindResult>(`chats/${encodeURIComponent(chatID)}/rewind`, {
    turnID,
    what,
  });
}

/* A queued message out of the queue (its sender or the owner may); the
   entry comes back for the composer (queue.ts). */
export function withdrawMessage(chatID: string, id: string): Promise<Entry> {
  return api<Entry>(`chats/${encodeURIComponent(chatID)}/withdraw`, { id });
}

/* Lets a held queue go: the queued messages send in order. */
export function sendQueued(chatID: string): Promise<unknown> {
  return api(`chats/${encodeURIComponent(chatID)}/send-queued`, {});
}

/* A sibling chat copied from this one up to `turnID` (a user message, or
   its turn); the whole transcript when unset. */
export function forkChat(
  chatID: string,
  turnID?: string,
  copyWorkspace = false,
): Promise<ForkResult> {
  return api<ForkResult>(`chats/${encodeURIComponent(chatID)}/fork`, {
    turnID: turnID || "",
    copyWorkspace,
  });
}
export type ForkResult = {
  id: string;
  title: string;
  session: "forked" | "fresh" | "none";
  /* The fork's workspace: the source's ("shared") or a copy of it. */
  sandboxID?: string;
  workspace?: "shared" | "copied";
};

/* What the chat can mention with "@": its workspace's shared documents,
   repositories and previews (chats/mentions.go). */
export async function chatResources(chatID: string): Promise<Resources> {
  const result = await api<Partial<Resources>>(
    `chats/${encodeURIComponent(chatID)}/resources`,
  );
  return {
    documents: Array.isArray(result.documents) ? result.documents : [],
    repositories: Array.isArray(result.repositories) ? result.repositories : [],
    previews: Array.isArray(result.previews) ? result.previews : [],
  };
}

/* A side question answered from a copy of the chat's session; the answer
   lands as an aside entry over the event stream too. */
export function askAside(chatID: string, text: string): Promise<AsideResult> {
  return api<AsideResult>(`chats/${encodeURIComponent(chatID)}/aside`, {
    text,
  });
}
export type AsideResult = {
  id: string;
  text?: string;
  error?: string;
  costUSD?: number;
};

/* The output style a Claude chat launches with next ("" for the default). */
export function setOutputStyle(chatID: string, style: string) {
  return api(`chats/${encodeURIComponent(chatID)}/style`, { style });
}

export function sessionChanges(chatID: string): Promise<SessionChanges> {
  return api<SessionChanges>(`chats/${encodeURIComponent(chatID)}/diff`);
}
