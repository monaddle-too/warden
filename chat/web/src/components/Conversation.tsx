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
  Brain,
  Bug,
  Cpu,
  Download,
  Eraser,
  FlaskConical,
  GitFork,
  MessageCircleQuestion,
  Receipt,
  File as FileIcon,
  FileText,
  Folder,
  Gauge,
  Globe,
  Paperclip,
  Pencil,
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
  promoteAside,
  editQueued as editQueuedMessage,
  me,
  newID,
  downloadFile,
  rewindChat,
  sendQueued,
  undoRewind,
  uploadAttachment,
  withdrawMessage,
} from "../api";
import {
  agentCommandNamed,
  agentHint,
  bugReport,
  commandItems,
  exactCommand,
  isBugTest,
  MODES,
  mentionFor,
  nextMode,
  prefixed,
  quoteCommand,
  replaceTrigger,
  resourceItems,
  resourceQuery,
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
import {
  canEditAndResend,
  canWithdraw,
  editingScope,
  heldHint,
  lastQueued,
  queueHeld,
  queueHint,
  queueLabel,
  queuedLast,
  type Editing,
} from "../queue";
import { canRewind, canUndoRewind, doubleEscape, excerpt } from "../rewind";
import { sameFooter, turnFooters, type TurnFooter } from "../turns";
import type { AgentOptions, Chat, Entry, SessionSettings } from "../types";
import { ComposerAttachments, type Pending } from "./Attachments";
import { ContextMeter } from "./ContextMeter";
import { chatStatusLabel, startupLine } from "../stages";
import { pendingReply } from "../thinking";
import { ActivityGroup, EntryView } from "./EntryView";
import { ApprovalCard } from "./Approvals";
import { FindBar, type FindRequest } from "./FindBar";
import { ComposerMenu } from "./ComposerMenu";
import { modelOptions } from "../models";
import { SpendChip } from "./SpendChip";
import { arrowStep, isKey, modifierKey } from "../shortcuts";
import { CostCard } from "./CostCard";
import { ComposerPastes } from "./Pastes";
import {
  Suggest,
  usePathCompletion,
  useResourceCompletion,
  type Suggestion,
} from "./Suggest";
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
  ) : name === "thinking" ? (
    <Brain size={15} />
  ) : name === "effort" ? (
    <Gauge size={15} />
  ) : name === "export" ? (
    <Download size={15} />
  ) : name === "fork" ? (
    <GitFork size={15} />
  ) : name === "btw" ? (
    <MessageCircleQuestion size={15} />
  ) : name === "cost" ? (
    <Receipt size={15} />
  ) : name === "bug" ? (
    <Bug size={15} />
  ) : name === "test" ? (
    <FlaskConical size={15} />
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
  onSettings,
  agentOptions,
  onExport,
  onRewind,
  onChanges,
  onFork,
  onStyle,
  prefill,
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
  /* The thinking, effort and fast-mode controls beside the model and
     /thinking, /effort (Claude chats); agentOptions says which of the
     costlier choices this Warden offers. */
  onSettings?: (change: SessionSettings) => Promise<unknown>;
  agentOptions?: AgentOptions;
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
  /* A message to put into an empty composer: the one a rewind from the
     chooser went back to before (Claude Code's prefill), so it can be
     edited and sent again. `key` changes with every rewind. */
  prefill?: { key: number; entry: Entry };
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
  // A line the composer says back (a bug report drafted, or how to turn
  // reporting on); cleared by the next send.
  const [notice, setNotice] = useState("");
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
  // Queued messages read last, wherever they sit (queue.ts).
  const { top: entries, nested } = useMemo(() => {
    const { top, nested } = nestEntries(all);
    return { top: queuedLast(top), nested };
  }, [all]);
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
  // This person's earlier prompts, newest first, for Up/Down.
  const history = useMemo(() => promptHistory(all, me.principalID), [all]);
  const [recall, setRecall] = useState<Recall>(NOT_BROWSING);
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
      if (!isKey(event, "find")) return;
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
  // How long a side question holds the composer before it is freed (a
// refusal arrives well within it).
const ASIDE_RELEASE_MS = 1500;

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
  // Load puts an entry's text and uploads into the composer, asking first
  // when that would replace something already there; false when declined.
  const load = useCallback(
    (entry: Entry): boolean => {
      const { text, pending } = current.current;
      const draft = text.trim();
      const replacing =
        (draft !== "" && draft !== entry.text.trim()) ||
        pending.some((p) => !p.reused);
      if (replacing && !window.confirm("Replace your draft with this message?"))
        return false;
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
      setPastes([]);
      setRecall(NOT_BROWSING);
      setError("");
      requestAnimationFrame(() => {
        const el = input.current;
        if (!el) return;
        el.focus();
        el.setSelectionRange(el.value.length, el.value.length);
      });
      return true;
    },
    [forget],
  );
  // Edit-and-resend on a message the agent got (queue.ts): the composer
  // holds the message, and sending it rewinds the conversation to before
  // it first. Cancel gives the earlier draft back.
  const [editing, setEditing] = useState<Editing | null>(null);
  const edit = useCallback(
    (entry: Entry) => {
      const draft = current.current.text;
      if (!load(entry)) return;
      setEditing({ entry, draft, code: false });
    },
    [load],
  );
  function cancelEditing() {
    if (!editing) return;
    setEditing(null);
    setText(editing.draft);
    setPending([]);
    input.current?.focus();
  }
  // A queued message edited on its card (queue.ts): the draft lives here
  // so it survives the card, and the save keeps the message's slot. A
  // message the agent gets meanwhile ends the edit with the draft moved
  // into the composer, to send as a new message.
  const [queuedEdit, setQueuedEdit] = useState<{
    id: string;
    text: string;
  } | null>(null);
  const editQueued = useCallback((entry: Entry) => {
    setError("");
    setQueuedEdit({ id: entry.id, text: entry.text });
  }, []);
  const overtaken = useCallback(
    (draft: string) => {
      setQueuedEdit(null);
      if (draft.trim())
        setText((text) => (text.trim() ? text + "\n" : "") + draft);
      setError(
        "The agent got the message before your edit was saved; the edit is in the composer to send as a new message",
      );
    },
    [],
  );
  const saveQueued = useCallback(
    async (entry: Entry, text: string, attachments: string[]) => {
      try {
        await editQueuedMessage(chat.id, entry.id, text, attachments);
        setQueuedEdit(null);
      } catch (e) {
        if (/already sent/.test(String(e))) {
          overtaken(text);
          return;
        }
        throw e;
      }
    },
    [chat.id, overtaken],
  );
  useEffect(() => {
    if (!queuedEdit) return;
    const entry = chat.conversation.entries.find((e) => e.id === queuedEdit.id);
    if (entry && entry.delivery === "queued") return;
    // Withdrawn elsewhere, or handed to the agent: the card is gone.
    overtaken(entry ? queuedEdit.text : "");
  }, [chat.conversation.entries, queuedEdit, overtaken]);
  const undo = useCallback(
    async (entry: Entry, code: boolean) => {
      setError("");
      try {
        await undoRewind(chat.id, entry.id, code);
        setFollow(true);
      } catch (e) {
        setError(String(e));
      }
    },
    [chat.id, setFollow],
  );
  const withdraw = useCallback(
    async (entry: Entry) => {
      setError("");
      try {
        await withdrawMessage(chat.id, entry.id);
      } catch (e) {
        setError(String(e));
      }
    },
    [chat.id],
  );
  const sendQueuedNow = useCallback(async () => {
    setError("");
    try {
      await sendQueued(chat.id);
      setFollow(true);
    } catch (e) {
      setError(String(e));
    }
  }, [chat.id, setFollow]);
  const changeQueuedEdit = useCallback(
    (text: string) => setQueuedEdit((q) => (q ? { ...q, text } : q)),
    [],
  );
  const cancelQueuedEdit = useCallback(() => setQueuedEdit(null), []);
  // The message a rewind went back to before, into an empty composer.
  useEffect(() => {
    if (!prefill || current.current.text.trim() !== "") return;
    load(prefill.entry);
    setEditing(null);
  }, [prefill, load]);
  const held = queueHeld(chat);
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
    () => modelOptions(chat.provider || "codex", agentOptions),
    [chat.provider, agentOptions],
  );
  // Permission modes are a Claude chat's (the service refuses them for
  // Codex); the mode can change at any time, a running turn included.
  const modes = chat.provider === "claude" && !!onMode;
  // So are the session settings (thinking, effort, fast mode). A Claude
  // chat's model changes on its live session too; Codex's at the next run.
  const settings = chat.provider === "claude" && !!onSettings;
  const modelLocked = running && chat.provider !== "claude";
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
  const mentionOpen = open && trigger.kind === "path";
  // A resource-only query (`@doc:…`) asks for no paths.
  const { paths, error: pathError } = usePathCompletion(
    chat.id,
    mentionOpen && !resourceQuery(trigger.query) ? trigger.query : undefined,
  );
  const resources = useResourceCompletion(chat.id, mentionOpen);
  const resourceRows = useMemo(
    () => (mentionOpen ? resourceItems(resources, trigger.query) : []),
    [mentionOpen, resources, trigger],
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
                      ? modelLocked
                      : item.command.name === "mode"
                        ? !modes
                        : item.command.name === "thinking" ||
                            item.command.name === "effort"
                          ? !settings
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
              : item.kind === "thinking"
                ? {
                    id: "thinking:" + item.thinking.value,
                    label: item.thinking.label,
                    hint: item.thinking.hint,
                    icon: <Brain size={15} />,
                    disabled: !settings || chat.archived,
                  }
                : item.kind === "effort"
                  ? {
                      id: "effort:" + item.effort.value,
                      label: item.effort.label,
                      hint: item.effort.hint,
                      icon: <Gauge size={15} />,
                      disabled: !settings || chat.archived,
                    }
                  : item.kind === "model"
                    ? {
                        id: "model:" + item.model.value,
                        label: item.model.label,
                        hint: item.model.hint || item.model.value,
                        icon: <Cpu size={15} />,
                        disabled: modelLocked || item.model.disabled,
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
      if (/^bug(\s|$)/i.test(trigger.query.trimStart()))
        return {
          items,
          note:
            bugReport(text) === undefined
              ? "Type what went wrong; sending drafts a bug report you review before it goes to Monaddle"
              : `${modifierKey}Enter drafts the report; you review it before it is sent`,
        };
      if (/^test(\s|$)/i.test(trigger.query.trimStart()))
        return {
          items,
          note: isBugTest(text)
            ? `${modifierKey}Enter raises a test exception in the chat service; its report opens for review`
            : "/test bugreporting raises a test exception in the chat service; its report opens for review",
        };
      if (/^btw(\s|$)/i.test(trigger.query.trimStart()))
        return {
          items,
          note: !asides
            ? "Side questions are a Claude chat's"
            : running
              ? "Side questions wait until the agent's turn is over"
              : sideQuestion(text) === undefined
                ? "Type the question; sending it asks a copy of the session, and the agent never sees it"
                : `${modifierKey}Enter asks a copy of the session; the agent never sees it`,
        };
      // An agent command with its argument typed: nothing to pick, the
      // message goes as it is; its hint stays up while it is written.
      const named = agentCommandNamed(trigger.query, agentCommands);
      if (named) return { items, note: agentHint(named) || undefined };
      return { items, note: "No such command" };
    }
    // The shared resources first (documents, repositories, previews),
    // then the workspace paths; a kind typed (`@doc:`) lists resources
    // alone.
    const items: Suggestion[] = resourceRows.map((row) => ({
      id: "resource:" + row.kind + ":" + row.name,
      label: row.name,
      hint: row.hint,
      icon:
        row.kind === "doc" ? (
          <FileText size={15} />
        ) : row.kind === "repo" ? (
          <GitFork size={15} />
        ) : (
          <Globe size={15} />
        ),
      group:
        row.kind === "doc"
          ? "Documents"
          : row.kind === "repo"
            ? "Repositories"
            : "Previews",
    }));
    if (resourceQuery(trigger.query))
      return {
        items,
        note: !items.length
          ? !resources
            ? "Looking up what is shared…"
            : "Nothing shared by that name"
          : undefined,
      };
    if (pathError) return { items, note: pathError };
    if (!paths) return { items, note: "Looking up paths…" };
    for (const path of paths)
      items.push({
        id: "path:" + path,
        label: path,
        mono: true,
        group: "Paths",
        icon: path.endsWith("/") ? (
          <Folder size={15} />
        ) : (
          <FileIcon size={15} />
        ),
      });
    return {
      items,
      note: !items.length
        ? "No matching paths"
        : paths.length >= PATH_LIMIT
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
    resources,
    resourceRows,
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
    if (item.kind === "thinking" || item.kind === "effort") {
      setError(settings ? "" : "thinking and effort apply to Claude chats");
      const change: SessionSettings =
        item.kind === "thinking"
          ? { thinking: item.thinking.value }
          : { effort: item.effort.value };
      if (settings) void onSettings(change).catch((e) => setError(String(e)));
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
      // The list disables a Codex chat's models while the agent runs;
      // "/model x" typed in full and sent gets the same answer the
      // service would give. A Claude chat's live session takes the model
      // at any time.
      setError(modelLocked ? "wait until the conversation is idle" : "");
      if (!modelLocked)
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
      case "thinking":
        place({ text: "/thinking " + rest, caret: 10 });
        break;
      case "effort":
        place({ text: "/effort " + rest, caret: 8 });
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
      case "bug":
        // The report is typed after it; Enter then drafts it.
        place({ text: "/bug " + rest, caret: 5 });
        break;
      case "test":
        place({ text: "/test bugreporting", caret: 18 });
        break;
      case "style":
        place({ text: "/style " + rest, caret: 7 });
        break;
      case "clear":
        for (const item of pending) forget(item);
        setPending([]);
        setEditing(null);
        setError("");
        place({ text: "", caret: 0 });
        break;
    }
  }
  function pick(item: Suggestion) {
    if (!trigger || item.disabled) return;
    if (trigger.kind === "path") {
      const row = resourceRows.find(
        (r) => "resource:" + r.kind + ":" + r.name === item.id,
      );
      place(
        replaceTrigger(
          text,
          trigger,
          row ? row.insert : mentionFor(item.id.slice(5)),
        ),
      );
      return;
    }
    const chosen = commands.find(
      (c) =>
        (c.kind === "command"
          ? "command:" + c.command.name
          : c.kind === "mode"
            ? "mode:" + c.mode.value
            : c.kind === "thinking"
              ? "thinking:" + c.thinking.value
              : c.kind === "effort"
                ? "effort:" + c.effort.value
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
  // A "/bug" report: drafted by the service with this chat's ids, shown
  // for review by the launcher; the answer is a line, not a card. Off,
  // the draft stays in the composer beside the way to turn reporting on.
  async function reportBug(text: string) {
    setBusy(true);
    setError("");
    setNotice("");
    try {
      const result = await api<{ drafted: boolean; notice: string }>(
        `chats/${chat.id}/bug`,
        { text },
      );
      setNotice(result.notice);
      if (result.drafted) {
        setText("");
        setPastes([]);
        setRecall(NOT_BROWSING);
      }
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  // "/test bugreporting": a test exception in the chat service.
  async function testBugReporting() {
    setBusy(true);
    setError("");
    setNotice("");
    try {
      const result = await api<{ drafted: boolean; notice: string }>(
        "bug-test",
        {},
      );
      setNotice(result.notice);
      if (result.drafted) setText("");
    } catch (e) {
      setError(String(e));
    } finally {
      setBusy(false);
    }
  }
  // A "/btw" question: asked of a copy of the agent's session; the card
  // arrives over the event stream (starting, running, then answered). A
  // refusal comes back at once; an answer takes seconds, and a released
  // session's start up to minutes, so the composer is freed after a
  // moment either way and a late failure is shown when it lands (the
  // card carries it too).
  async function ask(question: string) {
    if (!asides) {
      setError("side questions are a Claude chat's");
      return;
    }
    setBusy(true);
    setError("");
    const request = askAside(chat.id, question);
    const outcome = await Promise.race<"ok" | "pending" | Error>([
      request.then(
        () => "ok" as const,
        (e) => (e instanceof Error ? e : new Error(String(e))),
      ),
      new Promise<"pending">((resolve) =>
        setTimeout(() => resolve("pending"), ASIDE_RELEASE_MS),
      ),
    ]);
    setBusy(false);
    if (outcome instanceof Error) {
      setError(String(outcome));
      return;
    }
    setText("");
    setPastes([]);
    setRecall(NOT_BROWSING);
    setFollow(true);
    if (outcome === "pending") request.catch((e) => setError(String(e)));
  }
  // "Ask in chat" on an aside card: the question goes as this person's
  // message with the answer quoted (the service composes it); the card
  // then says so.
  async function promote(entry: Entry) {
    setError("");
    try {
      await promoteAside(chat.id, entry.id);
      setFollow(true);
    } catch (e) {
      setError(String(e));
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
    const bug = bugReport(expandPastes(text, pastes));
    if (bug !== undefined && !busy) {
      void reportBug(bug);
      return;
    }
    if (isBugTest(text) && !busy) {
      void testBugReporting();
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
      if (editing) {
        // The conversation goes back to before the message being edited
        // (the code too when asked), then the edit goes as a new message.
        await rewindChat(chat.id, editing.entry.id, editingScope(editing));
        setEditing(null);
      }
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
          entries={all}
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
                      onPromote={promote}
                      onEditQueued={editQueued}
                      onWithdraw={withdraw}
                      onSendQueued={sendQueuedNow}
                      queuedEdit={
                        queuedEdit?.id === item.entry.id
                          ? queuedEdit.text
                          : undefined
                      }
                      onQueuedEditChange={changeQueuedEdit}
                      onSaveQueued={saveQueued}
                      onCancelQueuedEdit={cancelQueuedEdit}
                      onUndoRewind={undo}
                      undoable={canUndoRewind(item.entry, chat)}
                      queue={
                        item.entry.delivery === "queued"
                          ? {
                              label: queueLabel(chat),
                              held,
                              mine: canWithdraw(item.entry, me),
                            }
                          : undefined
                      }
                      actions={!busy && canResend(item.entry, chat, live)}
                      editable={
                        !busy && canEditAndResend(item.entry, chat, live)
                      }
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
                entries={chat.conversation.entries}
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
        {notice && !error && (
          <p className="composer-notice" role="status">
            {notice}
            <button
              type="button"
              className="link"
              onClick={() => setNotice("")}
              aria-label="Dismiss"
            >
              Dismiss
            </button>
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
          {open && (items.length > 0 || note) && (
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
          <ComposerPastes
            items={kept}
            onRemove={(paste) => {
              setText((current) => removePaste(current, paste));
              setPastes((list) => list.filter((p) => p !== paste));
              input.current?.focus();
            }}
          />
          {editing && (
            <div className="composer-editing" role="status">
              <span className="composer-editing-what">
                <Pencil size={13} aria-hidden="true" /> Editing “
                {excerpt(editing.entry.text, 40)}” — sending rewinds the
                conversation to before it
              </span>
              <label className="composer-editing-code">
                <input
                  type="checkbox"
                  checked={editing.code}
                  onChange={(e) =>
                    setEditing({ ...editing, code: e.target.checked })
                  }
                />
                also rewind the code
              </label>
              <button type="button" className="ghost" onClick={cancelEditing}>
                Cancel
              </button>
            </div>
          )}
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
              // Every key here is a row of shortcuts.ts (the `?` overlay
              // lists the same table).
              if (open) {
                if (isKey(e, "menu-close")) {
                  e.preventDefault();
                  setDismissed(triggerKey);
                  return;
                }
                // With every row disabled the arrows keep moving the caret.
                if (selected >= 0 && isKey(e, "menu-move")) {
                  e.preventDefault();
                  setActive(enabledFrom(selected, arrowStep(e)));
                  return;
                }
                if (selected >= 0 && isKey(e, "menu-pick")) {
                  e.preventDefault();
                  pick(items[selected]);
                  return;
                }
              } else if (editing && isKey(e, "edit-cancel")) {
                // Editing a message: Esc leaves it, the draft comes back.
                e.preventDefault();
                lastEscape.current = 0;
                cancelEditing();
                return;
              } else if (isKey(e, "rewind")) {
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
              if (isKey(e, "send")) {
                e.preventDefault();
                e.currentTarget.form?.requestSubmit();
                return;
              }
              if (modes && isKey(e, "mode-cycle")) {
                // Shift-Tab, Claude Code's: the next permission mode.
                e.preventDefault();
                setError("");
                void onMode(nextMode(chat.mode)).catch((err) =>
                  setError(String(err)),
                );
                return;
              }
              // Up in an empty composer with a message of this person's
              // queued edits it (Claude Code's ↑), before the history.
              if (isKey(e, "edit-queued") && text === "" && !editing) {
                const last = lastQueued(all, me);
                if (last) {
                  e.preventDefault();
                  editQueued(last);
                  return;
                }
              }
              // Up at the draft's first line recalls the previous prompt,
              // Down at its last line the next (then the draft again);
              // inside a longer draft the arrows move the caret.
              if (
                isKey(e, "history-older") &&
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
                isKey(e, "history-newer") &&
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
            <div className="composer-tools">
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
            </div>
            <div className="composer-actions">
              {running && (
                <button
                  type="button"
                  className="composer-stop"
                  title="Interrupt the agent's turn; the workspace stays up"
                  disabled={busy || chat.status === "stopping"}
                  onClick={stop}
                >
                  <Square size={12} fill="currentColor" />
                  Stop
                </button>
              )}
              <ComposerMenu
                provider={chat.provider || "codex"}
                model={chat.model || ""}
                disabled={modelLocked || chat.archived}
                onModel={(model) => {
                  setError("");
                  void onModel(model).catch((e) => setError(String(e)));
                }}
                session={chat.session}
                settings={
                  settings
                    ? {
                        thinking: chat.thinking,
                        effort: chat.effort,
                        fast: chat.fast,
                      }
                    : undefined
                }
                style={chat.outputStyle || ""}
                options={agentOptions}
                onSettings={
                  settings
                    ? (change) => {
                        setError("");
                        void onSettings(change).catch((e) =>
                          setError(String(e)),
                        );
                      }
                    : undefined
                }
                onStyle={
                  styles
                    ? (style) => {
                        setError("");
                        void onStyle(style).catch((e) => setError(String(e)));
                      }
                    : undefined
                }
              />
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
          <span className="composer-status">
            {modes && chat.mode && chat.mode !== "auto" && (
              <span
                className={`composer-mode-mark mode-${chat.mode}`}
                title={`${MODES.find((m) => m.value === chat.mode)?.hint || ""} · Shift+Tab or /mode changes it`}
              >
                {chat.mode === "plan" ? "Plan mode" : "Ask mode"}
              </span>
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
          <span className="composer-hint-text">
          {!live
            ? "Reconnecting · your draft is preserved"
            : prefix?.kind === "shell"
              ? heldHint(chat, "shell") ||
                "Runs as a shell command in the workspace, by you — the agent sees it only if you send the result to it"
              : prefix?.kind === "memory"
                ? heldHint(chat, "memory") ||
                  (chat.provider === "codex"
                    ? "Appends a note to CLAUDE.md in the workspace (Codex reads AGENTS.md, not CLAUDE.md)"
                    : "Appends a note to CLAUDE.md in the workspace — the agent reads it only once the workspace's settings are loaded")
                : question !== undefined
                  ? asides
                    ? "Asks a copy of the agent's session, from this chat's context — the agent never sees the question or the answer"
                    : "Side questions are a Claude chat's"
                  : editing
                    ? "Sending rewinds the conversation to before the message and sends this in its place · Esc cancels"
                    : queueHint(chat, me) ||
                      (chat.status === "running"
                        ? chat.provider === "claude"
                          ? "Queued for the next turn — edit or withdraw it from the transcript until then"
                          : "Send to steer the current run"
                        : "")}
          </span>
          <span className="composer-meters">
            {chat.conversation.context && (
              <ContextMeter context={chat.conversation.context} />
            )}
            <SpendChip chat={chat} />
          </span>
        </div>
      </form>
    </div>
  );
}
