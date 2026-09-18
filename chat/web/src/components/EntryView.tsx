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
  Send,
  User,
} from "lucide-react";
import { hasDiff, parseDiff } from "../diff";
import { senderLabel } from "../export";
import { subagentInput, subagentProgress } from "../tools";
import { groupEntries } from "../transcript";
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
function StepSummary({ entry, steps }: { entry: Entry; steps?: Entry[] }) {
  if (entry.tool) return <ToolSummary entry={entry} steps={steps} />;
  return (
    <>
      <FileText size={13} />
      <span>{entry.text || "Agent activity"}</span>
      {entry.isStreaming && <LoaderCircle size={12} className="spin" />}
    </>
  );
}
/* What a card's body needs beyond its entry: the file opener, and for a
   subagent's card the transcript nested under it. */
type StepContext = {
  onFile?: (href: string) => void;
  nested?: Map<string, Entry[]>;
  chatID?: string;
  provider?: string;
};
function StepBody({ entry, ctx }: { entry: Entry; ctx: StepContext }) {
  if (!entry.tool) return <ActivityDetail text={entry.detail} />;
  const children = ctx.nested?.get(entry.id);
  return (
    <ToolBody
      entry={entry}
      onFile={ctx.onFile}
      nested={
        entry.tool.kind === "task" && children?.length ? (
          <SubagentTranscript parent={entry} entries={children} ctx={ctx} />
        ) : undefined
      }
    />
  );
}
/* A subagent's own work, inside its card: its messages and tool cards as
   the transcript shows the conversation's, collapsed behind a count until
   opened. A subagent's subagent nests the same way. */
function SubagentTranscript({
  parent,
  entries,
  ctx,
}: {
  parent: Entry;
  entries: Entry[];
  ctx: StepContext;
}) {
  const { steps, running } = subagentProgress(entries);
  const label = subagentInput(parent.tool).type || "Subagent";
  const messages = entries.length - steps;
  const parts = [];
  if (steps) parts.push(`${steps} tool call${steps === 1 ? "" : "s"}`);
  if (messages) parts.push(`${messages} message${messages === 1 ? "" : "s"}`);
  return (
    <details className="subagent">
      <summary>
        <ChevronRight size={14} className="chevron" />
        <span>
          {label}: {parts.join(", ") || "no activity yet"}
        </span>
        {running && <LoaderCircle size={12} className="spin" />}
      </summary>
      <div className="subagent-transcript">
        {groupEntries(entries).map((item) =>
          "group" in item ? (
            <ActivityGroup
              key={item.group[0].id}
              entries={item.group}
              nested={ctx.nested}
              chatID={ctx.chatID}
              provider={ctx.provider}
              onFile={ctx.onFile}
            />
          ) : (
            <EntryView
              key={item.entry.id}
              entry={item.entry}
              chatID={ctx.chatID ?? ""}
              provider={ctx.provider}
              label={label}
              nested={ctx.nested}
              onFile={ctx.onFile ?? (() => {})}
            />
          ),
        )}
      </div>
    </details>
  );
}
/* A run of consecutive tool steps collapses into one row; a todo list
   stands open on its own. */
export const ActivityGroup = memo(function ActivityGroup({
  entries,
  onFile,
  nested,
  chatID,
  provider,
  onQuote,
}: {
  entries: Entry[];
  /* Opens a workspace file a step names (a read's path). */
  onFile?: (href: string) => void;
  /* Subagents' entries by the ID of their card (transcript.ts
     `nestEntries`), for the cards of Agent calls. */
  nested?: Map<string, Entry[]>;
  chatID?: string;
  provider?: string;
  /* Puts a command the person ran (a card with a sender) into the
     composer for the agent: the agent sees such a card only that way. */
  onQuote?: (entry: Entry) => void;
}) {
  const ctx: StepContext = { onFile, nested, chatID, provider };
  const streaming = entries.some((e) => e.isStreaming);
  const latest = entries[entries.length - 1];
  // `data-entry` is how the find bar lands on an entry from the palette.
  if (entries.length === 1)
    return (
      <details
        className={`activity-group${latest.tool ? ` tool-card tool-kind-${latest.tool.kind}` : ""}${latest.sender ? " by-person" : ""}`}
        data-entry={latest.id}
        open={latest.tool?.kind === "todo" || !!latest.sender || undefined}
      >
        <summary>
          <ChevronRight size={14} className="chevron" />
          {latest.tool ? (
            <ToolSummary entry={latest} steps={nested?.get(latest.id)} />
          ) : (
            <>
              <span>{latest.text || "Agent activity"}</span>
              {streaming && <LoaderCircle size={12} className="spin" />}
            </>
          )}
        </summary>
        <StepBody entry={latest} ctx={ctx} />
        {latest.sender && onQuote && !latest.isStreaming && (
          <div className="tool-actions">
            <button
              type="button"
              className="ghost"
              title="Put this command and its output into the composer, for the agent"
              disabled={latest.tool?.status === "running"}
              onClick={() => onQuote(latest)}
            >
              <Send size={13} />
              Send to agent
            </button>
          </div>
        )}
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
            open={entry.tool?.kind === "todo" || undefined}
          >
            <summary>
              <StepSummary entry={entry} steps={nested?.get(entry.id)} />
            </summary>
            <StepBody entry={entry} ctx={ctx} />
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
  onQuote,
  actions = false,
  stats,
  nested,
  label,
}: {
  entry: Entry;
  chatID: string;
  provider?: string;
  onFile: (href: string) => void;
  onEdit?: (entry: Entry) => void;
  onRetry?: (entry: Entry) => void;
  /* For a command the person ran: quote it into the composer. */
  onQuote?: (entry: Entry) => void;
  /* Whether retry and edit would be accepted right now. */
  actions?: boolean;
  /* The turn's timing and usage, under the turn's last message. */
  stats?: TurnFooter;
  /* Subagents' entries by the ID of their card, for a task entry. */
  nested?: Map<string, Entry[]>;
  /* The name over an agent message, instead of the provider's: the
     subagent's type inside its card. */
  label?: string;
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
    return (
      <ActivityGroup
        entries={[entry]}
        nested={nested}
        chatID={chatID}
        provider={provider}
        onFile={onFile}
        onQuote={onQuote}
      />
    );
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
  const user = entry.role === "user";
  const header = (
    <header>
      <strong>
        {user
          ? senderLabel(entry.sender)
          : label || (provider === "claude" ? "Claude" : "Codex")}
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
