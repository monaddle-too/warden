/* Spend (docs/claude-parity.md, R2.2): what the agent's turns took, as
   the service sums each chat's turn records (chat.spend; the /cost card
   sums the same rows locally), shown as a chip beside the model, as the
   workspace's total in its panel, and in the admin console's Spend
   section (GET spend: today, seven days, all time by provider). Codex
   reports tokens and no cost. */
import { sessionCost } from "./cost";
import { formatCost, formatTokens } from "./turns";
import type { Chat, Spend } from "./types";

export const NO_SPEND: Spend = {
  turns: 0,
  input: 0,
  output: 0,
  total: 0,
  costUSD: 0,
  priced: false,
};

/* A chat's spend: the service's sum, or the turns summed here when the
   service is older than the field. */
export function chatSpend(chat: Chat): Spend {
  if (chat.spend) return chat.spend;
  const c = sessionCost(chat.conversation.turns);
  return {
    turns: c.turns,
    input: c.usage.input,
    output: c.usage.output,
    total: c.usage.total,
    costUSD: c.usage.costUSD || 0,
    priced: c.priced,
  };
}

export function sumSpend(list: Spend[]): Spend {
  const out = { ...NO_SPEND };
  for (const s of list) {
    out.turns += s.turns;
    out.input += s.input;
    out.output += s.output;
    out.total += s.total;
    out.costUSD += s.costUSD;
    out.priced = out.priced || s.priced;
  }
  return out;
}

/* The chats on a workspace summed (archived ones included), and how many. */
export function workspaceSpend(
  chats: Chat[],
  sandboxID: string,
): { spend: Spend; chats: number } {
  const mine = chats.filter((c) => c.sandboxID === sandboxID);
  return { spend: sumSpend(mine.map(chatSpend)), chats: mine.length };
}

/* The chip's text: the cost where the provider prices its turns, the
   tokens otherwise. */
export function spendLabel(s: Spend): string {
  return s.priced ? formatCost(s.costUSD) : `${formatTokens(s.total)} tok`;
}

/* One line: "3 turns · 31k tokens (30k in, 1.0k out) · $0.12" (or "no
   cost reported" for Codex). */
export function spendLine(s: Spend): string {
  const parts = [
    `${s.turns} turn${s.turns === 1 ? "" : "s"}`,
    `${formatTokens(s.total)} tokens (${formatTokens(s.input)} in, ${formatTokens(s.output)} out)`,
    s.priced ? formatCost(s.costUSD) : "no cost reported",
  ];
  return parts.join(" · ");
}

/* The chip's title. */
export function spendTitle(s: Spend): string {
  return `This chat so far: ${spendLine(s)}. The agent's turns summed; /cost shows the breakdown.`;
}
