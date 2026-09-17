import { describe, it, expect } from "vitest";
import {
  footerText,
  formatCost,
  formatDuration,
  formatTokens,
  sameFooter,
  turnFooters,
  usageDetail,
  usageSummary,
} from "./turns";
import type { Turn, Usage } from "./types";

const entry = (id: string, role: string, createdAt: number, turnID?: string) =>
  ({ id, role, createdAt, turnID }) as const;
const usage: Usage = {
  input: 10231,
  cached: 8120,
  cacheWrite: 100,
  output: 3201,
  reasoning: 1000,
  total: 13432,
  costUSD: 0.041,
};
const entries = [
  entry("u1", "user", 100, "t1"),
  entry("a1", "activity", 101, "t1"),
  entry("m1", "assistant", 102, "t1"),
  entry("m2", "assistant", 110, "t1"),
  entry("u2", "user", 200, "t2"),
  entry("a2", "activity", 201, "t2"),
  entry("u3", "user", 300),
];
const turns: Turn[] = [
  { id: "t1", startedAt: 100.5, endedAt: 172, usage },
  { id: "t2", startedAt: 201 },
];

describe("turn footers", () => {
  it("go under the last agent entry of each turn, timed from the owner's message", () => {
    const footers = turnFooters(entries, turns, true);
    expect([...footers.keys()]).toEqual(["m2", "a2"]);
    expect(footers.get("m2")).toEqual({
      turnID: "t1",
      start: 100,
      end: 172,
      usage,
      active: false,
    });
    // The running turn counts up; it is the last one begun.
    expect(footers.get("a2")).toEqual({
      turnID: "t2",
      start: 200,
      end: undefined,
      usage: undefined,
      active: true,
    });
  });
  it("leave out turns without a record unless they are running", () => {
    const old = [entry("u", "user", 1, "t0"), entry("m", "assistant", 2, "t0")];
    expect(turnFooters(old, undefined, false).size).toBe(0);
    expect(turnFooters(old, [], false).size).toBe(0);
    const running = turnFooters(old, undefined, true).get("m");
    expect(running).toEqual({
      turnID: "t0",
      start: 1,
      end: undefined,
      usage: undefined,
      active: true,
    });
    // A turn cut short by a restart keeps its usage line but stops counting.
    const cut = turnFooters(old, [{ id: "t0", usage }], false).get("m");
    expect(cut).toMatchObject({ active: false, end: undefined, usage });
    // Only the last turn begun can be running: an earlier open turn is not.
    expect(turnFooters(entries, [], true).get("m2")).toBeUndefined();
  });
  it("fall back to the service's start when the owner's message is gone", () => {
    const orphan = [entry("m", "assistant", 5, "t1")];
    expect(turnFooters(orphan, turns, false).get("m")?.start).toBe(100.5);
    expect(
      turnFooters(orphan, [{ id: "t1", endedAt: 9 }], false).get("m")?.start,
    ).toBeUndefined();
  });
  it("read the same when nothing shown changed", () => {
    const a = turnFooters(entries, turns, true).get("m2")!;
    const b = turnFooters(entries, turns, true).get("m2")!;
    expect(a).not.toBe(b);
    expect(sameFooter(a, b)).toBe(true);
    expect(sameFooter(a, { ...b, usage: { ...usage, output: 1 } })).toBe(false);
    expect(sameFooter(a, { ...b, active: true })).toBe(false);
    expect(
      sameFooter({ ...a, usage: undefined }, { ...b, usage: undefined }),
    ).toBe(true);
  });
});

describe("turn figures", () => {
  it("format a duration to the precision its length wants", () => {
    expect(formatDuration(0.84)).toBe("0.8s");
    expect(formatDuration(-3)).toBe("0.0s");
    expect(formatDuration(12.4)).toBe("12s");
    expect(formatDuration(65)).toBe("1m 05s");
    expect(formatDuration(3599.6)).toBe("1h 00m");
    expect(formatDuration(3725)).toBe("1h 02m");
  });
  it("format tokens and cost", () => {
    expect(formatTokens(842)).toBe("842");
    expect(formatTokens(9540)).toBe("9.5k");
    expect(formatTokens(13432)).toBe("13k");
    expect(formatTokens(1_234_000)).toBe("1.2M");
    expect(formatCost(0.041)).toBe("$0.04");
    expect(formatCost(0.004)).toBe("<$0.01");
    expect(formatCost(1.5)).toBe("$1.50");
  });
  it("summarise usage with the split and detail the caches", () => {
    expect(usageSummary(usage)).toBe("13k tokens (10k in, 3.2k out)");
    expect(usageDetail(usage)).toBe(
      "Input 10,231 tokens, 8,120 read from cache, 100 written to cache · Output 3,201 tokens, 1,000 reasoning",
    );
    expect(usageDetail({ input: 5, cached: 0, output: 2, total: 7 })).toBe(
      "Input 5 tokens · Output 2 tokens",
    );
  });
  it("write the whole line", () => {
    const done = turnFooters(entries, turns, false).get("m2")!;
    expect(footerText(done)).toBe(
      "1m 12s · 13k tokens (10k in, 3.2k out) · $0.04",
    );
    const running = turnFooters(entries, turns, true).get("a2")!;
    expect(footerText(running)).toBe("");
    expect(footerText(running, 230)).toBe("30s");
    expect(
      footerText({
        turnID: "x",
        active: false,
        usage: { ...usage, costUSD: 0 },
      }),
    ).toBe("13k tokens (10k in, 3.2k out)");
  });
});
