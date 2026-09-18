/* The chat's cost so far (the composer's /cost): what every recorded turn
   took, summed — turns, tokens by kind, the provider's cost estimate
   where it gives one (Claude), the turns' wall time — shown as a card in
   the transcript that is local to the reader and never sent to the
   agent. The service keeps one record per turn (`Turn`); the sums are
   made here from `conversation.turns`. */
import { formatCost, formatDuration, formatTokens } from "./turns";
import type { Entry, Turn, Usage } from "./types";

export type CostSummary = {
  turns: number;
  /* Turns whose usage the provider reported. */
  reported: number;
  usage: Usage;
  /* Whether any turn carried a cost estimate; Codex reports none. */
  priced: boolean;
  /* The turns' wall time, in seconds, from the ones that ended. */
  seconds: number;
  /* A turn still running counts up at `now`. */
  running: boolean;
  /* Answered side questions (R2.19), in the usage above and marked here
     with their share of the cost. */
  asides: number;
  asideCostUSD: number;
};

/* The chat's cost from its turn records, plus its answered side
   questions when the entries are given (their tokens and cost are the
   chat's spend as much as the turns'). */
export function sessionCost(
  turns: Turn[] | undefined,
  now?: number,
  entries?: Entry[],
): CostSummary {
  const usage: Usage = {
    input: 0,
    cached: 0,
    cacheWrite: 0,
    output: 0,
    reasoning: 0,
    total: 0,
    costUSD: 0,
  };
  let reported = 0;
  let priced = false;
  let seconds = 0;
  let running = false;
  for (const t of turns || []) {
    if (t.usage) {
      reported++;
      usage.input += t.usage.input;
      usage.cached += t.usage.cached;
      usage.cacheWrite = (usage.cacheWrite || 0) + (t.usage.cacheWrite || 0);
      usage.output += t.usage.output;
      usage.reasoning = (usage.reasoning || 0) + (t.usage.reasoning || 0);
      usage.total += t.usage.total;
      if (t.usage.costUSD) {
        priced = true;
        usage.costUSD = (usage.costUSD || 0) + t.usage.costUSD;
      }
    }
    if (t.startedAt) {
      if (t.endedAt) seconds += Math.max(0, t.endedAt - t.startedAt);
      else if (now !== undefined) {
        seconds += Math.max(0, now - t.startedAt);
        running = true;
      }
    }
  }
  let asides = 0;
  let asideCostUSD = 0;
  for (const v of entries || []) {
    if (v.role !== "aside" || v.aside?.status !== "completed") continue;
    asides++;
    usage.input += v.aside.input || 0;
    usage.output += v.aside.output || 0;
    usage.total += (v.aside.input || 0) + (v.aside.output || 0);
    if (v.aside.costUSD) {
      priced = true;
      usage.costUSD = (usage.costUSD || 0) + v.aside.costUSD;
      asideCostUSD += v.aside.costUSD;
    }
  }
  return {
    turns: (turns || []).length,
    reported,
    usage,
    priced,
    seconds,
    running,
    asides,
    asideCostUSD,
  };
}

/* The card's rows: label and value. */
export function costRows(c: CostSummary): [string, string][] {
  const rows: [string, string][] = [
    ["Turns", String(c.turns) + (c.reported < c.turns ? ` (${c.reported} with usage)` : "")],
    ["Input tokens", formatTokens(c.usage.input)],
  ];
  if (c.usage.cached) rows.push(["  read from cache", formatTokens(c.usage.cached)]);
  if (c.usage.cacheWrite) rows.push(["  written to cache", formatTokens(c.usage.cacheWrite)]);
  rows.push(["Output tokens", formatTokens(c.usage.output)]);
  if (c.usage.reasoning) rows.push(["  thinking", formatTokens(c.usage.reasoning)]);
  rows.push(["Total tokens", formatTokens(c.usage.total)]);
  rows.push([
    "Cost",
    c.priced ? formatCost(c.usage.costUSD || 0) : "not reported by this agent",
  ]);
  if (c.asides)
    rows.push([
      "  of it, side questions",
      `${c.asides} · ${c.asideCostUSD ? formatCost(c.asideCostUSD) : "no cost reported"}`,
    ]);
  rows.push([
    "Time in turns",
    formatDuration(c.seconds) + (c.running ? " (one running)" : ""),
  ]);
  return rows;
}

/* The one-line version, for a status line or a notice. */
export function costLine(c: CostSummary): string {
  const parts = [
    `${c.turns} turn${c.turns === 1 ? "" : "s"}`,
    `${formatTokens(c.usage.total)} tokens (${formatTokens(c.usage.input)} in, ${formatTokens(c.usage.output)} out)`,
  ];
  if (c.priced) parts.push(formatCost(c.usage.costUSD || 0));
  if (c.asides) parts.push(`${c.asides} side question${c.asides === 1 ? "" : "s"}`);
  if (c.seconds > 0) parts.push(formatDuration(c.seconds));
  return parts.join(" · ");
}
