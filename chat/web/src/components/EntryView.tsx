// Adapted from Panta Conversation.tsx at bf61d5b; presentation retained, app dependencies removed.
import { memo, useCallback, useMemo } from "react";
import {
  Bot,
  Check,
  ChevronRight,
  Copy,
  FileText,
  LoaderCircle,
  Pencil,
  RotateCcw,
  User,
} from "lucide-react";
import { compactionLabel } from "../context";
import { hasDiff, parseDiff } from "../diff";
import { senderLabel } from "../export";
import type { TurnFooter } from "../turns";
import type { Entry } from "../types";
import { EntryAttachments } from "./Attachments";
import { DiffView } from "./DiffView";
import { ImageAttachment } from "./ImageAttachment";
import { RichText } from "./RichText";
import { ThinkingBlock } from "./Thinking";
import { ToolBody, ToolSummary } from "./ToolCard";
import { TurnStats } from "./TurnStats";
import { useCopy } from "./useCopy";
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
/* One step's summary row: the typed card's (ToolCard.tsx) when the
   service recorded the tool call, else the step's text. */
function StepSummary({ entry }: { entry: Entry }) {
  if (entry.tool) return <ToolSummary entry={entry} />;
  return (
    <>
      <FileText size={13} />
      <span>{entry.text || "Agent activity"}</span>
      {entry.isStreaming && <LoaderCircle size={12} className="spin" />}
    </>
  );
}
function StepBody({
  entry,
  onFile,
}: {
  entry: Entry;
  onFile?: (href: string) => void;
}) {
  if (entry.tool) return <ToolBody entry={entry} onFile={onFile} />;
  return <ActivityDetail text={entry.detail} />;
}
/* A run of consecutive tool steps collapses into one row. */
export const ActivityGroup = memo(function ActivityGroup({
  entries,
  onFile,
}: {
  entries: Entry[];
  /* Opens a workspace file a step names (a read's path). */
  onFile?: (href: string) => void;
}) {
  const streaming = entries.some((e) => e.isStreaming);
  const latest = entries[entries.length - 1];
  // `data-entry` is how the find bar lands on an entry from the palette.
  if (entries.length === 1)
    return (
      <details
        className={`activity-group${latest.tool ? ` tool-card tool-kind-${latest.tool.kind}` : ""}`}
        data-entry={latest.id}
      >
        <summary>
          <ChevronRight size={14} className="chevron" />
          {latest.tool ? (
            <ToolSummary entry={latest} />
          ) : (
            <>
              <span>{latest.text || "Agent activity"}</span>
              {streaming && <LoaderCircle size={12} className="spin" />}
            </>
          )}
        </summary>
        <StepBody entry={latest} onFile={onFile} />
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
          <details
            className={`activity-entry${entry.tool ? ` tool-card tool-kind-${entry.tool.kind}` : ""}`}
            key={entry.id}
            data-entry={entry.id}
          >
            <summary>
              <StepSummary entry={entry} />
            </summary>
            <StepBody entry={entry} onFile={onFile} />
          </details>
        ))}
      </div>
    </details>
  );
});
/* Under every message: copy its markdown source, and for one the owner
   sent, retry and edit. Shown on hover or focus (always when the message
   failed to deliver, since retrying is the fix); `enabled` is false while a
   send would be refused, so the buttons still show what is possible instead
   of failing in the composer. Copy waits for a streaming message to finish,
   so the clipboard never holds half a message. */
function MessageActions({
  entry,
  enabled,
  onEdit,
  onRetry,
}: {
  entry: Entry;
  enabled: boolean;
  onEdit?: (entry: Entry) => void;
  onRetry?: (entry: Entry) => void;
}) {
  const { copied, copy } = useCopy(useCallback(() => entry.text, [entry.text]));
  return (
    <div
      className={`message-actions${entry.delivery === "failed" ? " shown" : ""}`}
      role="group"
      aria-label="Message actions"
    >
      <button
        type="button"
        className="ghost icon"
        aria-label={copied ? "Copied" : "Copy as markdown"}
        title="Copy as markdown"
        disabled={entry.isStreaming || !entry.text}
        onClick={copy}
      >
        {copied ? <Check size={14} /> : <Copy size={14} />}
      </button>
      {onEdit && (
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
      )}
      {onRetry && (
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
      )}
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
  stats,
}: {
  entry: Entry;
  chatID: string;
  provider?: string;
  onFile: (href: string) => void;
  onEdit?: (entry: Entry) => void;
  onRetry?: (entry: Entry) => void;
  /* Whether retry and edit would be accepted right now. */
  actions?: boolean;
  /* The turn's timing and usage, under the turn's last message. */
  stats?: TurnFooter;
}) {
  if (entry.role === "image")
    return (
      <ImageAttachment
        chatID={chatID}
        id={entry.detail}
        caption={entry.text}
        entryID={entry.id}
      />
    );
  if (entry.role === "activity")
    return <ActivityGroup entries={[entry]} onFile={onFile} />;
  if (entry.role === "thinking")
    return (
      <ThinkingBlock
        entry={entry}
        provider={provider}
        chatID={chatID}
        onFile={onFile}
      />
    );
  if (entry.role === "system")
    return (
      <div className="system-entry" data-entry={entry.id}>
        {entry.text}
      </div>
    );
  if (entry.role === "compaction")
    return <CompactionDivider entry={entry} chatID={chatID} onFile={onFile} />;
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
      <article className="message message-user" data-entry={entry.id}>
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
        <MessageActions
          entry={entry}
          enabled={actions}
          onEdit={onEdit}
          onRetry={onRetry}
        />
      </article>
    );
  return (
    <article className={`message message-${entry.role}`} data-entry={entry.id}>
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
          agent
        />
        <div className="message-foot">
          <MessageActions entry={entry} enabled={false} />
          {stats && <TurnStats footer={stats} />}
        </div>
      </div>
    </article>
  );
});

/* The divider where the agent compacted its context: "Context compacted ·
   manual · 171k → 2.2k tokens", with the summary it continues from
   behind a disclosure; "Compacting context…" while it runs; the error
   when it failed. */
function CompactionDivider({
  entry,
  chatID,
  onFile,
}: {
  entry: Entry;
  chatID: string;
  onFile: (href: string) => void;
}) {
  const c = entry.compaction ?? { status: "completed" };
  const running = c.status === "running" || (entry.isStreaming && !c.trigger);
  const failed = c.status === "failed";
  const label = compactionLabel(c);
  return (
    <div
      className={`compaction-entry${running ? " running" : failed ? " failed" : ""}`}
      data-entry={entry.id}
      role="separator"
      aria-label={entry.text}
    >
      <div className="compaction-line">
        <span>
          {running
            ? "Compacting context…"
            : failed
              ? `Compaction failed${c.error ? `: ${c.error}` : ""}`
              : label
                ? `Context compacted · ${label}`
                : "Context compacted"}
        </span>
      </div>
      {!running && !failed && entry.detail && (
        <details className="compaction-summary">
          <summary>Summary the agent continues from</summary>
          <RichText
            text={entry.detail}
            chatID={chatID}
            entryID={entry.id}
            onFile={onFile}
            agent
          />
        </details>
      )}
    </div>
  );
}
