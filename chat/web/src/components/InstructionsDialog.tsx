import { useEffect, useRef, useState, type FormEvent } from "react";
import { api } from "../api";
import type { Instructions } from "../types";

/* "Instructions…" in the sidebar: the person's own standing instructions
   for the agent (parity item 13), markdown in a textarea. They reach the
   agent in every chat the person takes part in: appended to the system
   prompt when a session starts, or once as a prefix on their next message
   into a session that is already running. The text is the person's alone;
   the service keeps it by principal. */
export function InstructionsDialog({ onClose }: { onClose: () => void }) {
  const [text, setText] = useState("");
  const [loaded, setLoaded] = useState<Instructions>();
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    dialog.current?.showModal();
    let cancelled = false;
    api<Instructions>("me/instructions")
      .then((v) => {
        if (cancelled) return;
        setLoaded(v);
        setText(v.text);
      })
      .catch((e) => !cancelled && setError(String(e)));
    return () => {
      cancelled = true;
    };
  }, []);
  const changed = loaded !== undefined && text !== loaded.text;
  async function save(event: FormEvent) {
    event.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api("me/instructions", { text });
      dialog.current?.close();
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  return (
    <dialog
      ref={dialog}
      className="modal instructions-dialog"
      aria-labelledby="instructions-title"
      onClose={onClose}
      onKeyDown={(event) => {
        if (event.key === "Escape") dialog.current?.close();
      }}
    >
      <form onSubmit={save}>
        <h2 id="instructions-title">Your instructions</h2>
        <p className="muted">
          Standing instructions the agent gets in every chat you take part in,
          as “Instructions from you” after Warden’s own. Markdown; a new chat
          starts with them, a running one gets them with your next message.
        </p>
        <textarea
          aria-label="Instructions"
          value={text}
          rows={12}
          placeholder={
            loaded === undefined
              ? "Loading…"
              : "Always answer in English. Prefer small commits. Ask before deleting files."
          }
          disabled={loaded === undefined || busy}
          maxLength={16 * 1024}
          onChange={(e) => setText(e.target.value)}
          onKeyDown={(e) => {
            if ((e.metaKey || e.ctrlKey) && e.key === "Enter")
              e.currentTarget.form?.requestSubmit();
          }}
        />
        {error && (
          <p className="error" role="alert">
            {error}
          </p>
        )}
        <div className="button-row">
          {loaded?.updatedAt ? (
            <small className="muted instructions-updated">
              Saved {new Date(loaded.updatedAt * 1000).toLocaleString()}
            </small>
          ) : null}
          <button
            type="button"
            onClick={() => dialog.current?.close()}
            disabled={busy}
          >
            Cancel
          </button>
          <button className="primary" disabled={!changed || busy}>
            {busy ? "Saving…" : text.trim() ? "Save" : "Remove"}
          </button>
        </div>
      </form>
    </dialog>
  );
}
