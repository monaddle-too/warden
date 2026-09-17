import { describe, it, expect } from "vitest";
import { KATEX_OPTIONS, hasMath } from "./math";

describe("math detection", () => {
  it("finds display math, math fences and Pandoc-style inline math", () => {
    expect(hasMath("$$\nE = mc^2\n$$")).toBe(true);
    expect(hasMath("inline $$x$$ too")).toBe(true);
    expect(hasMath("```math\nx^2\n```")).toBe(true);
    expect(hasMath("~~~ Math\nx\n~~~")).toBe(true);
    expect(hasMath("so $x_1 + y$ holds")).toBe(true);
    expect(hasMath("$\\alpha$")).toBe(true);
  });
  it("leaves prices and lone dollars alone", () => {
    expect(hasMath("costs $5 and $10")).toBe(false);
    expect(hasMath("between $5-$10 each")).toBe(false);
    expect(hasMath("the $ sign")).toBe(false);
    expect(hasMath("a $ b $ c")).toBe(false);
    expect(hasMath("escaped \\$x$")).toBe(false);
    expect(hasMath("split $x\n y$ lines")).toBe(false);
    expect(hasMath("")).toBe(false);
  });
  it("never trusts the formula", () => {
    expect(KATEX_OPTIONS.trust).toBe(false);
    expect(KATEX_OPTIONS.throwOnError).toBe(false);
    expect(KATEX_OPTIONS.maxSize).toBeGreaterThan(0);
  });
});
