import { describe, it, expect } from "vitest";
import {
  asidesLabel,
  chatSpend,
  spendLabel,
  spendLine,
  spendSummary,
  spendTitle,
  sumSpend,
  workspaceSpend,
} from "./spend";
import type { Chat, Spend } from "./types";

const chat = (
  id: string,
  sandboxID: string,
  spend?: Spend,
  turns?: Chat["conversation"]["turns"],
): Chat =>
  ({
    id,
    title: id,
    sandboxID,
    repository: "",
    status: "idle",
    archived: false,
    conversation: { entries: [], turns },
    approvals: [],
    spend,
  }) as Chat;

const priced: Spend = {
  turns: 3,
  input: 30000,
  output: 1000,
  total: 31000,
  costUSD: 0.12,
  priced: true,
};
const tokens: Spend = {
  turns: 2,
  input: 900,
  output: 100,
  total: 1000,
  costUSD: 0,
  priced: false,
};

describe("spend", () => {
  it("labels the chip with the cost, or the tokens where the provider prices nothing", () => {
    expect(spendLabel(priced)).toBe("$0.12");
    expect(spendLabel(tokens)).toBe("1.0k tok");
    expect(spendLabel({ ...priced, costUSD: 0.001 })).toBe("<$0.01");
    expect(spendLine(priced)).toBe(
      "3 turns · 31k tokens (30k in, 1.0k out) · $0.12",
    );
    expect(spendLine({ ...tokens, turns: 1 })).toBe(
      "1 turn · 1.0k tokens (900 in, 100 out) · no cost reported",
    );
    expect(spendTitle(priced)).toContain("This chat so far: 3 turns");
    expect(spendSummary(priced)).toBe("$0.12 · 31k tokens · 3 turns");
    expect(spendSummary({ ...tokens, turns: 1 })).toBe("1.0k tokens · 1 turn");
  });
  it("marks the side questions in the line and sums them across chats", () => {
    const withAsides: Spend = { ...priced, asides: 2, asideCostUSD: 0.05 };
    expect(asidesLabel(withAsides)).toBe("2 side questions ($0.05)");
    expect(asidesLabel({ ...priced, asides: 1 })).toBe("1 side question");
    expect(asidesLabel(priced)).toBe("");
    expect(spendLine(withAsides)).toBe(
      "3 turns · 31k tokens (30k in, 1.0k out) · $0.12 · 2 side questions ($0.05)",
    );
    expect(spendTitle(withAsides)).toContain("turns and your side questions summed");
    expect(spendTitle(priced)).not.toContain("side questions");
    const sum = sumSpend([withAsides, priced, { ...tokens, asides: 1 }]);
    expect(sum.asides).toBe(3);
    expect(sum.asideCostUSD).toBeCloseTo(0.05);
    expect(sumSpend([priced]).asides).toBeUndefined();
  });
  it("takes the service's sum, or sums the turns when the service gave none", () => {
    expect(chatSpend(chat("a", "ws", priced))).toBe(priced);
    const summed = chatSpend(
      chat("b", "ws", undefined, [
        {
          id: "t1",
          startedAt: 1,
          endedAt: 2,
          usage: {
            input: 100,
            cached: 0,
            output: 10,
            total: 110,
            costUSD: 0.01,
          },
        },
        { id: "t2", startedAt: 3 },
      ]),
    );
    expect(summed).toEqual({
      turns: 2,
      input: 100,
      output: 10,
      total: 110,
      costUSD: 0.01,
      priced: true,
    });
    expect(chatSpend(chat("c", "ws")).turns).toBe(0);
  });
  it("sums a workspace's chats, archived ones included, and leaves other workspaces out", () => {
    const archived = { ...chat("d", "ws", tokens), archived: true };
    const { spend, chats } = workspaceSpend(
      [chat("a", "ws", priced), archived, chat("e", "other", priced)],
      "ws",
    );
    expect(chats).toBe(2);
    expect(spend).toEqual({
      turns: 5,
      input: 30900,
      output: 1100,
      total: 32000,
      costUSD: 0.12,
      priced: true,
    });
    expect(sumSpend([])).toEqual({
      turns: 0,
      input: 0,
      output: 0,
      total: 0,
      costUSD: 0,
      priced: false,
    });
    expect(sumSpend([tokens, tokens]).priced).toBe(false);
  });
});
