import { describe, it, expect } from "vitest";
import { effectiveNetwork, networkLabel, networkText } from "./network";

describe("workspace network access", () => {
  it("names the install's setting in the follow choice", () => {
    expect(networkLabel("")).toBe("Install setting");
    expect(networkLabel("", { mode: "open", source: "console" })).toBe(
      "Install setting (open)",
    );
    expect(
      networkLabel("restricted", { mode: "open", source: "console" }),
    ).toBe("Restricted");
    expect(networkLabel("open")).toBe("Open");
  });
  it("explains the mode in effect", () => {
    expect(networkText("open")).toMatch(/^Any public/);
    expect(networkText("", { mode: "open", source: "config" })).toMatch(
      /^Any public/,
    );
    expect(networkText("", { mode: "restricted", source: "config" })).toMatch(
      /^The AI providers/,
    );
    expect(networkText("")).toMatch(/admin console/);
  });
  it("resolves the effective mode", () => {
    expect(
      effectiveNetwork("open", { mode: "restricted", source: "config" }),
    ).toBe("open");
    expect(effectiveNetwork("", { mode: "restricted", source: "config" })).toBe(
      "restricted",
    );
    expect(effectiveNetwork(undefined)).toBeUndefined();
  });
});
