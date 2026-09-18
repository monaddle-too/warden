import { useEffect, useRef, useState, type FormEvent } from "react";
import { chatCheckpoints, rewindChat } from "../api";
import { senderLabel } from "../export";
import {
  WHAT_LABELS,
  canRewind,
  excerpt,
  rewindOutcome,
  rewindTargets,
  whatAllowed,
  type RewindResult,
  type RewindTarget,
  type RewindWhat,
} from "../rewind";
import type { Chat } from "../types";

/* The rewind chooser (Claude Code's Esc-Esc): pick one of the chat's user
   messages and what to take back to before it — the workspace (from the
   checkpoint the runner took before that message), the conversation (the
   transcript is cut there and the agent's session forgets the rest), or
   both. The service refuses while the agent runs; the dialog says so
   instead of failing. */
export function RewindDialog({
  chat,
  initial,
  onClose,
}: {
  chat: Chat;
  /* The message to start on; the last user message when unset. */
  initial?: string;
  onClose: (result?: RewindResult) => void;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const [checkpoints, setCheckpoints] = useState<Set<string>>();
  const [selected, setSelected] = useState(initial || "");
  const [what, setWhat] = useState<RewindWhat>("both");
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  // The outcome, with the message's text as it was: the transcript is cut
  // by the rewind, so the target cannot be looked up afterwards.
  const [result, setResult] = useState<{ done: RewindResult; text: string }>();
  useEffect(() => {
    dialog.current?.showModal();
    // The chosen message in view: the list is scrolled to it once.
    dialog.current
      ?.querySelector(".rewind-target.selected")
      ?.scrollIntoView({ block: "nearest" });
  }, []);
  useEffect(() => {
    let cancelled = false;
    chatCheckpoints(chat.id)
      .then((list) => {
        if (!cancelled) setCheckpoints(new Set(list.map((c) => c.id)));
      })
      .catch((e) => {
        if (!cancelled) {
          setCheckpoints(new Set());
          setError(String(e));
        }
      });
    return () => {
      cancelled = true;
    };
  }, [chat.id]);
  const targets = rewindTargets(chat.conversation.entries, checkpoints || []);
  const target: RewindTarget | undefined =
    targets.find((t) => t.entry.id === selected) || targets[targets.length - 1];
  const allowed = canRewind(chat);
  const possible = !!target && whatAllowed(what, target);
  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!target || busy || !allowed || !possible) return;
    setBusy(true);
    setError("");
    try {
      const text = target.entry.text;
      const done = await rewindChat(chat.id, target.entry.id, what);
      setResult({ done, text });
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  const close = () => dialog.current?.close();
  return (
    <dialog
      ref={dialog}
      className="modal rewind-dialog"
      aria-labelledby="rewind-title"
      onClose={() => onClose(result?.done)}
      onKeyDown={(event) => {
        if (event.key === "Escape") {
          event.stopPropagation();
          close();
        }
      }}
    >
      {result ? (
        <div>
          <h2 id="rewind-title">Rewound</h2>
          <p>
            Back to before “{excerpt(result.text)}” (
            {WHAT_LABELS[result.done.what].label.toLowerCase()}).
          </p>
          {rewindOutcome(result.done) && (
            <p className="muted">{rewindOutcome(result.done)}</p>
          )}
          <div className="button-row">
            <button type="button" className="primary" onClick={close}>
              Close
            </button>
          </div>
        </div>
      ) : (
        <form onSubmit={submit}>
          <h2 id="rewind-title">Rewind</h2>
          <p className="muted">
            Go back to before one of your messages. A checkpoint of the
            workspace is taken before each message the agent gets.
          </p>
          {!targets.length && (
            <p className="muted">This chat has no messages to rewind to.</p>
          )}
          <ul className="rewind-targets" role="listbox" aria-label="Messages">
            {targets.map((t) => (
              <li key={t.entry.id}>
                <label
                  className={`rewind-target${target?.entry.id === t.entry.id ? " selected" : ""}`}
                >
                  <input
                    type="radio"
                    name="target"
                    value={t.entry.id}
                    checked={target?.entry.id === t.entry.id}
                    onChange={() => setSelected(t.entry.id)}
                  />
                  <span className="rewind-number">{t.number}</span>
                  <span className="rewind-text">
                    <span>{excerpt(t.entry.text)}</span>
                    <small>
                      {senderLabel(t.entry.sender)}
                      {checkpoints && !t.checkpoint
                        ? " · no checkpoint (conversation only)"
                        : ""}
                    </small>
                  </span>
                </label>
              </li>
            ))}
          </ul>
          <fieldset className="export-format">
            <legend>What to rewind</legend>
            {(["both", "conversation", "code"] as RewindWhat[]).map((w) => (
              <label className="export-option" key={w}>
                <input
                  type="radio"
                  name="what"
                  value={w}
                  checked={what === w}
                  disabled={!!target && !whatAllowed(w, target)}
                  onChange={() => setWhat(w)}
                />
                <span>
                  {WHAT_LABELS[w].label}
                  <small>{WHAT_LABELS[w].hint}</small>
                </span>
              </label>
            ))}
          </fieldset>
          {!allowed && (
            <p className="muted">
              Wait for the agent to finish, or stop it, before rewinding.
            </p>
          )}
          {error && (
            <p className="error" role="alert">
              {error}
            </p>
          )}
          <div className="button-row">
            <button type="button" onClick={close} disabled={busy}>
              Cancel
            </button>
            <button
              className="primary"
              disabled={busy || !allowed || !possible || !target}
            >
              {busy ? "Rewinding…" : "Rewind"}
            </button>
          </div>
        </form>
      )}
    </dialog>
  );
}
