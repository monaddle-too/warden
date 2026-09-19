import { useCallback, useEffect, useRef, useState } from "react";
import { isKey } from "../shortcuts";
import { RefreshCw } from "lucide-react";
import { sessionChanges } from "../api";
import { parseDiff } from "../diff";
import { changesSummary, splitDiff, type SessionChanges } from "../rewind";
import { DiffView } from "./DiffView";

/* The session diff: what changed in the workspace since the chat's first
   checkpoint (or its last code rewind), one DiffView per file, each
   folded behind its path and counts. The diff is git's, taken inside the
   sandbox; the text is rendered as text only. */
export function SessionDiff({
  chatID,
  onClose,
}: {
  chatID: string;
  onClose: () => void;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [changes, setChanges] = useState<SessionChanges>();
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(true);
  const load = useCallback(() => {
    setLoading(true);
    setError("");
    sessionChanges(chatID)
      .then(setChanges)
      .catch((e) => setError(String(e)))
      .finally(() => setLoading(false));
  }, [chatID]);
  useEffect(() => {
    dialog.current?.showModal();
    load();
  }, [load]);
  const files = changes ? splitDiff(changes.diff) : new Map<string, string>();
  const summary = changes ? changesSummary(changes.files) : undefined;
  return (
    <dialog
      ref={dialog}
      className="modal session-diff"
      aria-labelledby="changes-title"
      onClose={onClose}
      onKeyDown={(event) => {
        if (isKey(event, "dialog-close")) {
          event.stopPropagation();
          dialog.current?.close();
        }
      }}
    >
      <header className="session-diff-head">
        <h2 id="changes-title">Changes</h2>
        <button
          type="button"
          className="ghost icon"
          aria-label="Refresh"
          title="Refresh"
          disabled={loading}
          onClick={load}
        >
          <RefreshCw size={15} />
        </button>
      </header>
      <p className="muted">
        What changed in the workspace since this chat began, or since its last
        code rewind.
        {summary &&
          ` ${summary.files === 1 ? "1 file" : `${summary.files} files`}, `}
        {summary && (
          <>
            <b className="diff-plus">+{summary.added}</b>{" "}
            <b className="diff-minus">−{summary.removed}</b>
          </>
        )}
        {changes?.truncated &&
          " · the diff was cut at 2 MiB; counts are complete"}
      </p>
      {error && (
        <p className="error" role="alert">
          {error}
        </p>
      )}
      {loading && !changes && <p className="muted">Comparing…</p>}
      {changes && !changes.files.length && <p className="muted">No changes.</p>}
      <div className="session-diff-files">
        {changes?.files.map((f, i) => {
          const text = files.get(f.path);
          return (
            <details key={f.path} open={changes.files.length <= 3 || i === 0}>
              <summary>
                <span className="diff-path">{f.path}</span>
                <span className="diff-count">
                  {f.binary ? (
                    <span className="muted">binary</span>
                  ) : (
                    <>
                      <b className="diff-plus">+{f.added}</b>{" "}
                      <b className="diff-minus">−{f.removed}</b>
                    </>
                  )}
                </span>
              </summary>
              {text ? (
                <DiffView segments={parseDiff(text)} />
              ) : (
                <p className="muted">
                  {f.binary
                    ? "Binary file."
                    : "Not in the diff (cut at the size limit)."}
                </p>
              )}
            </details>
          );
        })}
      </div>
      <div className="button-row">
        <button
          type="button"
          className="primary"
          onClick={() => dialog.current?.close()}
        >
          Close
        </button>
      </div>
    </dialog>
  );
}
