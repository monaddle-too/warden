import { describe, expect, it } from "vitest";
import {
  compactionLabel,
  compactionText,
  contextLevel,
  contextSummary,
} from "./context";

describe("context meter", () => {
  it("reads used against the window with the level at 80 and 95 percent", () => {
    const s = contextSummary({
      used: 42787,
      window: 200000,
      model: "claude-sonnet-5",
    });
    expect(s.label).toBe("43k / 200k");
    expect(s.percent).toBe(21);
    expect(s.level).toBe("ok");
    expect(s.title).toContain(
      "42,787 of 200,000 tokens (21 %) on claude-sonnet-5",
    );
    expect(s.title).toContain("/compact");
    expect(contextSummary({ used: 171238, window: 200000 }).level).toBe("warn");
    expect(contextSummary({ used: 171238, window: 200000 }).title).toContain(
      "Nearing the window",
    );
    expect(contextSummary({ used: 191000, window: 200000 }).level).toBe("high");
    expect(contextSummary({ used: 250000, window: 200000 }).fraction).toBe(1);
    expect(contextLevel(0.799)).toBe("ok");
    expect(contextLevel(0.8)).toBe("warn");
    expect(contextLevel(0.95)).toBe("high");
  });
  it("shows the count alone when the window is unknown", () => {
    const s = contextSummary({ used: 42787, window: 0 });
    expect(s.label).toBe("43k");
    expect(s.fraction).toBe(0);
    expect(s.level).toBe("ok");
  });
});

describe("compaction divider", () => {
  it("names the trigger and the counts", () => {
    expect(
      compactionLabel({
        status: "completed",
        trigger: "manual",
        preTokens: 171238,
        postTokens: 2194,
      }),
    ).toBe("manual · 171k → 2.2k tokens");
    expect(
      compactionLabel({
        status: "completed",
        trigger: "auto",
        preTokens: 184293,
      }),
    ).toBe("automatic · from 184k tokens");
    expect(compactionLabel({ status: "completed" })).toBe("");
    expect(
      compactionText({
        status: "completed",
        trigger: "auto",
        preTokens: 184293,
        postTokens: 2484,
      }),
    ).toBe("Context compacted · automatic · 184k → 2.5k tokens");
    expect(compactionText({ status: "completed" })).toBe("Context compacted");
    expect(compactionText({ status: "running" })).toBe("Compacting context…");
    expect(
      compactionText({ status: "failed", error: "API Error: refused" }),
    ).toBe("Compaction failed: API Error: refused");
  });
});
