// The spend chip in the composer footer, beside the model: what the
// chat's turns cost so far (the tokens where the provider prices nothing),
// from the service's sum on the state (spend.ts). Nothing until the first
// turn.
import { chatSpend, spendLabel, spendTitle } from "../spend";
import type { Chat } from "../types";

export function SpendChip({ chat }: { chat: Chat }) {
  const spend = chatSpend(chat);
  if (!spend.turns) return null;
  return (
    <span
      className="spend-chip"
      title={spendTitle(spend)}
      aria-label={`Spend ${spendLabel(spend)}`}
    >
      {spendLabel(spend)}
    </span>
  );
}
