import { describe, it, expect } from "vitest";
import { costLine, costRows, sessionCost } from "./cost";
import type { Entry, Turn } from "./types";

const turns: Turn[] = [
  {
    id: "t1",
    startedAt: 100,
    endedAt: 112,
    usage: {
      input: 10000,
      cached: 8000,
      cacheWrite: 500,
      output: 300,
      reasoning: 100,
      total: 10300,
      costUSD: 0.03,
    },
  },
  {
    id: "t2",
    startedAt: 200,
    endedAt: 260,
    usage: { input: 20000, cached: 15000, output: 700, total: 20700, costUSD: 0.05 },
  },
  { id: "t3", startedAt: 300 },
];

describe("sessionCost", () => {
  it("counts the answered side questions in the usage, marked apart", () => {
    const entries = [
      { id: "a", role: "aside", text: "q", createdAt: 1, aside: { status: "completed", input: 1000, output: 50, costUSD: 0.02 } },
      { id: "b", role: "aside", text: "q", createdAt: 2, aside: { status: "failed", error: "no", input: 5, output: 1, costUSD: 0.01 } },
      { id: "c", role: "aside", text: "q", createdAt: 3, aside: { status: "running" } },
      { id: "d", role: "user", text: "hi", createdAt: 4 },
    ] as Entry[];
    const c = sessionCost(turns, undefined, entries);
    expect(c.asides).toBe(1);
    expect(c.asideCostUSD).toBeCloseTo(0.02);
    expect(c.usage.total).toBe(32050);
    expect(c.usage.costUSD).toBeCloseTo(0.1);
    expect(costRows(c)).toContainEqual(["  of it, side questions", "1 · $0.02"]);
    expect(costLine(c)).toContain("1 side question");
    const none = sessionCost(turns, undefined, []);
    expect(none.asides).toBe(0);
    expect(costRows(none).some(([l]) => l.includes("side questions"))).toBe(false);
    // Side questions alone price a chat that has no turn yet.
    const only = sessionCost([], undefined, entries);
    expect(only.priced).toBe(true);
    expect(only.usage.costUSD).toBeCloseTo(0.02);
  });
  it("sums the turns' usage, cost and time", () => {
    const c = sessionCost(turns);
    expect(c.turns).toBe(3);
    expect(c.reported).toBe(2);
    expect(c.usage).toEqual({
      input: 30000,
      cached: 23000,
      cacheWrite: 500,
      output: 1000,
      reasoning: 100,
      total: 31000,
      costUSD: 0.08,
    });
    expect(c.priced).toBe(true);
    expect(c.seconds).toBe(72);
    expect(c.running).toBe(false);
  });
  it("counts a running turn up to now", () => {
    const c = sessionCost(turns, 330);
    expect(c.seconds).toBe(102);
    expect(c.running).toBe(true);
  });
  it("shows Codex's tokens without a cost", () => {
    const c = sessionCost([
      { id: "t", startedAt: 1, endedAt: 2, usage: { input: 10, cached: 0, output: 5, total: 15 } },
    ]);
    expect(c.priced).toBe(false);
    const rows = costRows(c);
    expect(rows.find(([k]) => k === "Cost")?.[1]).toBe("not reported by this agent");
    expect(costLine(c)).toBe("1 turn · 15 tokens (10 in, 5 out) · 1.0s");
  });
  it("handles a chat with no turns", () => {
    const c = sessionCost(undefined);
    expect(c.turns).toBe(0);
    expect(costLine(c)).toBe("0 turns · 0 tokens (0 in, 0 out)");
    expect(costRows(c).map(([k]) => k)).toEqual([
      "Turns",
      "Input tokens",
      "Output tokens",
      "Total tokens",
      "Cost",
      "Time in turns",
    ]);
  });
  it("lays out the rows with the cache and thinking parts", () => {
    const rows = costRows(sessionCost(turns));
    expect(rows).toEqual([
      ["Turns", "3 (2 with usage)"],
      ["Input tokens", "30k"],
      ["  read from cache", "23k"],
      ["  written to cache", "500"],
      ["Output tokens", "1.0k"],
      ["  thinking", "100"],
      ["Total tokens", "31k"],
      ["Cost", "$0.08"],
      ["Time in turns", "1m 12s"],
    ]);
    expect(costLine(sessionCost(turns))).toBe(
      "3 turns · 31k tokens (30k in, 1.0k out) · $0.08 · 1m 12s",
    );
  });
});
