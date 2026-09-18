import { useEffect, useRef, useState } from "react";
import { api } from "../api";
import { decidedBy, historySummary, newestFirst } from "../rules";
import type { Chat, PermissionEvent } from "../types";

/* "Permissions…" from the chat menu (parity round 2 B): how each of the
   chat's tool asks was decided — allowed by auto mode, answered by a rule
   (which, and whether the chat's or the workspace's), or answered on a
   card by someone (with the rule an "Allow always" made, or a denial's
   message) — newest first, from chats/{id}/permissions (the last 200). */
export function PermissionHistory({
  chat,
  onClose,
}: {
  chat: Chat;
  onClose: () => void;
}) {
  const [events, setEvents] = useState<PermissionEvent[]>();
  const [error, setError] = useState("");
  const dialog = useRef<HTMLDialogElement>(null);
  useEffect(() => {
    dialog.current?.showModal();
    let cancelled = false;
    api<{ events: PermissionEvent[] }>(
      `chats/${encodeURIComponent(chat.id)}/permissions`,
    )
      .then((r) => !cancelled && setEvents(r.events))
      .catch((e) => !cancelled && setError(String(e)));
    return () => {
      cancelled = true;
    };
  }, [chat.id]);
  const list = events ? newestFirst(events) : [];
  return (
    <dialog
      ref={dialog}
      className="modal permission-history"
      aria-labelledby="permission-history-title"
      onClose={onClose}
      onKeyDown={(event) => {
        if (event.key === "Escape") dialog.current?.close();
      }}
    >
      <h2 id="permission-history-title">Permissions in “{chat.title}”</h2>
      <p className="muted">
        {events === undefined ? "Loading…" : historySummary(events)}
      </p>
      {error && (
        <p className="error" role="alert">
          {error}
        </p>
      )}
      <ul className="permission-list">
        {list.map((ev) => (
          <li key={ev.id} className={`permission-${ev.decision}`}>
            <span className="permission-decision">{ev.decision}</span>
            <span className="permission-what">
              <span className="permission-tool">{ev.tool}</span>
              <code title={ev.summary}>{ev.summary}</code>
            </span>
            <small title={new Date(ev.at * 1000).toLocaleString()}>
              {decidedBy(ev)} ·{" "}
              {new Date(ev.at * 1000).toLocaleTimeString([], {
                hour: "2-digit",
                minute: "2-digit",
              })}
            </small>
          </li>
        ))}
      </ul>
      <div className="button-row">
        <button type="button" onClick={() => dialog.current?.close()}>
          Close
        </button>
      </div>
    </dialog>
  );
}
