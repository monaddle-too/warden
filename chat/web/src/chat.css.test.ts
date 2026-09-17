import { describe, expect, it } from "vitest";
import css from "./chat.css?raw";

// The stylesheet is hand-merged now and then, and a lost brace silently
// swallows every rule after it (the build does not parse CSS): the braces
// must balance and every top-level rule must start at depth zero.
describe("chat.css", () => {
  it("has balanced braces and no rule opened inside another", () => {
    let depth = 0;
    css.split("\n").forEach((line: string, i: number) => {
      if (/^[^\s@}/].*\{\s*$/.test(line) && depth !== 0) {
        throw new Error(`line ${i + 1} starts a top-level rule at depth ${depth}: ${line}`);
      }
      for (const c of line) {
        if (c === "{") depth++;
        if (c === "}") depth--;
      }
    });
    expect(depth).toBe(0);
  });
});
