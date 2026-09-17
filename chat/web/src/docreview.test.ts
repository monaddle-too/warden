import { describe, expect, it } from "vitest";
import { parseInline, wordDiff } from "./docmarks";

describe("document review marks", () => {
  it("parses the toggle grammar the policy service renders", () => {
    expect(
      parseInline("Say **hi** to *the* [world](https://x.y/) \\*now\\*"),
    ).toEqual([
      { text: "Say ", bold: false, italic: false, link: "" },
      { text: "hi", bold: true, italic: false, link: "" },
      { text: " to ", bold: false, italic: false, link: "" },
      { text: "the", bold: false, italic: true, link: "" },
      { text: " ", bold: false, italic: false, link: "" },
      { text: "world", bold: false, italic: false, link: "https://x.y/" },
      { text: " *now*", bold: false, italic: false, link: "" },
    ]);
    expect(parseInline("[not a link")).toEqual([
      { text: "[not a link", bold: false, italic: false, link: "" },
    ]);
  });
  it("diffs words and marks", () => {
    const ops = wordDiff(
      "Say **hello** to the world",
      "Say **hi** to the *whole* world",
    );
    expect(ops.map((o) => o.kind + o.token.text).join("")).toBe(
      "=Say= -hello+hi= =to= =the= +whole+ =world",
    );
    expect(ops.find((o) => o.token.text === "hi")?.token.bold).toBe(true);
  });
});
