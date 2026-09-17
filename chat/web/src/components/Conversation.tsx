// Panta's transcript/composer layout adapted to Warden's standalone API.
import {
  Fragment,
  useCallback,
  useEffect,
  useLayoutEffect,
  useMemo,
  useRef,
  useState,
  type DragEvent,
  type FormEvent,
  type ReactNode,
} from "react";
import {
  ArrowDown,
  ArrowUp,
  Bot,
  Cpu,
  Download,
  Eraser,
  File as FileIcon,
  Folder,
  Paperclip,
  Square,
} from "lucide-react";
import {
  canResend,
  messageAttempt,
  readLocalAttempt,
  resendAttempt,
  type Attempt,
} from "../drafts";
import { api, me, newID, downloadFile, uploadAttachment } from "../api";
import {
  commandItems,
  exactCommand,
  mentionFor,
  replaceTrigger,
  triggerAt,
  withoutCommand,
  type CommandItem,
} from "../composer";
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
import { sameFooter, turnFooters, type TurnFooter } from "../turns";
import type { Chat, Entry } from "../types";
import { ComposerAttachments, type Pending } from "./Attachments";
import { ActivityGroup, EntryView } from "./EntryView";
import { ApprovalCard } from "./Approvals";
import { FindBar, isFindKey, type FindRequest } from "./FindBar";
import { ModelSelect, modelOptions } from "./ModelSelect";
import { Suggest, usePathCompletion, type Suggestion } from "./Suggest";
import { TurnStats } from "./TurnStats";

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
/* The worker answers at most this many paths; a full page means more. */
const PATH_LIMIT = 50;
const commandIcon = (name: string) =>
  name === "stop" ? (
    <Square size={15} />
  ) : name === "model" ? (
    <Cpu size={15} />
  ) : name === "export" ? (
    <Download size={15} />
  ) : (
    <Eraser size={15} />
  );
