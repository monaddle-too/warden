/* Long pastes in the composer: the pure parts.

   A paste over PASTE_LINES lines or PASTE_CHARS characters does not land
   in the text as it is. A placeholder, "[Pasted text #1 — 240 lines]",
   takes its place at the caret and a chip above the text stands for it
   (hover previews, click opens, remove drops both). The text itself is
   kept beside the draft and put back in place of the placeholder when the
   message is sent, so the agent gets the whole paste. Several pastes per
   draft are numbered in order; a placeholder the person deletes from the
   text takes its paste with it. The TUI does the same (tui/paste.go),
   with the same thresholds. */

export const PASTE_LINES = 8;
export const PASTE_CHARS = 1000;

export type Paste = { n: number; text: string };

const PLACEHOLDER = /\[Pasted text #(\d+) — \d+ lines?\]/g;

/* Lines as the chip counts them: no trailing empty line. */
export function pasteLines(text: string): number {
  return text.replace(/\n$/, "").split("\n").length;
}

/* Whether pasted text is collapsed rather than inserted. */
export function longPaste(text: string): boolean {
  return text.length > PASTE_CHARS || pasteLines(text) > PASTE_LINES;
}

/* What stands in the text for paste `n`. */
export function placeholder(paste: Paste): string {
  const lines = pasteLines(paste.text);
  return `[Pasted text #${paste.n} — ${lines} ${lines === 1 ? "line" : "lines"}]`;
}

/* The text with the paste collapsed at the caret (replacing a selection),
   the caret after the placeholder, and the pastes with the new one; the
   number is one past the highest so far, so a removed paste's number is
   never reused within the draft. */
export function collapsePaste(
  text: string,
  start: number,
  end: number,
  pasted: string,
  pastes: Paste[],
): { text: string; caret: number; pastes: Paste[] } {
  const n = pastes.reduce((max, p) => Math.max(max, p.n), 0) + 1;
  const paste = { n, text: pasted };
  const mark = placeholder(paste);
  const next = text.slice(0, start) + mark + text.slice(end);
  return { text: next, caret: start + mark.length, pastes: [...pastes, paste] };
}

/* The pastes whose placeholder is still in the text, in the text's order:
   deleting the placeholder is how a paste is dropped. */
export function livePastes(text: string, pastes: Paste[]): Paste[] {
  const out: Paste[] = [];
  for (const m of text.matchAll(PLACEHOLDER)) {
    const n = Number(m[1]);
    const paste = pastes.find((p) => p.n === n);
    if (paste && !out.includes(paste)) out.push(paste);
  }
  return out;
}

/* The text with every placeholder replaced by its paste; a placeholder
   with no paste behind it (typed by hand) stays as written. */
export function expandPastes(text: string, pastes: Paste[]): string {
  return text.replace(PLACEHOLDER, (mark, n) => {
    const paste = pastes.find((p) => p.n === Number(n));
    return paste ? paste.text : mark;
  });
}

/* The text with a paste's placeholder removed (and one space around it,
   so "see [Pasted…] and" reads "see and"). */
export function removePaste(text: string, paste: Paste): string {
  const mark = placeholder(paste);
  const at = text.indexOf(mark);
  if (at < 0) return text;
  let from = at,
    to = at + mark.length;
  if (text[to] === " " && text[from - 1] === " ") to++;
  return text.slice(0, from) + text.slice(to);
}

/* A preview of a paste for its chip's tooltip: the first lines. */
export function pastePreview(text: string, lines = 6): string {
  const all = text.replace(/\n$/, "").split("\n");
  const head = all.slice(0, lines).join("\n");
  return all.length > lines ? head + "\n…" : head;
}

/* The pastes kept with a draft, read back from storage. */
export function readPastes(
  storage: Pick<Storage, "getItem">,
  key: string,
): Paste[] {
  try {
    const v = JSON.parse(storage.getItem(key) || "null");
    if (!Array.isArray(v)) return [];
    return v.filter(
      (p): p is Paste =>
        !!p &&
        Number.isInteger(p.n) &&
        p.n > 0 &&
        typeof p.text === "string" &&
        p.text !== "",
    );
  } catch {
    return [];
  }
}
