// The model at work, in the manner of each agent's own desktop app. A
// thinking entry is a disclosure: for Claude folded under "Thinking…" with
// a shimmer, then "Thought for 12s", as claude.ai shows extended thinking;
// for Codex the reasoning summary streams in the open under a shimmering
// "Thinking" and folds when the model moves on. Before anything streams,
// Claude's avatar pulses the way claude.ai's mark does, and Codex shows the
// "Working" line with the turn's elapsed time that its app and CLI show.
import { memo, useEffect, useState } from "react";
import { Bot, ChevronRight } from "lucide-react";
import { providerOf, thinkingLabel, thinkingOpen } from "../thinking";
import { formatDuration } from "../turns";
import type { Entry } from "../types";
import { RichText } from "./RichText";

export const ThinkingBlock = memo(function ThinkingBlock({
  entry,
  provider,
  chatID,
  onFile,
}: {
  entry: Entry;
  provider?: string;
  chatID: string;
  onFile: (href: string) => void;
}) {
  const kind = providerOf(provider);
  // The reader's own toggle sticks; until then the provider's default,
  // which changes once when the thinking ends.
  const [chosen, setChosen] = useState<boolean>();
  const open = chosen ?? thinkingOpen(kind, entry.isStreaming);
  const label = thinkingLabel(kind, entry);
  return (
    <details
      className={`thinking thinking-${kind}${entry.isStreaming ? " streaming" : ""}`}
      open={open}
      data-entry={entry.id}
      onToggle={(event) => {
        if (event.currentTarget.open !== open)
          setChosen(event.currentTarget.open);
      }}
    >
      <summary aria-label={`${label}, the model's thinking`}>
        <ChevronRight size={14} className="chevron" />
        <span className={entry.isStreaming ? "shimmer" : ""}>{label}</span>
      </summary>
      <div className="thinking-body">
        {entry.text ? (
          <RichText
            text={entry.text}
            streaming={entry.isStreaming}
            chatID={chatID}
            entryID={entry.id}
            onFile={onFile}
            agent
          />
        ) : (
          <p className="muted">{entry.isStreaming ? "…" : "No summary."}</p>
        )}
      </div>
    </details>
  );
});

/* The row that stands for a reply not yet begun. */
export function PendingReply({
  provider,
  since,
}: {
  provider?: string;
  /* When the message being answered was sent, for Codex's elapsed count. */
  since?: number;
}) {
  const kind = providerOf(provider);
  const [now, setNow] = useState(() => Date.now() / 1000);
  useEffect(() => {
    if (kind !== "codex" || since === undefined) return;
    setNow(Date.now() / 1000);
    const id = setInterval(() => setNow(Date.now() / 1000), 1000);
    return () => clearInterval(id);
  }, [kind, since]);
  if (kind === "claude")
    return (
      <div
        className="message message-assistant pending-reply pending-claude"
        role="status"
        aria-label="Claude is thinking"
      >
        <div className="message-avatar pulse" aria-hidden="true">
          <Bot size={16} />
        </div>
        <div className="message-body">
          <span className="shimmer">Thinking…</span>
        </div>
      </div>
    );
  const elapsed = since === undefined ? 0 : Math.floor(now - since);
  return (
    <p
      className="pending-reply pending-codex"
      role="status"
      aria-label="Codex is working"
    >
      <span className="shimmer">Working</span>
      {elapsed >= 1 && <span className="muted">{formatDuration(elapsed)}</span>}
    </p>
  );
}