export function Conversation({
  chat,
  live,
  requests = [],
  find,
  onModel,
  onExport,
}: {
  chat: Chat;
  live: boolean;
  requests?: RequestCard[];
  /* Opens the find bar: from the header's button, or from the palette
     with the entry to land on. A request for another chat is ignored, so
     one made before a switch does not follow the reader. */
  find?: FindRequest;
  onModel: (model: string) => Promise<unknown>;
  /* The /export command; the chat menu's dialog lives in the shell. */
  onExport?: () => void;
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
    unreadEntry(
      entries,
      readSeenLocal(seenKey),
      (e) =>
        e.role === "user" &&
        (e.sender?.principalID ?? "owner") === me.principalID,
    ),
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
  // Slash commands and @path mentions: the caret decides which list is
  // up; Escape puts a trigger away until the caret leaves it, and the list
  // is only shown while the textarea has the focus.
  const [caret, setCaret] = useState(0);
  const [focused, setFocused] = useState(false);
  const [dismissed, setDismissed] = useState("");
  const [active, setActive] = useState(0);
  const trigger = useMemo(() => triggerAt(text, caret), [text, caret]);
  const triggerKey = trigger ? `${trigger.kind}:${trigger.start}` : "";
  const open =
    !!trigger && focused && !chat.archived && dismissed !== triggerKey;
  const models = useMemo(
    () => modelOptions(chat.provider || "codex"),
    [chat.provider],
  );
  const commands = useMemo(
    () =>
      open && trigger.kind === "command"
        ? commandItems(trigger.query, models)
        : [],
    [open, trigger, models],
  );
  const { paths, error: pathError } = usePathCompletion(
    chat.id,
    open && trigger.kind === "path" ? trigger.query : undefined,
  );
  const { items, note } = useMemo((): {
    items: Suggestion[];
    note?: string;
  } => {
    if (!open) return { items: [] };
    if (trigger.kind === "command") {
      const items = commands.map(
        (item): Suggestion =>
          item.kind === "command"
            ? {
                id: "command:" + item.command.name,
                label: item.command.label,
                hint: item.command.hint,
                icon: commandIcon(item.command.name),
                disabled:
                  item.command.name === "stop"
                    ? !running || chat.status === "stopping"
                    : item.command.name === "model"
                      ? running
                      : false,
              }
            : {
                id: "model:" + item.model.value,
                label: item.model.label,
                hint: item.model.value,
                icon: <Cpu size={15} />,
                disabled: running,
              },
      );
      return { items, note: items.length ? undefined : "No such command" };
    }
    if (pathError) return { items: [], note: pathError };
    if (!paths) return { items: [], note: "Looking up paths…" };
    const items = paths.map(
      (path): Suggestion => ({
        id: "path:" + path,
        label: path,
        mono: true,
        icon: path.endsWith("/") ? (
          <Folder size={15} />
        ) : (
          <FileIcon size={15} />
        ),
      }),
    );
    return {
      items,
      note: !items.length
        ? "No matching paths"
        : items.length >= PATH_LIMIT
          ? "Keep typing to narrow the list"
          : undefined,
    };
  }, [open, trigger, commands, paths, pathError, running, chat.status]);
  // The row the keys act on: never a disabled one, so Enter on a fresh
  // list runs something. -1 when every row is disabled.
  const selected = active < 0 ? -1 : Math.min(active, items.length - 1);
  const enabledFrom = (from: number, by: number) => {
    for (let n = 0, i = from; n < items.length; n++) {
      i = (i + by + items.length) % items.length;
      if (!items[i].disabled) return i;
    }
    return -1;
  };
  useEffect(() => {
    setActive(items.findIndex((item) => !item.disabled));
  }, [items]);
  useEffect(() => {
    if (!trigger) setDismissed("");
  }, [trigger]);
  // Puts text into the textarea with the caret where the pick left it.
  function place(next: { text: string; caret: number }) {
    setText(next.text);
    setCaret(next.caret);
    requestAnimationFrame(() => {
      const el = input.current;
      if (!el) return;
      el.focus();
      el.setSelectionRange(next.caret, next.caret);
    });
  }
  // Each turn's timing and usage line, keyed by the entry it goes under.
  // A line that would read the same keeps its object, so the memoised
  // entry it belongs to is not re-rendered by every streamed chunk.
  const turns = chat.conversation.turns;
  const footerCache = useRef(new Map<string, TurnFooter>());
  const footers = useMemo(() => {
    const fresh = turnFooters(
      entries,
      turns,
      chat.status === "running" || chat.status === "stopping",
    );
    for (const [id, footer] of fresh) {
      const old = footerCache.current.get(id);
      if (old && sameFooter(old, footer)) fresh.set(id, old);
    }
    footerCache.current = fresh;
    return fresh;
  }, [entries, turns, chat.status]);
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
  // A command picked from the list, or sent as exactly "/name": the
  // command line leaves the composer and `rest` of the draft stays.
  function runCommand(item: CommandItem, rest: string) {
    if (item.kind === "model") {
      // The list disables models while the agent runs; "/model x" typed in
      // full and sent gets the same answer the service would give.
      setError(running ? "wait until the conversation is idle" : "");
      if (!running)
        void onModel(item.model.value).catch((e) => setError(String(e)));
      place({ text: rest, caret: 0 });
      return;
    }
    switch (item.command.name) {
      case "stop":
        if (running && chat.status !== "stopping") void stop();
        place({ text: rest, caret: 0 });
        break;
      case "model":
        // The list then shows the models.
        place({ text: "/model " + rest, caret: 7 });
        break;
      case "export":
        onExport?.();
        place({ text: rest, caret: 0 });
        break;
      case "clear":
        for (const item of pending) forget(item);
        setPending([]);
        setError("");
        place({ text: "", caret: 0 });
        break;
    }
  }
  function pick(item: Suggestion) {
    if (!trigger || item.disabled) return;
    if (trigger.kind === "path") {
      place(replaceTrigger(text, trigger, mentionFor(item.id.slice(5))));
      return;
    }
    const chosen = commands.find(
      (c) =>
        (c.kind === "command"
          ? "command:" + c.command.name
          : "model:" + c.model.value) === item.id,
    );
    if (chosen) runCommand(chosen, withoutCommand(text, trigger));
  }
  async function send(event: FormEvent) {
    event.preventDefault();
    const command = exactCommand(text, models);
    if (command) {
      runCommand(command, "");
      return;
    }
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
    // Follow before the request: the message arrives over the event
    // stream, often before the reply, and the seen mark advances only
    // while following.
    setFollow(true);
    try {
      await api(`chats/${chat.id}/message`, message);
      lastTyping.current = 0;
      setText("");
      setPending([]);
      attempted.current = undefined;
      try {
        localStorage.removeItem(key + ":attempt");
      } catch {}
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
              const last =
                "group" in item
                  ? item.group[item.group.length - 1]
                  : item.entry;
              const footer = footers.get(last.id);
              // An assistant message carries its turn's line in its own
              // action row; after a tool-step group it stands alone.
              const inline = !("group" in item) && last.role === "assistant";
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
                      stats={inline ? footer : undefined}
                    />
                  )}
                  {footer && !inline && <TurnStats footer={footer} block />}
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
          {open && (
            <Suggest
              id="composer-suggest"
              items={items}
              active={selected}
              note={note}
              onHover={setActive}
              onPick={pick}
            />
          )}
          <ComposerAttachments
            chatID={chat.id}
            items={pending}
            onRemove={removePending}
          />
          <textarea
            ref={input}
            aria-label="Message agent"
            aria-haspopup="listbox"
            aria-autocomplete="list"
            aria-controls={
              open && items.length ? "composer-suggest" : undefined
            }
            aria-activedescendant={
              open && selected >= 0 ? `composer-suggest-${selected}` : undefined
            }
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
              setCaret(e.target.selectionStart);
              if (e.target.value) reportTyping();
            }}
            onSelect={(e) => setCaret(e.currentTarget.selectionStart)}
            onFocus={() => setFocused(true)}
            onBlur={() => setFocused(false)}
            onPaste={(e) => {
              const files = transferFiles(e.clipboardData);
              if (!files.length) return;
              e.preventDefault();
              addFiles(files, true);
            }}
            disabled={busy || chat.archived}
            rows={3}
            onKeyDown={(e) => {
              if (open) {
                if (e.key === "Escape") {
                  e.preventDefault();
                  setDismissed(triggerKey);
                  return;
                }
                // With every row disabled the arrows keep moving the caret.
                if (
                  selected >= 0 &&
                  (e.key === "ArrowDown" || e.key === "ArrowUp")
                ) {
                  e.preventDefault();
                  setActive(
                    enabledFrom(selected, e.key === "ArrowDown" ? 1 : -1),
                  );
                  return;
                }
                if (selected >= 0 && (e.key === "Enter" || e.key === "Tab")) {
                  e.preventDefault();
                  pick(items[selected]);
                  return;
                }
              }
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
              : "⌘ / Ctrl + Enter to send · / for commands · @ to name a file"}
        </div>
      </form>
    </div>
  );
}
