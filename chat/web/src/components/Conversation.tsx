// Panta's transcript/composer layout adapted to Warden's standalone API.
import {
  Fragment,
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type DragEvent,
  type FormEvent,
  type ReactNode,
} from "react";
import { ArrowDown, ArrowUp, Bot, Paperclip, Square } from "lucide-react";
import {
  canResend,
  messageAttempt,
  readLocalAttempt,
  resendAttempt,
  type Attempt,
} from "../drafts";
import { api, me, newID, downloadFile, uploadAttachment } from "../api";
import {
  attachmentError,
  hasFiles,
  pastedName,
  transferFiles,
} from "../attachments";
import {
  groupEntries,
  newSince,
  readSeen,
  unreadEntry,
  unreadIndex,
} from "../transcript";
import type { Chat, Entry } from "../types";
import { ComposerAttachments, type Pending } from "./Attachments";
import { ActivityGroup, EntryView } from "./EntryView";
import { ApprovalCard } from "./Approvals";
import { FindBar, isFindKey, type FindRequest } from "./FindBar";
import { ModelSelect } from "./ModelSelect";

/* An agent request that the owner answers from the transcript: document
   access, document creation, a pull request proposal. */
export type RequestCard = {
  id: string;
  icon: ReactNode;
  title: string;
  detail?: string;
  note?: ReactNode;
  actions: { label: string; primary?: boolean; onClick: () => void }[];
};
function draft(key: string) {
  try {
    return localStorage.getItem(key) || "";
  } catch {
    return "";
  }
}
function readSeenLocal(key: string) {
  try {
    return readSeen(localStorage, key);
  } catch {
    return undefined;
  }
}
const reducedMotion = () =>
  window.matchMedia("(prefers-reduced-motion: reduce)").matches;
/* Keys that scroll a scroll box when it, or the page, has the focus. */
const SCROLL_KEYS = new Set([
  "ArrowUp",
  "ArrowDown",
  "PageUp",
  "PageDown",
  "Home",
  "End",
  " ",
]);
/* Whether a key pressed here goes into text rather than to the page. */
const editable = (target: EventTarget | null) =>
  target instanceof HTMLElement &&
  (target.isContentEditable || target.matches("input, textarea, select"));
