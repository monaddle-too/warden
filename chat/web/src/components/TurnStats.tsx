// The line under a turn's last agent entry: how long the turn took, from
// the owner's message to its end, and the tokens (and cost, where the
// provider estimates one) it used. A running turn counts up once a second.
import { useEffect, useState } from "react";
import { Timer } from "lucide-react";
import {
  formatCost,
  formatDuration,
  usageDetail,
  usageSummary,
  type TurnFooter,
} from "../turns";

export function TurnStats({
  footer,
  block = false,
}: {
  footer: TurnFooter;
  /* Standing on its own after a tool-step group rather than in a
     message's action row. */
  block?: boolean;
}) {
  const [now, setNow] = useState(() => Date.now() / 1000);
  useEffect(() => {
    if (!footer.active) return;
    setNow(Date.now() / 1000);
    const id = setInterval(() => setNow(Date.now() / 1000), 1000);
    return () => clearInterval(id);
  }, [footer.active]);
  const end = footer.end ?? (footer.active ? now : undefined);
  const seconds =
    footer.start !== undefined && end !== undefined
      ? end - footer.start
      : undefined;
  const { usage } = footer;
  if (seconds === undefined && !usage) return null;
  return (
    <p
      className={`turn-stats${block ? " block" : ""}${footer.active ? " active" : ""}`}
      aria-label={footer.active ? "Turn in progress" : "Turn"}
    >
      <Timer size={12} aria-hidden="true" />
      {seconds !== undefined && <span>{formatDuration(seconds)}</span>}
      {/* A /compact turn reports no tokens, only the compaction's cost. */}
      {usage && usage.total > 0 && (
        <span title={usageDetail(usage)}>{usageSummary(usage)}</span>
      )}
      {!!usage?.costUSD && <span>{formatCost(usage.costUSD)}</span>}
    </p>
  );
}
