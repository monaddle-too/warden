/* What each agent turn took, for the line under its last message: the time
   from the owner's message to the end of the turn and the tokens the
   provider reported. The service keeps a record per turn (`Turn`); entries
   name their turn, so the line goes under the turn's last agent entry. */
import type { Turn, Usage } from "./types";

type Item = {
  id: string;
  role: string;
  createdAt: number;
  turnID?: string;
};

export type TurnFooter = {
  turnID: string;
  /* When the owner sent the message that started the turn (the service
     accepting it, when that message is gone). */
  start?: number;
  /* When the turn ended; unset while it runs, or when a restart cut it
     short. */
  end?: number;
  usage?: Usage;
  /* The turn is the one running now, so the line keeps counting. */
  active: boolean;
};

/* Where each turn's line goes: keyed by the ID of the turn's last agent
   entry. A turn from before the service kept records has no line unless it
   is running; a running turn's line counts up until its record ends. */
export function turnFooters<T extends Item>(
  entries: T[],
  turns: Turn[] | undefined,
  running: boolean,
): Map<string, TurnFooter> {
  const last = new Map<string, string>();
  const sent = new Map<string, number>();
  let current = "";
  for (const e of entries) {
    if (!e.turnID) continue;
    current = e.turnID;
    if (e.role === "user") {
      if (!sent.has(e.turnID)) sent.set(e.turnID, e.createdAt);
    } else last.set(e.turnID, e.id);
  }
  const out = new Map<string, TurnFooter>();
  for (const [turnID, entryID] of last) {
    const record = turns?.find((t) => t.id === turnID);
    const end = record?.endedAt || undefined;
    const start = sent.get(turnID) ?? record?.startedAt ?? undefined;
    const active = running && !end && turnID === current;
    if (!end && !record?.usage && !active) continue;
    out.set(entryID, { turnID, start, end, usage: record?.usage, active });
  }
  return out;
}

/* Whether two lines would read the same, so a memoised entry can keep the
   object it has. */
export function sameFooter(a: TurnFooter, b: TurnFooter): boolean {
  return (
    a.turnID === b.turnID &&
    a.start === b.start &&
    a.end === b.end &&
    a.active === b.active &&
    a.usage?.total === b.usage?.total &&
    a.usage?.input === b.usage?.input &&
    a.usage?.cached === b.usage?.cached &&
    a.usage?.cacheWrite === b.usage?.cacheWrite &&
    a.usage?.output === b.usage?.output &&
    a.usage?.reasoning === b.usage?.reasoning &&
    a.usage?.costUSD === b.usage?.costUSD
  );
}

const two = (n: number) => String(n).padStart(2, "0");

/* "0.8s", "12s", "1m 05s", "1h 02m": the precision a reader wants at
   that length. */
export function formatDuration(seconds: number): string {
  const s = Math.max(0, seconds);
  if (s < 10) return `${s.toFixed(1)}s`;
  if (s < 60) return `${Math.round(s)}s`;
  const whole = Math.round(s);
  if (whole < 3600) return `${Math.floor(whole / 60)}m ${two(whole % 60)}s`;
  return `${Math.floor(whole / 3600)}h ${two(Math.floor((whole % 3600) / 60))}m`;
}

/* "842", "9.5k", "13k", "1.2M". */
export function formatTokens(n: number): string {
  if (n < 1000) return String(Math.round(n));
  if (n < 10000) return `${(n / 1000).toFixed(1)}k`;
  if (n < 1e6) return `${Math.round(n / 1000)}k`;
  return `${(n / 1e6).toFixed(1)}M`;
}

/* "$0.04", or "<$0.01" for an estimate that would round to nothing. */
export function formatCost(usd: number): string {
  return usd < 0.005 ? "<$0.01" : `$${usd.toFixed(2)}`;
}

/* The tokens in one phrase: the total with the in/out split. */
export function usageSummary(u: Usage): string {
  return `${formatTokens(u.total)} tokens (${formatTokens(u.input)} in, ${formatTokens(u.output)} out)`;
}

/* The full breakdown, for the hover title: exact counts, cache reads and
   writes, reasoning where the provider reports it. */
export function usageDetail(u: Usage): string {
  const n = (v: number) => v.toLocaleString();
  const input = [`Input ${n(u.input)} tokens`];
  if (u.cached) input.push(`${n(u.cached)} read from cache`);
  if (u.cacheWrite) input.push(`${n(u.cacheWrite)} written to cache`);
  const output = [`Output ${n(u.output)} tokens`];
  if (u.reasoning) output.push(`${n(u.reasoning)} reasoning`);
  return `${input.join(", ")} · ${output.join(", ")}`;
}

/* The whole line as text, for the markdown export. */
export function footerText(footer: TurnFooter, now?: number): string {
  const parts: string[] = [];
  const end = footer.end ?? (footer.active ? now : undefined);
  if (footer.start !== undefined && end !== undefined)
    parts.push(formatDuration(end - footer.start));
  if (footer.usage) {
    if (footer.usage.total > 0) parts.push(usageSummary(footer.usage));
    if (footer.usage.costUSD) parts.push(formatCost(footer.usage.costUSD));
  }
  return parts.join(" · ");
}
