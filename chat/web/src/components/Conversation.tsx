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
  GitFork,
  MessageCircleQuestion,
  Receipt,
  File as FileIcon,
  Folder,
  Paperclip,
  ShieldCheck,
  Slash,
  SlidersHorizontal,
  Square,
  Terminal,
} from "lucide-react";
import {
  canResend,
  messageAttempt,
  readLocalAttempt,
  resendAttempt,
  type Attempt,
} from "../drafts";
import {
  api,
  askAside,
  me,
  newID,
  downloadFile,
  uploadAttachment,
} from "../api";
import {
  agentCommandNamed,
  agentHint,
  commandItems,
  exactCommand,
  mentionFor,
  prefixed,
  quoteCommand,
  replaceTrigger,
  sideQuestion,
  triggerAt,
  withoutCommand,
  type CommandItem,
} from "../composer";
import {
  NOT_BROWSING,
  onFirstLine,
  onLastLine,
  promptHistory,
  recallNewer,
  recallOlder,
  type Recall,
} from "../history";
import {
  collapsePaste,
  expandPastes,
  livePastes,
  longPaste,
  readPastes,
  removePaste,
  type Paste,
} from "../paste";
import {
  attachmentError,
  hasFiles,
  pastedName,
  transferFiles,
} from "../attachments";
import {
  groupEntries,
  nestEntries,
  newSince,
  readSeen,
  unreadEntry,
  unreadIndex,
} from "../transcript";
import { canRewind, doubleEscape } from "../rewind";
import { sameFooter, turnFooters, type TurnFooter } from "../turns";
import type { Chat, Entry } from "../types";
import { ComposerAttachments, type Pending } from "./Attachments";
import { ContextMeter } from "./ContextMeter";
import { chatStatusLabel, startupLine } from "../stages";
import { pendingReply } from "../thinking";
import { ActivityGroup, EntryView } from "./EntryView";
import { ApprovalCard } from "./Approvals";
import { FindBar, isFindKey, type FindRequest } from "./FindBar";
import { HistorySearch } from "./HistorySearch";
import { ModelSelect, modelOptions } from "./ModelSelect";
import { ModeSelect } from "./ModeSelect";
import { StyleSelect } from "./StyleSelect";
import { CostCard } from "./CostCard";
import { ComposerPastes } from "./Pastes";
import { Suggest, usePathCompletion, type Suggestion } from "./Suggest";
import { PendingReply } from "./Thinking";
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
function readPastesLocal(key: string) {
  try {
    return readPastes(localStorage, key);
  } catch {
    return [];
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
  ) : name === "mode" ? (
    <ShieldCheck size={15} />
  ) : name === "export" ? (
    <Download size={15} />
  ) : name === "fork" ? (
    <GitFork size={15} />
  ) : name === "btw" ? (
    <MessageCircleQuestion size={15} />
  ) : name === "cost" ? (
    <Receipt size={15} />
  ) : name === "style" ? (
    <SlidersHorizontal size={15} />
  ) : (
    <Eraser size={15} />
  );
export function Conversation({
  chat,
  live,
  requests = [],
  find,
  onModel,
  onMode,
  onExport,
  onRewind,
  onChanges,
  onFork,
  onStyle,
}: {
  chat: Chat;
  live: boolean;
  requests?: RequestCard[];
  /* Opens the find bar: from the header's button, or from the palette
     with the entry to land on. A request for another chat is ignored, so
     one made before a switch does not follow the reader. */
  find?: FindRequest;
  onModel: (model: string) => Promise<unknown>;
  /* The permission mode selector and /mode (Claude chats). */
  onMode?: (mode: string) => Promise<unknown>;
  /* The /export command; the chat menu's dialog lives in the shell. */
  onExport?: () => void;
  /* The rewind chooser (a message's hover action, Esc-Esc, /rewind) and
     the session diff (/diff); both dialogs live in the shell. */
  onRewind?: (entryID?: string) => void;
  onChanges?: () => void;
  /* The fork dialog (a message's hover action, /fork, the chat menu);
     the dialog lives in the shell. */
  onFork?: (entryID?: string) => void;
  /* The output style selector and /style (Claude chats). */
  onStyle?: (style: string) => Promise<unknown>;
}) {
  const key = "warden-draft:" + location.origin + ":" + chat.id;
  const [text, setText] = useState(() => draft(key));
  // Long pastes collapsed in the text (paste.ts): the placeholder stays
  // in `text`, the text itself here, put back when the message is sent.
  const [pastes, setPastes] = useState<Paste[]>(() =>
    readPastesLocal(key + ":pastes"),
  );
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
  // The transcript's own entries; a subagent's are keyed by its card
  // (`nested`) and render inside it, so counts, groups, the unread mark and
  // the turns' lines see only the flow the reader scrolls.
  const all = chat.conversation.entries;
  const { top: entries, nested } = useMemo(() => nestEntries(all), [all]);
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
      // Their own messages, commands and notes are never unread.
      (e) =>
        (e.role === "user" || !!e.sender) &&
        (e.sender?.principalID ?? "owner") === me.principalID,
    ),
  );
  const unread = unreadIndex(entries, unreadID);
  // This person's earlier prompts, newest first, for Up/Down and Ctrl-R.
  const history = useMemo(() => promptHistory(all, me.principalID), [all]);
  const [recall, setRecall] = useState<Recall>(NOT_BROWSING);
  const [searching, setSearching] = useState(false);
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
  // When Escape was last pressed in the composer, for Esc-Esc (rewind.ts).
  const lastEscape = useRef(0);
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
  // The model is being waited on, and nothing on screen shows it yet.
  const awaited = pendingReply(
    chat,
    requests.length > 0 || chat.approvals.some((a) => a.state === "pending"),
  );
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
  // Permission modes are a Claude chat's (the service refuses them for
  // Codex); the mode can change at any time, a running turn included.
  const modes = chat.provider === "claude" && !!onMode;
  // Side questions and output styles are a Claude chat's too.
  const asides = chat.provider === "claude";
  const styles = chat.provider === "claude" && !!onStyle;
  // The /cost card, shown until dismissed (local to this reader).
  const [costOpen, setCostOpen] = useState(false);
  // The agent's own commands (Claude Code's built-ins and the workspace's)
  // join the list after the chat's; "/name …" goes to the agent as text.
  const agentCommands = useMemo(() => chat.commands ?? [], [chat.commands]);
  const agentGroup = chat.provider === "claude" ? "Claude" : "Agent";
  const commands = useMemo(
    () =>
      open && trigger.kind === "command"
        ? commandItems(trigger.query, models, agentCommands)
        : [],
    [open, trigger, models, agentCommands],
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
                group: "Chat",
                disabled:
                  item.command.name === "stop"
                    ? !running || chat.status === "stopping"
                    : item.command.name === "model"
                      ? running
                      : item.command.name === "mode"
                        ? !modes
                        : item.command.name === "btw"
                          ? !asides || running
                          : item.command.name === "style"
                            ? !styles
                            : item.command.name === "fork"
                              ? !onFork || running
                              : false,
              }
            : item.kind === "mode"
              ? {
                  id: "mode:" + item.mode.value,
                  label: item.mode.label,
                  hint: item.mode.hint,
                  icon: <ShieldCheck size={15} />,
                  disabled: !modes || chat.archived,
                }
            : item.kind === "style"
              ? {
                  id: "style:" + (item.style.value || "default"),
                  label: item.style.label,
                  hint: item.style.hint,
                  icon: <SlidersHorizontal size={15} />,
                  disabled: !styles || chat.archived,
                }
              : item.kind === "model"
                ? {
                    id: "model:" + item.model.value,
                    label: item.model.label,
                    hint: item.model.value,
                    icon: <Cpu size={15} />,
                    disabled: running,
                  }
                : {
                    id: "agent:" + item.command.name,
                    label: "/" + item.command.name,
                    hint: agentHint(item.command),
                    icon: <Slash size={15} />,
                    group: agentGroup,
                  },
      );
      if (items.length) return { items };
      if (sideQuestion(text) !== undefined)
        return {
          items,
          note: asides
            ? running
              ? "Side questions wait until the agent's turn is over"
              : "Enter asks it of a copy of the session; the agent never sees it"
            : "Side questions are a Claude chat's",
        };
      // An agent command with its argument typed: nothing to pick, the
      // message goes as it is; its hint stays up while it is written.
      const named = agentCommandNamed(trigger.query, agentCommands);
      if (named) return { items, note: agentHint(named) || undefined };
      return { items, note: "No such command" };
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
  }, [
    open,
    trigger,
    commands,
    agentCommands,
    agentGroup,
    paths,
    pathError,
    running,
    chat.status,
    chat.archived,
    modes,
    asides,
    styles,
    onFork,
    text,
  ]);
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
    if (!chat.typing?.length && !chat.startup) return;
    const id = setInterval(() => setClock(Date.now() / 1000), 1000);
    return () => clearInterval(id);
  }, [chat.typing, chat.startup]);
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
  // The pastes still placed in the text; deleting a placeholder drops its
  // paste, and what is kept persists with the draft.
  const kept = useMemo(() => livePastes(text, pastes), [text, pastes]);
  useEffect(() => {
    try {
      if (kept.length)
        localStorage.setItem(key + ":pastes", JSON.stringify(kept));
      else localStorage.removeItem(key + ":pastes");
    } catch {}
  }, [kept, key]);
  // What the composer would send or run right now (prefixes are read on
  // the text as typed; the pastes are put back when it is sent).
  const prefix = useMemo(() => prefixed(text), [text]);
  const question = useMemo(() => sideQuestion(text), [text]);
  // "Send to agent" on a command the person ran: the command and its
  // output go into the draft as a fenced block, for the next message.
  const quote = useCallback((entry: Entry) => {
    setText((current) => {
      const lead =
        current && !current.endsWith("\n") ? current + "\n" : current;
      return lead + quoteCommand(entry);
    });
    requestAnimationFrame(() => {
      const el = input.current;
      if (!el) return;
      el.focus();
      el.setSelectionRange(el.value.length, el.value.length);
    });
  }, []);
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
    if (item.kind === "mode") {
      setError(modes ? "" : "permission modes apply to Claude chats");
      if (modes) void onMode(item.mode.value).catch((e) => setError(String(e)));
      place({ text: rest, caret: 0 });
      return;
    }
    if (item.kind === "agent") {
      // Filled in, not sent: the person adds an argument or sends it as
      // it is, and the agent expands it.
      const line = "/" + item.command.name + " ";
      place({ text: line + (rest ? "\n" + rest : ""), caret: line.length });
      return;
    }
    if (item.kind === "style") {
      setError(styles ? "" : "output styles apply to Claude chats");
      if (styles)
        void onStyle(item.style.value).catch((e) => setError(String(e)));
      place({ text: rest, caret: 0 });
      return;
    }
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
      case "mode":
        place({ text: "/mode " + rest, caret: 6 });
        break;
      case "export":
        onExport?.();
        place({ text: rest, caret: 0 });
        break;
      case "rewind":
        onRewind?.();
        place({ text: rest, caret: 0 });
        break;
      case "diff":
        onChanges?.();
        place({ text: rest, caret: 0 });
        break;
      case "fork":
        onFork?.();
        place({ text: rest, caret: 0 });
        break;
      case "btw":
        // The question is typed after it; Enter then asks it.
        place({ text: "/btw " + rest, caret: 5 });
        break;
      case "cost":
        setCostOpen(true);
        setFollow(true);
        place({ text: rest, caret: 0 });
        break;
      case "style":
        place({ text: "/style " + rest, caret: 7 });
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
          : c.kind === "mode"
            ? "mode:" + c.mode.value
            : c.kind === "model"
              ? "model:" + c.model.value
              : c.kind === "style"
                ? "style:" + (c.style.value || "default")
                : "agent:" + c.command.name) === item.id,
    );
    if (chosen) runCommand(chosen, withoutCommand(text, trigger));
  }
  // A "!" command: the draft clears at once and the card shows the
  // command running in the transcript (over the event stream); the
  // request itself may take up to a minute, so the composer is not held.
  function runShell(command: string) {
    setError("");
    setText("");
    setPastes([]);
    setRecall(NOT_BROWSING);
    void api(`chats/${chat.id}/exec`, { text: command }).catch((e) =>
      setError(String(e)),
    );
  }
  async function remember(note: string) {
    setBusy(true);
    setError("");
    try {
      await api(`chats/${chat.id}/memory`, { text: note });
      setText("");
      setPastes([]);
      setRecall(NOT_BROWSING);
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  // A "/btw" question: asked of a copy of the agent's session; the card
  // arrives over the event stream (running, then answered).
  async function ask(question: string) {
    if (!asides) {
      setError("side questions are a Claude chat's");
      return;
    }
    setBusy(true);
    setError("");
    try {
      await askAside(chat.id, question);
      setText("");
      setPastes([]);
      setRecall(NOT_BROWSING);
      setFollow(true);
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  async function send(event: FormEvent) {
    event.preventDefault();
    const command = exactCommand(text, models);
    if (command) {
      runCommand(command, "");
      return;
    }
    const asked = sideQuestion(expandPastes(text, pastes));
    if (asked !== undefined && !busy) {
      void ask(asked);
      return;
    }
    if (prefix && !busy) {
      // Pastes are put back here too: "!" with a pasted script runs it.
      const full = expandPastes(text, pastes);
      if (prefix.kind === "shell") runShell(full.slice(1).trim());
      else void remember(full.slice(1).trim());
      return;
    }
    const attachments = ready.map((p) => p.attachment!.id);
    if ((!text.trim() && !attachments.length) || busy || uploading) return;
    setBusy(true);
    setError("");
    const message = messageAttempt(
      attempted.current,
      expandPastes(text, pastes).trim(),
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
      setPastes([]);
      setRecall(NOT_BROWSING);
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
                    <ActivityGroup
                      entries={item.group}
                      nested={nested}
                      chatID={chat.id}
                      provider={chat.provider}
                      onFile={onFile}
                    />
                  ) : (
                    <EntryView
                      provider={chat.provider}
                      chatID={chat.id}
                      entry={item.entry}
                      nested={nested}
                      onFile={onFile}
                      onEdit={edit}
                      onRetry={retry}
                      onRewind={
                        onRewind ? (entry) => onRewind(entry.id) : undefined
                      }
                      onFork={onFork ? (entry) => onFork(entry.id) : undefined}
                      onQuote={quote}
                      actions={!busy && canResend(item.entry, chat, live)}
                      rewindable={canRewind(chat)}
                      stats={inline ? footer : undefined}
                    />
                  )}
                  {footer && !inline && <TurnStats footer={footer} block />}
                </Fragment>
              );
            })}
            {costOpen && (
              <CostCard
                turns={chat.conversation.turns}
                provider={chat.provider}
                running={running}
                onClose={() => setCostOpen(false)}
              />
            )}
            {awaited && (
              <PendingReply provider={chat.provider} since={awaited.since} />
            )}
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
          {open && !searching && (items.length > 0 || note) && (
            <Suggest
              id="composer-suggest"
              items={items}
              active={selected}
              note={note}
              onHover={setActive}
              onPick={pick}
            />
          )}
          {searching && (
            <HistorySearch
              history={history}
              onPick={(picked) => {
                setSearching(false);
                setRecall(NOT_BROWSING);
                place({ text: picked, caret: picked.length });
              }}
              onClose={() => {
                setSearching(false);
                input.current?.focus();
              }}
            />
          )}
          <ComposerAttachments
            chatID={chat.id}
            items={pending}
            onRemove={removePending}
          />
          <ComposerPastes
            items={kept}
            onRemove={(paste) => {
              setText((current) => removePaste(current, paste));
              setPastes((list) => list.filter((p) => p !== paste));
              input.current?.focus();
            }}
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
              // Typing makes the text the draft again, wherever the
              // history was.
              setRecall(NOT_BROWSING);
              if (e.target.value) reportTyping();
            }}
            onSelect={(e) => setCaret(e.currentTarget.selectionStart)}
            onFocus={() => setFocused(true)}
            onBlur={() => setFocused(false)}
            onPaste={(e) => {
              const files = transferFiles(e.clipboardData);
              if (files.length) {
                e.preventDefault();
                addFiles(files, true);
                return;
              }
              const pasted = e.clipboardData?.getData("text/plain") ?? "";
              if (!longPaste(pasted)) return;
              // A long paste becomes a placeholder and a chip; the text
              // goes back in when the message is sent.
              e.preventDefault();
              const el = e.currentTarget;
              const next = collapsePaste(
                el.value,
                el.selectionStart,
                el.selectionEnd,
                pasted,
                pastes,
              );
              setPastes(next.pastes);
              setRecall(NOT_BROWSING);
              place(next);
              reportTyping();
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
              } else if (e.key === "Escape") {
                // Esc-Esc, Claude Code's rewind key: the chooser opens on
                // the last message. One Esc is the interrupt (the Stop
                // button) and stays as it is.
                const now = Date.now();
                if (doubleEscape(lastEscape.current, now)) {
                  lastEscape.current = 0;
                  e.preventDefault();
                  onRewind?.();
                  return;
                }
                lastEscape.current = now;
              }
              if (e.key === "Enter" && (e.metaKey || e.ctrlKey)) {
                e.preventDefault();
                e.currentTarget.form?.requestSubmit();
                return;
              }
              if (e.key === "r" && (e.ctrlKey || e.metaKey) && !e.shiftKey) {
                // Ctrl-R (or ⌘R, which would reload): search the history.
                e.preventDefault();
                setSearching(true);
                return;
              }
              // Up at the draft's first line recalls the previous prompt,
              // Down at its last line the next (then the draft again);
              // inside a longer draft the arrows move the caret.
              if (
                e.key === "ArrowUp" &&
                !e.altKey &&
                !e.shiftKey &&
                !e.metaKey &&
                onFirstLine(text, e.currentTarget.selectionStart)
              ) {
                const step = recallOlder(recall, history, text);
                if (!step) return;
                e.preventDefault();
                setRecall(step.recall);
                place({ text: step.text, caret: step.text.length });
                return;
              }
              if (
                e.key === "ArrowDown" &&
                !e.altKey &&
                !e.shiftKey &&
                !e.metaKey &&
                onLastLine(text, e.currentTarget.selectionStart)
              ) {
                const step = recallNewer(recall, history);
                if (!step) return;
                e.preventDefault();
                setRecall(step.recall);
                place({ text: step.text, caret: step.text.length });
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
              {modes && (
                <span className="composer-mode">
                  <ModeSelect
                    value={chat.mode || "auto"}
                    disabled={chat.archived}
                    onChange={(mode) => {
                      setError("");
                      void onMode(mode).catch((e) => setError(String(e)));
                    }}
                  />
                </span>
              )}
              {styles && (
                <span className="composer-style">
                  <StyleSelect
                    value={chat.outputStyle || ""}
                    running={chat.session?.outputStyle}
                    disabled={chat.archived}
                    onChange={(style) => {
                      setError("");
                      void onStyle(style).catch((e) => setError(String(e)));
                    }}
                  />
                </span>
              )}
              {chat.conversation.context && (
                <ContextMeter context={chat.conversation.context} />
              )}
              <span
                className={`status-dot ${chat.startup && running ? "starting" : chat.status}`}
              />
              <span
                className={chat.startup && running ? "composer-startup" : ""}
                title={chat.startup?.detail}
              >
                {chat.startup && running
                  ? startupLine(chat.startup, clock)
                  : chat.archived && !running
                    ? "Archived"
                    : chatStatusLabel(chat)}
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
                  title="Interrupt the agent's turn; the workspace stays up"
                  disabled={busy || chat.status === "stopping"}
                  onClick={stop}
                >
                  <Square size={12} fill="currentColor" />
                  Stop
                </button>
              )}
              <button
                className="send-button"
                aria-label={
                  prefix?.kind === "shell"
                    ? "Run in the workspace"
                    : prefix?.kind === "memory"
                      ? "Add to CLAUDE.md"
                      : question !== undefined
                        ? "Ask a side question"
                        : "Send message"
                }
                title={
                  prefix?.kind === "shell"
                    ? "Run this shell command in the workspace, as you"
                    : prefix?.kind === "memory"
                      ? "Append this note to CLAUDE.md in the workspace"
                      : undefined
                }
                disabled={
                  (!text.trim() && !ready.length) ||
                  uploading ||
                  busy ||
                  !live ||
                  chat.archived ||
                  (question !== undefined && (!asides || running)) ||
                  (!prefix &&
                    (chat.status === "queued" || chat.status === "stopping"))
                }
              >
                {prefix?.kind === "shell" ? (
                  <Terminal size={16} />
                ) : question !== undefined ? (
                  <MessageCircleQuestion size={16} />
                ) : (
                  <ArrowUp size={17} />
                )}
              </button>
            </div>
          </div>
        </div>
        <div className="composer-hint">
          {!live
            ? "Reconnecting · your draft is preserved"
            : prefix?.kind === "shell"
              ? "Runs as a shell command in the workspace, by you — the agent sees it only if you send the result to it"
              : prefix?.kind === "memory"
                ? chat.provider === "codex"
                  ? "Appends a note to CLAUDE.md in the workspace (Codex reads AGENTS.md, not CLAUDE.md)"
                  : "Appends a note to CLAUDE.md in the workspace — the agent reads it only once the workspace's settings are loaded"
                : question !== undefined
                  ? asides
                    ? "Asks a copy of the agent's session, from this chat's context — the agent never sees the question or the answer"
                    : "Side questions are a Claude chat's"
                  : chat.status === "running"
                    ? chat.provider === "claude"
                      ? "Queued for the next turn"
                      : "Send to steer the current run"
                    : "⌘ / Ctrl + Enter to send · / commands · @ file · ! shell · # note · /btw aside · ↑ history · Ctrl+R search"}
        </div>
      </form>
    </div>
  );
}
