import { describe, it, expect } from "vitest";
import {
  FAST_MODE_HINT,
  LONG_CONTEXT_HINT,
  catalogRow,
  effortOptions,
  fastModeFor,
  modelOptions,
} from "./models";
import type { AgentOptions, CatalogModel } from "./types";

const catalog: CatalogModel[] = [
  {
    value: "default",
    resolved: "claude-sonnet-5",
    label: "Default (recommended)",
    description: "Sonnet 5 · Efficient for routine tasks",
    efforts: ["low", "medium", "high", "xhigh", "max"],
    adaptiveThinking: true,
  },
  {
    value: "sonnet",
    resolved: "claude-sonnet-5",
    label: "Sonnet",
    description: "Sonnet 5 · Efficient for routine tasks",
    efforts: ["low", "medium", "high", "xhigh", "max"],
    adaptiveThinking: true,
  },
  {
    value: "opus[1m]",
    resolved: "claude-opus-5[1m]",
    label: "Opus (1M context)",
    efforts: ["low", "max"],
    adaptiveThinking: true,
    fastMode: true,
  },
  { value: "haiku", resolved: "claude-haiku-4-5-20251001", label: "Haiku" },
];
const withCatalog: AgentOptions = {
  fastMode: false,
  longContext: false,
  models: { claude: catalog },
};

describe("modelOptions", () => {
  it("builds the picker's rows from the catalog, the default row first", () => {
    const rows = modelOptions("claude", withCatalog);
    expect(rows.map((r) => r.value)).toEqual([
      "",
      "sonnet",
      "opus[1m]",
      "haiku",
    ]);
    expect(rows[0].label).toBe("Provider default");
    expect(rows[0].hint).toContain("claude-sonnet-5");
    expect(rows[1].label).toBe("Claude Sonnet");
    expect(rows[1].hint).toBe(
      "claude-sonnet-5 · Sonnet 5 · Efficient for routine tasks",
    );
    expect(rows[1].disabled).toBeUndefined();
    expect(rows[3].hint).toBe("claude-haiku-4-5-20251001");
  });
  it("keeps a 1M row visible but disabled with the hint until the operator allows it", () => {
    const rows = modelOptions("claude", withCatalog);
    expect(rows[2]).toMatchObject({
      value: "opus[1m]",
      label: "Claude Opus (1M context)",
      disabled: true,
      hint: LONG_CONTEXT_HINT,
    });
    const allowed = modelOptions("claude", {
      ...withCatalog,
      longContext: true,
    });
    expect(allowed[2].disabled).toBeUndefined();
    expect(allowed[2].hint).toContain("claude-opus-5[1m]");
  });
  it("falls back to the static rows without a catalog", () => {
    expect(modelOptions("claude").map((r) => r.value)).toEqual([
      "",
      "sonnet",
      "opus",
      "haiku",
      "sonnet[1m]",
      "opus[1m]",
    ]);
    expect(
      modelOptions("claude")
        .filter((r) => r.disabled)
        .map((r) => r.value),
    ).toEqual(["sonnet[1m]", "opus[1m]"]);
    expect(
      modelOptions("claude", { fastMode: false, longContext: true }).some(
        (r) => r.disabled,
      ),
    ).toBe(false);
    expect(modelOptions("codex", withCatalog)).toHaveLength(6);
    expect(modelOptions("codex")[1]).toMatchObject({
      value: "gpt-6-astra",
      label: "GPT-6 Astra",
    });
    expect(modelOptions("other")).toHaveLength(1);
  });
});

describe("effortOptions and fastModeFor", () => {
  it("offers each model the effort levels the catalog says it takes", () => {
    expect(
      effortOptions("claude", "", withCatalog).map((e) => e.value),
    ).toEqual(["", "low", "medium", "high", "xhigh", "max"]);
    expect(
      effortOptions("claude", "opus[1m]", withCatalog).map((e) => e.value),
    ).toEqual(["", "low", "max"]);
    expect(
      effortOptions("claude", "haiku", withCatalog).map((e) => e.value),
    ).toEqual([""]);
    // Unknown to the catalog, or no catalog: every level.
    expect(effortOptions("claude", "opusplan", withCatalog)).toHaveLength(6);
    expect(effortOptions("claude", "haiku")).toHaveLength(6);
    expect(catalogRow("claude", "", withCatalog)?.value).toBe("default");
    expect(catalogRow("claude", "haiku")).toBeUndefined();
  });
  it("disables fast mode with the config hint until allowed, and says when the model lacks it", () => {
    const off = fastModeFor("claude", "opus[1m]", withCatalog);
    expect(off.allowed).toBe(false);
    expect(off.supported).toBe(true);
    expect(off.title).toContain(FAST_MODE_HINT);
    const on = fastModeFor(
      "claude",
      "opus[1m]",
      { ...withCatalog, fastMode: true },
      "on",
    );
    expect(on.allowed).toBe(true);
    expect(on.title).not.toContain(FAST_MODE_HINT);
    expect(on.title).toContain("now on");
    const sonnet = fastModeFor("claude", "sonnet", {
      ...withCatalog,
      fastMode: true,
    });
    expect(sonnet.supported).toBe(false);
    expect(sonnet.title).toContain("not offered on Sonnet");
    expect(
      fastModeFor("claude", "sonnet", { fastMode: true, longContext: false })
        .supported,
    ).toBeUndefined();
  });
});
