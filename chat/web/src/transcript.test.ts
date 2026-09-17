import { describe, it, expect } from "vitest";
import {
  groupEntries,
  newSince,
  readSeen,
  unreadEntry,
  unreadIndex,
  unreadStart,
} from "./transcript";

const entry = (id: string, role: string, createdAt: number) => ({
  id,
  role,
  createdAt,
});
const entries = [
  entry("u1", "user", 10),
  entry("a1", "activity", 11),
  entry("a2", "activity", 12),
  entry("m1", "assistant", 13),
  entry("u2", "user", 20),
  entry("a3", "activity", 21),
  entry("m2", "assistant", 22),
];

describe("unread divider", () => {
  it("starts after the last entry the reader saw", () => {
    expect(unreadStart(entries, { id: "m1", at: 13 })).toBe(4);
    expect(unreadStart(entries, { id: "a1", at: 11 })).toBe(2);
  });
  it("falls back to the time when the seen entry is gone", () => {
    expect(unreadStart(entries, { id: "gone", at: 13 })).toBe(4);
    expect(unreadStart(entries, { id: "gone", at: 12.5 })).toBe(3);
  });
  it("shows nothing on a first visit or when everything was seen", () => {
    expect(unreadStart(entries, undefined)).toBe(-1);
    expect(unreadStart(entries, { id: "m2", at: 22 })).toBe(-1);
    expect(unreadStart(entries, { id: "gone", at: 22 })).toBe(-1);
    expect(unreadStart(entries, { id: "gone", at: 99 })).toBe(-1);
  });
  it("does not put the divider above every entry", () => {
    expect(unreadStart(entries, { id: "gone", at: 0 })).toBe(-1);
    expect(unreadStart([], { id: "u1", at: 10 })).toBe(-1);
  });
  it("stays where it was fixed when entries arrive after a fully-seen open", () => {
    // A return visit to a chat read to its end: nothing is unread, and
    // the reader's next message (or the reply) must not grow a divider.
    const later = [...entries, entry("u3", "user", 30), entry("m3", "assistant", 31)];
    for (const seen of [
      { id: "m2", at: 22 },
      { id: "gone", at: 22 },
    ]) {
      const id = unreadEntry(entries, seen);
      expect(id).toBe("");
      expect(unreadIndex(later, id)).toBe(-1);
      // What the live recomputation would have done, and why it is not used.
      expect(unreadStart(later, seen)).toBe(7);
    }
  });
  it("keeps a fixed divider in place as the transcript grows or loses it", () => {
    const id = unreadEntry(entries, { id: "m1", at: 13 });
    expect(id).toBe("u2");
    expect(unreadIndex(entries, id)).toBe(4);
    expect(unreadIndex([...entries, entry("u3", "user", 30)], id)).toBe(4);
    expect(unreadIndex([entry("x", "user", 1), ...entries], id)).toBe(5);
    expect(unreadIndex(entries.slice(0, 4), id)).toBe(-1);
    expect(unreadIndex(entries, "")).toBe(-1);
    expect(unreadEntry(entries, undefined)).toBe("");
  });
  it("reads a remembered entry and tolerates corrupt or unavailable storage", () => {
    const seen = { id: "m1", at: 13 };
    expect(readSeen({ getItem: () => JSON.stringify(seen) }, "seen")).toEqual(
      seen,
    );
    expect(
      readSeen({ getItem: () => JSON.stringify({ id: "m1" }) }, "seen"),
    ).toEqual({ id: "m1", at: 0 });
    expect(readSeen({ getItem: () => null }, "seen")).toBeUndefined();
    expect(readSeen({ getItem: () => "{bad" }, "seen")).toBeUndefined();
    expect(
      readSeen({ getItem: () => JSON.stringify({ id: 5, at: 1 }) }, "seen"),
    ).toBeUndefined();
    expect(
      readSeen(
        {
          getItem: () => {
            throw Error("denied");
          },
        },
        "seen",
      ),
    ).toBeUndefined();
  });
});

describe("new messages since the reader left the bottom", () => {
  it("counts messages after the entry that was last in view, not steps", () => {
    expect(newSince(entries, "m1")).toBe(2);
    expect(newSince(entries, "u2")).toBe(1);
    expect(newSince(entries, "m2")).toBe(0);
  });
  it("counts everything after an empty transcript and nothing after a lost entry", () => {
    expect(newSince(entries, "")).toBe(4);
    expect(newSince(entries, "gone")).toBe(0);
  });
});

describe("activity groups", () => {
  it("collapses consecutive steps into one group", () => {
    const items = groupEntries(entries);
    expect(
      items.map((i) => ("group" in i ? i.group.length : i.entry.id)),
    ).toEqual(["u1", 2, "m1", "u2", 1, "m2"]);
  });
  it("breaks a group at the unread divider", () => {
    const items = groupEntries(entries, 2);
    expect(
      items.map((i) => ("group" in i ? i.group.length : i.entry.id)),
    ).toEqual(["u1", 1, 1, "m1", "u2", 1, "m2"]);
    expect("group" in items[2] && items[2].group[0].id).toBe("a2");
    expect(groupEntries(entries, 4)).toEqual(groupEntries(entries));
  });
});
