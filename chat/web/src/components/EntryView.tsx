// Adapted from Panta Conversation.tsx at bf61d5b; presentation retained, app dependencies removed.
import { memo, useCallback, useMemo } from "react";
import {
  Bot,
  Check,
  ChevronRight,
  Copy,
  FileText,
  GitFork,
  History,
  LoaderCircle,
  MessageCircleQuestion,
  Pencil,
  RotateCcw,
  Send,
  User,
  X,
} from "lucide-react";
import { compactionLabel } from "../context";
import { hasDiff, parseDiff } from "../diff";
import { senderLabel } from "../export";
import { subagentInput, subagentProgress } from "../tools";
import { groupEntries } from "../transcript";
import {
  formatCost,
  formatDuration,
  formatTokens,
  type TurnFooter,
} from "../turns";
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
  const { steps, running, lastTool } = subagentProgress(
    entries,
    parent.tool?.progress,
  );
  const label = subagentInput(parent.tool).type || "Subagent";
  const messages = entries.filter((e) => !e.tool).length;
  const parts = [];
  if (steps) parts.push(`${steps} tool call${steps === 1 ? "" : "s"}`);
  if (messages) parts.push(`${messages} message${messages === 1 ? "" : "s"}`);
  if (running && lastTool) parts.push(`using ${lastTool}`);
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
/* Under a queued message (queue.ts): edit takes it out of the queue into
   the composer, withdraw drops it, and Send lets a held queue go. Always
   shown: the queue is something to act on, not to discover on hover. */
function QueuedActions({
  entry,
  queue,
  onEditQueued,
  onWithdraw,
  onSendQueued,
}: {
  entry: Entry;
  queue: QueueState;
  onEditQueued?: (entry: Entry) => void;
  onWithdraw?: (entry: Entry) => void;
  onSendQueued?: () => void;
}) {
  return (
    <div
      className="message-actions shown queued-actions"
      role="group"
      aria-label="Queued message actions"
    >
      {queue.held && onSendQueued && (
        <button
          type="button"
          className="ghost queued-send"
          title="Send the queued messages now, in order"
          onClick={onSendQueued}
        >
          <Send size={14} /> Send
        </button>
      )}
      {onEditQueued && (
        <button
          type="button"
          className="ghost icon"
          aria-label="Edit this queued message"
          title={
            queue.mine
              ? "Edit: takes it out of the queue and into the composer"
              : "Only its sender or the owner can edit it"
          }
          disabled={!queue.mine}
          onClick={() => onEditQueued(entry)}
        >
          <Pencil size={14} />
        </button>
      )}
      {onWithdraw && (
        <button
          type="button"
          className="ghost icon"
          aria-label="Withdraw this queued message"
          title={
            queue.mine
              ? "Withdraw: the agent never sees it"
              : "Only its sender or the owner can withdraw it"
          }
          disabled={!queue.mine}
          onClick={() => onWithdraw(entry)}
        >
          <X size={14} />
        </button>
      )}
    </div>
  );
}

/* What a queued message's card shows (queue.ts): the line beside the
   sender, whether the queue is held, and whether this person may act on
   the message. */
export type QueueState = { label: string; held: boolean; mine: boolean };

/* Under every message: copy its markdown source, and for one the owner
   sent, retry and edit. Shown on hover or focus (always when the message
   failed to deliver, since retrying is the fix); `enabled` is false while a
   send would be refused, and `editable` while a rewind would be (editing
   a message the agent got rewinds the conversation to before it: queue.ts),
   so the buttons still show what is possible instead of failing in the
   composer. Copy waits for a streaming message to finish, so the clipboard
   never holds half a message. */
