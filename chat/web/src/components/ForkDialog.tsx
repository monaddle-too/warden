import { useEffect, useRef, useState, type FormEvent } from "react";
import { forkChat, type ForkResult } from "../api";
import { senderLabel } from "../export";
import { canRewind, excerpt, rewindTargets } from "../rewind";
import type { Chat } from "../types";

/* Fork a chat: a sibling chat on the same workspace whose transcript is
   this one's up to a chosen message (or the whole of it), continuing from
   a copy of the agent's session where the agent can copy one (Claude),
   so both chats can go on from there. The service refuses while the agent
   runs; the dialog says so. */
export function ForkDialog({
  chat,
  initial,
  onClose,
}: {
  chat: Chat;
  /* The message to cut before; the whole chat when unset. */
  initial?: string;
  /* `open` asks the shell to show the fork. */
  onClose: (result?: ForkResult, open?: boolean) => void;
}) {
  const dialog = useRef<HTMLDialogElement>(null);
  const openFork = useRef(false);
  // "" is the whole chat.
  const [selected, setSelected] = useState(initial || "");
  // A copy of the workspace too: a new workspace cloned from this one.
  const [copy, setCopy] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [result, setResult] = useState<ForkResult>();
  useEffect(() => {
    dialog.current?.showModal();
    dialog.current
      ?.querySelector(".rewind-target.selected")
      ?.scrollIntoView({ block: "nearest" });
  }, []);
  const targets = rewindTargets(chat.conversation.entries);
  const target = targets.find((t) => t.entry.id === selected);
  const allowed = canRewind(chat);
  async function submit(event: FormEvent) {
    event.preventDefault();
    if (busy || !allowed) return;
    setBusy(true);
    setError("");
    try {
      setResult(await forkChat(chat.id, target?.entry.id, copy));
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
      aria-labelledby="fork-title"
      onClose={() => onClose(result, openFork.current)}
      onKeyDown={(event) => {
        if (event.key === "Escape") {
          event.stopPropagation();
          close();
        }
      }}
    >
      {result ? (
        <div>
          <h2 id="fork-title">Forked</h2>
          <p>
            “{result.title}” continues from{" "}
            {target ? `before “${excerpt(target.entry.text)}”` : "here"}
            {result.workspace === "copied"
              ? " on a copy of this workspace"
              : ""}
            .
          </p>
          <p className="muted">{forkOutcome(result)}</p>
          {result.workspace === "copied" && (
            <p className="muted">
              The copy has this workspace's files as they were just now.
              Shared documents, repositories and network access stay with
              this workspace; share them with the copy from its panel.
            </p>
          )}
          <div className="button-row">
            <button type="button" onClick={close}>
              Stay here
            </button>
            <button
              type="button"
              className="primary"
              onClick={() => {
                openFork.current = true;
                close();
              }}
            >
              Open the fork
            </button>
          </div>
        </div>
      ) : (
        <form onSubmit={submit}>
          <h2 id="fork-title">Fork this chat</h2>
          <p className="muted">
            A new chat on the same workspace takes a copy of this
            conversation, so both can continue their own way. Choose where the
            copy stops.
          </p>
          <ul className="rewind-targets" role="listbox" aria-label="Where the copy stops">
            <li>
              <label className={`rewind-target${!target ? " selected" : ""}`}>
                <input
                  type="radio"
                  name="target"
                  value=""
                  checked={!target}
                  onChange={() => setSelected("")}
                />
                <span className="rewind-number">all</span>
                <span className="rewind-text">
                  <span>The whole conversation</span>
                  <small>
                    {targets.length
                      ? `${targets.length} message${targets.length === 1 ? "" : "s"} so far`
                      : "nothing yet; the fork starts empty on this workspace"}
                  </small>
                </span>
              </label>
            </li>
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
                    <span>Before “{excerpt(t.entry.text)}”</span>
                    <small>{senderLabel(t.entry.sender)}</small>
                  </span>
                </label>
              </li>
            ))}
          </ul>
          <label className="fork-copy">
            <input
              type="checkbox"
              checked={copy}
              onChange={(event) => setCopy(event.target.checked)}
            />
            <span>
              Copy the workspace
              <small>
                The fork gets its own workspace, cloned from this one's files
                as they are now (a stopped workspace is fine; a running one
                pauses briefly). Shared documents, repositories and network
                access are not copied.
              </small>
            </span>
          </label>
          {!allowed && (
            <p className="muted">
              Wait for the agent to finish, or stop it, before forking.
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
            <button className="primary" disabled={busy || !allowed}>
              {busy
                ? copy
                  ? "Copying the workspace…"
                  : "Forking…"
                : copy
                  ? "Fork with a copy"
                  : "Fork"}
            </button>
          </div>
        </form>
      )}
    </dialog>
  );
}

/* The line under the result: how the fork's agent session follows. */
export function forkOutcome(result: Pick<ForkResult, "session">): string {
  switch (result.session) {
    case "forked":
      return "The agent continues from a copy of its session; this chat keeps its own.";
    case "fresh":
      return "The agent's session cannot be copied: the fork's first message starts a new one with the conversation so far as context.";
    default:
      return "The fork starts its own agent session.";
  }
}
