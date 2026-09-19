import { useEffect, useRef } from "react";
import { isKey } from "../shortcuts";
import { createPortal } from "react-dom";
import { X } from "lucide-react";
/* Full-size view of a transcript image in a native modal dialog: Escape,
   the close button and a click on the backdrop all close it. Portalled to
   <body> because the trigger sits inside a paragraph, where a dialog would
   be invalid nesting. `url` is always an object URL of a blob the chat
   service served, never an agent-written address. */
export function Lightbox({
  url,
  alt,
  onClose,
}: {
  url: string;
  alt: string;
  onClose: () => void;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    dialog.current?.showModal();
  }, []);
  return createPortal(
    <dialog
      ref={dialog}
      className="lightbox"
      aria-label={alt}
      onClose={onClose}
      onClick={(event) => {
        if (event.target === event.currentTarget) dialog.current?.close();
      }}
      // Explicit as well as the dialog's own cancel handling, which some
      // synthetic key events do not reach.
      onKeyDown={(event) => {
        if (isKey(event, "dialog-close")) dialog.current?.close();
      }}
    >
      <button
        type="button"
        className="ghost icon lightbox-close"
        aria-label="Close"
        onClick={() => dialog.current?.close()}
      >
        <X size={18} />
      </button>
      <img src={url} alt={alt} />
    </dialog>,
    document.body,
  );
}