function MessageActions({
  entry,
  enabled,
  editable,
  onEdit,
  onRetry,
  onRewind,
  rewindable,
  onFork,
}: {
  entry: Entry;
  enabled: boolean;
  editable?: boolean;
  onEdit?: (entry: Entry) => void;
  onRetry?: (entry: Entry) => void;
  /* Opens the rewind chooser on this message (rewind.ts); disabled while
     the chat is busy. */
  onRewind?: (entry: Entry) => void;
  rewindable?: boolean;
  /* Opens the fork dialog cut before this message; same condition. */
  onFork?: (entry: Entry) => void;
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
          title="Edit and resend: the conversation goes back to before this message"
          disabled={!editable}
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
      {onRewind && (
        <button
          type="button"
          className="ghost icon"
          aria-label="Rewind to before this message"
          title="Rewind to before this message (code, conversation or both)"
          disabled={!rewindable}
          onClick={() => onRewind(entry)}
        >
          <History size={14} />
        </button>
      )}
      {onFork && (
        <button
          type="button"
          className="ghost icon"
          aria-label="Fork the chat before this message"
          title="Fork the chat before this message: a sibling chat continues from here"
          disabled={!rewindable}
          onClick={() => onFork(entry)}
        >
          <GitFork size={14} />
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
  onRewind,
  onFork,
  onQuote,
  onEditQueued,
  onWithdraw,
  onSendQueued,
  queue,
  actions = false,
  editable = false,
  rewindable = false,
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
  onRewind?: (entry: Entry) => void;
  onFork?: (entry: Entry) => void;
  /* For a command the person ran: quote it into the composer. */
  onQuote?: (entry: Entry) => void;
  /* For a queued message (queue.ts): edit it in the composer, withdraw
     it, let a held queue go; `queue` says what its card shows. */
  onEditQueued?: (entry: Entry) => void;
  onWithdraw?: (entry: Entry) => void;
  onSendQueued?: () => void;
  queue?: QueueState;
  /* Whether retry would be accepted right now. */
  actions?: boolean;
  /* Whether edit-and-resend would be (a rewind is possible). */
  editable?: boolean;
  /* Whether a rewind would be accepted right now (the chat is idle). */
  rewindable?: boolean;
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
  if (entry.role === "system" || entry.role === "notice")
    return (
      <div className={`system-entry ${entry.role}-entry`} data-entry={entry.id}>
        {entry.text}
      </div>
    );
  if (entry.role === "rewind")
    // The marker a rewind leaves: which message the chat went back to
    // before and what was taken back (rewind.ts).
    return (
      <div
        className="system-entry rewind-entry"
        data-entry={entry.id}
        role="separator"
        aria-label={entry.text}
      >
        <span>
          <History size={13} aria-hidden="true" /> {entry.text}
        </span>
        {entry.detail && <small>{entry.detail}</small>}
      </div>
    );
  if (entry.role === "compaction")
    return <CompactionDivider entry={entry} chatID={chatID} onFile={onFile} />;
  if (entry.role === "fork")
    // The marker a fork leaves at the top of the copy: which chat it came
    // from (a link) and where the copy stops.
    return (
      <div
        className="system-entry rewind-entry fork-entry"
        data-entry={entry.id}
        role="separator"
        aria-label={entry.text}
      >
        <span>
          <GitFork size={13} aria-hidden="true" />{" "}
          {entry.fork?.chatID ? (
            <>
              Forked from{" "}
              <a href={"?chat=" + encodeURIComponent(entry.fork.chatID)}>
                {entry.fork.title || "another chat"}
              </a>
              {entry.fork.messageID
                ? entry.text.replace(/^Forked from “[^”]*”/, "")
                : ""}
            </>
          ) : (
            entry.text
          )}
        </span>
        {entry.detail && <small>{entry.detail}</small>}
      </div>
    );
  if (entry.role === "aside")
    return <AsideCard entry={entry} chatID={chatID} onFile={onFile} />;
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
      {entry.delivery === "queued" && (
        <span className="muted queued-label">{queue?.label ?? "Queued"}</span>
      )}
      {entry.delivery === "failed" && (
        <span className="danger-text">{entry.detail || "Not delivered"}</span>
      )}
    </header>
  );
  if (user) {
    const queued = entry.delivery === "queued" && !!queue;
    return (
      <article
        className={`message message-user${queued ? " message-queued" : ""}`}
        data-entry={entry.id}
      >
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
        {queued ? (
          <QueuedActions
            entry={entry}
            queue={queue}
            onEditQueued={onEditQueued}
            onWithdraw={onWithdraw}
            onSendQueued={onSendQueued}
          />
        ) : (
          <MessageActions
            entry={entry}
            enabled={actions}
            editable={editable}
            onEdit={onEdit}
            onRetry={onRetry}
            onRewind={onRewind}
            onFork={onFork}
            rewindable={rewindable}
          />
        )}
      </article>
    );
  }
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

/* A side question (/btw) and its answer: asked by a person, answered from
   a copy of the agent's session, never part of the conversation the agent
   sees. The card says so, with what the answer cost. */
function AsideCard({
  entry,
  chatID,
  onFile,
}: {
  entry: Entry;
  chatID: string;
  onFile: (href: string) => void;
}) {
  const a = entry.aside ?? { status: "completed" as const };
  const running =
    a.status === "running" || (entry.isStreaming && !entry.detail);
  const failed = a.status === "failed";
  const facts: string[] = [];
  if (a.durationMS) facts.push(formatDuration(a.durationMS / 1000));
  if (a.input || a.output)
    facts.push(`${formatTokens((a.input || 0) + (a.output || 0))} tokens`);
  if (a.costUSD) facts.push(formatCost(a.costUSD));
  return (
    <article
      className={`aside-entry${running ? " running" : failed ? " failed" : ""}`}
      data-entry={entry.id}
      aria-label="Side question"
    >
      <header>
        <MessageCircleQuestion size={14} aria-hidden="true" />
        <strong>Side question</strong>
        <span className="muted">
          by {senderLabel(entry.sender)} · {time(entry.createdAt)} · not sent to
          the agent
        </span>
      </header>
      <p className="aside-question">{entry.text}</p>
      {running ? (
        <p className="aside-answer muted">
          Answering from a copy of the session
          <span className="typing-dots">…</span>
        </p>
      ) : failed ? (
        <p className="aside-answer danger-text">
          Could not answer{a.error ? `: ${a.error}` : ""}
        </p>
      ) : (
        <div className="aside-answer">
          <RichText
            text={entry.detail}
            chatID={chatID}
            entryID={entry.id}
            onFile={onFile}
            agent
          />
        </div>
      )}
      {facts.length > 0 && <p className="aside-facts">{facts.join(" · ")}</p>}
    </article>
  );
}
