// Chips for the long pastes collapsed in the composer (paste.ts): one per
// placeholder still in the text, its first lines on hover, the whole text
// in a dialog on click, and a remove control that drops the placeholder
// with it.
import { useEffect, useRef, useState } from "react";
import { ClipboardPaste, X } from "lucide-react";
import { pasteLines, pastePreview, placeholder, type Paste } from "../paste";

export function ComposerPastes({
  items,
  onRemove,
}: {
  items: Paste[];
  onRemove: (paste: Paste) => void;
}) {
  const [open, setOpen] = useState<Paste | undefined>();
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    const el = dialog.current;
    if (!el) return;
    if (open && !el.open) el.showModal();
    if (!open && el.open) el.close();
  }, [open]);
  if (!items.length) return null;
  return (
    <div className="composer-pastes" aria-label="Pasted text">
      {items.map((paste) => {
        const lines = pasteLines(paste.text);
        const label = `Pasted text #${paste.n}`;
        return (
          <span
            key={paste.n}
            className="chip paste-chip"
            title={pastePreview(paste.text)}
          >
            <button
              type="button"
              className="paste-open"
              aria-label={`Show ${label}`}
              onClick={() => setOpen(paste)}
            >
              <ClipboardPaste size={14} />
              <span>{label}</span>
              <span className="muted">
                {lines} {lines === 1 ? "line" : "lines"}
              </span>
            </button>
            <button
              type="button"
              className="ghost icon paste-remove"
              aria-label={`Remove ${label}`}
              onClick={() => onRemove(paste)}
            >
              <X size={13} />
            </button>
          </span>
        );
      })}
      <dialog
        ref={dialog}
        className="paste-dialog"
        aria-label={open ? placeholder(open) : "Pasted text"}
        onClose={() => setOpen(undefined)}
        onClick={(event) => {
          if (event.target === event.currentTarget) setOpen(undefined);
        }}
      >
        {open && (
          <>
            <header>
              <strong>{placeholder(open)}</strong>
              <button
                type="button"
                className="ghost icon"
                aria-label="Close"
                onClick={() => setOpen(undefined)}
              >
                <X size={15} />
              </button>
            </header>
            <pre>{open.text}</pre>
          </>
        )}
      </dialog>
    </div>
  );
}
