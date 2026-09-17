// Composer attachments: what a paste or drop carries, and the limits the
// chat service enforces (chat/internal/chats/attachments.go), checked here
// first so the sender hears about an oversized file before it uploads.
export const MAX_ATTACHMENT_BYTES = 8 * 1024 * 1024;
export const MAX_ATTACHMENTS = 8;

/* The shape shared by DataTransfer (drop) and clipboardData (paste). */
export type Transfer = {
  types?: ArrayLike<string>;
  items?: ArrayLike<{ kind: string; getAsFile(): File | null }>;
  files?: ArrayLike<File>;
};

/* Whether a drag carries files at all, so text drags keep their default. */
export function hasFiles(transfer: Transfer | null | undefined) {
  return Array.from(transfer?.types || []).includes("Files");
}

/* The files in a drop or paste. `items` is preferred because a pasted
   screenshot appears there (as a file item) while `files` is what a
   picker-style drop fills; both are read and duplicates kept once. */
export function transferFiles(transfer: Transfer | null | undefined): File[] {
  if (!transfer) return [];
  const out: File[] = [];
  for (const item of Array.from(transfer.items || [])) {
    if (item.kind !== "file") continue;
    const file = item.getAsFile();
    if (file) out.push(file);
  }
  if (!out.length) out.push(...Array.from(transfer.files || []));
  return out;
}

/* Why a file cannot be attached, or "" when it can. `count` is how many
   attachments the message already has. */
export function attachmentError(
  file: { name: string; size: number },
  count: number,
): string {
  if (count >= MAX_ATTACHMENTS)
    return `A message can carry at most ${MAX_ATTACHMENTS} attachments`;
  if (file.size === 0) return `${file.name || "This file"} is empty`;
  if (file.size > MAX_ATTACHMENT_BYTES)
    return `${file.name || "This file"} is larger than 8 MiB`;
  return "";
}

export function formatSize(bytes: number) {
  if (bytes < 1024) return `${bytes} B`;
  if (bytes < 1024 * 1024) return `${Math.round(bytes / 1024)} KB`;
  return `${(bytes / (1024 * 1024)).toFixed(1)} MB`;
}

/* A pasted image arrives as "image.png"; give it a name (local time) that
   tells one paste from the next. */
export function pastedName(file: { name: string; type: string }, at: Date) {
  if (file.name && file.name !== "image.png" && file.name !== "image.jpeg")
    return file.name;
  const two = (n: number) => String(n).padStart(2, "0");
  const stamp = `${at.getFullYear()}${two(at.getMonth() + 1)}${two(at.getDate())}T${two(at.getHours())}${two(at.getMinutes())}${two(at.getSeconds())}`;
  const ext = file.type === "image/jpeg" ? "jpg" : "png";
  return `pasted-${stamp}.${ext}`;
}
