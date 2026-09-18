/* Transcript layout that does not need the DOM: grouping of tool steps,
   the unread divider and the "new messages" count behind the jump button.
   What the reader has seen is remembered per chat in localStorage as the
   last entry that was on screen while they were following the transcript. */
type Item = {
  id: string;
  role: string;
  createdAt: number;
  turnID?: string;
  sender?: { principalID: string };
};

/* The last entry the reader saw: its ID, and its time for when the ID is
   gone (a chat whose entries the service replaced). */
export type Seen = { id: string; at: number };

export function readSeen(
  storage: Pick<Storage, "getItem">,
  key: string,
): Seen | undefined {
  try {
    const v = JSON.parse(storage.getItem(key) || "null");
    if (v && typeof v.id === "string" && v.id) {
      const at = Number(v.at);
      return { id: v.id, at: Number.isFinite(at) ? at : 0 };
    }
  } catch {}
  return undefined;
}

/* Index of the first entry the reader has not seen: the one after the
   remembered entry, or the first newer than its time if that entry is
   gone. -1 when nothing is unread, on a first visit (everything would be
   new, so a divider would say nothing) and when every entry is new (the
   divider divides; a chat seen empty starts at the top anyway). The
   reader's own messages are never unread: one sent while scrolled up (the
   seen mark only advances while following) would otherwise head the
   stretch, so the divider moves past them to what came after. */
export function unreadStart<T extends Item>(
  entries: T[],
  seen: Seen | undefined,
  mine: (entry: T) => boolean = () => false,
): number {
  if (!seen) return -1;
  const known = entries.findIndex((e) => e.id === seen.id);
  let start =
    known >= 0 ? known + 1 : entries.findIndex((e) => e.createdAt > seen.at);
  if (start <= 0) return -1;
  while (start < entries.length && mine(entries[start])) start++;
  return start < entries.length ? start : -1;
}

/* The divider is a place, not a rule: the ID of the entry it goes before,
   fixed when the chat opens. `unreadStart` recomputed against a growing
   transcript would put a divider above whatever arrives next once the
   seen mark reaches the last entry (the reader's own message included),
   so the view keeps this ID and looks it up with `unreadIndex`. */
export function unreadEntry<T extends Item>(
  entries: T[],
  seen: Seen | undefined,
  mine?: (entry: T) => boolean,
): string {
  const start = unreadStart(entries, seen, mine);
  return start >= 0 ? entries[start].id : "";
}

/* Where the fixed divider sits now: -1 when there is none, or when its
   entry is gone (a transcript the service replaced). */
export function unreadIndex<T extends Item>(entries: T[], id: string): number {
  return id ? entries.findIndex((e) => e.id === id) : -1;
}

/* How many messages (not tool steps) follow `lastID`, the last entry the
   reader had in view when they left the bottom of the transcript. An empty
   ID is an empty transcript, so everything counts; an ID that is gone
   counts nothing rather than everything. */
export function newSince<T extends Item>(entries: T[], lastID: string): number {
  let from = 0;
  if (lastID) {
    const index = entries.findIndex((e) => e.id === lastID);
    if (index < 0) return 0;
    from = index + 1;
  }
  let count = 0;
  for (let i = from; i < entries.length; i++)
    if (entries[i].role !== "activity" && entries[i].role !== "thinking")
      count++;
  return count;
}

/* Consecutive tool steps render as one collapsible group; a group never
   spans the unread divider, so the divider can sit before `breakAt`, and
   never two turns, so a turn's line can follow its last group. */
export function groupEntries<T extends Item>(
  entries: T[],
  breakAt = -1,
): ({ entry: T } | { group: T[] })[] {
  const items: ({ entry: T } | { group: T[] })[] = [];
  entries.forEach((entry, index) => {
    const last = items[items.length - 1];
    if (entry.role === "activity") {
      if (
        last &&
        "group" in last &&
        index !== breakAt &&
        last.group[0].turnID === entry.turnID
      )
        last.group.push(entry);
      else items.push({ group: [entry] });
    } else items.push({ entry });
  });
  return items;
}
