import { useEffect, useState } from "react";
import { Download, FileText, LoaderCircle, X } from "lucide-react";
import { attachmentBlob } from "../api";
import { formatSize } from "../attachments";
import type { Attachment } from "../types";
import { Lightbox } from "./Lightbox";

/* A file in the composer: chosen, being uploaded, uploaded (with the
   service's record) or refused. */
export type Pending = {
  key: number;
  file: File;
  status: "uploading" | "ready" | "failed";
  attachment?: Attachment;
  error?: string;
};

/* A local preview of an image the sender chose; the object URL lives as
   long as the chip does. */
function useFilePreview(file: File) {
  const [url, setURL] = useState("");
  useEffect(() => {
    if (!file.type.startsWith("image/")) return;
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

function PendingChip({
  item,
  onRemove,
}: {
  item: Pending;
  onRemove: () => void;
}) {
  const preview = useFilePreview(item.file);
  const name = item.attachment?.name || item.file.name || "attachment";
  return (
    <span
      className={`chip attachment-chip ${item.status}`}
      title={item.error || name}
    >
      {preview.url ? (
        <img
          className="attachment-thumb"
          src={preview.url}
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
            : formatSize(item.file.size)}
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
  items,
  onRemove,
}: {
  items: Pending[];
  onRemove: (key: number) => void;
}) {
  if (!items.length) return null;
  return (
    <div className="composer-attachments" aria-label="Attachments">
      {items.map((item) => (
        <PendingChip
          key={item.key}
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
  const [url, setURL] = useState("");
  const [error, setError] = useState(false);
  const [open, setOpen] = useState(false);
  // Keyed on the ID, not the record: every state frame is a fresh object.
  const { id, kind } = attachment;
  useEffect(() => {
    let stopped = false,
      object = "";
    setURL("");
    setError(false);
    void attachmentBlob(chatID, id, kind).then(
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
  }, [chatID, id, kind]);
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
      const blob = await attachmentBlob(chatID, attachment.id, attachment.kind);
      const url = URL.createObjectURL(blob);
      const link = document.createElement("a");
      link.href = url;
      link.download = attachment.name;
      link.click();
      setTimeout(() => URL.revokeObjectURL(url), 1000);
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
