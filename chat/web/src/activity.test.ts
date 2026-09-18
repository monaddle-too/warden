import { readFileSync } from "node:fs";
import { describe, expect, it } from "vitest";
import {
  activityLabel,
  basename,
  runningLabel,
  trimCommand,
  type ActivityEntry,
  WORKING,
} from "./activity";

/* The cases the TUI's Go derivation runs too (internal/tui/activity_test.go),
   so the two surfaces say the same words for the same transcript. */
const cases: { name: string; entries: ActivityEntry[]; want: string }[] =
  JSON.parse(
    readFileSync(
      new URL("../../internal/tui/testdata/activity.json", import.meta.url),
      "utf8",
    ),
  );

describe("activityLabel", () => {
  it("has the shared cases", () => {
    expect(cases.length).toBeGreaterThan(20);
  });
  for (const c of cases)
    it(c.name, () => {
      expect(activityLabel(c.entries)).toBe(c.want);
    });
  it("falls back to working", () => {
    expect(runningLabel([])).toBe(WORKING);
    expect(runningLabel(cases[1].entries)).toBe(cases[1].want);
  });
});

describe("words", () => {
  it("trims a command to its first line, collapsed", () => {
    expect(trimCommand("  go   test\t./...\nsleep 1\n")).toBe("go test ./...");
    expect(trimCommand("x".repeat(60), 10)).toBe("xxxxxxxxx…");
    expect(trimCommand("", 10)).toBe("");
  });
  it("names a file by its last segment", () => {
    expect(basename("/home/agent/workspace/chat/a.go")).toBe("a.go");
    expect(basename("a.go")).toBe("a.go");
    expect(basename("/home/agent/workspace/chat/")).toBe("chat");
  });
});
