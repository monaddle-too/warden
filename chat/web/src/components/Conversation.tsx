// Panta's transcript/composer layout adapted to Warden's standalone API.
import {
  useCallback,
  useEffect,
  useLayoutEffect,
  useRef,
  useState,
  type FormEvent,
  type ReactNode,
} from "react";
import { ArrowUp, Bot, Square } from "lucide-react";
import { readLocalAttempt, messageAttempt, type Attempt } from "../drafts";
import { api, me, newID, downloadFile } from "../api";
import type { Chat, Entry } from "../types";
import { ActivityGroup, EntryView } from "./EntryView";
import { ApprovalCard } from "./Approvals";
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
/* Consecutive tool steps render as one collapsible group. */
function groupEntries(entries: Entry[]) {
  const items: ({ entry: Entry } | { group: Entry[] })[] = [];
  for (const entry of entries) {
    const last = items[items.length - 1];
    if (entry.role === "activity") {
      if (last && "group" in last) last.group.push(entry);
      else items.push({ group: [entry] });
    } else items.push({ entry });
  }
  return items;
}
function draft(key: string) {
  try {
    return localStorage.getItem(key) || "";
  } catch {
    return "";
  }
}
export function Conversation({
  chat,
  live,
  requests = [],
  onModel,
}: {
  chat: Chat;
  live: boolean;
  requests?: RequestCard[];
  onModel: (model: string) => Promise<unknown>;
}) {
  const key = "warden-draft:" + location.origin + ":" + chat.id;
  const [text, setText] = useState(() => draft(key));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  // Stable per chat so memoised entries do not re-render on every streamed
  // chunk of another message.
  const onFile = useCallback(
    (href: string) => {
      void downloadFile(chat.id, href).catch((e) => setError(String(e)));
    },
    [chat.id],
  );
  const scroll = useRef<HTMLDivElement>(null);
  const follow = useRef(true);
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
  useLayoutEffect(() => {
    if (follow.current && scroll.current)
      scroll.current.scrollTop = scroll.current.scrollHeight;
  }, [chat, requests.length]);
  async function send(event: FormEvent) {
    event.preventDefault();
    if (!text.trim() || busy) return;
    setBusy(true);
    setError("");
    const message = messageAttempt(attempted.current, text.trim(), newID);
    attempted.current = message;
    try {
      localStorage.setItem(key + ":attempt", JSON.stringify(message));
    } catch {}
    try {
      await api(`chats/${chat.id}/message`, message);
      lastTyping.current = 0;
      setText("");
      attempted.current = undefined;
      try {
        localStorage.removeItem(key + ":attempt");
      } catch {}
      follow.current = true;
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
  return (
    <div className="conversation">
      <div
        className="conversation-scroll"
        ref={scroll}
        onScroll={() => {
          const el = scroll.current!;
          follow.current =
            el.scrollHeight - el.scrollTop - el.clientHeight < 100;
        }}
      >
        {!chat.conversation.entries.length && (
          <div className="empty">
            <Bot size={30} />
            <h2>Start the conversation</h2>
            <p>
              Give your agent a task. Warden keeps its work in a sandbox and
              controls access to your apps.
            </p>
          </div>
        )}
        <div className="transcript">
          {groupEntries(chat.conversation.entries).map((item) =>
            "group" in item ? (
              <ActivityGroup key={item.group[0].id} entries={item.group} />
            ) : (
              <EntryView
                provider={chat.provider}
                chatID={chat.id}
                key={item.entry.id}
                entry={item.entry}
                onFile={onFile}
              />
            ),
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
      <form className="composer-wrap" onSubmit={send}>
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
        <div className="composer">
          <textarea
            aria-label="Message agent"
            placeholder={
              chat.archived ? "This chat is archived" : "Message your agent…"
            }
            value={text}
            onChange={(e) => {
              setText(e.target.value);
              if (e.target.value) reportTyping();
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
                  !text.trim() ||
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