export function Conversation({
  chat,
  live,
  requests = [],
  find,
  onModel,
}: {
  chat: Chat;
  live: boolean;
  requests?: RequestCard[];
  /* Opens the find bar: from the header's button, or from the palette
     with the entry to land on. A request for another chat is ignored, so
     one made before a switch does not follow the reader. */
  find?: FindRequest;
  onModel: (model: string) => Promise<unknown>;
}) {
  const key = "warden-draft:" + location.origin + ":" + chat.id;
  const [text, setText] = useState(() => draft(key));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  // Files chosen for the next message. Each uploads as soon as it is added
  // and the message names the uploaded IDs; a chip that failed stays until
  // removed so the reason is visible.
  const [pending, setPending] = useState<Pending[]>([]);
  const [dragging, setDragging] = useState(false);
  const nextKey = useRef(0);
  const picker = useRef<HTMLInputElement>(null);
  const input = useRef<HTMLTextAreaElement>(null);
  const uploading = pending.some((p) => p.status === "uploading");
  const ready = pending.filter((p) => p.status === "ready");
  function addFiles(files: File[], pasted = false) {
    if (!files.length || chat.archived) return;
    setError("");
    let count = pending.length;
    for (const original of files) {
      const file = pasted
        ? new File([original], pastedName(original, new Date()), {
            type: original.type,
          })
        : original;
      const refused = attachmentError(file, count);
      if (refused) {
        setError(refused);
        continue;
      }
      count++;
      const key = nextKey.current++;
      setPending((list) => [...list, { key, file, status: "uploading" }]);
      void uploadAttachment(chat.id, file).then(
        (attachment) =>
          setPending((list) =>
            list.map((p) =>
              p.key === key ? { ...p, status: "ready", attachment } : p,
            ),
          ),
        (e: unknown) =>
          setPending((list) =>
            list.map((p) =>
              p.key === key
                ? {
                    ...p,
                    status: "failed",
                    error: e instanceof Error ? e.message : String(e),
                  }
                : p,
            ),
          ),
      );
    }
  }
  // Forgetting an unsent upload; one reused from a sent message stays.
  const forget = useCallback(
    (item: Pending) => {
      if (!item.attachment || item.reused) return;
      void api(
        `chats/${chat.id}/attachments/${item.attachment.id}/remove`,
        {},
      ).catch(() => {
        /* an unsent upload is forgotten by the service after a day anyway */
      });
    },
    [chat.id],
  );
  function removePending(key: number) {
    const item = pending.find((p) => p.key === key);
    setPending((list) => list.filter((p) => p.key !== key));
    if (item) forget(item);
  }
  function dragOver(event: DragEvent) {
    if (!hasFiles(event.dataTransfer)) return;
    event.preventDefault();
    if (!dragging) setDragging(true);
  }
  function drop(event: DragEvent) {
    if (!hasFiles(event.dataTransfer)) return;
    event.preventDefault();
    setDragging(false);
    addFiles(transferFiles(event.dataTransfer));
  }
  // Stable per chat so memoised entries do not re-render on every streamed
  // chunk of another message.
  const onFile = useCallback(
    (href: string) => {
      void downloadFile(chat.id, href).catch((e) => setError(String(e)));
    },
    [chat.id],
  );
  const scroll = useRef<HTMLDivElement>(null);
  const transcript = useRef<HTMLDivElement>(null);
  const entries = chat.conversation.entries;
  // Following: the transcript keeps its end in view as it grows. Once the
  // reader scrolls up, `away` holds the ID of the last entry they had in
  // view, so the jump button can say how many messages arrived since; the
  // ref is the same fact for callbacks that cannot wait for a render.
  const follow = useRef(true);
  const [away, setAway] = useState<string | null>(null);
  const setFollow = useCallback((value: boolean, lastID = "") => {
    follow.current = value;
    setAway(value ? null : lastID);
  }, []);
  // A smooth jump passes through positions that are not near the end;
  // those scroll events must not count as leaving again. The reader taking
  // over does: a wheel, a touch, a scrolling key or a press on the
  // scrollbar clears the guard before the scroll it causes, and `scrollend`
  // (where the browser has it) settles whatever else interrupted the jump.
  const jumping = useRef(false);
  const lastID = () => entries[entries.length - 1]?.id ?? "";
  function track() {
    const el = scroll.current!;
    const near = el.scrollHeight - el.scrollTop - el.clientHeight < 100;
    if (near) jumping.current = false;
    else if (jumping.current) return;
    if (near !== follow.current) setFollow(near, lastID());
  }
  useEffect(() => {
    // A space or arrow typed into the composer during a jump moves the
    // caret, not the transcript, so it is not the reader taking over.
    const onKey = (event: KeyboardEvent) => {
      if (
        jumping.current &&
        SCROLL_KEYS.has(event.key) &&
        !editable(event.target)
      )
        jumping.current = false;
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, []);
  function jump() {
    const el = scroll.current;
    if (!el) return;
    jumping.current = true;
    setFollow(true);
    el.scrollTo({
      top: el.scrollHeight,
      behavior: reducedMotion() ? "auto" : "smooth",
    });
  }
  // The unread divider sits before the first entry the reader has not
  // seen: what they saw is the last entry that was in view while they
  // followed the transcript with the tab visible, remembered per chat.
  // The entry the divider goes before is fixed once, when the chat opens
  // (the component is keyed by chat), so it stays put while reading and
  // never appears above what arrives during the visit.
  const seenKey = "warden-seen:" + location.origin + ":" + chat.id;
  const [unreadID] = useState(() =>
    unreadEntry(entries, readSeenLocal(seenKey)),
  );
  const unread = unreadIndex(entries, unreadID);
  // `wake` only re-runs the effect when the tab comes back (the state it
  // reads is the document's, taken live: a page that loads hidden may
  // become visible before any listener is attached).
  const [wake, setWake] = useState(0);
  useEffect(() => {
    const onChange = () => setWake((n) => n + 1);
    document.addEventListener("visibilitychange", onChange);
    window.addEventListener("focus", onChange);
    return () => {
      document.removeEventListener("visibilitychange", onChange);
      window.removeEventListener("focus", onChange);
    };
  }, []);
  // The ref, not `away`, decides: the mount layout effect below may stop
  // following (landing at the divider) in the same commit, and this effect
  // then runs with the `away` it closed over, still null, before the
  // re-render that state scheduled. `away` stays a dependency so the mark
  // advances again once the reader is back at the end.
  const remembered = useRef("");
  useEffect(() => {
    const last = entries[entries.length - 1];
    if (
      !follow.current ||
      document.visibilityState === "hidden" ||
      !last ||
      remembered.current === last.id
    )
      return;
    remembered.current = last.id;
    try {
      localStorage.setItem(
        seenKey,
        JSON.stringify({ id: last.id, at: last.createdAt }),
      );
    } catch {}
  }, [entries, away, wake, seenKey]);
  const [finding, setFinding] = useState(() =>
    find?.chatID === chat.id ? find : undefined,
  );
  useEffect(() => {
    if (find?.chatID === chat.id) setFinding(find);
  }, [find, chat.id]);
  // ⌘F / Ctrl+F while the transcript or composer has focus opens the find
  // bar (an open bar handles the key itself); elsewhere the browser's own
  // find keeps working.
  useEffect(() => {
    const onKey = (event: KeyboardEvent) => {
      if (!isFindKey(event)) return;
      const target = event.target as Element | null;
      const here =
        target === document.body ||
        (target instanceof Element &&
          !!target.closest(".conversation") &&
          !target.closest("dialog"));
      if (!here) return;
      event.preventDefault();
      setFinding((open) => open ?? { chatID: chat.id, query: "" });
    };
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [chat.id]);
  // The composer's current contents, for the transcript's edit action,
  // which is a stable callback and cannot close over state.
  const current = useRef({ text, pending });
  current.current = { text, pending };
  // Retry sends the entry again as a new message; the transcript's buttons
  // are disabled while a send would be refused, so this only guards
  // against a double click.
  const sending = useRef(false);
  const retry = useCallback(
    async (entry: Entry) => {
      if (sending.current) return;
      sending.current = true;
      setBusy(true);
      setError("");
      try {
        await api(`chats/${chat.id}/message`, resendAttempt(entry, newID));
        setFollow(true);
      } catch (e) {
        setError(String(e));
      } finally {
        sending.current = false;
        setBusy(false);
      }
    },
    [chat.id, setFollow],
  );
  // Edit puts the entry's text and uploads into the composer, asking first
  // when that would replace something already there.
  const edit = useCallback(
    (entry: Entry) => {
      const { text, pending } = current.current;
      const draft = text.trim();
      const replacing =
        (draft !== "" && draft !== entry.text.trim()) ||
        pending.some((p) => !p.reused);
      if (replacing && !window.confirm("Replace your draft with this message?"))
        return;
      for (const item of pending) forget(item);
      setPending(
        (entry.attachments || []).map((attachment) => ({
          key: nextKey.current++,
          status: "ready",
          attachment,
          reused: true,
        })),
      );
      setText(entry.text);
      setError("");
      requestAnimationFrame(() => {
        const el = input.current;
        if (!el) return;
        el.focus();
        el.setSelectionRange(el.value.length, el.value.length);
      });
    },
    [forget],
  );
  const attempted = useRef<Attempt | undefined>(
    readLocalAttempt(key + ":attempt"),
  );
  const running =
    chat.status === "running" ||
    chat.status === "queued" ||
    chat.status === "stopping";
  // Typing: every keystroke reports at most once per 3 s (the server keeps
  // an indicator for 8 s after the last report), so a steady typist stays
  // visible and a pause fades within seconds.
  const lastTyping = useRef(0);
  function reportTyping() {
    const now = Date.now();
    if (now - lastTyping.current < 3000) return;
    lastTyping.current = now;
    void api(`chats/${chat.id}/typing`, {}).catch(() => {
      /* indicator only; failures are invisible */
    });
  }
  const [clock, setClock] = useState(() => Date.now() / 1000);
  const others = (chat.typing || []).filter(
    (t) => t.principalID !== me.principalID && t.until > clock,
  );
  useEffect(() => {
    if (!chat.typing?.length) return;
    const id = setInterval(() => setClock(Date.now() / 1000), 1000);
    return () => clearInterval(id);
  }, [chat.typing]);
  const typingLine =
    others.length === 0
      ? ""
      : others.length === 1
        ? `${others[0].name} is typing…`
        : others.length === 2
          ? `${others[0].name} and ${others[1].name} are typing…`
          : `${others[0].name}, ${others[1].name} and ${others.length - 2} more are typing…`;
  useEffect(() => {
    try {
      if (text) localStorage.setItem(key, text);
      else localStorage.removeItem(key);
    } catch {}
  }, [text, key]);
  // A chat opens at its end, unless the unread stretch is longer than the
  // view: then it opens at the divider, with the jump button showing how
  // much is below.
  const landed = useRef(false);
  useLayoutEffect(() => {
    const el = scroll.current;
    if (!el) return;
    if (follow.current) el.scrollTop = el.scrollHeight;
    if (landed.current) return;
    landed.current = true;
    const divider = el.querySelector(".unread-divider");
    if (!divider || unread < 1) return;
    const top =
      divider.getBoundingClientRect().top - el.getBoundingClientRect().top;
    if (top >= 0) return;
    el.scrollTop += top - 12;
    setFollow(false, entries[unread - 1].id);
  }, [chat, requests.length, unread, entries, setFollow]);
  async function send(event: FormEvent) {
    event.preventDefault();
    const attachments = ready.map((p) => p.attachment!.id);
    if ((!text.trim() && !attachments.length) || busy || uploading) return;
    setBusy(true);
    setError("");
    const message = messageAttempt(
      attempted.current,
      text.trim(),
      newID,
      attachments,
    );
    attempted.current = message;
    try {
      localStorage.setItem(key + ":attempt", JSON.stringify(message));
    } catch {}
    try {
      await api(`chats/${chat.id}/message`, message);
      lastTyping.current = 0;
      setText("");
      setPending([]);
      attempted.current = undefined;
      try {
        localStorage.removeItem(key + ":attempt");
      } catch {}
      setFollow(true);
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  async function stop() {
    setBusy(true);
    try {
      await api(`chats/${chat.id}/stop`, {});
      window.dispatchEvent(new Event("warden-refresh-state"));
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  const fresh = away === null ? 0 : newSince(entries, away);
  return (
    <div className="conversation">
      {finding && (
        <FindBar
          root={transcript}
          scroller={scroll}
          request={finding}
          onClose={() => {
            setFinding(undefined);
            input.current?.focus();
          }}
        />
      )}
      <div className="conversation-body">
        <div
          className="conversation-scroll"
          ref={scroll}
          onScroll={track}
          onScrollEnd={() => {
            jumping.current = false;
            track();
          }}
          onWheel={() => {
            jumping.current = false;
          }}
          onTouchMove={() => {
            jumping.current = false;
          }}
          onPointerDown={(event) => {
            // The scrollbar is the box itself; content has its own target.
            if (event.target === event.currentTarget) jumping.current = false;
          }}
        >
          {!entries.length && (
            <div className="empty">
              <Bot size={30} />
              <h2>Start the conversation</h2>
              <p>
                Give your agent a task. Warden keeps its work in a sandbox and
                controls access to your apps.
              </p>
            </div>
          )}
          <div className="transcript" ref={transcript}>
            {groupEntries(entries, unread).map((item) => {
              const first = "group" in item ? item.group[0] : item.entry;
              return (
                <Fragment key={first.id}>
                  {first.id === unreadID && (
                    <div
                      className="unread-divider"
                      role="separator"
                      aria-label="New messages"
                    />
                  )}
                  {"group" in item ? (
                    <ActivityGroup entries={item.group} />
                  ) : (
                    <EntryView
                      provider={chat.provider}
                      chatID={chat.id}
                      entry={item.entry}
                      onFile={onFile}
                      onEdit={edit}
                      onRetry={retry}
                      actions={!busy && canResend(item.entry, chat, live)}
                    />
                  )}
                </Fragment>
              );
            })}
            {chat.approvals
              .filter((a) => a.state === "pending")
              .map((a) => (
                <ApprovalCard key={a.id} chatID={chat.id} approval={a} />
              ))}
            {requests.map((r) => (
              <section
                key={r.id}
                className="approval-card request-card"
                aria-label="Agent request"
              >
                <div className="approval-head">
                  <span className="approval-icon" aria-hidden="true">
                    {r.icon}
                  </span>
                  <div>
                    <h3>{r.title}</h3>
                    {r.detail && <p>{r.detail}</p>}
                  </div>
                </div>
                {r.note && <p>{r.note}</p>}
                <div className="approval-actions">
                  {r.actions.map((a) => (
                    <button
                      key={a.label}
                      className={a.primary ? "primary" : ""}
                      onClick={a.onClick}
                    >
                      {a.label}
                    </button>
                  ))}
                </div>
              </section>
            ))}
            {chat.error && (
              <div className="error" role="alert">
                {chat.error}
              </div>
            )}
          </div>
        </div>
        {away !== null && (
          <button
            type="button"
            className="jump-bottom"
            aria-label="Jump to the latest message"
            onClick={jump}
          >
            <ArrowDown size={15} />
            {fresh
              ? `${fresh} new message${fresh === 1 ? "" : "s"}`
              : "Jump to latest"}
          </button>
        )}
      </div>
      <form
        className="composer-wrap"
        onSubmit={send}
        onDragOver={dragOver}
        onDragLeave={(event) => {
          if (!event.currentTarget.contains(event.relatedTarget as Node))
            setDragging(false);
        }}
        onDrop={drop}
      >
        {error && (
          <p className="error" role="alert">
            {error}
          </p>
        )}
        <p className="typing-line" aria-live="polite">
          {typingLine && (
            <>
              {typingLine.replace(/…$/, "")}
              <span className="typing-dots">…</span>
            </>
          )}
        </p>
        <div className={`composer${dragging ? " dragging" : ""}`}>
          <ComposerAttachments
            chatID={chat.id}
            items={pending}
            onRemove={removePending}
          />
          <textarea
            ref={input}
            aria-label="Message agent"
            placeholder={
              chat.archived
                ? "This chat is archived"
                : dragging
                  ? "Drop files to attach them"
                  : "Message your agent…"
            }
            value={text}
            onChange={(e) => {
              setText(e.target.value);
              if (e.target.value) reportTyping();
            }}
            onPaste={(e) => {
              const files = transferFiles(e.clipboardData);
              if (!files.length) return;
              e.preventDefault();
              addFiles(files, true);
            }}
            disabled={busy || chat.archived}
            rows={3}
            onKeyDown={(e) => {
              if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
                e.preventDefault();
                e.currentTarget.form?.requestSubmit();
              }
            }}
          />
          <div className="composer-footer">
            <span>
              <span className="composer-model">
                <ModelSelect
                  provider={chat.provider || "codex"}
                  value={chat.model || ""}
                  disabled={running || chat.archived}
                  onChange={(model) => {
                    setError("");
                    void onModel(model).catch((e) => setError(String(e)));
                  }}
                  label="Model for the next turn"
                />
              </span>
              <span className={`status-dot ${chat.status}`} />
              <span>
                {chat.status === "running"
                  ? "Agent is running"
                  : chat.status === "queued"
                    ? "Message queued"
                    : chat.status === "stopping"
                      ? "Stopping…"
                      : chat.archived
                        ? "Archived"
                        : "Agent is idle"}
              </span>
            </span>
            <div>
              <input
                ref={picker}
                type="file"
                multiple
                hidden
                onChange={(e) => {
                  addFiles(Array.from(e.target.files || []));
                  e.target.value = "";
                }}
              />
              <button
                type="button"
                className="ghost icon"
                aria-label="Attach files"
                title="Attach files (or paste, or drop them here)"
                disabled={busy || chat.archived}
                onClick={() => picker.current?.click()}
              >
                <Paperclip size={16} />
              </button>
              {running && (
                <button
                  type="button"
                  disabled={busy || chat.status === "stopping"}
                  onClick={stop}
                >
                  <Square size={12} fill="currentColor" />
                  Stop
                </button>
              )}
              <button
                className="send-button"
                aria-label="Send message"
                disabled={
                  (!text.trim() && !ready.length) ||
                  uploading ||
                  busy ||
                  !live ||
                  chat.archived ||
                  chat.status === "queued" ||
                  chat.status === "stopping"
                }
              >
                <ArrowUp size={17} />
              </button>
            </div>
          </div>
        </div>
        <div className="composer-hint">
          {!live
            ? "Reconnecting · your draft is preserved"
            : chat.status === "running"
              ? chat.provider === "claude"
                ? "Queued for the next turn"
                : "Send to steer the current run"
              : "⌘ / Ctrl + Enter to send"}
        </div>
      </form>
    </div>
  );
}
