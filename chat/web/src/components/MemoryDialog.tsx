import { useEffect, useRef, useState, type FormEvent } from "react";
import { api } from "../api";
import { formatSize } from "../attachments";
import { memoryDescription, memoryPathError } from "../memory";
import type { MemoryFile } from "../types";

/* The editor behind the workspace panel's Memory section: one memory file
   (CLAUDE.md, a rule, an auto-memory file) as a textarea, written back
   through chats/{id}/memory/write, which leaves a notice in the transcript
   naming who edited it. A file that is not there yet is created by the
   same route. */
export function MemoryDialog({
  chatID,
  file,
  onSaved,
  onClose,
}: {
  chatID: string;
  /* The file to edit; text is "" for one being created. */
  file: Pick<MemoryFile, "scope" | "path" | "text" | "truncated"> & {
    size?: number;
  };
  onSaved: () => void;
  onClose: () => void;
}) {
  const [text, setText] = useState(file.text);
  const [error, setError] = useState(memoryPathError(file.scope, file.path));
  const [busy, setBusy] = useState(false);
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    dialog.current?.showModal();
  }, []);
  const creating = file.size === undefined;
  const changed = creating || text !== file.text;
  async function save(event: FormEvent) {
    event.preventDefault();
    if (file.truncated) return;
    setBusy(true);
    setError("");
    try {
      await api(`chats/${encodeURIComponent(chatID)}/memory/write`, {
        scope: file.scope,
        path: file.path,
        text,
      });
      onSaved();
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
      className="modal memory-dialog"
      aria-labelledby="memory-title"
      onClose={onClose}
      onKeyDown={(event) => {
        if (event.key === "Escape") dialog.current?.close();
      }}
    >
      <form onSubmit={save}>
        <h2 id="memory-title">
          <code>{file.path}</code>
        </h2>
        <p className="muted">
          {file.scope === "auto" ? "Auto-memory · " : ""}
          {memoryDescription(file)}
          {creating
            ? " · new file"
            : file.size !== undefined
              ? ` · ${formatSize(file.size)}`
              : ""}
          . Saving writes the file in the workspace and notes it in the chat.
        </p>
        {file.truncated && (
          <p className="error" role="alert">
            This file is larger than the view shows; saving from here would cut
            it. Edit it from the chat instead.
          </p>
        )}
        <textarea
          aria-label={`Contents of ${file.path}`}
          value={text}
          rows={18}
          spellCheck={false}
          disabled={busy || !!file.truncated}
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
          <button
            type="button"
            onClick={() => dialog.current?.close()}
            disabled={busy}
          >
            Cancel
          </button>
          <button
            className="primary"
            disabled={
              !changed ||
              busy ||
              !!file.truncated ||
              !!memoryPathError(file.scope, file.path)
            }
          >
            {busy ? "Saving…" : creating ? "Create" : "Save"}
          </button>
        </div>
      </form>
    </dialog>
  );
}
