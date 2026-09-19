import { describe, it, expect } from "vitest";
import {
  collapsePaste,
  expandPastes,
  livePastes,
  longPaste,
  pastePreview,
  placeholder,
  readPastes,
  removePaste,
} from "./paste";

const lines = (n: number) =>
  Array.from({ length: n }, (_, i) => `line ${i + 1}`).join("\n") + "\n";

describe("long pastes", () => {
  it("collapses over the line or character threshold", () => {
    expect(longPaste(lines(8))).toBe(false);
    expect(longPaste(lines(9))).toBe(true);
    expect(longPaste("x".repeat(1000))).toBe(false);
    expect(longPaste("x".repeat(1001))).toBe(true);
    expect(placeholder({ n: 1, text: lines(240) })).toBe(
      "[Pasted text #1 — 240 lines]",
    );
    expect(placeholder({ n: 2, text: "x".repeat(2000) })).toBe(
      "[Pasted text #2 — 1 line]",
    );
  });

  it("puts a placeholder at the caret and the text back on send", () => {
    const one = collapsePaste("see  here", 4, 4, lines(12), []);
    expect(one.text).toBe("see [Pasted text #1 — 12 lines] here");
    expect(one.caret).toBe(4 + "[Pasted text #1 — 12 lines]".length);
    expect(one.pastes).toEqual([{ n: 1, text: lines(12) }]);
    // A selection is replaced; numbers keep counting up.
    const two = collapsePaste(
      one.text + " and this",
      one.text.length + 5,
      one.text.length + 9,
      "y".repeat(1500),
      one.pastes,
    );
    expect(two.text).toBe(one.text + " and [Pasted text #2 — 1 line]");
    expect(two.pastes.map((p) => p.n)).toEqual([1, 2]);
    expect(expandPastes(two.text, two.pastes)).toBe(
      "see " + lines(12) + " here and " + "y".repeat(1500),
    );
    // A placeholder typed by hand, with no paste behind it, stays.
    expect(expandPastes("[Pasted text #7 — 3 lines]", two.pastes)).toBe(
      "[Pasted text #7 — 3 lines]",
    );
  });

  it("drops a paste whose placeholder was deleted, and removes one on request", () => {
    const pastes = [
      { n: 1, text: lines(9) },
      { n: 2, text: lines(10) },
    ];
    expect(livePastes("[Pasted text #2 — 10 lines] only", pastes)).toEqual([
      pastes[1],
    ]);
    expect(livePastes("nothing", pastes)).toEqual([]);
    expect(removePaste("see [Pasted text #1 — 9 lines] and", pastes[0])).toBe(
      "see and",
    );
    expect(removePaste("[Pasted text #1 — 9 lines]", pastes[0])).toBe("");
    expect(removePaste("untouched", pastes[0])).toBe("untouched");
    // A removed number is not reused.
    const next = collapsePaste("", 0, 0, lines(20), [pastes[1]]);
    expect(next.pastes.map((p) => p.n)).toEqual([2, 3]);
  });

  it("previews the first lines and reads stored pastes back", () => {
    expect(pastePreview(lines(3))).toBe("line 1\nline 2\nline 3");
    expect(pastePreview(lines(9), 2)).toBe("line 1\nline 2\n…");
    const storage = new Map<string, string>();
    const get = { getItem: (k: string) => storage.get(k) ?? null };
    expect(readPastes(get, "k")).toEqual([]);
    storage.set("k", "not json");
    expect(readPastes(get, "k")).toEqual([]);
    storage.set(
      "k",
      JSON.stringify([
        { n: 1, text: "a" },
        { n: 0, text: "b" },
        { n: 2 },
        null,
        { n: 3, text: "" },
      ]),
    );
    expect(readPastes(get, "k")).toEqual([{ n: 1, text: "a" }]);
  });
});
