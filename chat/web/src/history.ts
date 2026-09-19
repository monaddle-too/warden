/* Prompt history in the composer: the pure parts.

   The history is what this person sent in this chat, read from the
   transcript (the user entries whose sender is this principal), newest
   first and without repeats, so nothing is stored beyond the transcript
   itself and every device sees the same history. Up at the draft's first
   line recalls the newest prompt, Up again the one before, Down comes back
   toward the draft, which is kept while browsing. There is no Ctrl-R
   search: in a browser ⌘R / Ctrl+R reloads the page, as in Claude. */

type UserEntry = {
  role: string;
  text: string;
  sender?: { principalID: string };
};

/* The prompts this person sent, newest first, each once. An entry
   without a sender is the owner's (recorded before senders existed). */
export function promptHistory(
  entries: UserEntry[],
  principalID: string,
): string[] {
  const out: string[] = [];
  const seen = new Set<string>();
  for (let i = entries.length - 1; i >= 0; i--) {
    const e = entries[i];
    if (e.role !== "user") continue;
    if ((e.sender?.principalID ?? "owner") !== principalID) continue;
    const text = e.text.trim();
    if (!text || seen.has(text)) continue;
    seen.add(text);
    out.push(text);
  }
  return out;
}

/* Where the composer is in the history: `index` is the prompt shown (0 is
   the newest), or -1 for the draft, which `draft` keeps while browsing. */
export type Recall = { index: number; draft: string };

export const NOT_BROWSING: Recall = { index: -1, draft: "" };

/* Whether the caret is on the draft's first line (Up then recalls) or its
   last (Down then moves on); a one-line draft is both. */
export function onFirstLine(text: string, caret: number): boolean {
  return !text.slice(0, caret).includes("\n");
}
export function onLastLine(text: string, caret: number): boolean {
  return !text.slice(caret).includes("\n");
}

/* The step Up takes: the next older prompt, keeping the draft the first
   time; undefined at the oldest (the caret then stays put). */
export function recallOlder(
  recall: Recall,
  history: string[],
  text: string,
): { recall: Recall; text: string } | undefined {
  const next = recall.index + 1;
  if (next >= history.length) return undefined;
  return {
    recall: { index: next, draft: recall.index === -1 ? text : recall.draft },
    text: history[next],
  };
}

/* The step Down takes: the next newer prompt, or the draft back after the
   newest; undefined when not browsing. */
export function recallNewer(
  recall: Recall,
  history: string[],
): { recall: Recall; text: string } | undefined {
  if (recall.index < 0) return undefined;
  const next = recall.index - 1;
  if (next < 0) return { recall: NOT_BROWSING, text: recall.draft };
  return { recall: { ...recall, index: next }, text: history[next] };
}
