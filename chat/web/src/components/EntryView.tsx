// Adapted from Panta Conversation.tsx at bf61d5b; presentation retained, app dependencies removed.
import { memo, useMemo } from "react";
import {
  Bot,
  ChevronRight,
  FileText,
  LoaderCircle,
  Pencil,
  RotateCcw,
  User,
} from "lucide-react";
import { hasDiff, parseDiff } from "../diff";
import type { Entry } from "../types";
import { EntryAttachments } from "./Attachments";
import { DiffView } from "./DiffView";
import { ImageAttachment } from "./ImageAttachment";
import { RichText } from "./RichText";
const time = (v: number) =>
  new Date(v * 1000).toLocaleTimeString([], {
    hour: "2-digit",
    minute: "2-digit",
  });
/* A step's detail: a file edit (Codex's fileChange, a `git diff` an agent
   ran) shows as a diff, anything else as the raw text. */
function ActivityDetail({ text }: { text: string }) {
  const segments = useMemo(() => parseDiff(text), [text]);
  return hasDiff(segments) ? (
    <div className="activity-detail">
      <DiffView segments={segments} />
    </div>
  ) : (
    <pre>{text}</pre>
  );
}
/* A run of consecutive tool steps collapses into one row. */
export const ActivityGroup = memo(function ActivityGroup({
  entries,
}: {
  entries: Entry[];
}) {
  const streaming = entries.some((e) => e.isStreaming);
  const latest = entries[entries.length - 1];
  if (entries.length === 1)
    return (
      <details className="activity-group">
        <summary>
          <ChevronRight size={14} className="chevron" />
          <span>{latest.text || "Agent activity"}</span>
          {streaming && <LoaderCircle size={12} className="spin" />}
        </summary>
        <ActivityDetail text={latest.detail} />
      </details>
    );
  return (
    <details className="activity-group">
      <summary>
        <ChevronRight size={14} className="chevron" />
        <span>{entries.length} steps</span>
        {streaming && <LoaderCircle size={12} className="spin" />}
        <span className="muted">{latest.text}</span>
      </summary>
      <div className="activity-list">
        {entries.map((entry) => (
          <details className="activity-entry" key={entry.id}>
            <summary>
              <FileText size={13} />
              <span>{entry.text || "Agent activity"}</span>
              {entry.isStreaming && <LoaderCircle size={12} className="spin" />}
            </summary>
            <ActivityDetail text={entry.detail} />
          </details>
        ))}
      </div>
    </details>
  );
});
// Who wrote a user entry: their Google name, else their email, else the
// owner ("You" on a local install, where the owner is the only person).
export function senderLabel(sender?: Entry["sender"]) {
  if (!sender) return "You";
  if (sender.name) return sender.name;
  if (sender.email) return sender.email;
  return sender.principalID === "owner" ? "You" : "Collaborator";
}

/* Retry and edit for a message the owner sent. Shown on hover or focus
   (always when the message failed to deliver, since retrying is the fix);
   `enabled` is false while a send would be refused, so the buttons still
   show what is possible instead of failing in the composer. */
function UserActions({
  entry,
  enabled,
  onEdit,
  onRetry,
}: {
  entry: Entry;
  enabled: boolean;
  onEdit: (entry: Entry) => void;
  onRetry: (entry: Entry) => void;
}) {
  return (
    <div
      className={`message-actions${entry.delivery === "failed" ? " shown" : ""}`}
      role="group"
      aria-label="Message actions"
    >
      <button
        type="button"
        className="ghost icon"
        aria-label="Edit and resend"
        title="Edit and resend"
        disabled={!enabled}
        onClick={() => onEdit(entry)}
      >
        <Pencil size={14} />
      </button>
      <button
        type="button"
        className="ghost icon"
        aria-label="Retry"
        title={
          entry.delivery === "failed"
            ? "Send this message again"
            : "Send this message again as a new message"
        }
        disabled={!enabled}
        onClick={() => onRetry(entry)}
      >
        <RotateCcw size={14} />
      </button>
    </div>
  );
}

export const EntryView = memo(function EntryView({
  entry,
  chatID,
  provider,
  onFile,
  onEdit,
  onRetry,
  actions = false,
}: {
  entry: Entry;
  chatID: string;
  provider?: string;
  onFile: (href: string) => void;
  onEdit?: (entry: Entry) => void;
  onRetry?: (entry: Entry) => void;
  /* Whether retry and edit would be accepted right now. */
  actions?: boolean;
}) {
  if (entry.role === "image")
    return (
      <ImageAttachment chatID={chatID} id={entry.detail} caption={entry.text} />
    );
  if (entry.role === "activity") return <ActivityGroup entries={[entry]} />;
  if (entry.role === "system")
    return <div className="system-entry">{entry.text}</div>;
  const user = entry.role === "user";
  const header = (
    <header>
      <strong>
        {user
          ? senderLabel(entry.sender)
          : provider === "claude"
            ? "Claude"
            : "Codex"}
      </strong>
      <time>{time(entry.createdAt)}</time>
      {entry.isStreaming && (
        <span className="stream-label">
          Writing<span className="typing-dots">…</span>
        </span>
      )}
      {entry.delivery === "queued" && <span className="muted">Queued</span>}
      {entry.delivery === "failed" && (
        <span className="danger-text">{entry.detail || "Not delivered"}</span>
      )}
    </header>
  );
  if (user)
    return (
      <article className="message message-user">
        {header}
        <div className="message-body">
          {entry.text && (
            <RichText
              text={entry.text}
              chatID={chatID}
              entryID={entry.id}
              onFile={onFile}
            />
          )}
          {!!entry.attachments?.length && (
            <EntryAttachments chatID={chatID} attachments={entry.attachments} />
          )}
        </div>
        {onEdit && onRetry && (
          <UserActions
            entry={entry}
            enabled={actions}
            onEdit={onEdit}
            onRetry={onRetry}
          />
        )}
      </article>
    );
  return (
    <article className={`message message-${entry.role}`}>
      <div className="message-avatar" aria-hidden="true">
        {entry.role === "user" ? <User size={15} /> : <Bot size={16} />}
      </div>
      <div className="message-body">
        {header}
        <RichText
          text={entry.text}
          streaming={entry.isStreaming}
          chatID={chatID}
          entryID={entry.id}
          onFile={onFile}
        />
      </div>
    </article>
  );
});
