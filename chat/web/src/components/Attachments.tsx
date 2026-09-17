import { useEffect, useState } from "react";
import { Download, FileText, LoaderCircle, X } from "lucide-react";
import { attachmentBlob } from "../api";
import { formatSize } from "../attachments";
import { saveFile } from "../export";
import type { Attachment } from "../types";
import { Lightbox } from "./Lightbox";

/* A file in the composer: chosen, being uploaded, uploaded (with the
   service's record) or refused. An item taken from a sent message while it
   is edited has the record but no File, and is `reused`: the service keeps
   the upload, so removing the chip does not remove it. */
export type Pending = {
  key: number;
  file?: File;
  status: "uploading" | "ready" | "failed";
  attachment?: Attachment;
  error?: string;
  reused?: boolean;
};

/* A local preview of an image the sender chose; the object URL lives as
   long as the chip does. */
function useFilePreview(file?: File) {
  const [url, setURL] = useState("");
  useEffect(() => {
    if (!file?.type.startsWith("image/")) return;
    const object = URL.createObjectURL(file);
    setURL(object);
    return () => {
      setURL("");
      URL.revokeObjectURL(object);
    };
  }, [file]);
  // A file the browser cannot decode (a mislabelled type) shows the icon.
  const [broken, setBroken] = useState(false);
  return { url: broken ? "" : url, onBroken: () => setBroken(true) };
}

/* The service's copy of a sent image as an object URL, empty until it
   loads or when it cannot; nothing is fetched for a file or no record. */
function useAttachmentURL(chatID: string, attachment?: Attachment) {
  const [url, setURL] = useState("");
  const [error, setError] = useState(false);
  // Keyed on the ID, not the record: every state frame is a fresh object.
  const id = attachment?.id;
  const image = attachment?.kind === "image";
  useEffect(() => {
    let stopped = false,
      object = "";
    setURL("");
    setError(false);
    if (!id || !image) return;
    void attachmentBlob(chatID, id, "image").then(
      (blob) => {
        if (stopped) return;
        object = URL.createObjectURL(blob);
        setURL(object);
      },
      () => {
        if (!stopped) setError(true);
      },
    );
    return () => {
      stopped = true;
      if (object) URL.revokeObjectURL(object);
    };
  }, [chatID, id, image]);
  return { url, error };
}

function PendingChip({
  chatID,
  item,
  onRemove,
}: {
  chatID: string;
  item: Pending;
  onRemove: () => void;
}) {
  const preview = useFilePreview(item.file);
  const stored = useAttachmentURL(
    chatID,
    item.file ? undefined : item.attachment,
  );
  const thumb = preview.url || stored.url;
  const name = item.attachment?.name || item.file?.name || "attachment";
  const size = item.file?.size ?? item.attachment?.size ?? 0;
  return (
    <span
      className={`chip attachment-chip ${item.status}`}
      title={item.error || name}
    >
      {thumb ? (
        <img
          className="attachment-thumb"
          src={thumb}
          alt=""
          onError={preview.onBroken}
        />
      ) : item.status === "uploading" ? (
        <LoaderCircle size={14} className="spin" />
      ) : (
        <FileText size={14} />
      )}
      <span>{name}</span>
      <span className="muted">
        {item.status === "uploading"
          ? "Uploading…"
          : item.status === "failed"
            ? item.error || "Failed"
            : formatSize(size)}
      </span>
      <button
        type="button"
        className="ghost icon attachment-remove"
        aria-label={`Remove ${name}`}
        onClick={onRemove}
      >
        <X size={13} />
      </button>
    </span>
  );
}

/* The chips above the composer's text. */
export function ComposerAttachments({
  chatID,
  items,
  onRemove,
}: {
  chatID: string;
  items: Pending[];
  onRemove: (key: number) => void;
}) {
  if (!items.length) return null;
  return (
    <div className="composer-attachments" aria-label="Attachments">
      {items.map((item) => (
        <PendingChip
          key={item.key}
          chatID={chatID}
          item={item}
          onRemove={() => onRemove(item.key)}
        />
      ))}
    </div>
  );
}

/* An image the sender attached, from the service's normalised copy. */
function AttachmentImage({
  chatID,
  attachment,
}: {
  chatID: string;
  attachment: Attachment;
}) {
  const { url, error } = useAttachmentURL(chatID, attachment);
  const [open, setOpen] = useState(false);
  if (!url)
    return (
      <span className="chip attachment-chip" title={attachment.path}>
        <FileText size={14} />
        <span>{attachment.name}</span>
        <span className="muted">{error ? "Unavailable" : "Loading…"}</span>
      </span>
    );
  return (
    <>
      <button
        type="button"
        className="inline-image attachment-image"
        title={`${attachment.name} · ${attachment.path}`}
        onClick={() => setOpen(true)}
      >
        <img src={url} alt={attachment.name} />
      </button>
      {open && (
        <Lightbox
          url={url}
          alt={attachment.name}
          onClose={() => setOpen(false)}
        />
      )}
    </>
  );
}

/* A file the sender attached: a chip that downloads the service's copy
   under its original name. */
function AttachmentFile({
  chatID,
  attachment,
}: {
  chatID: string;
  attachment: Attachment;
}) {
  const [busy, setBusy] = useState(false);
  async function download() {
    setBusy(true);
    try {
      saveFile(
        attachment.name,
        await attachmentBlob(chatID, attachment.id, attachment.kind),
      );
    } catch {
      /* the chip stays; the file is still in the workspace */
    } finally {
      setBusy(false);
    }
  }
  return (
    <button
      type="button"
      className="chip attachment-chip"
      title={`Download · ${attachment.path}`}
      disabled={busy}
      onClick={download}
    >
      <FileText size={14} />
      <span>{attachment.name}</span>
      <span className="muted">{formatSize(attachment.size)}</span>
      <Download size={13} />
    </button>
  );
}

/* What a sent message carried, beneath its text. */
export function EntryAttachments({
  chatID,
  attachments,
}: {
  chatID: string;
  attachments: Attachment[];
}) {
  return (
    <div className="entry-attachments">
      {attachments.map((a) =>
        a.kind === "image" ? (
          <AttachmentImage key={a.id} chatID={chatID} attachment={a} />
        ) : (
          <AttachmentFile key={a.id} chatID={chatID} attachment={a} />
        ),
      )}
    </div>
  );
}
