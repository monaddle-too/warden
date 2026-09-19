import { useEffect, useRef, useState, type FormEvent } from "react";
import { isKey } from "../shortcuts";
import {
  exportMime,
  exportName,
  exportText,
  plural,
  saveFile,
  type ExportFormat,
} from "../export";
import type { Chat } from "../types";

/* "Export…" from the chat menu: the transcript as a markdown or JSON file,
   built here from the state the service already streamed, so nothing is
   asked of the service and an archived chat exports like a live one. */
export function ExportDialog({
  chat,
  onClose,
}: {
  chat: Chat;
  onClose: () => void;
}) {
  const [format, setFormat] = useState<ExportFormat>("markdown");
  const [activity, setActivity] = useState(false);
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    dialog.current?.showModal();
  }, []);
  const entries = chat.conversation.entries;
  const messages = entries.filter(
    (e) => e.role === "user" || e.role === "assistant",
  ).length;
  const steps = entries.filter(
    (e) => e.role === "activity" || e.role === "thinking",
  ).length;
  function save(event: FormEvent) {
    event.preventDefault();
    const at = new Date();
    const text = exportText(chat, { format, activity }, at);
    saveFile(
      exportName(chat.title, format, at),
      new Blob([text], { type: exportMime(format) }),
    );
    dialog.current?.close();
  }
  return (
    <dialog
      ref={dialog}
      className="modal export-dialog"
      aria-labelledby="export-title"
      onClose={onClose}
      // Explicit as well as the dialog's own cancel handling, which some
      // synthetic key events do not reach.
      onKeyDown={(event) => {
        if (isKey(event, "dialog-close")) dialog.current?.close();
      }}
    >
      <form onSubmit={save}>
        <h2 id="export-title">Export chat</h2>
        <p className="muted">
          {plural(messages, "message")}
          {steps ? ` and ${plural(steps, "agent step")}` : ""} from “
          {chat.title}”, saved as a file by your browser.
        </p>
        <fieldset className="export-format">
          <legend>Format</legend>
          <label className="export-option">
            <input
              type="radio"
              name="format"
              value="markdown"
              checked={format === "markdown"}
              onChange={() => setFormat("markdown")}
            />
            <span>
              Markdown
              <small>
                Messages as they were written, for notes or a document
              </small>
            </span>
          </label>
          <label className="export-option">
            <input
              type="radio"
              name="format"
              value="json"
              checked={format === "json"}
              onChange={() => setFormat("json")}
            />
            <span>
              JSON
              <small>Every field of every entry, for another tool</small>
            </span>
          </label>
        </fieldset>
        <label className="export-option">
          <input
            type="checkbox"
            checked={activity}
            disabled={!steps}
            onChange={(e) => setActivity(e.target.checked)}
          />
          <span>
            Include agent activity
            <small>
              {steps
                ? `The ${plural(steps, "tool step")} and their output`
                : "This chat has no tool steps"}
            </small>
          </span>
        </label>
        <div className="button-row">
          <button type="button" onClick={() => dialog.current?.close()}>
            Cancel
          </button>
          <button className="primary" disabled={!entries.length}>
            Download
          </button>
        </div>
      </form>
    </dialog>
  );
}
