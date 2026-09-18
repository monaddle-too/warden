import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";

// The stylesheets are hand-merged now and then, and a lost brace silently
// swallows every rule after it (the build does not parse CSS): the braces
// must balance and every top-level rule must start at depth zero. The files
// are read from disk as written: a "?raw" import of a stylesheet is empty
// under vitest, which made this test vacuous and let a dropped brace in.
const source = (name: string) =>
  readFileSync(new URL(`./${name}`, import.meta.url), "utf8");
describe.each([
  ["chat.css", source("chat.css")],
  ["conversation.css", source("conversation.css")],
])("%s", (_name, css) => {
  it("has balanced braces and no rule opened inside another", () => {
    let depth = 0;
    css.split("\n").forEach((line: string, i: number) => {
      if (/^[^\s@}/].*\{\s*$/.test(line) && depth !== 0) {
        throw new Error(
          `line ${i + 1} starts a top-level rule at depth ${depth}: ${line}`,
        );
      }
      for (const c of line) {
        if (c === "{") depth++;
        if (c === "}") depth--;
      }
    });
    expect(depth).toBe(0);
  });
});
