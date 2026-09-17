import { describe, it, expect } from "vitest";
import {
  findMatches,
  fold,
  locate,
  recentChats,
  searchChats,
  snippet,
} from "./search";
import type { Chat, Entry } from "./types";

const entry = (over: Partial<Entry>): Entry => ({
  id: "e",
  role: "assistant",
  text: "",
  detail: "",
  createdAt: 0,
  isStreaming: false,
  delivery: "",
  ...over,
});
const chat = (
  id: string,
  title: string,
  entries: Entry[],
  archived = false,
): Chat => ({
  id,
  title,
  sandboxID: "s",
  repository: "",
  status: "idle",
  archived,
  conversation: { entries },
  approvals: [],
});

describe("folding", () => {
  it("ignores case, accents and the kind of whitespace, keeping offsets", () => {
    expect(fold("Café\tAu Lait\n")).toBe("cafe au lait ");
    expect(fold("Ärger")).toBe("arger");
    for (const s of ["İstanbul", "straße", "𝐀 bold", "e\u0301", "日本語"])
      expect(fold(s).length).toBe(s.length);
    expect(fold("e\u0301")).toBe("e\u0301".toLowerCase());
    expect(fold("𝐀")).toBe("𝐀");
  });
});

describe("findMatches", () => {
  it("finds every literal, case-insensitive, non-overlapping match", () => {
    expect(findMatches("Aaa aA", "aa")).toEqual([
      { start: 0, end: 2 },
      { start: 4, end: 6 },
    ]);
    expect(findMatches("résumé resume", "RESUME")).toEqual([
      { start: 0, end: 6 },
      { start: 7, end: 13 },
    ]);
    expect(findMatches("a\nb", "a b")).toEqual([{ start: 0, end: 3 }]);
    expect(findMatches("x y", " x ")).toEqual([{ start: 0, end: 1 }]);
  });
  it("matches nothing for a blank query and honours the limit", () => {
    expect(findMatches("anything", "")).toEqual([]);
    expect(findMatches("anything", "  \n")).toEqual([]);
    expect(findMatches("", "a")).toEqual([]);
    expect(findMatches("a a a", "a", 2)).toHaveLength(2);
  });
});

describe("locate", () => {
  it("maps an offset into the concatenation of text nodes", () => {
    const lengths = [3, 0, 4];
    expect(locate(lengths, 0)).toEqual({ index: 0, offset: 0 });
    expect(locate(lengths, 2)).toEqual({ index: 0, offset: 2 });
    // A boundary starts in the next node and ends in the previous one.
    expect(locate(lengths, 3)).toEqual({ index: 2, offset: 0 });
    expect(locate(lengths, 3, true)).toEqual({ index: 0, offset: 3 });
    expect(locate(lengths, 7, true)).toEqual({ index: 2, offset: 4 });
    expect(locate([5], 5, true)).toEqual({ index: 0, offset: 5 });
  });
});

describe("snippet", () => {
  it("cuts around the match at word boundaries and collapses whitespace", () => {
    const text =
      "The quick brown fox\njumps over the lazy dog and keeps on running far away";
    const [match] = findMatches(text, "lazy");
    expect(snippet(text, match, 12)).toEqual({
      before: "…over the ",
      match: "lazy",
      after: " dog and…",
    });
    expect(snippet("lazy dog", { start: 0, end: 4 })).toEqual({
      before: "",
      match: "lazy",
      after: " dog",
    });
    expect(snippet("a\n\nlazy", { start: 3, end: 7 }).before).toBe("a ");
  });
});

describe("searchChats", () => {
  const old = chat("old", "Fix the build", [
    entry({ id: "o1", role: "user", text: "Why does the build fail?" }),
    entry({
      id: "o2",
      role: "activity",
      text: "bash",
      detail: "make: *** No rule to make target build",
      createdAt: 5,
    }),
  ]);
  const recent = chat("new", "Deploy notes", [
    entry({ id: "n1", role: "user", text: "Write the deploy notes" }),
    entry({ id: "n2", text: "Notes: the BUILD is green", createdAt: 10 }),
  ]);
  const gone = chat(
    "gone",
    "Build archive",
    [entry({ id: "g1", text: "archived build", createdAt: 20 })],
    true,
  );
  it("orders chats by recency with archived ones last", () => {
    expect(recentChats([old, gone, recent]).map((c) => c.id)).toEqual([
      "new",
      "old",
      "gone",
    ]);
  });
  it("lists title hits first, then entries newest first, one per entry", () => {
    const { hits, more } = searchChats([old, gone, recent], "build");
    expect(more).toBe(0);
    expect(
      hits.map((h) =>
        h.kind === "chat" ? `chat:${h.chat.id}` : `${h.entry.id}:${h.field}`,
      ),
    ).toEqual([
      "chat:old",
      "chat:gone",
      "n2:text",
      "o2:detail",
      "o1:text",
      "g1:text",
    ]);
    const title = hits[0];
    expect(title.kind === "chat" && title.match).toEqual({ start: 8, end: 13 });
  });
  it("matches nothing for a blank query and counts hits past the limit", () => {
    expect(searchChats([old, recent], " ")).toEqual({ hits: [], more: 0 });
    const { hits, more } = searchChats([old, gone, recent], "build", 2);
    expect(hits).toHaveLength(2);
    expect(more).toBe(4);
  });
  it("searches a tool step's output only when its label has no match", () => {
    const { hits } = searchChats([old], "make");
    expect(hits.map((h) => h.kind === "entry" && h.field)).toEqual(["detail"]);
    expect(searchChats([old], "bash").hits[0]).toMatchObject({
      field: "text",
    });
  });
});
