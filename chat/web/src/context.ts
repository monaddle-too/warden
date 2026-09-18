/* The agent's context against its window, for the meter beside the model
   and the compaction divider: the pure parts. Claude Code compacts the
   conversation on its own as the context nears the window (the history is
   replaced by a summary), so the meter warns before that happens and the
   divider says when it did. */
import { formatTokens } from "./turns";
import type { Compaction, Context } from "./types";

/* From what fraction of the way to a compaction the meter turns amber,
   then red: the way to the agent's threshold when it reports one (Claude
   Code compacts at the window less a buffer, 33k on the 200k models),
   else to the window. Red means "expect a compaction on the next call or
   two". */
export const WARN_AT = 0.8;
export const HIGH_AT = 0.95;

export type ContextLevel = "ok" | "warn" | "high";

export type ContextSummary = {
  /* used / window, clamped to [0, 1]; 0 when the window is unknown. */
  fraction: number;
  /* Whole percent of the window used. */
  percent: number;
  /* The way to the next automatic compaction: used / threshold (or the
     window), clamped to [0, 1]. */
  toCompaction: number;
  /* "42k / 200k" (just "42k" when the window is unknown). */
  label: string;
  level: ContextLevel;
  /* The hover text: exact counts and what happens near the window. */
  title: string;
};

export function contextLevel(fraction: number): ContextLevel {
  if (fraction >= HIGH_AT) return "high";
  if (fraction >= WARN_AT) return "warn";
  return "ok";
}

export function contextSummary(ctx: Context): ContextSummary {
  const clamp = (v: number) => Math.min(1, Math.max(0, v));
  const fraction = ctx.window > 0 ? clamp(ctx.used / ctx.window) : 0;
  const limit =
    ctx.threshold && ctx.threshold > 0 && ctx.threshold < ctx.window
      ? ctx.threshold
      : ctx.window;
  const toCompaction = limit > 0 ? clamp(ctx.used / limit) : 0;
  const percent = Math.round(fraction * 100);
  const level = contextLevel(toCompaction);
  const label =
    ctx.window > 0
      ? `${formatTokens(ctx.used)} / ${formatTokens(ctx.window)}`
      : formatTokens(ctx.used);
  const n = (v: number) => v.toLocaleString();
  const lines = [
    ctx.window > 0
      ? `Context: ${n(ctx.used)} of ${n(ctx.window)} tokens (${percent} %)${ctx.model ? ` on ${ctx.model}` : ""}`
      : `Context: ${n(ctx.used)} tokens${ctx.model ? ` on ${ctx.model}` : ""}`,
    "What the model was given on its last call.",
  ];
  const at =
    ctx.threshold && ctx.threshold > 0
      ? ` at about ${n(ctx.threshold)} tokens`
      : "";
  if (level === "ok")
    lines.push(
      `Claude compacts the conversation automatically${at || " as it nears the window"}; /compact [instructions] does it now.`,
    );
  else
    lines.push(
      level === "high"
        ? `Nearly there: Claude will compact the conversation automatically${at} on one of its next calls, replacing the history with a summary.`
        : `Nearing the limit: Claude will compact the conversation automatically${at} soon, replacing the history with a summary.`,
      "/compact [instructions] compacts now and lets you say what to keep.",
    );
  return {
    fraction,
    percent,
    toCompaction,
    label,
    level,
    title: lines.join("\n"),
  };
}

/* The divider's text after "Context compacted": how it was triggered and
   what it came down to. "manual · 171k → 2.2k tokens"; "auto"; "". */
export function compactionLabel(c: Compaction): string {
  const parts: string[] = [];
  if (c.trigger === "manual") parts.push("manual");
  else if (c.trigger === "auto") parts.push("automatic");
  else if (c.trigger) parts.push(c.trigger);
  if (c.preTokens && c.postTokens)
    parts.push(
      `${formatTokens(c.preTokens)} → ${formatTokens(c.postTokens)} tokens`,
    );
  else if (c.preTokens) parts.push(`from ${formatTokens(c.preTokens)} tokens`);
  return parts.join(" · ");
}

/* The whole divider as one line, for the export and the status. */
export function compactionText(c: Compaction): string {
  if (c.status === "running") return "Compacting context…";
  if (c.status === "failed")
    return `Compaction failed${c.error ? `: ${c.error}` : ""}`;
  const label = compactionLabel(c);
  return label ? `Context compacted · ${label}` : "Context compacted";
}
