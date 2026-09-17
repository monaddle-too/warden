// Pure matching behind the ⌘K palette (chat titles and entries across every
// chat) and the in-chat find bar (matches counted and highlighted in the
// rendered transcript). A match is a literal substring of folded text: case
// and accents are ignored and any whitespace equals a space, so "cafe" finds
// "Café" and "a b" finds "a\nb". `fold` keeps every UTF-16 offset where it
// was, so a match found in the folded text is the same range in the
// original, which is what highlight ranges and result snippets need.
import type { Chat, Entry } from "./types";

export type Match = { start: number; end: number };

const combining = /\p{M}/gu;

/* One code point, case- and accent-folded, unless that would change its
   length ("İ" lower-cases to two units, a bold 𝐀 is one code point but two
   units); such characters stay as they are rather than shift every offset
   after them. */
function foldChar(c: string): string {
  const f = c.normalize("NFD").replace(combining, "").toLowerCase();
  if (f.length === c.length) return f;
  const lower = c.toLowerCase();
  return lower.length === c.length ? lower : c;
}

export function fold(text: string): string {
  const out: string[] = [];
  for (const c of text) {
    const code = c.charCodeAt(0);
    if (c.length === 1 && code < 128)
      out.push(
        code >= 65 && code <= 90
          ? String.fromCharCode(code + 32)
          : code <= 32
            ? " "
            : c,
      );
    else if (/\s/.test(c)) out.push(" ");
    else out.push(foldChar(c));
  }
  return out.join("");
}

// The palette folds every entry of every chat per keystroke; a text folds
// once and the result is kept until the cache fills.
const foldedTexts = new Map<string, string>();
export function folded(text: string): string {
  let f = foldedTexts.get(text);
  if (f === undefined) {
    if (foldedTexts.size >= 4096) foldedTexts.clear();
    f = fold(text);
    foldedTexts.set(text, f);
  }
  return f;
}

/* Every non-overlapping match of `query` in `text`, in order; `hay` is the
   folded text when the caller already has it. A blank query matches
   nothing. */
export function findMatches(
  text: string,
  query: string,
  limit = Infinity,
  hay = fold(text),
): Match[] {
  const needle = fold(query).trim();
  const out: Match[] = [];
  if (!needle) return out;
  for (
    let at = hay.indexOf(needle);
    at !== -1 && out.length < limit;
    at = hay.indexOf(needle, at + needle.length)
  )
    out.push({ start: at, end: at + needle.length });
  return out;
}

/* Where an offset into the concatenation of segments with the given
   lengths falls. An offset on a boundary belongs to the later segment as a
   start and to the earlier one as an end, so a range never starts or ends
   in an empty position of the wrong node. */
export function locate(
  lengths: number[],
  offset: number,
  atEnd = false,
): { index: number; offset: number } {
  let index = 0;
  while (
    index < lengths.length - 1 &&
    (atEnd ? offset > lengths[index] : offset >= lengths[index])
  ) {
    offset -= lengths[index];
    index++;
  }
  return { index, offset };
}

export type Snippet = { before: string; match: string; after: string };

/* The text around a match for a result row: up to `radius` characters each
   side, cut at a word boundary when one is near, with an ellipsis where
   text was left out; whitespace runs become one space. */
export function snippet(text: string, match: Match, radius = 40): Snippet {
  let start = Math.max(0, match.start - radius);
  let end = Math.min(text.length, match.end + radius);
  if (start > 0) {
    const gap = text
      .slice(start, Math.min(match.start, start + 13))
      .search(/\s/);
    if (gap !== -1) start += gap + 1;
  }
  if (end < text.length) {
    const tail = text.slice(Math.max(match.end, end - 13), end);
    const gap = tail.search(/\s\S*$/);
    if (gap !== -1) end -= tail.length - gap;
  }
  const clean = (s: string) => s.replace(/\s+/g, " ");
  return {
    before: (start > 0 ? "…" : "") + clean(text.slice(start, match.start)),
    match: clean(text.slice(match.start, match.end)),
    after: clean(text.slice(match.end, end)) + (end < text.length ? "…" : ""),
  };
}

export type Hit =
  | { kind: "chat"; chat: Chat; match: Match }
  | {
      kind: "entry";
      chat: Chat;
      entry: Entry;
      field: "text" | "detail";
      match: Match;
    };

const lastActivity = (chat: Chat) => {
  const entries = chat.conversation.entries;
  return entries.length ? entries[entries.length - 1].createdAt : 0;
};

/* Chats by recency: live ones first, then by the time of their last entry,
   so the palette's empty state is a list of what was worked on last. */
export function recentChats(chats: Chat[]): Chat[] {
  return [...chats].sort(
    (a, b) =>
      Number(a.archived) - Number(b.archived) ||
      lastActivity(b) - lastActivity(a),
  );
}

/* Title matches first, then entries, newest chat and newest entry first;
   one hit per entry (its text, else a tool step's output). `more` counts
   the hits past `limit`. */
export function searchChats(
  chats: Chat[],
  query: string,
  limit = 40,
): { hits: Hit[]; more: number } {
  const hits: Hit[] = [];
  let total = 0;
  if (!fold(query).trim()) return { hits, more: 0 };
  const add = (hit: Hit) => {
    total++;
    if (hits.length < limit) hits.push(hit);
  };
  const ordered = recentChats(chats);
  for (const chat of ordered) {
    const [match] = findMatches(chat.title, query, 1, folded(chat.title));
    if (match) add({ kind: "chat", chat, match });
  }
  for (const chat of ordered) {
    const entries = chat.conversation.entries;
    for (let i = entries.length - 1; i >= 0; i--) {
      const entry = entries[i];
      const [inText] = findMatches(entry.text, query, 1, folded(entry.text));
      if (inText) {
        add({ kind: "entry", chat, entry, field: "text", match: inText });
        continue;
      }
      if (entry.role !== "activity" || !entry.detail) continue;
      const [inDetail] = findMatches(
        entry.detail,
        query,
        1,
        folded(entry.detail),
      );
      if (inDetail)
        add({ kind: "entry", chat, entry, field: "detail", match: inDetail });
    }
  }
  return { hits, more: total - hits.length };
}
