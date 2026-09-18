import { useEffect, useRef } from "react";
import { Keyboard } from "lucide-react";
import { groupedShortcuts, isKey, keysLabel } from "../shortcuts";

/* The `?` overlay (docs/claude-parity.md, R2.20): every keyboard shortcut
   of the web chat by area, from the same table the key handlers match
   against (shortcuts.ts), with the platform's modifier names. Opened by
   `?` outside an input and by "Keyboard shortcuts" in the chat menu. */
export function ShortcutsDialog({ onClose }: { onClose: () => void }) {
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    dialog.current?.showModal();
  }, []);
  return (
    <dialog
      ref={dialog}
      className="modal shortcuts-dialog"
      aria-labelledby="shortcuts-title"
      onClose={onClose}
      onKeyDown={(event) => {
        if (isKey(event, "dialog-close")) {
          event.preventDefault();
          dialog.current?.close();
        }
      }}
    >
      <h2 id="shortcuts-title">
        <Keyboard size={18} aria-hidden="true" />
        Keyboard shortcuts
      </h2>
      <div className="shortcuts-areas">
        {groupedShortcuts().map((area) => (
          <section key={area.id} aria-labelledby={`shortcuts-${area.id}`}>
            <h3 id={`shortcuts-${area.id}`}>{area.title}</h3>
            <table>
              <tbody>
                {area.rows.map((s) => (
                  <tr key={s.id}>
                    <th scope="row">
                      {keysLabel(s)
                        .split(/( or )/)
                        .map((part, i) =>
                          part === " or " ? (
                            <span key={i} className="muted">
                              {" "}
                              or{" "}
                            </span>
                          ) : (
                            <kbd key={i}>{part}</kbd>
                          ),
                        )}
                    </th>
                    <td>
                      {s.what}
                      {s.when && <span className="muted"> — {s.when}</span>}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </section>
        ))}
      </div>
      <div className="button-row">
        <button type="button" onClick={() => dialog.current?.close()}>
          Close
        </button>
      </div>
    </dialog>
  );
}
